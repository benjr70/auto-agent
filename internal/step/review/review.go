// Package review is the two Review Steps of an Agent PR, run once in its
// life: Review: correctness asks what breaks if the diff ships, and Review:
// Spec asks whether it does what the ticket asked. Each reads the branch and
// answers with Findings already sorted by the posting bar; neither changes
// anything, and the Daemon posts what they found.
package review

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
)

var (
	//go:embed correctness.md
	correctnessPrompt string
	//go:embed spec.md
	specPrompt string
	//go:embed schema.json
	schema string
)

// timeout is the wall-clock limit of one review call.
const timeout = 30 * time.Minute

// The Steps' names.
const (
	CorrectnessName = "review-correctness"
	SpecName        = "review-spec"
)

// readOnlyGit are the git commands the Steps may run: they read the history
// and the tree, and change neither.
var readOnlyGit = []string{"Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)"}

// Correctness is the Review: correctness Step for a Target Project with
// these commands.
func Correctness(commands implement.Commands) step.Spec {
	return spec(CorrectnessName, correctnessPrompt, commands)
}

// Spec is the Review: Spec Step for a Target Project with these commands.
func Spec(commands implement.Commands) step.Spec { return spec(SpecName, specPrompt, commands) }

// spec is a Review Step: it sees no tool that edits, and its shell may run
// read-only git and exactly the Harness config's test and lint commands.
func spec(name, prompt string, commands implement.Commands) step.Spec {
	allowed := append([]string{"Read", "Glob", "Grep"}, readOnlyGit...)
	for _, command := range []string{commands.Test, commands.Lint} {
		if command = strings.TrimSpace(command); command != "" {
			allowed = append(allowed, "Bash("+command+")")
		}
	}
	return step.Spec{Name: name, Prompt: prompt, Schema: schema, Tools: []string{"Read", "Glob", "Grep", "Bash"}, Allowed: allowed, Timeout: timeout}
}

// Input is what a Review Step is told.
type Input struct {
	// PR is the pull request under review.
	PR int `json:"pr"`
	// Branch is the branch the checkout is on; Base is the ref it is
	// reviewed against.
	Branch string          `json:"branch"`
	Base   string          `json:"base"`
	Issue  implement.Issue `json:"issue"`
	// Spec is the ticket's parent Spec; nil when its body names none.
	Spec     *implement.Issue   `json:"spec"`
	Commands implement.Commands `json:"commands"`
}

// Found is one Finding as a Review Step reports it.
type Found struct {
	Kind              finding.Kind `json:"kind"`
	Category          string       `json:"category"`
	Path              string       `json:"path"`
	Line              int          `json:"line"`
	Severity          string       `json:"severity"`
	Summary           string       `json:"summary"`
	FailureScenario   string       `json:"failure_scenario"`
	QuotedRequirement string       `json:"quoted_requirement"`
}

// Result is what a Review Step answers.
type Result struct {
	Findings []Found `json:"findings"`
}

// Check re-checks what the schema cannot express: every Finding says what it
// is about.
func (r Result) Check() error {
	for i, f := range r.Findings {
		switch {
		case strings.TrimSpace(f.Path) == "":
			return fmt.Errorf("finding %d names no file", i+1)
		case strings.TrimSpace(f.Summary) == "":
			return fmt.Errorf("finding %d has an empty summary", i+1)
		case strings.TrimSpace(f.Category) == "":
			return fmt.Errorf("finding %d has no category", i+1)
		}
	}
	return nil
}

// On are the Findings on the given axis.
func (r Result) On(axis string) []finding.Finding {
	findings := make([]finding.Finding, len(r.Findings))
	for i, f := range r.Findings {
		findings[i] = finding.Finding{
			Kind: f.Kind, Axis: axis, Category: f.Category, Path: f.Path, Line: f.Line, Severity: f.Severity,
			Summary: f.Summary, FailureScenario: f.FailureScenario, QuotedRequirement: f.QuotedRequirement,
		}
	}
	return findings
}
