package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diverged makes the remote hold ticket 42's branch, one commit on trunk
// writing file, and then moves trunk on with a commit writing upstream: the
// same file conflicts, another does not. The Workspace has the branch open.
func (f *fixture) diverged(file, upstream string) {
	f.t.Helper()
	git(f.t, f.other, "checkout", "--quiet", "-b", "feat/issue-42")
	write(f.t, filepath.Join(f.other, file), "the branch's line\n")
	git(f.t, f.other, "add", "-A")
	git(f.t, f.other, "commit", "--quiet", "-m", "feat: the branch's change")
	git(f.t, f.other, "push", "--quiet", "origin", "feat/issue-42")
	git(f.t, f.other, "checkout", "--quiet", "trunk")
	f.upstream(upstream, "trunk's line\n", "trunk moves on")
	if _, err := f.ws.Prepare(ctx); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.ws.OpenBranch(ctx, 42); err != nil {
		f.t.Fatal(err)
	}
}

// Ported from lib/rebase-driver.test.sh, against real throwaway
// repositories: rebase and lease semantics are what is under test.
func TestRebase(t *testing.T) {
	t.Run("clean rebase onto the configured default branch -> CLEAN, lease push lands", func(t *testing.T) {
		f := newFixture(t)
		f.diverged("feature.txt", "other.txt")
		conflicts, err := f.ws.Rebase(ctx)
		if err != nil || conflicts != nil {
			t.Fatalf("conflicts %v, err %v", conflicts, err)
		}
		commit, err := f.ws.PushWithLease(ctx, "feat/issue-42")
		if err != nil || commit.SHA != f.remoteRef("refs/heads/feat/issue-42") {
			t.Fatalf("commit %v, err %v", commit, err)
		}
		// The branch sits on trunk's head now, its own commit on top.
		if parent := git(t, f.remote, "rev-parse", "feat/issue-42^"); parent != f.remoteRef("refs/heads/trunk") {
			t.Errorf("the branch's parent is %s, not trunk's head", parent)
		}
		log, _ := os.ReadFile(f.argLog)
		for _, line := range strings.Split(string(log), "\n") {
			if strings.HasPrefix(line, "push ") && (!strings.Contains(line, "--force-with-lease=refs/heads/feat/issue-42:") || strings.Contains(line, " --force ")) {
				t.Errorf("pushed as %s", line)
			}
		}
	})
	t.Run("conflict stops with files, continue completes after resolution", func(t *testing.T) {
		f := newFixture(t)
		f.diverged("README.md", "README.md")
		conflicts, err := f.ws.Rebase(ctx)
		if err != nil || len(conflicts) != 1 || conflicts[0] != "README.md" {
			t.Fatalf("conflicts %v, err %v", conflicts, err)
		}
		write(t, filepath.Join(f.ws.Dir(), "README.md"), "trunk's line\nthe branch's line\n")
		if conflicts, err := f.ws.RebaseContinue(ctx, conflicts); err != nil || conflicts != nil {
			t.Fatalf("conflicts %v, err %v", conflicts, err)
		}
		if _, err := f.ws.PushWithLease(ctx, "feat/issue-42"); err != nil {
			t.Fatal(err)
		}
		if got := git(t, f.remote, "show", "feat/issue-42:README.md"); got != "trunk's line\nthe branch's line" {
			t.Errorf("README.md is %q", got)
		}
	})
	t.Run("a file still holding a conflict marker is not resolved, and the rebase waits", func(t *testing.T) {
		f := newFixture(t)
		f.diverged("README.md", "README.md")
		conflicts, _ := f.ws.Rebase(ctx)
		if _, err := f.ws.RebaseContinue(ctx, conflicts); err == nil || !strings.Contains(err.Error(), "README.md still holds a conflict marker") {
			t.Errorf("err %v", err)
		}
		if !isDir(filepath.Join(f.ws.Dir(), ".git", "rebase-merge")) {
			t.Error("the rebase is no longer in progress")
		}
	})
	t.Run("a command planted in the rebase's plan is never run", func(t *testing.T) {
		f := newFixture(t)
		f.diverged("README.md", "README.md")
		conflicts, _ := f.ws.Rebase(ctx)
		planted := filepath.Join(f.root, "planted")
		plan := filepath.Join(f.ws.Dir(), ".git", "rebase-merge", "git-rebase-todo")
		write(t, plan, "exec touch "+planted+"\n")
		write(t, filepath.Join(f.ws.Dir(), "README.md"), "resolved\n")
		if _, err := f.ws.RebaseContinue(ctx, conflicts); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(planted); err == nil {
			t.Error("the planted command ran")
		}
	})
	t.Run("lease push refused when remote moved after fetch", func(t *testing.T) {
		f := newFixture(t)
		f.diverged("feature.txt", "other.txt")
		if _, err := f.ws.Rebase(ctx); err != nil {
			t.Fatal(err)
		}
		// Someone pushes to the branch after the Daemon read it.
		git(t, f.other, "checkout", "--quiet", "feat/issue-42")
		write(t, filepath.Join(f.other, "theirs.txt"), "a human's change\n")
		git(t, f.other, "add", "-A")
		git(t, f.other, "commit", "--quiet", "-m", "a human's change")
		git(t, f.other, "push", "--quiet", "origin", "feat/issue-42")
		theirs := f.remoteRef("refs/heads/feat/issue-42")
		if _, err := f.ws.PushWithLease(ctx, "feat/issue-42"); !errors.Is(err, ErrLeaseRefused) {
			t.Fatalf("err %v", err)
		}
		if f.remoteRef("refs/heads/feat/issue-42") != theirs {
			t.Error("the human's push was overwritten")
		}
	})
	t.Run("abort restores the pre-rebase branch tip", func(t *testing.T) {
		f := newFixture(t)
		f.diverged("README.md", "README.md")
		before, _ := f.ws.Head(ctx)
		if _, err := f.ws.Rebase(ctx); err != nil {
			t.Fatal(err)
		}
		if err := f.ws.RebaseAbort(ctx); err != nil {
			t.Fatal(err)
		}
		if after, _ := f.ws.Head(ctx); after != before || isDir(filepath.Join(f.ws.Dir(), ".git", "rebase-merge")) {
			t.Errorf("head %s, was %s", after, before)
		}
		if err := f.ws.RebaseAbort(ctx); err != nil {
			t.Errorf("aborting no rebase: %v", err)
		}
	})
	t.Run("the default branch is never force-pushed", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		if _, err := f.ws.PushWithLease(ctx, "trunk"); err == nil {
			t.Error("it was")
		}
	})
}
