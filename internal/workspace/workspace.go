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
	"strconv"
	"strings"
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
}

// Dir is the clone's directory.
func (w *Workspace) Dir() string { return filepath.Join(w.StateDir, "clone") }

// Commit is what CommitAndPush did.
type Commit struct {
	// SHA is the commit pushed; empty when there was nothing to commit.
	SHA string
	// Empty is set when the checkout had no changes: nothing was committed
	// and nothing pushed.
	Empty bool
}

// Prepare makes the clone ready to start work: it is created from the
// remote on first use and fetched after that, then reset to the default
// branch's head with every local change and stray file removed. created
// reports whether this call made the clone.
func (w *Workspace) Prepare(ctx context.Context) (created bool, err error) {
	dir := w.Dir()
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); errors.Is(statErr, os.ErrNotExist) {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			return false, fmt.Errorf("%s exists and is not a clone", dir)
		}
		if err := os.MkdirAll(w.StateDir, 0o755); err != nil {
			return false, err
		}
		if _, err := w.git(ctx, w.StateDir, true, "clone", "--quiet", "--origin", "origin", "--", w.Remote, dir); err != nil {
			os.RemoveAll(dir) // a half-made clone is worse than none
			return false, err
		}
		created = true
	} else {
		origin, err := w.git(ctx, dir, false, "config", "--local", "--get", "remote.origin.url")
		if err != nil {
			return false, err
		}
		if origin != w.Remote {
			return false, fmt.Errorf("%s is a clone of %s, not of %s", dir, origin, w.Remote)
		}
		// Fetch by URL, so nothing in the clone's config chooses where from.
		if _, err := w.git(ctx, dir, true, "fetch", "--quiet", "--prune", "--", w.Remote, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return false, err
		}
	}
	base := "origin/" + w.DefaultBranch
	for _, args := range [][]string{
		{"reset", "--quiet", "--hard"},
		{"clean", "--quiet", "-ffdx"},
		{"checkout", "--quiet", "--force", "-B", w.DefaultBranch, base},
		{"reset", "--quiet", "--hard", base},
	} {
		if _, err := w.git(ctx, dir, false, args...); err != nil {
			return created, err
		}
	}
	return created, nil
}

// OpenBranch checks out the ticket's branch in a prepared clone and returns
// its name. A branch the remote already has is carried on from; otherwise it
// starts at the default branch's head.
func (w *Workspace) OpenBranch(ctx context.Context, issue int) (string, error) {
	branch := BranchName(issue)
	start := "origin/" + w.DefaultBranch
	if _, err := w.git(ctx, w.Dir(), false, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		start = "origin/" + branch
	}
	if _, err := w.git(ctx, w.Dir(), false, "checkout", "--quiet", "--force", "-B", branch, start); err != nil {
		return "", err
	}
	return branch, nil
}

// CommitAndPush commits every change in the checkout to branch with the
// given message and pushes the branch. An empty change set is reported and
// nothing is committed or pushed. The push is never forced: if the remote's
// branch has moved on, it fails.
func (w *Workspace) CommitAndPush(ctx context.Context, branch, message string) (Commit, error) {
	dir := w.Dir()
	if !strings.HasPrefix(branch, branchPrefix) {
		return Commit{}, fmt.Errorf("%q is not a ticket's branch: the Daemon commits only to %s<n>", branch, branchPrefix)
	}
	current, err := w.git(ctx, dir, false, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return Commit{}, err
	}
	if current != branch {
		return Commit{}, fmt.Errorf("the clone is on %s, not on %s", current, branch)
	}
	if _, err := w.git(ctx, dir, false, "add", "--all"); err != nil {
		return Commit{}, err
	}
	// diff --quiet exits 1 when something is staged.
	if _, err := w.git(ctx, dir, false, "diff", "--cached", "--quiet"); err == nil {
		return Commit{Empty: true}, nil
	}
	if _, err := w.gitInput(ctx, dir, false, message, "commit", "--quiet", "--no-verify", "--file", "-"); err != nil {
		return Commit{}, err
	}
	sha, err := w.git(ctx, dir, false, "rev-parse", "HEAD")
	if err != nil {
		return Commit{}, err
	}
	// Push by URL and by full ref name: neither a pushurl nor a push
	// refspec in the clone's config has a say.
	ref := "refs/heads/" + branch
	if _, err := w.git(ctx, dir, true, "push", "--quiet", "--no-verify", "--", w.Remote, ref+":"+ref); err != nil {
		return Commit{}, err
	}
	return Commit{SHA: sha}, nil
}

func (w *Workspace) git(ctx context.Context, dir string, network bool, args ...string) (string, error) {
	return w.gitInput(ctx, dir, network, "", args...)
}

// gitInput runs one git command and returns its trimmed output. network
// marks the commands that talk to the remote: only those are given the
// token.
func (w *Workspace) gitInput(ctx context.Context, dir string, network bool, stdin string, args ...string) (string, error) {
	binary := w.Git
	if binary == "" {
		binary = "git"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = w.env(network)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], w.scrub(detail))
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

// env is the environment for one git command. Git is cut off from the
// user's and the system's config, and the settings below override whatever
// the clone's own config says, which a Step can edit: no hooks, no
// filesystem monitor, no credential helper, no prompt. A network command
// also gets the token, as an HTTP header scoped to the remote's host.
func (w *Workspace) env(network bool) []string {
	environ := os.Environ
	if w.Environ != nil {
		environ = w.Environ
	}
	var env []string
	for _, kv := range environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "GIT_") || key == "SSH_ASKPASS" {
			continue // nothing inherited steers git
		}
		env = append(env, kv)
	}
	config := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"credential.helper", ""},
		{"commit.gpgsign", "false"},
	}
	if host := httpHost(w.Remote); network && w.Token != "" && host != "" {
		config = append(config, [2]string{"http." + host + "/.extraheader", "Authorization: Basic " + basicAuth(w.Token)})
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+w.Author.Name, "GIT_AUTHOR_EMAIL="+w.Author.Email,
		"GIT_COMMITTER_NAME="+w.Author.Name, "GIT_COMMITTER_EMAIL="+w.Author.Email,
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(config)),
	)
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// httpHost is the scheme and host of an HTTP remote, such as
// "https://github.com"; empty for any other kind of remote, which the token
// is never sent to.
func httpHost(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
