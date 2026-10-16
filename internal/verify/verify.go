// Package verify drives the Target Project's Environment provider (ADR 0003):
// down before the first up, the one retry, the KEY=value block, the smoke
// with the block exported, and down on every way out. The result is a fact
// Go records; no agent judges it. It drives the smoke before a pull request
// opens, and boots a Verification round's environment from the PR head and
// works out which Surfaces the PR touched.
package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/state"
)

// ErrBootstrap is a Target Project that declares no hermetic tier: the
// Bootstrap state. Nothing is booted, and it is not a failure.
var ErrBootstrap = errors.New("the Harness config declares no hermetic tier (Bootstrap state): no Environment provider to run")

// The exit codes of the contract.
const (
	upHealthy        = 0
	upPrerequisite   = 3
	upBootFailed     = 4
	smokePass        = 0
	smokeFail        = 1
	smokeCouldNotRun = 2
)

// callTimeout bounds one provider call. Readiness is the provider's to wait
// for, but a provider that never returns must not hold a Fire for ever.
const callTimeout = time.Hour

// stderrLines is how much of a call's stderr a message quotes.
const stderrLines = 3

// logLines and logBytes bound the end of a failed smoke's output kept for the
// Fix: enough to see why, little enough for a Step's input.
const (
	logLines = 40
	logBytes = 4096
)

// Status is how the smoke ended.
type Status string

const (
	Pass    Status = "PASS"
	Fail    Status = "FAIL"
	Skipped Status = "SKIPPED"
)

// Result is one drive of the provider.
type Result struct {
	Status Status `json:"status"`
	// Detail is the smoke's own words for a pass or a fail, or why it was
	// skipped.
	Detail string `json:"detail"`
	// Violation is set when the provider broke its contract; Detail is
	// then the verdict `provider-check` gives.
	Violation bool `json:"violation"`
	// Log is the end of a failed smoke's output, for the Fix.
	Log string `json:"log,omitempty"`
	// Teardown is why the last down failed, when everything else did not.
	Teardown string `json:"teardown,omitempty"`
	// Bootstrap is set when no provider was run because the Target Project
	// has none: a human verifies the PR.
	Bootstrap bool `json:"bootstrap,omitempty"`
}

// Bootstrap is the result for a Target Project in the Bootstrap state.
func Bootstrap() Result {
	return Result{Status: Skipped, Detail: "Bootstrap state: no Environment provider, " + state.LabelVerifyHuman + " applied", Bootstrap: true}
}

// Trailer is the result as the pull request's Smoke section states it.
func (r Result) Trailer() string { return fmt.Sprintf("smoke: %s — %s", r.Status, r.Detail) }

// Provider is a Target Project's Environment provider, ready to drive.
type Provider struct {
	// Path is the executable; Dir the checkout it runs in.
	Path, Dir string
	// Smoke is whether the hermetic tier runs the smoke.
	Smoke bool
}

// Resolve finds the provider the hermetic tier declares in the checkout in
// dir: ErrBootstrap when there is none.
func Resolve(dir string, hermetic *config.Hermetic) (Provider, error) {
	if hermetic == nil {
		return Provider{}, ErrBootstrap
	}
	path := hermetic.Command
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return Provider{}, fmt.Errorf("the hermetic command %s is not an executable file under %s", hermetic.Command, dir)
	}
	return Provider{Path: path, Dir: dir, Smoke: hermetic.Smoke}, nil
}

// call is one run of the provider.
type call struct {
	stdout, stderr string
	exit           int
}

func (p Provider) run(ctx context.Context, env []string, args ...string) call {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Path, args...)
	cmd.Dir, cmd.Env = p.Dir, env
	// A provider's children (a server, a compose run) die with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	c := call{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		c.exit = exit.ExitCode()
	case err != nil:
		c.exit, c.stderr = -1, strings.TrimSpace(c.stderr+" "+err.Error())
	}
	return c
}

// tail is the end of a call's stderr, on one line.
func tail(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if strings.TrimSpace(text) == "" {
		return "no stderr"
	}
	lines = lines[max(0, len(lines)-stderrLines):]
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

var (
	blockKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	passLine = regexp.MustCompile(`^smoke: PASS \((.*)\)$`)
	failLine = regexp.MustCompile(`^smoke: FAIL \((.*)\)$`)
)

// Block reads up's stdout as the contract's KEY=value block, in order.
func Block(stdout string) ([][2]string, error) {
	var block [][2]string
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("up printed a line that is not KEY=value: %s", line)
		}
		if !blockKey.MatchString(key) {
			return nil, fmt.Errorf("up printed a key that is not an uppercase shell identifier: %s", key)
		}
		block = append(block, [2]string{key, value})
	}
	if len(block) == 0 {
		return nil, errors.New("up printed no keys")
	}
	return block, nil
}

// missingURLKey names the first declared Surface whose url_key the block does
// not carry, by name: empty when it carries them all.
func missingURLKey(block [][2]string, surfaces map[string]config.Surface) string {
	carried := map[string]bool{}
	for _, kv := range block {
		carried[kv[0]] = true
	}
	names := make([]string, 0, len(surfaces))
	for name := range surfaces {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if key := surfaces[name].URLKey; key != "" && !carried[key] {
			return fmt.Sprintf("surface %s declares url_key %s, which the up block does not carry", name, key)
		}
	}
	return ""
}

// lastLine is the last non-empty line of a call's stdout.
func lastLine(stdout string) string {
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	return lines[len(lines)-1]
}

// Run drives the provider for pr, the number the environment is kept apart
// by, and returns how the smoke went. The environment is torn down on every
// way out once anything may have booted. env is the environment the provider
// runs in, which the caller has cleared of credentials.
func (p Provider) Run(ctx context.Context, pr int, surfaces map[string]config.Surface, env []string) Result {
	n := strconv.Itoa(pr)
	down := func() call { return p.run(context.WithoutCancel(ctx), env, "down", "--pr", n) }
	// A killed Fire can have left an environment behind.
	if c := down(); c.exit != 0 {
		return Violation(fmt.Sprintf("down before the first up exited %d, want 0 (down is idempotent): %s", c.exit, tail(c.stderr)))
	}
	result := p.boot(ctx, n, surfaces, env, down)
	if c := down(); c.exit != 0 {
		result.Teardown = fmt.Sprintf("down after up exited %d, want 0 (down is idempotent)", c.exit)
	}
	return result
}

// boot is the environment, then the smoke.
func (p Provider) boot(ctx context.Context, n string, surfaces map[string]config.Surface, env []string, down func() call) Result {
	block, failed := p.start(ctx, n, surfaces, env, down)
	if failed != nil {
		return *failed
	}
	if !p.Smoke {
		return skipped("the hermetic tier does not run the smoke (verification.hermetic.smoke is false); the environment booted")
	}
	return p.smoke(ctx, block, env)
}

// start is up, with its one retry, then the block it printed, checked for
// its grammar and for every declared Surface's url_key. A boot that did not
// give a usable block is the Result saying why.
func (p Provider) start(ctx context.Context, n string, surfaces map[string]config.Surface, env []string, down func() call) ([][2]string, *Result) {
	fail := func(r Result) ([][2]string, *Result) { return nil, &r }
	up := p.run(ctx, env, "up", "--pr", n)
	if up.exit == upBootFailed {
		// The one retry, with a down between: a missing prerequisite (3)
		// will still be missing a second later, so only a failed boot gets it.
		if c := down(); c.exit != 0 {
			return fail(Violation(fmt.Sprintf("down between the one retry exited %d, want 0 (down is idempotent): %s", c.exit, tail(c.stderr))))
		}
		if up = p.run(ctx, env, "up", "--pr", n); up.exit == upBootFailed {
			return fail(skipped(fmt.Sprintf("up exited 4 (boot failed) on both attempts: %s", tail(up.stderr))))
		}
	}
	switch up.exit {
	case upHealthy:
	case upPrerequisite:
		return fail(skipped(fmt.Sprintf("up exited 3 (prerequisite missing): %s", tail(up.stderr))))
	default:
		return fail(Violation(fmt.Sprintf("up exited %d, want 0 healthy, 3 prerequisite missing or 4 boot failed: %s", up.exit, tail(up.stderr))))
	}
	block, err := Block(up.stdout)
	if err != nil {
		return fail(Violation(err.Error()))
	}
	if missing := missingURLKey(block, surfaces); missing != "" {
		return fail(Violation(missing))
	}
	return block, nil
}

// smoke runs the smoke with the block exported.
func (p Provider) smoke(ctx context.Context, block [][2]string, env []string) Result {
	smokeEnv := append([]string{}, env...)
	for _, kv := range block {
		smokeEnv = append(smokeEnv, kv[0]+"="+kv[1])
	}
	smoke := p.run(ctx, smokeEnv, "smoke")
	line := lastLine(smoke.stdout)
	switch smoke.exit {
	case smokePass:
		if m := passLine.FindStringSubmatch(line); m != nil {
			return Result{Status: Pass, Detail: m[1]}
		}
		return Violation(fmt.Sprintf("smoke exited 0 but its last stdout line is not 'smoke: PASS (…)': %s", line))
	case smokeFail:
		r := Result{Status: Fail, Log: logTail(smoke.stdout + smoke.stderr)}
		if m := failLine.FindStringSubmatch(line); m != nil {
			r.Detail = m[1]
		} else {
			r.Detail, r.Violation = fmt.Sprintf("smoke exited 1 but its last stdout line is not 'smoke: FAIL (…)': %s", line), true
		}
		return r
	case smokeCouldNotRun:
		return skipped(fmt.Sprintf("smoke exited 2 (could not run): %s", tail(smoke.stderr)))
	}
	return Violation(fmt.Sprintf("smoke exited %d, want 0 pass, 1 fail or 2 could not run", smoke.exit))
}

func skipped(detail string) Result { return Result{Status: Skipped, Detail: detail} }

// Violation is a provider that broke its contract, detail the verdict. The
// smoke did not run as the contract says, so it is skipped, never invented
// as a pass.
func Violation(detail string) Result { return Result{Status: Skipped, Detail: detail, Violation: true} }

// logTail is the end of a failed smoke's output, its lines kept.
func logTail(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	text = strings.Join(lines[max(0, len(lines)-logLines):], "\n")
	if len(text) > logBytes {
		text = text[len(text)-logBytes:]
	}
	return text
}
