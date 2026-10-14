package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/precommit"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// namesOf are the names of a record's Step entries, in order.
func namesOf(record map[string]any) string {
	var names []string
	for _, entry := range stepsOf(record) {
		names = append(names, entry["name"].(string))
	}
	return strings.Join(names, ",")
}

// branchLog is the subjects of the commits the ticket's branch carries on the
// remote, oldest first.
func (f *fireFixture) branchLog() string {
	return git(f.t, f.remote, "log", "--reverse", "--format=%s", "trunk..feat/issue-20")
}

// The review round of the dispatch skill, run by Go. Where a case carries
// over from plugin/hooks/hooks.test.sh (the review-gate.sh cases) or from a
// rule lib/runbook-check.sh holds the dispatch skill to, it keeps that name.
func TestFirePreCommitReview(t *testing.T) {
	// Behaviour 1; hooks.test.sh: "an approved verdict allows the stop".
	t.Run("an approved verdict allows the stop: approve on the first round opens the PR (AC 6)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.mustFire(0)
		calls := f.review.Calls()
		if len(calls) != 1 || len(f.fixer.Calls()) != 0 {
			t.Fatalf("%d reviews, %d Fixes", len(calls), len(f.fixer.Calls()))
		}
		var input precommit.Input
		json.Unmarshal([]byte(strings.TrimPrefix(calls[0].Stdin, "/factory:pre-commit-review ")), &input)
		if input.Round != 1 || input.Cap != 10 || input.Base != "origin/trunk" || input.Branch != "feat/issue-20" || input.Issue.Number != 20 || input.Commands.Test != "y" {
			t.Errorf("the review was sent %q", calls[0].Stdin)
		}
		// It reviews what was pushed: the Implement commit is on the branch.
		if f.branchLog() != "feat(a): add the greeting" || f.record()["pr"] != 55.0 {
			t.Errorf("branch %q, record %v", f.branchLog(), f.record())
		}
	})

	// Behaviour 4; agents/reviewer.md: tools.
	t.Run("tools: the review is called read-only, with exactly the test and lint commands", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"make deps","test":"make test","lint":"make lint"},"pick":{"labels":{}}}`)
		f.answer(finished)
		f.mustFire(0)
		call := f.review.Calls()[0]
		tools, _ := call.Arg("--tools")
		allowed, _ := call.Arg("--allowedTools")
		if tools != "Read,Glob,Grep,Bash" || allowed != "Read,Glob,Grep,Bash(git diff:*),Bash(git log:*),Bash(git show:*),Bash(git status:*),Bash(make test),Bash(make lint)" {
			t.Errorf("--tools %q --allowedTools %q", tools, allowed)
		}
		if mode, _ := call.Arg("--permission-mode"); mode != "dontAsk" {
			t.Errorf("--permission-mode %q", mode)
		}
	})

	// Behaviour 2; hooks.test.sh: "a pending change-request on the
	// checked-out branch blocks the stop and lists the asks". An ask is never
	// abandoned: every one goes to the Fix.
	t.Run("a pending change-request on the checked-out branch blocks the stop and lists the asks: Fix, approve, one extra commit, then the PR (AC 4, 6)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.review.Script(verdict(precommit.ChangeRequest, "cover the empty-list case", "drop the mock of ItemStore"), verdict(precommit.Approved))
		f.fixer.Script(fixed("echo covered > empty.txt", "test(a): cover the empty list"))
		f.mustFire(0)

		fixes := f.fixer.Calls()
		if len(fixes) != 1 || !strings.Contains(fixes[0].Stdin, `"brief":{"kind":"change-request","change_requests":["cover the empty-list case","drop the mock of ItemStore"]}`) {
			t.Fatalf("the Fix was sent %v", fixes)
		}
		if f.branchLog() != "feat(a): add the greeting\ntest(a): cover the empty list" {
			t.Errorf("the branch carries\n%s", f.branchLog())
		}
		reviews := f.review.Calls()
		if len(reviews) != 2 || !strings.Contains(reviews[1].Stdin, `"round":2`) {
			t.Errorf("%d reviews", len(reviews))
		}
		record := f.record()
		if namesOf(record) != "implement,pre-commit-review,fix,pre-commit-review" || record["reviewRounds"] != 2.0 || record["pr"] != 55.0 ||
			record["commit"] != f.remoteRef("refs/heads/feat/issue-20") {
			t.Errorf("record = %v", record)
		}
		// The PR is the last write before the lock comes off.
		if want := "POST " + lockPath + "\nPOST " + pullsPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s", f.writes())
		}
	})

	t.Run("what the review's test and lint runs leave in the checkout is never committed by the Fix", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		asks := verdict(precommit.ChangeRequest, "cover it")
		asks.Run = "echo artifact > coverage.out"
		f.review.Script(asks, verdict(precommit.Approved))
		f.fixer.Script(fixed("echo covered > empty.txt", "test(a): cover it"))
		f.mustFire(0)
		head := f.remoteRef("refs/heads/feat/issue-20")
		if files := git(t, f.remote, "show", "--name-only", "--format=", head); files != "empty.txt" {
			t.Errorf("the Fix commit holds %q", files)
		}
	})

	// Behaviour 3.
	t.Run("the cap: ten rounds of change requests follow the failure path and open no PR (AC 5)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.review.Script(verdict(precommit.ChangeRequest, "still not covered"))
		f.fixer.Script(fixed("date +%N >> tries.txt", "test(a): try again"))
		f.mustFire(1)
		if len(f.review.Calls()) != 10 || len(f.fixer.Calls()) != 9 {
			t.Errorf("%d reviews, %d Fixes", len(f.review.Calls()), len(f.fixer.Calls()))
		}
		if f.labelled() != "AFK:failed" || len(f.comments) != 1 || !strings.Contains(f.comments[0].Body, "10 rounds") || !strings.Contains(f.comments[0].Body, "- still not covered") {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
		if strings.Contains(f.writes(), pullsPath) {
			t.Errorf("a PR was opened:\n%s", f.writes())
		}
		record := f.record()
		if record["phase"] != "failed" || record["reviewRounds"] != 10.0 || record["pr"] != nil || !strings.Contains(record["error"].(string), "10 rounds") {
			t.Errorf("record = %v", record)
		}
		// Every Fix was committed and pushed.
		if n := strings.Count(f.branchLog(), "test(a): try again"); n != 9 {
			t.Errorf("%d Fix commits on the branch", n)
		}
	})

	// Behaviour 7.
	t.Run("a restart at branch-has-commits-no-PR runs the review again, not Implement (AC 7)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.gh.Handle("POST", pullsPath, githubtest.Text(500, `{"message":"boom"}`), githubtest.JSON(201, map[string]any{"number": 55}))
		f.mustFire(1)
		f.later(time.Minute)
		f.mustFire(0)
		if len(f.claude.Calls()) != 1 || len(f.review.Calls()) != 2 {
			t.Fatalf("%d Implement calls, %d reviews", len(f.claude.Calls()), len(f.review.Calls()))
		}
		records := f.records()
		second := records[len(records)-1]
		if namesOf(second) != "pre-commit-review" || second["pr"] != 55.0 || second["reviewRounds"] != 1.0 {
			t.Errorf("second Fire = %v", second)
		}
		// The Implement answer is gone: the PR comes from the commit and the
		// ticket's acceptance criteria, as the bash opened it.
		var pr map[string]any
		writes := f.gh.Writes()
		for _, w := range writes {
			if w.Path == pullsPath {
				json.Unmarshal([]byte(w.Body), &pr)
			}
		}
		if body, _ := pr["body"].(string); pr["title"] != "feat(a): add the greeting" || !strings.Contains(body, "## Summary\n\nIt says hello.\n") || !strings.Contains(body, "## Manual verification\n\n- [ ] it greets\n") {
			t.Errorf("the PR was %v", pr)
		}
	})

	t.Run("a branch of wip work only is Implement's again, not the review's", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(implement.Result{Summary: "half of it", Checklist: []string{}})
		f.mustFire(1)
		f.answer(finished)
		f.later(time.Minute)
		f.mustFire(0)
		if len(f.claude.Calls()) != 2 || len(f.review.Calls()) != 1 {
			t.Errorf("%d Implement calls, %d reviews", len(f.claude.Calls()), len(f.review.Calls()))
		}
	})

	t.Run("a review that crashes is a failed attempt at the review, and the work stays pushed", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.review.Script(steptest.Reply{Stderr: "segfault", ExitCode: 139})
		f.mustFire(1)
		if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0].Body, "<!-- auto-agent:attempts step=pre-commit-review count=1 -->") || f.labelled() != "" {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
		if f.branchLog() != "feat(a): add the greeting" || strings.Contains(f.writes(), pullsPath) {
			t.Errorf("branch %q; GitHub received\n%s", f.branchLog(), f.writes())
		}
	})

	// Adapted from hooks.test.sh: "a change-request with no asks recorded
	// still blocks". A change request with no asks is no answer: it is
	// corrected once, then it is a failed attempt, never an approval.
	t.Run("a change request with no asks is corrected once, then a failed attempt, and opens no PR", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.review.Script(verdict(precommit.ChangeRequest))
		f.mustFire(1)
		if len(f.review.Calls()) != 2 || len(f.fixer.Calls()) != 0 || strings.Contains(f.writes(), pullsPath) {
			t.Errorf("%d reviews, %d Fixes", len(f.review.Calls()), len(f.fixer.Calls()))
		}
		if session, _ := f.review.Calls()[1].Arg("--resume"); session != "review-1" || !strings.Contains(f.review.Calls()[1].Stdin, "asks for nothing") {
			t.Errorf("the correction was %v", f.review.Calls()[1])
		}
	})

	t.Run("a Fix the usage limit cuts off pauses the ticket, and the next Fire reviews again", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.review.Script(verdict(precommit.ChangeRequest, "cover it"), verdict(precommit.Approved))
		f.fixer.Script(steptest.Reply{Run: "echo half > fix.txt", ExitCode: 1, Stderr: "You've hit your session limit · resets 10:50pm (America/New_York)",
			Lines: []string{steptest.Init("fix-1", "claude-opus", append([]string{"StructuredOutput"}, fix.Spec().Tools...), []string{"factory:fix"})}})
		f.mustFire(0)
		if f.labelled() != "AFK:paused" || !strings.Contains(f.branchLog(), "wip: freeze partial work on #20 (usage exhausted)") {
			t.Fatalf("labels %q, branch\n%s", f.labelled(), f.branchLog())
		}
		f.later(18 * time.Hour)
		f.mustFire(0)
		records := f.records()
		second := records[len(records)-1]
		if len(f.claude.Calls()) != 1 || namesOf(second) != "pre-commit-review" || second["pr"] != 55.0 || f.labelled() != "" {
			t.Errorf("%d Implement calls; second Fire %v; labels %q", len(f.claude.Calls()), second, f.labelled())
		}
	})
}
