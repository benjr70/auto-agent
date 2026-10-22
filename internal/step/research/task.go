package research

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/benjr70/auto-agent/internal/step"
)

var (
	//go:embed task.md
	taskPrompt string
	//go:embed task_schema.json
	taskSchema string
)

// TaskSpec is the Research Step in task mode: a task Decision ticket's work
// done in the checkout, with nothing to write down but the answer. It has
// the shell a task needs, and still no credential: what keeps it off GitHub
// is the runner, and what keeps its changes out of the Target Project is Go,
// which commits nothing and puts the checkout back afterwards.
func TaskSpec() step.Spec {
	tools := []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash", "WebFetch", "WebSearch"}
	return step.Spec{Name: Name, Prompt: taskPrompt, Schema: taskSchema, Tools: tools, Allowed: tools, Timeout: timeout}
}

// TaskInput is what the Step is told in task mode: the ticket, as for
// research, with no findings file to write.
type TaskInput struct {
	Ticket   Ticket `json:"ticket"`
	Question string `json:"question"`
	Map      *Map   `json:"map"`
	Date     string `json:"date"`
}

// TaskInputOf is the task mode's input for what a research Step would be
// told.
func TaskInputOf(in Input) TaskInput {
	return TaskInput{Ticket: in.Ticket, Question: in.Question, Map: in.Map, Date: in.Date}
}

// The outcomes a task result names.
const (
	// TaskDone: the work is done.
	TaskDone = "done"
	// TaskNeedsCode: the task needs a product code change, which a
	// Decision ticket never carries.
	TaskNeedsCode = "needs-code"
	// TaskNeedsHuman: the work needs access or a decision only a human
	// has.
	TaskNeedsHuman = "needs-human"
)

// TaskResult is what the Step answers in task mode.
type TaskResult struct {
	Outcome string `json:"outcome"`
	// Summary is, in one paragraph, what was done; for needs-code what the
	// code change would take; for needs-human what is needed.
	Summary string `json:"summary"`
	// Facts are what later tickets need of a done task, one line each.
	Facts []string `json:"facts"`
}

// Check re-checks what the schema cannot express.
func (r TaskResult) Check() error {
	summary := strings.TrimSpace(r.Summary)
	switch {
	case summary == "":
		return errors.New("the summary is empty")
	case paragraphBreak.MatchString(summary):
		return errors.New("the summary is more than one paragraph")
	case r.Outcome != TaskDone && len(r.Facts) > 0:
		return errors.New("only a done task has facts")
	}
	for _, fact := range r.Facts {
		if strings.TrimSpace(fact) == "" || strings.ContainsAny(fact, "\r\n") {
			return fmt.Errorf("the fact %q is not one line of text", fact)
		}
	}
	return nil
}

// TaskResolution is the comment that resolves a done task, as
// plugin/skills/afk-resolve §8 wrote it: what was done, and every fact later
// tickets need.
func TaskResolution(r TaskResult) string {
	text := strings.TrimSpace(r.Summary)
	if len(r.Facts) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text + "\n\nFacts for later tickets:\n")
	for _, fact := range r.Facts {
		b.WriteString("\n- " + strings.TrimSpace(fact))
	}
	return b.String()
}

// NeedsCodeComment is the comment that hands a task needing product code to
// a human, in the words of plugin/skills/afk-resolve §8.
func NeedsCodeComment(what string) string {
	return "afk-resolve: this task needs a change to product code, which a Decision ticket never carries — relabelled HITL for a human to route (as a Slice off a Spec, or by re-scoping the ticket). What it would take: " + strings.TrimSpace(what)
}
