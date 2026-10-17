package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func probeOK(context.Context, string) error   { return nil }
func probeDead(context.Context, string) error { return errors.New("no answer") }

// Ported from lib/display-env.test.sh, under its case names. The display
// comes from the Host's DISPLAY only: AUTO_AGENT_DISPLAY is no longer read,
// so its case now asserts that it is ignored.
func TestDisplay(t *testing.T) {
	for _, tt := range []struct {
		name  string
		env   []string
		probe func(context.Context, string) error
		want  string
		infra string
	}{
		{name: "resolve: a declared display", env: []string{"DISPLAY=:99"}, probe: probeOK, want: ":99"},
		{name: "resolve: AUTO_AGENT_DISPLAY", env: []string{"AUTO_AGENT_DISPLAY=:7"}, probe: probeOK, infra: "refuses to fall back to headless"},
		{name: "resolve: no display", probe: probeOK, infra: "refuses to fall back to headless"},
		{name: "resolve: a dead display", env: []string{"DISPLAY=:99"}, probe: probeDead, infra: "does not answer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Host{Env: tt.env, ProbeDisplay: tt.probe}.Display(context.Background())
			var infra *Infra
			switch {
			case tt.infra == "" && (err != nil || got != tt.want):
				t.Errorf("Display() = %q, %v; want %q", got, err, tt.want)
			case tt.infra != "" && (!errors.As(err, &infra) || !strings.Contains(infra.Reason, tt.infra)):
				t.Errorf("Display() = %q, %v; want an Infra finding saying %q", got, err, tt.infra)
			}
		})
	}
}

func write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSandboxMode(t *testing.T) {
	off, on := write(t, "userns-off", "0\n"), write(t, "userns-on", "1\n")
	profiles := write(t, "profiles", "/opt/app/electron (unconfined)\n/usr/bin/other (enforce)\n")
	other := write(t, "profiles-other", "/usr/bin/other (enforce)\n")
	for _, tt := range []struct {
		name              string
		env               []string
		restrict, profile string
		want              Sandbox
	}{
		{name: "sandbox: unrestricted Host", restrict: off, want: SandboxOn},
		{name: "sandbox: the AppArmor profile", restrict: on, profile: profiles, want: SandboxAppArmor},
		{name: "sandbox: degraded fallback", restrict: on, profile: other, want: SandboxDisabled},
		{name: "sandbox: forced by the Host env", env: []string{"AUTO_AGENT_ELECTRON_SANDBOX=disabled"}, restrict: off, want: SandboxDisabled},
		{name: "sandbox: no kernel switch", restrict: filepath.Join(t.TempDir(), "not-there"), want: SandboxOn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := Host{Env: tt.env, UsernsRestrictFile: tt.restrict, AppArmorProfilesFile: or(tt.profile, filepath.Join(t.TempDir(), "none"))}
			if got := h.SandboxMode("/opt/app/electron"); got != tt.want {
				t.Errorf("SandboxMode() = %s, want %s", got, tt.want)
			}
		})
	}
	t.Run("degraded: one mode", func(t *testing.T) {
		for mode, want := range map[Sandbox]bool{SandboxOn: false, SandboxAppArmor: false, SandboxDisabled: true} {
			if mode.Degraded() != want {
				t.Errorf("%s.Degraded() = %v", mode, !want)
			}
		}
	})
}
