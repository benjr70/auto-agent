// Command auto-agent is the Daemon: one binary, one Target Project per process.
package main

import (
	"os"

	"github.com/benjr70/auto-agent/internal/cli"
	"github.com/benjr70/auto-agent/internal/version"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], version.Current(), os.Stdout, os.Stderr))
}
