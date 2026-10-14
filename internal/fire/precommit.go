package fire

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/precommit"
)

// preCommitReview runs the pre-commit review on the ticket's branch: a
// review, and on a change request a Fix whose work Go commits and pushes,
// then the next review, up to the cap. approved is false when the Fire ends
// here, with err its error, if any; at the cap the ticket is failed. verdict
// is the gate's answer for the first review, when it was asked already.
func (f *Fire) preCommitReview(ctx context.Context, record *journal.Record, t *ticket, verdict *budget.Verdict) (approved bool, err error) {
	review := precommit.Spec(t.commands())
	var asks []string
	for round := 1; round <= state.PreCommitReviewCap; round++ {
		if verdict == nil {
			var ok bool
			if verdict, ok, err = f.gate(ctx, record, review.Name); !ok {
				return false, err
			}
		}
		record.Phase, record.ReviewRounds = PhaseReview, round
		call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: precommit.Input{
			Issue: t.stepIssue(), Branch: t.branch, Base: f.Workspace.Base(), Commands: t.commands(),
			CommitScopes: t.harness.CommitScopes, Round: round, Cap: state.PreCommitReviewCap,
		}}
		check := func(r precommit.Result) error { return r.Check() }
		result, run := runStep(ctx, f, record, review, call, *verdict, "", check)
		result, run = correctOnce(ctx, f, record, review, call, *verdict, check, result, run)
		if stop, err := f.settle(ctx, record, t, review.Name, run, ""); stop {
			return false, err
		}
		if result.Approved() {
			return true, nil
		}
		if asks = result.ChangeRequests; round == state.PreCommitReviewCap {
			break
		}
		if verdict, err = f.fix(ctx, record, t, fix.ChangeRequest(asks)); verdict == nil {
			return false, err
		}
		// The next review asks the gate for itself.
		verdict = nil
	}
	return false, f.reviewCap(ctx, record, t.issue.Number, asks)
}

// fix runs the Fix Step on a brief and commits and pushes its work. The
// verdict it returns is nil when the Fire ends here, with err its error, if
// any.
func (f *Fire) fix(ctx context.Context, record *journal.Record, t *ticket, brief fix.Brief) (*budget.Verdict, error) {
	spec := fix.Spec()
	verdict, ok, err := f.gate(ctx, record, spec.Name)
	if !ok {
		return nil, err
	}
	record.Phase = PhaseFix
	call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: fix.Input{
		Issue: t.stepIssue(), Branch: t.branch, Commands: t.commands(), CommitScopes: t.harness.CommitScopes, Brief: brief,
	}}
	check := func(r fix.Result) error { return r.Check(t.harness.CommitScopes) }
	result, run := runStep(ctx, f, record, spec, call, *verdict, "", check)
	result, run = correctOnce(ctx, f, record, spec, call, *verdict, check, result, run)
	if stop, err := f.settle(ctx, record, t, spec.Name, run, ""); stop {
		return nil, err
	}
	record.Phase = PhaseCommit
	commit, err := f.Workspace.CommitAndPush(ctx, t.branch, strings.TrimSpace(result.CommitMessage)+"\n")
	if err != nil {
		return nil, fmt.Errorf("committing and pushing the Fix: %w", err)
	}
	if commit.SHA != "" {
		record.Commit = commit.SHA
	}
	return verdict, nil
}

// reviewCap hands a ticket whose review asked for changes as often as it may
// to a human, as a dispatch that could not satisfy its reviewer always was.
func (f *Fire) reviewCap(ctx context.Context, record *journal.Record, number int, asks []string) error {
	if err := f.fail(ctx, number); err != nil {
		return fmt.Errorf("failing #%d at the pre-commit review cap: %w", number, err)
	}
	record.Phase = PhaseFailed
	reason := fmt.Errorf("the pre-commit review asked for changes %d rounds running: %s applied", state.PreCommitReviewCap, state.LabelFailed)
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, number, state.PreCommitCapComment(state.PreCommitReviewCap, asks)); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("#%d is failed at the pre-commit review cap, but the comment saying so was not posted: %v", number, err))
	}
	return reason
}

// noCriteria stands in for the checklist of a ticket whose body has no
// acceptance criteria.
const noCriteria = "_(no Acceptance Criteria found in issue body)_"

var (
	criteriaHeading = regexp.MustCompile(`(?i)^##\s*acceptance criteria\s*$`)
	listItem        = regexp.MustCompile(`^\s*[-*]\s+(\[[ xX]\]\s+)?(.+)$`)
)

// implementedResult is the Implement result of a branch that carried the
// ticket's finished Implement commit when this Fire found it: the Step's
// answer is gone, so the pull request is opened, as it always was, from the
// commit and from the ticket's acceptance criteria.
func implementedResult(issue github.Issue, message string) implement.Result {
	subject := step.Subject(message)
	_, body, _ := strings.Cut(strings.TrimSpace(message), "\n")
	summary, _, _ := strings.Cut(strings.TrimSpace(regexp.MustCompile(`(?m)^Closes #\d+\s*$`).ReplaceAllString(body, "")), "\n\n")
	result := implement.Result{Finished: true, Summary: strings.TrimSpace(cmp.Or(strings.TrimSpace(summary), subject)), CommitMessage: message}
	inCriteria := false
	for _, line := range strings.Split(issue.Body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case criteriaHeading.MatchString(line):
			inCriteria = true
		case strings.HasPrefix(line, "## "):
			inCriteria = false
		case inCriteria:
			if m := listItem.FindStringSubmatch(line); m != nil {
				result.Checklist = append(result.Checklist, strings.TrimSpace(m[2]))
			}
		}
	}
	if len(result.Checklist) == 0 {
		result.Checklist = []string{noCriteria}
	}
	return result
}
