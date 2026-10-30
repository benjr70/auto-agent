package daemon

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/journal"
)

var clock = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)

// world is a Daemon's collaborators as fakes: Fires that end as scripted,
// a gate that answers as scripted, and a clock that moves only by the
// sleeps the Daemon asks for.
type world struct {
	t      *testing.T
	fires  []func() journal.Record
	fired  int
	gates  []bool
	gated  int
	now    time.Time
	sleeps []time.Duration
	// events is what happened, in order: "fire", "gate", "sleep <d>".
	events []string
	cancel context.CancelFunc
	// stopAfter cancels the Daemon once that many Fires have run.
	stopAfter int
	// sweep, when set, is the Daemon's Sweep: it notes "sweep" and fails
	// with sweepErr.
	sweep    bool
	sweepErr error
}

func (w *world) Once(ctx context.Context) journal.Record {
	w.events = append(w.events, "fire")
	w.fired++
	defer func() {
		if w.stopAfter != 0 && w.fired == w.stopAfter {
			w.cancel()
		}
	}()
	if len(w.fires) == 0 {
		return journal.Record{Kind: "idle", Action: "none"}
	}
	next := w.fires[0]
	w.fires = w.fires[1:]
	return next()
}

func (w *world) Verdict(context.Context) (budget.Verdict, error) {
	w.events = append(w.events, "gate")
	w.gated++
	if len(w.gates) == 0 {
		return budget.Verdict{ShouldFire: true}, nil
	}
	ok := w.gates[0]
	w.gates = w.gates[1:]
	return budget.Verdict{ShouldFire: ok}, nil
}

func (w *world) run(t *testing.T, cycles int) (error, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.cancel = cancel
	var log bytes.Buffer
	d := Daemon{
		Fire: w, Gate: w, Cycles: cycles, Log: &log,
		Now: func() time.Time { return w.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			w.events = append(w.events, "sleep "+d.String())
			w.sleeps = append(w.sleeps, d)
			w.now = w.now.Add(d)
			return ctx.Err()
		},
	}
	if w.sweep {
		d.Sweep = func() error {
			w.events = append(w.events, "sweep")
			return w.sweepErr
		}
	}
	return d.Run(ctx), log.String()
}

func record(kind, action string) func() journal.Record {
	return func() journal.Record { return journal.Record{Kind: kind, Action: action} }
}

// failing is a Fire that ends in an error.
func failing() journal.Record {
	return journal.Record{Kind: "pick", Action: "implement", Error: "boom"}
}

// The cases of lib/daemon.test.sh that the Daemon keeps, under their names;
// the PR that ported them lists the ones dropped and why.
func TestDaemon(t *testing.T) {
	tests := []struct {
		name   string
		fires  []func() journal.Record
		gates  []bool
		cycles int
		want   []string
	}{
		{
			name:   "an idle cycle sleeps about 60 seconds, then derives again",
			cycles: 2,
			want:   []string{"fire", "sleep 1m0s", "fire", "sleep 1m0s"},
		},
		{
			name:   "fires twice without sleeping",
			fires:  []func() journal.Record{record("pick", "implement"), record("reconcile", "review")},
			cycles: 3,
			want:   []string{"fire", "fire", "fire", "sleep 1m0s"},
		},
		{
			name:   "in flight elsewhere, or a Lane this build does not work, sleeps like idle: no hot loop",
			fires:  []func() journal.Record{record("in-flight", "none"), record("pick-decision", "none")},
			cycles: 2,
			want:   []string{"fire", "sleep 1m0s", "fire", "sleep 1m0s"},
		},
		{
			name: "sleeps exactly to the verdict's reset; two polls, then the next cycle fires",
			fires: []func() journal.Record{
				func() journal.Record {
					return journal.Record{Kind: "pick", Action: "implement", ResetAt: "2026-10-12T10:30:00Z"}
				},
				record("pick", "implement"),
			},
			gates:  []bool{false, false, true},
			cycles: 2,
			want:   []string{"fire", "sleep 1h0m0s", "gate", "sleep 5m0s", "gate", "sleep 5m0s", "gate", "fire"},
		},
		{
			name: "then polls the gate up to the planner's cap",
			fires: []func() journal.Record{
				func() journal.Record {
					return journal.Record{Kind: "pick", Action: "implement", ResetAt: "2026-10-12T09:30:00Z"}
				},
			},
			gates:  []bool{false, false, false, false, false, false, false, false, false, false, false, false},
			cycles: 1,
			want:   append([]string{"fire"}, strings.Split(strings.Repeat("gate,sleep 5m0s,", 12), ",")[:24]...),
		},
		{
			name: "sleeps to the Fire's reset, not the verdict's: a Step the usage limit cut off",
			fires: []func() journal.Record{
				func() journal.Record {
					return journal.Record{Kind: "pick", Action: "implement", Phase: "paused", ResetAt: "2026-10-12T11:30:00Z"}
				},
			},
			cycles: 1,
			want:   []string{"fire", "sleep 2h0m0s", "gate"},
		},
		{
			name: "sleeps SLEEP_DEGRADED_SECS; exits the cycle cleanly after a degraded sleep",
			fires: []func() journal.Record{
				func() journal.Record { return journal.Record{Kind: "pick", Action: "implement", ResetAt: "soon"} },
			},
			cycles: 1,
			want:   []string{"fire", "sleep 5h0m0s", "gate"},
		},
		{
			name: "a hold that names no reset sleeps the degraded interval and re-gates: no hot loop",
			fires: []func() journal.Record{
				func() journal.Record {
					return journal.Record{Kind: "pick", Action: "implement", Phase: "gate", Steps: []journal.Step{
						journal.NotRun("implement", clock, budget.Verdict{ShouldFire: false}),
					}}
				},
				record("pick", "implement"),
			},
			cycles: 2,
			want:   []string{"fire", "sleep 5h0m0s", "gate", "fire"},
		},
		{
			name: "a parked Daemon only sleeps to its probe and re-probes, never gates or fires",
			fires: []func() journal.Record{
				func() journal.Record {
					return journal.Record{Kind: "pick", Error: "auth dead", Park: &journal.Park{ProbeAt: clock.Add(time.Hour)}}
				},
				func() journal.Record {
					return journal.Record{Kind: "parked", Park: &journal.Park{ProbeAt: clock.Add(2 * time.Hour)}}
				},
				record("parked", "implement"),
			},
			cycles: 3,
			want:   []string{"fire", "sleep 1h0m0s", "fire", "sleep 1h0m0s", "fire"},
		},
		{
			name: "one Fire per cycle, no hot loop: a failed Fire sleeps like idle and derives again",
			fires: []func() journal.Record{
				failing,
				record("pick", "implement"),
			},
			cycles: 2,
			want:   []string{"fire", "sleep 1m0s", "fire"},
		},
		{
			name: "a round whose environment did not boot is a failed Fire, not work: no Fires back to back",
			fires: []func() journal.Record{
				func() journal.Record {
					return journal.Record{Kind: "reconcile", Action: "deps-land", Verify: &journal.Verify{Infra: "up exited 3"}}
				},
				record("reconcile", "deps-land"),
			},
			cycles: 2,
			want:   []string{"fire", "sleep 1m0s", "fire"},
		},
		{
			name: "failed Fires in a row back off, doubling to the cap; one that works starts over",
			fires: append(slices.Repeat([]func() journal.Record{
				failing,
			}, 7),
				record("pick", "implement"),
				failing,
			),
			cycles: 9,
			want: []string{
				"fire", "sleep 1m0s", "fire", "sleep 2m0s", "fire", "sleep 4m0s", "fire", "sleep 8m0s",
				"fire", "sleep 16m0s", "fire", "sleep 30m0s", "fire", "sleep 30m0s",
				"fire", "fire", "sleep 1m0s",
			},
		},
		{
			name: "a Deployed tier round whose environment did not answer is a failed Fire too",
			fires: []func() journal.Record{
				func() journal.Record {
					return journal.Record{Kind: "deployed", Action: "deployed", Deployed: &journal.Verify{Infra: "status exited 1"}}
				},
				record("deployed", "deployed"),
			},
			cycles: 2,
			want:   []string{"fire", "sleep 1m0s", "fire"},
		},
		{
			name: "a park between failed Fires starts the back-off over",
			fires: []func() journal.Record{
				failing,
				func() journal.Record {
					return journal.Record{Kind: "pick", Error: "auth dead", Park: &journal.Park{ProbeAt: clock.Add(2 * time.Minute)}}
				},
				failing,
			},
			cycles: 3,
			want:   []string{"fire", "sleep 1m0s", "fire", "sleep 1m0s", "fire", "sleep 1m0s"},
		},
		{
			name: "an idle Fire after failed ones starts the back-off over",
			fires: []func() journal.Record{
				failing,
				failing,
				record("idle", "none"),
				failing,
			},
			cycles: 4,
			want:   []string{"fire", "sleep 1m0s", "fire", "sleep 2m0s", "fire", "sleep 1m0s", "fire", "sleep 1m0s"},
		},
		{
			name: "a Fire that panics does not end the Daemon, and the next cycle continues",
			fires: []func() journal.Record{
				func() journal.Record { panic("the Fire fell over") },
				record("pick", "implement"),
			},
			cycles: 2,
			want:   []string{"fire", "sleep 1m0s", "fire"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &world{t: t, fires: tt.fires, gates: tt.gates, now: clock}
			err, _ := w.run(t, tt.cycles)
			if err != nil {
				t.Fatalf("Run = %v", err)
			}
			if !reflect.DeepEqual(w.events, tt.want) {
				t.Errorf("events =\n  %v\nwant\n  %v", w.events, tt.want)
			}
		})
	}
}

func TestDaemonLog(t *testing.T) {
	t.Run("the Daemon logs each Fire's record, one line of JSON, and a panic names itself", func(t *testing.T) {
		w := &world{t: t, now: clock, fires: []func() journal.Record{func() journal.Record { panic("the Fire fell over") }, record("idle", "none")}}
		_, log := w.run(t, 2)
		lines := strings.Split(strings.TrimSpace(log), "\n")
		if len(lines) != 2 || !strings.Contains(lines[0], `"error":"the Fire panicked: the Fire fell over"`) || !strings.Contains(lines[1], `"kind":"idle"`) {
			t.Errorf("log =\n%s", log)
		}
	})
}

func TestDaemonStop(t *testing.T) {
	t.Run("a stop during a Fire ends the Daemon once the Fire returns, with no sleep after it", func(t *testing.T) {
		w := &world{t: t, now: clock, stopAfter: 1}
		if err, _ := w.run(t, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
		if !reflect.DeepEqual(w.events, []string{"fire"}) {
			t.Errorf("events = %v", w.events)
		}
	})
	t.Run("a stop during a sleep ends the Daemon before another Fire", func(t *testing.T) {
		w := &world{t: t, now: clock}
		ctx, cancel := context.WithCancel(context.Background())
		d := Daemon{Fire: w, Gate: w, Log: &bytes.Buffer{}, Now: func() time.Time { return w.now },
			Sleep: func(ctx context.Context, d time.Duration) error {
				w.events = append(w.events, "sleep")
				cancel()
				return ctx.Err()
			}}
		if err := d.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
		if !reflect.DeepEqual(w.events, []string{"fire", "sleep"}) {
			t.Errorf("events = %v", w.events)
		}
	})
}

func TestSweepAtStart(t *testing.T) {
	t.Run("the Daemon sweeps once, before its first Fire", func(t *testing.T) {
		w := &world{t: t, now: clock, sweep: true, fires: []func() journal.Record{record("pick", "implement"), record("pick", "implement")}}
		if err, _ := w.run(t, 2); err != nil {
			t.Fatal(err)
		}
		if want := []string{"sweep", "fire", "fire"}; !reflect.DeepEqual(w.events, want) {
			t.Errorf("events = %v, want %v", w.events, want)
		}
	})
	t.Run("a Sweep that fails is logged, and the Daemon fires on", func(t *testing.T) {
		w := &world{t: t, now: clock, sweep: true, sweepErr: errors.New("disk on fire"), fires: []func() journal.Record{record("pick", "implement")}}
		err, log := w.run(t, 1)
		if err != nil || w.fired != 1 {
			t.Fatalf("err %v, %d Fires", err, w.fired)
		}
		if !strings.Contains(log, "the Sweep at start: disk on fire") {
			t.Errorf("log = %q", log)
		}
	})
}
