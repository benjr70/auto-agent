// Package doctor tells the operator what this machine is missing before a
// Fire finds out. Each Finding is fatal or a warning, says what is missing,
// and gives the command that fixes it. Checking changes nothing: it reads
// GitHub, asks the claude CLI, and looks at the machine.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/verify"
)

// MinClaude is the oldest claude CLI the Daemon runs its Steps through.
const MinClaude = "2.1.205"

// probeTimeout bounds each command doctor runs.
const probeTimeout = 15 * time.Second

// Severity is how much a Finding matters.
type Severity string

const (
	OK Severity = "ok"
	// Warning: the Daemon runs, worse than it could.
	Warning Severity = "WARNING"
	// Fatal: `run` refuses to start.
	Fatal Severity = "FATAL"
)

// Finding is the result of one check.
type Finding struct {
	Check    string
	Severity Severity
	// Detail is what was found, or what is missing.
	Detail string
	// Fix is the command that fixes it; empty when the check is OK.
	Fix string
}

// String is the Finding as one line.
func (f Finding) String() string {
	if f.Severity == OK {
		return fmt.Sprintf("%s: ok — %s", f.Check, oneLine(f.Detail))
	}
	return fmt.Sprintf("%s: %s — %s — fix: %s", f.Check, f.Severity, oneLine(f.Detail), f.Fix)
}

// oneLine joins the lines of a multi-line error, such as a schema
// validation's list of problems.
func oneLine(s string) string {
	var out string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		switch {
		case out == "":
			out = line
		case strings.HasSuffix(out, ":"):
			out += " " + line
		default:
			out += "; " + line
		}
	}
	return out
}

// Report is every Finding of one check of the machine, in order.
type Report []Finding

// Fatal is whether any Finding stops `run` from starting.
func (r Report) Fatal() bool {
	return slices.ContainsFunc(r, func(f Finding) bool { return f.Severity == Fatal })
}

// Machine is everything the checks reach outside themselves through, so
// that a test can stand in for each. A nil func is the real one.
type Machine struct {
	// TargetDir is the Target Project checkout.
	TargetDir string
	// HarnessOptional is for a Fire, which reads the Harness config from
	// its own clone of the default branch: a TargetDir with none is then
	// checked against the default branch's, read from GitHub.
	HarnessOptional bool
	// Repo is "owner/name"; empty means the origin remote of TargetDir.
	Repo      string
	OriginURL func(dir string) (string, error)
	LookupEnv func(string) (string, bool)
	// GHAuthToken asks the gh CLI for its token (github.GHAuthToken).
	GHAuthToken func() (string, error)
	// GitHubAPI is the API root; empty means api.github.com.
	GitHubAPI string
	UserAgent string
	// ClaudeVersion is what `claude --version` prints.
	ClaudeVersion func(context.Context) ([]byte, error)
	// AuthStatus is what `claude auth status --json` prints.
	AuthStatus func(context.Context) ([]byte, error)
	// SurfaceHost is the Host the Surfaces run on; its Env, when nil, is
	// read through LookupEnv.
	SurfaceHost verify.Host
	// LookPath finds an executable on the PATH, as exec.LookPath does.
	LookPath func(string) (string, error)
	// Docker answers whether the Docker daemon is reachable.
	Docker func(context.Context) error
}

// ClaudeVersion asks the claude CLI its version.
func ClaudeVersion(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "claude", "--version").Output()
}

// Docker asks the Docker daemon whether it answers.
func Docker(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput()
	if err != nil {
		if line := lastLine(string(out)); line != "" {
			return fmt.Errorf("%w: %s", err, line)
		}
	}
	return err
}

// harnessLabels are the labels the Daemon and the skills that write its
// tickets use.
var harnessLabels = []string{
	state.LabelAFK, "HITL", "spec", state.LabelInProgress, state.LabelDone, state.LabelFailed,
	state.LabelChecksFailed, state.LabelRevise, state.LabelReviseFailed, state.LabelRuling,
	state.LabelRebaseFailed, state.LabelPaused, state.LabelDepsFailed, state.LabelVerifyHuman,
	state.LabelNeedsHuman, "wayfinder:map", "wayfinder:grilling", "wayfinder:prototype",
	"wayfinder:research", "wayfinder:task",
}

// Check runs every check and returns what it found, one Finding per check.
func Check(ctx context.Context, m Machine) Report {
	m = m.withDefaults()
	var r Report
	repo, named := m.repo(&r)
	if !named {
		return r
	}
	host, hostErr := config.LoadHost(repo, m.LookupEnv)
	harness, remote, harnessErr := m.harness(ctx, repo, host, hostErr)
	m.github(ctx, &r, repo, harness, harnessErr == nil)
	m.claude(ctx, &r, m.declaredMode(repo, host, hostErr))
	r = append(r, harnessFinding(harness, harnessErr, remote))
	r = append(r, m.host(repo, host, hostErr))
	if harnessErr == nil {
		m.surfaces(ctx, &r, harness, remote)
	}
	return r
}

// harness is the Harness config the checks are of: the one in TargetDir,
// or, for a Fire whose TargetDir has none, the default branch's on GitHub,
// which is what the Fire reads from its clone. remote says it came from
// GitHub.
func (m Machine) harness(ctx context.Context, repo config.Repo, host config.Host, hostErr error) (harness config.Harness, remote bool, err error) {
	file := filepath.Join(m.TargetDir, config.HarnessDirName, config.HarnessFileName)
	if _, statErr := os.Stat(file); !m.HarnessOptional || !errors.Is(statErr, os.ErrNotExist) {
		harness, err = config.LoadHarness(m.TargetDir, "", repo)
		return harness, false, err
	}
	// Its prose and commands resolve beside the Fire's clone, which need
	// not exist yet.
	configDir, _ := filepath.Abs(filepath.Join(m.TargetDir, config.HarnessDirName))
	if hostErr == nil {
		configDir = filepath.Join(host.StateDir, "clone", config.HarnessDirName)
	}
	token, err := github.ResolveToken(m.LookupEnv, m.GHAuthToken)
	if err != nil {
		return config.Harness{}, true, fmt.Errorf("none in %s, and the default branch's cannot be read from GitHub: %w", m.TargetDir, err)
	}
	client := github.New(github.Options{BaseURL: m.GitHubAPI, Token: token, UserAgent: m.UserAgent})
	path := config.HarnessDirName + "/" + config.HarnessFileName
	data, err := client.FileContent(ctx, repo.Owner, repo.Name, path)
	if github.IsNotFound(err) {
		return config.Harness{}, true, fmt.Errorf("none in %s, and the default branch of %s has no %s either", m.TargetDir, repo.Slug(), path)
	}
	if err != nil {
		return config.Harness{}, true, fmt.Errorf("none in %s, and the default branch's cannot be read from GitHub: %w", m.TargetDir, err)
	}
	harness, err = config.ParseHarness(data, repo.Slug()+"@default-branch:"+path, configDir, repo)
	return harness, true, err
}

// declaredMode is the auth mode the Host config declares, as the Fire's
// Usage sensor reads it: the Host config file, overridden by the
// environment. A Host config that does not load (its own finding) leaves
// the environment's.
func (m Machine) declaredMode(repo config.Repo, host config.Host, hostErr error) declared {
	where := "the Host config (" + config.HostConfigPath(repo, m.LookupEnv) + ")"
	if hostErr != nil {
		return declared{mode: m.env("CLAUDE_AUTH_MODE"), where: where}
	}
	return declared{mode: host.AuthMode, where: where}
}

// declared is an auth mode, and where it is set.
type declared struct{ mode, where string }

func (m Machine) withDefaults() Machine {
	if m.ClaudeVersion == nil {
		m.ClaudeVersion = ClaudeVersion
	}
	if m.AuthStatus == nil {
		m.AuthStatus = budget.ClaudeAuthStatus
	}
	if m.LookPath == nil {
		m.LookPath = exec.LookPath
	}
	if m.Docker == nil {
		m.Docker = Docker
	}
	if m.GHAuthToken == nil {
		m.GHAuthToken = github.GHAuthToken
	}
	if m.SurfaceHost.Env == nil {
		for _, key := range []string{"DISPLAY", "AUTO_AGENT_ELECTRON_SANDBOX"} {
			if value, ok := m.LookupEnv(key); ok {
				m.SurfaceHost.Env = append(m.SurfaceHost.Env, key+"="+value)
			}
		}
	}
	return m
}

func (m Machine) env(key string) string {
	value, _ := m.LookupEnv(key)
	return value
}

func ok(check, detail string) Finding { return Finding{Check: check, Severity: OK, Detail: detail} }

func warn(check, detail, fix string) Finding {
	return Finding{Check: check, Severity: Warning, Detail: detail, Fix: fix}
}

func fatal(check, detail, fix string) Finding {
	return Finding{Check: check, Severity: Fatal, Detail: detail, Fix: fix}
}

// repo is the Target Project, from --repo or the checkout's origin remote.
func (m Machine) repo(r *Report) (config.Repo, bool) {
	slug := m.Repo
	if slug == "" {
		origin, err := m.OriginURL(m.TargetDir)
		if err != nil {
			*r = append(*r, fatal("repo", fmt.Sprintf("the Target Project cannot be told: %v", err),
				"run it in the Target Project's checkout, or name it: --repo owner/name"))
			return config.Repo{}, false
		}
		slug = origin
	}
	owner, name, err := config.ParseRepo(slug)
	if err != nil {
		*r = append(*r, fatal("repo", err.Error(), "name the Target Project: --repo owner/name"))
		return config.Repo{}, false
	}
	return config.Repo{Owner: owner, Name: name}, true
}

// github checks the token: found, readable, allowed to read and write the
// Target Project, scoped as the Daemon needs, and whose it is.
func (m Machine) github(ctx context.Context, r *Report, repo config.Repo, harness config.Harness, harnessOK bool) {
	const loginFix = "export GH_TOKEN=<the machine user's token>, or log gh in as the machine user: gh auth login"
	token, err := github.ResolveToken(m.LookupEnv, m.GHAuthToken)
	if err != nil {
		*r = append(*r, fatal("github token", err.Error(), loginFix))
		return
	}
	*r = append(*r, ok("github token", "found in "+tokenSource(m)))
	client := github.New(github.Options{BaseURL: m.GitHubAPI, Token: token, UserAgent: m.UserAgent})
	user, err := client.User(ctx)
	if err != nil {
		*r = append(*r, fatal("github login", fmt.Sprintf("the token cannot be read: %v", err), loginFix))
		return
	}
	*r = append(*r, ok("github login", "the token is "+user.Login))
	accessFix := fmt.Sprintf("give %s write access: gh api -X PUT repos/%s/collaborators/%s -f permission=maintain (as an admin of the repo)", user.Login, repo.Slug(), user.Login)
	repository, err := client.Repository(ctx, repo.Owner, repo.Name)
	switch {
	case err != nil:
		*r = append(*r, fatal("github repo", fmt.Sprintf("%s cannot read %s: %v", user.Login, repo.Slug(), err), accessFix))
		return
	case repository.Permissions == nil:
		*r = append(*r, warn("github repo", fmt.Sprintf("%s can read %s, and GitHub did not say whether it can write", user.Login, repo.Slug()),
			fmt.Sprintf("check %s's role on the repo: gh api repos/%s --jq .permissions", user.Login, repo.Slug())))
	case !repository.Permissions.Push:
		*r = append(*r, fatal("github repo", fmt.Sprintf("%s can read %s but not write to it", user.Login, repo.Slug()), accessFix))
	default:
		*r = append(*r, ok("github repo", fmt.Sprintf("%s can read and write %s", user.Login, repo.Slug())))
	}
	if current, name, err := config.ParseRepo(repository.FullName); err == nil {
		repo.Owner, repo.Name = current, name
	}
	*r = append(*r, scopes(user, harnessOK && harness.Pick.Shape == config.PickProject))
	if strings.EqualFold(user.Login, repo.Owner) {
		*r = append(*r, warn("github owner", fmt.Sprintf("the token is the repo owner %s's, so: no second-party review (the Daemon's PRs are the owner's own); "+
			"a ticket the owner assigned to themselves gets picked; a PR the owner made by hand on an agent branch (feat/issue-<N>, research/<slug>) is treated as an Agent PR; "+
			"and no notifications reach the owner of what the Daemon does", user.Login),
			"run the Daemon as a machine user of its own: export GH_TOKEN=<the machine user's token>"))
	} else {
		*r = append(*r, ok("github owner", fmt.Sprintf("%s is not the repo owner", user.Login)))
	}
	*r = append(*r, labels(ctx, client, repo))
}

func tokenSource(m Machine) string {
	if strings.TrimSpace(m.env("GH_TOKEN")) != "" {
		return "GH_TOKEN"
	}
	return "`gh auth token`"
}

// scopes checks a classic token has the scopes the Daemon needs; a
// fine-grained one names none to check.
func scopes(user github.User, project bool) Finding {
	if !user.Classic {
		return ok("github scopes", "a fine-grained or app token: no scopes to check")
	}
	need := []string{"repo"}
	if project {
		need = append(need, "read:project")
	}
	var missing []string
	for _, scope := range need {
		has := slices.Contains(user.Scopes, scope) || (scope == "read:project" && slices.Contains(user.Scopes, "project"))
		if !has {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return fatal("github scopes", fmt.Sprintf("the classic token lacks the %s scope (it has: %s)", strings.Join(missing, ", "), strings.Join(user.Scopes, ", ")),
			"gh auth refresh -s "+strings.Join(missing, ",")+", or make a classic token with "+strings.Join(need, ", ")+" and export GH_TOKEN")
	}
	return ok("github scopes", "the classic token has "+strings.Join(need, ", "))
}

func labels(ctx context.Context, client *github.Client, repo config.Repo) Finding {
	have, err := client.Labels(ctx, repo.Owner, repo.Name)
	if err != nil {
		return warn("labels", fmt.Sprintf("the labels of %s cannot be read: %v", repo.Slug(), err), "gh label list --repo "+repo.Slug())
	}
	var missing []string
	for _, label := range harnessLabels {
		if !slices.Contains(have, label) {
			missing = append(missing, label)
		}
	}
	if len(missing) > 0 {
		return warn("labels", fmt.Sprintf("%s lacks the harness labels %s (run creates them)", repo.Slug(), strings.Join(missing, ", ")),
			fmt.Sprintf("gh label create <label> --repo %s, for each", repo.Slug()))
	}
	return ok("labels", fmt.Sprintf("all %d harness labels exist", len(harnessLabels)))
}

// claude checks the claude CLI: on the PATH, new enough, logged in, and
// logged in the way the Host config says.
func (m Machine) claude(ctx context.Context, r *Report, declared declared) {
	const installFix = "npm install -g @anthropic-ai/claude-code"
	out, err := m.ClaudeVersion(ctx)
	if errors.Is(err, exec.ErrNotFound) {
		*r = append(*r, fatal("claude cli", "claude is not on the PATH", installFix))
		return
	}
	if err != nil {
		*r = append(*r, fatal("claude cli", fmt.Sprintf("claude --version failed: %v", err), installFix))
		return
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		*r = append(*r, fatal("claude cli", "claude --version printed no version", installFix))
		return
	}
	version := fields[0]
	if !atLeast(version, MinClaude) {
		*r = append(*r, fatal("claude cli", fmt.Sprintf("claude is %s; the Daemon needs %s or later", version, MinClaude), "claude update"))
		return
	}
	*r = append(*r, ok("claude cli", "claude "+version))

	raw, statusErr := m.AuthStatus(ctx)
	sensor := budget.Sensor{DeclaredMode: declared.mode, LookupEnv: m.LookupEnv}
	_, alive, reason := sensor.Auth(raw, statusErr)
	if !alive {
		fix := "claude auth login"
		if declared.mode == budget.ModeSetupToken {
			fix = "claude setup-token, then export CLAUDE_CODE_OAUTH_TOKEN=<the token>"
		}
		*r = append(*r, fatal("claude login", "claude is not logged in", fix))
		return
	}
	switch {
	case declared.mode == "":
		*r = append(*r, warn("claude login", "claude is logged in, but CLAUDE_AUTH_MODE is not set, in the environment or in "+declared.where+", and the Usage sensor refuses to guess it",
			"set CLAUDE_AUTH_MODE=login, setup-token or api-key in "+declared.where))
	case reason != "":
		*r = append(*r, fatal("claude login", reason, "set CLAUDE_AUTH_MODE in "+declared.where+" to how claude is logged in, or log claude in that way"))
	case declared.mode == budget.ModeAPIKey:
		*r = append(*r, fatal("claude login", "api-key mode refuses to start until spend pacing exists",
			"log claude in with a subscription and set CLAUDE_AUTH_MODE=login or setup-token in "+declared.where))
	default:
		*r = append(*r, ok("claude login", "logged in, as CLAUDE_AUTH_MODE="+declared.mode+" says"))
	}
}

// atLeast is whether the dotted version v is min or later.
func atLeast(v, min string) bool {
	have, want := strings.Split(strings.TrimPrefix(v, "v"), "."), strings.Split(min, ".")
	for i := range want {
		if i >= len(have) {
			return false
		}
		h, err := strconv.Atoi(have[i])
		if err != nil {
			return false
		}
		w, _ := strconv.Atoi(want[i])
		if h != w {
			return h > w
		}
	}
	return true
}

func harnessFinding(harness config.Harness, err error, remote bool) Finding {
	file := filepath.Join(config.HarnessDirName, config.HarnessFileName)
	if err != nil {
		where := "in the Target Project"
		if remote {
			where = "on the Target Project's default branch"
		}
		return fatal("harness config", err.Error(), "correct "+file+" "+where+", then: auto-agent doctor")
	}
	valid := "valid"
	if remote {
		valid = "valid, as the default branch on GitHub declares it (the one the Fire reads)"
	}
	if harness.Verification.Hermetic == nil {
		return ok("harness config", valid+"; Bootstrap state: no Environment provider yet, so a human verifies each PR")
	}
	return ok("harness config", valid)
}

// host checks the Host config loads and its State dir can be written.
func (m Machine) host(repo config.Repo, host config.Host, err error) Finding {
	if err != nil {
		return fatal("host config", err.Error(), "correct "+config.HostConfigPath(repo, m.LookupEnv)+", or the environment variable it names")
	}
	if err := writable(host.StateDir); err != nil {
		return fatal("host config", fmt.Sprintf("the State dir %s cannot be written: %v", host.StateDir, err),
			fmt.Sprintf("sudo install -d -o \"$USER\" %s", host.StateDir))
	}
	return ok("host config", "the State dir "+host.StateDir+" is writable")
}

// writable is whether dir, or the nearest ancestor that exists when it does
// not yet, can be written. Nothing is created.
func writable(dir string) error {
	for at := dir; ; at = filepath.Dir(at) {
		info, err := os.Stat(at)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", at)
			}
			if err := syscall.Access(at, 0o2); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			return nil
		}
		if at == filepath.Dir(at) {
			return err
		}
	}
}

// surfaces checks what each declared Surface needs of the machine, what
// host.docker asks for, and the Environment provider.
// A remote Harness config's relative provider is a file of the repo the
// Fire checks out, not of this machine, so it is not looked for here.
func (m Machine) surfaces(ctx context.Context, r *Report, harness config.Harness, remote bool) {
	var display string
	var displayErr error
	probed := false
	for _, name := range slices.Sorted(maps.Keys(harness.Surfaces)) {
		surface := harness.Surfaces[name]
		check := "surface " + name
		switch surface.Kind {
		case verify.KindBrowser, verify.KindElectron:
		default:
			*r = append(*r, ok(check, fmt.Sprintf("a %s Surface needs nothing of this machine", surface.Kind)))
			continue
		}
		if !probed {
			display, displayErr = m.SurfaceHost.Display(ctx)
			probed = true
		}
		if displayErr != nil {
			*r = append(*r, fatal(check+": display", fmt.Sprintf("the %s Surface needs a display: %v", surface.Kind, displayErr),
				"start one and name it in the Host's environment: Xvfb :99 -screen 0 1920x1080x24 & export DISPLAY=:99"))
		} else {
			*r = append(*r, ok(check+": display", "DISPLAY="+display))
		}
		if surface.Kind == verify.KindBrowser {
			*r = append(*r, m.chromium(check))
		} else {
			*r = append(*r, m.sandbox(check, surface, harness.TargetDir()))
		}
	}
	if harness.Host.Docker {
		if err := m.Docker(ctx); err != nil {
			*r = append(*r, fatal("docker", fmt.Sprintf("host.docker is set but Docker is not reachable: %v", err),
				"sudo systemctl enable --now docker && sudo usermod -aG docker \"$USER\" (then log in again)"))
		} else {
			*r = append(*r, ok("docker", "reachable"))
		}
	}
	if hermetic := harness.Verification.Hermetic; hermetic != nil && remote && !filepath.IsAbs(hermetic.Command) {
		*r = append(*r, ok("provider", hermetic.Command+" is the repo's own: the Fire checks it in its clone"))
	} else if hermetic != nil {
		if provider, err := verify.Resolve(harness.TargetDir(), hermetic); err != nil {
			*r = append(*r, fatal("provider", err.Error(), "chmod +x "+filepath.Join(harness.TargetDir(), hermetic.Command)))
		} else {
			*r = append(*r, ok("provider", provider.Path+" can be executed"))
		}
	}
}

// browsers are the names a Chrome or Chromium goes by on the PATH.
var browsers = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"}

func (m Machine) chromium(check string) Finding {
	for _, name := range browsers {
		if path, err := m.LookPath(name); err == nil {
			return ok(check+": chromium", path)
		}
	}
	return fatal(check+": chromium", "the browser Surface needs Chrome or Chromium, and none is on the PATH ("+strings.Join(browsers, ", ")+")",
		"npx playwright install --with-deps chrome")
}

func (m Machine) sandbox(check string, surface config.Surface, targetDir string) Finding {
	bin := surface.Launcher
	if bin != "" && !filepath.IsAbs(bin) {
		bin = filepath.Join(targetDir, bin)
	}
	if !m.SurfaceHost.SandboxMode(bin).Degraded() {
		return ok(check+": sandbox", "the Electron sandbox can run")
	}
	return warn(check+": sandbox", fmt.Sprintf("AppArmor blocks the Electron sandbox: no profile grants user namespaces to %s, so the app runs with ELECTRON_DISABLE_SANDBOX=1 and each round says DEGRADED", or(bin, "the app")),
		fmt.Sprintf("printf 'abi <abi/4.0>,\\ninclude <tunables/global>\\nprofile \"%s\" flags=(unconfined) {\\n  userns,\\n}\\n' | sudo tee /etc/apparmor.d/auto-agent-electron && sudo apparmor_parser -r /etc/apparmor.d/auto-agent-electron", or(bin, "<the app>")))
}

func or(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
