// Package daemon is the always-on Daemon: it runs one Fire after another
// against one Target Project, and sleeps between them for as long as the
// last Fire says it should. Each Fire derives the state from GitHub afresh,
// so the Daemon keeps nothing between cycles but the clock.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
)

// IdleInterval is how long the Daemon waits before it derives again when
// the last Fire had nothing to do. Its repeat reads are conditional, so a
// quiet repo costs GitHub mostly 304s.
const IdleInterval = 60 * time.Second

// Fire is one pass over the Target Project (fire.Fire).
type Fire interface {
	Once(ctx context.Context) journal.Record
}

// Gate is the Budget gate, asked again after a sleep to the reset.
type Gate interface {
	Verdict(ctx context.Context) (budget.Verdict, error)
}

// Daemon runs Fires until it is stopped.
type Daemon struct {
	Fire Fire
	Gate Gate
	Now  func() time.Time
	// Sleep waits, or returns the context's error early when it is done.
	Sleep func(context.Context, time.Duration) error
	// Cycles stops the Daemon after that many cycles, each a Fire and the
	// wait after it; 0 runs it until it is
	// stopped.
	Cycles int
	// Log gets every Fire's record, one line of JSON each.
	Log io.Writer
}

// Run runs the Daemon until ctx is done, or Cycles cycles have run. A Fire
// that fails or panics never ends it: the next cycle derives again, and
// whatever the Fire left behind is cleared up from GitHub's state. It
// returns the context's error when it was stopped, nil at the cycle cap.
func (d *Daemon) Run(ctx context.Context) error {
	for cycle := 1; ; cycle++ {
		record := d.once(ctx)
		if line, err := json.Marshal(record); err == nil {
			fmt.Fprintln(d.Log, string(line))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.wait(ctx, record); err != nil {
			return err
		}
		if d.Cycles != 0 && cycle == d.Cycles {
			return nil
		}
	}
}

// once runs one Fire. A panic is its record's error.
func (d *Daemon) once(ctx context.Context) (record journal.Record) {
	defer func() {
		if p := recover(); p != nil {
			record = journal.Record{Error: fmt.Sprintf("the Fire panicked: %v", p)}
		}
	}()
	return d.Fire.Once(ctx)
}

// wait sleeps as long as the last Fire's record says: to the probe when it
// left the Daemon Parked, to the reset on the sleep plan when the Budget
// gate held it, not at all when it worked, and IdleInterval otherwise.
func (d *Daemon) wait(ctx context.Context, record journal.Record) error {
	switch {
	case record.Park != nil:
		return d.sleep(ctx, record.Park.ProbeAt.Sub(d.Now()))
	case record.ResetAt != "":
		return d.waitForReset(ctx, record.ResetAt)
	case record.Error == "" && record.Action != "" && record.Action != string(state.ActionNone):
		return nil
	}
	return d.sleep(ctx, IdleInterval)
}

// waitForReset sleeps to the reset, then asks the gate again at the plan's
// interval until it frees or the plan's polls run out. The next Fire gates
// again either way, before any Step.
func (d *Daemon) waitForReset(ctx context.Context, resetAt string) error {
	plan := budget.PlanSleep(resetAt, d.Now())
	if err := d.sleep(ctx, time.Duration(plan.SleepSecs)*time.Second); err != nil {
		return err
	}
	for range plan.PollMaxAttempts {
		// A refusal is the next Fire's to record: it parks on a dead
		// credential.
		if v, err := d.Gate.Verdict(ctx); err != nil || v.ShouldFire {
			return nil
		}
		if err := d.sleep(ctx, time.Duration(plan.PollIntervalSecs)*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) sleep(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return ctx.Err()
	}
	return d.Sleep(ctx, wait)
}
