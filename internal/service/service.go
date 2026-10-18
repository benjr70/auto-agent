// Package service renders the systemd user unit that keeps one Target
// Project's Daemon running across reboots, and names where it and the
// operator's secrets file live.
package service

import (
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
)

//go:embed auto-agent.service.in
var template string

// EnvUnitPath overrides the PATH the unit runs the Daemon with.
const EnvUnitPath = "AUTO_AGENT_UNIT_PATH"

var placeholder = regexp.MustCompile(`@[A-Z_]+@`)

// Unit is what one Target Project's unit is rendered from.
type Unit struct {
	Repo config.Repo
	// Binary is the absolute path of the auto-agent binary the unit runs.
	Binary string
	// EnvFile is the operator's secrets file the unit reads.
	EnvFile string
	// Path is the PATH the Daemon runs with.
	Path string
}

// Name is the unit's file name: one per Target Project, so that Daemons for
// two of them can share a machine. A GitHub owner never contains a dot, so
// the first dot after the prefix splits owner from name and no two Target
// Projects share a name (a dash would: acme-co/x and acme/co-x).
func Name(repo config.Repo) string {
	return "auto-agent-" + repo.Owner + "." + repo.Name + ".service"
}

// Render fills the template. A value that would break the unit file, or
// smuggle a line into it, is refused.
func Render(u Unit) (string, error) {
	binary, err := value("the binary", u.Binary, true)
	if err != nil {
		return "", err
	}
	envFile, err := value("the environment file", u.EnvFile, true)
	if err != nil {
		return "", err
	}
	path, err := value("PATH", u.Path, false)
	if err != nil {
		return "", err
	}
	repo, err := value("the repo", u.Repo.Slug(), false)
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(Name(u.Repo), ".service")
	unit := strings.NewReplacer(
		"@NAME@", Name(u.Repo), "@IDENT@", name, "@REPO@", repo,
		"@PATH@", path, "@ENV_FILE@", envFile, "@BINARY@", binary,
	).Replace(template)
	if left := placeholder.FindAllString(unit, -1); len(left) > 0 {
		return "", fmt.Errorf("placeholder left unrendered: %s", strings.Join(left, " "))
	}
	return unit, nil
}

// value checks one value for a unit line and escapes % to the unit-file
// literal %%.
func value(what, v string, absolute bool) (string, error) {
	switch {
	case v == "":
		return "", fmt.Errorf("%s is empty", what)
	case strings.ContainsAny(v, "\n\r"):
		return "", fmt.Errorf("%s contains a newline", what)
	case strings.ContainsAny(v, `&\`):
		return "", fmt.Errorf("%s contains & or a backslash: %s", what, v)
	case strings.ContainsAny(v, " \t\"'"):
		return "", fmt.Errorf("%s contains whitespace or a quote: %s", what, v)
	case absolute && !filepath.IsAbs(v):
		return "", fmt.Errorf("%s must be an absolute path: %s", what, v)
	}
	return strings.ReplaceAll(v, "%", "%%"), nil
}

// Dir is the user's systemd unit directory.
func Dir(lookup func(string) (string, bool)) string {
	return filepath.Join(configHome(lookup), "systemd", "user")
}

// EnvFile is where the unit looks for the operator's secrets by default:
// beside the Host config, which never holds one.
func EnvFile(repo config.Repo, lookup func(string) (string, bool)) string {
	return filepath.Join(configHome(lookup), "auto-agent", repo.Owner, repo.Name, "secrets.env")
}

// UnitPath is the PATH the unit runs the Daemon with: EnvUnitPath, else
// ~/.local/bin and the system directories.
func UnitPath(lookup func(string) (string, bool)) string {
	if path, _ := lookup(EnvUnitPath); path != "" {
		return path
	}
	home, _ := lookup("HOME")
	return filepath.Join(home, ".local", "bin") + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

func configHome(lookup func(string) (string, bool)) string {
	if dir, _ := lookup("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	home, _ := lookup("HOME")
	return filepath.Join(home, ".config")
}

// ErrNotLinux is the refusal on any platform but Linux.
var ErrNotLinux = errors.New("service install writes a systemd user unit, so it runs on Linux only")
