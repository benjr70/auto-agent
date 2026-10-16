package fire

import (
	"context"
	"fmt"
	"slices"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/thread"
)

// The phases of answering an Agent PR's review threads.
const (
	// PhaseRevise: reading the threads, and answering them after a Fix.
	PhaseRevise = "revise"
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
	open, err := f.unanswered(ctx, pr)
	if err != nil {
		return err
	}
	if len(open) == 0 {
		// Nothing for a Fix: the label is all there is to answer.
		record.Revise = &journal.Revise{Fixed: []string{}, Disputes: []journal.Dispute{}, Failing: []string{}}
		if err := f.answered(ctx, pr); err != nil {
			return err
		}
		record.Phase = PhaseDone
		return nil
	}
	// Asked before the lock: a Fire told to wait leaves no label behind.
	record.Phase = PhaseGate
	verdict, ok, err := f.gate(ctx, record, fix.Spec().Name)
	if !ok {
		return err
	}
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness, paused: slices.Contains(issue.Labels, state.LabelPaused)}
	if finished, err := f.revise(ctx, record, t, pr, open, verdict); !finished {
		return err
	}
	record.Phase = PhaseDone
	return nil
}

// revise answers the PR's open review threads, round by round: a Fix gets
// every thread still to be answered, whole; Go commits and pushes its work,
// then replies in each thread, resolves the ones it fixed and leaves the
// disputed ones open. A thread disputed and not yet answered waits on the
// Arbiter or the human, not on a Fix. When every thread is answered,
// AFK:revise comes off. Threads still open after rounds.revise rounds park
// the PR with AFK:revise-failed. open are the threads to answer first, and
// verdict the gate's answer for the first Fix. finished is false when the
// Fire ends here, with err its error, if any.
func (f *Fire) revise(ctx context.Context, record *journal.Record, t *ticket, pr int, open []thread.Thread, verdict *budget.Verdict) (finished bool, err error) {
	rev := &journal.Revise{Fixed: []string{}, Disputes: []journal.Dispute{}, Failing: []string{}}
	record.Revise = rev
	rounds := t.harness.Rounds.Revise
	for round := 1; ; round++ {
		record.Phase = PhaseRevise
		if round > 1 {
			if open, err = f.unanswered(ctx, pr); err != nil {
				return false, err
			}
		}
		if len(open) == 0 {
			return true, f.answered(ctx, pr)
		}
		if round > rounds {
			return f.reviseCap(ctx, record, pr, open, rounds)
		}
		rev.Rounds = round
		result, ok, err := f.fix(ctx, record, t, fix.ReviewThreads(briefOf(open)), verdict)
		verdict = nil
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

// unanswered are the PR's open review threads a Fix answers: every one but
// a Dispute still waiting on its answer.
func (f *Fire) unanswered(ctx context.Context, pr int) ([]thread.Thread, error) {
	listed, err := f.GitHub.ReviewThreads(ctx, f.Repo.Owner, f.Repo.Name, pr)
	if err != nil {
		return nil, fmt.Errorf("reading PR #%d's review threads: %w", pr, err)
	}
	open := []thread.Thread{}
	for _, raw := range listed {
		if t := thread.Read(raw); !t.Disputed() {
			open = append(open, t)
		}
	}
	return open, nil
}

// answered takes AFK:revise off a PR whose threads are all answered.
func (f *Fire) answered(ctx context.Context, pr int) error {
	if err := f.GitHub.RemoveLabel(ctx, f.Repo.Owner, f.Repo.Name, pr, state.LabelRevise); err != nil {
		return fmt.Errorf("taking %s off PR #%d, its threads answered: %w", state.LabelRevise, pr, err)
	}
	return nil
}

// briefOf is the review-threads brief's threads: each one whole, every
// comment in the voice it was written in.
func briefOf(threads []thread.Thread) []fix.Thread {
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
		brief = append(brief, fix.Thread{ID: t.ID, Path: t.Path, Line: t.Line, Authored: t.Authored, Comments: comments, Ruling: ruling})
	}
	return brief
}

// answer posts the Fix's reply in each thread, after its work is pushed,
// and resolves each thread it fixed. A disputed thread stays open, its
// Dispute recorded for whoever answers it. head is the commit the fixes are
// in.
func (f *Fire) answer(ctx context.Context, rev *journal.Revise, pr int, threads []thread.Thread, answers []fix.Answer, head string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
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
		body, err := thread.Body(marker, text)
		if err != nil {
			return err
		}
		if err := f.GitHub.ReplyToThread(ctx, owner, name, pr, t.First.ID, body); err != nil {
			return fmt.Errorf("replying in thread %s: %w", t.ID, err)
		}
		if a.Action == fix.Disputed {
			rev.Disputes = append(rev.Disputes, journal.Dispute{Thread: t.ID, Path: t.Path, Line: t.Line, Authored: t.Authored, Reply: a.Reply})
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
	owner, name := f.Repo.Owner, f.Repo.Name
	record.Phase = PhaseReviseFailed
	for _, t := range failing {
		body, err := thread.Body(thread.MarkerEscalate, state.ReviseEscalateReply(rounds))
		if err != nil {
			return false, err
		}
		if err := f.GitHub.ReplyToThread(ctx, owner, name, pr, t.First.ID, body); err != nil {
			return false, fmt.Errorf("replying in thread %s, still open at the round cap: %w", t.ID, err)
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

// short is a commit's short form, as a reply names it.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
