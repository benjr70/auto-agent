package fire

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/thread"
)

// PhaseApply: answering the human's reply to a Ruling request.
const PhaseApply = "apply-ruling"

// reconcileRuling answers the human's reply to an Agent PR's Ruling
// request. A reply that is not a full Ruling gets one nudge, and the PR
// keeps waiting. A full Ruling is applied exactly: a Fix applies the chosen
// options that change code, under the ticket's Single-flight lock, and Go
// commits and pushes; then Go replies in and resolves each decision's
// thread, says the Ruling was applied, and takes AFK:ruling off.
func (f *Fire) reconcileRuling(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) (err error) {
	number, pr := next.Issue, next.PR
	record.PR = &pr
	record.Phase = PhaseApply
	pending, err := f.openRequest(ctx, pr)
	if err != nil {
		return err
	}
	if pending.Request == nil || pending.Reply == nil {
		// Answered, or withdrawn, since the PR was triaged.
		record.Phase = PhaseDone
		return nil
	}
	reply, decisions := pending.Reply, pending.Request.Decisions
	record.Applied = &journal.Applied{Ruling: reply.Ruling, Status: string(reply.Status), Fixed: []int{}}
	if reply.Status != ruling.Full {
		// Never nudged yet: a nudged reply that is still no full Ruling
		// waits on the human, and the Lane does not take its PR.
		if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, pr, ruling.Nudge(decisions, nudgeReason(reply.Parsed))); err != nil {
			return fmt.Errorf("nudging PR #%d's reply: %w", pr, err)
		}
		record.Applied.Nudged = true
		record.Phase = PhaseDone
		return nil
	}
	branch, err := f.Workspace.OpenBranch(ctx, number)
	if err != nil {
		return fmt.Errorf("opening the branch: %w", err)
	}
	record.Branch = branch
	normalized := ruling.Normalized(decisions)
	var ruled []fix.Ruled
	for i, d := range normalized {
		if chosen := chosenOption(d, reply.Answers[i+1]); chosen.Fix {
			path, line := threadPlace(d.Thread)
			ruled = append(ruled, fix.Ruled{Decision: i + 1, Title: d.Title, Scenario: d.Scenario, Now: d.Now, Chosen: chosen.Text, Cost: chosen.Cost, Path: path, Line: line})
			record.Applied.Fixed = append(record.Applied.Fixed, i+1)
		}
	}
	sha := ""
	if len(ruled) > 0 {
		if sha, err = f.applyFix(ctx, record, harness, number, branch, ruled); sha == "" {
			return err
		}
	}
	record.Phase = PhaseApply
	var results []ruling.Applied
	for i, d := range normalized {
		letter := reply.Answers[i+1]
		chosen := chosenOption(d, letter)
		result := ruling.Applied{N: i + 1, Letter: letter, Summary: summary(chosen.Text)}
		if chosen.Fix {
			result.SHA = sha
		}
		if ref := threadOf(d.Thread); ref.ID != "" {
			result.Thread = fmt.Sprintf("%s:%d", ref.Path, ref.Line)
			body, err := thread.Body(thread.MarkerRuling, ruling.ThreadReply(i+1, letter, result.Summary, result.SHA))
			if err != nil {
				return err
			}
			if err := f.GitHub.ReplyToThread(ctx, f.Repo.Owner, f.Repo.Name, pr, ref.Comment, body); err != nil {
				return fmt.Errorf("replying in thread %s with the Ruling: %w", ref.ID, err)
			}
			if err := f.GitHub.ResolveThread(ctx, ref.ID); err != nil {
				return fmt.Errorf("resolving thread %s on the Ruling: %w", ref.ID, err)
			}
		}
		results = append(results, result)
	}
	head := sha
	if head == "" {
		if head, err = f.Workspace.Head(ctx); err != nil {
			return fmt.Errorf("reading the PR's head: %w", err)
		}
		head = short(head)
	}
	// The label comes off after the comment is up: a Fire cut off between
	// the two leaves the PR still waiting, never silently unlabelled.
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, pr, ruling.ComposeApplied(results, head, reply.Ruling, "")); err != nil {
		return fmt.Errorf("posting PR #%d's applied Ruling: %w", pr, err)
	}
	if err := f.GitHub.RemoveLabel(ctx, f.Repo.Owner, f.Repo.Name, pr, state.LabelRuling); err != nil {
		return fmt.Errorf("the Ruling on PR #%d is applied, but %s could not be taken off: %w", pr, state.LabelRuling, err)
	}
	record.Phase = PhaseDone
	return nil
}

// applyFix has a Fix apply the chosen options that change code, on the
// ticket's branch, checked out, and under its Single-flight lock; Go commits
// and pushes.
// sha is the commit the fixes are in, short; empty when the Fire ends here,
// with err its error, if any.
func (f *Fire) applyFix(ctx context.Context, record *journal.Record, harness config.Harness, number int, branch string, ruled []fix.Ruled) (sha string, err error) {
	issue, err := f.GitHub.Issue(ctx, f.Repo.Owner, f.Repo.Name, number)
	if err != nil {
		return "", err
	}
	// Asked before the lock: a Fire told to wait leaves no label behind.
	record.Phase = PhaseGate
	var verdict *budget.Verdict
	var ok bool
	if verdict, ok, err = f.gate(ctx, record, fix.Spec().Name); !ok {
		return "", err
	}
	if err := f.lock(ctx, number); err != nil {
		return "", err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness, paused: slices.Contains(issue.Labels, state.LabelPaused)}
	if _, ok, err := f.fix(ctx, record, t, fix.Ruling(ruled), verdict); !ok {
		return "", err
	}
	head, err := f.Workspace.Head(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the commit the Ruling's fixes are in: %w", err)
	}
	return short(head), nil
}

// nudgeReason says why a reply is not a full Ruling.
func nudgeReason(p ruling.Parsed) string {
	if p.Status == ruling.Partial {
		missing := make([]string, len(p.Missing))
		for i, n := range p.Missing {
			missing[i] = strconv.Itoa(n)
		}
		return "no letter for decision " + strings.Join(missing, ", ")
	}
	return p.Reason
}

// chosenOption is a normalized decision's option of that letter.
func chosenOption(d ruling.Decision, letter string) ruling.Option {
	for _, o := range d.Options {
		if o.Letter == letter {
			return o
		}
	}
	return ruling.Option{}
}

// summary is an option's text as a sentence of the applied comment.
func summary(text string) string {
	text = strings.TrimSpace(text)
	if text != "" && !strings.ContainsAny(text[len(text)-1:], ".!?") {
		text += "."
	}
	return text
}

// threadRef is the thread a decision came from.
type threadRef struct {
	ID      string `json:"threadId"`
	Comment int64  `json:"commentDatabaseId"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
}

// threadOf is the thread a decision names; its ID is empty when it names
// none.
func threadOf(raw json.RawMessage) threadRef {
	var ref threadRef
	if len(raw) > 0 {
		json.Unmarshal(raw, &ref)
	}
	return ref
}

// threadPlace is where a decision's thread is; empty when it names none.
func threadPlace(raw json.RawMessage) (path string, line int) {
	ref := threadOf(raw)
	return ref.Path, ref.Line
}
