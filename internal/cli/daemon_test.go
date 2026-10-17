package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
)

// daemon runs the always-on Daemon for that many cycles, every harness
// label already in the Target Project unless the test says otherwise.
func (f *fireFixture) daemon(cycles int) int {
	f.t.Helper()
	if !f.queued {
		f.gh.HandleGraphQL("issues(first: 100", f.queue...)
		f.queued = true
	}
	f.cycles = cycles
	f.stdout.Reset()
	f.stderr.Reset()
	return f.run("run", "--repo", "acme/widgets")
}

// allLabels makes the fake GitHub hold every harness label.
func (f *fireFixture) allLabels() {
	list := []map[string]string{}
	for _, l := range state.HarnessLabels {
		list = append(list, map[string]string{"name": l.Name})
	}
	f.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, list))
}

// logged are the Fire records the Daemon logged, one line each.
func (f *fireFixture) logged() []map[string]any {
	f.t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.stdout.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			f.t.Fatalf("a logged line is not a Fire record: %q (stderr %s)", line, f.stderr.String())
		}
		records = append(records, record)
	}
	return records
}

// panicOnce is a Step runner whose first Step panics.
type panicOnce struct {
	step.Runner
	panicked bool
}

func (r *panicOnce) Run(ctx context.Context, spec step.Spec, call step.Call) step.Run {
	if !r.panicked {
		r.panicked = true
		panic("the Step fell over")
	}
	return r.Runner.Run(ctx, spec, call)
}

// The always-on Daemon through the whole-Fire seam: real Fires against the
// fake GitHub, a frozen clock and a cycle cap, so nothing really sleeps. The
// loop's own cases, under the names of lib/daemon.test.sh, are in
// internal/daemon.
func TestDaemon(t *testing.T) {
	t.Run("two idle cycles: the second one's reads are conditional, and nothing is written", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		f.queue = []githubtest.Reply{queueReply()}
		f.gh.Handle("GET", "/repos/acme/widgets/issues", githubtest.Text(200, `[]`).WithETag(`"issues"`))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls", githubtest.Text(200, `[]`).WithETag(`"pulls"`), githubtest.Reply{Status: 304})
		var firstCycle int
		f.onSleep = func(time.Duration) {
			if firstCycle == 0 {
				firstCycle = len(f.gh.Requests())
			}
		}
		if code := f.daemon(2); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if len(f.sleeps) != 2 || f.sleeps[0] != time.Minute || f.sleeps[1] != time.Minute {
			t.Errorf("sleeps = %v, want a minute after each idle cycle", f.sleeps)
		}
		if writes := f.gh.Writes(); len(writes) != 0 {
			t.Errorf("idle cycles wrote %+v", writes)
		}
		records := f.logged()
		if len(records) != 2 || records[0]["kind"] != "idle" || records[1]["kind"] != "idle" || records[1]["action"] != "none" {
			t.Errorf("records = %v", records)
		}
		gets := 0
		for _, r := range f.gh.Requests()[firstCycle:] {
			if r.Method != "GET" {
				continue
			}
			gets++
			if r.Header.Get("If-None-Match") == "" {
				t.Errorf("the second cycle's GET %s was not conditional", r.Path)
			}
		}
		if gets == 0 {
			t.Error("the second cycle read nothing")
		}
	})

	t.Run("a Fire that panics, then a cycle that picks work", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		f.answer(finished)
		f.fixture.runner = &panicOnce{Runner: f.runner}
		if code := f.daemon(2); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		records := f.logged()
		if len(records) != 2 || records[0]["error"] != "the Fire panicked: the Step fell over" || records[1]["error"] != "" || records[1]["pr"] != 55.0 {
			t.Fatalf("records = %v", records)
		}
		if len(f.sleeps) == 0 || f.sleeps[0] != time.Minute {
			t.Errorf("sleeps = %v, want the idle minute after the panic", f.sleeps)
		}
		// The panicking Fire released its lock as it unwound, and the next
		// one picked the ticket afresh.
		if want := "POST " + lockPath + "\nDELETE " + unlockPath + "\nPOST " + lockPath; !strings.HasPrefix(f.writes(), want) || f.labelled() != "AFK:done" {
			t.Errorf("GitHub received\n%s\nlabels %q", f.writes(), f.labelled())
		}
		if disk := f.records(); len(disk) != 2 || disk[0]["error"] != "the Fire panicked: the Step fell over" || disk[0]["exit"] != 1.0 {
			t.Errorf("records on disk = %v", disk)
		}
	})

	t.Run("a wait verdict, then the reset, then a Fire", func(t *testing.T) {
		const reset = "2026-10-12T14:00:00+00:00"
		usage := func(session int) string {
			return fmt.Sprintf(`{"five_hour":{"utilization":%d,"resets_at":%q},"seven_day":{"utilization":10,"resets_at":"2026-10-16T00:00:00+00:00"}}`, session, reset)
		}
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		f.loginHost(usage(80))
		f.answer(finished)
		var writesAsleep int
		f.onSleep = func(d time.Duration) {
			if len(f.sleeps) == 1 {
				writesAsleep = len(f.gh.Writes())
				f.usage = usage(5) // the window reset while the Daemon slept
			}
		}
		if code := f.daemon(2); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if len(f.sleeps) == 0 || f.sleeps[0] != 4*time.Hour+30*time.Minute {
			t.Errorf("sleeps = %v, want exactly to the reset first", f.sleeps)
		}
		records := f.logged()
		if len(records) != 2 || records[0]["resetAt"] != reset || records[0]["phase"] != "gate" || records[1]["pr"] != 55.0 {
			t.Fatalf("records = %v", records)
		}
		if writesAsleep != 0 || len(f.claude.Calls()) != 1 {
			t.Errorf("%d writes before the sleep; %d Implement calls", writesAsleep, len(f.claude.Calls()))
		}
	})

	t.Run("parked, then a credential that comes alive, then un-parked", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		f.environ["AUTO_AGENT_PARK_REPROBE_SECS"] = "900"
		f.authErr = errors.New("exit status 1")
		f.answer(finished)
		f.onSleep = func(time.Duration) {
			if len(f.sleeps) == 2 {
				f.authErr = nil // a human logged in again
			}
		}
		if code := f.daemon(3); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if len(f.sleeps) < 2 || f.sleeps[0] != 15*time.Minute || f.sleeps[1] != 15*time.Minute {
			t.Errorf("sleeps = %v, want the reprobe interval while Parked", f.sleeps)
		}
		records := f.logged()
		if len(records) != 3 || records[0]["park"] == nil || records[1]["park"] == nil || records[2]["park"] != nil || records[2]["pr"] != 55.0 {
			t.Fatalf("records = %v", records)
		}
		if len(f.parks) != 1 || f.parks[0].State != "closed" || len(f.claude.Calls()) != 1 {
			t.Errorf("parks %+v; %d Implement calls", f.parks, len(f.claude.Calls()))
		}
		// One probe per Parked Fire, then the gate before each Step.
		if f.probes < 3 {
			t.Errorf("claude auth status was asked %d times", f.probes)
		}
	})

	t.Run("SIGTERM ends the Daemon between Steps with the lock released and the Fire recorded", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		f.fixture.runner = interruptedRunner{}
		if code := f.daemon(0); code != 0 {
			t.Fatalf("exit %d, want 0: a stopped Daemon did not fail (stderr %q)", code, f.stderr.String())
		}
		if want := "POST " + lockPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		if records := f.records(); len(records) != 1 || records[0]["endedAt"] == nil || len(f.sleeps) != 0 {
			t.Errorf("records %v; sleeps %v", records, f.sleeps)
		}
	})

	t.Run("missing harness labels are created at start, and existing ones are left alone", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.queue = []githubtest.Reply{queueReply()}
		f.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, []map[string]string{{"name": "AFK"}, {"name": "bug"}}))
		f.gh.Handle("POST", "/repos/acme/widgets/labels", githubtest.JSON(201, map[string]string{}))
		if code := f.daemon(1); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		created := map[string]bool{}
		for _, w := range f.gh.Writes() {
			var label struct{ Name string }
			json.Unmarshal([]byte(w.Body), &label)
			if w.Path != "/repos/acme/widgets/labels" {
				t.Errorf("unexpected write %s %s", w.Method, w.Path)
			}
			created[label.Name] = true
		}
		if len(created) != len(state.HarnessLabels)-1 || created["AFK"] || !created["AFK:in-progress"] {
			t.Errorf("created %v", created)
		}
	})

	t.Run("only a failure to start ends the process: labels that cannot be read", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.Text(404, `{"message":"Not Found"}`))
		if code := f.daemon(1); code != 1 || !strings.Contains(f.stderr.String(), "reading the labels of acme/widgets") || len(f.records()) != 0 {
			t.Errorf("exit %d, stderr %q", code, f.stderr.String())
		}
	})
}
