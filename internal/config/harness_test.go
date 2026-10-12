package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The case names below are the ones lib/harness-config.test.sh uses.

const (
	fixtureDir = "../../plugin/fixtures/target-project"
	validMin   = `{"commit_scopes":["core"],"commands":{"install":"npm ci","test":"npm test"},"pick":{"project":{"number":7}}}`
)

var testRepo = Repo{Owner: "acme", Name: "widgets", DefaultBranch: "trunk"}

// makeTarget is a throwaway Target Project: the fixture's verifier runbook
// beside the given harness.json (the fixture's own when content is empty).
func makeTarget(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, HarnessDirName)
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(fixtureDir, HarnessDirName, "verifier-runbook.md"), filepath.Join(cfg, "verifier-runbook.md"))
	if content == "" {
		copyFile(t, filepath.Join(fixtureDir, HarnessDirName, HarnessFileName), filepath.Join(cfg, HarnessFileName))
	} else {
		writeFile(t, filepath.Join(cfg, HarnessFileName), content+"\n")
	}
	return dir
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, to, string(data))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadHarnessRejects(t *testing.T) {
	const base = `"commit_scopes":["a"],"commands":{"install":"x","test":"y"}`
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "missing required block fails closed naming the key",
			content: `{"commit_scopes":["a"],"pick":{"labels":{}}}`,
			want:    []string{`/: missing required key "commands"`},
		},
		{
			name:    "unknown top-level key is rejected",
			content: `{` + base + `,"pick":{"labels":{}},"model":"opus"}`,
			want:    []string{`/: unknown key "model"`},
		},
		{
			name:    "a declared default branch is rejected (detected, never read)",
			content: `{` + base + `,"pick":{"labels":{}},"default_branch":"main"}`,
			want:    []string{`/: unknown key "default_branch"`},
		},
		{
			name:    "pick with both shapes is rejected",
			content: `{` + base + `,"pick":{"labels":{},"project":{"number":1}}}`,
			want:    []string{`/pick: matches more than one shape; declare exactly one of project | labels`},
		},
		{
			name:    "pick with neither shape is rejected",
			content: `{` + base + `,"pick":{}}`,
			want:    []string{`/pick: matches none of the allowed shapes (project | labels)`},
		},
		{
			name:    "a sixth round cap is rejected; a zero cap is rejected",
			content: `{` + base + `,"pick":{"labels":{}},"rounds":{"poll":5,"revise":0}}`,
			want:    []string{`/rounds: unknown key "poll"`, `/rounds/revise: must be >= 1`},
		},
		{
			name:    "surface with a bad kind, lowercase url_key and no paths lists every error",
			content: `{` + base + `,"pick":{"labels":{}},"surfaces":{"ui":{"kind":"tv","url_key":"url","paths":[]}}}`,
			want: []string{
				`/surfaces/ui/kind: must be one of "browser", "electron", "cli", "api", got "tv"`,
				`/surfaces/ui/url_key: "url" does not match ^[A-Z][A-Z0-9_]*$`,
				`/surfaces/ui/paths: needs at least 1 item(s)`,
			},
		},
		{
			name:    "hermetic block without a command is rejected; deployed enabled must be boolean",
			content: `{` + base + `,"pick":{"labels":{}},"verification":{"hermetic":{"smoke":true},"deployed":{"command":"c","enabled":"yes"}}}`,
			want: []string{
				`/verification/hermetic: missing required key "command"`,
				`/verification/deployed/enabled: expected boolean, got string`,
			},
		},
		{
			name:    "optional lane blocks accept enabled and nothing else",
			content: `{` + base + `,"pick":{"labels":{}},"dependabot":{"enabled":false,"login":"x"}}`,
			want:    []string{`/dependabot: unknown key "login"`},
		},
		{
			name:    "labels pick rejects keys of its own (no configurable label vocabulary)",
			content: `{` + base + `,"pick":{"labels":{"priority":["P0"]}}}`,
			want:    []string{`/pick/labels: unknown key "priority"`},
		},
		{
			name:    "electron surface without a launcher fails the cross-checks",
			content: `{` + base + `,"pick":{"labels":{}},"surfaces":{"app":{"kind":"electron","url_key":"APP_URL","paths":["src/**"]}}}`,
			want:    []string{`/surfaces/app: an electron surface needs a launcher`},
		},
		{
			name:    "deployed tier without a hermetic tier fails the cross-checks",
			content: `{` + base + `,"pick":{"labels":{}},"verification":{"deployed":{"command":"c"}}}`,
			want:    []string{`/verification/deployed: a deployed tier needs a hermetic tier to defer from`},
		},
		{
			name:    "two surfaces declaring one url_key fail the cross-checks",
			content: `{` + base + `,"pick":{"labels":{}},"surfaces":{"a":{"kind":"api","url_key":"U","paths":["x"]},"b":{"kind":"cli","url_key":"U","paths":["y"]}}}`,
			want:    []string{`/surfaces: url_key "U" is declared by more than one surface`},
		},
		{
			name:    "invalid file fails before any git or gh call",
			content: `{"pick":{"labels":{}}}`,
			want:    []string{`/: missing required key "commit_scopes"`, `/: missing required key "commands"`},
		},
		{
			name:    "a config that is not an object names the root",
			content: `[]`,
			want:    []string{`/: expected object, got array`},
		},
		{
			name:    "a round cap too large to hold is rejected, not crashed on",
			content: `{` + base + `,"pick":{"labels":{}},"rounds":{"revise":99999999999999999999}}`,
			want:    []string{`/rounds/revise: is too large a number`},
		},
		{
			name:    "a fractional round cap is not an integer",
			content: `{` + base + `,"pick":{"labels":{}},"rounds":{"revise":1.5}}`,
			want:    []string{`/rounds/revise: expected integer, got number`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := makeTarget(t, tt.content)
			_, err := LoadHarness(dir, "", testRepo)
			var invalid *InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("LoadHarness error = %v, want an *InvalidError", err)
			}
			for _, want := range tt.want {
				found := false
				for _, p := range invalid.Problems {
					if p == want {
						found = true
					}
				}
				if !found {
					t.Errorf("problems = %q, want them to include %q", invalid.Problems, want)
				}
			}
			if !strings.Contains(err.Error(), tt.want[0]) {
				t.Errorf("error text %q does not name %q", err.Error(), tt.want[0])
			}
		})
	}
}

func TestLoadHarnessUnreadable(t *testing.T) {
	t.Run("missing file exits 2 with a message", func(t *testing.T) {
		dir := makeTarget(t, "")
		os.Remove(filepath.Join(dir, HarnessDirName, HarnessFileName))
		_, err := LoadHarness(dir, "", testRepo)
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "missing") {
			t.Errorf("error = %v, want a missing-file error", err)
		}
	})
	t.Run("non-JSON file exits 2", func(t *testing.T) {
		dir := makeTarget(t, "not json")
		_, err := LoadHarness(dir, "", testRepo)
		if err == nil || !strings.Contains(err.Error(), "is not valid JSON") {
			t.Errorf("error = %v, want a not-valid-JSON error", err)
		}
		var invalid *InvalidError
		if errors.As(err, &invalid) {
			t.Errorf("a non-JSON file is not a schema failure: %v", err)
		}
	})
	t.Run("a target dir that does not exist is named", func(t *testing.T) {
		_, err := LoadHarness(filepath.Join(t.TempDir(), "nope"), "", testRepo)
		if err == nil || !strings.Contains(err.Error(), "target dir not found") {
			t.Errorf("error = %v, want target dir not found", err)
		}
	})
}

func TestLoadHarnessFixture(t *testing.T) {
	dir := makeTarget(t, "")
	h, err := LoadHarness(dir, "", testRepo)
	if err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(dir, HarnessDirName)

	t.Run("fixture config loads to the documented resolved shape", func(t *testing.T) {
		if h.ConfigDir != cfgDir || h.TargetDir() != dir {
			t.Errorf("ConfigDir = %q, TargetDir = %q; want %q and %q", h.ConfigDir, h.TargetDir(), cfgDir, dir)
		}
		if want := []string{"app", "verify", "ci"}; !reflect.DeepEqual(h.CommitScopes, want) {
			t.Errorf("CommitScopes = %q, want %q", h.CommitScopes, want)
		}
	})
	t.Run("fixture harness.json validates", func(t *testing.T) {
		if _, err := LoadHarness(fixtureDir, "", testRepo); err != nil {
			t.Error(err)
		}
	})
	t.Run("harness_config_target_dir is the parent of config_dir", func(t *testing.T) {
		if got := (Harness{ConfigDir: "/srv/target/.auto-agent"}).TargetDir(); got != "/srv/target" {
			t.Errorf("TargetDir = %q", got)
		}
	})
	t.Run("the repo the caller supplies is carried through, with its slug", func(t *testing.T) {
		if h.Repo != testRepo || h.Repo.Slug() != "acme/widgets" {
			t.Errorf("Repo = %+v (slug %q)", h.Repo, h.Repo.Slug())
		}
	})
	t.Run("label-only pick resolves to the labels shape", func(t *testing.T) {
		if h.Pick.Shape != PickLabels || h.Pick.Project != nil {
			t.Errorf("Pick = %+v", h.Pick)
		}
	})
	t.Run("hermetic and deployed tiers resolve; deployed lane present but disabled; deps-land absent", func(t *testing.T) {
		wantV := Verification{
			Hermetic: &Hermetic{Command: "verify/provider", Smoke: true},
			Deployed: &Deployed{Command: "verify/provider", Enabled: false},
		}
		wantL := Lanes{Deployed: Lane{Present: true, Enabled: false}}
		if !reflect.DeepEqual(h.Verification, wantV) || h.Lanes != wantL {
			t.Errorf("Verification = %+v, Lanes = %+v", h.Verification, h.Lanes)
		}
	})
	t.Run("surfaces keep their viewport and get null launcher; api surface gets no viewport", func(t *testing.T) {
		web, api := h.Surfaces["web"], h.Surfaces["api"]
		if web.Viewport != "1024x768" || web.Launcher != "" || api.Viewport != "" || len(h.Surfaces) != 2 {
			t.Errorf("Surfaces = %+v", h.Surfaces)
		}
	})
	t.Run("prose siblings resolve to absolute paths or null; host extension null when absent", func(t *testing.T) {
		want := Prose{VerifierRunbook: filepath.Join(cfgDir, "verifier-runbook.md")}
		if h.Prose != want || h.Host.Docker {
			t.Errorf("Prose = %+v, Host = %+v", h.Prose, h.Host)
		}
	})
	t.Run("fixture round caps and commands pass through", func(t *testing.T) {
		wantR := Rounds{PRWatch: 10, ManualVerify: 3, Revise: 3, DepsFix: 3, PauseResume: 3}
		wantC := Commands{Install: "true", Test: "python3 -m unittest discover -s app", Lint: "python3 -m compileall -q app", PlanGatedPaths: []string{}}
		if h.Rounds != wantR || !reflect.DeepEqual(h.Commands, wantC) || h.DocsResearchPrefix != "docs/findings/" || len(h.RequiredChecks) != 0 {
			t.Errorf("Rounds = %+v, Commands = %+v, prefix = %q, checks = %q", h.Rounds, h.Commands, h.DocsResearchPrefix, h.RequiredChecks)
		}
	})
}

func TestLoadHarnessResolves(t *testing.T) {
	const base = `"commit_scopes":["a"],"commands":{"install":"x","test":"y"}`
	load := func(t *testing.T, content string) Harness {
		t.Helper()
		h, err := LoadHarness(makeTarget(t, content), "", testRepo)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	t.Run("Project pick shape resolves with defaults; minimal config gets every default", func(t *testing.T) {
		h := load(t, validMin)
		wantPick := Pick{Shape: PickProject, Project: &Project{Number: 7, PriorityField: "Priority", Order: []string{"P0", "P1", "P2"}}}
		wantRounds := Rounds{PRWatch: 10, ManualVerify: 3, Revise: 3, DepsFix: 3, PauseResume: 3}
		if !reflect.DeepEqual(h.Pick, wantPick) || h.Rounds != wantRounds {
			t.Errorf("Pick = %+v (%+v), Rounds = %+v", h.Pick, h.Pick.Project, h.Rounds)
		}
		if h.Verification.Hermetic != nil || h.Verification.Deployed != nil || len(h.Surfaces) != 0 || h.Lanes != (Lanes{}) {
			t.Errorf("Verification = %+v, Surfaces = %+v, Lanes = %+v", h.Verification, h.Surfaces, h.Lanes)
		}
		if h.DocsResearchPrefix != "docs/research/" || h.RequiredChecks == nil || h.Host.Docker {
			t.Errorf("prefix = %q, checks = %#v, host = %+v", h.DocsResearchPrefix, h.RequiredChecks, h.Host)
		}
	})
	t.Run("Project pick keeps a declared priority field and order", func(t *testing.T) {
		h := load(t, `{`+base+`,"pick":{"project":{"number":3,"priority_field":"Urgency","order":["now","later"]}}}`)
		want := &Project{Number: 3, PriorityField: "Urgency", Order: []string{"now", "later"}}
		if !reflect.DeepEqual(h.Pick.Project, want) {
			t.Errorf("Project = %+v", h.Pick.Project)
		}
	})
	t.Run("dependabot block present with default enabled turns the deps-land lane on", func(t *testing.T) {
		h := load(t, `{`+base+`,"pick":{"labels":{}},"dependabot":{}}`)
		if want := (Lane{Present: true, Enabled: true}); h.Lanes.DepsLand != want {
			t.Errorf("DepsLand = %+v", h.Lanes.DepsLand)
		}
	})
	t.Run("a dependabot block switched off is present but disabled", func(t *testing.T) {
		h := load(t, `{`+base+`,"pick":{"labels":{}},"dependabot":{"enabled":false}}`)
		if want := (Lane{Present: true}); h.Lanes.DepsLand != want {
			t.Errorf("DepsLand = %+v", h.Lanes.DepsLand)
		}
	})
	t.Run("viewport is null when not declared; launcher passes through", func(t *testing.T) {
		h := load(t, `{`+base+`,"pick":{"labels":{}},"surfaces":{"web":{"kind":"browser","url_key":"W","paths":["a"]},"app":{"kind":"electron","url_key":"A","paths":["b"],"launcher":"bin/app"}}}`)
		if h.Surfaces["web"].Viewport != "" || h.Surfaces["app"].Viewport != "" || h.Surfaces["app"].Launcher != "bin/app" {
			t.Errorf("Surfaces = %+v", h.Surfaces)
		}
	})
	t.Run("a whole number written as 3.0 or 1e2 is an integer, as it is to the bash loader", func(t *testing.T) {
		h := load(t, `{`+base+`,"pick":{"project":{"number":1e2}},"rounds":{"pr_watch":3.0}}`)
		if h.Pick.Project.Number != 100 || h.Rounds.PRWatch != 3 {
			t.Errorf("Number = %d, PRWatch = %d", h.Pick.Project.Number, h.Rounds.PRWatch)
		}
	})
	t.Run("a declared false survives where the default is true", func(t *testing.T) {
		h := load(t, `{`+base+`,"pick":{"labels":{}},"verification":{"hermetic":{"command":"p"},"deployed":{"command":"d"}},"host":{"docker":true}}`)
		if h.Verification.Hermetic.Smoke || !h.Verification.Deployed.Enabled || !h.Lanes.Deployed.Enabled || !h.Host.Docker {
			t.Errorf("Verification = %+v %+v, Lanes = %+v, Host = %+v", h.Verification.Hermetic, h.Verification.Deployed, h.Lanes, h.Host)
		}
	})
	t.Run("a file from elsewhere loads against the target's repo (PR-head seam)", func(t *testing.T) {
		dir := makeTarget(t, "")
		elsewhere := filepath.Join(t.TempDir(), "from-pr-head.json")
		writeFile(t, elsewhere, validMin)
		h, err := LoadHarness(dir, elsewhere, testRepo)
		if err != nil {
			t.Fatal(err)
		}
		if h.Pick.Shape != PickProject || h.Repo.Slug() != "acme/widgets" || h.TargetDir() != dir || h.Prose.VerifierRunbook == "" {
			t.Errorf("Pick = %+v, Repo = %+v, TargetDir = %q, Prose = %+v", h.Pick, h.Repo, h.TargetDir(), h.Prose)
		}
	})
}

func TestParseRepo(t *testing.T) {
	t.Run("ssh and bare-https origin URLs derive the same slug", func(t *testing.T) {
		for _, url := range []string{
			"https://github.com/acme/widgets.git",
			"git@github.com:acme/widgets.git",
			"ssh://git@github.com/acme/widgets",
			"https://github.com/acme/widgets",
			"https://github.com/acme/widgets/",
			"acme/widgets",
		} {
			owner, name, err := ParseRepo(url)
			if err != nil || owner != "acme" || name != "widgets" {
				t.Errorf("ParseRepo(%q) = %q, %q, %v", url, owner, name, err)
			}
		}
	})
	t.Run("no origin remote exits 3 with a message", func(t *testing.T) {
		for _, url := range []string{"", "widgets", "https://github.com/", "/widgets", "acme/", "https://github.com/../widgets", "https://github.com/acme/.."} {
			if _, _, err := ParseRepo(url); err == nil || !strings.Contains(err.Error(), "cannot derive the repo") {
				t.Errorf("ParseRepo(%q) error = %v", url, err)
			}
		}
	})
}

// The embedded schema is a copy: Go cannot embed a file outside its package,
// and the bash loader still reads the plugin's until the cut-over (ADR 0014).
func TestEmbeddedSchemaMatchesThePlugins(t *testing.T) {
	plugin, err := os.ReadFile("../../plugin/schema/harness.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(plugin) != string(harnessSchemaJSON) {
		t.Error("internal/config/harness.schema.json differs from plugin/schema/harness.schema.json; copy the plugin's over it")
	}
}
