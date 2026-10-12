package workspace

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const token = "ghp_workspace_test_token"

var ctx = context.Background()

// git runs git for the test's own setup, hermetically.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Upstream", "GIT_AUTHOR_EMAIL=up@example.com", "GIT_COMMITTER_NAME=Upstream", "GIT_COMMITTER_EMAIL=up@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fixture is a bare remote with one commit on trunk, a second checkout that
// stands in for other people pushing to it, and a Workspace pointed at it.
type fixture struct {
	t      *testing.T
	root   string
	remote string // the bare repository
	other  string // someone else's checkout of it
	argLog string // every argument the Workspace passed to git
	ws     *Workspace
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, remote: filepath.Join(root, "remote.git"), other: filepath.Join(root, "other"), argLog: filepath.Join(root, "git-args.log")}
	git(t, root, "init", "--quiet", "--bare", "--initial-branch=trunk", f.remote)
	git(t, root, "clone", "--quiet", f.remote, f.other)
	write(t, filepath.Join(f.other, "README.md"), "one\n")
	git(t, f.other, "add", "-A")
	git(t, f.other, "commit", "--quiet", "-m", "first")
	git(t, f.other, "push", "--quiet", "origin", "HEAD:trunk")

	// A git that records its arguments, to show what never appears in them.
	wrapper := filepath.Join(root, "git-wrapper")
	write(t, wrapper, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+f.argLog+"'\nexec git \"$@\"\n")
	f.ws = f.workspace(f.remote)
	f.ws.Git = wrapper
	return f
}

func (f *fixture) workspace(remote string) *Workspace {
	return &Workspace{
		StateDir: filepath.Join(f.root, "state"), Remote: remote, DefaultBranch: "trunk", Token: token,
		Author:  Author{Name: "machine-user", Email: "machine-user@users.noreply.github.com"},
		Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(f.root, "home")} },
	}
}

// upstream adds a commit to the remote's trunk from the other checkout.
func (f *fixture) upstream(file, content, message string) string {
	f.t.Helper()
	git(f.t, f.other, "pull", "--quiet", "origin", "trunk")
	write(f.t, filepath.Join(f.other, file), content)
	git(f.t, f.other, "add", "-A")
	git(f.t, f.other, "commit", "--quiet", "-m", message)
	git(f.t, f.other, "push", "--quiet", "origin", "HEAD:trunk")
	return git(f.t, f.other, "rev-parse", "HEAD")
}

func (f *fixture) remoteRef(ref string) string {
	cmd := exec.Command("git", "-C", f.remote, "rev-parse", "--verify", "--quiet", ref)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func TestPrepare(t *testing.T) {
	t.Run("first use clones; second use refreshes", func(t *testing.T) {
		f := newFixture(t)
		created, err := f.ws.Prepare(ctx)
		if err != nil || !created {
			t.Fatalf("first Prepare = %v, %v", created, err)
		}
		clone := f.ws.Dir()
		if clone != filepath.Join(f.root, "state", "clone") {
			t.Errorf("Dir = %q, want it under the State dir", clone)
		}
		if got, _ := os.ReadFile(filepath.Join(clone, "README.md")); string(got) != "one\n" {
			t.Errorf("README.md = %q", got)
		}
		head := f.upstream("NEW.md", "two\n", "second")
		created, err = f.ws.Prepare(ctx)
		if err != nil || created {
			t.Fatalf("second Prepare = %v, %v", created, err)
		}
		if got := git(t, clone, "rev-parse", "HEAD"); got != head {
			t.Errorf("HEAD = %s, want the remote's new head %s", got, head)
		}
		if got := git(t, clone, "rev-parse", "--abbrev-ref", "HEAD"); got != "trunk" {
			t.Errorf("branch = %q", got)
		}
	})
	t.Run("starting work resets the clone to the default branch's head", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		clone := f.ws.Dir()
		// What a Fire that died half-way leaves behind.
		git(t, clone, "checkout", "--quiet", "-b", "feat/issue-7")
		write(t, filepath.Join(clone, "README.md"), "half-edited\n")
		write(t, filepath.Join(clone, "untracked.txt"), "junk\n")
		write(t, filepath.Join(clone, "ignored", "build.o"), "junk\n")
		write(t, filepath.Join(clone, ".gitignore"), "ignored/\n")
		git(t, clone, "add", "README.md")
		if _, err := f.ws.Prepare(ctx); err != nil {
			t.Fatal(err)
		}
		if status := git(t, clone, "status", "--porcelain", "--ignored"); status != "" {
			t.Errorf("the clone is not clean:\n%s", status)
		}
		if got, _ := os.ReadFile(filepath.Join(clone, "README.md")); string(got) != "one\n" || git(t, clone, "rev-parse", "--abbrev-ref", "HEAD") != "trunk" {
			t.Errorf("README.md = %q on %s", got, git(t, clone, "rev-parse", "--abbrev-ref", "HEAD"))
		}
	})
	t.Run("nothing outside the State dir is touched", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, _ := f.ws.OpenBranch(ctx, 7)
		write(t, filepath.Join(f.ws.Dir(), "a.txt"), "a\n")
		if _, err := f.ws.CommitAndPush(ctx, branch, "feat: a"); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(f.root)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if got := strings.Join(names, " "); got != "git-args.log git-wrapper other remote.git state" {
			t.Errorf("the test root now holds %q: something was written outside the State dir (HOME would be %q)", got, "home")
		}
	})
	t.Run("a clone of some other repo is refused, not reset", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		elsewhere := f.workspace(filepath.Join(f.root, "another.git"))
		if _, err := elsewhere.Prepare(ctx); err == nil || !strings.Contains(err.Error(), "is a clone of") {
			t.Errorf("Prepare = %v", err)
		}
	})
	t.Run("a remote that cannot be reached fails the Prepare; the clone is left as it was", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		before := git(t, f.ws.Dir(), "rev-parse", "HEAD")
		os.Rename(f.remote, f.remote+".gone")
		if _, err := f.ws.Prepare(ctx); err == nil {
			t.Error("Prepare succeeded with the remote gone")
		}
		if got := git(t, f.ws.Dir(), "rev-parse", "HEAD"); got != before {
			t.Errorf("HEAD moved to %s", got)
		}
	})
}

func TestBranch(t *testing.T) {
	t.Run("a ticket's branch uses today's feat/issue-<n> shape", func(t *testing.T) {
		if got := BranchName(108); got != "feat/issue-108" {
			t.Errorf("BranchName = %q", got)
		}
	})
	t.Run("a new branch starts at the default branch's head", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, err := f.ws.OpenBranch(ctx, 7)
		clone := f.ws.Dir()
		if err != nil || branch != "feat/issue-7" || git(t, clone, "rev-parse", "--abbrev-ref", "HEAD") != "feat/issue-7" ||
			git(t, clone, "rev-parse", "HEAD") != git(t, clone, "rev-parse", "origin/trunk") {
			t.Errorf("OpenBranch = %q, %v", branch, err)
		}
	})
	t.Run("a branch the remote already has is carried on from, not started over", func(t *testing.T) {
		f := newFixture(t)
		git(t, f.other, "checkout", "--quiet", "-b", "feat/issue-7")
		write(t, filepath.Join(f.other, "wip.txt"), "earlier work\n")
		git(t, f.other, "add", "-A")
		git(t, f.other, "commit", "--quiet", "-m", "earlier work")
		git(t, f.other, "push", "--quiet", "origin", "feat/issue-7")
		f.ws.Prepare(ctx)
		if _, err := f.ws.OpenBranch(ctx, 7); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(f.ws.Dir(), "wip.txt")); string(got) != "earlier work\n" {
			t.Errorf("wip.txt = %q: the earlier work is not in the clone", got)
		}
	})
}

func TestCommitAndPush(t *testing.T) {
	t.Run("commit and push against a local bare remote", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, _ := f.ws.OpenBranch(ctx, 7)
		clone := f.ws.Dir()
		write(t, filepath.Join(clone, "src", "new.go"), "package src\n")
		write(t, filepath.Join(clone, "README.md"), "edited\n")
		message := "feat(pick): the new thing (#7)\n\nA body with \"quotes\" and a\nsecond line.\n"
		commit, err := f.ws.CommitAndPush(ctx, branch, message)
		if err != nil || commit.Empty || commit.SHA == "" {
			t.Fatalf("CommitAndPush = %+v, %v", commit, err)
		}
		if got := f.remoteRef("refs/heads/feat/issue-7"); got != commit.SHA {
			t.Errorf("the remote's branch is at %q, want %s", got, commit.SHA)
		}
		if got := git(t, f.remote, "log", "-1", "--format=%B%an <%ae>|%cn", "feat/issue-7"); got != strings.TrimSpace(message)+"\nmachine-user <machine-user@users.noreply.github.com>|machine-user" {
			t.Errorf("commit = %q", got)
		}
		if got := git(t, f.remote, "show", "--stat", "--format=", "feat/issue-7"); !strings.Contains(got, "src/new.go") || !strings.Contains(got, "README.md") {
			t.Errorf("the commit holds:\n%s", got)
		}
		if got := f.remoteRef("refs/heads/trunk"); got != git(t, clone, "rev-parse", "origin/trunk") {
			t.Errorf("trunk moved to %s", got)
		}
	})
	t.Run("an empty change set is reported, not committed", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, _ := f.ws.OpenBranch(ctx, 7)
		before := git(t, f.ws.Dir(), "rev-parse", "HEAD")
		commit, err := f.ws.CommitAndPush(ctx, branch, "feat: nothing")
		if err != nil || !commit.Empty || commit.SHA != "" {
			t.Errorf("CommitAndPush = %+v, %v", commit, err)
		}
		if git(t, f.ws.Dir(), "rev-parse", "HEAD") != before || f.remoteRef("refs/heads/feat/issue-7") != "" {
			t.Error("an empty change set was committed or pushed")
		}
	})
	t.Run("a second commit on the same branch is pushed on top of the first", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, _ := f.ws.OpenBranch(ctx, 7)
		write(t, filepath.Join(f.ws.Dir(), "a.txt"), "a\n")
		first, _ := f.ws.CommitAndPush(ctx, branch, "feat: a")
		write(t, filepath.Join(f.ws.Dir(), "b.txt"), "b\n")
		second, err := f.ws.CommitAndPush(ctx, branch, "fix: b")
		if err != nil || git(t, f.remote, "rev-parse", "feat/issue-7~1") != first.SHA || f.remoteRef("refs/heads/feat/issue-7") != second.SHA {
			t.Errorf("second = %+v, %v", second, err)
		}
	})
	t.Run("a push is never forced here", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, _ := f.ws.OpenBranch(ctx, 7)
		// Someone pushes the branch first, with different history.
		git(t, f.other, "checkout", "--quiet", "-b", "feat/issue-7")
		write(t, filepath.Join(f.other, "theirs.txt"), "theirs\n")
		git(t, f.other, "add", "-A")
		git(t, f.other, "commit", "--quiet", "-m", "theirs")
		git(t, f.other, "push", "--quiet", "origin", "feat/issue-7")
		theirs := f.remoteRef("refs/heads/feat/issue-7")

		write(t, filepath.Join(f.ws.Dir(), "ours.txt"), "ours\n")
		_, err := f.ws.CommitAndPush(ctx, branch, "feat: ours")
		if err == nil || !strings.Contains(err.Error(), "push") {
			t.Errorf("CommitAndPush = %v, want the push refused", err)
		}
		if got := f.remoteRef("refs/heads/feat/issue-7"); got != theirs {
			t.Errorf("the remote's branch moved from %s to %s", theirs, got)
		}
		log, _ := os.ReadFile(f.argLog)
		for _, line := range strings.Split(string(log), "\n") {
			if strings.HasPrefix(line, "push") && (strings.Contains(line, "--force") || strings.Contains(line, " -f") || strings.Contains(line, " +")) {
				t.Errorf("a forced push was attempted: git %s", line)
			}
		}
	})
	t.Run("committing on a branch other than the one named is refused", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		f.ws.OpenBranch(ctx, 7)
		write(t, filepath.Join(f.ws.Dir(), "a.txt"), "a\n")
		if _, err := f.ws.CommitAndPush(ctx, "feat/issue-8", "feat: a"); err == nil || !strings.Contains(err.Error(), "feat/issue-7") {
			t.Errorf("CommitAndPush = %v", err)
		}
		if _, err := f.ws.CommitAndPush(ctx, "trunk", "feat: a"); err == nil {
			t.Error("a commit to the default branch was allowed")
		}
	})
	t.Run("hooks and config a Step planted in the clone are not run or obeyed", func(t *testing.T) {
		f := newFixture(t)
		f.ws.Prepare(ctx)
		branch, _ := f.ws.OpenBranch(ctx, 7)
		clone := f.ws.Dir()
		proof := filepath.Join(f.root, "state", "hook-ran")
		for _, hook := range []string{"pre-commit", "commit-msg", "pre-push", "post-commit"} {
			write(t, filepath.Join(clone, ".git", "hooks", hook), "#!/bin/sh\necho \"$GIT_CONFIG_VALUE_0\" > '"+proof+"'\n")
		}
		git(t, clone, "config", "core.fsmonitor", "sh -c 'echo fsmonitor > "+proof+"'")
		git(t, clone, "config", "remote.origin.pushurl", filepath.Join(f.root, "state", "attacker.git"))
		write(t, filepath.Join(clone, "a.txt"), "a\n")
		commit, err := f.ws.CommitAndPush(ctx, branch, "feat: a")
		if err != nil || f.remoteRef("refs/heads/feat/issue-7") != commit.SHA {
			t.Errorf("CommitAndPush = %+v, %v; the remote has %q", commit, err, f.remoteRef("refs/heads/feat/issue-7"))
		}
		if got, err := os.ReadFile(proof); err == nil {
			t.Errorf("planted code ran and saw %q", got)
		}
	})
}

// authRemote serves the fixture's bare remote over HTTP, as GitHub does, and
// refuses every request that does not carry the token.
func authRemote(t *testing.T, f *fixture) (url string, sawToken *bool) {
	t.Helper()
	execPath := git(t, f.root, "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("git-http-backend is not installed (%s): the token test cannot run", backend)
		}
		t.Skipf("git-http-backend is not installed (%s)", backend)
	}
	git(t, f.remote, "config", "http.receivepack", "true")
	seen := false
	handler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + f.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || pass != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		seen = true
		r.Header.Del("Authorization") // the CGI must not see it
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/remote.git", &seen
}

func TestToken(t *testing.T) {
	f := newFixture(t)
	url, sawToken := authRemote(t, f)
	f.ws.Remote = url
	if _, err := f.ws.Prepare(ctx); err != nil {
		t.Fatalf("Prepare over HTTP: %v", err)
	}
	branch, _ := f.ws.OpenBranch(ctx, 7)
	write(t, filepath.Join(f.ws.Dir(), "a.txt"), "a\n")
	commit, err := f.ws.CommitAndPush(ctx, branch, "feat: a")

	t.Run("the push authenticates with the Daemon's token", func(t *testing.T) {
		if err != nil || !*sawToken || f.remoteRef("refs/heads/feat/issue-7") != commit.SHA {
			t.Errorf("CommitAndPush = %+v, %v; token seen %v", commit, err, *sawToken)
		}
	})
	t.Run("no token in the clone's config after a push", func(t *testing.T) {
		filepath.Walk(filepath.Join(f.root, "state"), func(path string, info os.FileInfo, err error) error {
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if data, _ := os.ReadFile(path); strings.Contains(string(data), token) || strings.Contains(string(data), basicAuth(token)) {
				t.Errorf("%s holds the token", path)
			}
			return nil
		})
		if got := git(t, f.ws.Dir(), "remote", "get-url", "origin"); got != url {
			t.Errorf("origin = %q", got)
		}
	})
	t.Run("the token is never an argument, where any user could read it", func(t *testing.T) {
		log, _ := os.ReadFile(f.argLog)
		if strings.Contains(string(log), token) || strings.Contains(string(log), basicAuth(token)) || len(log) == 0 {
			t.Errorf("git's arguments:\n%s", log)
		}
	})
	t.Run("without the token the remote refuses, and the error does not ask a terminal", func(t *testing.T) {
		anonymous := f.workspace(url)
		anonymous.StateDir = filepath.Join(f.root, "state", "anonymous")
		anonymous.Token = ""
		if _, err := anonymous.Prepare(ctx); err == nil {
			t.Error("a clone with no token succeeded against a remote that requires one")
		}
	})
	t.Run("a wrong token's error never repeats it", func(t *testing.T) {
		wrong := f.workspace(url)
		wrong.StateDir = filepath.Join(f.root, "state", "wrong")
		wrong.Token = "ghp_some_other_token"
		_, err := wrong.Prepare(ctx)
		if err == nil || strings.Contains(err.Error(), "ghp_some_other_token") || strings.Contains(err.Error(), basicAuth("ghp_some_other_token")) {
			t.Errorf("Prepare = %v", err)
		}
	})
}

func TestTokenHandling(t *testing.T) {
	holdsToken := func(env []string) bool {
		return strings.Contains(strings.Join(env, "\n"), basicAuth(token)) || strings.Contains(strings.Join(env, "\n"), token)
	}
	ws := &Workspace{Remote: "https://github.com/acme/widgets.git", Token: token, Environ: func() []string {
		return []string{"PATH=/usr/bin", "GIT_DIR=/elsewhere", "GIT_SSH_COMMAND=ssh -i key", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/evil"}
	}}
	t.Run("only a command that talks to the remote is given the token", func(t *testing.T) {
		if holdsToken(ws.env(false)) || !holdsToken(ws.env(true)) {
			t.Errorf("local env holds the token: %v; network env: %v", holdsToken(ws.env(false)), holdsToken(ws.env(true)))
		}
	})
	t.Run("the token is scoped to the remote's host", func(t *testing.T) {
		if env := strings.Join(ws.env(true), "\n"); !strings.Contains(env, "=http.https://github.com/.extraheader") {
			t.Errorf("env = %s", env)
		}
	})
	t.Run("a remote that is not HTTP is never sent the token", func(t *testing.T) {
		for _, remote := range []string{"git@github.com:acme/widgets.git", "/srv/git/widgets.git", "ssh://git@github.com/acme/widgets", "file:///srv/widgets.git"} {
			other := &Workspace{Remote: remote, Token: token, Environ: ws.Environ}
			if holdsToken(other.env(true)) {
				t.Errorf("the token is in the environment for %s", remote)
			}
		}
	})
	t.Run("nothing inherited steers git", func(t *testing.T) {
		env := strings.Join(ws.env(true), "\n")
		if strings.Contains(env, "/elsewhere") || strings.Contains(env, "ssh -i key") || strings.Contains(env, "/evil") || !strings.Contains(env, "PATH=/usr/bin") {
			t.Errorf("env = %s", env)
		}
	})
	t.Run("the token is scrubbed from any text that leaves the package", func(t *testing.T) {
		got := ws.scrub("fatal: bad header 'Authorization: Basic " + basicAuth(token) + "' for " + token)
		if strings.Contains(got, token) || strings.Contains(got, basicAuth(token)) || !strings.Contains(got, "<token>") {
			t.Errorf("scrub = %q", got)
		}
	})
}
