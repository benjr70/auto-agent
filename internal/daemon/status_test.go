package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/journal"
)

// watched runs the Daemon with a Status, and reads it each time a Fire runs
// and each time the Daemon sleeps.
func watched(t *testing.T, fires []func() journal.Record, cycles int) (seen []State, tail []string) {
	t.Helper()
	status := &Status{}
	w := &world{t: t, now: clock, fires: fires}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := Daemon{
		Fire: firing(func(ctx context.Context) journal.Record {
			seen = append(seen, status.Now())
			return w.Once(ctx)
		}),
		Gate: w, Cycles: cycles, Status: status, Log: status,
		Now: func() time.Time { return w.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			seen = append(seen, status.Now())
			w.now = w.now.Add(d)
			return ctx.Err()
		},
	}
	if err := d.Run(ctx); err != nil {
		t.Fatal(err)
	}
	return seen, status.Tail()
}

type firing func(context.Context) journal.Record

func (f firing) Once(ctx context.Context) journal.Record { return f(ctx) }

func TestStatusFollowsTheDaemon(t *testing.T) {
	t.Run("firing during a Fire, idle between polls", func(t *testing.T) {
		seen, _ := watched(t, nil, 1)
		if len(seen) != 2 || seen[0].State != StateFiring || seen[1].State != StateIdle || !seen[1].At.Equal(clock) || seen[1].Detail == "" {
			t.Errorf("seen = %+v", seen)
		}
	})
	t.Run("held to the reset while the gate waits", func(t *testing.T) {
		reset := "2026-10-12T10:30:00Z"
		seen, _ := watched(t, []func() journal.Record{func() journal.Record {
			return journal.Record{Kind: "pick", Action: "implement", ResetAt: reset}
		}}, 1)
		held := seen[1]
		if held.State != StateHeld || held.ResetAt != reset || !strings.Contains(held.Detail, reset) {
			t.Errorf("held = %+v", held)
		}
		for _, s := range seen[1:] {
			if s.State != StateHeld {
				t.Errorf("a poll of the gate is still held: %+v", s)
			}
		}
	})
	t.Run("parked with the probe time", func(t *testing.T) {
		probe := clock.Add(time.Hour)
		issue := 77
		seen, _ := watched(t, []func() journal.Record{func() journal.Record {
			return journal.Record{Kind: "parked", Action: "probe", Park: &journal.Park{Issue: &issue, Reason: "401", ProbeAt: probe}}
		}}, 1)
		parked := seen[1]
		if parked.State != StateParked || parked.Parked == nil || !parked.Parked.ProbeAt.Equal(probe) || !strings.Contains(parked.Detail, probe.Format(time.RFC3339)) {
			t.Errorf("parked = %+v", parked)
		}
	})
	t.Run("failed Fires are counted until one does not fail", func(t *testing.T) {
		unbooted := func() journal.Record {
			return journal.Record{Kind: "reconcile", Action: "deps-land", Verify: &journal.Verify{Infra: "up exited 3"}}
		}
		seen, _ := watched(t, []func() journal.Record{failing, unbooted, record("idle", "none")}, 3)
		var fails []int
		for _, s := range seen {
			if s.State == StateIdle {
				fails = append(fails, s.Fails)
			}
		}
		if fmt.Sprint(fails) != "[1 2 0]" {
			t.Errorf("fails at each sleep = %v", fails)
		}
	})
	t.Run("the tail is the last log lines, in memory", func(t *testing.T) {
		_, tail := watched(t, nil, TailLines)
		if len(tail) != TailLines {
			t.Fatalf("tail has %d lines, want %d", len(tail), TailLines)
		}
		if last := tail[len(tail)-1]; !strings.HasPrefix(last, "[daemon ") || !strings.Contains(last, "idle") {
			t.Errorf("last line = %q", last)
		}
		if !strings.Contains(strings.Join(tail, "\n"), `"kind":"idle"`) {
			t.Errorf("the tail lacks the Fire records the Daemon logs:\n%s", strings.Join(tail, "\n"))
		}
	})
	t.Run("a Daemon with no Status runs as before", func(t *testing.T) {
		w := &world{t: t, now: clock}
		if err, _ := w.run(t, 1); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStatusTailKeepsPartialLines(t *testing.T) {
	s := &Status{}
	fmt.Fprint(s, "one\ntw")
	fmt.Fprint(s, "o\n")
	if got := s.Tail(); fmt.Sprint(got) != "[one two]" {
		t.Errorf("tail = %q", got)
	}
}
