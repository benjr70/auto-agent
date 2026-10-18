package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

const electronConfig = `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},
	"surfaces":{"app":{"kind":"electron","url_key":"APP_URL","paths":["app/"],"launcher":"bin/app"}}}`

func TestDoctor(t *testing.T) {
	tests := []struct {
		name     string
		harness  string
		prepare  func(*fixture)
		wantCode int
		// want are lines, or parts of lines, stdout must carry.
		want []string
	}{
		{
			name:    "a healthy setup: exit 0, with the login printed",
			harness: labelsConfig,
			want: []string{
				"github token: ok — found in GH_TOKEN\n", "github login: ok — the token is machine-user\n",
				"github repo: ok — machine-user can read and write acme/widgets\n", "claude cli: ok — claude 2.1.205\n",
				"harness config: ok — valid; Bootstrap state", "labels: ok — all 20 harness labels exist\n",
			},
		},
		{
			name:    "the token is the repo owner's: a warning listing what degrades, exit 0",
			harness: labelsConfig,
			prepare: func(f *fixture) {
				f.gh.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "acme"}))
			},
			want: []string{"github owner: WARNING — the token is the repo owner acme's, so: no second-party review",
				"a ticket the owner assigned to themselves gets picked", "is treated as an Agent PR", "no notifications reach the owner",
				"— fix: run the Daemon as a machine user of its own"},
		},
		{
			name:     "claude older than the minimum: fatal, exit 1",
			harness:  labelsConfig,
			prepare:  func(f *fixture) { f.claudeVersion = "2.1.204 (Claude Code)" },
			wantCode: 1,
			want:     []string{"claude cli: FATAL — claude is 2.1.204; the Daemon needs 2.1.205 or later — fix: claude update\n"},
		},
		{
			name:     "an Electron Surface with no display: fatal, with the fix",
			harness:  electronConfig,
			wantCode: 1,
			want:     []string{"surface app: display: FATAL — the electron Surface needs a display", "— fix: start one and name it in the Host's environment"},
		},
		{
			name:     "an invalid Harness config in the checkout is fatal",
			harness:  `{"pick":{}}`,
			wantCode: 1,
			want:     []string{`harness config: FATAL — `, `missing required key "commands"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.harness)
			if tt.prepare != nil {
				tt.prepare(f)
			}
			code := f.run("doctor", f.dir)
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d\n%s", code, tt.wantCode, f.stdout.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(f.stdout.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, f.stdout.String())
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(f.stdout.String()), "\n") {
				if !strings.Contains(line, ": ok — ") && !strings.Contains(line, " — fix: ") {
					t.Errorf("a line that is neither ok nor a finding with its fix: %q", line)
				}
			}
			if len(f.gh.Writes()) != 0 || f.stderr.Len() != 0 {
				t.Errorf("writes %+v, stderr %q", f.gh.Writes(), f.stderr.String())
			}
		})
	}
	// setup.test.sh: test_unattended_setup
	t.Run("check exits 0", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		if code := f.run("doctor", f.dir); code != 0 {
			t.Errorf("exit %d\n%s", code, f.stdout.String())
		}
	})
	// setup.test.sh: test_unattended_setup
	t.Run("check runs no other stage", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.environ["AUTO_AGENT_STATE_DIR"] = filepath.Join(t.TempDir(), "state")
		f.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, []map[string]string{}))
		f.run("doctor", f.dir)
		if _, err := os.Stat(f.environ["AUTO_AGENT_STATE_DIR"]); err == nil || len(f.gh.Writes()) != 0 {
			t.Errorf("the doctor made the State dir or wrote to GitHub: %+v", f.gh.Writes())
		}
	})
	// setup.test.sh: test_secrets
	t.Run("check stays silent about secrets", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.environ["GH_TOKEN"], f.environ["CLAUDE_CODE_OAUTH_TOKEN"] = "SENTINEL-gh", "SENTINEL-claude"
		f.gh.Handle("GET", "/repos/acme/widgets", githubtest.Text(403, `{"message":"Resource not accessible"}`))
		f.run("doctor", f.dir)
		if strings.Contains(f.stdout.String()+f.stderr.String(), "SENTINEL") {
			t.Errorf("a secret was printed:\n%s%s", f.stdout.String(), f.stderr.String())
		}
	})
	t.Run("the repo comes from --repo, and two target dirs are one too many", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.origin = "not a repo"
		if code := f.run("doctor", "--repo", "acme/widgets", f.dir); code != 0 {
			t.Errorf("exit %d: %s", code, f.stdout.String())
		}
		if code := f.run("doctor", "a", "b"); code != 2 || !strings.Contains(f.stderr.String(), "at most one target dir") {
			t.Errorf("exit %d: %s", code, f.stderr.String())
		}
	})
}

func TestRunPreflight(t *testing.T) {
	t.Run("claude older than the minimum: run --once refuses to start", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claudeVersion = "2.0.0 (Claude Code)"
		if code := f.fire(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if !strings.Contains(f.stderr.String(), "auto-agent: run: claude cli: FATAL — claude is 2.0.0") || !strings.Contains(f.stderr.String(), "refusing to start") || f.stdout.Len() != 0 {
			t.Errorf("stdout %q, stderr %q", f.stdout.String(), f.stderr.String())
		}
		if len(f.gh.Writes()) != 0 || len(f.claude.Calls()) != 0 || len(f.records()) != 0 {
			t.Errorf("the Fire started: writes %+v, %d Step calls, %d records", f.gh.Writes(), len(f.claude.Calls()), len(f.records()))
		}
		if _, err := os.Stat(filepath.Join(f.state, "fire.lock")); err == nil {
			t.Error("the Fire took its lock")
		}
	})
	t.Run("claude logged out, or logged in another way, at the start: run --once refuses to start", func(t *testing.T) {
		for _, status := range []string{`{"loggedIn":false}`, liveAs("login")} {
			f := newFireFixture(t, labelsConfig)
			f.authStatus, f.asIsAtStart = status, true
			if code := f.fire(); code != 1 || !strings.Contains(f.stderr.String(), "claude login: FATAL") || len(f.records()) != 0 || len(f.gh.Writes()) != 0 {
				t.Errorf("%s: exit %d, stderr %q, %d records", status, code, f.stderr.String(), len(f.records()))
			}
		}
	})
	t.Run("warnings only: run --once starts, and says them", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.queue = []githubtest.Reply{queueReply()}
		f.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, []map[string]string{{"name": "AFK"}}))
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if !strings.Contains(f.stderr.String(), "auto-agent: run: labels: WARNING — ") || strings.Contains(f.stderr.String(), "refusing") || len(f.records()) != 1 {
			t.Errorf("stderr %q, %d records", f.stderr.String(), len(f.records()))
		}
	})
	t.Run("a Surface the Harness config declares is checked before the Fire, from the checkout or else the default branch", func(t *testing.T) {
		f := newFireFixture(t, electronConfig)
		if code := f.run("run", "--once", f.dir); code != 1 || !strings.Contains(f.stderr.String(), "surface app: display: FATAL") {
			t.Errorf("exit %d, stderr %q", code, f.stderr.String())
		}
		// With no checkout, the default branch's config is the one the
		// Fire reads, and so the one checked: the same finding refuses.
		f.stderr.Reset()
		if code := f.fire(); code != 1 || !strings.Contains(f.stderr.String(), "surface app: display: FATAL") || len(f.records()) != 0 {
			t.Errorf("with no checkout: exit %d, stderr %q, %d records", code, f.stderr.String(), len(f.records()))
		}
		// A Surface it does not declare is not checked.
		g := newFireFixture(t, labelsConfig)
		g.queue = []githubtest.Reply{queueReply()}
		if code := g.fire(); code != 0 || strings.Contains(g.stderr.String(), "surface") {
			t.Errorf("an undeclared Surface: exit %d, stderr %q", code, g.stderr.String())
		}
	})
	t.Run("CLAUDE_AUTH_MODE the Host config file declares is the one checked before the Fire", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		delete(f.environ, "CLAUDE_AUTH_MODE")
		dir := filepath.Join(f.environ["HOME"], ".config", "auto-agent", "acme", "widgets")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "config.env"), []byte("CLAUDE_AUTH_MODE=login\n"), 0o644)
		f.authStatus, f.asIsAtStart = liveAs("setup-token"), true
		if code := f.fire(); code != 1 || !strings.Contains(f.stderr.String(), "claude login: FATAL") || len(f.records()) != 0 {
			t.Errorf("exit %d, stderr %q, %d records", code, f.stderr.String(), len(f.records()))
		}
	})
	t.Run("bare run is checked first too: a fatal finding refuses it, and with warnings only the always-on Daemon starts", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.claudeVersion = "2.0.0 (Claude Code)"
		if code := f.run("run", f.dir); code != 1 || !strings.Contains(f.stderr.String(), "auto-agent: run: claude cli: FATAL — claude is 2.0.0") ||
			!strings.Contains(f.stderr.String(), "refusing to start") || f.stdout.Len() != 0 || len(f.gh.Writes()) != 0 {
			t.Errorf("fatal: exit %d, stdout %q, stderr %q", code, f.stdout.String(), f.stderr.String())
		}
		g := newFireFixture(t, labelsConfig)
		g.queue = []githubtest.Reply{queueReply()}
		g.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, []map[string]string{{"name": "AFK"}}))
		g.gh.Handle("POST", "/repos/acme/widgets/labels", githubtest.JSON(201, map[string]string{}))
		if code := g.daemon(1); code != 0 || !strings.Contains(g.stderr.String(), "auto-agent: run: labels: WARNING — ") ||
			strings.Contains(g.stderr.String(), "refusing") || len(g.records()) != 1 {
			t.Errorf("warnings only: exit %d, stderr %q, %d records", code, g.stderr.String(), len(g.records()))
		}
	})
	t.Run("a dry run starts nothing, and is not checked", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.claudeVersion = "1.0.0"
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
			t.Errorf("exit %d, stderr %q", code, f.stderr.String())
		}
	})
}
