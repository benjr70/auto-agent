// Package fire runs one Fire: it reads the Target Project's state from
// GitHub, derives what to do, and does it. Where judgement is needed it
// calls one Step and then acts on the answer itself: the Step never writes
// to GitHub and never pushes (ADR 0011, ADR 0012).
package fire

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/precommit"
	"github.com/benjr70/auto-agent/internal/verify"
	"github.com/benjr70/auto-agent/internal/workspace"
)

// The phases of a Fire, as its record names them: the last one it started.
const (
	PhasePreflight = "preflight"
	PhaseGate      = "gate"
	PhaseImplement = "implement"
	PhaseCommit    = "commit"
	// PhaseReview and PhaseFix: a round of the pre-commit review, and the
	// Fix that answers it.
	PhaseReview = "review"
	PhaseFix    = "fix"
	// PhaseSmoke: the smoke, through the Environment provider.
	PhaseSmoke = "smoke"
	PhasePR    = "pr"
	PhaseDone  = "done"
	// PhasePaused: the usage limit cut the Step off, and the ticket waits.
	PhasePaused = "paused"
	// PhaseFailed: the ticket used its attempts and was handed to a human.
	PhaseFailed = "failed"
	// PhaseParked: Claude's credential is dead, and the Daemon is Parked.
	PhaseParked = "parked"
)

// releaseTimeout bounds releasing the Single-flight lock once the Fire's own
// context is gone.
const releaseTimeout = 30 * time.Second

// maxRecoveries bounds how many crashed Fires one Fire clears up after.
const maxRecoveries = 3

// maxUnparks bounds how many park issues one Fire closes.
const maxUnparks = 3

// ReadFacts reads from GitHub what a Fire derives its state from. It makes
// read requests only, and no more of them than the state needs: a park
// issue ends the read, and so do a held lock, an Agent PR to reconcile and
// a paused ticket, in the order between Lanes. warnings are what GitHub
// could not tell us that did not stop the read.
func ReadFacts(ctx context.Context, s Source) (facts state.Facts, warnings []string, err error) {
	warnings = []string{}
	client, repo, login := s.GitHub, s.Repo, s.Login
	facts = state.Facts{Login: login, Pick: s.Harness.Pick, PauseCap: s.Harness.Rounds.PauseResume, LiveFire: s.Live}
	needsHuman, err := client.OpenIssues(ctx, repo.Owner, repo.Name, state.LabelNeedsHuman)
	if err != nil {
		return facts, warnings, err
	}
	if facts.Parked = parkIssue(needsHuman, login); facts.Parked != 0 {
		return facts, warnings, nil
	}
	held, err := client.OpenIssuesLabelled(ctx, repo.Owner, repo.Name, state.LabelInProgress)
	if err != nil {
		// An unreadable lock counts as held; say why.
		facts.LockUnreadable = true
		return facts, append(warnings, "the Single-flight lock could not be read: "+err.Error()), nil
	}
	sort.Ints(held)
	if facts.Held = held; len(held) > 0 && s.Live {
		return facts, warnings, nil
	}
	listed, err := client.OpenPullRequests(ctx, repo.Owner, repo.Name)
	if err != nil {
		return facts, warnings, err
	}
	prs := ownBranches(listed, repo)
	if len(held) > 0 {
		// The lock was left behind: where its ticket stands decides what is
		// done about it.
		facts.AgentPRs = agentPRs(prs, held[:1])
		return facts, warnings, nil
	}
	open, triageWarnings, err := s.triageReads(ctx, prs)
	warnings = append(warnings, triageWarnings...)
	if err != nil {
		return facts, warnings, err
	}
	if facts.OpenPRs = open; state.Triage(open, login) != nil {
		return facts, warnings, nil
	}
	paused, err := client.OpenIssuesLabelled(ctx, repo.Owner, repo.Name, state.LabelPaused)
	if err != nil {
		return facts, warnings, err
	}
	sort.Ints(paused)
	if len(paused) > 0 {
		ticket := state.Paused{Issue: paused[0]}
		comments, err := client.Comments(ctx, repo.Owner, repo.Name, ticket.Issue)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("the pauses of #%d could not be counted: %v", ticket.Issue, err))
		}
		for _, comment := range comments {
			// Only the Daemon's own comments count: anyone can write one.
			if state.IsPause(comment.Body) && strings.EqualFold(comment.Author, login) {
				ticket.Pauses++
			}
		}
		facts.Paused = []state.Paused{ticket}
		facts.AgentPRs = agentPRs(prs, paused[:1])
		return facts, warnings, nil
	}
	priorityField := ""
	if s.Harness.Pick.Project != nil {
		priorityField = s.Harness.Pick.Project.PriorityField
	}
	queue, queueWarnings, err := client.Queue(ctx, repo.Owner, repo.Name, state.LabelAFK, priorityField)
	if err != nil {
		return facts, warnings, err
	}
	facts.Queue = queue
	warnings = append(warnings, queueWarnings...)
	if facts.Merged, err = s.mergedResearch(ctx, queue); err != nil {
		warnings = append(warnings, "the merged research PRs could not be read: "+err.Error())
	}
	tickets := make([]int, len(queue))
	for i, issue := range queue {
		tickets[i] = issue.Number
	}
	facts.AgentPRs = agentPRs(prs, tickets)
	return facts, warnings, nil
}

// parkIssue is the lowest of the Daemon's own park issues among issues; 0
// when there is none. One that anyone else opened does not park it.
func parkIssue(issues []github.Issue, login string) int {
	lowest := 0
	for _, issue := range issues {
		if state.IsPark(issue.Body) && strings.EqualFold(issue.Author, login) && (lowest == 0 || issue.Number < lowest) {
			lowest = issue.Number
		}
	}
	return lowest
}

// ownBranches are the pull requests from the repository's own branches: a
// fork's branch of the same name is not the Daemon's.
func ownBranches(prs []github.PullRequest, repo config.Repo) []github.PullRequest {
	own := []github.PullRequest{}
	for _, pr := range prs {
		if strings.EqualFold(pr.HeadRepo, repo.Slug()) {
			own = append(own, pr)
		}
	}
	return own
}

// agentPRs are the open Agent PRs of the given tickets, by ticket, of the
// pull requests from the repository's own branches: a Slice's on its
// feat/issue-<n> branch, and a Decision ticket's research PR, by the ticket
// its title names.
func agentPRs(prs []github.PullRequest, tickets []int) map[int]int {
	branches, research := map[string]int{}, map[int]int{}
	for _, pr := range prs {
		branches[pr.HeadRef] = pr.Number
		if state.IsResearch(pr.HeadRef) {
			if ticket := state.TicketOf(pr); ticket != 0 {
				research[ticket] = pr.Number
			}
		}
	}
	open := map[int]int{}
	for _, ticket := range tickets {
		if pr, ok := branches[workspace.BranchName(ticket)]; ok {
			open[ticket] = pr
		} else if pr, ok := research[ticket]; ok {
			open[ticket] = pr
		}
	}
	return open
}

// Gate is the Budget gate as a Fire uses it: asked before every Step, told
// how every Step ended.
type Gate interface {
	// Verdict says whether the next Step may go. An error is a refusal
	// whatever the budget: a dead credential, or an auth mode it cannot
	// vouch for.
	Verdict(ctx context.Context) (budget.Verdict, error)
	// Observe records what a Step's run says about the budget.
	Observe(fireID string, run step.Run) error
}

// Fire is one pass of the Daemon over one Target Project.
type Fire struct {
	Repo config.Repo
	Host config.Host
	// Login is the account the Daemon acts as.
	Login     string
	GitHub    *github.Client
	Runner    step.Runner
	Gate      Gate
	Workspace *workspace.Workspace
	Journal   journal.Journal
	Now       func() time.Time
	// Sleep waits between reads while GitHub works something out.
	Sleep func(context.Context, time.Duration) error
	// SurfaceHost is the Host a Verification round's Surfaces run on; its
	// zero value is this process's environment.
	SurfaceHost verify.Host
	// StopBetweenSteps makes the end of Once's context a stop between
	// Steps, as the always-on Daemon's signals are: the Step in flight runs
	// to its end, and the Fire ends before it starts another, its lock
	// released and its record written. A wait on GitHub is cut short.
	// Unset, the end of the context cancels the Step in flight.
	StopBetweenSteps bool
}

// Once runs the Fire and returns its record, which is also in the State
// dir. The record's Error says why a Fire failed; a Fire that found nothing
// to do did not fail.
func (f *Fire) Once(ctx context.Context) journal.Record {
	started := f.Now().UTC()
	record := journal.Record{FireID: journal.NewFireID(started), Repo: f.Repo.Slug(), StartedAt: started, Phase: PhasePreflight}
	if f.StopBetweenSteps {
		ctx = betweenSteps(ctx)
	}
	err := f.runCaught(ctx, &record)
	ended, exit := f.Now().UTC(), 0
	if err != nil {
		record.Error, exit = err.Error(), 1
	}
	record.EndedAt, record.Exit = &ended, &exit
	if err := f.Journal.Write(record); err != nil && record.Error == "" {
		record.Error, exit = "writing the Fire record: "+err.Error(), 1
	}
	return record
}

// runCaught runs the Fire, with a panic as its error: the record says so,
// and the Single-flight lock it may have left is a crashed Fire's, cleared
// up by the next one.
func (f *Fire) runCaught(ctx context.Context, record *journal.Record) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the Fire panicked: %v", p)
		}
	}()
	return f.run(ctx, record)
}

func (f *Fire) run(ctx context.Context, record *journal.Record) error {
	// The record exists before anything is done, so a Fire that dies leaves
	// one behind.
	if err := f.Journal.Write(*record); err != nil {
		return fmt.Errorf("writing the Fire record: %w", err)
	}
	// One Fire at a time in a State dir: the clone is not shared, and a
	// lock on GitHub with no Fire running here is how a crash is known.
	release, alone, err := hold(f.Journal.StateDir)
	if err != nil {
		return fmt.Errorf("marking the Fire as running: %w", err)
	}
	defer release()
	if !alone {
		record.Kind, record.Phase = string(state.Derive(state.Facts{LiveFire: true}).Kind), PhaseDone
		return nil
	}
	if _, err := f.Workspace.Prepare(ctx); err != nil {
		return fmt.Errorf("preparing the Workspace: %w", err)
	}
	// The Harness config is the default branch's, read from the clone just
	// reset to it.
	harness, err := config.LoadHarness(f.Workspace.Dir(), "", f.Repo)
	if err != nil {
		return err
	}
	var derived state.State
	for recovered, unparked := 0, 0; ; {
		facts, warnings, err := ReadFacts(ctx, Source{GitHub: f.GitHub, Repo: f.Repo, Login: f.Login, Harness: harness, Sleep: f.sleep})
		record.Warnings = append(record.Warnings, warnings...)
		if err != nil {
			return err
		}
		derived = state.Derive(facts)
		if record.Kind == "" {
			record.Kind = string(derived.Kind)
		}
		switch derived.Kind {
		case state.KindParked:
			if unparked == maxUnparks {
				return fmt.Errorf("the Daemon is still Parked after closing %d park issues", unparked)
			}
			// Once the credential has been found alive, any other park
			// issue is closed without asking again.
			if unparked == 0 {
				if alive, err := f.probe(ctx, record, derived.Parked); err != nil || !alive {
					return err
				}
			}
			if err := f.unpark(ctx, derived.Parked); err != nil {
				return err
			}
			unparked++
			continue
		case state.KindCrashed:
			if recovered == maxRecoveries {
				return fmt.Errorf("the Single-flight lock is still held after clearing up after %d crashed Fires", recovered)
			}
			// With the crash cleared up, the state is derived again and the
			// work continues from wherever GitHub says it stands.
			if err := f.recover(ctx, record, derived.Next); err != nil {
				return err
			}
			recovered++
			continue
		}
		break
	}
	record.Action = string(derived.Next.Action)
	number := derived.Next.Issue
	if derived.Next.Action != state.ActionNone {
		record.Issue = &number
	}
	if r := derived.Reconcile; r != nil {
		// Nothing in the Lane is done yet: no lock, no write.
		pr, issue := r.PR, r.Issue
		record.PR, record.Reconcile, record.NotWorked = &pr, string(r.Reason), derived.Next.Reason
		if issue != 0 {
			record.Issue = &issue
		}
	}
	switch derived.Next.Action {
	case state.ActionImplement, state.ActionResume:
		return f.work(ctx, record, harness, derived.Next)
	case state.ActionResearch:
		issue, err := f.GitHub.Issue(ctx, f.Repo.Owner, f.Repo.Name, number)
		if err != nil {
			return err
		}
		return f.research(ctx, record, harness, issue, false)
	case state.ActionTask:
		issue, err := f.GitHub.Issue(ctx, f.Repo.Owner, f.Repo.Name, number)
		if err != nil {
			return err
		}
		return f.task(ctx, record, harness, issue, false)
	case state.ActionDocsMerge:
		return f.reconcileDocsMerge(ctx, record, harness, derived.Next)
	case state.ActionFinish:
		return f.finish(ctx, record, harness, derived.Next)
	case state.ActionReview:
		return f.reconcileReview(ctx, record, harness, derived.Next)
	case state.ActionRevise:
		return f.reconcileRevise(ctx, record, harness, derived.Next)
	case state.ActionRuling:
		return f.reconcileRuling(ctx, record, harness, derived.Next)
	case state.ActionWaitCI:
		return f.reconcileCI(ctx, record, harness, derived.Next)
	case state.ActionRebase:
		return f.reconcileRebase(ctx, record, harness, derived.Next)
	case state.ActionRelease:
		if err := f.release(ctx, number); err != nil {
			return err
		}
	case state.ActionFail:
		if err := f.fail(ctx, number); err != nil {
			return fmt.Errorf("failing #%d at the pause cap: %w", number, err)
		}
		if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, number, state.PauseCapComment(derived.Paused.Pauses)); err != nil {
			record.Warnings = append(record.Warnings, fmt.Sprintf("#%d is failed at the pause cap, but the comment saying so was not posted: %v", number, err))
		}
	}
	record.Phase = PhaseDone
	return nil
}

// recover clears up after a Fire that died holding the Single-flight lock.
// The stale lock comes off first, so that whatever stops this Fire next, the
// ticket is not found crashed again and charged twice for one crash. A crash
// is a failed attempt, unless the work was already done.
func (f *Fire) recover(ctx context.Context, record *journal.Record, next state.Next) error {
	number := next.Issue
	record.Issue = &number
	if next.Action == state.ActionRelease {
		return f.release(ctx, number)
	}
	if err := f.GitHub.RemoveLabel(ctx, f.Repo.Owner, f.Repo.Name, number, state.LabelInProgress); err != nil {
		return fmt.Errorf("releasing the Single-flight lock a crashed Fire left on #%d: %w", number, err)
	}
	crash := "the Fire working it ended without releasing the Single-flight lock: it crashed, or the machine went down"
	if attempts, err := f.countAttempt(ctx, number, implement.Spec().Name, crash); err != nil || attempts.Spent() {
		return f.attemptError(record, crash, attempts, err)
	}
	return nil
}

// release takes the state labels a finished ticket was left with off it.
func (f *Fire) release(ctx context.Context, number int) error {
	for _, label := range []string{state.LabelInProgress, state.LabelPaused} {
		if err := f.GitHub.RemoveLabel(ctx, f.Repo.Owner, f.Repo.Name, number, label); err != nil {
			return fmt.Errorf("taking %s, left behind, off #%d: %w", label, number, err)
		}
	}
	return nil
}

// fail hands a ticket to a human. The label goes on first: a ticket that is
// failed and still paused is failed again by the next Fire, one that is
// neither is picked anew.
func (f *Fire) fail(ctx context.Context, number int) error {
	if err := f.GitHub.AddLabels(ctx, f.Repo.Owner, f.Repo.Name, number, state.LabelFailed); err != nil {
		return err
	}
	return f.GitHub.RemoveLabel(ctx, f.Repo.Owner, f.Repo.Name, number, state.LabelPaused)
}

// reprobe is how long a Parked Daemon waits between probes.
func (f *Fire) reprobe() time.Duration { return time.Duration(f.Host.Gate.ReprobeSecs) * time.Second }

// hostname names this machine in the park issue.
func hostname() string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "host"
}

// probe asks the Budget gate about the credential of a Parked Daemon. The
// probe is the gate's own check, so a credential it passes is one the gate
// will not park on again. Only a gate with nothing against the credential
// passes it: an auth mode the gate cannot vouch for keeps the Daemon Parked
// too, with the issue still open for a human. alive is false when the
// Daemon stays Parked.
func (f *Fire) probe(ctx context.Context, record *journal.Record, issue int) (alive bool, err error) {
	if _, err := f.Gate.Verdict(ctx); err != nil {
		record.Phase = PhaseParked
		record.Park = &journal.Park{Issue: &issue, Reason: err.Error(), ProbeAt: f.Now().Add(f.reprobe()).UTC()}
		return false, nil
	}
	return true, nil
}

// unpark closes a park issue: the credential is alive again.
func (f *Fire) unpark(ctx context.Context, issue int) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	if err := f.GitHub.CreateComment(ctx, owner, name, issue, state.UnparkComment(f.Now())); err != nil {
		return fmt.Errorf("un-parking: commenting on #%d: %w", issue, err)
	}
	if err := f.GitHub.CloseIssue(ctx, owner, name, issue); err != nil {
		return fmt.Errorf("un-parking: closing #%d: %w", issue, err)
	}
	return nil
}

// park Parks the Daemon on a dead credential: it opens the one
// AFK:needs-human issue, or reuses the open one. A park issue that cannot be
// opened is opened by the next Fire, which finds the credential dead again.
func (f *Fire) park(ctx context.Context, record *journal.Record, reason string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	record.Phase = PhaseParked
	record.Park = &journal.Park{Reason: reason, ProbeAt: f.Now().Add(f.reprobe()).UTC()}
	open, err := f.GitHub.OpenIssues(ctx, owner, name, state.LabelNeedsHuman)
	if err != nil {
		return fmt.Errorf("parked, but the open %s issues could not be read: %w", state.LabelNeedsHuman, err)
	}
	if number := parkIssue(open, f.Login); number != 0 {
		record.Park.Issue = &number
		if err := f.GitHub.CreateComment(ctx, owner, name, number, state.ParkAgainComment(hostname(), f.Now(), reason, f.reprobe())); err != nil {
			record.Warnings = append(record.Warnings, fmt.Sprintf("parked behind #%d, but the comment saying so again was not posted: %v", number, err))
		}
		return nil
	}
	issue, err := f.GitHub.CreateIssue(ctx, owner, name, state.ParkIssue(hostname(), f.Now(), reason, f.reprobe()))
	if err != nil {
		return fmt.Errorf("parked, but the %s issue could not be opened: %w", state.LabelNeedsHuman, err)
	}
	record.Park.Issue = &issue.Number
	return nil
}

// parkOnDead parks the Daemon and returns the error of a Fire that found the credential dead: why, and
// whether the Daemon could say so on GitHub.
func (f *Fire) parkOnDead(ctx context.Context, record *journal.Record, dead error) error {
	if err := f.park(ctx, record, dead.Error()); err != nil {
		return fmt.Errorf("%w (%v; the next Fire tries again)", dead, err)
	}
	return dead
}

// detached is a context for the writes a Fire still owes GitHub once its own
// context is gone.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
}

// work takes one ticket to an open pull request: a ticket just picked, or a
// paused one resumed. A branch that already carries the ticket's finished
// Implement commit goes straight to the pre-commit review: how far a review
// got is not on GitHub, so it starts again.
func (f *Fire) work(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) (err error) {
	number := next.Issue
	owner, name := f.Repo.Owner, f.Repo.Name
	issue, err := f.GitHub.Issue(ctx, owner, name, number)
	if err != nil {
		return err
	}
	if next.Action == state.ActionResume {
		// A Research Step the usage limit cut off: resumed like any Step.
		switch state.WayfinderType(issue.Labels) {
		case "research":
			return f.research(ctx, record, harness, issue, true)
		case "task":
			return f.task(ctx, record, harness, issue, true)
		}
	}
	branch, err := f.Workspace.OpenBranch(ctx, number)
	if err != nil {
		return fmt.Errorf("opening the branch: %w", err)
	}
	record.Branch = branch
	messages, err := f.Workspace.Messages(ctx)
	if err != nil {
		return fmt.Errorf("reading the branch: %w", err)
	}
	implemented, done := state.Implemented(number, messages)
	first := implement.Spec().Name
	if done {
		first = precommit.Name
	}

	// The Budget gate is asked before the first Step, and before the lock: a
	// Fire told to wait leaves no label behind.
	record.Phase = PhaseGate
	verdict, ok, err := f.gate(ctx, record, first)
	if !ok {
		return err
	}

	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness, paused: next.Action == state.ActionResume}

	var result implement.Result
	if done {
		result = implementedResult(issue, implemented)
	} else {
		var finished bool
		if result, finished, err = f.implementStep(ctx, record, t, verdict); !finished {
			return err
		}
		// The review asks the gate for itself.
		verdict = nil
	}
	smoke, approved, err := f.preCommitReview(ctx, record, t, verdict)
	if !approved {
		return err
	}

	record.Phase = PhasePR
	pr, err := f.GitHub.CreatePullRequest(ctx, owner, name, github.NewPullRequest{
		Title: result.Subject(), Head: branch, Base: f.Repo.DefaultBranch,
		Body: PullRequestBody(issue, result, smoke, f.Now()),
	})
	if err != nil {
		return fmt.Errorf("opening the pull request: %w", err)
	}
	record.PR = &pr.Number
	if smoke.Bootstrap {
		// No provider smoked it: a human verifies it by hand.
		if err := f.GitHub.AddLabels(ctx, owner, name, pr.Number, state.LabelVerifyHuman); err != nil {
			return fmt.Errorf("PR #%d is open, but %s could not be applied: %w", pr.Number, state.LabelVerifyHuman, err)
		}
	}
	// Nothing waits between the PR opening and its review.
	if finished, err := f.reviewPR(ctx, record, t, pr.Number, nil); !finished {
		return err
	}
	record.Phase = PhaseDone
	return nil
}

// lock applies the Single-flight lock to the ticket.
func (f *Fire) lock(ctx context.Context, number int) error {
	if err := f.GitHub.AddLabels(ctx, f.Repo.Owner, f.Repo.Name, number, state.LabelInProgress); err != nil {
		return fmt.Errorf("applying the Single-flight lock: %w", err)
	}
	return nil
}

// unlock takes the Single-flight lock off the ticket, on every exit of the
// Fire that applied it, a cancelled one included: a lock left behind stops
// every later Fire. A failure is the Fire's error, or a warning when it has
// one already.
func (f *Fire) unlock(ctx context.Context, record *journal.Record, number int, err *error) {
	releaseCtx, cancel := detached(ctx)
	defer cancel()
	if releaseErr := f.GitHub.RemoveLabel(releaseCtx, f.Repo.Owner, f.Repo.Name, number, state.LabelInProgress); releaseErr != nil {
		releaseErr = fmt.Errorf("releasing the Single-flight lock on #%d: %w", number, releaseErr)
		if *err != nil {
			record.Warnings = append(record.Warnings, releaseErr.Error())
			return
		}
		*err = releaseErr
	}
}

// implementStep runs the Implement Step on the ticket and commits and pushes
// its work. finished is false when the Fire ends here, with err its error,
// if any; a ticket the Step did not finish is a failed attempt.
func (f *Fire) implementStep(ctx context.Context, record *journal.Record, t *ticket, verdict *budget.Verdict) (result implement.Result, finished bool, err error) {
	number, spec := t.issue.Number, implement.Spec()
	session := ""
	if t.paused {
		session = f.Journal.Session(f.Repo.Slug(), number, spec.Name)
	}
	record.Phase = PhaseImplement
	if err := f.Journal.Write(*record); err != nil {
		return result, false, fmt.Errorf("writing the Fire record: %w", err)
	}
	result, run := f.implement(ctx, record, t, *verdict, session)
	// The session the next Fire resumes, if this one leaves the ticket paused.
	if stop, err := f.settle(ctx, record, t, spec.Name, run, cmp.Or(run.SessionID, session)); stop {
		return result, false, err
	}
	record.Phase = PhaseCommit
	commit, err := f.Workspace.CommitAndPush(ctx, t.branch, commitMessage(result, number))
	if err != nil {
		return result, false, fmt.Errorf("committing and pushing: %w", err)
	}
	record.Commit = commit.SHA
	if !result.Finished {
		// What it did is on the branch; the next Fire carries on from there.
		return result, false, f.failedAttempt(ctx, record, number, spec.Name, fmt.Sprintf("the %s Step did not finish: %s", spec.Name, strings.TrimSpace(result.Summary)))
	}
	return result, true, nil
}

// What a resumed session is told.
const (
	carryOn = "This Step was cut off, by the usage limit or a dead credential, before it returned its result. The checkout is as you left it, with your work so far committed. Carry on from where you stopped, and return the result when you are done."
	correct = "Your result was not accepted: %s. Return the result again with that corrected. Change nothing else."
)

// implement runs the Implement Step, journals every call it makes, and
// returns the result with the last call's run. The result counts only when
// that run is OK. session is the session to resume, when the Step was cut
// off in an earlier Fire.
func (f *Fire) implement(ctx context.Context, record *journal.Record, t *ticket, verdict budget.Verdict, session string) (implement.Result, step.Run) {
	spec := implement.Spec()
	fresh := step.Call{Dir: f.Workspace.Dir(), Model: f.model(verdict), Input: implement.Input{
		Issue: t.stepIssue(), Branch: t.branch, Commands: t.commands(), CommitScopes: t.harness.CommitScopes,
	}}
	check := func(r implement.Result) error { return r.Check(t.harness.CommitScopes) }

	return resumeOrCall(ctx, f, record, spec, fresh, verdict, session, check)
}

// pause leaves a ticket whose Step was cut off ready to be resumed: its
// partial work pushed, AFK:paused on it, and comment on it, which counts the
// pause when it is a pause on the usage limit. why says what cut it off. A
// paused ticket is not a failed Fire.
func (f *Fire) pause(ctx context.Context, record *journal.Record, number int, branch, why, comment string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	// A task has no branch: nothing it did is ever pushed, and the session
	// holds its work.
	if branch != "" {
		commit, err := f.Workspace.CommitAndPush(ctx, branch, fmt.Sprintf("wip: freeze partial work on #%d (%s)\n", number, why))
		if err != nil {
			// Still paused: the session holds what the push did not.
			record.Warnings = append(record.Warnings, "the partial work was not pushed: "+err.Error())
		}
		record.Commit = commit.SHA
	}
	if err := f.GitHub.AddLabels(ctx, owner, name, number, state.LabelPaused); err != nil {
		return fmt.Errorf("pausing #%d: %w", number, err)
	}
	record.Phase = PhasePaused
	// The comment is the count: without it a pause on the usage limit is
	// not held against the cap.
	if err := f.GitHub.CreateComment(ctx, owner, name, number, comment); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("#%d is paused, but the comment saying so was not posted: %v", number, err))
	}
	return nil
}

// countAttempt counts one failed attempt at a Step in the hidden marker on
// the ticket, and at the cap hands the ticket to a human.
func (f *Fire) countAttempt(ctx context.Context, number int, stepName, reason string) (state.Attempts, error) {
	owner, name := f.Repo.Owner, f.Repo.Name
	comments, err := f.GitHub.Comments(ctx, owner, name, number)
	if err != nil {
		return state.Attempts{}, fmt.Errorf("the attempt was not counted: %w", err)
	}
	var prior state.Attempts
	var marker int64
	for _, comment := range comments {
		// Only the Daemon's own marker counts.
		if attempts, ok := state.ParseAttempts(comment.Body); ok && strings.EqualFold(comment.Author, f.Login) {
			prior, marker = attempts, comment.ID
		}
	}
	attempts := prior.After(stepName)
	if attempts.Spent() {
		// The label before the count: a count that says spent on a ticket
		// that is not failed would start again at one.
		if err := f.fail(ctx, number); err != nil {
			return state.Attempts{}, fmt.Errorf("the attempt was not counted: %s could not be applied: %w", state.LabelFailed, err)
		}
	}
	if marker == 0 {
		err = f.GitHub.CreateComment(ctx, owner, name, number, attempts.Comment(reason))
	} else {
		err = f.GitHub.UpdateComment(ctx, owner, name, marker, attempts.Comment(reason))
	}
	if err != nil {
		return state.Attempts{}, fmt.Errorf("the attempt was not counted: %w", err)
	}
	return attempts, nil
}

// attemptError is the error of a Fire that ended on a failed attempt: the
// reason, with where the count stands.
func (f *Fire) attemptError(record *journal.Record, reason string, attempts state.Attempts, err error) error {
	switch {
	case err != nil:
		return fmt.Errorf("%s (%w)", reason, err)
	case attempts.Spent():
		record.Phase = PhaseFailed
		return fmt.Errorf("%s (failed attempt %d of %d: %s applied)", reason, attempts.Count, state.MaxAttempts, state.LabelFailed)
	}
	return fmt.Errorf("%s (failed attempt %d of %d)", reason, attempts.Count, state.MaxAttempts)
}

// failedAttempt counts a failed attempt and returns the Fire's error for it.
func (f *Fire) failedAttempt(ctx context.Context, record *journal.Record, number int, stepName, reason string) error {
	attempts, err := f.countAttempt(ctx, number, stepName, reason)
	return f.attemptError(record, reason, attempts, err)
}

// commitMessage is the message Go commits the Step's work with: the Step's
// own, closing the ticket, or a marker for work that is not finished.
func commitMessage(result implement.Result, issue int) string {
	if !result.Finished {
		return fmt.Sprintf("wip: partial work on #%d\n\n%s\n", issue, strings.TrimSpace(result.Summary))
	}
	message := strings.TrimSpace(result.CommitMessage)
	closes := "Closes #" + strconv.Itoa(issue)
	if regexp.MustCompile(`(?im)^` + regexp.QuoteMeta(closes) + `\s*$`).MatchString(message) {
		return message + "\n"
	}
	return message + "\n\n" + closes + "\n"
}

// PullRequestBody is the body of the pull request a Fire opens: the sections
// an Agent PR has always had, with the smoke's result and the Step's
// checklist as the manual verification.
func PullRequestBody(issue github.Issue, result implement.Result, smoke verify.Result, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Summary\n\n%s\n\n", strings.TrimSpace(result.Summary))
	fmt.Fprintf(&b, "## Closes\n\nCloses #%d — %s\n\n", issue.Number, issue.Title)
	fmt.Fprintf(&b, "## Smoke\n\n`%s`\n\n", smoke.Trailer())
	b.WriteString("## Manual verification\n\n")
	for _, item := range result.Checklist {
		fmt.Fprintf(&b, "- [ ] %s\n", strings.TrimSpace(item))
	}
	fmt.Fprintf(&b, "\n---\n\nGenerated by `auto-agent` at %s\n", now.UTC().Format(time.RFC3339))
	return b.String()
}

// noCriteria stands in for the checklist of a ticket whose body has no
// acceptance criteria.
const noCriteria = "_(no Acceptance Criteria found in issue body)_"

var (
	criteriaHeading = regexp.MustCompile(`(?i)^##\s*acceptance criteria\s*$`)
	listItem        = regexp.MustCompile(`^\s*[-*]\s+(\[[ xX]\]\s+)?(.+)$`)
	closingLine     = regexp.MustCompile(`(?im)^Closes #\d+\s*$`)
)

// implementedResult is the Implement result of a branch that carried the
// ticket's finished Implement commit when this Fire found it: the Step's
// answer is gone, so the pull request is opened, as it always was, from the
// commit and from the ticket's acceptance criteria.
func implementedResult(issue github.Issue, message string) implement.Result {
	subject := step.Subject(message)
	_, body, _ := strings.Cut(strings.TrimSpace(message), "\n")
	summary, _, _ := strings.Cut(strings.TrimSpace(closingLine.ReplaceAllString(body, "")), "\n\n")
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
