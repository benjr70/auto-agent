package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wipBranch makes the remote hold ticket 7's branch with two commits of
// partial work on trunk (a wip commit, then one a Step made itself), and
// the Workspace has it open, as a Fire resuming the ticket does.
func (f *fixture) wipBranch() {
	f.t.Helper()
	git(f.t, f.other, "checkout", "--quiet", "-b", "feat/issue-7")
	write(f.t, filepath.Join(f.other, "a.txt"), "a\n")
	git(f.t, f.other, "add", "-A")
	git(f.t, f.other, "commit", "--quiet", "-m", "wip: partial work on #7")
	write(f.t, filepath.Join(f.other, "b.txt"), "b\n")
	git(f.t, f.other, "add", "-A")
	git(f.t, f.other, "commit", "--quiet", "-m", "test(a): the Step's own commit")
	git(f.t, f.other, "push", "--quiet", "origin", "feat/issue-7")
	git(f.t, f.other, "checkout", "--quiet", "trunk")
	if _, err := f.ws.Prepare(ctx); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.ws.OpenBranch(ctx, 7); err != nil {
		f.t.Fatal(err)
	}
}

func TestSquashAndPush(t *testing.T) {
	message := "feat(a): the finished thing\n\nCloses #7\n"
	t.Run("the branch's commits and the checkout become one commit on trunk, pushed with a lease", func(t *testing.T) {
		f := newFixture(t)
		f.wipBranch()
		write(t, filepath.Join(f.ws.Dir(), "c.txt"), "c\n")
		commit, err := f.ws.SquashAndPush(ctx, "feat/issue-7", message)
		if err != nil || commit.Empty() {
			t.Fatalf("SquashAndPush = %+v, %v", commit, err)
		}
		if got := f.remoteRef("refs/heads/feat/issue-7"); got != commit.SHA {
			t.Errorf("the remote's branch is at %q, want %s", got, commit.SHA)
		}
		if parent := git(t, f.remote, "rev-parse", "feat/issue-7^"); parent != f.remoteRef("refs/heads/trunk") {
			t.Errorf("the commit's parent is %s, not trunk's head: the branch carries more than one commit", parent)
		}
		if got := git(t, f.remote, "log", "-1", "--format=%B", "feat/issue-7"); strings.TrimSpace(got) != strings.TrimSpace(message) {
			t.Errorf("message = %q", got)
		}
		if got := git(t, f.remote, "show", "--stat", "--format=", "feat/issue-7"); !strings.Contains(got, "a.txt") || !strings.Contains(got, "b.txt") || !strings.Contains(got, "c.txt") {
			t.Errorf("the commit holds:\n%s", got)
		}
		log, _ := os.ReadFile(f.argLog)
		leased := false
		for _, line := range strings.Split(string(log), "\n") {
			if strings.HasPrefix(line, "push ") {
				if strings.Contains(line, " --force ") {
					t.Errorf("pushed as %s", line)
				}
				leased = leased || strings.Contains(line, "--force-with-lease=refs/heads/feat/issue-7:")
			}
		}
		if !leased {
			t.Error("the push carried no lease")
		}
	})
	t.Run("a branch the remote moved meanwhile refuses the lease", func(t *testing.T) {
		f := newFixture(t)
		f.wipBranch()
		git(t, f.other, "checkout", "--quiet", "feat/issue-7")
		write(t, filepath.Join(f.other, "d.txt"), "d\n")
		git(t, f.other, "add", "-A")
		git(t, f.other, "commit", "--quiet", "-m", "wip: someone else")
		git(t, f.other, "push", "--quiet", "origin", "feat/issue-7")
		moved := f.remoteRef("refs/heads/feat/issue-7")
		if _, err := f.ws.SquashAndPush(ctx, "feat/issue-7", message); err == nil {
			t.Fatal("the push landed over a branch that moved")
		}
		if f.remoteRef("refs/heads/feat/issue-7") != moved {
			t.Error("the remote's branch was overwritten")
		}
	})
	t.Run("a branch with no commit of its own is committed and pushed plainly", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, _ := f.ws.OpenBranch(ctx, 7)
		write(t, filepath.Join(f.ws.Dir(), "a.txt"), "a\n")
		commit, err := f.ws.SquashAndPush(ctx, branch, message)
		if err != nil || commit.Empty() || f.remoteRef("refs/heads/feat/issue-7") != commit.SHA {
			t.Fatalf("SquashAndPush = %+v, %v; remote %q", commit, err, f.remoteRef("refs/heads/feat/issue-7"))
		}
		if log, _ := os.ReadFile(f.argLog); strings.Contains(string(log), "--force-with-lease") {
			t.Error("a first push carried a lease")
		}
	})
	t.Run("commits a Step made on a branch the remote never had are squashed and pushed plainly", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, _ := f.ws.OpenBranch(ctx, 7)
		write(t, filepath.Join(f.ws.Dir(), "a.txt"), "a\n")
		git(t, f.ws.Dir(), "add", "-A")
		git(t, f.ws.Dir(), "-c", "user.name=s", "-c", "user.email=s@x", "commit", "--quiet", "-m", "wip: the Step's")
		commit, err := f.ws.SquashAndPush(ctx, branch, message)
		if err != nil || commit.Empty() || f.remoteRef("refs/heads/feat/issue-7") != commit.SHA {
			t.Fatalf("SquashAndPush = %+v, %v; remote %q", commit, err, f.remoteRef("refs/heads/feat/issue-7"))
		}
		if parent := git(t, f.remote, "rev-parse", "feat/issue-7^"); parent != f.remoteRef("refs/heads/trunk") {
			t.Errorf("the commit's parent is %s, not trunk's head", parent)
		}
		if got := git(t, f.remote, "log", "-1", "--format=%s", "feat/issue-7"); got != "feat(a): the finished thing" {
			t.Errorf("subject = %q", got)
		}
	})
	t.Run("work that squashes to no change is an error, and nothing is pushed", func(t *testing.T) {
		f := newFixture(t)
		f.wipBranch()
		before := f.remoteRef("refs/heads/feat/issue-7")
		os.Remove(filepath.Join(f.ws.Dir(), "a.txt"))
		os.Remove(filepath.Join(f.ws.Dir(), "b.txt"))
		if _, err := f.ws.SquashAndPush(ctx, "feat/issue-7", message); err == nil {
			t.Fatal("no change landed as a commit")
		}
		if f.remoteRef("refs/heads/feat/issue-7") != before {
			t.Error("the remote's branch moved")
		}
	})
}
