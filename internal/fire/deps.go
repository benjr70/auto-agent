package fire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/ci"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/workspace"
)

// The phases of the Bot PR Lane.
const (
	// PhaseDeps: taking a Bot PR through the Lane.
	PhaseDeps = "deps"
	// PhaseDepsFailed: the bump's fix budget is spent, or its conflict is a
	// human's; the PR is parked.
	PhaseDepsFailed = "deps-failed"
)

// depsLeft is what a Bot PR still needs once Tier A is green, which this
// build does not do.
const depsLeft = "Tier B, the gate and the merge are not in this build yet"

// bump is the Bot PR a Fire takes through the Lane, as it stands.
type bump struct {
	pr      github.PullRequest
	harness config.Harness
	history state.DepsHistory
	// opened is set once its branch is checked out at its head.
	opened bool
}

// stepBump is the bump as a Fix on its branch is told it.
func (b *bump) stepBump() *fix.Bump {
	return &fix.Bump{PR: b.pr.Number, Title: b.pr.Title, Body: b.pr.Body, LockfileRefresh: b.harness.Commands.LockfileRefresh}
}

// reconcileDeps takes a Bot PR through the Bot PR Lane, with no ticket and
// no lock: the PR is the whole of it. The PR is read again first: one that
// closed, became a draft or stopped being Dependabot's is left alone. Its
// fix budget is counted from the Daemon's markers for its head, and a
// history that cannot be read stops the Lane: it never refunds the budget.
// A conflict is answered, and ends the Fire. Otherwise a security bump is
// retitled, the Bot-PR checklist injected, and Tier A run: CI waited on, a
// failure answered by a Fix, and green recorded for the head. Go makes
// every write to GitHub; the Fix only changes files.
func (f *Fire) reconcileDeps(ctx context.Context, record *journal.Record, harness config.Harness, r state.Reconcile) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	record.Phase, record.Issue = PhaseDeps, nil
	deps := &journal.Deps{TierA: "skipped", Cap: harness.Rounds.DepsFix}
	record.Deps = deps
	if r.Security != nil {
		deps.Security = *r.Security
	}
	pr, err := f.GitHub.PullRequest(ctx, owner, name, r.PR)
	if err != nil {
		return fmt.Errorf("reading Bot PR #%d: %w", r.PR, err)
	}
	deps.Head = pr.HeadSHA
	if pr.State != "open" || pr.Merged || pr.Draft || !state.IsBotPR(pr) || pr.HeadRef != r.Branch {
		deps.Outcome, record.Phase = journal.DepsSuperseded, PhaseDone
		return nil
	}
	comments, err := f.GitHub.Comments(ctx, owner, name, pr.Number)
	if err != nil {
		return fmt.Errorf("the Bot PR Lane stops: the comments of PR #%d could not be read, so its fix attempts cannot be counted: %w", pr.Number, err)
	}
	history, err := state.ReadDepsHistory(comments, f.Login, pr.HeadSHA)
	if err != nil {
		return fmt.Errorf("the Bot PR Lane stops on PR #%d: %w", pr.Number, err)
	}
	deps.Attempts = history.Attempts
	b := &bump{pr: pr, harness: harness, history: history}
	if r.Reason == state.ReasonConflict && pr.Mergeable != github.Mergeable {
		return f.depsConflict(ctx, record, b, r.AgentCommits == nil || *r.AgentCommits)
	}
	if title := state.Retitle(pr.Title, deps.Security); title != pr.Title {
		if err := f.GitHub.EditPullRequestTitle(ctx, owner, name, pr.Number, title); err != nil {
			return fmt.Errorf("retitling Bot PR #%d: %w", pr.Number, err)
		}
		b.pr.Title, deps.Retitled = title, true
	}
	if err := f.injectChecklist(ctx, record, b); err != nil {
		return err
	}
	return f.tierA(ctx, record, b)
}

// injectChecklist puts the Target Project's Bot-PR checklist in the PR's
// body, once. A checklist that is missing, or holds nothing to verify,
// stops the Lane.
func (f *Fire) injectChecklist(ctx context.Context, record *journal.Record, b *bump) error {
	path := b.harness.Prose.BotPRChecklist
	if path == "" {
		return errors.New("the Bot PR Lane stops: there is no bot-pr-checklist.md beside the Harness config, so it has no checklist to inject")
	}
	file, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("the Bot PR Lane stops: reading the Bot-PR checklist: %w", err)
	}
	body, err := state.InjectChecklist(b.pr.Body, string(file))
	if err != nil {
		return fmt.Errorf("the Bot PR Lane stops: %s: %w", path, err)
	}
	if body == b.pr.Body {
		return nil
	}
	if err := f.GitHub.EditPullRequestBody(ctx, f.Repo.Owner, f.Repo.Name, b.pr.Number, body); err != nil {
		return fmt.Errorf("injecting the Bot-PR checklist into PR #%d: %w", b.pr.Number, err)
	}
	b.pr.Body, record.Deps.Checklist = body, true
	return nil
}

// tierA proves the bump's head with CI. A head with its marker already is
// green. Otherwise CI is waited on; a failure is answered by a Fix, its
// commit ending in the skip trailer and pushed, never by force, and waited
// on again, while the bump's fix budget lasts. Every attempt is counted in
// a marker on the head before the next: a push moves the head, and the
// count goes with it. CI is read before the budget is: the Fix that spent
// the last attempt is credited when it worked. A failure with no budget left
// parks the PR.
func (f *Fire) tierA(ctx context.Context, record *journal.Record, b *bump) error {
	deps, budgetCap := record.Deps, b.harness.Rounds.DepsFix
	if b.history.TierA {
		deps.TierA, deps.Outcome, record.NotWorked, record.Phase = "green", journal.DepsTierAGreen, depsLeft, PhaseDone
		return nil
	}
	head := b.pr.HeadSHA
	wait, err := f.waitCI(ctx, record, b.harness.RequiredChecks, head, ciReads)
	if err != nil {
		return err
	}
	fixes := 0
	for wait.Result == ci.Fail {
		if b.history.RoundsLeft(budgetCap) == 0 {
			deps.TierA = "failed"
			failure := "CI still failing: " + failedNames(wait)
			return f.parkDeps(ctx, record, b, func(sha string) string { return state.DepsParkedComment(budgetCap, failure, sha) })
		}
		failed := make([]fix.FailedCheck, len(wait.Failed))
		for i, c := range wait.Failed {
			failed[i] = fix.FailedCheck{Name: c.Name, Link: c.Link, Log: c.Log}
		}
		record.CIRounds++
		result, ok, unanswered, err := f.fixBump(ctx, record, b, fix.CIFailure(failed))
		if unanswered {
			return f.countBumpAttempt(ctx, record, b, head, false, err)
		}
		if !ok {
			return err
		}
		message, err := state.CommitTrailer(result.CommitMessage)
		if err != nil {
			return err
		}
		record.Phase = PhaseCommit
		commit, err := f.Workspace.CommitAndPush(ctx, b.pr.HeadRef, message)
		if err != nil {
			return fmt.Errorf("committing and pushing the Fix: %w", err)
		}
		if !commit.Empty() {
			head, record.Commit = commit.SHA, commit.SHA
			fixes++
			if err := f.countBumpAttempt(ctx, record, b, head, true, nil); err != nil {
				return err
			}
		} else {
			if err := f.countBumpAttempt(ctx, record, b, head, false, nil); err != nil {
				return err
			}
			if strings.TrimSpace(result.Flake) == "" {
				return errors.New("the Fix changed nothing for the failing CI run and named no flake")
			}
			if err := f.rerun(ctx, wait.Failed); err != nil {
				return err
			}
			// The jobs run again on the same commit: the first read after
			// asking must not find the failure they are replacing.
			if err := f.sleep(ctx, ciInterval); err != nil {
				return err
			}
		}
		if wait, err = f.waitCI(ctx, record, b.harness.RequiredChecks, head, ciReads); err != nil {
			return err
		}
	}
	record.Phase = PhaseDone
	if wait.Result != ci.Green {
		// Nothing red, nothing green: the next Fire reads it again.
		deps.TierA, deps.Outcome = "pending", journal.DepsWaiting
		return nil
	}
	comment, err := state.TierAComment(head)
	if err != nil {
		return err
	}
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, b.pr.Number, comment); err != nil {
		return fmt.Errorf("Tier A is green on Bot PR #%d, but its marker could not be posted: %w", b.pr.Number, err)
	}
	deps.TierA = "green"
	if fixes > 0 {
		deps.TierA = fmt.Sprintf("fixed(%d)", fixes)
	}
	deps.Outcome, record.NotWorked = journal.DepsTierAGreen, depsLeft
	return nil
}

// failedNames are the names of a wait's failed checks.
func failedNames(wait *journal.CI) string {
	names := make([]string, len(wait.Failed))
	for i, c := range wait.Failed {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// countBumpAttempt counts one fix attempt in a marker on head: after a push
// every attempt so far, as the head is new; otherwise the one attempt, on
// the head that has the rest. why is the error of an attempt that ended the
// Fire, if any; it is returned with the count.
func (f *Fire) countBumpAttempt(ctx context.Context, record *journal.Record, b *bump, head string, pushed bool, why error) error {
	b.history.Attempts++
	record.Deps.Attempts = b.history.Attempts
	from := b.history.Attempts
	if pushed {
		from = 1
	}
	comment, err := state.FixAttemptsComment(from, b.history.Attempts, b.harness.Rounds.DepsFix, head)
	if err == nil {
		err = f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, b.pr.Number, comment)
	}
	switch {
	case err != nil && why != nil:
		return fmt.Errorf("%w (and its fix attempt could not be counted: %v)", why, err)
	case err != nil:
		return fmt.Errorf("fix attempt %d on Bot PR #%d could not be counted: %w", b.history.Attempts, b.pr.Number, err)
	case why != nil:
		return fmt.Errorf("%w (fix attempt %d of %d)", why, b.history.Attempts, b.harness.Rounds.DepsFix)
	}
	return nil
}

// openBump checks out the bump's branch, at the head the Fire read. ok is
// false when the branch is not there: the PR moved under the Fire.
func (f *Fire) openBump(ctx context.Context, record *journal.Record, b *bump) (ok bool, err error) {
	if b.opened {
		return true, nil
	}
	branch, err := f.Workspace.Open(ctx, b.pr.HeadRef)
	if err != nil {
		return false, fmt.Errorf("opening the branch: %w", err)
	}
	record.Branch = branch
	if head, err := f.Workspace.Head(ctx); err != nil || head != b.pr.HeadSHA {
		return false, err
	}
	b.opened = true
	return true, nil
}

// fixBump runs the Fix Step on the bump's branch and leaves its work in the
// checkout. ok is false when the Fire ends here, with err its error, if
// any. unanswered is set when the Step ran and did not answer: that is an
// attempt spent. Cut off by the usage limit the Fire just ends, nothing
// pushed, and the next one takes the bump again.
func (f *Fire) fixBump(ctx context.Context, record *journal.Record, b *bump, brief fix.Brief) (result fix.Result, ok, unanswered bool, err error) {
	spec := fix.Spec()
	if err := brief.Check(); err != nil {
		return result, false, false, fmt.Errorf("the Fix brief: %w", err)
	}
	if opened, err := f.openBump(ctx, record, b); !opened {
		if err == nil {
			record.Deps.Outcome, record.Phase = journal.DepsSuperseded, PhaseDone
		}
		return result, false, false, err
	}
	verdict, ok, err := f.gate(ctx, record, spec.Name)
	if !ok {
		return result, false, false, err
	}
	record.Phase = PhaseFix
	c := b.harness.Commands
	input := fix.Input{
		Branch: b.pr.HeadRef, Commands: implement.Commands{Install: c.Install, Test: c.Test, Lint: c.Lint},
		Base: f.Workspace.Base(), CommitScopes: b.harness.CommitScopes, Brief: brief, Bump: b.stepBump(),
	}
	call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: input}
	result, run := callStep(ctx, f, record, spec, call, *verdict, func(r fix.Result) error { return r.Check(brief, b.harness.CommitScopes) })
	switch run.Outcome {
	case step.OK:
		return result, true, false, nil
	case step.Exhausted:
		record.ResetAt = budget.ReportOf(run, f.Now()).ResetAt
		return result, false, false, nil
	case step.AuthDead:
		return result, false, false, f.parkOnDead(ctx, record, fmt.Errorf("the %s Step of Fire %s ended %s: %s", spec.Name, record.FireID, run.Outcome, run.Detail))
	}
	err = fmt.Errorf("the %s Step ended %s: %s", spec.Name, run.Outcome, run.Detail)
	return result, false, !stopping(ctx), err
}

// depsConflict answers a conflict on a Bot PR, and ends the Fire: the next
// one proves the head it leaves. A branch with only Dependabot's commits is
// Dependabot's to rebase, and is asked to. One the Daemon has pushed to is
// rebased as an Agent PR is, and pushed with a lease; a stop on a conflict
// is a lockfile's, resolved by a Fix with the Target Project's lockfile
// command. One with no such command, or one the Fix cannot resolve, is a
// human's, and the PR is parked. A rebase that errs or loses its lease is
// aborted, the branch left as it was: the next Fire tries again.
func (f *Fire) depsConflict(ctx context.Context, record *journal.Record, b *bump, agentCommits bool) error {
	deps := record.Deps
	if !agentCommits {
		if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, b.pr.Number, state.DependabotRebase); err != nil {
			return fmt.Errorf("asking Dependabot to rebase Bot PR #%d: %w", b.pr.Number, err)
		}
		deps.Outcome, record.Phase = journal.DepsNudged, PhaseDone
		return nil
	}
	if opened, err := f.openBump(ctx, record, b); !opened {
		if err == nil {
			deps.Outcome, record.Phase = journal.DepsSuperseded, PhaseDone
		}
		return err
	}
	pushed, failed, err := f.rebaseBump(ctx, record, b)
	switch {
	case failed != "":
		record.Rebase.Failed = failed
		return f.parkDeps(ctx, record, b, func(sha string) string { return state.DepsConflictParkedComment(failed, sha) })
	case !pushed:
		return err
	}
	deps.Outcome, record.Phase = journal.DepsRebased, PhaseDone
	return nil
}

// rebaseBump rebases the bump's open branch onto the default branch and
// pushes it with a lease. failed says why a conflict is left to a human;
// otherwise a rebase not pushed ended the Fire, with err its error, if any.
// A rebase left in progress is aborted.
func (f *Fire) rebaseBump(ctx context.Context, record *journal.Record, b *bump) (pushed bool, failed string, err error) {
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
		return false, "", fmt.Errorf("rebasing Bot PR #%d: %w", b.pr.Number, err)
	}
	for len(conflicts) > 0 {
		switch {
		case strings.TrimSpace(b.harness.Commands.LockfileRefresh) == "":
			return false, "conflicts only a human can resolve: the Harness config names no commands.lockfile_refresh (" + strings.Join(conflicts, ", ") + ")", nil
		case rebase.Stops == maxStops:
			return false, fmt.Sprintf("conflicts unresolvable: still stopping after %d resolutions", maxStops), nil
		}
		rebase.Stops++
		rebase.Conflicts = append(rebase.Conflicts, conflicts...)
		_, ok, unanswered, err := f.fixBump(ctx, record, b, fix.RebaseConflict(conflicts))
		if unanswered {
			return false, "the Fix resolving the conflict did not answer: " + err.Error(), nil
		}
		if !ok {
			return false, "", err
		}
		record.Phase = PhaseRebase
		if conflicts, err = f.Workspace.RebaseContinue(ctx, conflicts); err != nil {
			return false, "conflicts unresolvable: " + err.Error(), nil
		}
	}
	commit, err := f.Workspace.PushWithLease(ctx, b.pr.HeadRef)
	switch {
	case errors.Is(err, workspace.ErrLeaseRefused):
		return false, "", fmt.Errorf("pushing the rebased Bot PR #%d: the branch moved since it was read", b.pr.Number)
	case err != nil:
		return false, "", fmt.Errorf("pushing the rebased branch: %w", err)
	}
	record.Commit = commit.SHA
	return true, "", nil
}

// parkDeps leaves a Bot PR to a human: a draft, which is what keeps the PR
// Triage off it, AFK:deps-failed, and one comment saying why, carrying the
// park's marker for its head. It reads the PR first and makes only the moves
// still owed, so a park after a crash halfway through posts nothing twice.
// comment is that comment for the head, saying why: a spent budget, or a
// conflict that spent none. A parked PR is not a failed Fire.
func (f *Fire) parkDeps(ctx context.Context, record *journal.Record, b *bump, comment func(sha string) string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	record.Phase = PhaseDepsFailed
	current, err := f.GitHub.PullRequest(ctx, owner, name, b.pr.Number)
	if err != nil {
		return fmt.Errorf("parking Bot PR #%d: %w", b.pr.Number, err)
	}
	if !current.Draft {
		if err := f.GitHub.MarkDraft(ctx, current); err != nil {
			return fmt.Errorf("parking Bot PR #%d: marking it draft: %w", b.pr.Number, err)
		}
	}
	if !slices.Contains(current.Labels, state.LabelDepsFailed) {
		if err := f.GitHub.AddLabels(ctx, owner, name, b.pr.Number, state.LabelDepsFailed); err != nil {
			return fmt.Errorf("parking Bot PR #%d: %w", b.pr.Number, err)
		}
	}
	record.ParkedPR, record.Deps.Outcome = state.LabelDepsFailed, journal.DepsParked
	comments, err := f.GitHub.Comments(ctx, owner, name, b.pr.Number)
	if err != nil {
		return fmt.Errorf("Bot PR #%d is parked, but whether it says so could not be read: %w", b.pr.Number, err)
	}
	marker := state.ParkMarker(current.HeadSHA)
	if slices.ContainsFunc(comments, func(c github.Comment) bool {
		return strings.Contains(c.Body, marker) && strings.EqualFold(c.Author, f.Login)
	}) {
		return nil
	}
	if err := f.GitHub.CreateComment(ctx, owner, name, b.pr.Number, comment(current.HeadSHA)); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("Bot PR #%d is parked, but the comment saying so was not posted: %v", b.pr.Number, err))
	}
	return nil
}
