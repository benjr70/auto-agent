// Package arbitrate is the Arbitrate Step: the Arbiter. When a Fix disputes
// review threads, it reads each disputed thread with the code and the
// written requirements and rules it: fix, dismiss, or a product ambiguity
// for the human. It rules a thread the one-time review opened, and only
// recommends on one a human opened. It changes nothing and posts nothing:
// the Daemon reads its verdicts and acts on them.
package arbitrate

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

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
const Name = "arbitrate"

// timeout is the wall-clock limit of one Arbitrate call.
const timeout = 30 * time.Minute

// Spec is the Arbitrate Step for a Target Project with these commands: read
// tools, read-only git, and the Harness config's test and lint commands.
func Spec(commands implement.Commands) step.Spec {
	return step.ReadOnly(Name, prompt, schema, timeout, commands.Test, commands.Lint)
}

// Who opened a disputed thread.
const (
	Bot   = "bot"
	Human = "human"
)

// Dispute is one disputed thread, as the Arbiter is told it: the Finding
// that opened it and the Fix's Dispute, and nothing else the Fix said.
type Dispute struct {
	Thread string `json:"thread"`
	Path   string `json:"path"`
	Line   int    `json:"line,omitempty"`
	// Authored is Bot, which the Arbiter rules, or Human, on which it only
	// recommends.
	Authored string `json:"authored"`
	// Finding is the thread's first comment, verbatim.
	Finding string `json:"finding"`
	// Dispute is the Fix's Dispute, verbatim.
	Dispute string `json:"dispute"`
}

// Input is what the Step is told.
type Input struct {
	PR int `json:"pr"`
	// Branch is the branch the checkout is on, at the PR's head; Base is
	// the branch it merges into.
	Branch string          `json:"branch"`
	Base   string          `json:"base"`
	Issue  implement.Issue `json:"issue"`
	// Spec is the ticket's parent Spec; nil when its body names none.
	Spec     *implement.Issue   `json:"spec"`
	Commands implement.Commands `json:"commands"`
	Disputes []Dispute          `json:"disputes"`
}

// The verdicts.
const (
	// Fix: the Finding stands; the next Fix applies the instruction and may
	// not dispute it.
	Fix = "fix"
	// Dismiss: the Finding does not stand; the thread is resolved with the
	// reason.
	Dismiss = "dismiss"
	// Ambiguity: a product ambiguity, for the human in the Ruling request.
	Ambiguity = "ambiguity"
	// Unruled: no verdict Go can act on; carried to the human like an
	// ambiguity, never dismissed. Go's, never the Step's.
	Unruled = "unruled"
	// Cannot: the Fix could not apply a fix verdict; a decision for the
	// human. Go's, never the Step's.
	Cannot = "cannot"
)

// Result is what the Step answers.
type Result struct {
	Verdicts []Given `json:"verdicts"`
}

// Given is one verdict as the Step gave it.
type Given struct {
	Thread  string `json:"thread"`
	Verdict string `json:"verdict"`
	// Text is the instruction of a fix, or the reason of a dismissal,
	// written for whoever opened the thread.
	Text string `json:"text"`
	// Decision is what the human decides, for an ambiguity; null for any
	// other verdict.
	Decision *Decision `json:"decision"`
}

// Decision is one decision a product ambiguity puts to the human.
type Decision struct {
	Title string `json:"title"`
	// Scenario is one line a user of the Target Project would recognise.
	Scenario      string `json:"scenario"`
	Now           string `json:"now"`
	ReviewerWants string `json:"reviewer_wants"`
	// WhyYours says why it is the human's call: where the ticket and the
	// Spec are silent or disagree, and what a user would see differ.
	WhyYours    string   `json:"why_yours"`
	Options     []Option `json:"options"`
	Recommended string   `json:"recommended"`
}

// Option is one lettered option of a decision.
type Option struct {
	Letter string `json:"letter"`
	Text   string `json:"text"`
	Cost   string `json:"cost"`
}

// Verdict is the Arbiter's verdict on one disputed thread, as Go reads it.
type Verdict struct {
	Thread   string    `json:"thread"`
	Authored string    `json:"authored"`
	Verdict  string    `json:"verdict"`
	Text     string    `json:"text"`
	Decision *Decision `json:"decision,omitempty"`
	// Reason says why a thread is unruled.
	Reason string `json:"reason,omitempty"`
}

// Read reads the Step's verdicts against the disputes it was given: one
// verdict per thread. A thread with no verdict, or more than one, or one
// that is not fix, dismiss or ambiguity, is unruled. A fix or a dismissal
// on a thread a human opened is refused, and unruled: a human's thread is
// never dismissed, or ruled, on the Arbiter's word. So is a fix or a
// dismissal with nothing to post. A verdict on a thread not given is
// ignored.
func Read(disputes []Dispute, result Result) []Verdict {
	verdicts := make([]Verdict, 0, len(disputes))
	for _, d := range disputes {
		v := Verdict{Thread: d.Thread, Authored: d.Authored, Verdict: Unruled}
		var given []Given
		for _, g := range result.Verdicts {
			if g.Thread == d.Thread {
				given = append(given, g)
			}
		}
		switch {
		case len(given) == 0:
			v.Reason = "no verdict"
		case len(given) > 1:
			v.Reason = "verdict doubled"
		case given[0].Verdict != Fix && given[0].Verdict != Dismiss && given[0].Verdict != Ambiguity:
			v.Reason = fmt.Sprintf("verdict outside fix/dismiss/ambiguity: %s", given[0].Verdict)
		case d.Authored != Bot && given[0].Verdict != Ambiguity:
			v.Text = given[0].Text
			v.Reason = fmt.Sprintf("human-authored thread: %s refused, collected for the human", given[0].Verdict)
		case given[0].Verdict != Ambiguity && strings.TrimSpace(given[0].Text) == "":
			v.Reason = fmt.Sprintf("%s with nothing to post", given[0].Verdict)
		default:
			v.Verdict, v.Text, v.Decision = given[0].Verdict, given[0].Text, given[0].Decision
		}
		verdicts = append(verdicts, v)
	}
	return verdicts
}
