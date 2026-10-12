// Package fire runs one Fire: it reads the Target Project's state from
// GitHub, derives what to do, and does it. Where judgement is needed it
// calls one Step and then acts on the answer itself: the Step never writes
// to GitHub and never pushes (ADR 0011, ADR 0012).
package fire

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/workspace"
)

// The phases of a Fire, as its record names them: the last one it started.
const (
	PhasePreflight = "preflight"
	PhaseImplement = "implement"
	PhaseCommit    = "commit"
	PhasePR        = "pr"
	PhaseDone      = "done"
)

// releaseTimeout bounds releasing the Single-flight lock once the Fire's own
// context is gone.
const releaseTimeout = 30 * time.Second

// ReadFacts reads from GitHub what a Fire derives its state from. It makes
// read requests only. warnings are what GitHub could not tell us that did
// not stop the read.
func ReadFacts(ctx context.Context, client *github.Client, repo config.Repo, login string, pick config.Pick) (facts state.Facts, warnings []string, err error) {
	warnings = []string{}
	facts = state.Facts{Login: login, Pick: pick}
	holders, err := client.OpenIssuesLabelled(ctx, repo.Owner, repo.Name, state.LabelInProgress)
	if err != nil {
		// An unreadable lock counts as held; say why.
		facts.LockUnreadable = true
		return facts, append(warnings, "the Single-flight lock could not be read: "+err.Error()), nil
	}
	if facts.InFlight = len(holders); facts.InFlight > 0 {
		return facts, warnings, nil
	}
	priorityField := ""
	if pick.Project != nil {
		priorityField = pick.Project.PriorityField
	}
	queue, queueWarnings, err := client.Queue(ctx, repo.Owner, repo.Name, state.LabelAFK, priorityField)
	if err != nil {
		return facts, warnings, err
	}
	facts.Queue = queue
	warnings = append(warnings, queueWarnings...)
	if len(queue) == 0 {
		return facts, warnings, nil
	}
	prs, err := client.OpenPullRequests(ctx, repo.Owner, repo.Name)
	if err != nil {
		return facts, warnings, err
	}
	for _, pr := range prs {
		// A fork's branch of the same name is not the Daemon's.
		if strings.EqualFold(pr.HeadRepo, repo.Slug()) {
			facts.OpenPRs = append(facts.OpenPRs, pr)
		}
	}
	return facts, warnings, nil
}

// Fire is one pass of the Daemon over one Target Project.
type Fire struct {
	Repo config.Repo
	Host config.Host
	// Login is the account the Daemon acts as.
	Login     string
	GitHub    *github.Client
	Runner    step.Runner
	Workspace *workspace.Workspace
	Journal   journal.Journal
	Now       func() time.Time
}

// Once runs the Fire and returns its record, which is also in the State
// dir. The record's Error says why a Fire failed; a Fire that found nothing
// to do did not fail.
func (f *Fire) Once(ctx context.Context) journal.Record {
	started := f.Now().UTC()
	record := journal.Record{FireID: journal.NewFireID(started), Repo: f.Repo.Slug(), StartedAt: started, Phase: PhasePreflight}
	err := f.run(ctx, &record)
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

func (f *Fire) run(ctx context.Context, record *journal.Record) (err error) {
	// The record exists before anything is done, so a Fire that dies leaves
	// one behind.
	if err := f.Journal.Write(*record); err != nil {
		return fmt.Errorf("writing the Fire record: %w", err)
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
	facts, warnings, err := ReadFacts(ctx, f.GitHub, f.Repo, f.Login, harness.Pick)
	record.Warnings = warnings
	if err != nil {
		return err
	}
	derived := state.Derive(facts)
	record.Kind = string(derived.Kind)
	if derived.Next.Action != state.ActionImplement {
		record.Phase = PhaseDone
		return nil
	}
	number := derived.Next.Issue
	record.Issue = &number
	owner, name := f.Repo.Owner, f.Repo.Name
	issue, err := f.GitHub.Issue(ctx, owner, name, number)
	if err != nil {
		return err
	}

	if err := f.GitHub.AddLabels(ctx, owner, name, number, state.LabelInProgress); err != nil {
		return fmt.Errorf("applying the Single-flight lock: %w", err)
	}
	// The lock comes off on every exit, a cancelled Fire included: a lock
	// left behind stops every later Fire.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if releaseErr := f.GitHub.RemoveLabel(releaseCtx, owner, name, number, state.LabelInProgress); releaseErr != nil {
			releaseErr = fmt.Errorf("releasing the Single-flight lock on #%d: %w", number, releaseErr)
			if err != nil {
				record.Warnings = append(record.Warnings, releaseErr.Error())
				return
			}
			err = releaseErr
		}
	}()

	branch, err := f.Workspace.OpenBranch(ctx, number)
	if err != nil {
		return fmt.Errorf("opening the branch: %w", err)
	}
	record.Branch, record.Phase = branch, PhaseImplement
	if err := f.Journal.Write(*record); err != nil {
		return fmt.Errorf("writing the Fire record: %w", err)
	}
	result, err := f.implement(ctx, record, harness, issue, branch)
	if err != nil {
		return err
	}

	record.Phase = PhaseCommit
	commit, err := f.Workspace.CommitAndPush(ctx, branch, commitMessage(result, number))
	if err != nil {
		return fmt.Errorf("committing and pushing: %w", err)
	}
	record.Commit = commit.SHA
	if !result.Finished {
		// What it did is on the branch; the next Fire carries on from there.
		return fmt.Errorf("the implement Step did not finish: %s", strings.TrimSpace(result.Summary))
	}

	record.Phase = PhasePR
	pr, err := f.GitHub.CreatePullRequest(ctx, owner, name, github.NewPullRequest{
		Title: result.Subject(), Head: branch, Base: f.Repo.DefaultBranch,
		Body: PullRequestBody(issue, result, f.Now()),
	})
	if err != nil {
		return fmt.Errorf("opening the pull request: %w", err)
	}
	record.PR, record.Phase = &pr.Number, PhaseDone
	return nil
}

// implement runs the Implement Step and journals it. Anything but a result
// that passes its checks is an error, and nothing is committed.
func (f *Fire) implement(ctx context.Context, record *journal.Record, harness config.Harness, issue github.Issue, branch string) (implement.Result, error) {
	spec := implement.Spec()
	input := implement.Input{
		Issue:  implement.Issue{Number: issue.Number, Title: issue.Title, Body: issue.Body},
		Branch: branch,
		Commands: implement.Commands{
			Install: harness.Commands.Install, Test: harness.Commands.Test, Lint: harness.Commands.Lint,
		},
		CommitScopes: harness.CommitScopes,
	}
	started := f.Now()
	run := f.Runner.Run(ctx, spec, step.Call{Dir: f.Workspace.Dir(), Model: f.Host.Model.Primary, Input: input})
	var result implement.Result
	if run.Outcome == step.OK {
		// After the schema passes, Go re-checks what a schema cannot say.
		var err error
		if result, err = step.Decode[implement.Result](run); err == nil {
			err = result.Check(harness.CommitScopes)
		}
		if err != nil {
			run.Outcome, run.Detail = step.Malformed, err.Error()
		}
	}
	record.Steps = append(record.Steps, journal.StepOf(spec.Name, started, run))
	if run.Outcome != step.OK {
		return implement.Result{}, fmt.Errorf("the %s Step ended %s: %s", spec.Name, run.Outcome, run.Detail)
	}
	return result, nil
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

// smokeNotRun stands in the pull request's Smoke section until the Daemon
// runs the smoke itself.
const smokeNotRun = "smoke: SKIPPED — this build does not run the smoke yet"

// PullRequestBody is the body of the pull request a Fire opens: the sections
// an Agent PR has always had, with the Step's checklist as the manual
// verification.
func PullRequestBody(issue github.Issue, result implement.Result, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Summary\n\n%s\n\n", strings.TrimSpace(result.Summary))
	fmt.Fprintf(&b, "## Closes\n\nCloses #%d — %s\n\n", issue.Number, issue.Title)
	fmt.Fprintf(&b, "## Smoke\n\n`%s`\n\n", smokeNotRun)
	b.WriteString("## Manual verification\n\n")
	for _, item := range result.Checklist {
		fmt.Fprintf(&b, "- [ ] %s\n", strings.TrimSpace(item))
	}
	fmt.Fprintf(&b, "\n---\n\nGenerated by `auto-agent` at %s\n", now.UTC().Format(time.RFC3339))
	return b.String()
}
