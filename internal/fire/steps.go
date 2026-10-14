package fire

import (
	"context"
	"errors"
	"fmt"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
)

// ticket is the ticket a Fire works, as each Step on it sees it.
type ticket struct {
	issue   github.Issue
	branch  string
	harness config.Harness
	// paused is set while the ticket still carries AFK:paused from an
	// earlier Fire: it keeps the label until one of its Steps has ended.
	paused bool
}

// stepIssue is the ticket as a Step's input carries it.
func (t *ticket) stepIssue() implement.Issue {
	return implement.Issue{Number: t.issue.Number, Title: t.issue.Title, Body: t.issue.Body}
}

// commands are the Harness config commands a Step runs.
func (t *ticket) commands() implement.Commands {
	c := t.harness.Commands
	return implement.Commands{Install: c.Install, Test: c.Test, Lint: c.Lint}
}

// model is the Model policy's choice for a Step asked on verdict: the
// primary, unless its own limit is spent.
func (f *Fire) model(verdict budget.Verdict) string {
	if verdict.FireModel != "" {
		return string(verdict.FireModel)
	}
	return f.Host.Model.Primary
}

// gate asks the Budget gate before a Step and journals the verdict as a Step
// not yet run, so that every exit from here on leaves it in the record. ok
// is false when the Fire ends here: on a wait verdict, with no error, or on
// a refusal, with one; a dead credential parks the Daemon.
func (f *Fire) gate(ctx context.Context, record *journal.Record, name string) (verdict *budget.Verdict, ok bool, err error) {
	v, err := f.Gate.Verdict(ctx)
	record.Steps = append(record.Steps, journal.NotRun(name, f.Now(), v))
	if err != nil {
		err = fmt.Errorf("the Budget gate refused the %s Step: %w", name, err)
		if errors.Is(err, budget.ErrAuthDead) {
			return nil, false, f.parkOnDead(ctx, record, err)
		}
		return nil, false, err
	}
	if !v.ShouldFire {
		record.ResetAt = string(v.ResetAt)
		return nil, false, nil
	}
	return &v, true, nil
}

// runStep makes one call of a Step, re-checks its result where the schema
// cannot, and journals the call: in place of the entry the gate left, or
// after it. The result counts only when the run is OK.
func runStep[R any](ctx context.Context, f *Fire, record *journal.Record, spec step.Spec, call step.Call, verdict budget.Verdict, resume journal.Resume, check func(R) error) (R, step.Run) {
	started := f.Now()
	run := f.Runner.Run(ctx, spec, call)
	if err := f.Gate.Observe(record.FireID, run); err != nil {
		record.Warnings = append(record.Warnings, "the Budget gate could not record the Step: "+err.Error())
	}
	var result R
	if run.Outcome == step.OK {
		var err error
		if result, err = step.Decode[R](run); err == nil {
			err = check(result)
		}
		if err != nil {
			run.Outcome, run.Detail = step.Malformed, err.Error()
		}
	}
	entry := journal.StepOf(spec.Name, started, verdict, run)
	entry.Resume = resume
	if last := len(record.Steps) - 1; last >= 0 && record.Steps[last].Outcome == "" {
		record.Steps[last] = entry
	} else {
		record.Steps = append(record.Steps, entry)
	}
	return result, run
}

// correctOnce gives a result that failed a check one chance to be corrected,
// in the same session, before it counts as a failed attempt.
func correctOnce[R any](ctx context.Context, f *Fire, record *journal.Record, spec step.Spec, call step.Call, verdict budget.Verdict, check func(R) error, result R, run step.Run) (R, step.Run) {
	if run.Outcome != step.Malformed || run.SessionID == "" {
		return result, run
	}
	call.Resume, call.Message = run.SessionID, fmt.Sprintf(correct, run.Detail)
	return runStep(ctx, f, record, spec, call, verdict, journal.ResumeCorrection, check)
}

// settle acts on how a Step's call ended, for every end but a result. stop is
// true when the Fire ends here, with err its error. session is the session
// the next Fire resumes if this one leaves the ticket paused; empty for a
// Step that is run again instead.
func (f *Fire) settle(ctx context.Context, record *journal.Record, t *ticket, name string, run step.Run, session string) (stop bool, err error) {
	number := t.issue.Number
	switch run.Outcome {
	case step.Exhausted:
		record.ResumeSession = session
		record.ResetAt = budget.ReportOf(run, f.Now()).ResetAt
		return true, f.pause(ctx, record, number, t.branch, "usage exhausted", state.PauseComment(f.Now(), record.ResetAt))
	case step.AuthDead:
		// Not the ticket's fault: its work is paused, never failed, and the
		// Daemon parks.
		record.ResumeSession = session
		if err := f.pause(ctx, record, number, t.branch, "credential dead", state.CredentialPauseComment(f.Now())); err != nil {
			return true, err
		}
		return true, f.parkOnDead(ctx, record, fmt.Errorf("the %s Step of Fire %s ended %s: %s", name, record.FireID, run.Outcome, run.Detail))
	}
	if ctx.Err() != nil {
		// Not the ticket's fault: the Daemon was told to stop. A ticket that
		// was paused still is.
		if t.paused {
			record.ResumeSession = session
		}
		return true, fmt.Errorf("the %s Step ended %s: %s", name, run.Outcome, run.Detail)
	}
	if t.paused {
		if err := f.GitHub.RemoveLabel(ctx, f.Repo.Owner, f.Repo.Name, number, state.LabelPaused); err != nil {
			return true, fmt.Errorf("taking %s off #%d: %w", state.LabelPaused, number, err)
		}
		t.paused = false
	}
	if run.Outcome != step.OK {
		return true, f.failedAttempt(ctx, record, number, name, fmt.Sprintf("the %s Step ended %s: %s", name, run.Outcome, run.Detail))
	}
	return false, nil
}
