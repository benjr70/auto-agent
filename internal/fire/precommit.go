package fire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/precommit"
	"github.com/benjr70/auto-agent/internal/verify"
)

// preCommitReview runs the pre-commit review on the ticket's branch: a
// review, and on approval the smoke. A change request or a failed smoke goes
// to a Fix whose work Go commits and pushes, and the next round reviews
// again. Review rounds and failed smokes count against one cap, as they
// always have; at the cap the ticket is failed. approved is false when the
// Fire ends here, with err its error, if any. verdict is the gate's answer
// for the first review, when it was asked already.
func (f *Fire) preCommitReview(ctx context.Context, record *journal.Record, t *ticket, verdict *budget.Verdict) (smoke verify.Result, approved bool, err error) {
	review := precommit.Spec(t.commands())
	var said []string
	for round := 1; ; round++ {
		if verdict == nil {
			var ok bool
			if verdict, ok, err = f.gate(ctx, record, review.Name); !ok {
				return smoke, false, err
			}
		}
		record.Phase, record.ReviewRounds = PhaseReview, round
		call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: precommit.Input{
			Issue: t.stepIssue(), Branch: t.branch, Base: f.Workspace.Base(), Commands: t.commands(),
			CommitScopes: t.harness.CommitScopes, Round: round, Cap: state.PreCommitReviewCap,
		}}
		result, run := callStep(ctx, f, record, review, call, *verdict, func(r precommit.Result) error { return r.Check() })
		// The review is read-only: whatever its test and lint runs left in
		// the checkout is not the Fix's to commit.
		if err := f.Workspace.Discard(ctx); err != nil {
			return smoke, false, fmt.Errorf("discarding what the review left in the checkout: %w", err)
		}
		if stop, err := f.settle(ctx, record, t, review.Name, run, ""); stop {
			return smoke, false, err
		}
		var brief fix.Brief
		if result.Approved() {
			if smoke, err = f.smoke(ctx, record); err != nil || smoke.Status != verify.Fail {
				return smoke, err == nil, err
			}
			// A failed smoke is a round of its own.
			round++
			record.ReviewRounds = round
			said = []string{"the smoke failed: " + smoke.Detail}
			brief = fix.FailedVerification("the smoke", smoke.Detail, smoke.Log)
		} else {
			said = result.ChangeRequests
			brief = fix.ChangeRequest(said)
		}
		if round >= state.PreCommitReviewCap {
			return smoke, false, f.reviewCap(ctx, record, t.issue.Number, said)
		}
		if verdict, err = f.fix(ctx, record, t, brief); verdict == nil {
			return smoke, false, err
		}
		// The next review asks the gate for itself.
		verdict = nil
	}
}

// smoke runs the smoke through the Target Project's Environment provider,
// as the branch's own Harness config declares it: a branch that adds a
// provider is smoked by the provider it adds (ADR 0007). The result is in
// the record whatever it is.
func (f *Fire) smoke(ctx context.Context, record *journal.Record) (verify.Result, error) {
	record.Phase = PhaseSmoke
	dir := f.Workspace.Dir()
	harness, err := config.LoadHarness(dir, "", f.Repo)
	if err != nil {
		return verify.Result{}, fmt.Errorf("reading the branch's Harness config for the smoke: %w", err)
	}
	var result verify.Result
	switch provider, err := verify.Resolve(dir, harness.Verification.Hermetic); {
	case errors.Is(err, verify.ErrBootstrap):
		result = verify.Bootstrap()
	case err != nil:
		result = verify.Result{Status: verify.Skipped, Detail: err.Error(), Violation: true}
	default:
		// The provider runs the Target Project's code, from the ticket's
		// branch: it gets no GitHub credential.
		empty := filepath.Join(f.Journal.StateDir, "provider-gh")
		if err := os.MkdirAll(empty, 0o700); err != nil {
			return verify.Result{}, err
		}
		// The environment is kept apart by the ticket's number: its pull
		// request is not open yet.
		result = provider.Run(ctx, *record.Issue, harness.Surfaces, verify.Env(os.Environ(), empty))
	}
	record.Smoke = &result
	if result.Teardown != "" {
		record.Warnings = append(record.Warnings, "the Environment provider: "+result.Teardown)
	}
	return result, nil
}

// fix runs the Fix Step on a brief and commits and pushes its work. The
// verdict it returns is nil when the Fire ends here, with err its error, if
// any.
func (f *Fire) fix(ctx context.Context, record *journal.Record, t *ticket, brief fix.Brief) (*budget.Verdict, error) {
	spec := fix.Spec()
	if err := brief.Check(); err != nil {
		return nil, fmt.Errorf("the Fix brief: %w", err)
	}
	verdict, ok, err := f.gate(ctx, record, spec.Name)
	if !ok {
		return nil, err
	}
	record.Phase = PhaseFix
	call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: fix.Input{
		Issue: t.stepIssue(), Branch: t.branch, Commands: t.commands(), CommitScopes: t.harness.CommitScopes, Brief: brief,
	}}
	result, run := callStep(ctx, f, record, spec, call, *verdict, func(r fix.Result) error { return r.Check(t.harness.CommitScopes) })
	if stop, err := f.settle(ctx, record, t, spec.Name, run, ""); stop {
		return nil, err
	}
	record.Phase = PhaseCommit
	commit, err := f.Workspace.CommitAndPush(ctx, t.branch, strings.TrimSpace(result.CommitMessage)+"\n")
	if err != nil {
		return nil, fmt.Errorf("committing and pushing the Fix: %w", err)
	}
	if !commit.Empty() {
		record.Commit = commit.SHA
	}
	return verdict, nil
}

// reviewCap hands a ticket whose review asked for changes, or whose smoke
// failed, as often as it may to a human, as a dispatch that could not satisfy
// its reviewer and its verifier always was. said is what the last round said.
func (f *Fire) reviewCap(ctx context.Context, record *journal.Record, number int, said []string) error {
	if err := f.fail(ctx, number); err != nil {
		return fmt.Errorf("failing #%d at the pre-commit review cap: %w", number, err)
	}
	record.Phase = PhaseFailed
	reason := fmt.Errorf("the pre-commit review and the smoke did not pass in %d rounds: %s applied", state.PreCommitReviewCap, state.LabelFailed)
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, number, state.PreCommitCapComment(state.PreCommitReviewCap, said)); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("#%d is failed at the pre-commit review cap, but the comment saying so was not posted: %v", number, err))
	}
	return reason
}
