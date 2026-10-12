package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/version"
)

const (
	labelsConfig  = `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}}}`
	projectConfig = `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"project":{"number":1,"priority_field":"Urgency"}}}`
)

// fixture is a Target Project checkout, a fake GitHub that knows the repo
// and the login, and the Env that points a command at both.
type fixture struct {
	dir            string
	gh             *githubtest.Server
	stdout, stderr bytes.Buffer
	environ        map[string]string
	origin         string
	originErr      error
	ghToken        string
}

func newFixture(t *testing.T, harness string) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), gh: githubtest.New(t), origin: "git@github.com:acme/widgets.git"}
	if err := os.MkdirAll(filepath.Join(f.dir, ".auto-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, ".auto-agent", "harness.json"), []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	f.environ = map[string]string{"GH_TOKEN": "t0ken", "HOME": t.TempDir(), EnvGitHubAPI: f.gh.URL}
	f.gh.Handle("GET", "/repos/acme/widgets", githubtest.JSON(200, map[string]string{"default_branch": "trunk"}))
	f.gh.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "machine-user"}))
	f.gh.Handle("GET", "/repos/acme/widgets/issues", githubtest.Text(200, `[]`))
	return f
}

func (f *fixture) run(args ...string) int {
	return Run(args, Env{
		Build:  version.Info{Version: "v0.0.0-test"},
		Stdout: &f.stdout, Stderr: &f.stderr,
		LookupEnv:   func(k string) (string, bool) { v, ok := f.environ[k]; return v, ok },
		GHAuthToken: func() (string, error) { return f.ghToken, nil },
		OriginURL:   func(string) (string, error) { return f.origin, f.originErr },
	})
}

func (f *fixture) report(t *testing.T) map[string]any {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal(f.stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s\nstderr: %s", err, f.stdout.String(), f.stderr.String())
	}
	return report
}

func queueReply(nodes ...string) githubtest.Reply {
	return githubtest.Text(200, `{"data":{"repository":{"issues":{"nodes":[`+strings.Join(nodes, ",")+`]}}}}`)
}

const cleanSlice = `{"number":20,"title":"Tracer","createdAt":"2026-02-01T00:00:00Z","labels":{"nodes":[{"name":"AFK"}]},
	"blockedBy":{"nodes":[],"pageInfo":{"hasNextPage":false}},"assignees":{"nodes":[]}}`

func TestRunOnceDryRun(t *testing.T) {
	t.Run("run --once --dry-run prints the state and next action as JSON and makes no write request", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply(cleanSlice))
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		report := f.report(t)
		next, _ := report["next"].(map[string]any)
		pick, _ := report["pick"].(map[string]any)
		if report["state"] != "pick" || report["repo"] != "acme/widgets" || report["login"] != "machine-user" || report["pickShape"] != "labels" ||
			next["action"] != "implement" || next["issue"] != float64(20) || pick["title"] != "Tracer" {
			t.Errorf("report = %s", f.stdout.String())
		}
		if writes := f.gh.Writes(); len(writes) != 0 {
			t.Errorf("the dry run made write requests: %+v", writes)
		}
		for _, r := range f.gh.Requests() {
			if r.Header.Get("Authorization") != "Bearer t0ken" || r.Header.Get("User-Agent") != "auto-agent/v0.0.0-test" {
				t.Errorf("%s %s headers = %v", r.Method, r.Path, r.Header)
			}
		}
	})
	for shape, harness := range map[string]string{"project": projectConfig, "labels": labelsConfig} {
		t.Run(shape+" pick: AFK:in-progress held: verdict in-flight, no pick query", func(t *testing.T) {
			f := newFixture(t, harness)
			f.gh.Handle("GET", "/repos/acme/widgets/issues", githubtest.Text(200, `[{"number":7},{"number":9}]`))
			if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
				t.Fatalf("exit %d: %s", code, f.stderr.String())
			}
			report := f.report(t)
			if report["state"] != "in-flight" || report["inFlight"] != float64(2) || report["pick"] != nil || report["pickShape"] != shape {
				t.Errorf("report = %s", f.stdout.String())
			}
			for _, r := range f.gh.Requests() {
				if r.Path == "/graphql" {
					t.Error("the pick query was sent although the lock is held")
				}
				if r.Path == "/repos/acme/widgets/issues" && r.Query.Get("labels") != "AFK:in-progress" {
					t.Errorf("lock read asked for labels %q", r.Query.Get("labels"))
				}
			}
		})
	}
	t.Run("flags may follow the target dir, and --help prints the usage", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply(cleanSlice))
		if code := f.run("run", f.dir, "--once", "--dry-run"); code != 0 || f.report(t)["state"] != "pick" {
			t.Errorf("exit %d: %s%s", code, f.stdout.String(), f.stderr.String())
		}
		f.stdout.Reset()
		if code := f.run("run", "--help"); code != 0 || !strings.Contains(f.stdout.String(), "Usage:") {
			t.Errorf("--help: exit %d, stdout %q", code, f.stdout.String())
		}
	})
	t.Run("a ticket GitHub returned only part of is skipped, never picked", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", githubtest.Text(200, `{"data":{"repository":{"issues":{"nodes":[
			{"number":10,"title":"partial","createdAt":"2026-01-01T00:00:00Z","labels":{"nodes":[{"name":"AFK"}]},"blockedBy":null,"assignees":{"nodes":[]}},`+cleanSlice+`
		]}}},"errors":[{"message":"blockedBy timed out"}]}`))
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		report := f.report(t)
		skipped, _ := report["skipped"].([]any)
		pick, _ := report["pick"].(map[string]any)
		if pick["issue"] != float64(20) || len(skipped) != 1 || !strings.Contains(f.stdout.String(), "GitHub did not return its blockers") {
			t.Errorf("report = %s", f.stdout.String())
		}
	})
	t.Run("a board pick takes the highest priority on the configured board, through the whole dry run", func(t *testing.T) {
		node := func(n int, created, priority string) string {
			return fmt.Sprintf(`{"number":%d,"title":"t%d","createdAt":"%sT00:00:00Z","labels":{"nodes":[{"name":"AFK"}]},
				"blockedBy":{"nodes":[],"pageInfo":{"hasNextPage":false}},"assignees":{"nodes":[]},
				"projectItems":{"nodes":[{"project":{"number":1},"fieldValueByName":{"name":%q}}]}}`, n, n, created, priority)
		}
		f := newFixture(t, projectConfig)
		f.gh.HandleGraphQL("projectItems", queueReply(node(10, "2026-01-01", "P2"), node(20, "2026-03-01", "P0"), node(30, "2026-02-01", "P0")))
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		pick, _ := f.report(t)["pick"].(map[string]any)
		if pick["issue"] != float64(30) || pick["priority"] != "P0" {
			t.Errorf("report = %s", f.stdout.String())
		}
	})
	t.Run("unreadable lock: fails safe to in-flight", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.Handle("GET", "/repos/acme/widgets/issues", githubtest.Text(500, "boom"))
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		report := f.report(t)
		warnings, _ := report["warnings"].([]any)
		if report["state"] != "in-flight" || len(warnings) != 1 || !strings.Contains(warnings[0].(string), "could not be read") {
			t.Errorf("report = %s", f.stdout.String())
		}
	})
	t.Run("the configured priority field is queried", func(t *testing.T) {
		f := newFixture(t, projectConfig)
		f.gh.HandleGraphQL("projectItems", queueReply())
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		var sent string
		for _, r := range f.gh.Requests() {
			if r.Path == "/graphql" {
				sent = r.Body
			}
		}
		report := f.report(t)
		if !strings.Contains(sent, `"field":"Urgency"`) || report["state"] != "idle" || report["pickShape"] != "project" {
			t.Errorf("sent = %s\nreport = %s", sent, f.stdout.String())
		}
	})
	t.Run("partial GraphQL data never aborts: the warning is reported beside the state", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", githubtest.Text(200,
			`{"data":{"repository":{"issues":{"nodes":[`+cleanSlice+`]}}},"errors":[{"message":"Something went wrong"}]}`))
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		report := f.report(t)
		warnings, _ := report["warnings"].([]any)
		if report["state"] != "pick" || len(warnings) != 1 || warnings[0] != "Something went wrong" {
			t.Errorf("report = %s", f.stdout.String())
		}
	})
	t.Run("the repo comes from --repo, and from the origin remote without it", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		f.originErr = errors.New("not a git repository")
		if code := f.run("run", "--once", "--dry-run", "--repo", "acme/widgets", f.dir); code != 0 {
			t.Fatalf("with --repo: exit %d: %s", code, f.stderr.String())
		}
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 1 || !strings.Contains(f.stderr.String(), "not a git repository") {
			t.Errorf("without --repo or an origin: exit %d: %s", code, f.stderr.String())
		}
	})
}

func TestRunRefusals(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		prepare  func(*fixture)
		wantCode int
		want     string
	}{
		{
			name:     "run without --once --dry-run is refused: this build only derives",
			args:     []string{"run", "--once"},
			wantCode: 2, want: "use run --once --dry-run",
		},
		{
			name:     "an unknown flag exits non-zero with usage",
			args:     []string{"run", "--once", "--dry-run", "--force"},
			wantCode: 2, want: "Usage:",
		},
		{
			name:     "two target dirs are one too many",
			args:     []string{"run", "--once", "--dry-run", "a", "b"},
			wantCode: 2, want: "at most one target dir",
		},
		{
			name:     "no token anywhere fails naming both sources",
			args:     []string{"run", "--once", "--dry-run", "--repo", "acme/widgets"},
			prepare:  func(f *fixture) { delete(f.environ, "GH_TOKEN") },
			wantCode: 1, want: "GH_TOKEN is not set and `gh auth token` gave none",
		},
		{
			name: "an invalid Harness config fails naming the key, before the queue is read",
			args: []string{"run", "--once", "--dry-run", "--repo", "acme/widgets"},
			prepare: func(f *fixture) {
				os.WriteFile(filepath.Join(f.dir, ".auto-agent", "harness.json"), []byte(`{"pick":{}}`), 0o644)
			},
			wantCode: 1, want: `missing required key "commands"`,
		},
		{
			name:     "a queue GitHub will not give is a failure, not an idle state",
			args:     []string{"run", "--once", "--dry-run", "--repo", "acme/widgets"},
			prepare:  func(f *fixture) { f.gh.Handle("POST", "/graphql", githubtest.Text(502, "bad gateway")) },
			wantCode: 1, want: "502",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, labelsConfig)
			if tt.prepare != nil {
				tt.prepare(f)
			}
			args := tt.args
			if tt.wantCode == 1 {
				args = append(args, f.dir)
			}
			code := f.run(args...)
			if code != tt.wantCode || !strings.Contains(f.stderr.String(), tt.want) || f.stdout.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and %q", code, f.stdout.String(), f.stderr.String(), tt.wantCode, tt.want)
			}
			if len(f.gh.Writes()) != 0 {
				t.Errorf("write requests were made: %+v", f.gh.Writes())
			}
		})
	}
}
