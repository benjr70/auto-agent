package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
)

var widgets = config.Repo{Owner: "acme", Name: "widgets"}

func unitFor(t *testing.T) Unit {
	t.Helper()
	return Unit{Repo: widgets, Binary: "/home/op/go/bin/auto-agent", EnvFile: "/home/op/.config/auto-agent/acme/widgets/secrets.env", Path: "/opt/claude/bin:/usr/bin:/bin"}
}

// lines are the unit's KEY=value lines whose key is in keys, in order.
func lines(unit string, keys ...string) string {
	var got []string
	for _, line := range strings.Split(unit, "\n") {
		for _, k := range keys {
			if strings.HasPrefix(line, k+"=") {
				got = append(got, line)
			}
		}
	}
	return strings.Join(got, "\n")
}

func TestName(t *testing.T) {
	if got := Name(widgets); got != "auto-agent-acme.widgets.service" {
		t.Errorf("Name = %q", got)
	}
	for _, pair := range [][2]config.Repo{
		{widgets, {Owner: "acme", Name: "gadgets"}},
		{{Owner: "acme-co", Name: "x"}, {Owner: "acme", Name: "co-x"}},
		{{Owner: "acme", Name: "co.x"}, {Owner: "acme", Name: "co-x"}},
	} {
		if Name(pair[0]) == Name(pair[1]) {
			t.Errorf("%s and %s share the unit name %s", pair[0].Slug(), pair[1].Slug(), Name(pair[0]))
		}
	}
}

func TestRender(t *testing.T) {
	t.Run("daemon: user, install, PATH, EnvironmentFile, ExecStart and MemoryMax come from the Host env and the install", func(t *testing.T) {
		// Ported: the unit is a user unit (no User=), runs `run` for one
		// Target Project with the binary's absolute path, and points at the
		// operator's environment file. MemoryMax and the install dir are gone.
		unit, err := Render(unitFor(t))
		if err != nil {
			t.Fatal(err)
		}
		want := `Environment=PATH=/opt/claude/bin:/usr/bin:/bin
EnvironmentFile=-/home/op/.config/auto-agent/acme/widgets/secrets.env
ExecStart=/home/op/go/bin/auto-agent run --repo acme/widgets`
		if got := lines(unit, "User", "WorkingDirectory", "Environment", "EnvironmentFile", "ExecStart"); got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
		if !strings.Contains(unit, "Description=auto-agent Daemon for acme/widgets\n") || !strings.Contains(unit, "SyslogIdentifier=auto-agent-acme.widgets\n") {
			t.Errorf("the unit does not name its Target Project:\n%s", unit)
		}
	})
	t.Run("daemon: restarts on failure, except on the Daemon's usage refusal", func(t *testing.T) {
		unit, err := Render(unitFor(t))
		if err != nil {
			t.Fatal(err)
		}
		if got := lines(unit, "Restart", "RestartPreventExitStatus", "WantedBy"); got != "Restart=on-failure\nRestartPreventExitStatus=2\nWantedBy=default.target" {
			t.Errorf("got\n%s", got)
		}
	})
	t.Run("no placeholder is left", func(t *testing.T) {
		unit, err := Render(unitFor(t))
		if err != nil {
			t.Fatal(err)
		}
		if placeholder.MatchString(unit) {
			t.Errorf("placeholders left: %v", placeholder.FindAllString(unit, -1))
		}
	})
	t.Run("75% renders as 75%%", func(t *testing.T) {
		u := unitFor(t)
		u.EnvFile = "/home/op/75%/secrets.env"
		unit, err := Render(u)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(unit, "\nEnvironmentFile=-/home/op/75%%/secrets.env\n") {
			t.Errorf("got %s", lines(unit, "EnvironmentFile"))
		}
	})
	t.Run("systemd-analyze verify is silent on both units", func(t *testing.T) {
		analyze, err := exec.LookPath("systemd-analyze")
		if err != nil {
			t.Skip("systemd-analyze not installed")
		}
		dir := t.TempDir()
		u := unitFor(t)
		u.Binary = filepath.Join(dir, "auto-agent")
		if err := os.WriteFile(u.Binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		unit, err := Render(u)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, Name(widgets))
		if err := os.WriteFile(file, []byte(unit), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(analyze, "verify", file).CombinedOutput()
		if err != nil || len(out) != 0 {
			t.Errorf("systemd-analyze verify: %v\n%s", err, out)
		}
	})
}

func TestRenderRefusesUnsafeValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*Unit)
		want string
	}{
		{"AUTO_AGENT_UNIT_PATH: exit 1 and says why", func(u *Unit) { u.Path = "/a b:/usr/bin" }, "whitespace"},
		{"AUTO_AGENT_UNIT_PATH: exit 1 and says why", func(u *Unit) { u.Path = "/a&b:/usr/bin" }, "& or a backslash"},
		{"a relative binary", func(u *Unit) { u.Binary = "bin/auto-agent" }, "absolute"},
		{"a binary path with whitespace", func(u *Unit) { u.Binary = "/home/op/my bin/auto-agent" }, "whitespace"},
		{"a relative environment file", func(u *Unit) { u.EnvFile = "secrets.env" }, "absolute"},
		{"a newline in the environment file", func(u *Unit) { u.EnvFile = "/home/op/a\nExecStartPre=/bin/evil" }, "newline"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := unitFor(t)
			tt.edit(&u)
			if _, err := Render(u); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to say %q", err, tt.want)
			}
		})
	}
}

func TestPaths(t *testing.T) {
	env := map[string]string{"HOME": "/home/op"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if got := Dir(lookup); got != "/home/op/.config/systemd/user" {
		t.Errorf("Dir = %q", got)
	}
	if got := EnvFile(widgets, lookup); got != "/home/op/.config/auto-agent/acme/widgets/secrets.env" {
		t.Errorf("EnvFile = %q", got)
	}
	t.Run("the invoking user, 8G and ~/.local/bin on PATH", func(t *testing.T) {
		// Ported: only ~/.local/bin on PATH still applies.
		if got := UnitPath(lookup); !strings.HasPrefix(got, "/home/op/.local/bin:/usr/local/sbin:") {
			t.Errorf("UnitPath = %q", got)
		}
		env["AUTO_AGENT_UNIT_PATH"] = "/opt/claude/bin:/usr/bin"
		if got := UnitPath(lookup); got != "/opt/claude/bin:/usr/bin" {
			t.Errorf("UnitPath = %q, want the override", got)
		}
	})
	env["XDG_CONFIG_HOME"] = "/cfg"
	if got := Dir(lookup); got != "/cfg/systemd/user" {
		t.Errorf("Dir with XDG_CONFIG_HOME = %q", got)
	}
	if got := EnvFile(widgets, lookup); got != "/cfg/auto-agent/acme/widgets/secrets.env" {
		t.Errorf("EnvFile with XDG_CONFIG_HOME = %q", got)
	}
}
