package fire

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/pick"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/research"
)

// PhaseTask: the Research Step in task mode, on a task Decision ticket.
const PhaseTask = "task"

// How a task ticket ends when it is not done.
const (
	// NeedsHuman: the task's work needs access or a decision only a human
	// has.
	NeedsHuman = research.TaskNeedsHuman
	// TaskError is how a task ticket fails once its attempts at the Step
	// ran out.
	TaskError = "task-error"
	// TaskResolveFailed is how a done task fails when it cannot be closed:
	// AFK:failed keeps the next Fire from doing its work a second time.
	TaskResolveFailed = "resolve-failed"
)

// task works a task Decision ticket to a resolved one, as
// plugin/skills/afk-resolve §8 did: claimed, its work done by the Research
// Step in task mode on the default branch's checkout, then resolved by Go
// from the answer. A task opens no branch and no PR: whatever the Step
// changed in the clone is never committed or pushed, and the clone is put
// back where it started on every way out. paused is set when the ticket
// carries AFK:paused from a Step the usage limit cut off: it is resumed by
// its session, like any Step.
func (f *Fire) task(ctx context.Context, record *journal.Record, harness config.Harness, issue github.Issue, paused bool) (err error) {
	number, owner, name := issue.Number, f.Repo.Owner, f.Repo.Name
	in := research.TaskInputOf(f.researchInput(ctx, record, harness, issue))
	resolve := &journal.Resolve{Type: "task"}
	record.Resolve = resolve
	start, err := f.Workspace.Head(ctx)
	if err != nil {
		return fmt.Errorf("reading the clone: %w", err)
	}
	defer func() {
		resetCtx, cancel := detached(ctx)
		defer cancel()
		if resetErr := f.Workspace.Reset(resetCtx, start); resetErr != nil {
			resetErr = fmt.Errorf("putting the clone back after the task Step: %w", resetErr)
			if err != nil {
				record.Warnings = append(record.Warnings, resetErr.Error())
				return
			}
			err = resetErr
		}
	}()
	spec := research.TaskSpec()

	record.Phase = PhaseGate
	verdict, ok, err := f.gate(ctx, record, spec.Name)
	if !ok {
		return err
	}
	if f.Login != "" {
		if err := f.GitHub.AddAssignees(ctx, owner, name, number, f.Login); err != nil {
			return fmt.Errorf("claiming #%d: %w", number, err)
		}
	}
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	// No branch: a pause freezes nothing, and the session holds the work.
	t := &ticket{issue: issue, harness: harness, paused: paused}

	session := ""
	if paused {
		session = f.Journal.Session(f.Repo.Slug(), number, spec.Name)
	}
	record.Phase = PhaseTask
	if err := f.Journal.Write(*record); err != nil {
		return fmt.Errorf("writing the Fire record: %w", err)
	}
	fresh := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: in}
	result, run := resumeOrCall(ctx, f, record, spec, fresh, *verdict, session, research.TaskResult.Check)
	if stop, err := f.settle(ctx, record, t, spec.Name, run, cmp.Or(run.SessionID, session)); stop {
		if record.Phase == PhaseFailed {
			resolve.Failed = TaskError
			f.resolveComment(ctx, record, number, TaskError, fmt.Sprintf("the Research Step was asked to do the task; its last attempt came back %s: %s", run.Outcome, run.Detail))
		}
		return err
	}
	switch result.Outcome {
	case research.TaskNeedsHuman:
		return f.failResearch(ctx, record, number, NeedsHuman, strings.TrimSuffix(strings.TrimSpace(result.Summary), "."))
	case research.TaskNeedsCode:
		return f.needsCode(ctx, record, harness, number, result.Summary)
	}

	record.Phase = PhaseResolve
	answer := research.TaskResolution(result)
	// The work is done and may not be safe to do twice: a ticket that cannot
	// be closed is failed, so no later Fire picks it to run again.
	if err := f.GitHub.CreateComment(ctx, owner, name, number, answer); err != nil {
		return f.failResearch(ctx, record, number, TaskResolveFailed, fmt.Sprintf("the task is done, but its resolution comment was not posted (%v); what was done: %s", err, strings.TrimSpace(answer)))
	}
	if err := f.GitHub.CloseIssue(ctx, owner, name, number); err != nil {
		return f.failResearch(ctx, record, number, TaskResolveFailed, fmt.Sprintf("the task is done and its resolution comment is posted, but the ticket was not closed (%v)", err))
	}
	if err := f.GitHub.AddLabels(ctx, owner, name, number, state.LabelDone); err != nil {
		return fmt.Errorf("#%d is resolved, but %s could not be applied: %w", number, state.LabelDone, err)
	}
	resolve.Resolved = true
	mapNumber := 0
	if in.Map != nil {
		mapNumber = in.Map.Number
	}
	f.graduate(ctx, record, harness, issue, landing{gist: answer, mapNumber: mapNumber})
	record.Phase = PhaseDone
	return nil
}

// needsCode hands a task that needs product code to a human, as
// plugin/skills/afk-resolve §8 did: HITL on first, so the ticket is never
// nobody's, then off the pick signal (AFK, and its item on the board under a
// Project pick), then the comment saying what the code would take. The lock
// comes off as the Fire ends. It is a resolve, not a failure.
func (f *Fire) needsCode(ctx context.Context, record *journal.Record, harness config.Harness, number int, what string) error {
	owner, name := f.Repo.Owner, f.Repo.Name
	if err := f.GitHub.AddLabels(ctx, owner, name, number, state.LabelHITL); err != nil {
		return fmt.Errorf("the task #%d needs code, but %s could not be applied: %w", number, state.LabelHITL, err)
	}
	record.Resolve.HITL = true
	harness.Repo = f.Repo
	if _, err := pick.Remove(ctx, f.GitHub, harness, number); err != nil {
		return fmt.Errorf("the task #%d needs code and is %s, but it could not be taken off the pick signal: %w", number, state.LabelHITL, err)
	}
	if err := f.GitHub.CreateComment(ctx, owner, name, number, research.NeedsCodeComment(what)); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("#%d is %s, but the comment saying what its code would take was not posted: %v", number, state.LabelHITL, err))
	}
	record.Phase = PhaseDone
	return nil
}
