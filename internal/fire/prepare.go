package fire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/journal"
)

// The files the Workspace's readying leaves in the State dir: the keep
// patterns of the last default-branch Harness config, the prepare command's
// output, what it last succeeded for, and the clean command's output.
const (
	keptFile      = "workspace-keep"
	prepareLog    = "workspace-prepare.log"
	preparedStamp = "workspace-prepared"
	cleanLog      = "workspace-clean.log"
)

// cacheDir is the cache root in the State dir: the package and tool caches
// the Target Project's commands fill, which the Disk guard deletes whole
// when the disk is short.
const cacheDir = "cache"

// prepareGrace is how long a cancelled prepare command's process group has
// to go after it is killed.
const prepareGrace = 5 * time.Second

// kept are the patterns the reset before this Fire keeps: the ones the last
// Fire read from the default branch and wrote to the State dir. They are
// never read from the clone, which may be on a branch a Step wrote. None
// written yet keeps nothing.
func kept(stateDir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, keptFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("reading the Workspace's keep patterns: %w", err)
	}
	return strings.Fields(string(data)), nil
}

// keep sets the patterns this Fire's resets keep from the default branch's
// Harness config, and remembers them for the next Fire's first reset. The
// rest of the default branch's workspace block (its cache entries and clean
// command) is this Fire's too.
func (f *Fire) keep(harness config.Harness) error {
	f.Workspace.Keep, f.workspace = harness.Workspace.Keep, harness.Workspace
	data := strings.Join(harness.Workspace.Keep, "\n")
	if err := os.WriteFile(filepath.Join(f.Journal.StateDir, keptFile), []byte(data), 0o600); err != nil {
		return fmt.Errorf("remembering the Workspace's keep patterns: %w", err)
	}
	return nil
}

// prepareWorkspace runs the Harness config's workspace.prepare command in
// the clone just reset to the default branch, unless it last succeeded for
// this head, this command and these keep patterns in a clone that is not
// new. It installs what the Target Project needs into the clone
// (dependencies, a build its Surfaces run), and the paths workspace.keep
// names carry that across Fires. It runs with no credential, as the
// Environment provider does; its output goes to a log in the State dir. A
// failure fails the Fire, and the next Fire runs it again.
func (f *Fire) prepareWorkspace(ctx context.Context, harness config.Harness, created bool) error {
	command := harness.Workspace.Prepare
	if command == "" {
		return nil
	}
	head, err := f.Workspace.Head(ctx)
	if err != nil {
		return fmt.Errorf("reading the Workspace's head: %w", err)
	}
	stamp := filepath.Join(f.Journal.StateDir, preparedStamp)
	want := strings.Join(append([]string{head, command}, harness.Workspace.Keep...), "\n") + "\n"
	if done, err := os.ReadFile(stamp); err == nil && !created && string(done) == want {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", stamp, err)
	}
	// A failed run must not pass for the one before it.
	if err := unstamp(f.Journal.StateDir); err != nil {
		return err
	}
	logPath := filepath.Join(f.Journal.StateDir, prepareLog)
	if err := f.runWorkspaceCommand(ctx, "prepare", command, logPath); err != nil {
		return fmt.Errorf("the Harness config's workspace.prepare failed at %.12s (%w)", head, err)
	}
	if err := os.WriteFile(stamp, []byte(want), 0o600); err != nil {
		return fmt.Errorf("remembering the prepare command's success: %w", err)
	}
	return nil
}

// runWorkspaceCommand runs one of the Harness config's workspace commands
// in the clone, in the environment the Environment provider runs in: no
// credential, its caches under the State dir. Its output goes to logPath.
func (f *Fire) runWorkspaceCommand(ctx context.Context, name, command, logPath string) error {
	env, err := f.providerEnv()
	if err != nil {
		return fmt.Errorf("making the %s command's environment: %w", name, err)
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening the %s log: %w", name, err)
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = f.Workspace.Dir(), env, log, log
	// Kill the whole process group: an install the command started must
	// not go on writing into the clone after the Fire is cancelled.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = prepareGrace
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v; its output is in %s", err, logPath)
	}
	return nil
}

// dropCache deletes the State dir's cache root whole, for the Disk guard.
// What the prepare command put there may be what the Target Project's
// commands need (a browser a test runner downloaded), so a cache that was
// there makes the next Fire run the prepare command again, which refills
// it.
func (f *Fire) dropCache(context.Context) error {
	root := f.cacheRoot()
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("deleting the cache root: %w", err)
	}
	return unstamp(f.Journal.StateDir)
}

// cleanWorkspace runs the default branch's workspace.clean command, when it
// declares one: what only the Target Project can judge, such as its old
// base images. Its output goes to a log in the State dir.
func (f *Fire) cleanWorkspace(ctx context.Context) error {
	command := f.workspace.Clean
	if command == "" {
		return nil
	}
	if err := f.runWorkspaceCommand(ctx, "clean", command, filepath.Join(f.Journal.StateDir, cleanLog)); err != nil {
		return fmt.Errorf("the Harness config's workspace.clean failed (%w)", err)
	}
	return nil
}

// unstamp forgets that the prepare command succeeded, so the next Fire runs
// it again.
func unstamp(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, preparedStamp)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("forgetting the prepare command's last success: %w", err)
	}
	return nil
}

// failedFire reports whether the Fire failed, or ran a round whose
// environment did not boot or answer. Either may be what the prepare
// command installs having gone (a Step removed a kept directory), so the
// next Fire runs it again rather than trusting its stamp.
func failedFire(record journal.Record) bool {
	unbooted := func(v *journal.Verify) bool { return v != nil && v.Infra != "" }
	return record.Error != "" || unbooted(record.Verify) || unbooted(record.Deployed)
}

// cacheRoot is the State dir's cache root.
func (f *Fire) cacheRoot() string { return filepath.Join(f.Journal.StateDir, cacheDir) }

// cacheEnv is what the Target Project's commands get of the cache root, from
// the default branch's workspace block: XDG_CACHE_HOME, AUTO_AGENT_CACHE_DIR
// and each workspace.cache variable, under the State dir. A Step never gets
// it: the Daemon's own environment is left alone.
func (f *Fire) cacheEnv() []string { return f.workspace.CacheEnv(f.cacheRoot()) }
