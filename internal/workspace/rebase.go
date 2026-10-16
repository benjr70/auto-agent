package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrLeaseRefused: the remote's branch moved since the Daemon last read it,
// and a push with a lease was refused. It is never retried harder.
var ErrLeaseRefused = errors.New("the branch moved on the remote since it was read: the lease refused the push")

// rebaseState is the directory in .git where git keeps a rebase in
// progress: its plan, the branch it finishes on, where it started. A Step
// that resolves a conflict works in the clone with a shell, and git reads
// all of it on continue: a command added to the plan would run, a branch
// named there would be moved. So it is kept as git left it at the stop.
const rebaseState = "rebase-merge"

// Rebase rebases the open branch onto the default branch as the remote had
// it when the clone was prepared. conflicts are the files a stop left
// conflicted, the rebase still in progress for them to be resolved in
// place; none when it completed. A rebase that stops on anything else is
// aborted, and is an error.
func (w *Workspace) Rebase(ctx context.Context) (conflicts []string, err error) {
	if err := w.harden(); err != nil {
		return nil, err
	}
	w.state = nil
	if _, err := w.git(ctx, "-c", "core.editor=true", "rebase", "--quiet", w.Base()); err == nil {
		return nil, nil
	}
	return w.stopped(ctx, "rebase")
}

// RebaseContinue carries a stopped rebase on, once its conflicts are
// resolved in the files: Go stages them. A file that still holds a conflict
// marker is not resolved, and is an error, the rebase still in progress.
// conflicts are the files the next stop left conflicted; none when it
// completed.
func (w *Workspace) RebaseContinue(ctx context.Context, resolved []string) (conflicts []string, err error) {
	if err := w.harden(); err != nil {
		return nil, err
	}
	for _, path := range resolved {
		data, err := os.ReadFile(filepath.Join(w.Dir(), path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if hasMarkers(string(data)) {
			return nil, fmt.Errorf("%s still holds a conflict marker", path)
		}
	}
	if err := w.restoreState(); err != nil {
		return nil, err
	}
	if _, err := w.git(ctx, "add", "--all"); err != nil {
		return nil, err
	}
	if _, err := w.git(ctx, "-c", "core.editor=true", "rebase", "--continue"); err == nil {
		w.state = nil
		return nil, nil
	}
	return w.stopped(ctx, "rebase --continue")
}

// RebaseAbort leaves the branch as it was before the rebase. Aborting no
// rebase is not an error.
func (w *Workspace) RebaseAbort(ctx context.Context) error {
	if err := w.harden(); err != nil {
		return err
	}
	if !isDir(filepath.Join(w.Dir(), ".git", rebaseState)) && !isDir(filepath.Join(w.Dir(), ".git", "rebase-apply")) {
		return nil
	}
	// What the Step may have left in the rebase's state must not steer the
	// abort either.
	if err := w.restoreState(); err != nil {
		return err
	}
	w.state = nil
	_, err := w.git(ctx, "rebase", "--abort")
	return err
}

// stopped reads a rebase that did not complete: the files it left
// conflicted, with its state kept to be restored; or, with none, the rebase
// is aborted and that is an error. step names the git command that stopped.
func (w *Workspace) stopped(ctx context.Context, step string) ([]string, error) {
	listed, err := w.git(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var conflicts []string
	for _, path := range strings.Split(listed, "\n") {
		if path = strings.TrimSpace(path); path != "" {
			conflicts = append(conflicts, path)
		}
	}
	if len(conflicts) == 0 {
		if abortErr := w.RebaseAbort(ctx); abortErr != nil {
			return nil, fmt.Errorf("the %s stopped with no conflict, and could not be aborted: %w", step, abortErr)
		}
		return nil, fmt.Errorf("the %s stopped with no conflict, and was aborted", step)
	}
	state, err := readState(filepath.Join(w.Dir(), ".git", rebaseState))
	if err != nil {
		return nil, fmt.Errorf("reading the stopped rebase: %w", err)
	}
	w.state = state
	return conflicts, nil
}

// readState reads every file of a rebase's state, by its path inside it.
func readState(dir string) (map[string][]byte, error) {
	state := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		state[rel] = data
		return err
	})
	return state, err
}

// restoreState puts the rebase's state back as git left it at the stop:
// whatever is there now, a link included, is removed without following it,
// and the files written afresh.
func (w *Workspace) restoreState() error {
	if w.state == nil {
		return nil
	}
	dotGit := filepath.Join(w.Dir(), ".git")
	if err := removeUnder(dotGit, rebaseState); err != nil {
		return err
	}
	for rel, data := range w.state {
		path := filepath.Join(dotGit, rebaseState, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// hasMarkers says a file still holds a conflict as git writes one: a
// "<<<<<<< " line, then "=======", then a ">>>>>>> " line. Any one of them
// alone, a heading underlined with seven "=" say, is the file's own.
func hasMarkers(text string) bool {
	want := 0
	for _, line := range strings.Split(text, "\n") {
		switch {
		case want == 0 && strings.HasPrefix(line, "<<<<<<< "):
			want = 1
		case want == 1 && line == "=======":
			want = 2
		case want == 2 && strings.HasPrefix(line, ">>>>>>> "):
			return true
		}
	}
	return false
}

// PushWithLease pushes the open branch over the remote's, but only while
// the remote's is where the Daemon last read it: the one force-push it ever
// makes, after a rebase. A remote branch that moved meanwhile refuses it,
// with ErrLeaseRefused.
func (w *Workspace) PushWithLease(ctx context.Context, branch string) (Commit, error) {
	if branch == "" || branch == w.DefaultBranch {
		return Commit{}, fmt.Errorf("the Daemon never force-pushes %q", branch)
	}
	if err := w.harden(); err != nil {
		return Commit{}, err
	}
	expected, known := w.pushed[branch]
	if !known {
		return Commit{}, fmt.Errorf("where %s is on the remote is not known: the lease cannot be set", branch)
	}
	sha, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Commit{}, err
	}
	// The commit read, not whatever the local branch now names: nothing in
	// the clone says what is pushed.
	ref := "refs/heads/" + branch
	if _, err := w.gitRemote(ctx, "push", "--quiet", "--no-verify", "--force-with-lease="+ref+":"+expected, "--", w.Remote, sha+":"+ref); err != nil {
		if strings.Contains(err.Error(), "stale info") {
			return Commit{}, fmt.Errorf("%w: %v", ErrLeaseRefused, err)
		}
		return Commit{}, err
	}
	w.pushed[branch] = sha
	return Commit{SHA: sha}, nil
}
