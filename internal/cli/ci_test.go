package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/review"
)

// ciOf is the CI wait in a Fire record.
func ciOf(record map[string]any) map[string]any {
	c, _ := record["ci"].(map[string]any)
	return c
}

// lastRecord is the newest Fire record.
func (f *fireFixture) lastRecord() map[string]any {
	records := f.records()
	return records[len(records)-1]
}

// The wait on CI, with no Step: Go reads the PR head's checks on its own
// clock. Ported from lib/ci-wait.test.sh, through a whole Fire.
func TestFireCI(t *testing.T) {
	pending := []map[string]any{checkRun(1, "build", "in_progress", "")}
	t.Run("pending then green -> exit 0, result green, polls 2", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{pending, {checkRun(1, "build", "completed", "success")}}
		f.answer(finished)
		f.mustFire(0)
		record := f.record()
		wait := ciOf(record)
		if wait["result"] != "green" || wait["reads"] != 2.0 || wait["head"] != f.remoteRef("refs/heads/feat/issue-20") || record["phase"] != "done" {
			t.Errorf("record %v", record)
		}
		// One wait between the reads, a minute long, and no Step for it.
		if len(f.sleeps) != 1 || f.sleeps[0] != time.Minute || namesOf(record) != "implement,pre-commit-review,review-correctness,review-spec" {
			t.Errorf("sleeps %v, steps %s", f.sleeps, namesOf(record))
		}
	})
	t.Run("deadline with pending -> exit 2, result timeout", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{pending}
		f.answer(finished)
		f.mustFire(0)
		record := f.record()
		if wait := ciOf(record); wait["result"] != "timeout" || wait["reads"] != 45.0 || len(f.sleeps) != 44 || record["error"] != "" || len(f.pr.labels) != 0 {
			t.Errorf("record %v, %d sleeps, labels %v", record, len(f.sleeps), f.pr.labels)
		}
		// The next Fire, in the reconcile Lane, reads CI again, once: the
		// Daemon's Fires are the wait from here on, and none waits long.
		f.listOurPR()
		f.pr.comments = f.pr.comments[:1]
		sleeps := len(f.sleeps)
		f.mustFire(0)
		if wait := ciOf(f.lastRecord()); wait["result"] != "pending" || wait["reads"] != 1.0 || len(f.sleeps) != sleeps || f.lastRecord()["reconcile"] != "incomplete" {
			t.Errorf("record %v", f.lastRecord())
		}
	})
	t.Run("red -> exit 1, failed[] + '=== job ===' log tail (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		legacy := checkRun(3, "codecov", "completed", "failure")
		legacy["app"] = map[string]any{"slug": "codecov"}
		f.ci = [][]map[string]any{{checkRun(2, "unit-tests", "completed", "failure"), checkRun(4, "lint", "completed", "success"), legacy}, {checkRun(1, "build", "completed", "success")}}
		f.gh.Handle("GET", "/repos/acme/widgets/actions/jobs/2/logs", githubtest.Text(200, strings.Repeat("noise\n", 300)+"FAIL: TestGreeting\n"))
		f.answer(finished)
		f.fixer.Script(fixed("echo Hello > greeting.txt", "fix(a): capitalise the greeting"))
		f.mustFire(0)
		// The failed run, logs and all, is the Fix's brief.
		stdin := f.fixer.Calls()[0].Stdin
		var brief struct {
			Brief struct {
				Kind string
				CI   []struct{ Name, Link, Log string }
			}
		}
		json.Unmarshal([]byte(stdin[strings.Index(stdin, "{"):]), &brief)
		failed := brief.Brief.CI
		if brief.Brief.Kind != "ci-failure" || len(failed) != 2 {
			t.Fatalf("the Fix was sent %s", stdin)
		}
		if failed[0].Name != "unit-tests" || failed[0].Link != "https://github.com/acme/widgets/actions/runs/1/job/2" || !strings.HasSuffix(failed[0].Log, "FAIL: TestGreeting") || strings.Count(failed[0].Log, "\n") != 199 {
			t.Errorf("unit-tests %+v", failed[0])
		}
		if failed[1].Name != "codecov" || !strings.HasPrefix(failed[1].Log, "(log unavailable") {
			t.Errorf("codecov %+v", failed[1])
		}
	})
	t.Run("fail+pending waits for settle before verdict", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{{checkRun(2, "unit", "completed", "failure"), checkRun(5, "e2e", "in_progress", "")}, {checkRun(2, "unit", "completed", "failure"), checkRun(5, "e2e", "completed", "success")}, {checkRun(1, "build", "completed", "success")}}
		f.gh.Handle("GET", "/repos/acme/widgets/actions/jobs/2/logs", githubtest.Text(200, "boom\n"))
		f.answer(finished)
		f.fixer.Script(fixed("echo Hello > greeting.txt", "fix(a): capitalise the greeting"))
		f.mustFire(0)
		// Read 1 was still running: no Fix until read 2 said red.
		if f.ciReads != 3 || len(f.fixer.Calls()) != 1 || ciOf(f.record())["result"] != "green" {
			t.Errorf("%d reads, %d Fixes, ci %v", f.ciReads, len(f.fixer.Calls()), ciOf(f.record()))
		}
	})
	t.Run("skipping bucket ignored -> green; a missing required check is not green (behaviour 4)", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"required_checks":["title"]}`)
		skipped := checkRun(6, "docs", "completed", "skipped")
		f.ci = [][]map[string]any{{skipped, checkRun(1, "build", "completed", "success")}, {skipped, checkRun(1, "build", "completed", "success"), checkRun(7, "title", "completed", "success")}}
		f.answer(finished)
		f.mustFire(0)
		if wait := ciOf(f.record()); wait["result"] != "green" || wait["reads"] != 2.0 {
			t.Errorf("ci %v", wait)
		}
	})
	t.Run("a commit no check ever comes for is none after a few reads, and the Fire carries on", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{{}}
		f.answer(finished)
		f.mustFire(0)
		if wait := ciOf(f.record()); wait["result"] != "none" || wait["reads"] != 5.0 {
			t.Errorf("ci %v", wait)
		}
	})
	t.Run("3 unreadable polls -> exit 3, result error", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = nil
		f.answer(finished)
		f.mustFire(1)
		if wait := ciOf(f.record()); wait["result"] != "error" || !strings.Contains(fmt.Sprint(f.record()["error"]), "reading the checks") {
			t.Errorf("record %v", f.record())
		}
	})
	t.Run("a read that fails now and then is a flake: only three in a row end the wait", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{nil, nil, pending, nil, nil, {checkRun(1, "build", "completed", "success")}}
		f.answer(finished)
		f.mustFire(0)
		if wait := ciOf(f.record()); wait["result"] != "green" || wait["reads"] != 2.0 {
			t.Errorf("ci %v", wait)
		}
	})
	t.Run("the cap counts every read, a failed one too", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{pending, nil}
		for len(f.ci) < 45 {
			f.ci = append(f.ci, pending, nil)
		}
		f.ci = f.ci[:45]
		f.answer(finished)
		f.mustFire(0)
		if wait := ciOf(f.record()); wait["result"] != "timeout" || f.ciReads != 45 {
			t.Errorf("ci %v after %d reads", wait, f.ciReads)
		}
	})
	t.Run("a review's ambiguities a cut-off Fire never asked are asked by the reconcile Lane", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.listOurPR()
		f.pr.comments = []string{finding.Done(1, 0, "abc", finding.Routes{Ambiguities: []finding.Finding{{Kind: finding.ProductAmbiguity, Path: "greeting.txt", Line: 1, Summary: "greet once or every time?", FailureScenario: "once: quiet; every time: noisy"}}}, "")}
		// The branch the review read: a first Fire left it.
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:refs/heads/feat/issue-20")
		f.mustFire(0)
		requests := f.rulingRequests()
		if len(requests) != 1 || !strings.Contains(requests[0], ": CI green, manual verification not run by this build.") || !strings.Contains(f.writer.Calls()[0].Stdin, "greet once or every time?") {
			t.Fatalf("requests %v", requests)
		}
		// Asked once: the next Fire only reads CI.
		f.mustFire(0)
		if len(f.rulingRequests()) != 1 || len(f.writer.Calls()) != 1 {
			t.Errorf("%d requests", len(f.rulingRequests()))
		}
	})
	t.Run("the Ruling request states what CI said (behaviour: the evidence)", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"pr_watch":1}}`)
		f.ci = [][]map[string]any{{checkRun(2, "unit", "completed", "failure")}}
		f.gh.Handle("GET", "/repos/acme/widgets/actions/jobs/2/logs", githubtest.Text(200, "boom\n"))
		f.answer(finished)
		f.fixer.Script(fixed("echo Hello > greeting.txt", "fix(a): capitalise the greeting"))
		f.specReview.Script(found(review.SpecName, unsure))
		f.mustFire(0)
		if requests := f.rulingRequests(); len(requests) != 1 || !strings.Contains(requests[0], ": CI red (1 failing: unit), manual verification not run by this build. A reply of") {
			t.Errorf("requests %v", requests)
		}
	})
	t.Run("an applied Ruling that changed code states the CI of its new head", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.waitingOnRuling(t, "1A 2B")
		f.fixer.Script(answers("echo Hi > greeting.txt"))
		f.mustFire(0)
		head := f.remoteRef("refs/heads/feat/issue-20")
		if !strings.Contains(f.applied(), "Verification re-ran on `"+head[:7]+"`: CI green, manual verification not run by this build.") || ciOf(f.lastRecord())["head"] != head {
			t.Errorf("applied\n%s", f.applied())
		}
	})
	t.Run("a round of thread fixes is followed by the wait on its push", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.fixer.Script(answers("echo Hello > greeting.txt", fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"}))
		f.mustFire(0)
		if wait := ciOf(f.lastRecord()); wait["result"] != "green" || wait["head"] != f.remoteRef("refs/heads/feat/issue-20") {
			t.Errorf("ci %v", wait)
		}
	})
	t.Run("the dry run names the wait it would make", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.listOurPR()
		f.prComments(55, done[:1]...)
		f.stdout.Reset()
		if code := f.run("run", "--once", "--dry-run", "--repo", "acme/widgets", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if next, _ := f.report(t)["next"].(map[string]any); next["action"] != "wait-ci" || next["pr"] != 55.0 {
			t.Errorf("next = %v", next)
		}
	})
}
