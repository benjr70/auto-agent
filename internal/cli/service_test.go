package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serviceFixture is a Linux Host with a stubbed systemctl and loginctl.
type serviceFixture struct {
	home           string
	stdout, stderr bytes.Buffer
	environ        map[string]string
	goos           string
	origin         string
	// linger is what loginctl says of the user's lingering.
	linger string
	// fail makes the systemctl call whose arguments contain it fail.
	fail string
	// calls are the commands run, one string each.
	calls []string
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	home := t.TempDir()
	return &serviceFixture{
		home: home, goos: "linux", origin: "git@github.com:acme/widgets.git", linger: "no",
		environ: map[string]string{"HOME": home, "USER": "op"},
	}
}

func (f *serviceFixture) unitFile() string {
	return filepath.Join(f.home, ".config", "systemd", "user", "auto-agent-acme.widgets.service")
}

func (f *serviceFixture) run(args ...string) int {
	return Run(args, Env{
		Stdout: &f.stdout, Stderr: &f.stderr,
		LookupEnv:  func(k string) (string, bool) { v, ok := f.environ[k]; return v, ok },
		OriginURL:  func(string) (string, error) { return f.origin, nil },
		GOOS:       f.goos,
		Executable: func() (string, error) { return "/opt/aa/auto-agent", nil },
		Command: func(name string, args ...string) ([]byte, error) {
			call := name + " " + strings.Join(args, " ")
			f.calls = append(f.calls, call)
			if name == "loginctl" {
				return []byte(f.linger + "\n"), nil
			}
			if f.fail != "" && strings.Contains(call, f.fail) {
				return []byte("Failed to connect to bus"), errors.New("exit status 1")
			}
			return nil, nil
		},
	})
}

func (f *serviceFixture) systemctlCalls() []string {
	var got []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "systemctl ") {
			got = append(got, c)
		}
	}
	return got
}

func TestServiceInstall(t *testing.T) {
	t.Run("daemon: writes the unit and prints its path", func(t *testing.T) {
		f := newServiceFixture(t)
		if code := f.run("service", "install"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		unit, err := os.ReadFile(f.unitFile())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(unit), "\nExecStart=/opt/aa/auto-agent run --repo acme/widgets\n") {
			t.Errorf("unit:\n%s", unit)
		}
		envFile := filepath.Join(f.home, ".config", "auto-agent", "acme", "widgets", "secrets.env")
		if !strings.Contains(string(unit), "\nEnvironmentFile=-"+envFile+"\n") {
			t.Errorf("unit does not point at the operator's environment file:\n%s", unit)
		}
		if !strings.Contains(f.stdout.String(), f.unitFile()) {
			t.Errorf("stdout does not name the unit: %s", f.stdout.String())
		}
		want := "systemctl --user daemon-reload|systemctl --user enable auto-agent-acme.widgets.service|systemctl --user restart auto-agent-acme.widgets.service"
		if got := strings.Join(f.systemctlCalls(), "|"); got != want {
			t.Errorf("systemctl calls = %s", got)
		}
		if !strings.Contains(f.stdout.String(), "journalctl --user -u auto-agent-acme.widgets.service -f") {
			t.Errorf("no log hint: %s", f.stdout.String())
		}
	})
	t.Run("no secret is written: the environment file is the operator's", func(t *testing.T) {
		f := newServiceFixture(t)
		f.environ["GH_TOKEN"] = "ghp_secret"
		f.environ["CLAUDE_CODE_OAUTH_TOKEN"] = "sk-secret"
		if code := f.run("service", "install"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		envFile := filepath.Join(f.home, ".config", "auto-agent", "acme", "widgets", "secrets.env")
		if _, err := os.Stat(envFile); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the environment file was created: %v", err)
		}
		unit, _ := os.ReadFile(f.unitFile())
		if strings.Contains(string(unit), "ghp_secret") || strings.Contains(string(unit), "sk-secret") {
			t.Errorf("a secret reached the unit:\n%s", unit)
		}
		if !strings.Contains(f.stdout.String(), envFile) || !strings.Contains(f.stdout.String(), "chmod 600") {
			t.Errorf("no word on creating the missing environment file: %s", f.stdout.String())
		}
	})
	t.Run("--env-file names another environment file", func(t *testing.T) {
		f := newServiceFixture(t)
		envFile := filepath.Join(f.home, "aa.env")
		if err := os.WriteFile(envFile, []byte("GH_TOKEN=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := f.run("service", "install", "--env-file", envFile); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		unit, _ := os.ReadFile(f.unitFile())
		if !strings.Contains(string(unit), "\nEnvironmentFile=-"+envFile+"\n") {
			t.Errorf("unit:\n%s", unit)
		}
		if strings.Contains(f.stdout.String(), "chmod 600") {
			t.Errorf("an existing environment file was reported missing: %s", f.stdout.String())
		}
		if data, _ := os.ReadFile(envFile); string(data) != "GH_TOKEN=x\n" {
			t.Errorf("the operator's environment file was changed: %q", data)
		}
	})
	t.Run("lingering off: the hint is printed", func(t *testing.T) {
		f := newServiceFixture(t)
		if code := f.run("service", "install"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if f.calls[len(f.calls)-1] != "loginctl show-user op --property=Linger --value" {
			t.Errorf("calls = %v", f.calls)
		}
		if !strings.Contains(f.stdout.String(), "loginctl enable-linger op") {
			t.Errorf("no linger hint: %s", f.stdout.String())
		}
	})
	t.Run("lingering on: no hint", func(t *testing.T) {
		f := newServiceFixture(t)
		f.linger = "yes"
		if code := f.run("service", "install"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if strings.Contains(f.stdout.String(), "enable-linger") {
			t.Errorf("linger hint with lingering on: %s", f.stdout.String())
		}
	})
	t.Run("an existing unit is refused without --force", func(t *testing.T) {
		f := newServiceFixture(t)
		if err := os.MkdirAll(filepath.Dir(f.unitFile()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.unitFile(), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := f.run("service", "install"); code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if data, _ := os.ReadFile(f.unitFile()); string(data) != "mine\n" {
			t.Errorf("the unit was overwritten: %q", data)
		}
		if !strings.Contains(f.stderr.String(), "--force") || len(f.systemctlCalls()) != 0 {
			t.Errorf("stderr %q, calls %v", f.stderr.String(), f.calls)
		}
		f.stderr.Reset()
		if code := f.run("service", "install", "--force"); code != 0 {
			t.Fatalf("--force: exit %d: %s", code, f.stderr.String())
		}
		if data, _ := os.ReadFile(f.unitFile()); !strings.Contains(string(data), "ExecStart=") {
			t.Errorf("--force did not replace the unit: %q", data)
		}
	})
	t.Run("--no-enable only prints the commands", func(t *testing.T) {
		f := newServiceFixture(t)
		if code := f.run("service", "install", "--no-enable"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if len(f.systemctlCalls()) != 0 {
			t.Errorf("systemctl ran: %v", f.calls)
		}
		for _, want := range []string{"systemctl --user daemon-reload", "systemctl --user enable --now auto-agent-acme.widgets.service"} {
			if !strings.Contains(f.stdout.String(), want) {
				t.Errorf("stdout lacks %q: %s", want, f.stdout.String())
			}
		}
		if _, err := os.Stat(f.unitFile()); err != nil {
			t.Errorf("the unit was not written: %v", err)
		}
	})
	t.Run("--repo names the Target Project without a checkout", func(t *testing.T) {
		f := newServiceFixture(t)
		f.origin = ""
		if code := f.run("service", "install", "--repo", "acme/gadgets", "--no-enable"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if _, err := os.Stat(filepath.Join(f.home, ".config", "systemd", "user", "auto-agent-acme.gadgets.service")); err != nil {
			t.Error(err)
		}
	})
	t.Run("a systemctl failure fails the install, and says so", func(t *testing.T) {
		f := newServiceFixture(t)
		f.fail = "daemon-reload"
		if code := f.run("service", "install"); code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if !strings.Contains(f.stderr.String(), "Failed to connect to bus") {
			t.Errorf("stderr = %q", f.stderr.String())
		}
	})
	t.Run("non-Linux: a clear refusal", func(t *testing.T) {
		f := newServiceFixture(t)
		f.goos = "darwin"
		if code := f.run("service", "install"); code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if !strings.Contains(f.stderr.String(), "Linux only") || !strings.Contains(f.stderr.String(), "darwin") {
			t.Errorf("stderr = %q", f.stderr.String())
		}
		if _, err := os.Stat(f.unitFile()); !errors.Is(err, os.ErrNotExist) || len(f.calls) != 0 {
			t.Errorf("something was done: %v %v", err, f.calls)
		}
	})
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"an unknown option", []string{"service", "install", "--bogus"}},
		{"no service subcommand", []string{"service"}},
		{"an unknown service subcommand", []string{"service", "remove"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newServiceFixture(t)
			if code := f.run(tt.args...); code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
		})
	}
}
