package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/verify/providertest"
)

// Ported from lib/deployed-tier.test.sh ("status is read-only and maps the
// contract's exits"), under its case names. Where the tier gave exit 4 (not
// reachable) or 2 (the command broke the contract), StatusBlock gives an
// *InfraError, Violation set for 2: either way an infrastructure error for
// the round, never a verdict. Dropped: "the command's own progress reaches
// stderr", as Go quotes the stderr's tail in the error instead of passing it
// through. The lane-off cases are internal/config's TestDeployedLane and
// internal/cli's TestFireDeployed, where no status runs with the Lane off.
func TestStatusBlock(t *testing.T) {
	deployed := &config.Deployed{Command: "verify/provider", Enabled: true}
	surfaces := map[string]config.Surface{
		"web": {Kind: "browser", URLKey: "FIXTURE_WEB_URL"},
		"api": {Kind: "api", URLKey: "FIXTURE_API_URL"},
	}
	const block = "FIXTURE_WEB_URL=https://live.example\nFIXTURE_API_URL=https://live.example/api?a=b\n"
	status := func(t *testing.T, reply providertest.Reply) ([][2]string, error, *providertest.Fake, string) {
		t.Helper()
		dir := t.TempDir()
		fake := providertest.New(t, providertest.Script{Status: []providertest.Reply{reply}})
		os.MkdirAll(filepath.Join(dir, "verify"), 0o755)
		if err := os.WriteFile(filepath.Join(dir, "verify", "provider"), []byte("#!/bin/sh\nexec \""+fake.Path+"\" \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := StatusBlock(context.Background(), dir, deployed, surfaces, os.Environ())
		return got, err, fake, dir
	}

	t.Run("healthy: exit 0, stdout is exactly the block", func(t *testing.T) {
		got, err, fake, dir := status(t, providertest.Reply{Stdout: block, Stderr: "status: probing the live environment\n"})
		if err != nil || len(got) != 2 || got[0] != [2]string{"FIXTURE_WEB_URL", "https://live.example"} || got[1] != [2]string{"FIXTURE_API_URL", "https://live.example/api?a=b"} {
			t.Fatalf("block = %v, err = %v", got, err)
		}
		if fake.Dir() != dir {
			t.Errorf("status ran in %s, want the checkout %s", fake.Dir(), dir)
		}
	})
	t.Run("healthy: the command was called with status, and nothing else", func(t *testing.T) {
		_, _, fake, _ := status(t, providertest.Reply{Stdout: block})
		if calls := strings.Join(fake.Calls(), "|"); calls != "status" {
			t.Errorf("calls = %s", calls)
		}
	})

	for _, c := range []struct {
		name      string
		reply     providertest.Reply
		want      string
		violation bool
	}{
		{"unhealthy (status exit 1)", providertest.Reply{Exit: 1, Stderr: "the tunnel is down\n"}, "status exited 1 (the deployed environment is unhealthy): the tunnel is down", false},
		{"prerequisite missing (status exit 3)", providertest.Reply{Exit: 3, Stderr: "no key\n"}, "status exited 3 (prerequisite missing): no key", false},
		{"an exit outside the contract", providertest.Reply{Exit: 7}, "status exited 7, want 0 healthy, 1 unhealthy or 3 prerequisite missing", true},
		{"progress on stdout", providertest.Reply{Stdout: "probing...\n"}, "status printed a line that is not KEY=value: probing...", true},
		{"a lower-case key", providertest.Reply{Stdout: "web_url=http://x\n"}, "status printed a key that is not an uppercase shell identifier: web_url", true},
		{"an empty block", providertest.Reply{}, "status printed no keys", true},
		{"a declared Surface's url_key missing (ADR 0003: an infra-error)", providertest.Reply{Stdout: "FIXTURE_WEB_URL=https://live.example\n"},
			"surface api declares url_key FIXTURE_API_URL, which the status block does not carry", true},
	} {
		t.Run(c.name+": an infra error, no block", func(t *testing.T) {
			got, err, _, _ := status(t, c.reply)
			var infra *InfraError
			if got != nil || !errors.As(err, &infra) || !strings.Contains(infra.Detail, c.want) || infra.Violation != c.violation {
				t.Errorf("block = %v, err = %#v", got, err)
			}
		})
		t.Run(c.name+": never up, never down", func(t *testing.T) {
			_, _, fake, _ := status(t, c.reply)
			if calls := strings.Join(fake.Calls(), "|"); calls != "status" {
				t.Errorf("calls = %s", calls)
			}
		})
	}

	t.Run("a command that is not executable: exit 2", func(t *testing.T) {
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "verify"), 0o755)
		os.WriteFile(filepath.Join(dir, "verify", "provider"), []byte("#!/bin/sh\n"), 0o644)
		_, err := StatusBlock(context.Background(), dir, deployed, surfaces, os.Environ())
		var infra *InfraError
		if !errors.As(err, &infra) || !infra.Violation || !strings.Contains(infra.Detail, "the deployed command verify/provider is not an executable file") {
			t.Errorf("err = %v", err)
		}
	})
}
