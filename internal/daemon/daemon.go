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

// MaxFailInterval caps the wait after failed Fires in a row, which doubles
// from IdleInterval: a failure that does not clear in a minute, such as an
// environment that cannot boot, rarely clears in the next, and each retry
// may post to GitHub again.
const MaxFailInterval = 30 * time.Minute

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
	// Status, when set, is told what the Daemon is doing, for the
	// Dashboard.
	Status *Status
	// fails counts the failed Fires in a row, for the back-off.
	fails int
}

// Run runs the Daemon until ctx is done, or Cycles cycles have run. A Fire
// that fails or panics never ends it: the next cycle derives again, and
// whatever the Fire left behind is cleared up from GitHub's state. It
// returns the context's error when it was stopped, nil at the cycle cap.
func (d *Daemon) Run(ctx context.Context) error {
	for cycle := 1; ; cycle++ {
		d.Status.set(State{State: StateFiring, Detail: "firing", At: d.Now()})
		record := d.once(ctx)
		d.Status.ended(record)
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
// gate held it, from IdleInterval doubling to MaxFailInterval when it
// failed, not at all when it worked, and IdleInterval when it was idle. Any
// Fire that did not fail, a held one included, ends the failure streak.
func (d *Daemon) wait(ctx context.Context, record journal.Record) error {
	// A park waits on its own probe, so it starts no failure streak.
	if failed(record) && record.Park == nil {
		d.fails++
	} else {
		d.fails = 0
	}
	switch {
	case record.Park != nil:
		probe := record.Park.ProbeAt.UTC().Format(time.RFC3339)
		d.Status.set(State{State: StateParked, Detail: "parked: Claude's credential is dead, probing it at " + probe, At: d.Now(), Parked: record.Park})
		return d.sleep(ctx, record.Park.ProbeAt.Sub(d.Now()))
	case record.ResetAt != "" || held(record):
		detail := "held by the Budget gate"
		if record.ResetAt != "" {
			detail += " until " + record.ResetAt
		}
		d.Status.set(State{State: StateHeld, Detail: detail, At: d.Now(), ResetAt: record.ResetAt})
		// A hold that names no reset sleeps the plan's degraded interval:
		// never a hot loop of Fires the gate keeps stopping.
		return d.waitForReset(ctx, record.ResetAt)
	case d.fails > 0:
		wait := IdleInterval
		for range d.fails - 1 {
			wait = min(2*wait, MaxFailInterval)
		}
		d.Status.set(State{State: StateIdle, Detail: fmt.Sprintf("%d failed Fire(s) in a row, deriving again in %v", d.fails, wait), At: d.Now()})
		return d.sleep(ctx, wait)
	case record.Action != "" && record.Action != string(state.ActionNone):
		return nil
	}
	d.Status.set(State{State: StateIdle, Detail: fmt.Sprintf("idle, deriving again in %v", IdleInterval), At: d.Now()})
	return d.sleep(ctx, IdleInterval)
}

// failed reports whether the Fire failed: an error, a non-zero exit, or a
// Verification or Deployed tier round whose environment did not boot or
// answer. Such a round posts its comment and ends the Fire without changing
// what GitHub says, so the next Fire would derive the same work at once.
func failed(record journal.Record) bool {
	unbooted := func(v *journal.Verify) bool { return v != nil && v.Infra != "" }
	return record.Error != "" || (record.Exit != nil && *record.Exit != 0) ||
		unbooted(record.Verify) || unbooted(record.Deployed)
}

// held reports whether the Budget gate stopped the Fire before a Step: its
// last Step is one the gate did not let run.
func held(record journal.Record) bool {
	if len(record.Steps) == 0 {
		return false
	}
	last := record.Steps[len(record.Steps)-1]
	return last.Outcome == "" && last.Gate != nil && !last.Gate.ShouldFire
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
