package fire

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/ci"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step/arbitrate"
	"github.com/benjr70/auto-agent/internal/step/fix"
)

// The phases of CI.
const (
	// PhaseCI: waiting on the PR's checks.
	PhaseCI = "ci"
	// PhaseChecksFailed: CI still red when no fix round remains; the PR is
	// parked for a human.
	PhaseChecksFailed = "checks-failed"
)

// The wait on CI: a read about every minute for up to 45 minutes, as the
// harness has always waited, and a few reads before a commit with no check
// at all is taken to have none coming.
const (
	ciInterval = time.Minute
	ciReads    = 45
	ciGrace    = 5
	// ciLogLines is how much of a failed job's log is kept.
	ciLogLines = 200
	// ciUnreadable is how many reads in a row may fail before the wait
	// gives up: one is a flake, three is GitHub or the token gone.
	ciUnreadable = 3
)

// waitCI waits for the checks on a commit to settle, with no Step: Go reads
// them on its own clock, at most reads times. A failed run is gathered with
// the end of each failed job's log. The wait is in the Fire record whatever
// its end; a wait cut short by its cap is not an error, and the next Fire
// reads again. Every read counts against the cap, a failed one too.
func (f *Fire) waitCI(ctx context.Context, record *journal.Record, required []string, head string, reads int) (*journal.CI, error) {
	record.Phase = PhaseCI
	wait := &journal.CI{Head: head, Failed: []journal.FailedCheck{}}
	record.CI = wait
	unreadable := 0
	for try := 1; ; try++ {
		checks, err := f.GitHub.Checks(ctx, f.Repo.Owner, f.Repo.Name, head)
		if err != nil {
			if unreadable++; unreadable == ciUnreadable || try == reads {
				wait.Result = ci.Error
				return wait, fmt.Errorf("reading the checks on %s: %w", short(head), err)
			}
		} else {
			unreadable = 0
			wait.Reads++
			switch result := ci.Settle(checks, required); {
			case result == ci.Green:
				wait.Result = result
				return wait, nil
			case result == ci.Fail:
				wait.Result = result
				wait.Failed = f.failedChecks(ctx, ci.Failed(checks))
				return wait, nil
			case result == ci.None && (wait.Reads >= ciGrace || try == reads):
				wait.Result = result
				return wait, nil
			case try == reads:
				// A wait of one read says where CI stands; the whole wait
				// timed out.
				wait.Result = ci.Pending
				if reads == ciReads {
					wait.Result = ci.Timeout
				}
				return wait, nil
			}
		}
		if err := f.sleep(ctx, ciInterval); err != nil {
			return wait, err
		}
	}
}

// sleep waits for d, or until ctx is done.
func (f *Fire) sleep(ctx context.Context, d time.Duration) error {
	if f.Sleep == nil {
		return Sleep(ctx, d)
	}
	return f.Sleep(ctx, d)
}

// failedChecks are the failed checks with the end of each one's log, where
// GitHub Actions ran it.
func (f *Fire) failedChecks(ctx context.Context, checks []github.Check) []journal.FailedCheck {
	failed := make([]journal.FailedCheck, 0, len(checks))
	for _, c := range checks {
		check := journal.FailedCheck{Name: c.Name, Link: c.Link, Log: "(log unavailable: not a GitHub Actions job)"}
		if c.Job != 0 {
			if log, err := f.GitHub.JobLog(ctx, f.Repo.Owner, f.Repo.Name, c.Job); err != nil {
				check.Log = "(log unavailable: " + err.Error() + ")"
			} else {
				check.Log = ci.Tail(log, ciLogLines)
			}
		}
		failed = append(failed, check)
	}
	return failed
}

// evidence is what a wait on CI, and the Verification round after it,
// say of the head, as a Ruling request and an applied Ruling state it.
// round is nil when no round ran.
func evidence(wait *journal.CI, round *journal.Verify) string {
	manual := "manual verification not run"
	switch {
	case round == nil:
	case round.Bootstrap:
		manual = "manual verification left to a human (Bootstrap state)"
	case round.Infra != "":
		manual = "manual verification could not run (infrastructure error)"
	default:
		manual = "manual verification " + strings.TrimPrefix(round.Line, "manual-verify: ")
	}
	switch wait.Result {
	case ci.Green:
		return "CI green, " + manual
	case ci.Fail:
		names := make([]string, len(wait.Failed))
		for i, c := range wait.Failed {
			names[i] = c.Name
		}
		return fmt.Sprintf("CI red (%d failing: %s), %s", len(names), strings.Join(names, ", "), manual)
	case ci.None:
		return "no CI checks ran, " + manual
	case ci.Timeout:
		return fmt.Sprintf("CI still running after %d minutes, %s", int(ciReads*ciInterval/time.Minute), manual)
	case ci.Pending:
		return "CI still running, " + manual
	}
	return "CI could not be read, " + manual
}

// afterPush is what follows a push to an Agent PR: the wait on its CI, the
// Verification round once CI has nothing red to say, when verify asks for
// one, then the Ruling request for whatever needs the human, stating what
// CI and the round said. finished is false when the Fire ends here, with
// err its error, if any.
func (f *Fire) afterPush(ctx context.Context, record *journal.Record, t *ticket, pr int, roundOwed bool, ambiguities []finding.Finding, decisions []arbitrate.Verdict) (finished bool, err error) {
	head, err := f.Workspace.Head(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the PR's head: %w", err)
	}
	wait, err := f.waitCI(ctx, record, t.harness.RequiredChecks, head, ciReads)
	if err != nil {
		return false, err
	}
	if wait, finished, err = f.fixCI(ctx, record, t, pr, wait); !finished {
		return false, err
	}
	if roundOwed && verifiable(wait) {
		if wait, finished, err = f.verify(ctx, record, t, pr, wait); !finished {
			return false, err
		}
	}
	if len(ambiguities)+len(decisions) == 0 {
		return true, nil
	}
	return f.requestRuling(ctx, record, t, pr, ambiguities, decisions, evidence(wait, record.Verify))
}

// fixCI answers a failing CI run on the PR, round by round: a Fix gets the
// failed checks and the end of their logs, Go commits and pushes its work
// (never by force), and waits on CI again, up to rounds.pr_watch rounds,
// fresh each Fire. A Fix that changes nothing and names a flake has the
// failed jobs run again; one that changes nothing and names none has not
// answered, and is a failed attempt. A run still red when no round remains
// parks the PR, as always: draft, AFK:checks-failed, and a comment on the
// ticket. wait is the last wait; finished is false when the Fire ends here,
// with err its error, if any.
func (f *Fire) fixCI(ctx context.Context, record *journal.Record, t *ticket, pr int, wait *journal.CI) (_ *journal.CI, finished bool, err error) {
	rounds := t.harness.Rounds.PRWatch
	for round := 1; wait.Result == ci.Fail; round++ {
		if round > rounds {
			record.Phase = PhaseChecksFailed
			if err := f.parkChecks(ctx, record, t, pr, state.ChecksFailedComment(rounds, pr)); err != nil {
				return wait, false, err
			}
			return wait, true, nil
		}
		record.CIRounds = round
		failed := make([]fix.FailedCheck, len(wait.Failed))
		for i, c := range wait.Failed {
			failed[i] = fix.FailedCheck{Name: c.Name, Link: c.Link, Log: c.Log}
		}
		result, ok, err := f.fix(ctx, record, t, fix.CIFailure(failed), nil)
		if !ok {
			return wait, false, f.gaveUp(ctx, record, t, pr, err)
		}
		head, err := f.Workspace.Head(ctx)
		if err != nil {
			return wait, false, fmt.Errorf("reading the PR's head: %w", err)
		}
		if head == wait.Head {
			if strings.TrimSpace(result.Flake) == "" {
				err := f.failedAttempt(ctx, record, t.issue.Number, fix.Spec().Name, "the Fix changed nothing for the failing CI run and named no flake")
				return wait, false, f.gaveUp(ctx, record, t, pr, err)
			}
			if err := f.rerun(ctx, wait.Failed); err != nil {
				return wait, false, err
			}
			// The jobs run again on the same commit: the first read after
			// asking must not find the failure they are replacing.
			if err := f.sleep(ctx, ciInterval); err != nil {
				return wait, false, err
			}
		}
		if wait, err = f.waitCI(ctx, record, t.harness.RequiredChecks, head, ciReads); err != nil {
			return wait, false, err
		}
	}
	return wait, true, nil
}

// rerun runs the failed GitHub Actions jobs of a run again, once per
// workflow run; a check that is not an Actions job cannot be.
func (f *Fire) rerun(ctx context.Context, failed []journal.FailedCheck) error {
	done := map[int64]bool{}
	for _, c := range failed {
		run := github.RunOf(c.Link)
		if run == 0 || done[run] {
			continue
		}
		done[run] = true
		if err := f.GitHub.RerunFailedJobs(ctx, f.Repo.Owner, f.Repo.Name, run); err != nil {
			return fmt.Errorf("running the failed jobs of run %d again: %w", run, err)
		}
	}
	return nil
}

// gaveUp is the error of a Fire whose Fix ended it: at the ticket's attempt
// cap, the PR is parked too, or the reconcile Lane would take it for ever.
func (f *Fire) gaveUp(ctx context.Context, record *journal.Record, t *ticket, pr int, err error) error {
	if record.Phase == PhaseFailed {
		if parkErr := f.parkChecks(ctx, record, t, pr, state.ChecksGiveUpComment(pr, err)); parkErr != nil {
			record.Warnings = append(record.Warnings, parkErr.Error())
		}
	}
	return err
}

// parkChecks parks a PR whose CI could not be made green: a draft again,
// AFK:checks-failed on it, and comment on its ticket.
func (f *Fire) parkChecks(ctx context.Context, record *journal.Record, t *ticket, pr int, comment string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	current, err := f.GitHub.PullRequest(ctx, owner, name, pr)
	if err != nil {
		return fmt.Errorf("parking PR #%d: %w", pr, err)
	}
	if !current.Draft {
		if err := f.GitHub.MarkDraft(ctx, current); err != nil {
			return fmt.Errorf("parking PR #%d: marking it draft: %w", pr, err)
		}
	}
	if err := f.GitHub.AddLabels(ctx, owner, name, pr, state.LabelChecksFailed); err != nil {
		return fmt.Errorf("parking PR #%d: %w", pr, err)
	}
	record.ParkedPR = state.LabelChecksFailed
	if err := f.GitHub.CreateComment(ctx, owner, name, t.issue.Number, comment); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("PR #%d is parked, but the comment saying so on #%d was not posted: %v", pr, t.issue.Number, err))
	}
	return nil
}

// reconcileCI reads the CI of an Agent PR whose review ran and whose
// verification never finished, once: the Daemon's next Fire reads it
// again, so no Fire here waits on CI it did not push for. A red run is
// fixed, under the ticket's lock, and each fix waited on. A PR with nothing
// red and no Verification round yet has its round. The review's product
// ambiguities are asked when no Ruling request ever asked them: the Fire
// that reviewed the PR was cut off while it waited on CI.
func (f *Fire) reconcileCI(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) (err error) {
	number, pr := next.Issue, next.PR
	record.PR = &pr
	listed, err := f.GitHub.PullRequest(ctx, f.Repo.Owner, f.Repo.Name, pr)
	if err != nil {
		return fmt.Errorf("reading PR #%d's head: %w", pr, err)
	}
	comments, err := f.GitHub.Comments(ctx, f.Repo.Owner, f.Repo.Name, pr)
	if err != nil {
		return fmt.Errorf("reading PR #%d's review: %w", pr, err)
	}
	wait, err := f.waitCI(ctx, record, harness.RequiredChecks, listed.HeadSHA, 1)
	if err != nil {
		return err
	}
	ambiguities := unasked(comments)
	_, verified := state.LastRound(comments, f.Login)
	if wait.Result != ci.Fail && len(ambiguities) == 0 && (verified || !verifiable(wait)) {
		record.Phase = PhaseDone
		return nil
	}
	issue, err := f.GitHub.Issue(ctx, f.Repo.Owner, f.Repo.Name, number)
	if err != nil {
		return err
	}
	branch, err := f.Workspace.OpenBranch(ctx, number)
	if err != nil {
		return fmt.Errorf("opening the branch: %w", err)
	}
	record.Branch = branch
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness, paused: slices.Contains(issue.Labels, state.LabelPaused)}
	var finished bool
	if wait, finished, err = f.fixCI(ctx, record, t, pr, wait); !finished {
		return err
	}
	if !verified && verifiable(wait) {
		if wait, finished, err = f.verify(ctx, record, t, pr, wait); !finished {
			return err
		}
	}
	if len(ambiguities) > 0 {
		if finished, err := f.requestRuling(ctx, record, t, pr, ambiguities, nil, evidence(wait, record.Verify)); !finished {
			return err
		}
	}
	record.Phase = PhaseDone
	return nil
}

// unasked are the product ambiguities of the PR's review that no Ruling
// request has asked: none came after the review's comment.
func unasked(comments []github.Comment) []finding.Finding {
	at := slices.IndexFunc(comments, func(c github.Comment) bool { return finding.IsDone(c.Body) })
	if at < 0 {
		return nil
	}
	for _, c := range comments[at+1:] {
		if strings.HasPrefix(c.Body, ruling.RequestMarker) || strings.HasPrefix(c.Body, ruling.AppliedMarker) {
			return nil
		}
	}
	return finding.Ambiguities(comments[at].Body)
}
