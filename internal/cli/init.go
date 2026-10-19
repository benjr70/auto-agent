package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/benjr70/auto-agent/internal/config"
)

// scaffoldSections explains each section init writes, in file order: the
// schema allows no comments in harness.json, so the report carries them.
var scaffoldSections = []struct{ key, what string }{
	{"$schema", "the Harness config schema; every key is described there"},
	{"commit_scopes", "the conventional-commit scopes an Agent PR may use; replace with your project's areas"},
	{"commands", "install, test and lint, run from the repo root; a placeholder fails until you set it"},
	{"pick", "labels: AFK tickets are taken oldest first; {\"project\": {\"number\": N}} ranks a Project board instead"},
	{"docs_research_prefix", "where a Decision ticket's research lands; a PR touching only this is merged without review"},
	{"required_checks", "CI checks that must be green; empty means every check"},
	{"rounds", "the round caps, at their defaults"},
	{"host", "what the Target Project needs of the machine"},
}

func initCmd(args []string, env Env) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	force := flags.Bool("force", false, "")
	var positional []string
	for rest := args; ; {
		err := flags.Parse(rest)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return exitOK
		}
		if err != nil {
			return usageError(env.Stderr, "init: "+err.Error())
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(positional) > 1 {
		return usageError(env.Stderr, "init takes at most one target dir")
	}
	targetDir := "."
	if len(positional) == 1 {
		targetDir = positional[0]
	}
	if info, err := os.Stat(targetDir); err != nil || !info.IsDir() {
		fmt.Fprintf(env.Stderr, "auto-agent: init: target dir not found: %s\n", targetDir)
		return exitFailed
	}
	file := filepath.Join(targetDir, config.HarnessDirName, config.HarnessFileName)
	if _, err := os.Lstat(file); err == nil && !*force {
		fmt.Fprintf(env.Stderr, "auto-agent: init: %s already exists; it is left as it is (--force overwrites it)\n", file)
		return exitFailed
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.Stderr, "auto-agent: init: %v\n", err)
		return exitFailed
	}
	scaffold, err := config.NewScaffold(targetDir)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(file), 0o755)
	}
	if err == nil {
		err = os.WriteFile(file, scaffold.Data, 0o644)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "auto-agent: init: %v\n", err)
		return exitFailed
	}
	reportScaffold(env.Stdout, file, scaffold)
	return exitOK
}

func reportScaffold(w io.Writer, file string, scaffold config.Scaffold) {
	fmt.Fprintf(w, "wrote %s\n\ndetected:\n", file)
	for _, d := range scaffold.Commands {
		from := "from " + d.Source
		if d.Source == "" {
			from = "nothing found"
			if d.Key != config.CommandInstall {
				from += ": a placeholder that fails until you set it"
			}
		}
		fmt.Fprintf(w, "  %-8s %s  (%s)\n", d.Key, d.Command, from)
	}
	fmt.Fprintln(w, "\nsections:")
	for _, s := range scaffoldSections {
		fmt.Fprintf(w, "  %-21s %s\n", s.key, s.what)
	}
	fmt.Fprintln(w, "  (verification, surfaces and dependabot are left out: add them when you have an Environment provider, a Surface or the Bot PR Lane)")
	fmt.Fprintf(w, "\nnext: review it, commit %s, then run auto-agent doctor\n", file)
}
