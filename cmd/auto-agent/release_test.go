package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// releaseWorkflow is the workflow that publishes a GitHub Release on a semver
// tag. Its trigger, its order and its build step are the release's contract.
const releaseWorkflow = "../../.github/workflows/release.yml"

// releaseTargets are the platforms a Release carries a binary for.
var releaseTargets = []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"}

func readReleaseWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatalf("read the release workflow: %v", err)
	}
	return string(b)
}

// block returns the lines indented under the top-level key `key:`.
func block(workflow, key string) []string {
	var lines []string
	in := false
	for _, l := range strings.Split(workflow, "\n") {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		top := !strings.HasPrefix(l, " ")
		switch {
		case top && l == key+":":
			in = true
		case top:
			in = false
		case in:
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

// stepRun returns the `run: |` script of the step named name.
func stepRun(t *testing.T, workflow, name string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "- name: "+name {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) != "run: |" {
				continue
			}
			indent := ""
			var script []string
			for _, s := range lines[j+1:] {
				if indent == "" && strings.TrimSpace(s) != "" {
					indent = s[:len(s)-len(strings.TrimLeft(s, " "))]
				}
				if strings.TrimSpace(s) != "" && !strings.HasPrefix(s, indent) {
					break
				}
				script = append(script, strings.TrimPrefix(s, indent))
			}
			return strings.Join(script, "\n")
		}
	}
	t.Fatalf("the release workflow has no step %q with a run: | script", name)
	return ""
}

func TestReleaseWorkflow(t *testing.T) {
	workflow := readReleaseWorkflow(t)

	t.Run("only a semver tag triggers it", func(t *testing.T) {
		want := []string{"push:", "tags:", "- 'v*.*.*'"}
		if got := block(workflow, "on"); !slices.Equal(got, want) {
			t.Errorf("on: is %q, want %q", got, want)
		}
	})

	t.Run("the tests run before anything is built or published", func(t *testing.T) {
		test := strings.Index(workflow, "go test ./...")
		build := strings.Index(workflow, "go build")
		publish := strings.Index(workflow, "gh release create")
		if test < 0 || build < 0 || publish < 0 {
			t.Fatalf("want go test, go build and gh release create; at %d, %d, %d", test, build, publish)
		}
		if !(test < build && build < publish) {
			t.Errorf("order is test@%d build@%d publish@%d, want test, then build, then publish", test, build, publish)
		}
		if strings.Contains(workflow, "continue-on-error") || strings.Contains(workflow, "if: always()") {
			t.Error("a failed test must stop the job; the workflow carries continue-on-error or if: always()")
		}
	})

	t.Run("the Release carries every binary and SHA256SUMS", func(t *testing.T) {
		publish := stepRun(t, workflow, "Publish")
		if !strings.Contains(publish, "dist/*") {
			t.Errorf("the Publish step attaches %q, want everything the build wrote to dist/", publish)
		}
	})

	t.Run("it uses only first-party actions", func(t *testing.T) {
		for _, l := range strings.Split(workflow, "\n") {
			l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "-"))
			if uses, ok := strings.CutPrefix(l, "uses:"); ok && !strings.HasPrefix(strings.TrimSpace(uses), "actions/") {
				t.Errorf("%q is not a first-party action", l)
			}
		}
	})
}

// The Build step is run as the workflow runs it, for a made-up tag: four
// binaries that carry the tag and commit, and a SHA256SUMS that vouches for
// each of them.
func TestReleaseBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles four binaries")
	}
	script := stepRun(t, readReleaseWorkflow(t), "Build")
	// A copy of the module, so the step's dist/ never lands in the checkout.
	src := t.TempDir()
	for _, sub := range []string{"cmd", "internal"} {
		if err := os.CopyFS(filepath.Join(src, sub), os.DirFS(filepath.Join("../..", sub))); err != nil {
			t.Fatalf("copy the module: %v", err)
		}
	}
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), gomod, 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("bash", "-euo", "pipefail", "-c", script)
	build.Dir = src
	build.Env = append(os.Environ(), "GITHUB_REF_NAME=v9.8.7", "GITHUB_SHA=0123456789abcdef0123456789abcdef01234567")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the Build step: %v\n%s", err, out)
	}

	dist := filepath.Join(src, "dist")
	var want []string
	for _, target := range releaseTargets {
		want = append(want, "auto-agent-v9.8.7-"+target)
	}
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatalf("read dist: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if wantAll := append(slices.Clone(want), "SHA256SUMS"); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(wantAll))) {
		t.Fatalf("dist holds %q, want %q", got, wantAll)
	}

	sums := map[string]string{}
	f, err := os.Open(filepath.Join(dist, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		sum, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			t.Fatalf("SHA256SUMS line %q is not `<sum>  <file>`", sc.Text())
		}
		sums[name] = sum
	}
	for _, name := range want {
		b, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		if sums[name] != hex.EncodeToString(h[:]) {
			t.Errorf("SHA256SUMS says %s is %q, it is %x", name, sums[name], h)
		}
	}

	native := filepath.Join(dist, "auto-agent-v9.8.7-"+runtime.GOOS+"-"+runtime.GOARCH)
	if _, err := os.Stat(native); err != nil {
		t.Skipf("no release binary for this platform to run: %v", err)
	}
	out, err := exec.Command(native, "version").Output()
	if err != nil {
		t.Fatalf("auto-agent version: %v", err)
	}
	if want := "auto-agent v9.8.7 (commit 0123456789ab)\n"; string(out) != want {
		t.Errorf("the released binary's version printed %q, want %q", out, want)
	}
}

// A released binary is built from the standard library alone: the module
// requires nothing.
func TestStandardLibraryOnly(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(gomod), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "require") {
			t.Errorf("go.mod requires a module: %q", l)
		}
	}
}
