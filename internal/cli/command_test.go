package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/fire"
)

// commandFixture is a Target Project directory with the given Harness config
// commands.
func commandFixture(t *testing.T, commands string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".auto-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	harness := `{"commit_scopes":["a"],"commands":` + commands + `,"pick":{"labels":{}}}`
	if err := os.WriteFile(filepath.Join(dir, ".auto-agent", "harness.json"), []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runCommand(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(append([]string{"command"}, args...), Env{Stdout: &out, Stderr: &errOut})
	return code, out.String(), errOut.String()
}

func TestCommand(t *testing.T) {
	t.Run("runs the named Harness command in the target dir, a chain with a glob and a substitution included", func(t *testing.T) {
		dir := commandFixture(t, `{"install":"true","test":"echo tested","lint":"ls *.txt && test -z \"$(cat empty.txt)\" && echo linted"}`)
		for _, name := range []string{"a.txt", "empty.txt"} {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		code, stdout, stderr := runCommand("lint", dir)
		if code != 0 || !strings.Contains(stdout, "a.txt\n") || !strings.HasSuffix(stdout, "linted\n") || stderr != "" {
			t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if code, stdout, _ := runCommand("test", dir); code != 0 || stdout != "tested\n" {
			t.Errorf("test: code %d, stdout %q", code, stdout)
		}
	})

	t.Run("exits as the command did, with its output", func(t *testing.T) {
		dir := commandFixture(t, `{"install":"true","test":"echo broken >&2; exit 3"}`)
		code, _, stderr := runCommand("test", dir)
		if code != 3 || stderr != "broken\n" {
			t.Errorf("code %d, stderr %q", code, stderr)
		}
	})

	t.Run("a command the Harness config does not declare fails and says so", func(t *testing.T) {
		dir := commandFixture(t, `{"install":"true","test":"true"}`)
		code, _, stderr := runCommand("lint", dir)
		if code != exitFailed || !strings.Contains(stderr, "declares no such command") {
			t.Errorf("code %d, stderr %q", code, stderr)
		}
	})

	t.Run("a wrong name or arity is a usage error", func(t *testing.T) {
		dir := commandFixture(t, `{"install":"true","test":"true"}`)
		if code, _, stderr := runCommand("build", dir); code != exitUsage || !strings.Contains(stderr, `unknown command "build"`) {
			t.Errorf("code %d, stderr %q", code, stderr)
		}
		if code, _, _ := runCommand(); code != exitUsage {
			t.Errorf("no name: code %d", code)
		}
	})

	t.Run("a directory with no Harness config fails", func(t *testing.T) {
		if code, _, stderr := runCommand("test", t.TempDir()); code != exitFailed || !strings.Contains(stderr, "harness.json") {
			t.Errorf("code %d, stderr %q", code, stderr)
		}
	})
}

func TestCommandInvocation(t *testing.T) {
	if got := fire.CommandInvocation("/opt/aa/auto-agent", "lint"); got != "/opt/aa/auto-agent command lint" {
		t.Errorf("CommandInvocation = %q", got)
	}
}
