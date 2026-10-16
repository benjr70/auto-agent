package fire

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/arbitrate"
	"github.com/benjr70/auto-agent/internal/step/rulingrequest"
	"github.com/benjr70/auto-agent/internal/thread"
)

// PhaseRuling: asking the human, in a Ruling request.
const PhaseRuling = "ruling"

// parkedEvidence is what a Ruling request on a PR parked at the round cap
// says of its verification.
const parkedEvidence = "PR parked (" + state.LabelReviseFailed + "): verification not re-run"

// asked is one thing a Ruling request asks about, with the thread it came
// from, as a decision names it; nil for one on no thread.
type asked struct {
	item   rulingrequest.Item
	thread json.RawMessage
}

// requestRuling asks the human, in one Ruling request, about the review's
// product ambiguities and the decisions the Arbiter and the Fix left: the
// Arbiter's drafts stand when the human can answer each as it is, and the
// Write Ruling request Step writes the decisions otherwise. A request still
// open is superseded, its decisions carried first; one the human has
// answered is the next Fire's to apply, and nothing is asked over it. Go
// posts the request, then applies AFK:ruling, and the Fire ends: the PR
// waits on the human. evidence is what the head's verification said.
// finished is false when the Fire ends here, with err its error, if any.
func (f *Fire) requestRuling(ctx context.Context, record *journal.Record, t *ticket, pr int, ambiguities []finding.Finding, decisions []arbitrate.Verdict, evidence string) (finished bool, err error) {
	owner, name := f.Repo.Owner, f.Repo.Name
	record.Phase = PhaseRuling
	pending, err := f.openRequest(ctx, pr)
	if err != nil {
		return false, err
	}
	if pending.Replied() {
		record.Warnings = append(record.Warnings, fmt.Sprintf("PR #%d's Ruling request is answered and not yet applied: what this Fire would ask is asked after it is", pr))
		return true, nil
	}
	items := reviewItems(ambiguities)
	more, err := f.threadItems(ctx, pr, decisions)
	if err != nil {
		return false, err
	}
	items = append(items, more...)
	var open []ruling.Decision
	var supersedes int64
	if pending.Request != nil {
		open, supersedes = pending.Request.Decisions, pending.Request.Comment
	}
	if len(items) == 0 {
		return true, nil
	}
	written, byStep, ok, err := f.writeDecisions(ctx, record, t, pr, items)
	if !ok {
		return false, err
	}
	head, err := f.Workspace.Head(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the commit the Ruling request is about: %w", err)
	}
	body, err := ruling.Compose(append(append([]ruling.Decision{}, open...), written...), short(head), evidence, supersedes)
	if err != nil {
		return false, err
	}
	// The label follows the comment: a PR labelled with no request to answer
	// would wait for ever.
	if err := f.GitHub.CreateComment(ctx, owner, name, pr, body); err != nil {
		return false, fmt.Errorf("posting PR #%d's Ruling request: %w", pr, err)
	}
	record.Ruling = &journal.Ruling{Head: short(head), Decisions: len(open) + len(written), New: len(written), Supersedes: supersedes, Written: byStep}
	if err := f.GitHub.AddLabels(ctx, owner, name, pr, state.LabelRuling); err != nil {
		return false, fmt.Errorf("PR #%d's Ruling request is posted, but %s could not be applied: %w", pr, state.LabelRuling, err)
	}
	return true, nil
}

// writeDecisions are the items' decisions, in order, each carrying its
// thread: the Arbiter's drafts when every item has one the human can
// answer, else the Write Ruling request Step's, and then byStep is set. ok
// is false when the Fire ends here, with err its error, if any.
func (f *Fire) writeDecisions(ctx context.Context, record *journal.Record, t *ticket, pr int, items []asked) (decisions []ruling.Decision, byStep, ok bool, err error) {
	plain := make([]rulingrequest.Item, len(items))
	for i, a := range items {
		plain[i] = a.item
	}
	decisions, drafted := rulingrequest.Drafted(plain)
	if !drafted {
		spec := rulingrequest.Spec(t.commands())
		verdict, ok, err := f.gate(ctx, record, spec.Name)
		if !ok {
			return nil, false, false, err
		}
		record.Phase = PhaseRuling
		input := rulingrequest.Input{PR: pr, Branch: t.branch, Base: f.Workspace.Base(), Issue: t.stepIssue(), Commands: t.commands(), Items: plain}
		if input.Spec, err = f.parentSpec(ctx, t.issue); err != nil {
			record.Warnings = append(record.Warnings, err.Error())
		}
		call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: input}
		result, run := callStep(ctx, f, record, spec, call, *verdict, func(r rulingrequest.Result) error { return r.Check(plain) })
		// The Step is read-only: whatever its test runs left in the checkout
		// is nobody's to commit.
		if err := f.Workspace.Discard(ctx); err != nil {
			return nil, false, false, fmt.Errorf("discarding what the Write Ruling request Step left in the checkout: %w", err)
		}
		if stop, err := f.settle(ctx, record, t, spec.Name, run, ""); stop {
			return nil, false, false, err
		}
		decisions = result.Ordered()
	}
	for i := range decisions {
		decisions[i].Thread = items[i].thread
	}
	return decisions, !drafted, true, nil
}

// reviewItems are the review's product ambiguities, as the human is asked
// about them: on no thread.
func reviewItems(ambiguities []finding.Finding) []asked {
	items := make([]asked, 0, len(ambiguities))
	for _, a := range ambiguities {
		text := a.Summary
		if s := strings.TrimSpace(a.FailureScenario); s != "" {
			text += "\n\nReadings: " + s
		}
		if q := strings.TrimSpace(a.QuotedRequirement); q != "" {
			text += "\n\nRequirement: " + q
		}
		items = append(items, asked{item: rulingrequest.Item{Kind: rulingrequest.KindProductAmbiguity, Path: a.Path, Line: a.Line, Finding: text}})
	}
	return items
}

// itemKinds are the item kinds of the decisions the Arbiter and the Fix
// leave.
var itemKinds = map[string]string{
	arbitrate.Ambiguity: rulingrequest.KindAmbiguity,
	arbitrate.Unruled:   rulingrequest.KindUnruled,
	arbitrate.Cannot:    rulingrequest.KindCannot,
}

// threadItems are the decisions the Arbiter and the Fix left, each read
// with its thread as GitHub has it now. A thread no longer open was settled
// some other way, and is not asked about.
func (f *Fire) threadItems(ctx context.Context, pr int, decisions []arbitrate.Verdict) ([]asked, error) {
	if len(decisions) == 0 {
		return nil, nil
	}
	listed, err := f.GitHub.ReviewThreads(ctx, f.Repo.Owner, f.Repo.Name, pr)
	if err != nil {
		return nil, fmt.Errorf("reading PR #%d's review threads: %w", pr, err)
	}
	items := []asked{}
	for _, d := range decisions {
		i := slices.IndexFunc(listed, func(raw github.ReviewThread) bool { return raw.ID == d.Thread })
		if i < 0 {
			continue
		}
		t := thread.Read(listed[i])
		item := rulingrequest.Item{Kind: itemKinds[d.Verdict], Path: t.Path, Line: t.Line, Authored: t.Authored, Finding: t.First.Body, Dispute: t.Dispute(), Note: d.Reason, Draft: d.Decision}
		ref, err := json.Marshal(map[string]any{"threadId": t.ID, "commentDatabaseId": t.First.ID, "path": t.Path, "line": t.Line})
		if err != nil {
			return nil, err
		}
		items = append(items, asked{item: item, thread: ref})
	}
	return items, nil
}

// openRequest is what the PR's comments say of its Ruling request.
func (f *Fire) openRequest(ctx context.Context, pr int) (ruling.Pending, error) {
	comments, err := f.GitHub.Comments(ctx, f.Repo.Owner, f.Repo.Name, pr)
	if err == nil {
		var pending ruling.Pending
		if pending, err = ruling.Read(comments, f.Login); err == nil {
			return pending, nil
		}
	}
	return ruling.Pending{}, fmt.Errorf("reading PR #%d's Ruling request: %w", pr, err)
}

// askedThreads are the threads the PR's open Ruling request asks about.
func (f *Fire) askedThreads(ctx context.Context, pr int) ([]string, error) {
	pending, err := f.openRequest(ctx, pr)
	if err != nil || pending.Request == nil {
		return []string{}, err
	}
	ids := []string{}
	for _, d := range pending.Request.Decisions {
		if id := threadOf(d.Thread).ID; id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
