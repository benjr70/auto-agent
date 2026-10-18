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
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/daemon"
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

// EnvUsageAPI points the Usage sensor at another usage endpoint: the fake in
// tests.
const EnvUsageAPI = "AUTO_AGENT_USAGE_API_URL"

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
	if *dryRun && !*once {
		return usageError(env.Stderr, "run: --dry-run says what one Fire would do: use run --once --dry-run")
	}
	targetDir := "."
	if len(positional) == 1 {
		targetDir = positional[0]
	}
	// A signal cancels the Fire, which still releases its lock; the
	// always-on Daemon's Fire stops between Steps instead.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A second signal is not caught: it ends the process.
	context.AfterFunc(ctx, stop)
	if !*once {
		return always(ctx, env, targetDir, *repoFlag)
	}
	var report any
	failure := ""
	s, err := openSession(ctx, env, targetDir, *repoFlag)
	if err == nil && *dryRun {
		report, err = s.dryRun(ctx, env, targetDir)
	} else if err == nil {
		var f *fire.Fire
		if f, err = s.newFire(ctx, env); err == nil {
			record := f.Once(ctx)
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

// session is what the run command needs before it can look at the Target Project:
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
	// A renamed or moved repository answers to its old name; everything
	// after this uses the name GitHub gives it now.
	if current, currentName, err := config.ParseRepo(repository.FullName); err == nil {
		owner, name = current, currentName
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
	facts, warnings, err := fire.ReadFacts(ctx, fire.Source{
		GitHub: s.client, Repo: s.repo, Login: login, Harness: cfg.Harness, Live: fire.Live(cfg.Host.StateDir), Sleep: env.Sleep,
	})
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

// always runs the Daemon until it is stopped: Fire after Fire, each in the
// Daemon's own clone, with the sleeps between them the last one asks for.
// Only a Daemon that cannot start fails; a stopped one exits 0.
func always(ctx context.Context, env Env, targetDir, repoSlug string) int {
	s, err := openSession(ctx, env, targetDir, repoSlug)
	var f *fire.Fire
	if err == nil {
		f, err = s.newFire(ctx, env)
	}
	if err == nil {
		var created []string
		created, _, err = daemon.EnsureLabels(ctx, s.client, s.repo.Owner, s.repo.Name)
		for _, name := range created {
			fmt.Fprintf(env.Stderr, "auto-agent: run: created the label %s\n", name)
		}
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "auto-agent: run: %v\n", err)
		return exitFailed
	}
	f.StopBetweenSteps = true
	d := daemon.Daemon{Fire: f, Gate: f.Gate, Now: env.Now, Sleep: env.Sleep, Cycles: env.Cycles, Log: env.Stdout}
	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintf(env.Stderr, "auto-agent: run: %v\n", err)
		return exitFailed
	}
	return exitOK
}

// newFire is a Fire in the Daemon's own clone. An error is a Fire that
// could not start, and so has no record.
func (s session) newFire(ctx context.Context, env Env) (*fire.Fire, error) {
	host, err := config.LoadHost(s.repo, env.LookupEnv)
	if err != nil {
		return nil, err
	}
	login, err := s.client.Login(ctx)
	if err != nil {
		return nil, err
	}
	usageURL, _ := env.LookupEnv(EnvUsageAPI)
	return &fire.Fire{
		Repo: s.repo, Host: host, Login: login, GitHub: s.client, Runner: env.Runner,
		Gate: &budget.Sensor{
			StateDir: host.StateDir, DeclaredMode: host.AuthMode, UsageURL: usageURL,
			Policy: budget.Policy{
				MinPct: float64(host.Gate.MinPct), StaleMax: time.Duration(host.Gate.StaleMaxSecs) * time.Second,
				Primary: host.Model.Primary, Fallback: host.Model.Fallback, SwitchPct: float64(host.Model.SwitchPct),
			},
			LookupEnv: env.LookupEnv, AuthStatus: env.AuthStatus, Now: env.Now,
		},
		Workspace: &workspace.Workspace{
			StateDir: host.StateDir, Remote: gitRemote(s.repo, env.LookupEnv), DefaultBranch: s.repo.DefaultBranch, Token: s.token,
			Author: workspace.Author{Name: login, Email: login + "@users.noreply.github.com"},
		},
		Journal: journal.Journal{StateDir: host.StateDir},
		Now:     env.Now,
		Sleep:   env.Sleep,
	}, nil
}
