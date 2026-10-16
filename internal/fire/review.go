package fire

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/review"
	"github.com/benjr70/auto-agent/internal/workspace"
)

// PhasePRReview: the one-time review of the PR.
const PhasePRReview = "pr-review"

// reviewPR runs the one-time review of an Agent PR, on its branch in the
// checkout: Review: correctness, then Review: Spec. Go applies the posting
// bar to what they found, opens a thread for each defect, applies
// AFK:revise when the PR has one, and posts the comment that says the review
// ran, last, so that a review cut short anywhere is run again whole by the
// reconcile Lane. A PR that has that comment is never reviewed again.
// verdict is the gate's answer for the first Step, when it was asked
// already. finished is false when the Fire ends here, with err its error,
// if any.
func (f *Fire) reviewPR(ctx context.Context, record *journal.Record, t *ticket, pr int, verdict *budget.Verdict) (finished bool, err error) {
	owner, name := f.Repo.Owner, f.Repo.Name
	record.Phase = PhasePRReview
	comments, err := f.GitHub.Comments(ctx, owner, name, pr)
	if err != nil {
		return false, fmt.Errorf("reading whether PR #%d was reviewed: %w", pr, err)
	}
	if slices.ContainsFunc(comments, func(c github.Comment) bool { return finding.IsDone(c.Body) }) {
		record.Warnings = append(record.Warnings, fmt.Sprintf("PR #%d was reviewed already", pr))
		return true, nil
	}
	head, err := f.Workspace.Head(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the commit under review: %w", err)
	}
	rev := &journal.Review{Reviewed: head, Ambiguities: []finding.Finding{}, ConfigChanged: []string{}}
	record.Review = rev

	// A PR that changes the Harness config changes its own verification
	// (ADR 0007): a human reviews that, whatever the review finds.
	var extra []string
	paths, err := f.Workspace.ChangedPaths(ctx)
	if err == nil {
		rev.ConfigChanged = configPaths(paths)
	}
	if err != nil || len(rev.ConfigChanged) > 0 {
		if labelErr := f.GitHub.AddLabels(ctx, owner, name, pr, state.LabelVerifyHuman); labelErr != nil {
			return false, fmt.Errorf("flagging PR #%d's Harness config change for a human: %w", pr, labelErr)
		}
		extra = append(extra, configSection(rev.ConfigChanged, err))
	}

	input := review.Input{PR: pr, Branch: t.branch, Base: f.Workspace.Base(), Issue: t.stepIssue(), Commands: t.commands()}
	if input.Spec, err = f.parentSpec(ctx, t.issue); err != nil {
		record.Warnings = append(record.Warnings, err.Error())
	}
	var all []finding.Finding
	for _, axis := range []struct {
		spec step.Spec
		name string
	}{{review.Correctness(t.commands()), finding.Correctness}, {review.Spec(t.commands()), finding.SpecAxis}} {
		if verdict == nil {
			var ok bool
			if verdict, ok, err = f.gate(ctx, record, axis.spec.Name); !ok {
				return false, err
			}
		}
		call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: input}
		result, run := callStep(ctx, f, record, axis.spec, call, *verdict, func(r review.Result) error { return r.Check() })
		verdict = nil
		// The review is read-only: whatever its test and lint runs left in
		// the checkout is nobody's to commit.
		if err := f.Workspace.Discard(ctx); err != nil {
			return false, fmt.Errorf("discarding what the review left in the checkout: %w", err)
		}
		if stop, err := f.settle(ctx, record, t, axis.spec.Name, run, ""); stop {
			if record.Phase == PhaseFailed {
				// The ticket is a human's now; so is the review, or the
				// reconcile Lane would take the PR for ever.
				f.giveUpReview(ctx, record, pr, head, err)
			}
			return false, err
		}
		all = append(all, result.On(axis.name)...)
	}

	routes := finding.Split(finding.Bar(all))
	rev.Findings, rev.Notes, rev.Ambiguities = len(all), len(routes.Notes), routes.Ambiguities
	threads, unanchored, err := f.postThreads(ctx, pr, head, finding.Dedupe(routes.Defects))
	rev.Threads = threads
	if err != nil {
		return false, err
	}
	if len(unanchored) > 0 {
		extra = append(extra, "**Could not anchor** (GitHub refused the line, and the file has no changed line to fall back to):\n\n"+strings.Join(unanchored, "\n"))
	}
	if threads > 0 {
		if err := f.GitHub.AddLabels(ctx, owner, name, pr, state.LabelRevise); err != nil {
			return false, fmt.Errorf("handing PR #%d's defects to the reconcile Lane: %w", pr, err)
		}
	}
	// Last: it is what says the review ran.
	if err := f.GitHub.CreateComment(ctx, owner, name, pr, finding.Done(len(all), threads, head, routes, strings.Join(extra, "\n\n"))); err != nil {
		return false, fmt.Errorf("posting PR #%d's review: %w", pr, err)
	}
	if len(routes.Ambiguities) == 0 {
		return true, nil
	}
	// A product ambiguity is the human's: they are asked once the review
	// has run.
	return f.requestRuling(ctx, record, t, pr, routes.Ambiguities, nil)
}

// reconcileReview runs the one-time review of an Agent PR the review never
// finished on: its Fire was cut off, or crashed, after the PR opened. The
// ticket holds the Single-flight lock while it runs, as it does for any work.
func (f *Fire) reconcileReview(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) (err error) {
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
	// Asked before the lock: a Fire told to wait leaves no label behind.
	record.Phase = PhaseGate
	verdict, ok, err := f.gate(ctx, record, review.CorrectnessName)
	if !ok {
		return err
	}
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness, paused: slices.Contains(issue.Labels, state.LabelPaused)}
	if finished, err := f.reviewPR(ctx, record, t, pr, verdict); !finished {
		return err
	}
	record.Phase = PhaseDone
	return nil
}

// postThreads opens a thread for each defect the PR does not have one for
// already: a review cut short after posting some is not posted twice. A line
// GitHub refuses is posted on the file's first changed line instead, naming
// the line it was meant for. threads is how many threads the PR has;
// unanchored lists the defects that could not be posted anywhere.
func (f *Fire) postThreads(ctx context.Context, pr int, head string, defects []finding.Thread) (threads int, unanchored []string, err error) {
	owner, name := f.Repo.Owner, f.Repo.Name
	existing, err := f.GitHub.ReviewComments(ctx, owner, name, pr)
	if err != nil {
		return 0, nil, fmt.Errorf("reading PR #%d's threads: %w", pr, err)
	}
	for _, thread := range defects {
		if slices.ContainsFunc(existing, func(c github.ReviewComment) bool { return thread.PostedAs(c.Path, c.Line, c.Body) }) {
			threads++
			continue
		}
		err := f.GitHub.CreateReviewComment(ctx, owner, name, pr, github.ReviewComment{Body: thread.Body(), Path: thread.Path, Line: thread.Line, Side: "RIGHT", CommitID: head})
		if github.IsUnprocessable(err) {
			// Only a refused anchor moves: anything else is a failure.
			line, side, lineErr := f.Workspace.FirstChangedLine(ctx, thread.Path)
			switch {
			case errors.Is(lineErr, workspace.ErrNotChanged):
				unanchored = append(unanchored, fmt.Sprintf("- `%s:%d` — %s", thread.Path, thread.Line, thread.Findings[0].Summary))
				continue
			case lineErr != nil:
				return threads, unanchored, fmt.Errorf("finding where to post %s:%d instead: %w", thread.Path, thread.Line, lineErr)
			}
			err = f.GitHub.CreateReviewComment(ctx, owner, name, pr, github.ReviewComment{Body: thread.FallbackBody(), Path: thread.Path, Line: line, Side: side, CommitID: head})
		}
		if err != nil {
			return threads, unanchored, fmt.Errorf("opening a thread on %s:%d: %w", thread.Path, thread.Line, err)
		}
		threads++
	}
	return threads, unanchored, nil
}

// giveUpReview posts the comment that says the review ran, for a review that
// could not: a human reviews the PR, and the reconcile Lane takes it no more.
func (f *Fire) giveUpReview(ctx context.Context, record *journal.Record, pr int, head string, why error) {
	body := finding.Done(0, 0, head, finding.Routes{}, fmt.Sprintf("**The automated review could not run** (%v). Review this PR by hand.", why))
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, pr, body); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("PR #%d's review was given up, but the comment saying so was not posted: %v", pr, err))
	}
}

// configPaths are the paths under the Harness config directory.
func configPaths(paths []string) []string {
	touched := []string{}
	for _, path := range paths {
		if strings.HasPrefix(path, config.HarnessDirName+"/") || strings.Contains(path, "/"+config.HarnessDirName+"/") {
			touched = append(touched, path)
		}
	}
	slices.Sort(touched)
	return slices.Compact(touched)
}

// configSection is what the review's comment tells the human about a PR
// that changes the Harness config, or whose changes could not be read.
func configSection(paths []string, unread error) string {
	var b strings.Builder
	b.WriteString("**Harness config changed — human review required**\n\n")
	if unread != nil {
		fmt.Fprintf(&b, "The PR's changed paths could not be read (%v), so it is flagged in case it changes the config.\n", unread)
	}
	for _, path := range paths {
		fmt.Fprintf(&b, "- `%s`\n", path)
	}
	b.WriteString("\nThis PR's verification reads its config from the PR itself. Check that the change does not weaken it: a removed Surface, `smoke` turned off, a redirected `hermetic.command`. The screenshot tour of any touched UI Surface is still owed, whatever the config now says.")
	return b.String()
}

// parentSpec is the ticket's parent Spec, as a Step reads it; nil when the
// ticket names none.
func (f *Fire) parentSpec(ctx context.Context, issue github.Issue) (*implement.Issue, error) {
	number := state.ParentOf(issue.Body)
	if number == 0 {
		return nil, nil
	}
	spec, err := f.GitHub.Issue(ctx, f.Repo.Owner, f.Repo.Name, number)
	if err != nil {
		return nil, fmt.Errorf("the parent Spec #%d could not be read, so the review judges the ticket alone: %w", number, err)
	}
	return &implement.Issue{Number: spec.Number, Title: spec.Title, Body: spec.Body}, nil
}
