package fire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/benjr70/auto-agent/internal/config"
)

// The files the prepare command leaves in the State dir: its output, and
// the default-branch head and command it last succeeded for.
const (
	prepareLog    = "workspace-prepare.log"
	preparedStamp = "workspace-prepared"
)

// keptBy is what the clone's own Harness config names to keep: the default
// branch's as the last Fire left it, since the reset that keeps those paths
// runs before the new config can be read. A clone with no readable config
// keeps nothing.
func keptBy(dir string, repo config.Repo) []string {
	harness, err := config.LoadHarness(dir, "", repo)
	if err != nil {
		return nil
	}
	return harness.Workspace.Keep
}

// prepareWorkspace runs the Harness config's workspace.prepare command in
// the clone just reset to the default branch, when that branch's head or
// the command has changed since it last succeeded, or the clone is new. It
// installs what the Target Project needs into the clone (dependencies, a
// build its Surfaces run), and the paths workspace.keep names carry that
// across Fires. It runs with no credential, as the Environment provider
// does; its output goes to a log in the State dir. A failure fails the
// Fire, and the next Fire runs it again.
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
	want := head + "\n" + command + "\n"
	if done, err := os.ReadFile(stamp); err == nil && !created && string(done) == want {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", stamp, err)
	}
	// A failed run must not pass for the one before it.
	if err := os.Remove(stamp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	env, err := f.providerEnv()
	if err != nil {
		return err
	}
	logPath := filepath.Join(f.Journal.StateDir, prepareLog)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = f.Workspace.Dir(), env, log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the Harness config's workspace.prepare failed at %.12s (%v); its output is in %s", head, err, logPath)
	}
	return os.WriteFile(stamp, []byte(want), 0o600)
}
