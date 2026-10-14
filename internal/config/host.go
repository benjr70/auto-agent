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
	// AuthMode is how Claude is logged in, as the operator declares it:
	// login, setup-token or api-key. It has no default: the Usage sensor
	// refuses to guess it, and cross-checks what is declared.
	AuthMode string
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
// above which work starts, how old a remembered reading may be, and how often
// a Parked Daemon probes its Claude credential.
type GatePolicy struct {
	MinPct       int
	StaleMaxSecs int
	ReprobeSecs  int
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
	// emptyMeans is set where an empty value says something (no fallback);
	// elsewhere an empty environment variable counts as unset, as it did in
	// the Host env.
	emptyMeans bool
}{
	{name: "AUTO_AGENT_STATE_DIR", set: func(h *Host, v string) error {
		if !filepath.IsAbs(v) {
			return errors.New("must be an absolute path")
		}
		h.StateDir = filepath.Clean(v)
		return nil
	}},
	{name: "AUTO_AGENT_MODEL_PRIMARY", set: func(h *Host, v string) error { return setText(&h.Model.Primary, v) }},
	{name: "AUTO_AGENT_MODEL_FALLBACK", set: func(h *Host, v string) error { h.Model.Fallback = v; return nil }, emptyMeans: true},
	{name: "AUTO_AGENT_MODEL_SWITCH_PCT", set: func(h *Host, v string) error { return setPercent(&h.Model.SwitchPct, v) }},
	{name: "AUTO_AGENT_GATE_MIN_PCT", set: func(h *Host, v string) error { return setPercent(&h.Gate.MinPct, v) }},
	{name: "AUTO_AGENT_GATE_STALE_MAX_SECS", set: func(h *Host, v string) error { return setSeconds(&h.Gate.StaleMaxSecs, v) }},
	{name: "AUTO_AGENT_PARK_REPROBE_SECS", set: func(h *Host, v string) error { return setSeconds(&h.Gate.ReprobeSecs, v) }},
	{name: "CLAUDE_AUTH_MODE", set: func(h *Host, v string) error {
		if v != "login" && v != "setup-token" && v != "api-key" {
			return errors.New("must be login, setup-token or api-key")
		}
		h.AuthMode = v
		return nil
	}},
	{name: "AUTO_AGENT_DASHBOARD_BIND", set: func(h *Host, v string) error { return setText(&h.Dashboard.Bind, v) }},
	{name: "AUTO_AGENT_DASHBOARD_PORT", set: func(h *Host, v string) error {
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

func setSeconds(field *int, v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return errors.New("must be a whole number of seconds, 1 or more")
	}
	*field = n
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
// It matches whole words of the key, so AUTO_AGENT_MAX_TOKENS is not one.
var credentialKey = regexp.MustCompile(`(?i)(^|_)(TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?|PAT|KEY|APIKEY)(_|$)`)

// HostConfigPath is where the Host config file for repo is looked for: the
// file EnvHostConfig names, else one per Target Project under the user's
// config home.
func HostConfigPath(repo Repo, lookup func(string) (string, bool)) string {
	if path, _ := lookup(EnvHostConfig); path != "" {
		return path
	}
	return filepath.Join(userDir(lookup, "XDG_CONFIG_HOME", ".config"), "auto-agent", repo.Owner, repo.Name, "config.env")
}

// userDir is the directory the XDG variable names, else fallback under HOME.
func userDir(lookup func(string) (string, bool), xdg, fallback string) string {
	if dir, _ := lookup(xdg); dir != "" {
		return dir
	}
	home, _ := lookup("HOME")
	return filepath.Join(home, fallback)
}

// LoadHost resolves the Host config for repo: the defaults, then the Host
// config file when there is one, then the environment, which overrides any
// key. A missing file is not an error unless the environment named it.
func LoadHost(repo Repo, lookup func(string) (string, bool)) (Host, error) {
	if !repo.named() {
		return Host{}, fmt.Errorf("the Host config is per Target Project: %q is not a repo", repo.Slug())
	}
	host := Host{
		StateDir:  filepath.Join(userDir(lookup, "XDG_STATE_HOME", filepath.Join(".local", "state")), "auto-agent", repo.Owner, repo.Name),
		Model:     ModelPolicy{Primary: "opus", SwitchPct: 95},
		Gate:      GatePolicy{MinPct: 25, StaleMaxSecs: 3600, ReprobeSecs: 3600},
		Dashboard: Dashboard{Bind: "127.0.0.1", Port: 8090},
	}
	if !filepath.IsAbs(host.StateDir) {
		return Host{}, errors.New("cannot place the State dir: neither HOME nor XDG_STATE_HOME names an absolute directory")
	}
	// The environment overrides the file, so a key it sets is never read
	// from the file, not even to be faulted.
	setters := map[string]func(*Host, string) error{}
	fromEnv := map[string]string{}
	for _, k := range hostKeys {
		setters[k.name] = k.set
		if value, set := lookup(k.name); set && (value != "" || k.emptyMeans) {
			fromEnv[k.name] = value
		}
	}

	path := HostConfigPath(repo, lookup)
	named, _ := lookup(EnvHostConfig)
	var problems []string
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && named == "":
		// No file: every key keeps its default.
	case err != nil:
		return Host{}, fmt.Errorf("the Host config: %w", err)
	default:
		entries, unreadable := parseHostFile(string(data))
		problems = unreadable
		for _, e := range entries {
			set, known := setters[e.key]
			_, overridden := fromEnv[e.key]
			switch {
			case known && overridden:
			case known:
				if err := set(&host, e.value); err != nil {
					problems = append(problems, e.key+": "+err.Error())
				}
			case credentialKey.MatchString(e.key):
				problems = append(problems, e.key+": secrets are not stored in the Host config")
			default:
				problems = append(problems, e.key+": not a Host config key")
			}
		}
	}
	for _, k := range hostKeys {
		if value, set := fromEnv[k.name]; set {
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

// entry is one KEY=value line of the Host config file.
type entry struct{ key, value string }

var entryKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseHostFile reads the Host config file's lines in the systemd
// EnvironmentFile shape: KEY=value, one layer of quotes, an `export` prefix,
// comments, CRLF. A line it cannot read is named in problems by its number
// alone, never quoted: it may be a mistyped secret.
func parseHostFile(content string) (entries []entry, problems []string) {
	for i, line := range strings.Split(strings.TrimPrefix(content, "\ufeff"), "\n") {
		line = strings.Trim(line, " \t\r")
		line = strings.TrimPrefix(line, "export ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			problems = append(problems, fmt.Sprintf("line %d: not a KEY=value line", i+1))
			continue
		}
		if !entryKey.MatchString(key) {
			problems = append(problems, fmt.Sprintf("line %d: the key is not a valid name", i+1))
			continue
		}
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		entries = append(entries, entry{key, value})
	}
	return entries, problems
}
