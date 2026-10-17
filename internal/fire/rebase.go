package fire

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/workspace"
)

// PhaseRebase: rebasing the PR's branch onto the default branch.
const PhaseRebase = "rebase"

// maxStops bounds how many times one rebase may stop on a conflict and be
// resolved: once per commit on the branch, and a branch has few.
const maxStops = 20

// reconcileRebase rebases an Agent PR the default branch moved under, under
// the ticket's Single-flight lock. Go rebases with plain git: a clean
// rebase costs no Step. Only a conflict calls one: a Fix resolves it in
// place, and Go carries the rebase on, once per stop. The rebased branch is
// pushed with a lease, the one force-push the Daemon makes, and its CI is
// waited on. A rebase that cannot complete or be pushed is aborted, the
// branch left as the PR shows it, and the PR parked with AFK:rebase-failed.
// One attempt per Fire.
func (f *Fire) reconcileRebase(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) (err error) {
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
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness, paused: slices.Contains(issue.Labels, state.LabelPaused)}
	pushed, failed, err := f.rebase(ctx, record, t)
	switch {
	case failed != "":
		return f.parkRebase(ctx, record, pr, failed)
	case !pushed:
		// The Fire ended in the Fix: a wait, a pause or a failure. Nothing
		// was pushed, so there is no CI of it to wait on.
		if record.Phase == PhaseFailed {
			// The ticket is a human's now; so is the rebase, or the
			// reconcile Lane would take the PR for ever.
			if parkErr := f.parkRebase(ctx, record, pr, "the Fix resolving its conflicts used the ticket's attempts"); parkErr != nil {
				record.Warnings = append(record.Warnings, parkErr.Error())
			}
		}
		return err
	}
	if finished, err := f.afterPush(ctx, record, t, pr, false, nil, nil); !finished {
		return err
	}
	record.Phase = PhaseDone
	return nil
}

// rebase rebases the open branch onto the default branch and pushes it with
// a lease. pushed is set when it did. failed says why a rebase that could
// not complete was given up, the branch left as it was; otherwise a rebase
// not pushed is a Fire that ended in its Fix, with err its error, if any. A
// rebase left in progress is aborted.
func (f *Fire) rebase(ctx context.Context, record *journal.Record, t *ticket) (pushed bool, failed string, err error) {
	record.Phase = PhaseRebase
	rebase := &journal.Rebase{Onto: f.Workspace.Base(), Conflicts: []string{}}
	record.Rebase = rebase
	defer func() {
		abortCtx, cancel := detached(ctx)
		defer cancel()
		if abortErr := f.Workspace.RebaseAbort(abortCtx); abortErr != nil {
			record.Warnings = append(record.Warnings, "the rebase could not be aborted: "+abortErr.Error())
		}
	}()
	conflicts, err := f.Workspace.Rebase(ctx)
	if err != nil {
		return false, "rebase error: " + err.Error(), nil
	}
	for len(conflicts) > 0 {
		if rebase.Stops == maxStops {
			return false, fmt.Sprintf("conflicts unresolvable: still stopping after %d resolutions", maxStops), nil
		}
		rebase.Stops++
		rebase.Conflicts = append(rebase.Conflicts, conflicts...)
		if _, ok, err := f.fixStep(ctx, record, t, fix.RebaseConflict(conflicts), nil); !ok {
			return false, "", err
		}
		record.Phase = PhaseRebase
		if conflicts, err = f.Workspace.RebaseContinue(ctx, conflicts); err != nil {
			return false, "conflicts unresolvable: " + err.Error(), nil
		}
	}
	commit, err := f.Workspace.PushWithLease(ctx, t.branch)
	switch {
	case errors.Is(err, workspace.ErrLeaseRefused):
		return false, "lease rejected (the branch moved since it was read)", nil
	case err != nil:
		return false, "", fmt.Errorf("pushing the rebased branch: %w", err)
	}
	record.Commit = commit.SHA
	return true, "", nil
}

// parkRebase leaves a PR whose rebase failed to a human, as always:
// AFK:rebase-failed, and a comment saying why. A parked PR is not a failed
// Fire.
func (f *Fire) parkRebase(ctx context.Context, record *journal.Record, pr int, why string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	if err := f.GitHub.AddLabels(ctx, owner, name, pr, state.LabelRebaseFailed); err != nil {
		return fmt.Errorf("parking PR #%d: %w", pr, err)
	}
	record.ParkedPR = state.LabelRebaseFailed
	record.Rebase.Failed = why
	if err := f.GitHub.CreateComment(ctx, owner, name, pr, state.RebaseFailedComment(f.Workspace.Base(), f.Now(), why)); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("PR #%d is parked, but the comment saying so was not posted: %v", pr, err))
	}
	return nil
}
