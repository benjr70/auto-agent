package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/service"
)

// ExecCommand runs a command and returns its combined output.
func ExecCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func serviceCmd(args []string, env Env) int {
	if len(args) == 0 {
		return usageError(env.Stderr, "service: name a subcommand: install")
	}
	if args[0] != "install" {
		return usageError(env.Stderr, fmt.Sprintf("service: unknown subcommand %q", args[0]))
	}
	return serviceInstall(args[1:], env)
}

// serviceInstall writes the systemd user unit that runs the Daemon for one
// Target Project, and enables it unless told only to print how.
func serviceInstall(args []string, env Env) int {
	flags := flag.NewFlagSet("service install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	force := flags.Bool("force", false, "")
	noEnable := flags.Bool("no-enable", false, "")
	envFlag := flags.String("env-file", "", "")
	repoFlag := flags.String("repo", "", "")
	var positional []string
	for rest := args; ; {
		err := flags.Parse(rest)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return exitOK
		}
		if err != nil {
			return usageError(env.Stderr, "service install: "+err.Error())
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(positional) > 1 {
		return usageError(env.Stderr, "service install takes at most one target dir")
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "auto-agent: service install: %v\n", err)
		return exitFailed
	}
	if env.GOOS != "linux" {
		return fail(fmt.Errorf("%w; this is %s", service.ErrNotLinux, env.GOOS))
	}

	slug := *repoFlag
	if slug == "" {
		targetDir := "."
		if len(positional) == 1 {
			targetDir = positional[0]
		}
		origin, err := env.OriginURL(targetDir)
		if err != nil {
			return fail(err)
		}
		slug = origin
	}
	owner, name, err := config.ParseRepo(slug)
	if err != nil {
		return fail(err)
	}
	repo := config.Repo{Owner: owner, Name: name}
	binary, err := env.Executable()
	if err != nil {
		return fail(fmt.Errorf("finding this binary: %w", err))
	}
	envFile := *envFlag
	if envFile == "" {
		envFile = service.EnvFile(repo, env.LookupEnv)
	} else if envFile, err = filepath.Abs(envFile); err != nil {
		return fail(err)
	}
	unit, err := service.Render(service.Unit{Repo: repo, Binary: binary, EnvFile: envFile, Path: service.UnitPath(env.LookupEnv)})
	if err != nil {
		return fail(err)
	}

	unitName := service.Name(repo)
	file := filepath.Join(service.Dir(env.LookupEnv), unitName)
	if _, err := os.Stat(file); err == nil && !*force {
		return fail(fmt.Errorf("%s already exists: re-run with --force to replace it", file))
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(file+".tmp", []byte(unit), 0o644); err != nil {
		return fail(err)
	}
	if err := os.Rename(file+".tmp", file); err != nil {
		return fail(err)
	}
	fmt.Fprintf(env.Stdout, "wrote %s\n", file)

	if *noEnable {
		fmt.Fprintf(env.Stdout, "To start it now and at every boot:\n  systemctl --user daemon-reload\n  systemctl --user enable --now %s\n", unitName)
	} else {
		for _, step := range [][]string{{"daemon-reload"}, {"enable", unitName}, {"restart", unitName}} {
			args := append([]string{"--user"}, step...)
			if out, err := env.Command("systemctl", args...); err != nil {
				return fail(fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out))))
			}
		}
		fmt.Fprintf(env.Stdout, "enabled and started %s\n", unitName)
	}

	if _, err := os.Stat(envFile); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.Stdout, "No secrets file at %s. If the Daemon needs GH_TOKEN or a Claude token, create it\n"+
			"(KEY=value lines, then chmod 600) and restart the unit; auto-agent never writes it.\n", envFile)
	}
	if login := currentUser(env.LookupEnv); login != "" {
		out, err := env.Command("loginctl", "show-user", login, "--property=Linger", "--value")
		if err != nil || strings.TrimSpace(string(out)) != "yes" {
			fmt.Fprintf(env.Stdout, "Lingering is off for %s, so the Daemon stops at logout and waits for a login after a reboot. To keep it running:\n"+
				"  sudo loginctl enable-linger %s\n", login, login)
		}
	}
	fmt.Fprintf(env.Stdout, "Follow its logs with:\n  journalctl --user -u %s -f\n", unitName)
	return exitOK
}

// currentUser is the login the unit runs as.
func currentUser(lookup func(string) (string, bool)) string {
	if name, _ := lookup("USER"); name != "" {
		return name
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
