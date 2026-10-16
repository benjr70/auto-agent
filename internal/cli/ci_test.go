package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
	t.Run("pending then green -> exit 0, result green, polls 2 (behaviour 1)", func(t *testing.T) {
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
	t.Run("deadline with pending -> exit 2, result timeout: the Fire ends with no label (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{pending}
		f.answer(finished)
		f.mustFire(0)
		record := f.record()
		if wait := ciOf(record); wait["result"] != "timeout" || wait["reads"] != 45.0 || len(f.sleeps) != 44 || record["error"] != "" || len(f.pr.labels) != 0 {
			t.Errorf("record %v, %d sleeps, labels %v", record, len(f.sleeps), f.pr.labels)
		}
		// The next Fire, in the reconcile Lane, waits again.
		f.listOurPR()
		f.pr.comments = f.pr.comments[:1]
		f.mustFire(0)
		if wait := ciOf(f.lastRecord()); wait["result"] != "timeout" || f.lastRecord()["reconcile"] != "incomplete" {
			t.Errorf("record %v", f.lastRecord())
		}
	})
	t.Run("red -> exit 1, failed[] + '=== job ===' log tail (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		legacy := checkRun(3, "codecov", "completed", "failure")
		legacy["app"] = map[string]any{"slug": "codecov"}
		f.ci = [][]map[string]any{{checkRun(2, "unit-tests", "completed", "failure"), checkRun(4, "lint", "completed", "success"), legacy}}
		f.gh.Handle("GET", "/repos/acme/widgets/actions/jobs/2/logs", githubtest.Text(200, strings.Repeat("noise\n", 300)+"FAIL: TestGreeting\n"))
		f.answer(finished)
		f.mustFire(0)
		wait := ciOf(f.record())
		failed, _ := wait["failed"].([]any)
		if wait["result"] != "fail" || len(failed) != 2 {
			t.Fatalf("ci %v", wait)
		}
		unit, other := failed[0].(map[string]any), failed[1].(map[string]any)
		log := fmt.Sprint(unit["log"])
		if unit["name"] != "unit-tests" || unit["link"] != "https://github.com/acme/widgets/actions/runs/1/job/2" || !strings.HasSuffix(log, "FAIL: TestGreeting") || strings.Count(log, "\n") != 199 {
			t.Errorf("unit-tests %v", unit)
		}
		if other["name"] != "codecov" || !strings.HasPrefix(fmt.Sprint(other["log"]), "(log unavailable") {
			t.Errorf("codecov %v", other)
		}
	})
	t.Run("fail+pending waits for settle before verdict", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{{checkRun(2, "unit", "completed", "failure"), checkRun(5, "e2e", "in_progress", "")}, {checkRun(2, "unit", "completed", "failure"), checkRun(5, "e2e", "completed", "success")}}
		f.gh.Handle("GET", "/repos/acme/widgets/actions/jobs/2/logs", githubtest.Text(200, "boom\n"))
		f.answer(finished)
		f.mustFire(0)
		if wait := ciOf(f.record()); wait["result"] != "fail" || wait["reads"] != 2.0 {
			t.Errorf("ci %v", wait)
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
	t.Run("three unreadable reads in a row are an error, never a verdict", func(t *testing.T) {
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
	t.Run("the Ruling request states what CI said (behaviour: the evidence)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.ci = [][]map[string]any{{checkRun(2, "unit", "completed", "failure")}}
		f.gh.Handle("GET", "/repos/acme/widgets/actions/jobs/2/logs", githubtest.Text(200, "boom\n"))
		f.answer(finished)
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
