// Package workspace is the Daemon's own clone of the Target Project, inside
// the State dir. It refreshes the clone, opens a ticket's branch, and commits
// and pushes on Go's behalf, so that a Step never needs git write access and
// no checkout a human works in is ever touched.
package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// branchPrefix is the shape of a ticket's branch: feat/issue-<n>.
const branchPrefix = "feat/issue-"

// BranchName is the branch a ticket's work goes on.
func BranchName(issue int) string { return branchPrefix + strconv.Itoa(issue) }

// Author is who the Daemon's commits are by.
type Author struct {
	Name  string
	Email string
}

// Workspace is the Daemon's clone of one Target Project.
type Workspace struct {
	// StateDir is the Target Project's State dir; the clone lives in it.
	StateDir string
	// Remote is the URL the clone is made from and pushed to. It carries no
	// credential.
	Remote string
	// DefaultBranch is the branch work starts from.
	DefaultBranch string
	// Token authenticates fetches and pushes over HTTPS. It is handed to
	// git through the environment for the one command that needs it: never
	// as an argument, never written to disk or to the clone's config.
	Token string
	// Author is who commits are by.
	Author Author
	// Git is the git executable; empty means "git" on PATH.
	Git string
	// Environ is the environment git runs in; nil means os.Environ.
	Environ func() []string

	// pushed is where the remote's branches are known to be, by branch:
	// read after a fetch the Daemon made, or set by its own push. It lives
	// in memory because nothing in the clone can be trusted to say.
	pushed map[string]string
}

// Dir is the clone's directory.
func (w *Workspace) Dir() string { return filepath.Join(w.StateDir, "clone") }

// Commit is what CommitAndPush did.
type Commit struct {
	// SHA is the head of the branch as pushed; empty when there was nothing
	// to push.
	SHA string
}

// Empty reports that the checkout had no changes and the branch held nothing
// the remote lacks: nothing was committed and nothing pushed.
func (c Commit) Empty() bool { return c.SHA == "" }

// Prepare makes the clone ready to start work: it is created from the
// remote on first use and fetched after that, then reset to the default
// branch's head with every local change and stray file removed. A clone that
// is no longer one (its .git gone, or replaced) is made again. created
// reports whether this call made the clone.
func (w *Workspace) Prepare(ctx context.Context) (created bool, err error) {
	dir := w.Dir()
	if err := os.MkdirAll(w.gitHome(), 0o700); err != nil {
		return false, err
	}
	if !isDir(filepath.Join(dir, ".git")) {
		// Not there, or not a clone any more. It is the Daemon's own
		// directory and everything worth keeping is on the remote.
		if err := os.RemoveAll(dir); err != nil {
			return false, err
		}
		if _, err := w.run(ctx, command{dir: w.StateDir, remote: true, args: []string{"clone", "--quiet", "--origin", "origin", "--", w.Remote, dir}}); err != nil {
			os.RemoveAll(dir) // a half-made clone is worse than none
			return false, err
		}
		created = true
	}
	if err := w.harden(); err != nil {
		if created {
			return created, err
		}
		// Whatever is wrong with it, it is not a clone the Daemon can
		// trust. Start over.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return false, err
		}
		return w.Prepare(ctx)
	}
	w.pushed = map[string]string{}
	// A Fire that died mid-command leaves a lock, or a rebase or merge half
	// done. None of it is worth keeping: the remote is the truth.
	for _, leftover := range []string{"index.lock", "rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"} {
		os.RemoveAll(filepath.Join(dir, ".git", leftover))
	}
	if !created {
		// Forget what the clone believes the remote holds, then ask it: a
		// planted remote-tracking ref must not become a branch's start.
		os.RemoveAll(filepath.Join(dir, ".git", "refs", "remotes"))
		if _, err := w.gitRemote(ctx, "fetch", "--quiet", "--prune", "--", w.Remote, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return false, err
		}
	}
	base := w.tracking(w.DefaultBranch)
	for _, args := range [][]string{
		{"reset", "--quiet", "--hard"},
		{"clean", "--quiet", "-ffdx"},
		{"checkout", "--quiet", "--force", "-B", w.DefaultBranch, base},
		{"reset", "--quiet", "--hard", base},
	} {
		if _, err := w.git(ctx, args...); err != nil {
			return created, err
		}
	}
	return created, nil
}

// tracking is the remote-tracking ref of one of the remote's branches.
func (w *Workspace) tracking(branch string) string { return "refs/remotes/origin/" + branch }

// OpenBranch checks out the ticket's branch in a prepared clone and returns
// its name. A branch the remote already has is carried on from; otherwise it
// starts at the default branch's head.
func (w *Workspace) OpenBranch(ctx context.Context, issue int) (string, error) {
	if err := w.harden(); err != nil {
		return "", err
	}
	branch := BranchName(issue)
	start := w.tracking(w.DefaultBranch)
	if _, err := w.git(ctx, "rev-parse", "--verify", "--quiet", w.tracking(branch)); err == nil {
		start = w.tracking(branch)
	}
	if _, err := w.git(ctx, "checkout", "--quiet", "--force", "-B", branch, start); err != nil {
		return "", err
	}
	// The clone has been the Daemon's alone since Prepare, so this is where
	// the remote's branch is, or where a new one starts.
	head, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if w.pushed == nil {
		w.pushed = map[string]string{}
	}
	w.pushed[branch] = head
	return branch, nil
}

// Base is the ref a ticket's branch is compared with: the default branch as
// the remote had it when the clone was prepared.
func (w *Workspace) Base() string { return "origin/" + w.DefaultBranch }

// Messages are the messages of the commits the checked-out branch carries
// that the default branch does not, oldest first.
func (w *Workspace) Messages(ctx context.Context) ([]string, error) {
	out, err := w.git(ctx, "log", "--reverse", "--format=%B%x00", w.tracking(w.DefaultBranch)+"..HEAD")
	if err != nil {
		return nil, err
	}
	messages := []string{}
	for _, m := range strings.Split(out, "\x00") {
		if m = strings.TrimLeft(m, "\n"); m != "" {
			messages = append(messages, strings.TrimRight(m, "\n")+"\n")
		}
	}
	return messages, nil
}

// CommitAndPush commits every change in the checkout to branch with the
// given message and pushes the branch. An empty change set is not committed.
// The branch is still pushed when it holds commits the remote lacks (an
// earlier push that failed, a Step that committed), so that work is never
// stranded in the clone; with nothing to commit and nothing to push, the
// Commit is Empty. The push is never forced: if the remote's branch has
// moved on, it fails.
func (w *Workspace) CommitAndPush(ctx context.Context, branch, message string) (Commit, error) {
	if branch == "" || branch == w.DefaultBranch {
		return Commit{}, fmt.Errorf("the Daemon never commits to %q: work goes on a branch of its own", branch)
	}
	// A Step has been in the clone since it was prepared. Nothing it left
	// in .git may steer the commands below, which carry the token.
	if err := w.harden(); err != nil {
		return Commit{}, err
	}
	if err := w.flattenNestedRepos(); err != nil {
		return Commit{}, err
	}
	current, err := w.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return Commit{}, err
	}
	if current != branch {
		return Commit{}, fmt.Errorf("the clone is on %s, not on %s", current, branch)
	}
	if _, err := w.git(ctx, "add", "--all"); err != nil {
		return Commit{}, err
	}
	// diff --quiet exits 1 when something is staged.
	if _, err := w.git(ctx, "diff", "--cached", "--quiet"); err != nil {
		if _, err := w.run(ctx, command{dir: w.Dir(), stdin: message, args: []string{"commit", "--quiet", "--no-verify", "--file", "-"}}); err != nil {
			return Commit{}, err
		}
	}
	sha, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Commit{}, err
	}
	// Nothing to push when the head is where the branch was opened or last
	// pushed. That comes from memory, not from the clone's refs, which a
	// Step could set to make its work look pushed. With nothing remembered
	// the push is simply made: pushing what the remote has is harmless.
	if at, known := w.pushed[branch]; known && at == sha {
		return Commit{}, nil
	}
	// Push by URL and by full ref name: the destination is never read from
	// the clone.
	ref := "refs/heads/" + branch
	if _, err := w.gitRemote(ctx, "push", "--quiet", "--no-verify", "--", w.Remote, ref+":"+ref); err != nil {
		return Commit{}, err
	}
	if w.pushed == nil {
		w.pushed = map[string]string{}
	}
	w.pushed[branch] = sha
	return Commit{SHA: sha}, nil
}

// harden puts the clone's .git back under the Daemon's control. A Step works
// in the clone with a shell and can leave anything there, and git reads most
// of it: a config key can name a program to run or a proxy to send the token
// through, and a worktree setting can point git at another directory. So the
// config is rewritten to exactly what the Daemon needs, before every use.
func (w *Workspace) harden() error {
	dotGit := filepath.Join(w.Dir(), ".git")
	if !isDir(dotGit) {
		return fmt.Errorf("%s is not a clone: prepare it first", w.Dir())
	}
	// Per-worktree config, attributes that name filters, alternate object
	// stores, hooks, and anything included from the config are all gone or
	// unreachable once it is ours. Each is removed without following a
	// link: a directory the Step swapped for a symlink must not turn this
	// into a delete somewhere else.
	for _, extra := range [][]string{{"config.worktree"}, {"hooks"}, {"info", "attributes"}, {"objects", "info", "alternates"}, {"config"}} {
		if err := removeUnder(dotGit, extra...); err != nil {
			return err
		}
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n" +
		"[remote \"origin\"]\n\turl = " + w.Remote + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	return os.WriteFile(filepath.Join(dotGit, "config"), []byte(config), 0o644)
}

// removeUnder removes root/parts..., whatever it is, without leaving root:
// if a directory on the way is a symlink, the link is what gets removed.
func removeUnder(root string, parts ...string) error {
	path := root
	for i, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if last := i == len(parts)-1; last || !info.IsDir() {
			// The target itself, or something on the way that is not a
			// real directory. RemoveAll never follows a link it is given.
			return os.RemoveAll(path)
		}
	}
	return nil
}

// flattenNestedRepos removes any repository a Step created inside the
// checkout. Git would record one as a bare pointer and drop its files.
func (w *Workspace) flattenNestedRepos() error {
	root := w.Dir()
	var nested []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: git will say so if it matters
		}
		if d.Name() == ".git" && filepath.Dir(path) != root {
			nested = append(nested, path)
			return filepath.SkipDir
		}
		if path == filepath.Join(root, ".git") {
			return filepath.SkipDir
		}
		return nil
	})
	for _, path := range nested {
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return rmErr
		}
	}
	return err
}

// isDir reports whether path is a real directory: not missing, not a file
// and not a symlink to one.
func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// gitHome is an empty home directory for git, inside the State dir, so that
// nothing in the user's (a .netrc, say) is read and nothing there written.
func (w *Workspace) gitHome() string { return filepath.Join(w.StateDir, "git-home") }

// command is one git invocation.
type command struct {
	// dir is where it runs.
	dir string
	// remote marks a command that talks to the remote: only those are given
	// the token.
	remote bool
	stdin  string
	args   []string
}

// git runs a local git command in the clone.
func (w *Workspace) git(ctx context.Context, args ...string) (string, error) {
	return w.run(ctx, command{dir: w.Dir(), args: args})
}

// gitRemote runs, in the clone, a git command that talks to the remote.
func (w *Workspace) gitRemote(ctx context.Context, args ...string) (string, error) {
	return w.run(ctx, command{dir: w.Dir(), remote: true, args: args})
}

// killGrace is how long a cancelled git command's pipes are waited on.
const killGrace = 5 * time.Second

// run runs one git command and returns its trimmed output.
func (w *Workspace) run(ctx context.Context, c command) (string, error) {
	binary := w.Git
	if binary == "" {
		binary = "git"
	}
	cmd := exec.CommandContext(ctx, binary, c.args...)
	cmd.Dir = c.dir
	cmd.Env = w.env(c)
	cmd.Stdin = strings.NewReader(c.stdin)
	// Cancel the whole process group: git's transport helpers hold the
	// pipes and would keep a cancelled call waiting.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = killGrace
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", c.args[0], w.scrub(detail))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// scrub removes the token from text that is about to leave the package.
func (w *Workspace) scrub(text string) string {
	if w.Token == "" {
		return text
	}
	return strings.NewReplacer(w.Token, "<token>", basicAuth(w.Token), "<token>").Replace(text)
}

func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// inherited are the only variables git is given from the Daemon's own
// environment: where programs are, the locale, and how this network reaches
// the outside. Everything else stays behind, the GitHub token first of all:
// git's own commands never need it, and the one that does gets it below.
var inherited = map[string]bool{
	"PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TMPDIR": true, "TZ": true,
	"HTTPS_PROXY": true, "https_proxy": true, "NO_PROXY": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
}

// env is the environment for one git command. Git is cut off from the
// user's and the system's config and given a home of its own. The settings
// passed here outrank the clone's config: no hooks, no filesystem monitor,
// no credential helper, no prompt, no redirect to another host, and only
// the remote's own protocol. A command that talks to the remote also gets
// the token, as an HTTP header scoped to the remote's host.
func (w *Workspace) env(c command) []string {
	environ := os.Environ
	if w.Environ != nil {
		environ = w.Environ
	}
	var env []string
	for _, kv := range environ() {
		if key, _, _ := strings.Cut(kv, "="); inherited[key] {
			env = append(env, kv)
		}
	}
	settings := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"credential.helper", ""},
		{"commit.gpgsign", "false"},
		{"http.followRedirects", "false"},
	}
	protocol, origin := transport(w.Remote)
	if c.remote && w.Token != "" && origin != "" {
		settings = append(settings, [2]string{"http." + origin + "/.extraheader", "Authorization: Basic " + basicAuth(w.Token)})
	}
	env = append(env,
		"HOME="+w.gitHome(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+protocol,
		"GIT_AUTHOR_NAME="+w.Author.Name, "GIT_AUTHOR_EMAIL="+w.Author.Email,
		"GIT_COMMITTER_NAME="+w.Author.Name, "GIT_COMMITTER_EMAIL="+w.Author.Email,
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(settings)),
	)
	if c.dir == w.Dir() {
		// Say where the repository and its files are, so nothing in the
		// clone can say otherwise.
		env = append(env, "GIT_DIR="+filepath.Join(w.Dir(), ".git"), "GIT_WORK_TREE="+w.Dir())
	}
	for i, kv := range settings {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// transport reads a remote's protocol, which is the only one git is then
// allowed to speak, and for a remote the token may be sent to, its scheme
// and host, such as "https://github.com". The token goes only to an HTTPS
// remote; plain HTTP qualifies on this machine alone, where the tests'
// remote is. Any other remote gets no token.
func transport(remote string) (protocol, tokenOrigin string) {
	if strings.Contains(remote, "::") {
		return "none", "" // a transport helper, which runs a program
	}
	u, err := url.Parse(remote)
	if err != nil || u.Scheme == "" || !strings.Contains(remote, "://") {
		if i := strings.Index(remote, ":"); i > 0 && !strings.Contains(remote[:i], "/") {
			return "ssh", "" // user@host:path
		}
		return "file", ""
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"
	secure := u.Scheme == "https" || (u.Scheme == "http" && local)
	if !secure || u.Host == "" || u.User != nil {
		return u.Scheme, ""
	}
	return u.Scheme, u.Scheme + "://" + u.Host
}

// Discard puts the checkout back to its last commit: every change and every
// new file a read-only Step's commands left in it is dropped. Ignored files
// stay; nothing commits them.
func (w *Workspace) Discard(ctx context.Context) error {
	if err := w.harden(); err != nil {
		return err
	}
	if _, err := w.git(ctx, "reset", "--quiet", "--hard", "HEAD"); err != nil {
		return err
	}
	_, err := w.git(ctx, "clean", "--quiet", "--force", "-d")
	return err
}

// Head is the commit the checkout is on.
func (w *Workspace) Head(ctx context.Context) (string, error) {
	if err := w.harden(); err != nil {
		return "", err
	}
	return w.git(ctx, "rev-parse", "HEAD")
}

// ChangedPaths are the paths the checked-out branch changes against the
// default branch, as the pull request's diff has them.
func (w *Workspace) ChangedPaths(ctx context.Context) ([]string, error) {
	if err := w.harden(); err != nil {
		return nil, err
	}
	out, err := w.git(ctx, "diff", "--name-only", "--no-renames", w.Base()+"...HEAD")
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, path := range strings.Split(out, "\n") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// ErrNotChanged is a file the branch does not change: it has no line a
// review comment could anchor to.
var ErrNotChanged = errors.New("the branch does not change the file")

var hunkHeader = regexp.MustCompile(`(?m)^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// FirstChangedLine is the first line of path a review comment can anchor to
// in the branch's diff: the head-side start of the first hunk that adds a
// line (side RIGHT), else the base-side start of the first hunk that deletes
// one (side LEFT), for a file the branch deleted or only took lines out of.
func (w *Workspace) FirstChangedLine(ctx context.Context, path string) (line int, side string, err error) {
	if err := w.harden(); err != nil {
		return 0, "", err
	}
	diff, err := w.git(ctx, "diff", "--unified=0", "--no-renames", w.Base()+"...HEAD", "--", path)
	if err != nil {
		return 0, "", err
	}
	count := func(s string) int {
		if s == "" {
			return 1
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	deleted := 0
	for _, h := range hunkHeader.FindAllStringSubmatch(diff, -1) {
		oldStart, _ := strconv.Atoi(h[1])
		newStart, _ := strconv.Atoi(h[3])
		if count(h[4]) > 0 {
			return newStart, "RIGHT", nil
		}
		if deleted == 0 && count(h[2]) > 0 {
			deleted = oldStart
		}
	}
	if deleted > 0 {
		return deleted, "LEFT", nil
	}
	return 0, "", fmt.Errorf("%s: %w", path, ErrNotChanged)
}
