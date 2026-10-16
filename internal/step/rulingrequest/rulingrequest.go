// Package rulingrequest is the Write Ruling request Step: it turns what on
// an Agent PR needs the human (a product ambiguity, a Dispute on a thread a
// human opened, a thread the Arbiter could not rule, a fix it ruled that
// cannot be applied) into decisions the human answers with one letter each.
// It changes nothing and posts nothing: the Daemon composes the Ruling
// request from its decisions and posts it.
package rulingrequest

import (
	_ "embed"
	"fmt"
	"time"

	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
)

var (
	//go:embed prompt.md
	prompt string
	//go:embed schema.json
	schema string
)

// Name is the Step's name.
const Name = "write-ruling-request"

// timeout is the wall-clock limit of one call.
const timeout = 30 * time.Minute

// Spec is the Step for a Target Project with these commands: read tools,
// read-only git, and the Harness config's test and lint commands.
func Spec(commands implement.Commands) step.Spec {
	return step.ReadOnly(Name, prompt, schema, timeout, commands.Test, commands.Lint)
}

// The kinds of item a Ruling request asks about.
const (
	// KindProductAmbiguity: the one-time review found the ticket and its
	// Spec silent or contradicting on something a user would see.
	KindProductAmbiguity = "product-ambiguity"
	// KindAmbiguity: the Arbiter found a Dispute turns on a product
	// ambiguity, or recommends on a thread a human opened.
	KindAmbiguity = "ambiguity"
	// KindUnruled: the Arbiter left a Dispute without a verdict Go could
	// act on, or it came after the Arbiter ran.
	KindUnruled = "unruled"
	// KindCannot: the Fix could not apply the Arbiter's fix.
	KindCannot = "cannot"
)

// Item is one thing the human is asked about.
type Item struct {
	Kind string `json:"kind"`
	// Path and Line are where it is; Line is 0 when it names no line.
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	// Authored is who opened its thread, "bot" or "human"; empty for an
	// item on no thread.
	Authored string `json:"authored,omitempty"`
	// Finding is what was found: the thread's first comment, or the
	// review's Finding.
	Finding string `json:"finding"`
	// Dispute is the Fix's Dispute, on a thread; empty otherwise.
	Dispute string `json:"dispute,omitempty"`
	// Note is what else was said: why the Arbiter left it unruled, or the
	// fix it ruled that cannot be applied.
	Note string `json:"note,omitempty"`
	// Draft is the decision the Arbiter wrote, when it wrote one.
	Draft *ruling.Decision `json:"draft,omitempty"`
}

// Input is what the Step is told.
type Input struct {
	PR     int             `json:"pr"`
	Branch string          `json:"branch"`
	Base   string          `json:"base"`
	Issue  implement.Issue `json:"issue"`
	// Spec is the ticket's parent Spec; nil when its body names none.
	Spec     *implement.Issue   `json:"spec"`
	Commands implement.Commands `json:"commands"`
	Items    []Item             `json:"items"`
}

// Written is one decision, for the item it asks about, numbered from 1.
type Written struct {
	Item int `json:"item"`
	ruling.Decision
}

// Result is what the Step answers.
type Result struct {
	Decisions []Written `json:"decisions"`
}

// Check re-checks what the schema cannot express, against the items asked
// about: one decision per item, each one the human can answer in a letter.
func (r Result) Check(items []Item) error {
	seen := map[int]bool{}
	for _, w := range r.Decisions {
		switch {
		case w.Item < 1 || w.Item > len(items):
			return fmt.Errorf("a decision is for item %d; the items are 1 to %d", w.Item, len(items))
		case seen[w.Item]:
			return fmt.Errorf("item %d has two decisions", w.Item)
		}
		seen[w.Item] = true
		if err := w.Decision.Check(); err != nil {
			return fmt.Errorf("item %d: %w", w.Item, err)
		}
	}
	for i := range items {
		if !seen[i+1] {
			return fmt.Errorf("item %d has no decision", i+1)
		}
	}
	return nil
}

// Drafted are the items' decisions when every item carries a draft the
// human can answer as it stands: then nothing is left for the Step to
// write. ok is false when one does not.
func Drafted(items []Item) (decisions []ruling.Decision, ok bool) {
	for _, item := range items {
		if item.Draft == nil || item.Draft.Check() != nil {
			return nil, false
		}
		decisions = append(decisions, *item.Draft)
	}
	return decisions, len(decisions) > 0
}

// Ordered are the Step's decisions, in the order of the items they are
// for.
func (r Result) Ordered() []ruling.Decision {
	ordered := make([]ruling.Decision, len(r.Decisions))
	for _, w := range r.Decisions {
		if w.Item >= 1 && w.Item <= len(ordered) {
			ordered[w.Item-1] = w.Decision
		}
	}
	return ordered
}
