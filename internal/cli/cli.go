// Package cli is the auto-agent command line: it routes the arguments to a
// command and returns the process exit code.
package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/benjr70/auto-agent/internal/fire"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/verify"
	"github.com/benjr70/auto-agent/internal/version"
)

// Exit codes.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

const usage = `Usage: auto-agent <command>

Commands:
  init      write a Harness config scaffold with the test and lint commands detected
            init [--force] [<target-dir>]
            --force overwrites an existing .auto-agent/harness.json
  run       run the Daemon: Fire after Fire, each deriving the state from
            GitHub and acting on it, until SIGINT or SIGTERM
            run [--once [--dry-run]] [--repo owner/name] [<target-dir>]
            --once runs one Fire and exits
            --dry-run says what the Fire would do and changes nothing;
            without it, the doctor's checks run first and a fatal one
            stops the Daemon from starting
  doctor    say what this machine is missing, and the command that fixes it
            doctor [--repo owner/name] [<target-dir>]
            exits non-zero on a fatal finding; changes nothing
  service   install the Daemon as a systemd user unit (Linux)
            service install [--force] [--no-enable] [--env-file <path>] [--repo owner/name] [<target-dir>]
            --force replaces an existing unit; --no-enable only prints the commands
  status    print what the Dashboard shows, from the running Daemon
            status [--json] [--repo owner/name] [<target-dir>]
            --json prints /api/status as it came; with no Daemon answering,
            prints the Fire history from the State dir and exits non-zero
  pick      put a ticket on the pick signal, or take it off
            pick add <issue> [--priority <name>] [--repo owner/name] [<target-dir>]
            pick remove <issue> [--repo owner/name] [<target-dir>]
            pick labels [--repo owner/name] [<target-dir>]
            add ensures the harness labels, then under a Project pick adds the
            ticket to the board with its Priority (default: the first of the
            configured order), then labels it AFK; remove takes AFK off and the
            ticket off the board; labels only ensures the harness labels, for a
            ticket created with one that never goes on the signal; exits
            non-zero, saying what failed, when it did not land
  command   run one of the Harness config's commands in a checkout, as the
            Daemon's read-only Steps do, and exit as it did
            command install|test|lint [<target-dir>]
  version   print the version and commit of this binary
`

// Env is everything a command reaches outside itself through, so that a test
// can stand in for each.
type Env struct {
	// Build is the running binary's version and commit.
	Build  version.Info
	Stdout io.Writer
	Stderr io.Writer
	// LookupEnv reads the environment, as os.LookupEnv does.
	LookupEnv func(string) (string, bool)
	// GHAuthToken asks the gh CLI for its token (github.GHAuthToken).
	GHAuthToken func() (string, error)
	// OriginURL is the origin remote of the checkout in dir (GitOriginURL).
	OriginURL func(dir string) (string, error)
	// Runner runs a Step: step.Claude outside tests.
	Runner step.Runner
	// AuthStatus asks the claude CLI how it is logged in
	// (budget.ClaudeAuthStatus).
	AuthStatus func(context.Context) ([]byte, error)
	// Now is the clock.
	Now func() time.Time
	// Sleep waits, or returns early when the context is done (fire.Sleep).
	Sleep func(context.Context, time.Duration) error
	// Cycles stops the always-on Daemon after that many cycles; 0, as
	// outside tests, runs it until it is stopped.
	Cycles int
	// ClaudeVersion asks the claude CLI its version (doctor.ClaudeVersion);
	// nil is the real one.
	ClaudeVersion func(context.Context) ([]byte, error)
	// LookPath finds an executable on the PATH; nil is exec.LookPath.
	LookPath func(string) (string, error)
	// Docker answers whether the Docker daemon is reachable
	// (doctor.Docker); nil is the real one.
	Docker func(context.Context) error
	// SurfaceHost is the Host the doctor checks the Surfaces' needs on; its
	// Env, when nil, is read through LookupEnv.
	SurfaceHost verify.Host
	// GOOS is the platform the binary runs on (runtime.GOOS).
	GOOS string
	// Executable is the running binary's path (os.Executable).
	Executable func() (string, error)
	// Command runs a program such as systemctl and returns its combined
	// output (ExecCommand).
	Command func(name string, args ...string) ([]byte, error)
}

// Run executes the command named by args (the arguments after the program
// name) and returns the exit code.
func Run(args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version":
		if len(args) > 1 {
			return usageError(env.Stderr, "version takes no arguments")
		}
		fmt.Fprintln(env.Stdout, env.Build)
		return exitOK
	case "init":
		return initCmd(args[1:], env)
	case "run":
		return run(args[1:], env)
	case "doctor":
		return doctorCommand(args[1:], env)
	case "service":
		return serviceCmd(args[1:], env)
	case "status":
		return statusCmd(args[1:], env)
	case "pick":
		return pickCmd(args[1:], env)
	case fire.CommandVerb:
		return commandCmd(args[1:], env)
	default:
		return usageError(env.Stderr, fmt.Sprintf("unknown command %q", args[0]))
	}
}

func usageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "auto-agent: %s\n\n%s", msg, usage)
	return exitUsage
}
