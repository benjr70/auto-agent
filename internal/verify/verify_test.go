package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/verify/providertest"
)

const block = "API_URL=http://127.0.0.1:9/api\nWEB_URL=http://127.0.0.1:9/\n"

var surfaces = map[string]config.Surface{
	"api": {Kind: "api", URLKey: "API_URL"},
	"web": {Kind: "browser", URLKey: "WEB_URL"},
}

type R = providertest.Reply

func drive(t *testing.T, s providertest.Script, smoke bool) (Result, *providertest.Fake) {
	t.Helper()
	fake := providertest.New(t, s)
	p := Provider{Path: fake.Path, Dir: t.TempDir(), Smoke: smoke}
	return p.Run(context.Background(), 5, surfaces, os.Environ()), fake
}

func calls(fake *providertest.Fake) string { return strings.Join(fake.Calls(), "|") }

// Ported from lib/provider-check.test.sh and lib/verify-boot.test.sh. Where
// provider-check gave an exit code, the Fire gets a Result: a contract
// violation and a machine that could not boot are both a smoke that was
// skipped, never a pass, and only the smoke itself fails. A case whose name
// promised an exit code is renamed for what it asserts; its bash name is in
// the comment above it.
func TestRun(t *testing.T) {
	healthy := providertest.Healthy(block)

	// provider-check: "the call order is down, up, smoke, down"
	t.Run("the call order is down --all, up, smoke, down", func(t *testing.T) {
		r, fake := drive(t, healthy, true)
		if r != (Result{Status: Pass, Detail: "2/2"}) || calls(fake) != "down --all|up --pr 5|smoke|down --pr 5" {
			t.Errorf("result %+v, calls %s", r, calls(fake))
		}
	})
	// verify-boot: "down runs before the first up (a killed Fire can leave
	// an environment behind)"
	t.Run("down --all before the first up tears down what a killed Fire left, whatever its PR", func(t *testing.T) {
		healthy := healthy
		healthy.Live = []int{3}
		r, fake := drive(t, healthy, true)
		if r.Status != Pass || len(fake.Live()) != 0 {
			t.Errorf("result %+v, live %v", r, fake.Live())
		}
	})
	t.Run("the up block was exported into smoke's environment", func(t *testing.T) {
		_, fake := drive(t, healthy, true)
		api, _ := fake.SmokeEnv("API_URL")
		web, _ := fake.SmokeEnv("WEB_URL")
		if api != "http://127.0.0.1:9/api" || web != "http://127.0.0.1:9/" {
			t.Errorf("smoke saw API_URL=%q WEB_URL=%q", api, web)
		}
	})
	t.Run("a value may contain =", func(t *testing.T) {
		s := healthy
		s.Up = []R{{Stdout: block + "DSN=postgres://u:p@h/db?sslmode=disable&x=1\n"}}
		_, fake := drive(t, s, true)
		if dsn, _ := fake.SmokeEnv("DSN"); dsn != "postgres://u:p@h/db?sslmode=disable&x=1" {
			t.Errorf("DSN = %q", dsn)
		}
	})
	t.Run("the provider runs in the checkout", func(t *testing.T) {
		fake := providertest.New(t, healthy)
		dir := t.TempDir()
		Provider{Path: fake.Path, Dir: dir, Smoke: true}.Run(context.Background(), 5, surfaces, os.Environ())
		if got, _ := filepath.EvalSymlinks(fake.Dir()); got != mustEval(t, dir) {
			t.Errorf("ran in %s, want %s", fake.Dir(), dir)
		}
	})

	for _, tt := range []struct {
		name      string
		script    providertest.Script
		smoke     bool
		status    Status
		detail    string
		violation bool
		calls     string
	}{
		// provider-check: "a boot failure is retried once, with a down between the attempts"
		{"a boot failure is retried once, with a down between the attempts: down, up, down, up, smoke, down",
			providertest.Script{Up: []R{{Exit: 4}, {Stdout: block}}, Smoke: healthy.Smoke}, true,
			Pass, "2/2", false, "down --all|up --pr 5|down --pr 5|up --pr 5|smoke|down --pr 5"},
		// provider-check: "up exiting 4 twice is exit 4, and up exiting 3 is not retried"
		// was "a boot that fails twice is skipped naming both attempts; exactly two up attempts, and the environment is torn down afterwards"
		{"exit 4 and a verdict naming boot failure on both attempts; exactly two up attempts, and the environment is torn down afterwards",
			providertest.Script{Up: []R{{Exit: 4, Stderr: "up: exiting 4\n"}}}, true,
			Skipped, "up exited 4 (boot failed) on both attempts: up: exiting 4", false, "down --all|up --pr 5|down --pr 5|up --pr 5|down --pr 5"},
		// was "a missing prerequisite is skipped, named, tried once, and torn down after"
		{"a missing prerequisite is exit 4, named, tried once, and torn down after",
			providertest.Script{Up: []R{{Exit: 3, Stderr: "up: exiting 3\n"}}}, true,
			Skipped, "up exited 3 (prerequisite missing): up: exiting 3", false, "down --all|up --pr 5|down --pr 5"},
		// was "an exit code outside the contract is a contract violation, torn down after"
		{"an exit code outside the contract is a contract violation (exit 1), torn down after",
			providertest.Script{Up: []R{{Exit: 7, Stderr: "boom\n"}}}, true,
			Skipped, "up exited 7, want 0 healthy, 3 prerequisite missing or 4 boot failed: boom", true, "down --all|up --pr 5|down --pr 5"},
		// provider-check: "a Surface whose url_key the block omits is named"
		// was "a violation naming the surface and the key; the environment was still torn down, and smoke never ran"
		{"exit 1 naming the surface and the key; the environment was still torn down, and smoke never ran",
			providertest.Script{Up: []R{{Stdout: "WEB_URL=http://127.0.0.1:9/\n"}}}, true,
			Skipped, "surface api declares url_key API_URL, which the up block does not carry", true, "down --all|up --pr 5|down --pr 5"},
		// provider-check: "the block grammar"; verify-boot: "progress written
		// to stdout instead of stderr fails the boot rather than being exported"
		// was "a line that is not KEY=value is named, a violation"
		{"a line that is not KEY=value is named, exit 1",
			providertest.Script{Up: []R{{Stdout: "starting api...\n" + block}}}, true,
			Skipped, "up printed a line that is not KEY=value: starting api...", true, "down --all|up --pr 5|down --pr 5"},
		// was "a key that is not an uppercase shell identifier is named, a violation"
		{"a key that is not an uppercase shell identifier is named, exit 1",
			providertest.Script{Up: []R{{Stdout: "api_url=x\n"}}}, true,
			Skipped, "up printed a key that is not an uppercase shell identifier: api_url", true, "down --all|up --pr 5|down --pr 5"},
		// was "a key with a dash in it is named, a violation"
		{"a key with a dash in it is named, exit 1",
			providertest.Script{Up: []R{{Stdout: "WEB-URL=x\n"}}}, true,
			Skipped, "up printed a key that is not an uppercase shell identifier: WEB-URL", true, "down --all|up --pr 5|down --pr 5"},
		// was "a healthy up that printed nothing is named, a violation"
		{"a healthy up that printed nothing is named, exit 1",
			providertest.Script{Up: []R{{}}}, true,
			Skipped, "up printed no keys", true, "down --all|up --pr 5|down --pr 5"},
		// provider-check: "smoke"
		// was "smoke exit 1 is a FAIL, in the provider's own words"
		{"smoke exit 1 is exit 1, quoting the provider's own line",
			providertest.Script{Up: healthy.Up, Smoke: []R{{Stdout: "smoke: FAIL (1/2 failed)\n", Exit: 1}}}, true,
			Fail, "1/2 failed", false, "down --all|up --pr 5|smoke|down --pr 5"},
		// was "smoke exit 2 (could not run) is skipped, not a contract violation"
		{"smoke exit 2 (could not run) is exit 4, not a contract violation",
			providertest.Script{Up: healthy.Up, Smoke: []R{{Stderr: "no browser\n", Exit: 2}}}, true,
			Skipped, "smoke exited 2 (could not run): no browser", false, "down --all|up --pr 5|smoke|down --pr 5"},
		// was "a PASS line that is not the LAST stdout line is a violation, never a pass"
		{"a PASS line that is not the LAST stdout line is exit 1",
			providertest.Script{Up: healthy.Up, Smoke: []R{{Stdout: "smoke: PASS (2/2)\nall done\n"}}}, true,
			Skipped, "smoke exited 0 but its last stdout line is not 'smoke: PASS (…)': all done", true, "down --all|up --pr 5|smoke|down --pr 5"},
		{"a smoke that fails without its FAIL line still fails, and says the line is wrong",
			providertest.Script{Up: healthy.Up, Smoke: []R{{Stdout: "oops\n", Exit: 1}}}, true,
			Fail, "smoke exited 1 but its last stdout line is not 'smoke: FAIL (…)': oops", true, "down --all|up --pr 5|smoke|down --pr 5"},
		{"a smoke exit code outside the contract is a violation",
			providertest.Script{Up: healthy.Up, Smoke: []R{{Exit: 9}}}, true,
			Skipped, "smoke exited 9, want 0 pass, 1 fail or 2 could not run", true, "down --all|up --pr 5|smoke|down --pr 5"},
		// was "smoke off: it is never called, and the smoke is skipped saying so"
		{"smoke off: it is never called, and the verdict counts 5 checks",
			healthy, false,
			Skipped, "the hermetic tier does not run the smoke (verification.hermetic.smoke is false); the environment booted", false, "down --all|up --pr 5|down --pr 5"},
		// provider-check: "down's own exit code"
		// was "a down before the first up that exits non-zero is a violation, named; up was never attempted"
		{"a down --all before the first up that exits non-zero is exit 1, named; up was never attempted",
			providertest.Script{DownAll: []R{{Exit: 1, Stderr: "down: broken\n"}}}, true,
			Skipped, "down --all before the first up exited 1, want 0 (down --all is idempotent): down: broken", true, "down --all"},
		{"a provider that lacks down --all is a violation naming the verb; up was never attempted",
			providertest.Script{DownAll: []R{{Exit: 2, Stderr: "usage: provider up|down|smoke [--pr N]\n"}}}, true,
			Skipped, "down --all before the first up exited 2, want 0 (down --all is idempotent): usage: provider up|down|smoke [--pr N]", true, "down --all"},
		{"a down between the one retry that exits non-zero is named, and the environment is still torn down",
			providertest.Script{Up: []R{{Exit: 4}}, Down: []R{{Exit: 1, Stderr: "down: broken\n"}, {}}}, true,
			Skipped, "down between the one retry exited 1, want 0 (down is idempotent): down: broken", true, "down --all|up --pr 5|down --pr 5|down --pr 5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, fake := drive(t, tt.script, tt.smoke)
			if r.Status != tt.status || r.Detail != tt.detail || r.Violation != tt.violation || calls(fake) != tt.calls {
				t.Errorf("result %+v\ncalls %s\nwant %s %q violation=%v\ncalls %s", r, calls(fake), tt.status, tt.detail, tt.violation, tt.calls)
			}
		})
	}

	// was "a final down that exits non-zero is a teardown warning, and the smoke's result stands"
	t.Run("a final down that exits non-zero fails an otherwise clean run, exit 1", func(t *testing.T) {
		r, _ := drive(t, providertest.Script{Up: healthy.Up, Smoke: healthy.Smoke, Down: []R{{Exit: 1}}}, true)
		if r.Status != Pass || r.Teardown != "down after up exited 1, want 0 (down is idempotent)" {
			t.Errorf("result %+v", r)
		}
	})
	t.Run("a failed smoke carries the end of its output for the Fix", func(t *testing.T) {
		r, _ := drive(t, providertest.Script{Up: healthy.Up, Smoke: []R{{Stdout: "GET /api 500\nsmoke: FAIL (1/2 failed)\n", Stderr: "trace\n", Exit: 1}}}, true)
		if !strings.Contains(r.Log, "GET /api 500") {
			t.Errorf("log %q", r.Log)
		}
	})
	t.Run("a cancelled Fire still tears the environment down", func(t *testing.T) {
		fake := providertest.New(t, providertest.Script{Up: []R{{Exit: 4}}})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		Provider{Path: fake.Path, Dir: t.TempDir(), Smoke: true}.Run(ctx, 5, surfaces, os.Environ())
		if c := fake.Calls(); len(c) == 0 || c[len(c)-1] != "down --pr 5" {
			t.Errorf("calls %q", c)
		}
	})
}

func mustEval(t *testing.T, dir string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Ported from lib/provider-check.test.sh ("config-shaped outcomes") and
// lib/verify-boot.test.sh, under their case names.
func TestResolve(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "verify"), 0o755)
	os.WriteFile(filepath.Join(dir, "verify", "provider"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o644)
	// was "no hermetic tier is the Bootstrap state, and nothing is driven"
	t.Run("no hermetic tier is exit 3 with a BOOTSTRAP verdict, and nothing driven", func(t *testing.T) {
		if _, err := Resolve(dir, nil); !errors.Is(err, ErrBootstrap) || !strings.Contains(err.Error(), "Bootstrap state") {
			t.Errorf("Resolve = %v", err)
		}
	})
	// was "a hermetic command that is not executable is named"
	t.Run("a hermetic command that is not executable is exit 1, named", func(t *testing.T) {
		for _, command := range []string{"plain", "missing", "verify"} {
			if _, err := Resolve(dir, &config.Hermetic{Command: command}); err == nil || err.Error() != "the hermetic command "+command+" is not an executable file under "+dir {
				t.Errorf("Resolve(%s) = %v", command, err)
			}
		}
	})
	t.Run("the command is resolved from the root of the checkout", func(t *testing.T) {
		p, err := Resolve(dir, &config.Hermetic{Command: "verify/provider", Smoke: true})
		if err != nil || p.Path != filepath.Join(dir, "verify", "provider") || p.Dir != dir || !p.Smoke {
			t.Errorf("Resolve = %+v, %v", p, err)
		}
	})
}

func TestTrailer(t *testing.T) {
	if got := (Result{Status: Pass, Detail: "2/2"}).Trailer(); got != "smoke: PASS — 2/2" {
		t.Errorf("Trailer = %q", got)
	}
}

// Ported from lib/provider-check.test.sh: "the contract doc and the check
// say the same thing". Every verdict the driver can give is in CONTRACT.md.
func TestContractDoc(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "plugin", "providers", "CONTRACT.md"))
	if err != nil {
		t.Fatal(err)
	}
	source, _ := os.ReadFile("verify.go")
	// "every verdict CONTRACT.md documents is still in the check": the
	// verdicts are the first column of its failure table. The smoke's own
	// failure is reported in its own words, not as provider-check's
	// "smoke exited 1: <line>". The check alone drives a down with nothing
	// up and a down --all on the Host its run left clean.
	checkOnly := map[string]bool{"smoke exited 1:": true, "down before the first up exited": true, "down --all after the run exited": true}
	row := regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")
	table := string(doc)[strings.Index(string(doc), "| verdict | cause |"):]
	table = table[:strings.Index(table, "\n\n")]
	documented := row.FindAllStringSubmatch(table, -1)
	if len(documented) < 15 {
		t.Fatalf("found %d verdicts in CONTRACT.md's failure table", len(documented))
	}
	for _, m := range documented {
		head, _, _ := strings.Cut(m[1], "<")
		if head = strings.TrimSpace(head); !checkOnly[head] && !strings.Contains(string(source), head) {
			t.Errorf("CONTRACT.md documents %q, which the driver no longer gives", m[1])
		}
	}
	// "every verdict the check prints is in CONTRACT.md"
	for _, verdict := range []string{
		"the hermetic command %s is not an executable file under %s",
		"down --all before the first up exited %d, want 0 (down --all is idempotent)",
		"down between the one retry exited %d, want 0 (down is idempotent)",
		"down after up exited %d, want 0 (down is idempotent)",
		"up exited 3 (prerequisite missing)",
		"up exited 4 (boot failed) on both attempts",
		"up exited %d, want 0 healthy, 3 prerequisite missing or 4 boot failed",
		"up printed no keys",
		"up printed a line that is not KEY=value",
		"up printed a key that is not an uppercase shell identifier",
		"surface %s declares url_key %s, which the up block does not carry",
		"smoke exited 0 but its last stdout line is not 'smoke: PASS (…)'",
		"smoke exited 1 but its last stdout line is not 'smoke: FAIL (…)'",
		"smoke exited 2 (could not run)",
		"smoke exited %d, want 0 pass, 1 fail or 2 could not run",
	} {
		if !strings.Contains(string(source), verdict) {
			t.Errorf("the driver no longer says %q", verdict)
		}
		documented := strings.NewReplacer("%s", "<", "%d", "<").Replace(verdict)
		head, _, _ := strings.Cut(documented, "<")
		if !strings.Contains(string(doc), strings.TrimSpace(head)) {
			t.Errorf("CONTRACT.md does not document %q", verdict)
		}
	}
}
