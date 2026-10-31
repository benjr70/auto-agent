// Package sweep is the Sweep: what the Daemon does at its own start and at
// the start of every Fire, under the Fire lock, to remove the Footprint
// nothing needs any more. Each step is its own small function over the
// State dir and the clock; a step that fails never stops the others.
package sweep

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/pidfile"
)

// Window is how long the Fire records, the transcripts of their Steps and
// the rate-limit events are kept.
const Window = 30 * 24 * time.Hour

// EvidenceWindow is how long a round's evidence directory is kept once its
// PR is no longer open, counted from the round's own stamp.
const EvidenceWindow = 7 * 24 * time.Hour

// Sweep is the Sweep of one State dir.
type Sweep struct {
	StateDir string
	// Transcripts is the clone's Claude project directory, where Claude
	// keeps the transcripts of the Steps run in it; empty when it is not
	// known.
	Transcripts string
	// OpenPRs is the numbers of the repository's open pull requests, read
	// from GitHub; it is asked once, and only when a round's evidence is
	// past its window. Nil keeps every round's evidence.
	OpenPRs func() ([]int, error)
	Now     func() time.Time
}

// TmpDir is where the Steps, and the smoke's provider calls, keep their
// temporary files: under the State dir, never the Host's /tmp. Nothing in it
// outlives the Fire that made it.
func TmpDir(stateDir string) string { return filepath.Join(stateDir, "tmp") }

// TranscriptsDir is where Claude, its config in claudeDir, keeps the
// transcripts of the sessions run in dir: its path, every character but a
// letter or a digit a dash.
func TranscriptsDir(claudeDir, dir string) string {
	name := strings.Map(func(r rune) rune {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return '-'
	}, dir)
	return filepath.Join(claudeDir, "projects", name)
}

// Run sweeps the State dir. Its error joins every step's; it is never a
// reason to stop the Fire.
func (s Sweep) Run() error {
	cutoff := s.Now().Add(-Window)
	// A killed round is finished first: one past its window may still name an app.
	errs := []error{s.orphans(), s.rateLimits(cutoff), s.killedRounds(), s.evidence(s.Now().Add(-EvidenceWindow))}
	kept, err := s.records(cutoff)
	errs = append(errs, err)
	// Which sessions a paused ticket still needs is only known when every
	// record was looked at.
	if err == nil {
		errs = append(errs, s.transcripts(cutoff, kept))
	}
	return errors.Join(errs...)
}

// rateLimits drops the rate-limit events observed before cutoff.
func (s Sweep) rateLimits(cutoff time.Time) error {
	if err := budget.ForgetBefore(s.StateDir, cutoff); err != nil {
		return fmt.Errorf("dropping old rate-limit events: %w", err)
	}
	return nil
}

// transcripts deletes the transcripts last written before cutoff, and the
// directory of each beside it, except those of the kept sessions. Nothing
// else in the directory is Claude's transcripts.
func (s Sweep) transcripts(cutoff time.Time, kept map[string]bool) error {
	if s.Transcripts == "" {
		return nil
	}
	entries, err := os.ReadDir(s.Transcripts)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing the transcripts: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		session, ok := strings.CutSuffix(entry.Name(), ".jsonl")
		if !ok || entry.IsDir() || kept[session] {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		for _, path := range []string{filepath.Join(s.Transcripts, entry.Name()), filepath.Join(s.Transcripts, session)} {
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// orphans removes the half-written records of killed Fires: a record's
// .tmp whose Fire's process is gone. The Sweep runs under the Fire lock,
// so no Fire of this State dir is writing one; a Fire still alive, such
// as one that found the lock held, keeps its own.
func (s Sweep) orphans() error {
	j := journal.Journal{StateDir: s.StateDir}
	paths, err := filepath.Glob(filepath.Join(j.Dir(), "*.json.tmp"))
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range paths {
		id := strings.TrimSuffix(filepath.Base(path), ".json.tmp")
		if _, pid, ok := journal.ParseFireID(id); ok && alive(pid) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// alive reports whether a process with this pid is running.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// killedRounds finishes what a killed Fire's rounds left undone: it kills the
// app each pid file still names, when it is still the same process, removes
// every round's tmp directory and the Steps' tmp directory, and drops a
// round left with no file in it. The Sweep runs under the Fire lock, so no
// round of this State dir is running.
func (s Sweep) killedRounds() error {
	var errs []error
	rounds, err := filepath.Glob(filepath.Join(s.StateDir, "verify", "pr-*", "*"))
	if err != nil {
		return err
	}
	for _, round := range rounds {
		if info, err := os.Stat(round); err != nil || !info.IsDir() {
			continue
		}
		pids, _ := filepath.Glob(filepath.Join(round, "run", "*.pid"))
		for _, path := range pids {
			if err := pidfile.Stop(path); err != nil {
				errs = append(errs, fmt.Errorf("stopping the app %s names: %w", path, err))
			}
		}
		if err := os.RemoveAll(filepath.Join(round, "tmp")); err != nil {
			errs = append(errs, err)
		}
		checklist.Prune(round)
	}
	if err := os.RemoveAll(TmpDir(s.StateDir)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// records deletes the Fire records older than cutoff, judged by the stamp
// their file is named by, except the ones the Journal still finds a paused
// ticket's resume session in. It returns those sessions.
func (s Sweep) records(cutoff time.Time) (kept map[string]bool, err error) {
	j := journal.Journal{StateDir: s.StateDir}
	entries, err := os.ReadDir(j.Dir())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing the Fire records: %w", err)
	}
	var old []string
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || entry.IsDir() {
			continue
		}
		if started, _, ok := journal.ParseFireID(id); ok && started.Before(cutoff) {
			old = append(old, id)
		}
	}
	// Only the records past the window are read, and only for this.
	resumable := j.Resumable(old)
	kept = map[string]bool{}
	var errs []error
	for _, id := range old {
		if session, ok := resumable[id]; ok {
			kept[session] = true
			continue
		}
		if err := os.Remove(j.Path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return kept, errors.Join(errs...)
}

// evidence deletes the rounds' evidence directories, `verify/pr-N/<stamp>[-rK]`,
// stamped before cutoff whose PR is no longer open; a PR left with no round
// goes with them. Every round of an open PR is kept, and GitHub is asked
// which PRs are open only when a round is past the window.
func (s Sweep) evidence(cutoff time.Time) error {
	if s.OpenPRs == nil {
		return nil
	}
	root := filepath.Join(s.StateDir, "verify")
	prs, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing the evidence: %w", err)
	}
	old := map[int][]string{}
	for _, entry := range prs {
		pr, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "pr-"))
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "pr-") || err != nil {
			continue
		}
		rounds, err := os.ReadDir(filepath.Join(root, entry.Name()))
		if err != nil {
			return fmt.Errorf("listing PR #%d's evidence: %w", pr, err)
		}
		for _, r := range rounds {
			stamp, _, _ := strings.Cut(r.Name(), "-")
			at, err := time.Parse("20060102T150405Z", stamp)
			if r.IsDir() && err == nil && at.Before(cutoff) {
				old[pr] = append(old[pr], filepath.Join(root, entry.Name(), r.Name()))
			}
		}
	}
	if len(old) == 0 {
		return nil
	}
	open, err := s.OpenPRs()
	if err != nil {
		return fmt.Errorf("reading the open PRs, so no evidence is removed: %w", err)
	}
	for _, pr := range open {
		delete(old, pr)
	}
	var errs []error
	for _, dirs := range old {
		for _, dir := range dirs {
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, err)
			}
		}
		// Removed only when no round is left in it.
		os.Remove(filepath.Dir(dirs[0]))
	}
	return errors.Join(errs...)
}
