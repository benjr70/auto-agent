package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/fire"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/workspace"
)

// EnvGitHubAPI points the GitHub client at another API root: a GitHub
// Enterprise host, or the fake in tests.
const EnvGitHubAPI = "AUTO_AGENT_GITHUB_API_URL"

// EnvGitRemote points the Workspace at another remote than the Target
// Project on github.com: a GitHub Enterprise host, or a local repository in
// tests.
const EnvGitRemote = "AUTO_AGENT_GIT_REMOTE"

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
	// Flags may come before or after the target dir.
	var positional []string
	for rest := args; ; {
		err := flags.Parse(rest)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return exitOK
		}
		if err != nil {
			return usageError(env.Stderr, "run: "+err.Error())
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(positional) > 1 {
		return usageError(env.Stderr, "run takes at most one target dir")
	}
	if !*once {
		return usageError(env.Stderr, "run: this build runs one Fire at a time: use run --once")
	}
	targetDir := "."
	if len(positional) == 1 {
		targetDir = positional[0]
	}
	// A signal cancels the Fire, which still releases its lock.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var report any
	failure := ""
	s, err := openSession(ctx, env, targetDir, *repoFlag)
	if err == nil && *dryRun {
		report, err = s.dryRun(ctx, env, targetDir)
	} else if err == nil {
		var record journal.Record
		if record, err = s.fire(ctx, env); err == nil {
			report, failure = record, record.Error
		}
	}
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
	if failure != "" {
		fmt.Fprintf(env.Stderr, "auto-agent: run: %s\n", failure)
		return exitFailed
	}
	return exitOK
}

// session is what every run needs before it can look at the Target Project:
// which repository, and a client that can read it.
type session struct {
	repo   config.Repo
	token  string
	client *github.Client
}

func openSession(ctx context.Context, env Env, targetDir, repoSlug string) (session, error) {
	if repoSlug == "" {
		origin, err := env.OriginURL(targetDir)
		if err != nil {
			return session{}, err
		}
		repoSlug = origin
	}
	owner, name, err := config.ParseRepo(repoSlug)
	if err != nil {
		return session{}, err
	}
	token, err := github.ResolveToken(env.LookupEnv, env.GHAuthToken)
	if err != nil {
		return session{}, err
	}
	apiURL, _ := env.LookupEnv(EnvGitHubAPI)
	client := github.New(github.Options{BaseURL: apiURL, Token: token, UserAgent: "auto-agent/" + env.Build.Version})
	repository, err := client.Repository(ctx, owner, name)
	if err != nil {
		return session{}, err
	}
	return session{repo: config.Repo{Owner: owner, Name: name, DefaultBranch: repository.DefaultBranch}, token: token, client: client}, nil
}

// dryRun reads the facts one pass needs from GitHub and derives the state
// from them, with the Harness config of the checkout in targetDir. It makes
// read requests only and touches nothing on disk.
func (s session) dryRun(ctx context.Context, env Env, targetDir string) (dryRunReport, error) {
	cfg, err := config.Load(config.Input{TargetDir: targetDir, Repo: s.repo, LookupEnv: env.LookupEnv})
	if err != nil {
		return dryRunReport{}, err
	}
	login, err := s.client.Login(ctx)
	if err != nil {
		return dryRunReport{}, err
	}
	facts, warnings, err := fire.ReadFacts(ctx, s.client, s.repo, login, cfg.Harness.Pick)
	if err != nil {
		return dryRunReport{}, err
	}
	return dryRunReport{
		Repo: cfg.Harness.Repo.Slug(), Login: login, PickShape: cfg.Harness.Pick.Shape,
		State: state.Derive(facts), Warnings: warnings,
	}, nil
}

// gitRemote is where the Workspace clones from and pushes to.
func gitRemote(repo config.Repo, lookupEnv func(string) (string, bool)) string {
	if remote, ok := lookupEnv(EnvGitRemote); ok && remote != "" {
		return remote
	}
	return "https://github.com/" + repo.Slug() + ".git"
}

// fire runs one Fire in the Daemon's own clone and returns its record. An
// error is a Fire that could not start, and so has no record.
func (s session) fire(ctx context.Context, env Env) (journal.Record, error) {
	host, err := config.LoadHost(s.repo, env.LookupEnv)
	if err != nil {
		return journal.Record{}, err
	}
	login, err := s.client.Login(ctx)
	if err != nil {
		return journal.Record{}, err
	}
	f := fire.Fire{
		Repo: s.repo, Host: host, Login: login, GitHub: s.client, Runner: env.Runner,
		Workspace: &workspace.Workspace{
			StateDir: host.StateDir, Remote: gitRemote(s.repo, env.LookupEnv), DefaultBranch: s.repo.DefaultBranch, Token: s.token,
			Author: workspace.Author{Name: login, Email: login + "@users.noreply.github.com"},
		},
		Journal: journal.Journal{StateDir: host.StateDir},
		Now:     env.Now,
	}
	return f.Once(ctx), nil
}
