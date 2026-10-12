// Package cli is the auto-agent command line: it routes the arguments to a
// command and returns the process exit code.
package cli

import (
	"fmt"
	"io"

	"github.com/benjr70/auto-agent/internal/version"
)

// Exit codes.
const (
	exitOK    = 0
	exitUsage = 2
)

const usage = `Usage: auto-agent <command>

Commands:
  version   print the version and commit of this binary
`

// Run executes the command named by args (the arguments after the program
// name) and returns the exit code.
func Run(args []string, build version.Info, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version":
		if len(args) > 1 {
			return usageError(stderr, "version takes no arguments")
		}
		fmt.Fprintln(stdout, build)
		return exitOK
	default:
		return usageError(stderr, fmt.Sprintf("unknown command %q", args[0]))
	}
}

func usageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "auto-agent: %s\n\n%s", msg, usage)
	return exitUsage
}
