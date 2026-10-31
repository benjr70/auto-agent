package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/step/verifier"
	"github.com/benjr70/auto-agent/internal/verify/providertest"
)

// The Disk guard: before a round boots its environment, the State dir's
// filesystem must have the free space verification.hermetic.disk_free
// declares; a Host short of it, after cleaning up, waits rather than burn
// a round.
func TestDiskGuard(t *testing.T) {
	t.Run("short, then freed: no up, no comment, no round, a 5 minute wait, then the round boots (behaviour 1)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		f.reviewedPR(sayHello)
		f.verifier.Script(verdicts(nil, passes(sayHello)))
		free := uint64(1 << 30)
		var asked []string
		f.free = func(path string) (uint64, error) {
			asked = append(asked, path)
			return free, nil
		}
		var atWait struct {
			comments int
			calls    []string
			writes   string
		}
		f.onSleep = func(d time.Duration) {
			if d == 5*time.Minute && free < 10<<30 {
				atWait.comments, atWait.calls, atWait.writes = len(f.pr.comments), f.provider.Calls(), f.writes()
				free = 20 << 30
			}
		}
		if code := f.daemon(2); code != 0 {
			t.Fatalf("exit %d (stderr %s)", code, f.stderr.String())
		}
		if !slices.Contains(f.sleeps, 5*time.Minute) {
			t.Fatalf("the Daemon never waited 5 minutes for disk: sleeps %v", f.sleeps)
		}
		// While short: the Sweep's cleanup ran, nothing booted, and nothing
		// was said on GitHub.
		if slices.Contains(atWait.calls, "up --pr 55") || !slices.Contains(atWait.calls, "down --all") {
			t.Errorf("the provider was called %v while the disk was short", atWait.calls)
		}
		if atWait.comments != 1 || strings.Contains(atWait.writes, "/comments") {
			t.Errorf("%d comments on the PR, GitHub received\n%s", atWait.comments, atWait.writes)
		}
		if !slices.Contains(asked, f.state) {
			t.Errorf("the free space was read of %v, want the State dir %s", asked, f.state)
		}
		records := f.logged()
		waited := records[0]
		disk, _ := waited["disk"].(map[string]any)
		if disk["free"] != float64(1<<30) || disk["need"] != float64(10<<30) || waited["phase"] != "disk" || waited["exit"] != 0.0 || waited["error"] != "" || waited["verify"] != nil {
			t.Errorf("the waiting Fire's record is %v", waited)
		}
		// Freed: the next Fire boots, and its round is the PR's first.
		if calls := f.provider.Calls(); !slices.Contains(calls, "up --pr 55") {
			t.Errorf("the provider was called %v once the disk was freed", calls)
		}
		round := f.pr.comments[len(f.pr.comments)-1]
		if !strings.HasPrefix(round, "### Manual verification — round 1/3\n") || f.labelled() != "AFK:done" || records[len(records)-1]["disk"] != nil {
			t.Errorf("the round's comment is\n%s\nlabels %q", round, f.labelled())
		}
	})

	t.Run("short with a Deployed round pending: the Deployed round boots nothing and runs (behaviour 2)", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		f.free = func(string) (uint64, error) { return 1 << 20, nil }
		f.verifier.Script(verdicts(nil, verifier.Verdict{Text: liveItem, Verdict: "PASS", Class: verifier.DeployedEnv, Evidence: []string{"200"}}))
		f.mustFire(0)
		record := f.record()
		if d := deployedOf(record); d["done"] != true || record["disk"] != nil || len(w.rounds()) != 1 {
			t.Errorf("record %v, rounds %q", record, w.rounds())
		}
	})

	t.Run("a disk that fills mid-round is an infrastructure error whose comment says the declared need may be too small (behaviour 3)", func(t *testing.T) {
		t.Run("the provider says no space was left", func(t *testing.T) {
			f := newFireFixture(t, labelsConfig)
			f.reviewedPR(sayHello)
			full := providertest.Reply{Exit: 1, Stderr: "write /var/lib/docker/overlay2/x: no space left on device"}
			f.provider.Script(providertest.Script{Up: []providertest.Reply{full, full}})
			f.mustFire(0)
			round := f.pr.comments[len(f.pr.comments)-1]
			if !strings.Contains(round, "infrastructure error") || !strings.Contains(round, "verification.hermetic.disk_free declares 10GB") || !strings.Contains(round, "may be too small") {
				t.Errorf("the round's comment is\n%s", round)
			}
		})
		t.Run("the disk is short of the need once the boot failed", func(t *testing.T) {
			f := newFireFixture(t, labelsConfig)
			f.reviewedPR(sayHello)
			f.provider.Script(providertest.Script{Up: []providertest.Reply{{Exit: 1, Stderr: "the db did not start"}, {Exit: 1, Stderr: "the db did not start"}}})
			// Room to spare until the environment started to boot.
			f.free = func(string) (uint64, error) {
				if slices.Contains(f.provider.Calls(), "up --pr 55") {
					return 2 << 30, nil
				}
				return 20 << 30, nil
			}
			f.mustFire(0)
			round := f.pr.comments[len(f.pr.comments)-1]
			if !strings.Contains(round, "the db did not start") || !strings.Contains(round, "2.0 GB free") || !strings.Contains(round, "may be too small") {
				t.Errorf("the round's comment is\n%s", round)
			}
		})
		t.Run("an infrastructure error with room to spare says nothing of the disk", func(t *testing.T) {
			f := newFireFixture(t, labelsConfig)
			f.reviewedPR(sayHello)
			f.provider.Script(providertest.Script{Up: []providertest.Reply{{Exit: 1, Stderr: "the db did not start"}, {Exit: 1, Stderr: "the db did not start"}}})
			f.mustFire(0)
			if round := f.pr.comments[len(f.pr.comments)-1]; strings.Contains(round, "disk_free") {
				t.Errorf("the round's comment is\n%s", round)
			}
		})
	})
}
