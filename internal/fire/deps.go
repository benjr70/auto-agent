package fire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/ci"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/depsgate"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/verifier"
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

// PhaseDepsGate: the deps gate, on the head both tiers vouched for.
const PhaseDepsGate = "deps-gate"

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
// retitled, the Bot-PR checklist injected, and the bump proved: Tier A,
// Tier B, then the deps gate and the merge. Go makes every write to GitHub;
// the Fix only changes files.
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
	return f.prove(ctx, record, b)
}

// prove takes the bump's head through Tier A, then Tier B, then the deps
// gate, skipping a tier whose marker the head carries already. A tier that
// pushed a Fix ends the Fire: the head moved, and the next Fire proves the
// new one.
func (f *Fire) prove(ctx context.Context, record *journal.Record, b *bump) error {
	if green, err := f.tierA(ctx, record, b); !green {
		return err
	}
	if b.history.TierB {
		record.Deps.TierB = "skipped"
	} else if passed, err := f.tierB(ctx, record, b); !passed {
		return err
	}
	return f.gateBump(ctx, record, b)
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
// count goes with it. CI is read before the budget is: the Fire that spent
// the last attempt is credited when it worked. A failure with no budget left
// parks the PR. green says the head the Fire read is green, and Tier B is
// next; otherwise the Fire ends, with err its error, if any.
func (f *Fire) tierA(ctx context.Context, record *journal.Record, b *bump) (green bool, err error) {
	deps, budgetCap := record.Deps, b.harness.Rounds.DepsFix
	if b.history.TierA {
		deps.TierA = "green"
		return true, nil
	}
	head := b.pr.HeadSHA
	wait, err := f.waitCI(ctx, record, b.harness.RequiredChecks, head, ciReads)
	if err != nil {
		return false, err
	}
	fixes := 0
	for wait.Result == ci.Fail {
		if b.history.RoundsLeft(budgetCap) == 0 {
			deps.TierA = "failed"
			failure := "CI still failing: " + failedNames(wait)
			return false, f.parkDeps(ctx, record, b, func(sha string) string { return state.DepsParkedComment(budgetCap, failure, sha) })
		}
		failed := make([]fix.FailedCheck, len(wait.Failed))
		for i, c := range wait.Failed {
			failed[i] = fix.FailedCheck{Name: c.Name, Link: c.Link, Log: c.Log}
		}
		record.CIRounds++
		result, ok, unanswered, err := f.fixBump(ctx, record, b, fix.CIFailure(failed))
		if unanswered {
			return false, f.countBumpAttempt(ctx, record, b, head, false, err)
		}
		if !ok {
			return false, err
		}
		message, err := state.CommitTrailer(result.CommitMessage)
		if err != nil {
			return false, err
		}
		record.Phase = PhaseCommit
		commit, err := f.Workspace.CommitAndPush(ctx, b.pr.HeadRef, message)
		if err != nil {
			return false, fmt.Errorf("committing and pushing the Fix: %w", err)
		}
		if !commit.Empty() {
			head, record.Commit = commit.SHA, commit.SHA
			fixes++
			if err := f.countBumpAttempt(ctx, record, b, head, true, nil); err != nil {
				return false, err
			}
		} else {
			if err := f.countBumpAttempt(ctx, record, b, head, false, nil); err != nil {
				return false, err
			}
			if strings.TrimSpace(result.Flake) == "" {
				return false, errors.New("the Fix changed nothing for the failing CI run and named no flake")
			}
			if err := f.rerun(ctx, wait.Failed); err != nil {
				return false, err
			}
			// The jobs run again on the same commit: the first read after
			// asking must not find the failure they are replacing.
			if err := f.sleep(ctx, ciInterval); err != nil {
				return false, err
			}
		}
		if wait, err = f.waitCI(ctx, record, b.harness.RequiredChecks, head, ciReads); err != nil {
			return false, err
		}
	}
	record.Phase = PhaseDone
	if wait.Result != ci.Green {
		// Nothing red, nothing green: the next Fire reads it again.
		deps.TierA, deps.Outcome = "pending", journal.DepsWaiting
		return false, nil
	}
	comment, err := state.TierAComment(head)
	if err != nil {
		return false, err
	}
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, b.pr.Number, comment); err != nil {
		return false, fmt.Errorf("Tier A is green on Bot PR #%d, but its marker could not be posted: %w", b.pr.Number, err)
	}
	deps.TierA = "green"
	if fixes > 0 {
		// The Fixes moved the head: the next Fire takes the new one to
		// Tier B.
		deps.TierA, deps.Outcome = fmt.Sprintf("fixed(%d)", fixes), journal.DepsTierAGreen
		return false, nil
	}
	b.history.TierA = true
	return true, nil
}

// tierB proves the bump's head on the real app: one Verification round over
// the Bot-PR checklist, its tour forced over every UI Surface, as a
// lockfile-only diff touches none. Its boxes are unticked first: one ticked
// on an older head proves nothing of this one. Every item must pass: a
// deferral is a failure, as nobody checks a bump later, and a round over no
// item proves nothing. A failure is answered by a Fix on
// the bump's one fix budget, its commit pushed and counted on the new head,
// which ends the Fire. An environment that did not boot is no verdict and
// spends no attempt: the next Fire runs the round again. A head with no
// hermetic tier is the Bootstrap state: Tier B cannot run, and the PR is
// parked for a human. A PASS is recorded for the head. passed says Tier B
// is green and the gate is next; otherwise the Fire ends, with err its
// error, if any.
func (f *Fire) tierB(ctx context.Context, record *journal.Record, b *bump) (passed bool, err error) {
	deps := record.Deps
	if opened, err := f.openBump(ctx, record, b); !opened {
		if err == nil {
			deps.Outcome, record.Phase = journal.DepsSuperseded, PhaseDone
		}
		return false, err
	}
	if body := state.UntickChecklist(b.pr.Body); body != b.pr.Body {
		if err := f.GitHub.EditPullRequestBody(ctx, f.Repo.Owner, f.Repo.Name, b.pr.Number, body); err != nil {
			return false, fmt.Errorf("unticking Bot PR #%d's checklist for its head: %w", b.pr.Number, err)
		}
		b.pr.Body = body
	}
	got, err := f.runRound(ctx, record, subject{pr: b.pr.Number, branch: b.pr.HeadRef, forceTour: true})
	switch got.end {
	case endStopped:
		return false, err
	case endInfra:
		deps.TierB, deps.Outcome, record.Phase = "infra", journal.DepsTierBInfra, PhaseDone
		return false, err
	case endBootstrap:
		deps.TierB = "skipped"
		return false, f.parkBump(ctx, record, b, state.LabelVerifyHuman, journal.DepsVerifyHuman, state.DepsBootstrapComment)
	case endStep:
		return false, f.verifierEnded(ctx, record, b, got.run)
	}
	var failed []fix.Verification
	for _, res := range got.round.Results {
		if res.Verdict != checklist.Pass {
			failed = append(failed, tierBFailure(res))
		}
	}
	deps.TierB = fmt.Sprintf("%d/%d", len(got.round.Results)-len(failed), len(got.round.Results))
	if len(got.round.Results) == 0 {
		return false, fmt.Errorf("Tier B on Bot PR #%d verified no item: its Bot-PR checklist holds no `- [ ]` item under a `## Manual verification` heading", b.pr.Number)
	}
	if len(failed) > 0 {
		return false, f.fixTierB(ctx, record, b, failed)
	}
	comment, err := state.TierBComment(b.pr.HeadSHA, record.Verify.Line)
	if err != nil {
		return false, err
	}
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, b.pr.Number, comment); err != nil {
		return false, fmt.Errorf("Tier B passed on Bot PR #%d, but its marker could not be posted: %w", b.pr.Number, err)
	}
	b.history.TierB = true
	return true, nil
}

// tierBFailure is an item Tier B did not pass, as a Fix is told it: a
// deferral says why it could not be checked.
func tierBFailure(res checklist.Result) fix.Verification {
	detail := res.Evidence
	if res.Verdict == checklist.Defer {
		detail = append([]string{"deferred, which fails Tier B: " + given(res.Deferral)}, detail...)
	}
	return fix.Verification{Item: res.Item.Label(), Detail: strings.Join(detail, "\n")}
}

// given is a reason as the Fire says it.
func given(why string) string {
	if strings.TrimSpace(why) == "" {
		return "<not given>"
	}
	return why
}

// fixTierB answers a failed Tier B with a Fix while the bump's budget
// lasts, and parks the PR when it is spent, or when the Fix disputes the
// checklist itself. A Fix that answered is committed with the skip trailer,
// pushed, never by force, and counted on the pushed head.
func (f *Fire) fixTierB(ctx context.Context, record *journal.Record, b *bump, failed []fix.Verification) error {
	deps, budgetCap, head := record.Deps, b.harness.Rounds.DepsFix, b.pr.HeadSHA
	items := make([]string, len(failed))
	for i, v := range failed {
		items[i] = v.Item
	}
	failure := "Tier B: " + strings.Join(items, "; ")
	park := func(why string) error {
		deps.TierB += " failed"
		return f.parkDeps(ctx, record, b, func(sha string) string { return state.DepsParkedComment(budgetCap, why, sha) })
	}
	if b.history.RoundsLeft(budgetCap) == 0 {
		return park(failure)
	}
	result, ok, unanswered, err := f.fixBump(ctx, record, b, fix.FailedVerification(failed, nil))
	if unanswered {
		return f.countBumpAttempt(ctx, record, b, head, false, err)
	}
	if !ok {
		return err
	}
	if why := strings.TrimSpace(result.Dispute); why != "" {
		// A dispute changes nothing: whatever the Fix left is nobody's.
		if err := f.Workspace.Discard(ctx); err != nil {
			return fmt.Errorf("discarding what the disputing Fix left in the checkout: %w", err)
		}
		return park(failure + "; the Fix disputes the checklist: " + why)
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
	if commit.Empty() {
		return f.countBumpAttempt(ctx, record, b, head, false, errors.New("the Fix changed nothing for the failed Tier B items"))
	}
	record.Commit = commit.SHA
	if err := f.countBumpAttempt(ctx, record, b, commit.SHA, true, nil); err != nil {
		return err
	}
	deps.Outcome, record.Phase = journal.DepsTierBFixed, PhaseDone
	return nil
}

// verifierEnded ends the Fire on a Tier B Verify Step that did not answer.
// Cut off by the usage limit, by a dead credential, or by a stop, it is no
// verdict and spends nothing, as stepEnded says. Any other end, a crash or
// an answer that does not hold, is an attempt spent on the head from the
// bump's one fix budget: a Step failing the same way on every Fire would
// otherwise hold the reconcile Lane, and every pick, forever. With the
// budget spent the PR is parked.
func (f *Fire) verifierEnded(ctx context.Context, record *journal.Record, b *bump, run step.Run) error {
	if run.Outcome == step.Exhausted || run.Outcome == step.AuthDead || run.Outcome == step.OK || stopping(ctx) {
		return f.stepEnded(ctx, record, verifier.Name, run)
	}
	why := fmt.Errorf("the %s Step ended %s: %s", verifier.Name, run.Outcome, run.Detail)
	return f.spendOnHead(ctx, record, b, "Tier B: "+why.Error(), why)
}

// spendOnHead spends one attempt from the bump's fix budget on its head
// for a failure no Fix answers, and parks the PR when the budget is spent
// already: whatever ended this Fire would end every later one on the same
// head the same way. failure is what the park says; why is the Fire's
// error, if any.
func (f *Fire) spendOnHead(ctx context.Context, record *journal.Record, b *bump, failure string, why error) error {
	budgetCap := b.harness.Rounds.DepsFix
	if b.history.RoundsLeft(budgetCap) == 0 {
		return f.parkDeps(ctx, record, b, func(sha string) string { return state.DepsParkedComment(budgetCap, failure, sha) })
	}
	return f.countBumpAttempt(ctx, record, b, b.pr.HeadSHA, false, why)
}

// stepEnded ends the Fire on a Step of the Bot PR Lane that did not answer:
// cut off by the usage limit it just ends, and the next Fire takes the bump
// again; a dead credential parks the Daemon. Neither is the bump's fault,
// and no attempt is spent.
func (f *Fire) stepEnded(ctx context.Context, record *journal.Record, name string, run step.Run) error {
	switch {
	case run.Outcome == step.Exhausted:
		record.ResetAt = budget.ReportOf(run, f.Now()).ResetAt
		return nil
	case run.Outcome == step.AuthDead:
		return f.parkOnDead(ctx, record, fmt.Errorf("the %s Step of Fire %s ended %s: %s", name, record.FireID, run.Outcome, run.Detail))
	case run.Outcome == step.OK:
		return fmt.Errorf("the Daemon was told to stop during the %s Step", name)
	}
	return fmt.Errorf("the %s Step ended %s: %s", name, run.Outcome, run.Detail)
}

// gateBump asks the deps gate about the head both tiers vouched for, on what
// GitHub says now, and does what it says: merges the head, pinned to it, so
// that a commit pushed after the gate is never merged; hands a major bump
// to a human; says once why a head only a human can answer is refused; and
// leaves a head that moved, or a gate that could not read, for the next
// Fire. A refusal the next Fire would meet again on the same head, checks
// no longer green or missing, markers that do not stand, or a merge GitHub
// refused, spends an attempt from the bump's fix budget, and parks the PR
// once it is spent: the tier markers stand, so nothing else would ever
// answer it, and the reconcile Lane would be held on it forever.
func (f *Fire) gateBump(ctx context.Context, record *journal.Record, b *bump) error {
	owner, name, number, sha := f.Repo.Owner, f.Repo.Name, b.pr.Number, b.pr.HeadSHA
	deps := record.Deps
	record.Phase = PhaseDepsGate
	facts := f.gateFacts(ctx, record, b)
	verdict := depsgate.Decide(facts)
	deps.Gate, record.Phase = verdict.String(), PhaseDone
	switch {
	case verdict.Approved():
		record.Phase = PhaseMerge
		// The squash commit is titled as GitHub titles one, with the PR.
		merged, err := f.GitHub.MergePullRequest(ctx, owner, name, number, verdict.SHA, fmt.Sprintf("%s (#%d)", facts.PR.Title, number))
		record.Phase = PhaseDone
		if err != nil {
			deps.Gate, deps.Outcome = "merge-failed: "+err.Error(), journal.DepsRefused
			current, readErr := f.GitHub.PullRequest(ctx, owner, name, number)
			switch {
			case readErr != nil:
				return fmt.Errorf("the merge of Bot PR #%d failed (%v), and whether its head moved could not be read: %w", number, err, readErr)
			case current.State != "open" || current.Merged || current.HeadSHA != sha:
				// A new head, or none: the next Fire answers it.
				return nil
			}
			return f.spendOnHead(ctx, record, b, "the deps gate approved "+sha+", but the merge failed: "+err.Error(), nil)
		}
		deps.Merged, deps.Outcome = merged, journal.DepsMerged
		return nil
	case verdict.Reason == depsgate.MajorUnapproved:
		return f.handOff(ctx, record, b, facts.PR, facts.History)
	case verdict.Human() && facts.History.Refused != string(verdict.Reason):
		comment := state.RefusedComment(string(verdict.Reason), verdict.Detail, sha)
		if err := f.GitHub.CreateComment(ctx, owner, name, number, comment); err != nil {
			return fmt.Errorf("the deps gate refused Bot PR #%d (%s), but the comment saying so was not posted: %w", number, verdict, err)
		}
	case sameHeadRefusal(verdict, facts):
		deps.Outcome = journal.DepsRefused
		return f.spendOnHead(ctx, record, b, "the deps gate refused "+sha+": "+verdict.String(), nil)
	}
	deps.Outcome = journal.DepsRefused
	return nil
}

// sameHeadRefusal says the gate refused, for a reason that is neither a
// human's nor a read that failed, a head the PR is still at: the next Fire
// would take that head, skip both tiers on their markers, and be refused
// the same way.
func sameHeadRefusal(verdict depsgate.Verdict, facts depsgate.Facts) bool {
	switch verdict.Reason {
	case depsgate.ChecksNotGreen, depsgate.ChecksMissing, depsgate.MarkersStale:
		return facts.PRErr == nil && facts.PR.HeadSHA == verdict.SHA
	}
	return false
}

// gateFacts reads what the deps gate decides on, for the PR as it is now:
// its state, the Daemon's markers, the checks and the human reviews of the
// head both tiers vouched for, and the size of the bump from Dependabot's
// first commit message. A bump whose commits cannot be read is sized major:
// one nobody could size is never landed without a human.
func (f *Fire) gateFacts(ctx context.Context, record *journal.Record, b *bump) depsgate.Facts {
	owner, name, number, sha := f.Repo.Owner, f.Repo.Name, b.pr.Number, b.pr.HeadSHA
	facts := depsgate.Facts{Gated: sha, Required: b.harness.RequiredChecks, Major: true}
	if facts.PR, facts.PRErr = f.GitHub.PullRequest(ctx, owner, name, number); facts.PRErr != nil {
		return facts
	}
	comments, err := f.GitHub.Comments(ctx, owner, name, number)
	if err != nil {
		facts.PRErr = fmt.Errorf("its comments, which carry the markers: %w", err)
		return facts
	}
	if facts.History, err = state.ReadDepsHistory(comments, f.Login, sha); err != nil {
		facts.PRErr = err
		return facts
	}
	facts.Checks, facts.ChecksErr = f.GitHub.Checks(ctx, owner, name, sha)
	reviews, err := f.GitHub.Reviews(ctx, owner, name, number)
	facts.Reviewed, facts.ReviewsErr = state.ReviewOf(reviews, f.Login, sha), err
	commits, err := f.GitHub.PullCommits(ctx, owner, name, number)
	switch {
	case err != nil:
		record.Warnings = append(record.Warnings, fmt.Sprintf("the commits of Bot PR #%d could not be read, so it counts as a major bump: %v", number, err))
	case len(commits) > 0:
		facts.Major = state.MajorBump(commits[0].Message)
	}
	record.Deps.Major = facts.Major
	return facts
}

// handOff hands a major bump both tiers passed to a human: HITL, and one
// comment for the head saying what moved, what the tiers found, where the
// tour is, and that an approving review lands it. A later Fire that finds
// the head approved gates it again and merges it. Only the moves still owed
// are made.
func (f *Fire) handOff(ctx context.Context, record *journal.Record, b *bump, pr github.PullRequest, history state.DepsHistory) error {
	owner, name, number, sha := f.Repo.Owner, f.Repo.Name, b.pr.Number, b.pr.HeadSHA
	record.Deps.Outcome = journal.DepsHITL
	if !slices.Contains(pr.Labels, state.LabelHITL) {
		if err := f.GitHub.AddLabels(ctx, owner, name, number, state.LabelHITL); err != nil {
			return fmt.Errorf("handing major Bot PR #%d to a human: %w", number, err)
		}
	}
	comments, err := f.GitHub.Comments(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("Bot PR #%d is labelled %s, but whether it says why could not be read: %w", number, state.LabelHITL, err)
	}
	if f.posted(comments, state.HandOffMarker(sha)) {
		return nil
	}
	tierB := "PASS, recorded on " + sha
	if record.Verify != nil {
		tierB = "PASS (" + record.Verify.Line + ")"
	}
	if err := f.GitHub.CreateComment(ctx, owner, name, number, state.HandOffComment(pr.Title, tierB, sha)); err != nil {
		return fmt.Errorf("Bot PR #%d is labelled %s, but the hand-off was not posted: %w", number, state.LabelHITL, err)
	}
	return nil
}

// posted says the Daemon posted a comment carrying marker.
func (f *Fire) posted(comments []github.Comment, marker string) bool {
	return slices.ContainsFunc(comments, func(c github.Comment) bool {
		return strings.Contains(c.Body, marker) && strings.EqualFold(c.Author, f.Login)
	})
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

// parkDeps parks a Bot PR with AFK:deps-failed: its fix budget is spent, or
// its conflict is a human's. comment is the park's comment for the head,
// saying why.
func (f *Fire) parkDeps(ctx context.Context, record *journal.Record, b *bump, comment func(sha string) string) error {
	record.Phase = PhaseDepsFailed
	return f.parkBump(ctx, record, b, state.LabelDepsFailed, journal.DepsParked, comment)
}

// parkBump leaves a Bot PR to a human: a draft, which is what keeps the PR
// Triage off it, label, and one comment saying why, carrying the park's
// marker for its head. It reads the PR first and makes only the moves still
// owed, so a park after a crash halfway through posts nothing twice.
// comment is that comment for the head. A parked PR is not a failed Fire.
func (f *Fire) parkBump(ctx context.Context, record *journal.Record, b *bump, label, outcome string, comment func(sha string) string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	if record.Phase != PhaseDepsFailed {
		record.Phase = PhaseDone
	}
	current, err := f.GitHub.PullRequest(ctx, owner, name, b.pr.Number)
	if err != nil {
		return fmt.Errorf("parking Bot PR #%d: %w", b.pr.Number, err)
	}
	if !current.Draft {
		if err := f.GitHub.MarkDraft(ctx, current); err != nil {
			return fmt.Errorf("parking Bot PR #%d: marking it draft: %w", b.pr.Number, err)
		}
	}
	if !slices.Contains(current.Labels, label) {
		if err := f.GitHub.AddLabels(ctx, owner, name, b.pr.Number, label); err != nil {
			return fmt.Errorf("parking Bot PR #%d: %w", b.pr.Number, err)
		}
	}
	record.ParkedPR, record.Deps.Outcome = label, outcome
	comments, err := f.GitHub.Comments(ctx, owner, name, b.pr.Number)
	if err != nil {
		return fmt.Errorf("Bot PR #%d is parked, but whether it says so could not be read: %w", b.pr.Number, err)
	}
	if f.posted(comments, state.ParkMarker(current.HeadSHA)) {
		return nil
	}
	if err := f.GitHub.CreateComment(ctx, owner, name, b.pr.Number, comment(current.HeadSHA)); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("Bot PR #%d is parked, but the comment saying so was not posted: %v", b.pr.Number, err))
	}
	return nil
}
