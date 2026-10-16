package cli

import (
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// flaked is the fake Fix Step changing nothing and naming a flake.
func flaked(reason string) steptest.Reply {
	return steptest.Reply{Lines: []string{
		steptest.Init("fix-1", "claude-opus", append([]string{"StructuredOutput"}, fix.Spec().Tools...), []string{"factory:fix"}),
		steptest.Success(fix.Result{Summary: "nothing to fix", CommitMessage: "fix(a): nothing", Flake: reason}),
	}}
}

var (
	red   = []map[string]any{checkRun(2, "unit", "completed", "failure")}
	green = []map[string]any{checkRun(1, "build", "completed", "success")}
)

// redCI makes the first read of CI red, its job's log a failing test.
func (f *fireFixture) redCI(after ...[]map[string]any) {
	f.ci = append([][]map[string]any{red}, after...)
	f.gh.Handle("GET", "/repos/acme/widgets/actions/jobs/2/logs", githubtest.Text(200, "--- FAIL: TestGreeting\n"))
}

// Fixing a failing CI run, within rounds.pr_watch. The rules
// lib/runbook-check.sh held plugin/skills/pr-watch/SKILL.md to, under their
// names, where Go now does what the prompt was held to.
func TestFireCIFix(t *testing.T) {
	t.Run("a red run gets a Fix, a push and a green wait (behaviour 1); never-force", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.redCI(green)
		f.answer(finished)
		f.fixer.Script(fixed("echo Hello > greeting.txt", "fix(a): capitalise the greeting"))
		before := ""
		f.mustFire(0)
		head := f.remoteRef("refs/heads/feat/issue-20")
		record := f.record()
		if wait := ciOf(record); wait["result"] != "green" || wait["head"] != head || record["ciRounds"] != 1.0 || record["phase"] != "done" {
			t.Errorf("record %v", record)
		}
		// The fix is a commit on top: the branch was pushed, never forced.
		if log := git(t, f.dir, "--git-dir", f.remote, "log", "--format=%s", "feat/issue-20"); !strings.HasPrefix(log, "fix(a): capitalise the greeting\n") || head == before {
			t.Errorf("branch log %q", log)
		}
		if namesOf(record) != "implement,pre-commit-review,review-correctness,review-spec,fix" {
			t.Errorf("steps %s", namesOf(record))
		}
	})
	t.Run("cap-from-config and default-label: the cap drafts and labels the PR and comments on the ticket (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"pr_watch":2}}`)
		f.redCI()
		f.answer(finished)
		f.fixer.Script(fixed("echo one > greeting.txt", "fix(a): try one"), fixed("echo two > greeting.txt", "fix(a): try two"))
		f.mustFire(0)
		record := f.record()
		if len(f.fixer.Calls()) != 2 || record["ciRounds"] != 2.0 || record["parkedPR"] != "AFK:checks-failed" || record["error"] != "" {
			t.Errorf("%d Fixes, record %v", len(f.fixer.Calls()), record)
		}
		writes := f.writes()
		// default-label and the draft: the PR is a draft again, labelled.
		if !strings.Contains(writes, "POST /graphql") || !strings.Contains(strings.Join(f.pr.labels, ","), "AFK:checks-failed") {
			t.Errorf("labels %v, GitHub received\n%s", f.pr.labels, writes)
		}
		if last := f.comments[len(f.comments)-1].Body; !strings.Contains(last, "CI on PR #55 is still failing after 2 fix round(s). Marked draft and labelled `AFK:checks-failed`") {
			t.Errorf("the ticket's last comment %q", last)
		}
	})
	t.Run("a parked PR is not taken again (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		parked := openPR(55, "feat/issue-20", "2026-10-12T09:00:00Z", "AFK:checks-failed")
		parked["draft"] = true
		f.gh.Handle("GET", pullsPath, githubtest.JSON(200, []map[string]any{parked}))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/55", githubtest.JSON(200, merges(parked, true)))
		f.pr.comments = []string{finding.Done(0, 0, "abc", finding.Routes{}, "")}
		f.redCI()
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || f.ciReads != 0 || len(f.fixer.Calls()) != 0 {
			t.Errorf("record %v, %d CI reads", record, f.ciReads)
		}
	})
	t.Run("a Fix that names a flake has the failed jobs run again, and that is a round", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.redCI(green)
		f.gh.Handle("POST", "/repos/acme/widgets/actions/runs/1/rerun-failed-jobs", githubtest.Text(201, "{}"))
		f.answer(finished)
		f.fixer.Script(flaked("the runner lost its network"))
		f.mustFire(0)
		if record := f.record(); !strings.Contains(f.writes(), "POST /repos/acme/widgets/actions/runs/1/rerun-failed-jobs") || ciOf(record)["result"] != "green" || record["ciRounds"] != 1.0 {
			t.Errorf("record %v, GitHub received\n%s", record, f.writes())
		}
	})
	t.Run("after the failed jobs are run again, the wait reads only after an interval", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.redCI(green)
		f.gh.Handle("POST", "/repos/acme/widgets/actions/runs/1/rerun-failed-jobs", githubtest.Text(201, "{}"))
		f.answer(finished)
		f.fixer.Script(flaked("the runner lost its network"))
		f.mustFire(0)
		if len(f.sleeps) != 1 || f.ciReads != 2 {
			t.Errorf("sleeps %v, %d reads", f.sleeps, f.ciReads)
		}
	})
	t.Run("a park GitHub refuses is the Fire's error, never a quiet done", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"pr_watch":1}}`)
		f.redCI()
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/55", githubtest.Text(502, "bad gateway"))
		f.answer(finished)
		f.fixer.Script(fixed("echo Hello > greeting.txt", "fix(a): capitalise the greeting"))
		f.mustFire(1)
		if record := f.record(); !strings.Contains(record["error"].(string), "parking PR #55") || record["parkedPR"] != "" {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a Fix that changes nothing and names no flake is a failed attempt", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.redCI(green)
		f.answer(finished)
		f.fixer.Script(fixed("true", "fix(a): nothing"))
		f.mustFire(1)
		if err := f.record()["error"]; !strings.Contains(err.(string), "the Fix changed nothing for the failing CI run and named no flake (failed attempt 1 of 3)") || strings.Contains(f.writes(), "rerun") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("the reconcile Lane fixes a red run it reads, under the ticket's lock", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.listOurPR()
		f.pr.comments = []string{finding.Done(0, 0, "abc", finding.Routes{}, "")}
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:refs/heads/feat/issue-20")
		f.redCI(green)
		f.fixer.Script(fixed("echo Hello > greeting.txt", "fix(a): capitalise the greeting"))
		f.mustFire(0)
		record := f.record()
		if record["reconcile"] != "incomplete" || ciOf(record)["result"] != "green" || record["ciRounds"] != 1.0 || !strings.Contains(f.writes(), "POST "+lockPath) || !strings.HasSuffix(f.writes(), "DELETE "+unlockPath) {
			t.Errorf("record %v, GitHub received\n%s", record, f.writes())
		}
	})
	t.Run("a Fix that keeps failing parks the PR when the ticket's attempts are spent", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.redCI()
		f.answer(finished)
		f.fixer.Script(crashes)
		f.fire()
		f.listOurPR()
		for range 2 {
			f.fire()
		}
		if f.labelled() != "AFK:failed" || !strings.Contains(strings.Join(f.pr.labels, ","), "AFK:checks-failed") || !strings.Contains(f.comments[len(f.comments)-1].Body, "could not be fixed") {
			t.Errorf("ticket %q, PR labels %v", f.labelled(), f.pr.labels)
		}
	})
}
