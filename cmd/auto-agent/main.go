// Command auto-agent is the Daemon: one binary, one Target Project per process.
package main

import (
	"os"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/cli"
	"github.com/benjr70/auto-agent/internal/fire"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/version"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Env{
		Build:       version.Current(),
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		LookupEnv:   os.LookupEnv,
		GHAuthToken: github.GHAuthToken,
		OriginURL:   cli.GitOriginURL,
		Runner:      &step.Claude{},
		AuthStatus:  budget.ClaudeAuthStatus,
		Now:         time.Now,
		Sleep:       fire.Sleep,
	}))
}
