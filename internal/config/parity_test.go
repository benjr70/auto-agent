package config

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The bash loader is the oracle until the cut-over (ADR 0014): these tests
// run lib/harness-config.sh on the same input and compare what it resolves,
// or the errors it names, with what this package does.

const repoRoot = "../.."

// bashLoader runs one function of lib/harness-config.sh with git and gh
// stubbed to answer for testRepo. It returns stdout, stderr and the exit code.
func bashLoader(t *testing.T, function string, args ...string) (string, string, int) {
	t.Helper()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is not installed: the bash loader cannot be asked, and it is the oracle", bin)
		}
	}
	stubs := t.TempDir()
	writeStub := func(name, body string) string {
		path := filepath.Join(stubs, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	lib, err := filepath.Abs(filepath.Join(repoRoot, "lib", "harness-config.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"-c", `. "$1"; fn="$2"; shift 2; "$fn" "$@"`, "bash", lib, function}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_BIN="+writeStub("git", "echo https://github.com/"+testRepo.Slug()+".git"),
		"GH_BIN="+writeStub("gh", "echo "+testRepo.DefaultBranch),
		"HARNESS_CONFIG_JSON=", "AUTO_AGENT_TARGET_DIR=", "HARNESS_SCHEMA_DIR=",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func TestEveryFixtureResolvesAsTheBashLoaderDoes(t *testing.T) {
	const base = `"commit_scopes":["a"],"commands":{"install":"x","test":"y"}`
	inline := map[string]string{
		"minimal Project pick":   validMin,
		"declared Project order": `{` + base + `,"pick":{"project":{"number":3,"priority_field":"Urgency","order":["now","later"]}}}`,
		"dependabot default":     `{` + base + `,"pick":{"labels":{}},"dependabot":{}}`,
		"dependabot off":         `{` + base + `,"pick":{"labels":{}},"dependabot":{"enabled":false}}`,
		"surfaces":               `{` + base + `,"pick":{"labels":{}},"surfaces":{"web":{"kind":"browser","url_key":"W","paths":["a"]},"app":{"kind":"electron","url_key":"A","paths":["b"],"launcher":"bin/app"}}}`,
		"every optional key": `{"$schema":"x","commit_scopes":["a","b.c"],"commands":{"install":"i","test":"t","lint":"l","lockfile_refresh":"r","plan_gated_paths":["infra/**"]},` +
			`"pick":{"labels":{}},"docs_research_prefix":"notes/","required_checks":["ci"],"rounds":{"pr_watch":4,"revise":2},` +
			`"verification":{"hermetic":{"command":"p"},"deployed":{"command":"d"}},"dependabot":{"enabled":true},"host":{"docker":true},` +
			`"workspace":{"keep":["node_modules/"],"prepare":"make deps"}}`,
	}
	targets := map[string]string{}
	for _, dir := range []string{".", "plugin/fixtures/target-project", "plugin/providers/compose"} {
		abs, err := filepath.Abs(filepath.Join(repoRoot, dir))
		if err != nil {
			t.Fatal(err)
		}
		targets[dir] = abs
	}
	for name, content := range inline {
		targets[name] = makeTarget(t, content)
	}
	for name, dir := range targets {
		t.Run(name, func(t *testing.T) {
			got, err := LoadHarness(dir, "", testRepo)
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := bashLoader(t, "harness_config_load", dir)
			if code != 0 {
				t.Fatalf("the bash loader exited %d: %s", code, stderr)
			}
			var want Harness
			if err := json.Unmarshal([]byte(stdout), &want); err != nil {
				t.Fatalf("the bash loader's output: %v", err)
			}
			// Unmarshalling drops what the Go value does not carry: repo.slug
			// (Repo.Slug), pick.labels (always empty) and host.extension (the
			// Host extension is removed).
			if !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("resolved config differs from the bash loader's\n go: %s\nbash: %s", g, w)
			}
		})
	}
}

func TestEveryRejectionNamesWhatTheBashLoaderNames(t *testing.T) {
	const base = `"commit_scopes":["a"],"commands":{"install":"x","test":"y"}`
	for name, content := range map[string]string{
		"missing block":     `{"commit_scopes":["a"],"pick":{"labels":{}}}`,
		"nearly empty":      `{"pick":{"labels":{}}}`,
		"not an object":     `[]`,
		"unknown key":       `{` + base + `,"pick":{"labels":{}},"model":"opus"}`,
		"both pick shapes":  `{` + base + `,"pick":{"labels":{},"project":{"number":1}}}`,
		"no pick shape":     `{` + base + `,"pick":{}}`,
		"bad pick project":  `{` + base + `,"pick":{"project":{"number":0,"order":[]}}}`,
		"round caps":        `{` + base + `,"pick":{"labels":{}},"rounds":{"poll":5,"revise":0,"pr_watch":1.5}}`,
		"bad surface":       `{` + base + `,"pick":{"labels":{}},"surfaces":{"ui":{"kind":"tv","url_key":"url","paths":[]},"Bad Name":{"kind":"api","url_key":"U","paths":["a"],"viewport":"big"}}}`,
		"bad verification":  `{` + base + `,"pick":{"labels":{}},"verification":{"hermetic":{"smoke":true},"deployed":{"command":"c","enabled":"yes"}}}`,
		"bad lane block":    `{` + base + `,"pick":{"labels":{}},"dependabot":{"enabled":false,"login":"x"}}`,
		"bad scopes":        `{"commit_scopes":["Bad Scope",""],"commands":{"install":"","test":7},"pick":{"labels":{}}}`,
		"empty scopes":      `{"commit_scopes":[],"commands":{"install":"x","test":"y"},"pick":{"labels":{"priority":["P0"]}}}`,
		"wrong types":       `{"commit_scopes":"a","commands":[],"pick":null,"required_checks":{},"host":{"docker":"yes"}}`,
		"electron launcher": `{` + base + `,"pick":{"labels":{}},"surfaces":{"app":{"kind":"electron","url_key":"APP_URL","paths":["src/**"]}}}`,
		"deployed alone":    `{` + base + `,"pick":{"labels":{}},"verification":{"deployed":{"command":"c"}}}`,
		"duplicate url_key": `{` + base + `,"pick":{"labels":{}},"surfaces":{"a":{"kind":"api","url_key":"U","paths":["x"]},"b":{"kind":"cli","url_key":"U","paths":["y"]}}}`,
		"empty prefix":      `{` + base + `,"pick":{"labels":{}},"docs_research_prefix":""}`,
		"bad keep":          `{` + base + `,"pick":{"labels":{}},"workspace":{"keep":["node_modules/","!x","**/"]}}`,
		"quotes and markup": `{"commit_scopes":["a\"b<"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"k\"<&":1,"surfaces":{"s":{"kind":"<b>","url_key":"U","paths":["a"]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := makeTarget(t, content)
			_, err := LoadHarness(dir, "", testRepo)
			var invalid *InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("LoadHarness error = %v, want an *InvalidError", err)
			}
			_, stderr, code := bashLoader(t, "harness_config_check", dir)
			if code != 1 {
				t.Fatalf("the bash loader exited %d, want 1: %s", code, stderr)
			}
			var want []string
			for _, line := range strings.Split(stderr, "\n") {
				if strings.HasPrefix(line, "  ") {
					want = append(want, strings.TrimPrefix(line, "  "))
				}
			}
			// Sorted: the two walk a schema's keys in different orders.
			got := append([]string(nil), invalid.Problems...)
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("problems differ from the bash loader's\n go: %q\nbash: %q", got, want)
			}
		})
	}
}
