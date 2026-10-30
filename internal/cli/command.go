package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
)

// commandNames are the Harness config commands the verb runs, in the order
// the usage lists them.
var commandNames = []string{"install", "test", "lint"}

// commandCmd is the fire.CommandVerb: it runs the Harness config command
// named by args in the checkout (default: the current directory) through
// bash, with this process's standard streams, and exits as it did. It reads
// nothing but the Harness config and reaches nothing on the network, so a
// credential-free Step may run it; see fire.CommandInvocation for why a Step
// runs the Target Project's commands through it.
func commandCmd(args []string, env Env) int {
	if len(args) < 1 || len(args) > 2 {
		return usageError(env.Stderr, "command takes the command's name (install, test or lint) and at most one target dir")
	}
	name, targetDir := args[0], "."
	if len(args) == 2 {
		targetDir = args[1]
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "auto-agent: command %s: %v\n", name, err)
		return exitFailed
	}
	harness, err := config.LoadHarness(targetDir, "", config.Repo{})
	if err != nil {
		return fail(err)
	}
	var command string
	switch name {
	case "install":
		command = harness.Commands.Install
	case "test":
		command = harness.Commands.Test
	case "lint":
		command = harness.Commands.Lint
	default:
		return usageError(env.Stderr, fmt.Sprintf("command: unknown command %q: %s", name, strings.Join(commandNames, ", ")))
	}
	if strings.TrimSpace(command) == "" {
		return fail(errors.New("the Harness config declares no such command"))
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = targetDir
	cmd.Stdout, cmd.Stderr = env.Stdout, env.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return fail(err)
	}
	return exitOK
}
