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
// output, and what it last succeeded for.
const (
	keptFile      = "workspace-keep"
	prepareLog    = "workspace-prepare.log"
	preparedStamp = "workspace-prepared"
)

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
// Harness config, and remembers them for the next Fire's first reset.
func (f *Fire) keep(harness config.Harness) error {
	f.Workspace.Keep = harness.Workspace.Keep
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
	env, err := f.providerEnv()
	if err != nil {
		return fmt.Errorf("making the prepare command's environment: %w", err)
	}
	logPath := filepath.Join(f.Journal.StateDir, prepareLog)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening the prepare log: %w", err)
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
		return fmt.Errorf("the Harness config's workspace.prepare failed at %.12s (%v); its output is in %s", head, err, logPath)
	}
	if err := os.WriteFile(stamp, []byte(want), 0o600); err != nil {
		return fmt.Errorf("remembering the prepare command's success: %w", err)
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
