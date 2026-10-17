package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// conflicted runs the Fire that opens PR #55 on ticket #20's branch (it
// writes greeting.txt), then moves trunk on with a commit writing file:
// greeting.txt conflicts, any other file does not. GitHub then lists the PR
// as no longer merging.
func (f *fireFixture) conflicted(file string) {
	f.t.Helper()
	f.answer(finished)
	f.mustFire(0)
	if err := os.WriteFile(filepath.Join(f.dir, file), []byte("trunk's line\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	git(f.t, f.dir, "add", "-A")
	git(f.t, f.dir, "commit", "--quiet", "-m", "trunk moves on")
	git(f.t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
	pr := openPR(55, "feat/issue-20", "2026-10-12T09:00:00Z")
	f.gh.Handle("GET", pullsPath, githubtest.JSON(200, []map[string]any{pr}))
	f.gh.Handle("GET", "/repos/acme/widgets/pulls/55", githubtest.JSON(200, merges(pr, false)))
	f.gh.Handle("GET", "/repos/acme/widgets/pulls/55/files", githubtest.JSON(200, []map[string]string{{"filename": "greeting.txt"}}))
}

// resolves is the fake Fix Step resolving a rebase's conflicts with run.
func resolves(run string) steptest.Reply {
	return steptest.Reply{Run: run, Lines: []string{
		steptest.Init("fix-1", "claude-opus", append([]string{"StructuredOutput"}, fix.Spec().Tools...), []string{"factory:fix"}),
		steptest.Success(fix.Result{Summary: "kept both lines", CommitMessage: "fix(a): resolve the conflict"}),
	}}
}

// rebaseOf is the rebase in a Fire record.
func rebaseOf(record map[string]any) map[string]any {
	r, _ := record["rebase"].(map[string]any)
	return r
}

// Rebasing a PR the default branch moved under: Go's git, a Fix only on a
// conflict. Ported from the rules lib/runbook-check.sh held
// plugin/skills/pr-reconcile/SKILL.md §1 to.
func TestFireRebase(t *testing.T) {
	t.Run("rebase-driver: a clean rebase is pushed with a lease and no Step runs (behaviour 1)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.conflicted("other.txt")
		f.mustFire(0)
		record := f.lastRecord()
		trunk := f.remoteRef("refs/heads/trunk")
		if parent := git(t, f.dir, "--git-dir", f.remote, "rev-parse", "feat/issue-20^"); parent != trunk {
			t.Errorf("the branch's parent is %s, not trunk's %s", parent, trunk)
		}
		if record["reconcile"] != "conflict" || record["phase"] != "done" || len(stepsOf(record)) != 0 || rebaseOf(record)["stops"] != 0.0 || record["commit"] != f.remoteRef("refs/heads/feat/issue-20") {
			t.Errorf("record %v", record)
		}
		// Under the ticket's lock, then its CI is waited on.
		if ciOf(record)["head"] != f.remoteRef("refs/heads/feat/issue-20") || !strings.HasSuffix(f.writes(), "POST "+lockPath+"\nDELETE "+unlockPath) {
			t.Errorf("ci %v, GitHub received\n%s", ciOf(record), f.writes())
		}
	})
	t.Run("a conflict is resolved by a Fix and the rebase completes (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.conflicted("greeting.txt")
		f.fixer.Script(resolves("printf \"trunk's line\\nhello\\n\" > greeting.txt"))
		f.mustFire(0)
		record := f.lastRecord()
		if r := rebaseOf(record); r["stops"] != 1.0 || fmt.Sprint(r["conflicts"]) != "[greeting.txt]" || r["failed"] != "" {
			t.Errorf("rebase %v", r)
		}
		if got := git(t, f.dir, "--git-dir", f.remote, "show", "feat/issue-20:greeting.txt"); got != "trunk's line\nhello" {
			t.Errorf("greeting.txt is %q", got)
		}
		stdin := f.fixer.Calls()[0].Stdin
		if !strings.Contains(stdin, `"base":"origin/trunk"`) || !strings.Contains(stdin, `"brief":{"kind":"rebase-conflict","conflicts":["greeting.txt"]}`) {
			t.Errorf("the Fix was sent %s", stdin)
		}
		// The branch keeps its own commits: the Fix's message is not one.
		if log := git(t, f.dir, "--git-dir", f.remote, "log", "--format=%s", "trunk..feat/issue-20"); strings.Contains(log, "resolve the conflict") {
			t.Errorf("branch log %q", log)
		}
	})
	t.Run("a Fix the usage limit cuts off pauses the ticket: nothing is pushed, nor waited on", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.conflicted("greeting.txt")
		before := f.remoteRef("refs/heads/feat/issue-20")
		f.fixer.Script(steptest.Reply{ExitCode: 1, Stderr: "You've hit your session limit · resets 10:50pm (America/New_York)",
			Lines: []string{steptest.Init("fix-1", "claude-opus", append([]string{"StructuredOutput"}, fix.Spec().Tools...), []string{"factory:fix"})}})
		f.mustFire(0)
		record := f.lastRecord()
		if f.labelled() != "AFK:done,AFK:paused" || ciOf(record) != nil || record["phase"] != "paused" || f.remoteRef("refs/heads/feat/issue-20") != before || record["parkedPR"] != "" {
			t.Errorf("labels %q, record %v", f.labelled(), record)
		}
	})
	t.Run("lease-only and parked-labels: a refused lease aborts and parks the PR (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.conflicted("greeting.txt")
		// Someone pushes to the branch while the conflict is being resolved.
		f.fixer.Script(resolves(fmt.Sprintf("printf 'resolved\\n' > greeting.txt; git push --quiet --force '%s' refs/remotes/origin/trunk:refs/heads/feat/issue-20", f.remote)))
		f.mustFire(0)
		theirs := f.remoteRef("refs/heads/trunk")
		record := f.lastRecord()
		if f.remoteRef("refs/heads/feat/issue-20") != theirs || record["parkedPR"] != "AFK:rebase-failed" || !strings.Contains(fmt.Sprint(rebaseOf(record)["failed"]), "lease rejected") {
			t.Errorf("record %v", record)
		}
		comment := f.pr.comments[len(f.pr.comments)-1]
		if !strings.Contains(comment, "Automatic rebase onto origin/trunk failed at") || !strings.Contains(comment, "lease rejected") || !strings.Contains(strings.Join(f.pr.labels, ","), "AFK:rebase-failed") {
			t.Errorf("labels %v, comment %q", f.pr.labels, comment)
		}
		if ciOf(record) != nil {
			t.Error("CI was waited on for a rebase that was not pushed")
		}
	})
	t.Run("a conflict the Fix leaves unresolved is aborted, the branch as the PR shows it, and parked", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.conflicted("greeting.txt")
		before := f.remoteRef("refs/heads/feat/issue-20")
		f.fixer.Script(resolves("true"))
		f.mustFire(0)
		record := f.lastRecord()
		if f.remoteRef("refs/heads/feat/issue-20") != before || record["parkedPR"] != "AFK:rebase-failed" || !strings.Contains(fmt.Sprint(rebaseOf(record)["failed"]), "greeting.txt still holds a conflict marker") {
			t.Errorf("record %v", record)
		}
		// The clone is not left mid-rebase for the next Fire.
		if status := git(t, filepath.Join(f.state, "clone"), "status", "--short", "--branch"); strings.Contains(status, "rebase") || strings.Contains(status, "UU") {
			t.Errorf("status %q", status)
		}
	})
	t.Run("a parked PR is not rebased again", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.conflicted("greeting.txt")
		pr := openPR(55, "feat/issue-20", "2026-10-12T09:00:00Z", "AFK:rebase-failed")
		f.gh.Handle("GET", pullsPath, githubtest.JSON(200, []map[string]any{pr}))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/55", githubtest.JSON(200, merges(pr, false)))
		f.mustFire(0)
		if record := f.lastRecord(); record["reconcile"] != "" || rebaseOf(record) != nil {
			t.Errorf("record %v", record)
		}
	})
	t.Run("the dry run names the rebase it would make", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.conflicted("other.txt")
		f.stdout.Reset()
		if code := f.run("run", "--once", "--dry-run", "--repo", "acme/widgets", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if next, _ := f.report(t)["next"].(map[string]any); next["action"] != "rebase" || next["pr"] != 55.0 {
			t.Errorf("next = %v", next)
		}
	})
}
