package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/precommit"
	"github.com/benjr70/auto-agent/internal/step/steptest"
	"github.com/benjr70/auto-agent/internal/verify/providertest"
)

// prBody is the body of the pull request the Fire opened; empty when none.
func (f *fireFixture) prBody() string {
	for _, w := range f.gh.Writes() {
		if w.Path == pullsPath {
			var pr map[string]any
			json.Unmarshal([]byte(w.Body), &pr)
			body, _ := pr["body"].(string)
			return body
		}
	}
	return ""
}

// smokeOf is the smoke result in a Fire record.
func smokeOf(record map[string]any) map[string]any {
	smoke, _ := record["smoke"].(map[string]any)
	return smoke
}

type R = providertest.Reply

// The smoke, driven by Go through the Environment provider before the PR
// opens. Cases that carry over from lib/verify-boot.test.sh and
// lib/bootstrap-state.test.sh keep their names.
func TestFireSmoke(t *testing.T) {
	t.Run("the smoke runs after the review approves and before the PR, and its result is in the record and the PR body (AC 2)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		t.Setenv("GH_TOKEN", "a-secret")
		f.answer(finished)
		f.mustFire(0)
		if calls := strings.Join(f.provider.Calls(), "|"); calls != "down --all|down --all|up --pr 20|smoke|down --pr 20" {
			t.Errorf("the provider was called %s", calls)
		}
		record := f.record()
		if smoke := smokeOf(record); smoke["status"] != "PASS" || smoke["detail"] != "2/2" || record["phase"] != "done" {
			t.Errorf("record = %v", record)
		}
		if !strings.Contains(f.prBody(), "## Smoke\n\n`smoke: PASS — 2/2`\n") {
			t.Errorf("the PR body is\n%s", f.prBody())
		}
		// The provider runs the branch's code: in the clone, and with no
		// GitHub credential.
		if dir, _ := filepath.EvalSymlinks(f.provider.Dir()); dir != mustEvalDir(t, filepath.Join(f.state, "clone")) {
			t.Errorf("the provider ran in %s", f.provider.Dir())
		}
		if token, set := f.provider.SmokeEnv("GH_TOKEN"); set {
			t.Errorf("the provider got GH_TOKEN=%q", token)
		}
		if web, _ := f.provider.SmokeEnv("WEB_URL"); web != "http://127.0.0.1:9/" {
			t.Errorf("the smoke got WEB_URL=%q", web)
		}
		// Its temporary files go under the State dir, and go with the smoke.
		tmp, _ := f.provider.SmokeEnv("TMPDIR")
		if !strings.HasPrefix(tmp, filepath.Join(f.state, "tmp")+"/") {
			t.Errorf("the smoke got TMPDIR=%q", tmp)
		}
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Errorf("the smoke's tmp directory is still there: %v", err)
		}
	})

	t.Run("a failed smoke is handed to a Fix with a failed-verification brief, and the next round reviews again (AC 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.provider.Script(providertest.Script{Up: []R{{Stdout: "WEB_URL=http://127.0.0.1:9/\n"}}, Smoke: []R{
			{Stdout: "GET / 500\nsmoke: FAIL (1/2 failed)\n", Exit: 1}, {Stdout: "smoke: PASS (2/2)\n"},
		}})
		f.fixer.Script(fixed("echo fixed > page.txt", "fix(a): serve the page"))
		f.mustFire(0)
		fixes := f.fixer.Calls()
		if len(fixes) != 1 || !strings.Contains(fixes[0].Stdin, `"brief":{"kind":"failed-verification","failed":[{"item":"the smoke","detail":"1/2 failed","log":`) ||
			!strings.Contains(fixes[0].Stdin, "GET / 500") {
			t.Fatalf("the Fix was sent %v", fixes)
		}
		record := f.record()
		if namesOf(record) != "implement,pre-commit-review,fix,pre-commit-review,review-correctness,review-spec" || record["reviewRounds"] != 3.0 || smokeOf(record)["status"] != "PASS" || record["pr"] != 55.0 {
			t.Errorf("record = %v", record)
		}
		if f.branchLog() != "feat(a): add the greeting\nfix(a): serve the page" {
			t.Errorf("the branch carries\n%s", f.branchLog())
		}
	})

	t.Run("failed smokes count against the same cap as review rounds, and the cap opens no PR (AC 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.provider.Script(providertest.Script{Up: []R{{Stdout: "WEB_URL=x\n"}}, Smoke: []R{{Stdout: "smoke: FAIL (still broken)\n", Exit: 1}}})
		f.fixer.Script(fixed("date +%N >> tries.txt", "fix(a): try again"))
		f.mustFire(1)
		if len(f.review.Calls()) != 5 || len(f.fixer.Calls()) != 4 {
			t.Errorf("%d reviews, %d Fixes", len(f.review.Calls()), len(f.fixer.Calls()))
		}
		if f.labelled() != "AFK:failed" || len(f.comments) != 1 || !strings.Contains(f.comments[0].Body, "- the smoke failed: still broken") || f.prBody() != "" {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
		if record := f.record(); record["reviewRounds"] != 10.0 || smokeOf(record)["status"] != "FAIL" {
			t.Errorf("record = %v", record)
		}
	})

	// verify-boot: "a boot that fails twice is exit 4 — an infra-error, with
	// the environment torn down"
	t.Run("a boot that fails twice is an infra-error, with the environment torn down: skipped, and the PR still opens (AC 4)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.provider.Script(providertest.Script{Up: []R{{Exit: 4, Stderr: "port in use\n"}}})
		f.mustFire(0)
		if calls := strings.Join(f.provider.Calls(), "|"); calls != "down --all|down --all|up --pr 20|down --pr 20|up --pr 20|down --pr 20" {
			t.Errorf("the provider was called %s", calls)
		}
		// Never a pass the smoke did not give: the PR opens saying so.
		if smoke := smokeOf(f.record()); smoke["status"] != "SKIPPED" || !strings.Contains(f.prBody(), "`smoke: SKIPPED — up exited 4 (boot failed) on both attempts: port in use`") {
			t.Errorf("smoke %v, body\n%s", smoke, f.prBody())
		}
	})
	t.Run("a provider that breaks the contract is a skipped smoke naming the verdict, and is torn down", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.provider.Script(providertest.Script{Up: []R{{Stdout: "starting web...\nWEB_URL=x\n"}}})
		f.mustFire(0)
		smoke := smokeOf(f.record())
		if smoke["status"] != "SKIPPED" || smoke["violation"] != true || smoke["detail"] != "up printed a line that is not KEY=value: starting web..." {
			t.Errorf("smoke %v", smoke)
		}
		if calls := f.provider.Calls(); calls[len(calls)-1] != "down --pr 20" {
			t.Errorf("calls %q", calls)
		}
	})
	t.Run("a hermetic command that is not executable is a skipped smoke naming it, before anything is driven", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"verification":{"hermetic":{"command":"verify/missing","smoke":true}}}`)
		f.answer(finished)
		f.mustFire(0)
		if smoke := smokeOf(f.record()); smoke["status"] != "SKIPPED" || !strings.Contains(smoke["detail"].(string), "the hermetic command verify/missing is not an executable file under") {
			t.Errorf("smoke %v", smoke)
		}
	})

	// verify-boot: "the Bootstrap state is exit 3 — nothing to boot, and not
	// a failure"; bootstrap-state: "the pickup Fire still picks a ticket in
	// the Bootstrap state (AC 1)".
	t.Run("the Bootstrap state boots nothing and is not a failure: the PR opens with AFK:verify-human (AC 5)", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"verification":{}}`)
		var labelled []string
		f.gh.HandleFunc("POST", "/repos/acme/widgets/issues/55/labels", func(r githubtest.Request) githubtest.Reply {
			labelled = append(labelled, r.Body)
			f.pr.labels = append(f.pr.labels, "AFK:verify-human")
			return githubtest.Text(200, `[]`)
		})
		f.answer(finished)
		f.mustFire(0)
		if len(f.provider.Calls()) != 0 {
			t.Errorf("a provider was run: %q", f.provider.Calls())
		}
		if len(labelled) != 1 || labelled[0] != `{"labels":["AFK:verify-human"]}` {
			t.Errorf("the PR was labelled %q", labelled)
		}
		record := f.record()
		if smoke := smokeOf(record); smoke["status"] != "SKIPPED" || smoke["bootstrap"] != true || record["phase"] != "done" || record["exit"] != 0.0 {
			t.Errorf("record = %v", record)
		}
		if !strings.Contains(f.prBody(), "`smoke: SKIPPED — Bootstrap state: no Environment provider, AFK:verify-human applied`") {
			t.Errorf("the PR body is\n%s", f.prBody())
		}
	})

	// bootstrap-state: "a PR that adds the hermetic block is verified by the
	// provider it adds (AC 2)".
	t.Run("a PR that adds the hermetic block is verified by the provider it adds (AC 2)", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"verification":{}}`)
		adds := fmt.Sprintf(`{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"verification":{"hermetic":{"command":%q,"smoke":true}}}`, f.provider.Path)
		f.claude.Script(steptest.Reply{
			Run: "echo hello > greeting.txt; printf '%s' '" + adds + "' > .auto-agent/harness.json",
			Lines: []string{
				implementInit("session-1"),
				steptest.Result(map[string]any{"structured_output": finished}),
			},
		})
		f.mustFire(0)
		if calls := strings.Join(f.provider.Calls(), "|"); calls != "down --all|up --pr 20|smoke|down --pr 20" {
			t.Errorf("the provider the branch adds was not run: %s", calls)
		}
		if smoke := smokeOf(f.record()); smoke["status"] != "PASS" {
			t.Errorf("smoke %v", smoke)
		}
	})

	t.Run("what the provider leaves in the checkout is never committed by the Fix", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.provider.Script(providertest.Script{Up: []R{{Stdout: "WEB_URL=x\n", Run: "mkdir -p run && echo pid > run/web.pid"}}, Smoke: []R{
			{Stdout: "smoke: FAIL (1/2 failed)\n", Exit: 1, Run: "echo log > smoke.log"}, {Stdout: "smoke: PASS (2/2)\n"},
		}})
		f.fixer.Script(fixed("echo fixed > page.txt", "fix(a): serve the page"))
		f.mustFire(0)
		head := f.remoteRef("refs/heads/feat/issue-20")
		if files := git(t, f.remote, "show", "--name-only", "--format=", head); files != "page.txt" {
			t.Errorf("the Fix commit holds %q", files)
		}
	})
	t.Run("a review that asks for changes is answered before any smoke runs", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.review.Script(verdict(precommit.ChangeRequest, "cover it"), verdict(precommit.Approved))
		f.fixer.Script(fixed("echo covered > c.txt", "test(a): cover it"))
		f.mustFire(0)
		if n := strings.Count(strings.Join(f.provider.Calls(), "|"), "smoke"); n != 1 {
			t.Errorf("%d smokes", n)
		}
	})
}

func mustEvalDir(t *testing.T, dir string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
