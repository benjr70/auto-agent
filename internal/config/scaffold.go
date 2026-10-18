package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The commands a scaffold declares, in the order init reports them.
const (
	CommandInstall = "install"
	CommandTest    = "test"
	CommandLint    = "lint"
)

// noInstall is the install command of a repo init finds nothing to install
// for: the key is required, and doing nothing is the honest default.
const noInstall = "true"

// Detected is one command of a scaffold and where it came from. An empty
// Source means nothing was found and Command is a placeholder.
type Detected struct {
	Key     string
	Command string
	Source  string
}

// Scaffold is a Harness config for a Target Project that has none yet: what
// init writes, already validated, and the commands it detected.
type Scaffold struct {
	Data     []byte
	Commands []Detected
}

// scaffoldFile is harness.json as init writes it, keys in reading order. The
// schema allows no comments, so the "$schema" key is what tells a reader
// where every section is explained.
type scaffoldFile struct {
	Schema       string   `json:"$schema"`
	CommitScopes []string `json:"commit_scopes"`
	Commands     struct {
		Install string `json:"install"`
		Test    string `json:"test"`
		Lint    string `json:"lint"`
	} `json:"commands"`
	Pick struct {
		Labels struct{} `json:"labels"`
	} `json:"pick"`
	DocsResearchPrefix string   `json:"docs_research_prefix"`
	RequiredChecks     []string `json:"required_checks"`
	Rounds             struct {
		PRWatch      int `json:"pr_watch"`
		ManualVerify int `json:"manual_verify"`
		Revise       int `json:"revise"`
		DepsFix      int `json:"deps_fix"`
		PauseResume  int `json:"pause_resume"`
	} `json:"rounds"`
	Host struct {
		Docker bool `json:"docker"`
	} `json:"host"`
}

// scaffoldScopes are the commit scopes a scaffold starts from: init cannot
// know a project's areas, so it offers the ones every project has.
var scaffoldScopes = []string{"core", "docs", "ci"}

// NewScaffold builds the Harness config scaffold for the Target Project in
// targetDir from what the repo has. It reads files only: no network, no git.
func NewScaffold(targetDir string) (Scaffold, error) {
	commands := detectCommands(targetDir)
	var f scaffoldFile
	f.Schema, _ = harnessSchema["$id"].(string)
	f.CommitScopes = scaffoldScopes
	f.Commands.Install = commands[0].Command
	f.Commands.Test = commands[1].Command
	f.Commands.Lint = commands[2].Command
	schemaDefault("docs_research_prefix", &f.DocsResearchPrefix)
	schemaDefault("required_checks", &f.RequiredChecks)
	schemaDefault("rounds/pr_watch", &f.Rounds.PRWatch)
	schemaDefault("rounds/manual_verify", &f.Rounds.ManualVerify)
	schemaDefault("rounds/revise", &f.Rounds.Revise)
	schemaDefault("rounds/deps_fix", &f.Rounds.DepsFix)
	schemaDefault("rounds/pause_resume", &f.Rounds.PauseResume)
	schemaDefault("host/docker", &f.Host.Docker)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		return Scaffold{}, err
	}
	if err := ValidateHarness(filepath.Join(targetDir, HarnessDirName, HarnessFileName), buf.Bytes()); err != nil {
		return Scaffold{}, err
	}
	return Scaffold{Data: buf.Bytes(), Commands: commands}, nil
}

// placeholder is a command that fails loudly until the operator sets it, so
// a scaffold left as written stops the first Fire rather than passing it.
func placeholder(key string) string {
	return "echo 'auto-agent init found no " + key + " command: set commands." + key + " in " +
		HarnessDirName + "/" + HarnessFileName + "' >&2; exit 1"
}

// ecosystem is one kind of repo init recognises, and the commands it implies.
type ecosystem struct {
	source   string
	commands map[string]string
}

// detectCommands is the install, test and lint commands, in that order. A
// Makefile target is the repo's own entry point and wins for its command;
// otherwise every ecosystem found contributes, joined with &&.
func detectCommands(dir string) []Detected {
	makefile, targets := makeTargets(dir)
	found := ecosystems(dir)
	out := make([]Detected, 0, 3)
	for _, key := range []string{CommandInstall, CommandTest, CommandLint} {
		d := Detected{Key: key}
		if targets[key] {
			d.Command, d.Source = "make "+key, makefile+" target "+key
		} else {
			var cmds, sources []string
			for _, e := range found {
				if c := e.commands[key]; c != "" {
					cmds, sources = append(cmds, c), append(sources, e.source)
				}
			}
			d.Command, d.Source = strings.Join(cmds, " && "), strings.Join(sources, ", ")
		}
		if d.Command == "" {
			d.Command = placeholder(key)
			if key == CommandInstall {
				d.Command = noInstall
			}
		}
		out = append(out, d)
	}
	return out
}

func ecosystems(dir string) []ecosystem {
	var found []ecosystem
	if exists(dir, "go.mod") {
		found = append(found, ecosystem{"go.mod", map[string]string{
			CommandInstall: "go mod download",
			CommandTest:    "go test ./...",
			CommandLint:    "go vet ./...",
		}})
	}
	if e, ok := nodeEcosystem(dir); ok {
		found = append(found, e)
	}
	if exists(dir, "pyproject.toml") {
		e := ecosystem{"pyproject.toml", map[string]string{
			CommandInstall: "python3 -m pip install -e .",
			CommandTest:    "python3 -m pytest",
		}}
		pyproject, _ := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
		if exists(dir, "ruff.toml") || exists(dir, ".ruff.toml") || bytes.Contains(pyproject, []byte("[tool.ruff")) {
			e.commands[CommandLint] = "ruff check ."
		}
		found = append(found, e)
	}
	return found
}

// npmDefaultTest is the test script npm init writes, which tests nothing.
const npmDefaultTest = `echo "Error: no test specified" && exit 1`

// nodeEcosystem reads package.json: its install comes from the lockfile, its
// test and lint from its own scripts.
func nodeEcosystem(dir string) (ecosystem, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ecosystem{}, false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	_ = json.Unmarshal(data, &pkg) // an unreadable package.json still means Node
	tool, install := "npm", "npm install"
	switch {
	case exists(dir, "pnpm-lock.yaml"):
		tool, install = "pnpm", "pnpm install --frozen-lockfile"
	case exists(dir, "yarn.lock"):
		tool, install = "yarn", "yarn install --frozen-lockfile"
	case exists(dir, "package-lock.json"):
		install = "npm ci"
	}
	e := ecosystem{"package.json", map[string]string{CommandInstall: install}}
	if s := pkg.Scripts["test"]; s != "" && s != npmDefaultTest {
		e.commands[CommandTest] = tool + " test"
	}
	if pkg.Scripts["lint"] != "" {
		e.commands[CommandLint] = tool + " run lint"
	}
	return e, true
}

var makeTargetLine = regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+)[ \t]*::?([^=]|$)`)

// makeTargets is the Makefile's name and the targets it defines.
func makeTargets(dir string) (string, map[string]bool) {
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		targets := map[string]bool{}
		for _, m := range makeTargetLine.FindAllSubmatch(data, -1) {
			targets[string(m[1])] = true
		}
		return name, targets
	}
	return "", nil
}

func exists(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && info.Mode().IsRegular()
}
