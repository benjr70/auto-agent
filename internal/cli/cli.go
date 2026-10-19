// Package cli is the auto-agent command line: it routes the arguments to a
// command and returns the process exit code.
package cli

import (
	"context"
	"fmt"
	"io"
	"time"

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
	case "run":
		return run(args[1:], env)
	case "doctor":
		return doctorCommand(args[1:], env)
	default:
		return usageError(env.Stderr, fmt.Sprintf("unknown command %q", args[0]))
	}
}

func usageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "auto-agent: %s\n\n%s", msg, usage)
	return exitUsage
}
