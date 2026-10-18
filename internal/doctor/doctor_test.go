package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/verify"
)

const plainConfig = `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}}}`

// world is a machine the checks run against: a Target Project checkout, a
// fake GitHub, and stubs for claude, the PATH, Docker, the display and the
// kernel's sandbox files.
type world struct {
	t       *testing.T
	dir     string
	gh      *githubtest.Server
	environ map[string]string
	m       Machine
	// version is what claude --version prints; status what claude auth
	// status prints.
	version, status string
	versionErr      error
	onPath          map[string]string
	dockerErr       error
	displayErr      error
	userns          string
	profiles        string
}

func newWorld(t *testing.T, harness string) *world {
	t.Helper()
	w := &world{t: t, dir: t.TempDir(), gh: githubtest.New(t), version: "2.1.205 (Claude Code)", status: `{"loggedIn":true,"authMethod":"oauth_token"}`,
		onPath: map[string]string{"chromium": "/usr/bin/chromium"}}
	w.environ = map[string]string{"GH_TOKEN": "t0ken", "CLAUDE_AUTH_MODE": "setup-token", "CLAUDE_CODE_OAUTH_TOKEN": "t", "HOME": t.TempDir()}
	kernel := t.TempDir()
	w.userns, w.profiles = filepath.Join(kernel, "userns"), filepath.Join(kernel, "profiles")
	os.WriteFile(w.userns, []byte("0\n"), 0o644)
	w.writeHarness(harness)
	w.gh.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "machine-user"}))
	w.gh.Handle("GET", "/repos/acme/widgets", githubtest.JSON(200, map[string]any{"full_name": "acme/widgets", "permissions": map[string]bool{"pull": true, "push": true}}))
	var labels []map[string]string
	for _, name := range harnessLabels {
		labels = append(labels, map[string]string{"name": name})
	}
	w.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, labels))
	return w
}

func (w *world) writeHarness(harness string) {
	os.MkdirAll(filepath.Join(w.dir, ".auto-agent"), 0o755)
	os.WriteFile(filepath.Join(w.dir, ".auto-agent", "harness.json"), []byte(harness), 0o644)
}

// provider puts an executable provider at verify/provider in the checkout.
func (w *world) provider() {
	os.MkdirAll(filepath.Join(w.dir, "verify"), 0o755)
	os.WriteFile(filepath.Join(w.dir, "verify", "provider"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
}

func (w *world) check() Report {
	w.t.Helper()
	return Check(context.Background(), Machine{
		TargetDir: w.dir, Repo: "acme/widgets",
		LookupEnv:   func(k string) (string, bool) { v, ok := w.environ[k]; return v, ok },
		GHAuthToken: func() (string, error) { return "", errors.New("gh is not logged in") },
		GitHubAPI:   w.gh.URL, UserAgent: "auto-agent/test",
		ClaudeVersion: func(context.Context) ([]byte, error) { return []byte(w.version), w.versionErr },
		AuthStatus:    func(context.Context) ([]byte, error) { return []byte(w.status), nil },
		LookPath: func(name string) (string, error) {
			if path, ok := w.onPath[name]; ok {
				return path, nil
			}
			return "", exec.ErrNotFound
		},
		Docker: func(context.Context) error { return w.dockerErr },
		SurfaceHost: verify.Host{
			ProbeDisplay:       func(context.Context, string) error { return w.displayErr },
			UsernsRestrictFile: w.userns, AppArmorProfilesFile: w.profiles,
		},
	})
}

// finding is the Finding of the named check, or a zero one.
func (r Report) finding(check string) Finding {
	for _, f := range r {
		if f.Check == check {
			return f
		}
	}
	return Finding{}
}

func (r Report) String() string {
	var lines []string
	for _, f := range r {
		lines = append(lines, f.String())
	}
	return strings.Join(lines, "\n")
}

// want is what one check should say: its severity and words its detail and
// fix carry. An empty severity is a check that must not run.
type want struct {
	check    string
	severity Severity
	says     []string
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		prepare func(*world)
		want    []want
		fatal   bool
	}{
		{
			name: "a healthy setup: every check ok, and the login named",
			want: []want{
				{"github token", OK, []string{"GH_TOKEN"}}, {"github login", OK, []string{"machine-user"}},
				{"github repo", OK, []string{"read and write acme/widgets"}}, {"github scopes", OK, nil}, {"github owner", OK, nil},
				{"claude cli", OK, []string{"2.1.205"}}, {"claude login", OK, []string{"setup-token"}},
				{"harness config", OK, []string{"Bootstrap state"}}, {"host config", OK, []string{"writable"}}, {"labels", OK, nil},
			},
		},
		{
			name:    "the token is the repo owner's: a warning listing what degrades",
			prepare: func(w *world) { w.gh.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "acme"})) },
			want: []want{{"github owner", Warning, []string{
				"no second-party review", "assigned to themselves gets picked", "treated as an Agent PR", "no notifications", "machine user",
			}}},
		},
		{
			name:    "claude older than the minimum is fatal",
			prepare: func(w *world) { w.version = "2.1.99 (Claude Code)" },
			want:    []want{{"claude cli", Fatal, []string{"2.1.99", "2.1.205 or later", "claude update"}}, {"claude login", "", nil}},
			fatal:   true,
		},
		{
			name:    "a newer claude passes",
			prepare: func(w *world) { w.version = "2.10.0 (Claude Code)" },
			want:    []want{{"claude cli", OK, []string{"2.10.0"}}},
		},
		{
			// setup.test.sh: test_doctor
			name:    "the missing command and its install line",
			prepare: func(w *world) { w.versionErr = &exec.Error{Name: "claude", Err: exec.ErrNotFound} },
			want:    []want{{"claude cli", Fatal, []string{"not on the PATH", "npm install -g @anthropic-ai/claude-code"}}},
			fatal:   true,
		},
		{
			// setup.test.sh: test_identity_failures
			name:    "claude not logged in: exit 6 naming the precondition",
			prepare: func(w *world) { w.status = `{"loggedIn":false}` },
			want:    []want{{"claude login", Fatal, []string{"not logged in", "claude setup-token"}}},
			fatal:   true,
		},
		{
			// setup.test.sh: test_identity_failures
			name: "api-key refuses: exit 6",
			prepare: func(w *world) {
				w.environ["CLAUDE_AUTH_MODE"], w.environ["ANTHROPIC_API_KEY"] = "api-key", "k"
				delete(w.environ, "CLAUDE_CODE_OAUTH_TOKEN")
				w.status = `{"loggedIn":true,"apiKeySource":"ANTHROPIC_API_KEY"}`
			},
			want:  []want{{"claude login", Fatal, []string{"refuses to start until spend pacing exists", "CLAUDE_AUTH_MODE=login or setup-token"}}},
			fatal: true,
		},
		{
			name:    "claude logged in another way than CLAUDE_AUTH_MODE says is fatal",
			prepare: func(w *world) { w.status = `{"loggedIn":true,"authMethod":"claude.ai"}` },
			want:    []want{{"claude login", Fatal, []string{`authMethod "claude.ai"`, "CLAUDE_AUTH_MODE"}}},
			fatal:   true,
		},
		{
			name:    "no CLAUDE_AUTH_MODE is a warning: the mode is only checked when it is set",
			prepare: func(w *world) { delete(w.environ, "CLAUDE_AUTH_MODE") },
			want:    []want{{"claude login", Warning, []string{"CLAUDE_AUTH_MODE is not set", "set CLAUDE_AUTH_MODE="}}},
		},
		{
			name:    "no token anywhere is fatal, and GitHub is not asked",
			prepare: func(w *world) { delete(w.environ, "GH_TOKEN") },
			want:    []want{{"github token", Fatal, []string{"GH_TOKEN is not set", "gh auth login"}}, {"github login", "", nil}, {"labels", "", nil}},
			fatal:   true,
		},
		{
			name:    "a token GitHub refuses is unreadable: fatal",
			prepare: func(w *world) { w.gh.Handle("GET", "/user", githubtest.Text(401, `{"message":"Bad credentials"}`)) },
			want:    []want{{"github login", Fatal, []string{"cannot be read", "401"}}, {"github repo", "", nil}},
			fatal:   true,
		},
		{
			name: "a token that can read but not write the repo is fatal",
			prepare: func(w *world) {
				w.gh.Handle("GET", "/repos/acme/widgets", githubtest.JSON(200, map[string]any{"full_name": "acme/widgets", "permissions": map[string]bool{"pull": true}}))
			},
			want:  []want{{"github repo", Fatal, []string{"not write", "collaborators/machine-user"}}},
			fatal: true,
		},
		{
			name: "a repo the token cannot see is fatal",
			prepare: func(w *world) {
				w.gh.Handle("GET", "/repos/acme/widgets", githubtest.Text(404, `{"message":"Not Found"}`))
			},
			want:  []want{{"github repo", Fatal, []string{"cannot read acme/widgets", "permission=maintain"}}},
			fatal: true,
		},
		{
			// setup.test.sh: test_identity_failures
			name:    "a PAT without the project scope: exit 5",
			harness: `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"project":{"number":1,"priority_field":"P"}}}`,
			prepare: func(w *world) {
				w.gh.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "machine-user"}).WithHeader("X-OAuth-Scopes", "repo"))
			},
			want:  []want{{"github scopes", Fatal, []string{"lacks the read:project scope", "gh auth refresh -s read:project"}}},
			fatal: true,
		},
		{
			name: "a classic token without repo is fatal; a labels pick needs no project scope",
			prepare: func(w *world) {
				w.gh.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "machine-user"}).WithHeader("X-OAuth-Scopes", "read:org"))
			},
			want:  []want{{"github scopes", Fatal, []string{"lacks the repo scope"}}},
			fatal: true,
		},
		{
			name:    "a classic token with the scopes a project pick needs passes",
			harness: `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"project":{"number":1,"priority_field":"P"}}}`,
			prepare: func(w *world) {
				w.gh.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "machine-user"}).WithHeader("X-OAuth-Scopes", "repo, project"))
			},
			want: []want{{"github scopes", OK, []string{"repo, read:project"}}},
		},
		{
			name: "missing labels are a warning only",
			prepare: func(w *world) {
				w.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, []map[string]string{{"name": "AFK"}}))
			},
			want: []want{{"labels", Warning, []string{"AFK:in-progress", "run creates them", "gh label create"}}},
		},
		{
			name:    "an invalid Harness config is fatal, naming the key",
			harness: `{"pick":{}}`,
			want:    []want{{"harness config", Fatal, []string{`missing required key "commands"`, ".auto-agent/harness.json"}}},
			fatal:   true,
		},
		{
			name:    "a Host config that does not load is fatal",
			prepare: func(w *world) { w.environ["AUTO_AGENT_STATE_DIR"] = "relative/state" },
			want:    []want{{"host config", Fatal, []string{"absolute path", "config.env"}}},
			fatal:   true,
		},
		{
			name: "a State dir that cannot be written is fatal, and nothing is made",
			prepare: func(w *world) {
				locked := filepath.Join(t.TempDir(), "locked")
				os.MkdirAll(locked, 0o555)
				w.environ["AUTO_AGENT_STATE_DIR"] = filepath.Join(locked, "state")
			},
			want:  []want{{"host config", Fatal, []string{"cannot be written", "sudo install -d"}}},
			fatal: true,
		},
		{
			// setup.test.sh: test_base_needs
			name:    "a cli-only project needs nothing",
			harness: `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"surfaces":{"cli":{"kind":"cli","url_key":"X","paths":["a"]}}}`,
			prepare: func(w *world) { w.displayErr, w.onPath = errors.New("no display"), nil },
			want:    []want{{"surface cli", OK, []string{"needs nothing"}}, {"surface cli: display", "", nil}, {"surface cli: chromium", "", nil}, {"docker", "", nil}},
		},
		{
			// setup.test.sh: test_base_needs
			name: "no surfaces block at all needs nothing",
			prepare: func(w *world) {
				w.displayErr, w.onPath, w.dockerErr = errors.New("no display"), nil, errors.New("down")
			},
			want: []want{{"docker", "", nil}, {"provider", "", nil}},
		},
		{
			// setup.test.sh: test_base_needs
			name: "an electron Surface needs a display; host.docker needs Docker",
			harness: `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},
				"surfaces":{"app":{"kind":"electron","url_key":"X","paths":["a"],"launcher":"bin/app"}},"host":{"docker":true}}`,
			prepare: func(w *world) {
				w.environ["DISPLAY"], w.dockerErr = ":99", errors.New("Cannot connect to the Docker daemon")
			},
			want: []want{
				{"surface app: display", OK, []string{":99"}}, {"surface app: chromium", "", nil},
				{"docker", Fatal, []string{"Cannot connect to the Docker daemon", "systemctl enable --now docker"}},
			},
			fatal: true,
		},
		{
			name:    "an Electron Surface with no display: fatal, with the fix",
			harness: `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"surfaces":{"app":{"kind":"electron","url_key":"X","paths":["a"],"launcher":"bin/app"}}}`,
			want:    []want{{"surface app: display", Fatal, []string{"electron Surface needs a display", "DISPLAY is not set", "Xvfb :99", "export DISPLAY=:99"}}},
			fatal:   true,
		},
		{
			name:    "a browser Surface needs Chromium",
			harness: `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"surfaces":{"web":{"kind":"browser","url_key":"X","paths":["a"]}}}`,
			prepare: func(w *world) { w.environ["DISPLAY"], w.onPath = ":99", nil },
			want:    []want{{"surface web: display", OK, nil}, {"surface web: chromium", Fatal, []string{"none is on the PATH", "npx playwright install"}}, {"surface web: sandbox", "", nil}},
			fatal:   true,
		},
		{
			name: "a hermetic command that is not executable is exit 1, named",
			harness: `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},
				"verification":{"hermetic":{"command":"verify/provider","smoke":true}}}`,
			want:  []want{{"harness config", OK, []string{"valid"}}, {"provider", Fatal, []string{"the hermetic command verify/provider is not an executable file under", "chmod +x"}}},
			fatal: true,
		},
		{
			name: "an executable provider passes, and there is no Bootstrap state",
			harness: `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},
				"verification":{"hermetic":{"command":"verify/provider","smoke":true}}}`,
			prepare: func(w *world) { w.provider() },
			want:    []want{{"provider", OK, []string{"can be executed"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, plainConfig)
			if tt.harness != "" {
				w.writeHarness(tt.harness)
			}
			if tt.prepare != nil {
				tt.prepare(w)
			}
			report := w.check()
			for _, want := range tt.want {
				got := report.finding(want.check)
				if got.Severity != want.severity {
					t.Errorf("%s: severity %q, want %q\n%s", want.check, got.Severity, want.severity, report)
					continue
				}
				for _, says := range want.says {
					if !strings.Contains(got.String(), says) {
						t.Errorf("%s says %q, want it to carry %q", want.check, got, says)
					}
				}
				if got.Severity != OK && got.Severity != "" && got.Fix == "" {
					t.Errorf("%s names no fix", want.check)
				}
			}
			if report.Fatal() != tt.fatal {
				t.Errorf("fatal = %v, want %v\n%s", report.Fatal(), tt.fatal, report)
			}
			if writes := w.gh.Writes(); len(writes) != 0 {
				t.Errorf("the doctor wrote to GitHub: %+v", writes)
			}
		})
	}
}

// The display and sandbox cases of lib/display-env.test.sh, as the doctor
// reports them for a declared Surface.
func TestSurfaceHostNeeds(t *testing.T) {
	const electron = `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"surfaces":{"app":{"kind":"electron","url_key":"X","paths":["a"],"launcher":"bin/app"}}}`
	tests := []struct {
		name     string
		prepare  func(*world)
		check    string
		severity Severity
		says     string
	}{
		{name: "resolve: a declared display", prepare: func(w *world) { w.environ["DISPLAY"] = ":99" }, check: "surface app: display", severity: OK, says: "DISPLAY=:99"},
		{name: "resolve: no display", check: "surface app: display", severity: Fatal, says: "refuses to fall back to headless"},
		{name: "resolve: a dead display", prepare: func(w *world) { w.environ["DISPLAY"], w.displayErr = ":99", errors.New("exit status 1") }, check: "surface app: display", severity: Fatal, says: "does not answer"},
		{name: "sandbox: unrestricted Host", check: "surface app: sandbox", severity: OK, says: "can run"},
		{
			name: "sandbox: the AppArmor profile",
			prepare: func(w *world) {
				os.WriteFile(w.userns, []byte("1\n"), 0o644)
				os.WriteFile(w.profiles, []byte(filepath.Join(w.dir, "bin", "app")+" (unconfined)\n"), 0o644)
			},
			check: "surface app: sandbox", severity: OK, says: "can run",
		},
		{
			name: "sandbox: degraded fallback",
			prepare: func(w *world) {
				os.WriteFile(w.userns, []byte("1\n"), 0o644)
				os.WriteFile(w.profiles, []byte("/usr/bin/other (enforce)\n"), 0o644)
			},
			check: "surface app: sandbox", severity: Warning, says: "ELECTRON_DISABLE_SANDBOX=1",
		},
		{name: "sandbox: forced by the Host env", prepare: func(w *world) { w.environ["AUTO_AGENT_ELECTRON_SANDBOX"] = "disabled" }, check: "surface app: sandbox", severity: Warning, says: "DEGRADED"},
		{name: "sandbox: no kernel switch", prepare: func(w *world) { os.Remove(w.userns) }, check: "surface app: sandbox", severity: OK, says: "can run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, electron)
			if tt.prepare != nil {
				tt.prepare(w)
			}
			got := w.check().finding(tt.check)
			if got.Severity != tt.severity || !strings.Contains(got.String(), tt.says) {
				t.Errorf("%s = %q, want %s carrying %q", tt.check, got, tt.severity, tt.says)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"2.1.205", true}, {"2.1.206", true}, {"2.2.0", true}, {"3.0.0", true}, {"v2.1.205", true},
		{"2.1.204", false}, {"2.1.99", false}, {"1.9.999", false}, {"2.1", false}, {"garbage", false},
	}
	for _, tt := range tests {
		if got := atLeast(tt.version, MinClaude); got != tt.want {
			t.Errorf("atLeast(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}
