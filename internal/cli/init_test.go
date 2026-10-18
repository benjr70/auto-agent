package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
)

// fixtureRepo is a throwaway Target Project holding the given files.
func fixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runInit runs `init` with nothing behind the Env but its output, so a
// network or Claude call would panic the test.
func runInit(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(append([]string{"init"}, args...), Env{Stdout: &out, Stderr: &errOut})
	return code, out.String(), errOut.String()
}

// loadScaffold reads the written Harness config back through the Harness
// config validation.
func loadScaffold(t *testing.T, dir string) config.Harness {
	t.Helper()
	h, err := config.LoadHarness(dir, "", config.Repo{Owner: "acme", Name: "widgets"})
	if err != nil {
		t.Fatalf("the scaffold does not validate: %v", err)
	}
	return h
}

func TestInitGoRepo(t *testing.T) {
	dir := fixtureRepo(t, map[string]string{"go.mod": "module example.com/widgets\n\ngo 1.25\n"})
	code, stdout, stderr := runInit(t, dir)
	if code != exitOK {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	h := loadScaffold(t, dir)
	if h.Commands.Test != "go test ./..." {
		t.Errorf("test = %q, want go test ./...", h.Commands.Test)
	}
	if h.Commands.Lint != "go vet ./..." {
		t.Errorf("lint = %q, want go vet ./...", h.Commands.Lint)
	}
	for _, want := range []string{"go test ./...", "go.mod", "commit", "doctor"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestInitEmptyRepoGetsPlaceholders(t *testing.T) {
	dir := fixtureRepo(t, nil)
	code, stdout, stderr := runInit(t, dir)
	if code != exitOK {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	h := loadScaffold(t, dir)
	for key, cmd := range map[string]string{"test": h.Commands.Test, "lint": h.Commands.Lint} {
		if !strings.Contains(cmd, "set commands."+key) || !strings.HasSuffix(cmd, "exit 1") {
			t.Errorf("%s = %q, want a placeholder that fails until it is set", key, cmd)
		}
	}
	if h.Commands.Install != "true" {
		t.Errorf("install = %q, want true", h.Commands.Install)
	}
	if h.Verification.Hermetic != nil {
		t.Errorf("hermetic = %+v, want the Bootstrap state", h.Verification.Hermetic)
	}
	if !strings.Contains(stdout, "placeholder") {
		t.Errorf("stdout = %q, want it to name the placeholders", stdout)
	}
}

func TestInitDetects(t *testing.T) {
	tests := []struct {
		name                string
		files               map[string]string
		install, test, lint string
	}{
		{
			name:    "a Node repo runs its own scripts with the tool its lockfile names",
			files:   map[string]string{"package.json": `{"scripts":{"test":"vitest run","lint":"eslint ."}}`, "pnpm-lock.yaml": ""},
			install: "pnpm install --frozen-lockfile", test: "pnpm test", lint: "pnpm run lint",
		},
		{
			name:    "npm's default test script is no test command",
			files:   map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`, "package-lock.json": "{}"},
			install: "npm ci",
		},
		{
			name:    "a Python repo runs pytest, and ruff when it is configured",
			files:   map[string]string{"pyproject.toml": "[project]\nname = \"widgets\"\n\n[tool.ruff]\nline-length = 100\n"},
			install: "python3 -m pip install -e .", test: "python3 -m pytest", lint: "ruff check .",
		},
		{
			name:    "a Makefile target wins over the ecosystem for its command",
			files:   map[string]string{"go.mod": "module x\n", "Makefile": ".PHONY: test lint\nVERSION := 1\ntest:\n\tgo test -race ./...\nlint: vet\n\tgolangci-lint run\n"},
			install: "go mod download", test: "make test", lint: "make lint",
		},
		{
			name:    "a GNU make ::= or :::= variable is no target",
			files:   map[string]string{"go.mod": "module x\n", "Makefile": "test ::= go test ./...\nlint :::= golangci-lint run\ninstall::=true\n"},
			install: "go mod download", test: "go test ./...", lint: "go vet ./...",
		},
		{
			name:    "every ecosystem a repo has contributes",
			files:   map[string]string{"go.mod": "module x\n", "package.json": `{"scripts":{"test":"jest"}}`},
			install: "go mod download && npm install", test: "go test ./... && npm test", lint: "go vet ./...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := fixtureRepo(t, tt.files)
			if code, _, stderr := runInit(t, dir); code != exitOK {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			h := loadScaffold(t, dir)
			for _, c := range []struct{ key, got, want string }{
				{"install", h.Commands.Install, tt.install},
				{"test", h.Commands.Test, tt.test},
				{"lint", h.Commands.Lint, tt.lint},
			} {
				if c.want == "" {
					if !strings.Contains(c.got, "set commands."+c.key) {
						t.Errorf("%s = %q, want a placeholder", c.key, c.got)
					}
				} else if c.got != c.want {
					t.Errorf("%s = %q, want %q", c.key, c.got, c.want)
				}
			}
		})
	}
}

func TestInitNeverOverwritesWithoutForce(t *testing.T) {
	const existing = "{\"mine\": true}\n"
	dir := fixtureRepo(t, map[string]string{".auto-agent/harness.json": existing, "go.mod": "module x\n"})
	file := filepath.Join(dir, ".auto-agent", "harness.json")

	code, stdout, stderr := runInit(t, dir)
	if code != exitFailed {
		t.Errorf("exit code = %d, want %d", code, exitFailed)
	}
	if !strings.Contains(stderr, "already exists") || !strings.Contains(stderr, "--force") {
		t.Errorf("stderr = %q, want the refusal naming --force", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
	if data, _ := os.ReadFile(file); string(data) != existing {
		t.Errorf("harness.json = %q, want it untouched", data)
	}

	if code, _, stderr := runInit(t, "--force", dir); code != exitOK {
		t.Fatalf("--force: exit code = %d, stderr = %q", code, stderr)
	}
	if h := loadScaffold(t, dir); h.Commands.Test != "go test ./..." {
		t.Errorf("--force: test = %q, want the scaffold's", h.Commands.Test)
	}
}

func TestInitCommitsNothing(t *testing.T) {
	dir := fixtureRepo(t, map[string]string{"go.mod": "module x\n"})
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", "go.mod")
	git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "initial")
	head := git("rev-parse", "HEAD")

	if code, _, stderr := runInit(t, dir); code != exitOK {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if got := git("rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s: init committed", got, head)
	}
	if got := git("status", "--porcelain"); got != "?? .auto-agent/" {
		t.Errorf("git status = %q, want only the untracked scaffold", got)
	}
}

func TestInitUsage(t *testing.T) {
	if code, _, stderr := runInit(t, "a", "b"); code != exitUsage || !strings.Contains(stderr, "at most one target dir") {
		t.Errorf("two dirs: exit code = %d, stderr = %q", code, stderr)
	}
	if code, _, stderr := runInit(t, filepath.Join(t.TempDir(), "missing")); code != exitFailed || !strings.Contains(stderr, "target dir not found") {
		t.Errorf("missing dir: exit code = %d, stderr = %q", code, stderr)
	}
}
