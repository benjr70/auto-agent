package fire

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/ci"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
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

// The ends of a wait the checks did not settle.
const (
	ciTimeout = "timeout"
	ciError   = "error"
)

// waitCI waits for the checks on a commit to settle, with no Step: Go reads
// them on its own clock. A failed run is gathered with the end of each
// failed job's log. The wait is in the Fire record whatever its end; a wait
// cut short by the cap is not an error, and the next Fire waits again.
func (f *Fire) waitCI(ctx context.Context, record *journal.Record, required []string, head string) (*journal.CI, error) {
	record.Phase = PhaseCI
	wait := &journal.CI{Head: head, Failed: []journal.FailedCheck{}}
	record.CI = wait
	sleep := f.Sleep
	if sleep == nil {
		sleep = Sleep
	}
	for unreadable := 0; ; {
		checks, err := f.GitHub.Checks(ctx, f.Repo.Owner, f.Repo.Name, head)
		if err != nil {
			if unreadable++; unreadable == ciUnreadable {
				wait.Result = ciError
				return wait, fmt.Errorf("reading the checks on %s: %w", short(head), err)
			}
		} else {
			unreadable = 0
			wait.Reads++
			switch result := ci.Settle(checks, required); {
			case result == ci.Green:
				wait.Result = string(result)
				return wait, nil
			case result == ci.Red:
				wait.Result = string(result)
				wait.Failed = f.failedChecks(ctx, ci.Failed(checks))
				return wait, nil
			case result == ci.None && wait.Reads >= ciGrace:
				wait.Result = string(result)
				return wait, nil
			case wait.Reads >= ciReads:
				wait.Result = ciTimeout
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
	case string(ci.Green):
		return "CI green, " + manual
	case string(ci.Red):
		names := make([]string, len(wait.Failed))
		for i, c := range wait.Failed {
			names[i] = c.Name
		}
		return fmt.Sprintf("CI red (%d failing: %s), %s", len(names), strings.Join(names, ", "), manual)
	case string(ci.None):
		return "no CI checks ran, " + manual
	case ciTimeout:
		return fmt.Sprintf("CI still running after %d minutes, %s", int(ciReads*ciInterval/time.Minute), manual)
	}
	return "CI could not be read, " + manual
}

// tail is what follows a push to an Agent PR: the wait on its CI, then the
// Ruling request for whatever needs the human, stating what CI said.
// finished is false when the Fire ends here, with err its error, if any.
func (f *Fire) tail(ctx context.Context, record *journal.Record, t *ticket, pr int, ambiguities []finding.Finding, decisions []arbitrate.Verdict) (finished bool, err error) {
	head, err := f.Workspace.Head(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the PR's head: %w", err)
	}
	wait, err := f.waitCI(ctx, record, t.harness.RequiredChecks, head)
	if err != nil {
		return false, err
	}
	if len(ambiguities)+len(decisions) == 0 {
		return true, nil
	}
	return f.requestRuling(ctx, record, t, pr, ambiguities, decisions, evidence(wait))
}

// reconcileCI waits on the CI of an Agent PR whose review ran and whose
// verification never finished. Nothing is written: the wait is read-only.
func (f *Fire) reconcileCI(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) error {
	pr := next.PR
	record.PR = &pr
	listed, err := f.GitHub.PullRequest(ctx, f.Repo.Owner, f.Repo.Name, pr)
	if err != nil {
		return fmt.Errorf("reading PR #%d's head: %w", pr, err)
	}
	if _, err := f.waitCI(ctx, record, harness.RequiredChecks, listed.HeadSHA); err != nil {
		return err
	}
	record.Phase = PhaseDone
	return nil
}
