// Package verifier is the Verify Step: the manual verifier of one Agent PR.
// Handed the PR's unchecked verification items, its running environment's
// block, the declared Surfaces and the MCP servers that drive them, it
// exercises each item on the real Surfaces and answers with a verdict per
// item and the screenshot tour. It never writes to GitHub: the Daemon ticks
// the boxes, edits the PR's description and posts the evidence.
package verifier

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
)

//go:embed prompt.md
var prompt string

//go:embed schema.json
var schema string

// timeout is the wall-clock limit of one round's call.
const timeout = time.Hour

// Name is the Step's name.
const Name = "verify"

// The classes of an item.
const (
	Local       = "local"
	DeployedEnv = "deployed-env"
	Hardware    = "hardware"
	Unjustified = "unjustified"
)

// Spec is the Verify Step. It sees no tool that edits: its shell drives the
// cli and api Surfaces, and the MCP servers the call is handed drive the UI
// ones.
func Spec() step.Spec {
	tools := []string{"Read", "Glob", "Grep", "Bash"}
	return step.Spec{Name: Name, Prompt: prompt, Schema: schema, Tools: tools, Allowed: tools, Timeout: timeout}
}

// Item is one unchecked verification item as the Step is told it. ID is its
// place in the checklist, from 1: two items may share a text, never an ID.
type Item struct {
	ID         int    `json:"id"`
	Text       string `json:"text"`
	Section    string `json:"section"`
	PostDeploy string `json:"post_deploy,omitempty"`
}

// Surface is one declared Surface as the Step is told it.
type Surface struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// URL is the value the block gives its url_key; empty when it has none.
	URL      string `json:"url,omitempty"`
	Viewport string `json:"viewport,omitempty"`
	// Server is the MCP server that drives it; empty for cli and api.
	Server string `json:"server,omitempty"`
	// Tour says the PR touched it, and its screens are owed a screenshot.
	Tour bool `json:"tour"`
}

// Input is what the Step is told.
type Input struct {
	PR       int             `json:"pr"`
	Branch   string          `json:"branch"`
	Issue    implement.Issue `json:"issue"`
	Items    []Item          `json:"items"`
	Block    []string        `json:"block"`
	Surfaces []Surface       `json:"surfaces"`
	// EvidenceDir is the round's evidence directory.
	EvidenceDir string `json:"evidence_dir"`
	// Sandbox is the detail of a DEGRADED app sandbox; empty otherwise.
	Sandbox string `json:"sandbox,omitempty"`
	// Runbook is the maintainer's verifier runbook; empty when none.
	Runbook string `json:"runbook,omitempty"`
}

// Items are the checklist's items as the Step is told them.
func Items(items []checklist.Item) []Item {
	told := make([]Item, len(items))
	for i, item := range items {
		told[i] = Item{ID: i + 1, Text: item.Text, Section: string(item.Section), PostDeploy: item.PostDeploy}
	}
	return told
}

// Verdict is the Step's answer for one item, the item named by its ID.
type Verdict struct {
	ID           int      `json:"id"`
	Text         string   `json:"text"`
	Verdict      string   `json:"verdict"`
	Class        string   `json:"class"`
	Evidence     []string `json:"evidence"`
	SpecDemanded string   `json:"spec_demanded"`
	Blocker      string   `json:"blocker"`
}

// Shot is one screenshot of the tour, a file in the evidence directory.
type Shot struct {
	File  string `json:"file"`
	Shows string `json:"shows"`
}

// Result is what the Step answers.
type Result struct {
	Items []Verdict `json:"items"`
	Tour  []Shot    `json:"tour"`
}

// Check re-checks what the schema cannot: every item is answered, once, by
// its ID and with its text, and nothing else is; a pass or a fail cites
// evidence; a tour shot is a file name in the evidence directory.
func (r Result) Check(items []checklist.Item) error {
	answered := map[int]bool{}
	for _, v := range r.Items {
		switch {
		case v.ID < 1 || v.ID > len(items):
			return fmt.Errorf("item %d %q is not one of the items", v.ID, v.Text)
		case v.Text != items[v.ID-1].Text:
			return fmt.Errorf("item %d is not %q: it is %q", v.ID, v.Text, items[v.ID-1].Text)
		case answered[v.ID]:
			return fmt.Errorf("item %d %q is answered twice", v.ID, v.Text)
		case v.Verdict != string(checklist.Defer) && !hasEvidence(v.Evidence):
			return fmt.Errorf("%q is %s with no evidence", v.Text, v.Verdict)
		}
		answered[v.ID] = true
	}
	for i, item := range items {
		if !answered[i+1] {
			return fmt.Errorf("no verdict for item %d %q", i+1, item.Text)
		}
	}
	for _, shot := range r.Tour {
		if shot.File != filepath.Base(shot.File) || strings.HasPrefix(shot.File, ".") {
			return fmt.Errorf("tour shot %q is not a file name in the evidence directory", shot.File)
		}
	}
	return nil
}

func hasEvidence(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}

// Results are the verdicts as the round counts them, in the checklist's
// order. A deferral is justified only by what it needs: a deployed-env one
// by the post-deploy check it demands, a hardware one by its named blocker.
// Any other deferral is unjustified, and an unjustified deferral is a fail.
func (r Result) Results(items []checklist.Item) []checklist.Result {
	byID := map[int]Verdict{}
	for _, v := range r.Items {
		byID[v.ID] = v
	}
	results := make([]checklist.Result, 0, len(items))
	for i, item := range items {
		v := byID[i+1]
		result := checklist.Result{Item: item, Verdict: checklist.Verdict(v.Verdict), Classification: v.Class, Evidence: v.Evidence}
		if result.Verdict == checklist.Defer {
			switch spec, blocker := strings.TrimSpace(v.SpecDemanded), strings.TrimSpace(v.Blocker); {
			case v.Class == DeployedEnv && spec != "":
				result.Deferral, result.Demanded = "needs a deployed environment; post-deploy check demanded: "+spec, spec
			case v.Class == Hardware && blocker != "":
				result.Deferral = "needs hardware: " + blocker
			default:
				result.Verdict, result.Classification = checklist.Fail, Unjustified
				result.Evidence = append([]string{fmt.Sprintf("unjustified deferral: a %s item deferred with no %s", v.Class, justification(v.Class))}, v.Evidence...)
			}
		}
		results = append(results, result)
	}
	return results
}

// justification is what a deferral of the class must name.
func justification(class string) string {
	switch class {
	case DeployedEnv:
		return "post-deploy check demanded"
	case Hardware:
		return "named blocker"
	}
	return "reason it cannot run here: it can"
}

// TourLines are the tour as `ui-shot:` lines, what checklist.Tour reads.
func (r Result) TourLines() string {
	var b strings.Builder
	for _, shot := range r.Tour {
		b.WriteString("ui-shot: " + shot.File)
		if shows := strings.TrimSpace(shot.Shows); shows != "" {
			b.WriteString(" — " + shows)
		}
		b.WriteString("\n")
	}
	return b.String()
}
