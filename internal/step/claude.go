package step

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Runner runs a Step. It is the one interface the claude call sits behind:
// everything above it is tested with a fake.
type Runner interface {
	Run(ctx context.Context, spec Spec, call Call) Run
}

// Claude runs Steps as headless Claude Code calls.
type Claude struct {
	// Binary is the claude executable; empty means "claude" on PATH.
	Binary string
	// Environ is the environment the Daemon runs in; nil means os.Environ.
	// The Step gets it less every GitHub credential.
	Environ func() []string
}

// killGrace is how long the reader waits after the process group is killed
// before giving up on anything still holding the pipes.
const killGrace = 5 * time.Second

// credentialEnv are the variables through which a child could reach GitHub
// as the Daemon. A Step never writes to GitHub or pushes: Go does.
var credentialEnv = map[string]bool{
	"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true,
	"GH_CONFIG_DIR": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true, "SSH_AUTH_SOCK": true,
	"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_NOSYSTEM": true, "GIT_TERMINAL_PROMPT": true,
}

// stepEnv is environ with no GitHub credential in it: no token, gh pointed
// at an empty config dir, and git cut off from the user's and the system's
// config, where a credential helper would live. Git still works locally.
func stepEnv(environ []string, emptyGHConfig string) []string {
	var env []string
	for _, kv := range environ {
		if key, _, _ := strings.Cut(kv, "="); !credentialEnv[key] {
			env = append(env, kv)
		}
	}
	return append(env, "GH_CONFIG_DIR="+emptyGHConfig, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

// unpack writes the Step's prompt as a one-skill plugin under dir.
func unpack(spec Spec, dir string) error {
	manifest := fmt.Sprintf(`{"name": %q, "version": "0.0.0", "description": "The auto-agent Daemon's Step prompts."}`+"\n", pluginName)
	skill := fmt.Sprintf("---\nname: %s\ndescription: The %s Step of the auto-agent Daemon.\n---\n\n%s", spec.Name, spec.Name, spec.Prompt)
	files := map[string]string{
		filepath.Join(".claude-plugin", "plugin.json"): manifest,
		filepath.Join("skills", spec.Name, "SKILL.md"): skill,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Run makes one headless call for the Step and classifies how it ended. It
// never returns an error: every way the call can go wrong is an Outcome.
func (c *Claude) Run(ctx context.Context, spec Spec, call Call) Run {
	failed := func(format string, args ...any) Run {
		return Run{Outcome: Failed, Detail: fmt.Sprintf(format, args...)}
	}
	if err := spec.validate(); err != nil {
		return failed("%v", err)
	}
	if call.Dir == "" {
		return failed("the call names no checkout")
	}
	if call.Model == "" {
		return failed("the call names no model")
	}
	input, err := json.Marshal(call.Input)
	if err != nil {
		return failed("the Step's input cannot be sent: %v", err)
	}
	scratch, err := os.MkdirTemp("", "auto-agent-step-")
	if err != nil {
		return failed("%v", err)
	}
	defer os.RemoveAll(scratch)
	pluginDir, ghConfig := filepath.Join(scratch, "plugin"), filepath.Join(scratch, "gh")
	if err := os.MkdirAll(ghConfig, 0o700); err != nil {
		return failed("%v", err)
	}
	if err := unpack(spec, pluginDir); err != nil {
		return failed("unpacking the Step's prompt: %v", err)
	}

	binary := c.Binary
	if binary == "" {
		binary = "claude"
	}
	environ := os.Environ
	if c.Environ != nil {
		environ = c.Environ
	}
	// stopped is why the runner itself ended the call early, if it did.
	var stopped error
	callCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	timer := time.AfterFunc(spec.Timeout, func() { stop(context.DeadlineExceeded) })
	defer timer.Stop()

	cmd := exec.CommandContext(callCtx, binary,
		"-p", "/"+pluginName+":"+spec.Name+" "+string(input),
		"--output-format", "stream-json", "--verbose",
		"--json-schema", spec.Schema,
		"--plugin-dir", pluginDir,
		"--permission-mode", "dontAsk",
		"--tools", strings.Join(spec.Tools, ","),
		"--allowedTools", strings.Join(spec.Allowed, ","),
		// No --mcp-config is given, so this keeps every MCP server out: the
		// account's connectors (mail, calendar) load into a headless call
		// otherwise.
		"--strict-mcp-config",
		// The Target Project's CLAUDE.md and settings, never the user's.
		"--setting-sources", "project",
		"--model", call.Model,
	)
	cmd.Dir = call.Dir
	cmd.Env = stepEnv(environ(), ghConfig)
	// Kill the whole process group. Killing claude alone leaves its children
	// holding stdout open, and the reader below would never return.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = killGrace
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failed("%v", err)
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return failed("starting %s: %v", binary, err)
	}
	run := Run{}
	var result *resultMessage
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 1<<20), 64<<20)
	for lines.Scan() {
		var message struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(lines.Bytes(), &message) != nil {
			continue // not a stream message
		}
		switch {
		case message.Type == "system" && message.Subtype == "init":
			var init initMessage
			if json.Unmarshal(lines.Bytes(), &init) != nil {
				continue
			}
			run.SessionID, run.Model, run.PermissionMode, run.Tools = init.SessionID, init.Model, init.PermissionMode, init.Tools
			if err := checkInit(spec, init); err != nil && stopped == nil {
				// Do not let a Step run with tools it should not have, or
				// without the ones it needs.
				stopped = err
				stop(err)
			}
		case message.Type == "rate_limit_event":
			run.RateLimits = append(run.RateLimits, append(json.RawMessage(nil), lines.Bytes()...))
		case message.Type == "result":
			var parsed resultMessage
			if json.Unmarshal(lines.Bytes(), &parsed) == nil {
				result = &parsed
			}
		}
	}
	cmd.Wait()
	run.Elapsed = time.Since(start).Round(time.Millisecond)
	run.ExitCode = cmd.ProcessState.ExitCode()
	if result != nil {
		run.NumTurns, run.CostUSD = result.NumTurns, result.TotalCostUSD
		run.RawUsage, run.PermissionDenials = result.Usage, result.PermissionDenials
		json.Unmarshal(result.Usage, &run.Usage)
	}

	switch cause := context.Cause(callCtx); {
	case stopped != nil:
		run.Outcome, run.Detail = Failed, stopped.Error()
	case ctx.Err() != nil:
		run.Outcome, run.Detail = Failed, "the call was cancelled: "+ctx.Err().Error()
	case errors.Is(cause, context.DeadlineExceeded):
		run.Outcome, run.Detail = TimedOut, "the call outran the Step's timeout of "+spec.Timeout.String()
	default:
		run.Outcome, run.Detail = classify(result, run.RateLimits, run.ExitCode, stderr.String())
		if run.Outcome == OK {
			run.Result = result.StructuredOutput
		}
	}
	return run
}
