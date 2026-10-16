package fire

import (
	"context"
	"fmt"
	"slices"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/arbitrate"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/thread"
)

// The phases of answering an Agent PR's review threads.
const (
	// PhaseRevise: reading the threads, and answering them after a Fix.
	PhaseRevise = "revise"
	// PhaseArbitrate: the Arbiter ruling the Disputes.
	PhaseArbitrate = "arbitrate"
	// PhaseReviseFailed: threads still open at the round cap; the PR is
	// parked for a human.
	PhaseReviseFailed = "revise-failed"
)

// reconcileRevise answers the open review threads of an Agent PR handed
// back with AFK:revise. The ticket holds the Single-flight lock while it
// runs, as it does for any work.
func (f *Fire) reconcileRevise(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) (err error) {
	number, pr := next.Issue, next.PR
	record.PR = &pr
	issue, err := f.GitHub.Issue(ctx, f.Repo.Owner, f.Repo.Name, number)
	if err != nil {
		return err
	}
	branch, err := f.Workspace.OpenBranch(ctx, number)
	if err != nil {
		return fmt.Errorf("opening the branch: %w", err)
	}
	record.Branch = branch
	record.Phase = PhaseRevise
	record.Revise = newRevise()
	open, waiting, err := f.unanswered(ctx, record.Revise, pr)
	if err != nil {
		return err
	}
	if len(open) == 0 && len(waiting) == 0 {
		// Nothing for a Step: the label is all there is to answer.
		if err := f.answered(ctx, pr); err != nil {
			return err
		}
		record.Phase = PhaseDone
		return nil
	}
	// Asked before the lock: a Fire told to wait leaves no label behind.
	record.Phase = PhaseGate
	first := fix.Spec().Name
	if len(open) == 0 {
		first = arbitrate.Name
	}
	verdict, ok, err := f.gate(ctx, record, first)
	if !ok {
		return err
	}
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness, paused: slices.Contains(issue.Labels, state.LabelPaused)}
	if finished, err := f.revise(ctx, record, t, pr, open, waiting, verdict); !finished {
		return err
	}
	record.Phase = PhaseDone
	return nil
}

// revise answers the PR's open review threads, round by round: a Fix gets
// every thread still to be answered, whole; Go commits and pushes its work,
// then replies in each thread, resolves the ones it fixed and leaves the
// disputed ones open. A Dispute waits on the Arbiter, which rules every one
// waiting once per Fire, after the first Fix (or at once, when Disputes are
// all there is): its fix verdicts are the next round's work. When every
// thread is answered, AFK:revise comes off. Threads still open when no round
// remains park the PR with AFK:revise-failed. open and waiting are the
// threads to answer and the Disputes, as first read, and verdict the gate's
// answer for the first Step. finished is false when the Fire ends here, with
// err its error, if any.
func (f *Fire) revise(ctx context.Context, record *journal.Record, t *ticket, pr int, open, waiting []thread.Thread, verdict *budget.Verdict) (finished bool, err error) {
	rev := record.Revise
	rounds := t.harness.Rounds.Revise
	// ruledFixes are the Arbiter's fix verdicts no Fix has applied yet.
	arbitrated, ruledFixes := false, 0
	for round := 1; ; round++ {
		record.Phase = PhaseRevise
		if round > 1 {
			if open, waiting, err = f.unanswered(ctx, rev, pr); err != nil {
				return false, err
			}
		}
		if !arbitrated && len(waiting) > 0 && (round > 1 || len(open) == 0) {
			arbitrated = true
			fixes, ok, err := f.arbitrate(ctx, record, t, pr, waiting, verdict)
			verdict = nil
			if !ok {
				return false, err
			}
			if ruledFixes = fixes; fixes > 0 {
				// Its fix verdicts are threads to answer again.
				if open, waiting, err = f.unanswered(ctx, rev, pr); err != nil {
					return false, err
				}
			}
		}
		if len(open) == 0 {
			return true, f.answered(ctx, pr)
		}
		if round > 1 {
			if _, ok := thread.NextRound(round-1, rounds, ruledFixes); !ok {
				return f.reviseCap(ctx, record, pr, open, round-1)
			}
		}
		rev.Rounds = round
		result, ok, err := f.fix(ctx, record, t, fix.ReviewThreads(briefThreads(open)), verdict)
		verdict, ruledFixes = nil, 0
		if !ok {
			if record.Phase == PhaseFailed {
				// The ticket is a human's now; so are its threads, or the
				// reconcile Lane would take the PR for ever.
				f.giveUpRevise(ctx, record, pr, err)
			}
			return false, err
		}
		record.Phase = PhaseRevise
		// The replies name the commit the fixes are in: the one just pushed,
		// or the head, when the fixes were already on it.
		head, err := f.Workspace.Head(ctx)
		if err != nil {
			return false, fmt.Errorf("reading the commit the fixes are in: %w", err)
		}
		if err := f.answer(ctx, rev, pr, open, result.Threads, short(head)); err != nil {
			return false, err
		}
	}
}

// unanswered reads the PR's open review threads: open are the ones a Fix
// answers, waiting the Disputes no one has answered yet, which are noted in
// rev as GitHub has them now.
func (f *Fire) unanswered(ctx context.Context, rev *journal.Revise, pr int) (open, waiting []thread.Thread, err error) {
	listed, err := f.GitHub.ReviewThreads(ctx, f.Repo.Owner, f.Repo.Name, pr)
	if err != nil {
		return nil, nil, fmt.Errorf("reading PR #%d's review threads: %w", pr, err)
	}
	open, waiting = []thread.Thread{}, []thread.Thread{}
	rev.Waiting = []journal.Dispute{}
	for _, raw := range listed {
		t := thread.Read(raw)
		if !t.Disputed() {
			open = append(open, t)
			continue
		}
		waiting = append(waiting, t)
		rev.Waiting = append(rev.Waiting, journal.Dispute{Thread: t.ID, Path: t.Path, Line: t.Line, Authored: t.Authored, Reply: t.Dispute()})
	}
	return open, waiting, nil
}

// arbitrate runs the Arbiter on the Disputes waiting, and acts on its
// verdicts: a fix is posted in the thread, binding on the next Fix; a
// dismissal is posted and the thread resolved, with no commit; an ambiguity,
// or a thread left unruled, is recorded for the Ruling request, and the
// thread left as it is. fixes counts the fix verdicts. ok is false when the
// Fire ends here, with err its error, if any. verdict is the gate's answer
// for the Step, when it was asked already.
func (f *Fire) arbitrate(ctx context.Context, record *journal.Record, t *ticket, pr int, waiting []thread.Thread, verdict *budget.Verdict) (fixes int, ok bool, err error) {
	spec, rev := arbitrate.Spec(t.commands()), record.Revise
	if verdict == nil {
		if verdict, ok, err = f.gate(ctx, record, spec.Name); !ok {
			return 0, false, err
		}
	}
	record.Phase = PhaseArbitrate
	disputes := make([]arbitrate.Dispute, len(waiting))
	for i, w := range waiting {
		disputes[i] = arbitrate.Dispute{Thread: w.ID, Path: w.Path, Line: w.Line, Authored: w.Authored, Finding: w.First.Body, Dispute: w.Dispute()}
	}
	input := arbitrate.Input{PR: pr, Branch: t.branch, Base: f.Workspace.Base(), Issue: t.stepIssue(), Commands: t.commands(), Disputes: disputes}
	if input.Spec, err = f.parentSpec(ctx, t.issue); err != nil {
		record.Warnings = append(record.Warnings, err.Error())
	}
	call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: input}
	// Go reads every verdict it can act on; the rest are unruled, never a
	// failed result.
	result, run := callStep(ctx, f, record, spec, call, *verdict, func(arbitrate.Result) error { return nil })
	// The Arbiter is read-only: whatever its test runs left in the checkout
	// is nobody's to commit.
	if err := f.Workspace.Discard(ctx); err != nil {
		return 0, false, fmt.Errorf("discarding what the Arbiter left in the checkout: %w", err)
	}
	if stop, err := f.settle(ctx, record, t, spec.Name, run, ""); stop {
		if record.Phase == PhaseFailed {
			f.giveUpRevise(ctx, record, pr, err)
		}
		return 0, false, err
	}
	rev.Arbiter = arbitrate.Read(disputes, result)
	for i, v := range rev.Arbiter {
		w := waiting[i]
		switch v.Verdict {
		case arbitrate.Fix:
			if err := f.reply(ctx, pr, w, thread.MarkerArbiter, thread.ArbiterFix(v.Text)); err != nil {
				return fixes, false, err
			}
			fixes++
		case arbitrate.Dismiss:
			if err := f.reply(ctx, pr, w, thread.MarkerArbiter, thread.ArbiterDismissed(v.Text)); err != nil {
				return fixes, false, err
			}
			if err := f.GitHub.ResolveThread(ctx, w.ID); err != nil {
				return fixes, false, fmt.Errorf("resolving thread %s, dismissed: %w", w.ID, err)
			}
			rev.Dismissed = append(rev.Dismissed, w.ID)
		default:
			rev.Decisions = append(rev.Decisions, v)
		}
	}
	return fixes, true, nil
}

// reply posts a reply in a thread, under one of the Daemon's markers.
func (f *Fire) reply(ctx context.Context, pr int, t thread.Thread, marker, text string) error {
	body, err := thread.Body(marker, text)
	if err != nil {
		return err
	}
	if err := f.GitHub.ReplyToThread(ctx, f.Repo.Owner, f.Repo.Name, pr, t.First.ID, body); err != nil {
		return fmt.Errorf("replying in thread %s: %w", t.ID, err)
	}
	return nil
}

// answered takes AFK:revise off a PR whose threads are all answered.
func (f *Fire) answered(ctx context.Context, pr int) error {
	if err := f.GitHub.RemoveLabel(ctx, f.Repo.Owner, f.Repo.Name, pr, state.LabelRevise); err != nil {
		return fmt.Errorf("taking %s off PR #%d, its threads answered: %w", state.LabelRevise, pr, err)
	}
	return nil
}

// briefThreads are the review-threads brief's threads: each one whole, every
// comment in the voice it was written in.
func briefThreads(threads []thread.Thread) []fix.Thread {
	brief := make([]fix.Thread, 0, len(threads))
	for _, t := range threads {
		first := fix.FromHuman
		if t.Authored == thread.Bot {
			first = fix.FromReview
		}
		comments := []fix.Comment{{From: first, Body: t.First.Body}}
		for _, r := range t.Replies {
			from := fix.FromHuman
			if r.Daemon {
				from = fix.FromDaemon
			}
			comments = append(comments, fix.Comment{From: from, Body: r.Body})
		}
		ruling, _ := t.Ruling()
		instruction, _ := t.Instruction()
		brief = append(brief, fix.Thread{ID: t.ID, Path: t.Path, Line: t.Line, Authored: t.Authored, Comments: comments, Ruling: ruling, Arbiter: instruction})
	}
	return brief
}

// answer posts the Fix's reply in each thread, after its work is pushed,
// and resolves each thread it fixed. A disputed thread stays open, its
// Dispute recorded for whoever answers it; a Dispute of the Arbiter's fix
// says it cannot be applied, and is a decision for the human. head is the
// commit the fixes are in.
func (f *Fire) answer(ctx context.Context, rev *journal.Revise, pr int, threads []thread.Thread, answers []fix.Answer, head string) error {
	for _, t := range threads {
		i := slices.IndexFunc(answers, func(a fix.Answer) bool { return a.Thread == t.ID })
		if i < 0 {
			// The result check makes this impossible; never resolve a thread
			// nobody answered.
			return fmt.Errorf("the Fix did not answer thread %s", t.ID)
		}
		a := answers[i]
		_, ruled := t.Ruling()
		var marker, text string
		switch {
		case a.Action == fix.Disputed:
			marker, text = thread.MarkerDispute, "disputed: "+a.Reply
		case a.Action == fix.NoChange:
			marker, text = thread.MarkerRuling, "ruling applied: no change — "+a.Reply
		case ruled:
			marker, text = thread.MarkerRuling, fmt.Sprintf("ruling applied in %s: %s", head, a.Reply)
		default:
			marker, text = thread.MarkerFix, fmt.Sprintf("fixed in %s: %s", head, a.Reply)
		}
		if err := f.reply(ctx, pr, t, marker, text); err != nil {
			return err
		}
		if a.Action == fix.Disputed {
			rev.Disputes = append(rev.Disputes, journal.Dispute{Thread: t.ID, Path: t.Path, Line: t.Line, Authored: t.Authored, Reply: a.Reply})
			if instruction, binding := t.Instruction(); binding {
				rev.Decisions = append(rev.Decisions, arbitrate.Verdict{Thread: t.ID, Authored: t.Authored, Verdict: arbitrate.Cannot, Text: a.Reply,
					Reason: "the Fix could not apply the Arbiter's fix: " + instruction})
			}
			continue
		}
		if err := f.GitHub.ResolveThread(ctx, t.ID); err != nil {
			return fmt.Errorf("resolving thread %s: %w", t.ID, err)
		}
		rev.Fixed = append(rev.Fixed, t.ID)
	}
	return nil
}

// reviseCap parks a PR whose threads are still open after rounds.revise
// rounds, as it always was: a marked reply in each one, AFK:revise-failed
// for AFK:revise, and a comment saying how to retry. A parked PR is not a
// failed Fire.
func (f *Fire) reviseCap(ctx context.Context, record *journal.Record, pr int, failing []thread.Thread, rounds int) (finished bool, err error) {
	record.Phase = PhaseReviseFailed
	for _, t := range failing {
		if err := f.reply(ctx, pr, t, thread.MarkerEscalate, state.ReviseCapReply(rounds)); err != nil {
			return false, fmt.Errorf("at the round cap: %w", err)
		}
		record.Revise.Failing = append(record.Revise.Failing, t.ID)
	}
	if err := f.parkPR(ctx, record, pr, state.ReviseCapComment(len(failing), rounds, f.Now())); err != nil {
		return false, err
	}
	return true, nil
}

// giveUpRevise parks a PR whose ticket used its attempts answering the
// threads: a human takes over both.
func (f *Fire) giveUpRevise(ctx context.Context, record *journal.Record, pr int, why error) {
	if err := f.parkPR(ctx, record, pr, state.ReviseGiveUpComment(why)); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("PR #%d's threads were given up, but it could not be parked: %v", pr, err))
	}
}

// parkPR leaves a PR to a human: AFK:revise-failed on, AFK:revise off, and
// a comment saying why. The park label goes on first: a PR that has both is
// parked, one that has neither is reconciled no more.
func (f *Fire) parkPR(ctx context.Context, record *journal.Record, pr int, comment string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	if err := f.GitHub.AddLabels(ctx, owner, name, pr, state.LabelReviseFailed); err != nil {
		return fmt.Errorf("parking PR #%d: %w", pr, err)
	}
	if err := f.GitHub.RemoveLabel(ctx, owner, name, pr, state.LabelRevise); err != nil {
		return fmt.Errorf("parking PR #%d: taking %s off: %w", pr, state.LabelRevise, err)
	}
	if err := f.GitHub.CreateComment(ctx, owner, name, pr, comment); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("PR #%d is parked, but the comment saying so was not posted: %v", pr, err))
	}
	return nil
}

// newRevise is the record of threads answered, with nothing answered yet.
func newRevise() *journal.Revise {
	return &journal.Revise{Fixed: []string{}, Disputes: []journal.Dispute{}, Waiting: []journal.Dispute{},
		Arbiter: []arbitrate.Verdict{}, Dismissed: []string{}, Decisions: []arbitrate.Verdict{}, Failing: []string{}}
}

// short is a commit's short form, as a reply names it.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
