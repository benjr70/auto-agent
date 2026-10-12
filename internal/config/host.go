package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// EnvHostConfig names the Host config file, in place of the per-project
// default. It locates the file; it is not itself a Host config key.
const EnvHostConfig = "AUTO_AGENT_HOST_CONFIG"

// Host is the Host config: the machine's facts for one Target Project. Every
// key has a default, so no file is needed, and it never holds a secret.
type Host struct {
	// StateDir is where the Daemon keeps this Target Project's clone, Fire
	// records and logs.
	StateDir  string
	Model     ModelPolicy
	Gate      GatePolicy
	Dashboard Dashboard
}

// ModelPolicy is the Model policy: the family Steps run on, the one they
// switch to when it is spent (empty: never switch, wait for the reset) and
// the per-model utilization that switches.
type ModelPolicy struct {
	Primary   string
	Fallback  string
	SwitchPct int
}

// GatePolicy holds the Budget gate's thresholds: the remaining percent at or
// above which work starts, and how old a remembered reading may be.
type GatePolicy struct {
	MinPct       int
	StaleMaxSecs int
}

// Dashboard is the address the Dashboard listens on.
type Dashboard struct {
	Bind string
	Port int
}

// hostKeys are the Host config's keys. Each is spelled the same in the file
// and in the environment, which is what lets the environment override it.
var hostKeys = []struct {
	name string
	set  func(*Host, string) error
}{
	{"AUTO_AGENT_STATE_DIR", func(h *Host, v string) error {
		if !filepath.IsAbs(v) {
			return errors.New("must be an absolute path")
		}
		h.StateDir = filepath.Clean(v)
		return nil
	}},
	{"AUTO_AGENT_MODEL_PRIMARY", func(h *Host, v string) error { return setText(&h.Model.Primary, v) }},
	{"AUTO_AGENT_MODEL_FALLBACK", func(h *Host, v string) error { h.Model.Fallback = v; return nil }},
	{"AUTO_AGENT_MODEL_SWITCH_PCT", func(h *Host, v string) error { return setPercent(&h.Model.SwitchPct, v) }},
	{"AUTO_AGENT_GATE_MIN_PCT", func(h *Host, v string) error { return setPercent(&h.Gate.MinPct, v) }},
	{"AUTO_AGENT_GATE_STALE_MAX_SECS", func(h *Host, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return errors.New("must be a whole number of seconds, 1 or more")
		}
		h.Gate.StaleMaxSecs = n
		return nil
	}},
	{"AUTO_AGENT_DASHBOARD_BIND", func(h *Host, v string) error { return setText(&h.Dashboard.Bind, v) }},
	{"AUTO_AGENT_DASHBOARD_PORT", func(h *Host, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("must be a port from 1 to 65535")
		}
		h.Dashboard.Port = n
		return nil
	}},
}

func setText(field *string, v string) error {
	if v == "" {
		return errors.New("must not be empty")
	}
	*field = v
	return nil
}

func setPercent(field *int, v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 100 {
		return errors.New("must be a whole number from 0 to 100")
	}
	*field = n
	return nil
}

// credentialKey matches a key that looks like it holds a secret.
var credentialKey = regexp.MustCompile(`(?i)TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|API_?KEY|PRIVATE_?KEY`)

// HostConfigPath is where the Host config file for repo is looked for: the
// file EnvHostConfig names, else one per Target Project under the user's
// config home.
func HostConfigPath(repo Repo, getenv func(string) string) string {
	if path := getenv(EnvHostConfig); path != "" {
		return path
	}
	return filepath.Join(userDir(getenv, "XDG_CONFIG_HOME", ".config"), "auto-agent", repo.Owner, repo.Name, "config.env")
}

// userDir is the directory the XDG variable names, else fallback under HOME.
func userDir(getenv func(string) string, xdg, fallback string) string {
	if dir := getenv(xdg); dir != "" {
		return dir
	}
	return filepath.Join(getenv("HOME"), fallback)
}

// LoadHost resolves the Host config for repo: the defaults, then the Host
// config file when there is one, then the environment, which overrides any
// key. A missing file is not an error unless the environment named it.
func LoadHost(repo Repo, getenv func(string) string) (Host, error) {
	host := Host{
		StateDir:  filepath.Join(userDir(getenv, "XDG_STATE_HOME", filepath.Join(".local", "state")), "auto-agent", repo.Owner, repo.Name),
		Model:     ModelPolicy{Primary: "opus", SwitchPct: 95},
		Gate:      GatePolicy{MinPct: 25, StaleMaxSecs: 3600},
		Dashboard: Dashboard{Bind: "127.0.0.1", Port: 8090},
	}
	setters := map[string]func(*Host, string) error{}
	for _, k := range hostKeys {
		setters[k.name] = k.set
	}

	path := HostConfigPath(repo, getenv)
	var problems []string
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && getenv(EnvHostConfig) == "":
		// No file: every key keeps its default.
	case err != nil:
		return Host{}, fmt.Errorf("the Host config: %w", err)
	default:
		settings, unreadable := parseHostFile(string(data))
		problems = unreadable
		for _, s := range settings {
			set, known := setters[s.key]
			switch {
			case credentialKey.MatchString(s.key):
				problems = append(problems, s.key+": secrets are not stored in the Host config")
			case !known:
				problems = append(problems, s.key+": not a Host config key")
			default:
				if err := set(&host, s.value); err != nil {
					problems = append(problems, s.key+": "+err.Error())
				}
			}
		}
	}
	for _, k := range hostKeys {
		if value := getenv(k.name); value != "" {
			if err := k.set(&host, value); err != nil {
				problems = append(problems, k.name+" (from the environment): "+err.Error())
			}
		}
	}
	if len(problems) > 0 {
		return Host{}, &InvalidError{File: path, Rules: "is not a valid Host config", Problems: problems}
	}
	return host, nil
}

// setting is one KEY=value line of the Host config file.
type setting struct{ key, value string }

var settingKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseHostFile reads the Host config file's lines in the systemd
// EnvironmentFile shape: KEY=value, one layer of quotes, an `export` prefix,
// comments, CRLF. A line it cannot read is named in problems and skipped.
func parseHostFile(content string) (settings []setting, problems []string) {
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimLeft(strings.TrimSuffix(line, "\r"), " \t")
		line = strings.TrimPrefix(line, "export ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			problems = append(problems, fmt.Sprintf("line %d: no '=' in %q", i+1, line))
			continue
		}
		if !settingKey.MatchString(key) {
			problems = append(problems, fmt.Sprintf("line %d: invalid key %q", i+1, key))
			continue
		}
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		settings = append(settings, setting{key, value})
	}
	return settings, problems
}
