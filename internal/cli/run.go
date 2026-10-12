package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/state"
)

// EnvGitHubAPI points the GitHub client at another API root: a GitHub
// Enterprise host, or the fake in tests.
const EnvGitHubAPI = "AUTO_AGENT_GITHUB_API_URL"

// GitOriginURL is the origin remote of the checkout in dir.
func GitOriginURL(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("reading the origin remote of %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// dryRunReport is what `run --once --dry-run` prints.
type dryRunReport struct {
	Repo      string `json:"repo"`
	Login     string `json:"login"`
	PickShape string `json:"pickShape"`
	state.State
	// Warnings are things GitHub could not tell us that did not stop the
	// derivation.
	Warnings []string `json:"warnings"`
}

func run(args []string, env Env) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	once := flags.Bool("once", false, "")
	dryRun := flags.Bool("dry-run", false, "")
	repoFlag := flags.String("repo", "", "")
	if err := flags.Parse(args); err != nil {
		return usageError(env.Stderr, "run: "+err.Error())
	}
	if flags.NArg() > 1 {
		return usageError(env.Stderr, "run takes at most one target dir")
	}
	if !*once || !*dryRun {
		return usageError(env.Stderr, "run: this build only derives the state: use run --once --dry-run")
	}
	targetDir := "."
	if flags.NArg() == 1 {
		targetDir = flags.Arg(0)
	}
	report, err := deriveOnce(context.Background(), env, targetDir, *repoFlag)
	if err != nil {
		fmt.Fprintf(env.Stderr, "auto-agent: run: %v\n", err)
		return exitFailed
	}
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(env.Stderr, "auto-agent: run: %v\n", err)
		return exitFailed
	}
	fmt.Fprintln(env.Stdout, string(out))
	return exitOK
}

// deriveOnce reads the facts one pass needs from GitHub and derives the
// state from them. It makes read requests only.
func deriveOnce(ctx context.Context, env Env, targetDir, repoSlug string) (dryRunReport, error) {
	if repoSlug == "" {
		origin, err := env.OriginURL(targetDir)
		if err != nil {
			return dryRunReport{}, err
		}
		repoSlug = origin
	}
	owner, name, err := config.ParseRepo(repoSlug)
	if err != nil {
		return dryRunReport{}, err
	}
	token, err := github.ResolveToken(env.LookupEnv, env.GHAuthToken)
	if err != nil {
		return dryRunReport{}, err
	}
	apiURL, _ := env.LookupEnv(EnvGitHubAPI)
	client := github.New(github.Options{BaseURL: apiURL, Token: token, UserAgent: "auto-agent/" + env.Build.Version})

	repository, err := client.Repository(ctx, owner, name)
	if err != nil {
		return dryRunReport{}, err
	}
	cfg, err := config.Load(config.Input{
		TargetDir: targetDir,
		Repo:      config.Repo{Owner: owner, Name: name, DefaultBranch: repository.DefaultBranch},
		LookupEnv: env.LookupEnv,
	})
	if err != nil {
		return dryRunReport{}, err
	}
	login, err := client.Login(ctx)
	if err != nil {
		return dryRunReport{}, err
	}

	report := dryRunReport{Repo: cfg.Harness.Repo.Slug(), Login: login, PickShape: cfg.Harness.Pick.Shape, Warnings: []string{}}
	facts := state.Facts{Login: login, Pick: cfg.Harness.Pick}
	holders, err := client.OpenIssuesLabelled(ctx, owner, name, state.LabelInProgress)
	if err != nil {
		// An unreadable lock counts as held; say why.
		facts.LockUnreadable = true
		report.Warnings = append(report.Warnings, "the Single-flight lock could not be read: "+err.Error())
	}
	facts.InFlight = len(holders)
	if !facts.LockUnreadable && facts.InFlight == 0 {
		priorityField := ""
		if p := cfg.Harness.Pick.Project; p != nil {
			priorityField = p.PriorityField
		}
		queue, warnings, err := client.Queue(ctx, owner, name, state.LabelAFK, priorityField)
		if err != nil {
			return dryRunReport{}, err
		}
		facts.Queue = queue
		report.Warnings = append(report.Warnings, warnings...)
	}
	report.State = state.Derive(facts)
	return report, nil
}
