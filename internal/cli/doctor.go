package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/benjr70/auto-agent/internal/doctor"
)

// machine is what the doctor checks, reached through env.
func machine(env Env, targetDir, repo string) doctor.Machine {
	apiURL, _ := env.LookupEnv(EnvGitHubAPI)
	return doctor.Machine{
		TargetDir: targetDir, Repo: repo, OriginURL: env.OriginURL, LookupEnv: env.LookupEnv, GHAuthToken: env.GHAuthToken,
		GitHubAPI: apiURL, UserAgent: "auto-agent/" + env.Build.Version,
		ClaudeVersion: env.ClaudeVersion, AuthStatus: env.AuthStatus, SurfaceHost: env.SurfaceHost, LookPath: env.LookPath, Docker: env.Docker,
	}
}

// doctorCommand prints one line per check, and exits non-zero on any fatal
// finding.
func doctorCommand(args []string, env Env) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repoFlag := flags.String("repo", "", "")
	var positional []string
	for rest := args; ; {
		err := flags.Parse(rest)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return exitOK
		}
		if err != nil {
			return usageError(env.Stderr, "doctor: "+err.Error())
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(positional) > 1 {
		return usageError(env.Stderr, "doctor takes at most one target dir")
	}
	targetDir := "."
	if len(positional) == 1 {
		targetDir = positional[0]
	}
	report := doctor.Check(context.Background(), machine(env, targetDir, *repoFlag))
	for _, finding := range report {
		fmt.Fprintln(env.Stdout, finding)
	}
	if report.Fatal() {
		return exitFailed
	}
	return exitOK
}

// preflight runs the doctor's checks before a Fire. Warnings are printed and
// do not stop it; a fatal finding is printed and refuses the start.
func preflight(ctx context.Context, env Env, targetDir, repo string) bool {
	m := machine(env, targetDir, repo)
	m.HarnessOptional = true
	report := doctor.Check(ctx, m)
	for _, finding := range report {
		if finding.Severity != doctor.OK {
			fmt.Fprintf(env.Stderr, "auto-agent: run: %s\n", finding)
		}
	}
	if report.Fatal() {
		fmt.Fprintln(env.Stderr, "auto-agent: run: refusing to start on a fatal finding; `auto-agent doctor` checks this machine")
		return false
	}
	return true
}
