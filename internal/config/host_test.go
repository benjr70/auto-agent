package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The case names marked (bash) are the ones lib/host-env.test.sh uses.

func env(pairs ...string) func(string) (string, bool) {
	m := map[string]string{"HOME": "/h"}
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func hostFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	writeFile(t, path, content)
	return path
}

func TestParseHostFile(t *testing.T) {
	t.Run("the systemd EnvironmentFile shapes are read", func(t *testing.T) { // (bash)
		got, problems := parseHostFile("\ufeff# comment\nA=1\nB=\"two words\"\nC='three'\n  D=indented\nexport E=exported\nF=win\r\n\nG=a=b\nH=trailing \n")
		want := []entry{{"A", "1"}, {"B", "two words"}, {"C", "three"}, {"D", "indented"}, {"E", "exported"}, {"F", "win"}, {"G", "a=b"}, {"H", "trailing"}}
		if len(problems) != 0 || !reflect.DeepEqual(got, want) {
			t.Errorf("entries = %v, problems = %q", got, problems)
		}
	})
	t.Run("unreadable lines are named on stderr and skipped, the rest still loads", func(t *testing.T) { // (bash)
		got, problems := parseHostFile("GOOD=1\nnot a pair\n1BAD=x\nALSO_GOOD=2\n")
		if want := []entry{{"GOOD", "1"}, {"ALSO_GOOD", "2"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("entries = %v", got)
		}
		want := []string{"line 2: not a KEY=value line", "line 3: the key is not a valid name"}
		if !reflect.DeepEqual(problems, want) {
			t.Errorf("problems = %q, want %q", problems, want)
		}
	})
}

func TestLoadHost(t *testing.T) {
	defaults := Host{
		StateDir:  "/h/.local/state/auto-agent/acme/widgets",
		Model:     ModelPolicy{Primary: "opus", SwitchPct: 95},
		Gate:      GatePolicy{MinPct: 25, StaleMaxSecs: 3600, ReprobeSecs: 3600},
		Dashboard: Dashboard{Bind: "127.0.0.1", Port: 8090},
	}
	t.Run("a missing file yields every default", func(t *testing.T) {
		got, err := LoadHost(testRepo, env())
		if err != nil || got != defaults {
			t.Errorf("LoadHost = %+v, %v; want %+v", got, err, defaults)
		}
	})
	t.Run("the file overrides a default", func(t *testing.T) {
		file := hostFile(t, "AUTO_AGENT_MODEL_PRIMARY=sonnet\nAUTO_AGENT_MODEL_FALLBACK=haiku\nAUTO_AGENT_MODEL_SWITCH_PCT=80\n"+
			"AUTO_AGENT_GATE_MIN_PCT=10\nAUTO_AGENT_GATE_STALE_MAX_SECS=600\nAUTO_AGENT_PARK_REPROBE_SECS=900\nAUTO_AGENT_DASHBOARD_BIND=0.0.0.0\nAUTO_AGENT_DASHBOARD_PORT=9000\n"+
			"AUTO_AGENT_STATE_DIR=/from/file\n")
		got, err := LoadHost(testRepo, env(EnvHostConfig, file))
		want := Host{
			StateDir:  "/from/file",
			Model:     ModelPolicy{Primary: "sonnet", Fallback: "haiku", SwitchPct: 80},
			Gate:      GatePolicy{MinPct: 10, StaleMaxSecs: 600, ReprobeSecs: 900},
			Dashboard: Dashboard{Bind: "0.0.0.0", Port: 9000},
		}
		if err != nil || got != want {
			t.Errorf("LoadHost = %+v, %v; want %+v", got, err, want)
		}
	})
	t.Run("an exported key wins over the file; a missing file is a no-op", func(t *testing.T) { // (bash)
		file := hostFile(t, "AUTO_AGENT_STATE_DIR=/from/file\nAUTO_AGENT_GATE_MIN_PCT=10\n")
		got, err := LoadHost(testRepo, env(EnvHostConfig, file, "AUTO_AGENT_STATE_DIR", "/from/env"))
		if err != nil || got.StateDir != "/from/env" || got.Gate.MinPct != 10 {
			t.Errorf("LoadHost = %+v, %v; want the environment's State dir and the file's gate", got, err)
		}
		got, err = LoadHost(testRepo, env("AUTO_AGENT_DASHBOARD_PORT", "9999"))
		if err != nil || got.Dashboard.Port != 9999 || got.StateDir != defaults.StateDir {
			t.Errorf("LoadHost with no file = %+v, %v", got, err)
		}
	})
	t.Run("an empty environment value clears the fallback and is unset for every other key", func(t *testing.T) {
		file := hostFile(t, "AUTO_AGENT_MODEL_FALLBACK=haiku\nAUTO_AGENT_MODEL_PRIMARY=sonnet\n")
		got, err := LoadHost(testRepo, env(EnvHostConfig, file, "AUTO_AGENT_MODEL_FALLBACK", "", "AUTO_AGENT_MODEL_PRIMARY", "", "AUTO_AGENT_STATE_DIR", ""))
		if err != nil || got.Model.Fallback != "" || got.Model.Primary != "sonnet" || got.StateDir != defaults.StateDir {
			t.Errorf("LoadHost = %+v, %v", got, err)
		}
	})
	t.Run("a key the environment sets is not read from the file, even to be faulted", func(t *testing.T) {
		file := hostFile(t, "AUTO_AGENT_DASHBOARD_PORT=http\n")
		got, err := LoadHost(testRepo, env(EnvHostConfig, file, "AUTO_AGENT_DASHBOARD_PORT", "9001"))
		if err != nil || got.Dashboard.Port != 9001 {
			t.Errorf("LoadHost = %+v, %v", got, err)
		}
	})
	t.Run("no home and no repo are refused rather than guessed around", func(t *testing.T) {
		if _, err := LoadHost(testRepo, func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "State dir") {
			t.Errorf("with no HOME: error = %v", err)
		}
		for _, repo := range []Repo{{}, {Owner: "..", Name: ".."}, {Owner: "acme", Name: "a/b"}} {
			if _, err := LoadHost(repo, env()); err == nil || !strings.Contains(err.Error(), "is not a repo") {
				t.Errorf("LoadHost(%+v) error = %v", repo, err)
			}
		}
	})
	t.Run("the State dir defaults outside any checkout", func(t *testing.T) { // (bash)
		got, _ := LoadHost(testRepo, env("XDG_STATE_HOME", "/x"))
		if got.StateDir != "/x/auto-agent/acme/widgets" {
			t.Errorf("StateDir = %q, want XDG_STATE_HOME honoured", got.StateDir)
		}
		if path := HostConfigPath(testRepo, env()); path != "/h/.config/auto-agent/acme/widgets/config.env" {
			t.Errorf("HostConfigPath = %q", path)
		}
		if path := HostConfigPath(testRepo, env("XDG_CONFIG_HOME", "/c")); path != "/c/auto-agent/acme/widgets/config.env" {
			t.Errorf("HostConfigPath = %q, want XDG_CONFIG_HOME honoured", path)
		}
	})
	t.Run("two Target Projects get distinct State dirs and Host config files", func(t *testing.T) {
		other := Repo{Owner: "acme", Name: "gadgets"}
		a, _ := LoadHost(testRepo, env())
		b, _ := LoadHost(other, env())
		if a.StateDir == b.StateDir || HostConfigPath(testRepo, env()) == HostConfigPath(other, env()) {
			t.Errorf("State dirs %q and %q, or the Host config paths, collide", a.StateDir, b.StateDir)
		}
	})
	t.Run("the Host config file the environment names must exist", func(t *testing.T) {
		_, err := LoadHost(testRepo, env(EnvHostConfig, "/nonexistent/config.env"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want a missing-file error", err)
		}
	})
}

func TestLoadHostRejects(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		environ []string
		want    []string
	}{
		{
			name: "a credential-looking key is refused: secrets are not stored there",
			file: "GH_TOKEN=ghp_x\nCLAUDE_CODE_OAUTH_TOKEN=y\nANTHROPIC_API_KEY=z\nAUTO_AGENT_DASHBOARD_PASSWORD=p\n",
			want: []string{
				"GH_TOKEN: secrets are not stored in the Host config",
				"CLAUDE_CODE_OAUTH_TOKEN: secrets are not stored in the Host config",
				"ANTHROPIC_API_KEY: secrets are not stored in the Host config",
				"AUTO_AGENT_DASHBOARD_PASSWORD: secrets are not stored in the Host config",
			},
		},
		{
			name: "the refusal never repeats the secret",
			file: "GH_TOKEN=ghp_supersecret\nGH_TOKEN ghp_supersecret\n1GH_TOKEN=ghp_supersecret\n",
			want: []string{"line 2: not a KEY=value line", "line 3: the key is not a valid name", "GH_TOKEN: secrets are not stored in the Host config"},
		},
		{
			name: "an unknown key is named",
			file: "AUTO_AGENT_MODLE_PRIMARY=opus\nAUTO_AGENT_MAX_TOKENS=5\n",
			want: []string{"AUTO_AGENT_MODLE_PRIMARY: not a Host config key", "AUTO_AGENT_MAX_TOKENS: not a Host config key"},
		},
		{
			name: "unreadable lines are named",
			file: "not a pair\n1BAD=x\n",
			want: []string{"line 1: not a KEY=value line", "line 2: the key is not a valid name"},
		},
		{
			name: "values out of range name their key",
			file: "AUTO_AGENT_MODEL_SWITCH_PCT=101\nAUTO_AGENT_GATE_MIN_PCT=-1\nAUTO_AGENT_GATE_STALE_MAX_SECS=soon\nAUTO_AGENT_PARK_REPROBE_SECS=0\nAUTO_AGENT_DASHBOARD_PORT=70000\n" +
				"AUTO_AGENT_STATE_DIR=relative/dir\nAUTO_AGENT_MODEL_PRIMARY=\nAUTO_AGENT_DASHBOARD_BIND=\n",
			want: []string{
				"AUTO_AGENT_MODEL_SWITCH_PCT: must be a whole number from 0 to 100",
				"AUTO_AGENT_GATE_MIN_PCT: must be a whole number from 0 to 100",
				"AUTO_AGENT_GATE_STALE_MAX_SECS: must be a whole number of seconds, 1 or more",
				"AUTO_AGENT_PARK_REPROBE_SECS: must be a whole number of seconds, 1 or more",
				"AUTO_AGENT_DASHBOARD_PORT: must be a port from 1 to 65535",
				"AUTO_AGENT_STATE_DIR: must be an absolute path",
				"AUTO_AGENT_MODEL_PRIMARY: must not be empty",
				"AUTO_AGENT_DASHBOARD_BIND: must not be empty",
			},
		},
		{
			name:    "a bad value in the environment is named as the environment's",
			environ: []string{"AUTO_AGENT_DASHBOARD_PORT", "http"},
			want:    []string{"AUTO_AGENT_DASHBOARD_PORT (from the environment): must be a port from 1 to 65535"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			environ := tt.environ
			if tt.file != "" {
				environ = append(environ, EnvHostConfig, hostFile(t, tt.file))
			}
			_, err := LoadHost(testRepo, env(environ...))
			var invalid *InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("LoadHost error = %v, want an *InvalidError", err)
			}
			if !reflect.DeepEqual(invalid.Problems, tt.want) {
				t.Errorf("problems = %q\n      want %q", invalid.Problems, tt.want)
			}
			if strings.Contains(err.Error(), "supersecret") {
				t.Errorf("the error repeats the secret: %v", err)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := makeTarget(t, "")
	t.Run("the Harness config and the Host config come back as one validated value", func(t *testing.T) {
		cfg, err := Load(Input{TargetDir: dir, Repo: testRepo, LookupEnv: env("AUTO_AGENT_GATE_MIN_PCT", "40")})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Harness.Repo != testRepo || cfg.Harness.Commands.Test == "" || cfg.Host.Gate.MinPct != 40 {
			t.Errorf("Load = %+v", cfg)
		}
	})
	t.Run("an invalid Host config fails the load", func(t *testing.T) {
		_, err := Load(Input{TargetDir: dir, Repo: testRepo, LookupEnv: env("AUTO_AGENT_GATE_MIN_PCT", "lots")})
		if err == nil || !strings.Contains(err.Error(), "AUTO_AGENT_GATE_MIN_PCT") {
			t.Errorf("error = %v, want it to name the key", err)
		}
	})
	t.Run("an invalid Harness config fails the load", func(t *testing.T) {
		_, err := Load(Input{TargetDir: makeTarget(t, `{}`), Repo: testRepo, LookupEnv: env()})
		if err == nil || !strings.Contains(err.Error(), `missing required key "pick"`) {
			t.Errorf("error = %v, want it to name the key", err)
		}
	})
}
