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

// todo is the plan of an interactive rebase in progress. A Step that
// resolves a conflict works in the clone and could add a command to it,
// which git would run on continue: it is kept as it was.
var todo = []string{"rebase-merge", "git-rebase-todo"}

// Rebase rebases the open branch onto the default branch as the remote had
// it when the clone was prepared. conflicts are the files a stop left
// conflicted, the rebase still in progress for them to be resolved in
// place; none when it completed. A rebase that stops on anything else is
// aborted, and is an error.
func (w *Workspace) Rebase(ctx context.Context) (conflicts []string, err error) {
	if err := w.harden(); err != nil {
		return nil, err
	}
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
	if err := w.restoreTodo(); err != nil {
		return nil, err
	}
	if _, err := w.git(ctx, "add", "--all"); err != nil {
		return nil, err
	}
	if _, err := w.git(ctx, "-c", "core.editor=true", "rebase", "--continue"); err == nil {
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
	if !isDir(filepath.Join(w.Dir(), ".git", "rebase-merge")) && !isDir(filepath.Join(w.Dir(), ".git", "rebase-apply")) {
		return nil
	}
	_, err := w.git(ctx, "rebase", "--abort")
	return err
}

// stopped reads a rebase that did not complete: the files it left
// conflicted, with its plan kept to be restored; or, with none, the rebase
// is aborted and that is an error.
func (w *Workspace) stopped(ctx context.Context, what string) ([]string, error) {
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
			return nil, fmt.Errorf("the %s stopped with no conflict, and could not be aborted: %w", what, abortErr)
		}
		return nil, fmt.Errorf("the %s stopped with no conflict, and was aborted", what)
	}
	plan, err := os.ReadFile(filepath.Join(append([]string{w.Dir(), ".git"}, todo...)...))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	w.todo = plan
	return conflicts, nil
}

// restoreTodo writes back the rebase's plan as git left it at the stop.
func (w *Workspace) restoreTodo() error {
	if w.todo == nil {
		return nil
	}
	return os.WriteFile(filepath.Join(append([]string{w.Dir(), ".git"}, todo...)...), w.todo, 0o644)
}

// hasMarkers says a file still holds a line git writes around a conflict.
func hasMarkers(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") || line == "=======" {
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
	ref := "refs/heads/" + branch
	if _, err := w.gitRemote(ctx, "push", "--quiet", "--no-verify", "--force-with-lease="+ref+":"+expected, "--", w.Remote, ref+":"+ref); err != nil {
		if strings.Contains(err.Error(), "stale info") || strings.Contains(err.Error(), "rejected") {
			return Commit{}, fmt.Errorf("%w: %v", ErrLeaseRefused, err)
		}
		return Commit{}, err
	}
	w.pushed[branch] = sha
	return Commit{SHA: sha}, nil
}
