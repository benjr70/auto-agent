package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Where a Target Project keeps its Harness config and the prose beside it.
const (
	HarnessDirName  = ".auto-agent"
	HarnessFileName = "harness.json"

	proseVerifierRunbook = "verifier-runbook.md"
	proseBotPRChecklist  = "bot-pr-checklist.md"
	proseDeployedChecks  = "deployed-checks.md"
)

// The two shapes of pick signal.
const (
	PickProject = "project"
	PickLabels  = "labels"
)

// Harness is a Target Project's resolved Harness config: every key present,
// absent optionals defaulted or zero. Its JSON form is the resolved shape the
// bash loader prints.
type Harness struct {
	// ConfigDir is the absolute path of the Target Project's .auto-agent.
	ConfigDir          string             `json:"config_dir"`
	Repo               Repo               `json:"repo"`
	Pick               Pick               `json:"pick"`
	CommitScopes       []string           `json:"commit_scopes"`
	Commands           Commands           `json:"commands"`
	DocsResearchPrefix string             `json:"docs_research_prefix"`
	RequiredChecks     []string           `json:"required_checks"`
	Rounds             Rounds             `json:"rounds"`
	Verification       Verification       `json:"verification"`
	Surfaces           map[string]Surface `json:"surfaces"`
	Lanes              Lanes              `json:"lanes"`
	Host               HostNeeds          `json:"host"`
	Prose              Prose              `json:"prose"`
}

// TargetDir is the Target Project checkout the config came from.
func (h Harness) TargetDir() string { return filepath.Dir(h.ConfigDir) }

// Repo names the Target Project on GitHub. The Harness config never declares
// it: the caller derives the owner and name (ParseRepo) and detects the
// default branch.
type Repo struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
}

// Slug is "owner/name".
func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// Pick is the pick signal: a Project board with a priority field, or labels
// alone. Project is nil under PickLabels.
type Pick struct {
	Shape   string   `json:"shape"`
	Project *Project `json:"project"`
}

type Project struct {
	Number        int      `json:"number"`
	PriorityField string   `json:"priority_field"`
	Order         []string `json:"order"`
}

// Commands are the Target Project's own; an empty optional one is not declared.
type Commands struct {
	Install         string   `json:"install"`
	Test            string   `json:"test"`
	Lint            string   `json:"lint"`
	LockfileRefresh string   `json:"lockfile_refresh"`
	PlanGatedPaths  []string `json:"plan_gated_paths"`
}

type Rounds struct {
	PRWatch      int `json:"pr_watch"`
	ManualVerify int `json:"manual_verify"`
	Revise       int `json:"revise"`
	DepsFix      int `json:"deps_fix"`
	PauseResume  int `json:"pause_resume"`
}

// Verification holds the two tiers; a nil tier is not declared, and a nil
// Hermetic is the Bootstrap state.
type Verification struct {
	Hermetic *Hermetic `json:"hermetic"`
	Deployed *Deployed `json:"deployed"`
}

type Hermetic struct {
	Command string `json:"command"`
	Smoke   bool   `json:"smoke"`
}

type Deployed struct {
	Command string `json:"command"`
	Enabled bool   `json:"enabled"`
}

// Surface is one thing the verifier can drive. Viewport and Launcher are
// empty when not declared.
type Surface struct {
	Kind     string   `json:"kind"`
	URLKey   string   `json:"url_key"`
	Paths    []string `json:"paths"`
	Viewport string   `json:"viewport"`
	Launcher string   `json:"launcher"`
}

// Lanes are the optional Lanes: present when the config declares them, and
// enabled unless it switches them off.
type Lanes struct {
	DepsLand Lane `json:"deps_land"`
	Deployed Lane `json:"deployed"`
}

type Lane struct {
	Present bool `json:"present"`
	Enabled bool `json:"enabled"`
}

// HostNeeds is what the Target Project needs of the machine.
type HostNeeds struct {
	Docker bool `json:"docker"`
}

// Prose holds the absolute paths of the prose files beside the Harness
// config; an empty path means the file is absent.
type Prose struct {
	VerifierRunbook string `json:"verifier_runbook"`
	BotPRChecklist  string `json:"bot_pr_checklist"`
	DeployedChecks  string `json:"deployed_checks"`
}

// InvalidError is a config file that was read but breaks the rules; each
// problem names the key at fault.
type InvalidError struct {
	File     string
	Rules    string
	Problems []string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("%s %s:\n  %s", e.File, e.Rules, strings.Join(e.Problems, "\n  "))
}

// harnessFile is harness.json as written; a nil pointer is a key left out.
type harnessFile struct {
	CommitScopes []string `json:"commit_scopes"`
	Commands     Commands `json:"commands"`
	Pick         struct {
		Project *struct {
			Number        int      `json:"number"`
			PriorityField *string  `json:"priority_field"`
			Order         []string `json:"order"`
		} `json:"project"`
	} `json:"pick"`
	DocsResearchPrefix *string  `json:"docs_research_prefix"`
	RequiredChecks     []string `json:"required_checks"`
	Rounds             struct {
		PRWatch      *int `json:"pr_watch"`
		ManualVerify *int `json:"manual_verify"`
		Revise       *int `json:"revise"`
		DepsFix      *int `json:"deps_fix"`
		PauseResume  *int `json:"pause_resume"`
	} `json:"rounds"`
	Verification struct {
		Hermetic *struct {
			Command string `json:"command"`
			Smoke   *bool  `json:"smoke"`
		} `json:"hermetic"`
		Deployed *struct {
			Command string `json:"command"`
			Enabled *bool  `json:"enabled"`
		} `json:"deployed"`
	} `json:"verification"`
	Surfaces   map[string]Surface `json:"surfaces"`
	Dependabot *struct {
		Enabled *bool `json:"enabled"`
	} `json:"dependabot"`
	Host struct {
		Docker *bool `json:"docker"`
	} `json:"host"`
}

// LoadHarness reads and validates a Target Project's Harness config and
// returns it resolved. file is the harness.json to read; empty means the one
// in targetDir. Naming a file from elsewhere (one taken from a PR head,
// ADR 0007) still resolves the prose beside targetDir's own config. It makes
// no network call: repo is the caller's to supply.
func LoadHarness(targetDir, file string, repo Repo) (Harness, error) {
	if info, err := os.Stat(targetDir); err != nil || !info.IsDir() {
		return Harness{}, fmt.Errorf("target dir not found: %s", targetDir)
	}
	configDir, err := filepath.Abs(filepath.Join(targetDir, HarnessDirName))
	if err != nil {
		return Harness{}, err
	}
	if file == "" {
		file = filepath.Join(configDir, HarnessFileName)
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return Harness{}, fmt.Errorf("missing %s: %w", file, os.ErrNotExist)
	} else if err != nil {
		return Harness{}, err
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return Harness{}, fmt.Errorf("%s is not valid JSON: %w", file, err)
	}
	if problems := validate(doc, harnessSchema); len(problems) > 0 {
		return Harness{}, &InvalidError{File: file, Rules: "does not match the Harness config schema", Problems: problems}
	}
	var raw harnessFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return Harness{}, fmt.Errorf("%s: %w", file, err)
	}
	if problems := crossCheck(raw); len(problems) > 0 {
		return Harness{}, &InvalidError{File: file, Rules: "fails the Harness config cross-checks", Problems: problems}
	}
	return resolve(raw, configDir, repo), nil
}

// crossCheck applies the rules the schema cannot express.
func crossCheck(raw harnessFile) []string {
	var problems []string
	names := make([]string, 0, len(raw.Surfaces))
	for name := range raw.Surfaces {
		names = append(names, name)
	}
	sort.Strings(names)
	urlKeys := map[string]int{}
	for _, name := range names {
		s := raw.Surfaces[name]
		if s.Kind == "electron" && s.Launcher == "" {
			problems = append(problems, fmt.Sprintf("/surfaces/%s: an electron surface needs a launcher", name))
		}
		urlKeys[s.URLKey]++
	}
	shared := make([]string, 0)
	for key, n := range urlKeys {
		if n > 1 {
			shared = append(shared, key)
		}
	}
	sort.Strings(shared)
	for _, key := range shared {
		problems = append(problems, fmt.Sprintf("/surfaces: url_key %q is declared by more than one surface", key))
	}
	if raw.Verification.Deployed != nil && raw.Verification.Hermetic == nil {
		problems = append(problems, "/verification/deployed: a deployed tier needs a hermetic tier to defer from")
	}
	return problems
}

// or is the declared value, or the schema's default for the key at path.
func or[T any](declared *T, path string) T {
	if declared != nil {
		return *declared
	}
	var d T
	schemaDefault(path, &d)
	return d
}

func resolve(raw harnessFile, configDir string, repo Repo) Harness {
	h := Harness{
		ConfigDir:          configDir,
		Repo:               repo,
		Pick:               Pick{Shape: PickLabels},
		CommitScopes:       raw.CommitScopes,
		Commands:           raw.Commands,
		DocsResearchPrefix: or(raw.DocsResearchPrefix, "docs_research_prefix"),
		RequiredChecks:     raw.RequiredChecks,
		Rounds: Rounds{
			PRWatch:      or(raw.Rounds.PRWatch, "rounds/pr_watch"),
			ManualVerify: or(raw.Rounds.ManualVerify, "rounds/manual_verify"),
			Revise:       or(raw.Rounds.Revise, "rounds/revise"),
			DepsFix:      or(raw.Rounds.DepsFix, "rounds/deps_fix"),
			PauseResume:  or(raw.Rounds.PauseResume, "rounds/pause_resume"),
		},
		Surfaces: raw.Surfaces,
		Host:     HostNeeds{Docker: or(raw.Host.Docker, "host/docker")},
		Prose: Prose{
			VerifierRunbook: proseFile(configDir, proseVerifierRunbook),
			BotPRChecklist:  proseFile(configDir, proseBotPRChecklist),
			DeployedChecks:  proseFile(configDir, proseDeployedChecks),
		},
	}
	if p := raw.Pick.Project; p != nil {
		project := &Project{Number: p.Number, PriorityField: or(p.PriorityField, "pick/project/priority_field"), Order: p.Order}
		if project.Order == nil {
			schemaDefault("pick/project/order", &project.Order)
		}
		h.Pick = Pick{Shape: PickProject, Project: project}
	}
	if h.Commands.PlanGatedPaths == nil {
		h.Commands.PlanGatedPaths = []string{}
	}
	if h.RequiredChecks == nil {
		schemaDefault("required_checks", &h.RequiredChecks)
	}
	if h.Surfaces == nil {
		h.Surfaces = map[string]Surface{}
	}
	if v := raw.Verification.Hermetic; v != nil {
		h.Verification.Hermetic = &Hermetic{Command: v.Command, Smoke: or(v.Smoke, "verification/hermetic/smoke")}
	}
	if v := raw.Verification.Deployed; v != nil {
		enabled := or(v.Enabled, "verification/deployed/enabled")
		h.Verification.Deployed = &Deployed{Command: v.Command, Enabled: enabled}
		h.Lanes.Deployed = Lane{Present: true, Enabled: enabled}
	}
	if d := raw.Dependabot; d != nil {
		h.Lanes.DepsLand = Lane{Present: true, Enabled: or(d.Enabled, "dependabot/enabled")}
	}
	return h
}

// proseFile is the path of a prose file in dir, or empty when it is absent.
func proseFile(dir, name string) string {
	path := filepath.Join(dir, name)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

// ParseRepo reads the owner and name out of a GitHub remote URL, in any of
// the shapes git accepts (https, scp-like, ssh://), or out of a bare
// "owner/name".
func ParseRepo(remote string) (owner, name string, err error) {
	s := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(remote), "/"), ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "/"); j >= 0 {
			s = s[j+1:] // past the host
		} else {
			s = ""
		}
	} else if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:] // git@host:owner/name
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return "", "", fmt.Errorf("cannot derive the repo from the remote %q", remote)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}
