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
)

// PhaseCI: waiting on the PR's checks.
const PhaseCI = "ci"

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
	sleep := f.Sleep
	if sleep == nil {
		sleep = Sleep
	}
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
		if err := sleep(ctx, ciInterval); err != nil {
			return wait, err
		}
	}
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

// evidence is what a wait on CI says of the head, as a Ruling request and an
// applied Ruling state it.
func evidence(wait *journal.CI) string {
	const manual = "manual verification not run by this build"
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

// afterPush is what follows a push to an Agent PR: the wait on its CI, then the
// Ruling request for whatever needs the human, stating what CI said.
// finished is false when the Fire ends here, with err its error, if any.
func (f *Fire) afterPush(ctx context.Context, record *journal.Record, t *ticket, pr int, ambiguities []finding.Finding, decisions []arbitrate.Verdict) (finished bool, err error) {
	head, err := f.Workspace.Head(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the PR's head: %w", err)
	}
	wait, err := f.waitCI(ctx, record, t.harness.RequiredChecks, head, ciReads)
	if err != nil {
		return false, err
	}
	if len(ambiguities)+len(decisions) == 0 {
		return true, nil
	}
	return f.requestRuling(ctx, record, t, pr, ambiguities, decisions, evidence(wait))
}

// reconcileCI reads the CI of an Agent PR whose review ran and whose
// verification never finished, once: the Daemon's next Fire reads it
// again, so no Fire here waits long. The review's product ambiguities are
// asked when no Ruling request ever asked them: the Fire that reviewed the
// PR was cut off while it waited on CI.
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
	if len(ambiguities) == 0 {
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
	if finished, err := f.requestRuling(ctx, record, t, pr, ambiguities, nil, evidence(wait)); !finished {
		return err
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
