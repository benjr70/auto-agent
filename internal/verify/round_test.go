package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/verify/providertest"
)

// head writes a PR head checkout whose Harness config declares the hermetic
// tier (a JSON object, empty for none) and the web Surface, and returns its
// dir.
func head(t *testing.T, hermetic string) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".auto-agent"), 0o755)
	verification := `{}`
	if hermetic != "" {
		verification = `{"hermetic": ` + hermetic + `}`
	}
	cfg := `{"commit_scopes": ["app"], "commands": {"install": "true", "test": "true"}, "pick": {"labels": {}},
	  "verification": ` + verification + `,
	  "surfaces": {"web": {"kind": "browser", "url_key": "WEB_URL", "paths": ["web/**"]}}}`
	if err := os.WriteFile(filepath.Join(dir, ".auto-agent", "harness.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// withFake puts a fake provider in the head checkout at verify/provider.
func withFake(t *testing.T, s providertest.Script) (string, *providertest.Fake) {
	t.Helper()
	dir := head(t, `{"command": "verify/provider", "smoke": true}`)
	fake := providertest.New(t, s)
	os.MkdirAll(filepath.Join(dir, "verify"), 0o755)
	// The fake records beside itself, so the checkout's provider execs it.
	if err := os.WriteFile(filepath.Join(dir, "verify", "provider"), []byte("#!/bin/sh\nexec "+fake.Path+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, fake
}

func boot(t *testing.T, dir string) (*Round, error) {
	t.Helper()
	return BootRound(context.Background(), dir, fixtureRepo, 5, os.Environ())
}

// Ported from lib/verify-boot.test.sh (the environment half), under its case
// names. Where verify-boot gave an exit code, BootRound gives an error: 4 is
// an *InfraError, 2 a *UsageError, 3 ErrBootstrap. Dropped, as #150's (the
// app launches): "up: a failed launch leaves the environment up", "up: a
// browser Surface is not launched here", "up: the sandbox verdict is in the
// block", and the apps half of "down: apps then environment, twice" (its
// environment half is ported under that name). Dropped: "usage: --pr is
// required" and "usage: unknown subcommand" (no CLI to misuse); "without
// --head: the inherited config still wins (Bootstrap state)" (BootRound has
// no inherited config: it only ever reads the head's, so there is no
// fallback to test); and "round: tour Surface, viewport, named shot, ticked
// box and the tour in the body" (the evidence, checklist and tour-injection
// libs it chains are not ported yet).
func TestBootRound(t *testing.T) {
	webBlock := "WEB_URL=http://web.test\n"

	t.Run("up: the block is the contract", func(t *testing.T) {
		dir, _ := withFake(t, providertest.Script{Up: []R{{Stdout: webBlock}}})
		r, err := boot(t, dir)
		if err != nil || !slices.Equal(r.Block, [][2]string{{"WEB_URL", "http://web.test"}}) {
			t.Fatalf("BootRound = %+v, %v", r, err)
		}
		if _, ok := r.Harness.Surfaces["web"]; !ok {
			t.Error("the round does not carry the head's Surfaces")
		}
	})
	t.Run("up: down first", func(t *testing.T) {
		dir, fake := withFake(t, providertest.Script{Up: []R{{Stdout: webBlock}}})
		boot(t, dir)
		if calls(fake) != "down --pr 5|up --pr 5" {
			t.Errorf("calls %s", calls(fake))
		}
	})
	t.Run("up: one retry, down between", func(t *testing.T) {
		dir, fake := withFake(t, providertest.Script{Up: []R{{Exit: 4}, {Stdout: webBlock}}})
		if _, err := boot(t, dir); err != nil || calls(fake) != "down --pr 5|up --pr 5|down --pr 5|up --pr 5" {
			t.Errorf("err %v, calls %s", err, calls(fake))
		}
	})

	// Behaviour 2: a failed boot is retried once, then is an infrastructure
	// error, torn down. Every case here is an *InfraError, never a verdict,
	// and leaves the environment down.
	for _, tt := range []struct {
		name, detail, calls string
		script              providertest.Script
	}{
		{"up: boot failed twice", "up exited 4 (boot failed) on both attempts: up: exiting 4",
			"down --pr 5|up --pr 5|down --pr 5|up --pr 5|down --pr 5",
			providertest.Script{Up: []R{{Exit: 4, Stderr: "up: exiting 4\n"}}}},
		{"up: exit 3 is not retried", "up exited 3 (prerequisite missing): up: exiting 3",
			"down --pr 5|up --pr 5|down --pr 5",
			providertest.Script{Up: []R{{Exit: 3, Stderr: "up: exiting 3\n"}}}},
		{"up: the block grammar", "up printed a line that is not KEY=value: booting the thing...",
			"down --pr 5|up --pr 5|down --pr 5",
			providertest.Script{Up: []R{{Stdout: "booting the thing...\n" + webBlock}}}},
		{"up: a missing url_key", "surface web declares url_key WEB_URL, which the up block does not carry",
			"down --pr 5|up --pr 5|down --pr 5",
			providertest.Script{Up: []R{{Stdout: "OTHER=x\n"}}}},
		{"up: a down before the first up that fails", "down before the first up exited 1, want 0 (down is idempotent): broken",
			"down --pr 5",
			providertest.Script{Down: []R{{Exit: 1, Stderr: "broken\n"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, fake := withFake(t, tt.script)
			r, err := boot(t, dir)
			var infra *InfraError
			if r != nil || !errors.As(err, &infra) || infra.Detail != tt.detail || calls(fake) != tt.calls {
				t.Errorf("BootRound = %+v, %v\ncalls %s\nwant %q\ncalls %s", r, err, calls(fake), tt.detail, tt.calls)
			}
		})
	}

	t.Run("up: the reason is on stderr", func(t *testing.T) {
		// Go has no stderr to write: the reason is the error the caller reports.
		dir, _ := withFake(t, providertest.Script{Up: []R{{Exit: 4}}})
		if _, err := boot(t, dir); err == nil || !strings.Contains(err.Error(), "boot failed") {
			t.Errorf("BootRound = %v", err)
		}
	})

	// Behaviour 3: no hermetic tier is the Bootstrap state.
	t.Run("up: Bootstrap state", func(t *testing.T) {
		r, err := boot(t, head(t, ""))
		if r != nil || !errors.Is(err, ErrBootstrap) {
			t.Errorf("BootRound = %+v, %v", r, err)
		}
	})
	t.Run("up: no provider", func(t *testing.T) {
		r, err := boot(t, head(t, `{"command": "nope", "smoke": false}`))
		var usage *UsageError
		if r != nil || !errors.As(err, &usage) || !strings.Contains(err.Error(), "not an executable file") {
			t.Errorf("BootRound = %+v, %v", r, err)
		}
	})
	t.Run("up: a head with no Harness config is a usage error", func(t *testing.T) {
		var usage *UsageError
		if _, err := boot(t, t.TempDir()); !errors.As(err, &usage) {
			t.Errorf("BootRound = %v", err)
		}
	})

	t.Run("down: apps then environment, twice", func(t *testing.T) {
		dir, fake := withFake(t, providertest.Script{Up: []R{{Stdout: webBlock}}})
		r, err := boot(t, dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Teardown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := r.Teardown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if calls(fake) != "down --pr 5|up --pr 5|down --pr 5|down --pr 5" {
			t.Errorf("calls %s", calls(fake))
		}
	})
	t.Run("down: a failed teardown is named", func(t *testing.T) {
		dir, _ := withFake(t, providertest.Script{Up: []R{{Stdout: webBlock}}, Down: []R{{}, {Exit: 1}}})
		r, _ := boot(t, dir)
		if err := r.Teardown(context.Background()); err == nil || err.Error() != "down after up exited 1, want 0 (down is idempotent)" {
			t.Errorf("Teardown = %v", err)
		}
	})
	t.Run("down: Bootstrap state", func(t *testing.T) {
		r, _ := boot(t, head(t, ""))
		if err := r.Teardown(context.Background()); err != nil {
			t.Errorf("Teardown of nothing = %v", err)
		}
	})
	t.Run("down: a cancelled Fire still tears the environment down", func(t *testing.T) {
		dir, fake := withFake(t, providertest.Script{Up: []R{{Stdout: webBlock}}})
		r, _ := boot(t, dir)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r.Teardown(ctx)
		if c := fake.Calls(); c[len(c)-1] != "down --pr 5" || len(c) != 3 {
			t.Errorf("calls %q", c)
		}
	})
}

// The fixture Target Project's real provider, booted once from its own
// checkout; the bash cases run as subtests in order, teardown last.
func TestBootRoundFixture(t *testing.T) {
	for _, bin := range []string{"python3", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	env := append(os.Environ(), "FIXTURE_RUN_DIR="+t.TempDir())
	r, err := BootRound(context.Background(), filepath.Join("..", "..", "plugin", "fixtures", "target-project"), fixtureRepo, 91, env)
	if err != nil {
		t.Fatalf("--head: the checkout's provider is booted: %v", err)
	}
	defer r.Teardown(context.Background())
	var url string
	for _, kv := range r.Block {
		if kv[0] == "FIXTURE_WEB_URL" {
			url = kv[1]
		}
	}
	health := func() error { return exec.Command("curl", "-fsS", url+"/api/health").Run() }
	t.Run("--head: the checkout's provider is booted", func(t *testing.T) {
		if len(r.Block) == 0 {
			t.Errorf("no block from the checkout's provider")
		}
	})
	t.Run("fixture: the browser Surface is reachable at the key the block carried", func(t *testing.T) {
		if url == "" || health() != nil {
			t.Errorf("block %q", r.Block)
		}
	})
	t.Run("fixture: torn down", func(t *testing.T) {
		if err := r.Teardown(context.Background()); err != nil || health() == nil {
			t.Errorf("teardown: %v", err)
		}
	})
}
