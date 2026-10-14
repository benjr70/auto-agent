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

const (
	// killGrace is how long the reader waits after the process group is
	// killed before giving up on anything still holding the pipes.
	killGrace = 5 * time.Second
	// lingerGrace is how long a call may stay alive after its result
	// message before it is killed: the answer is in, the rest is a child
	// that did not exit.
	lingerGrace = 5 * time.Second
	// maxLine bounds one line of the stream.
	maxLine = 64 << 20
	// maxText bounds the stderr and stray output kept as evidence.
	maxText = 1 << 20
)

// errLingering ends a call that answered and then did not exit.
var errLingering = errors.New("the call did not exit after its result")

// capped is a buffer that keeps the first maxText bytes written to it and
// drops the rest, so a noisy child cannot grow the Daemon's memory.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxText - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// credentialEnv are the variables through which a child could reach GitHub
// as the Daemon. A Step never writes to GitHub or pushes: Go does.
var credentialEnv = map[string]bool{
	"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true,
	"GH_CONFIG_DIR": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true, "SSH_AUTH_SOCK": true,
	"GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_TERMINAL_PROMPT": true,
}

// CredentialFree is environ with no GitHub credential in it: no token, gh pointed
// at an empty config dir, git cut off from ssh and from the user's and the
// system's config, where a credential helper would live. Git still works
// locally. HOME is kept, because Claude's own login lives there; what a
// checkout's own .git/config holds is the Workspace's to keep clean.
func CredentialFree(environ []string, emptyGHConfig string) []string {
	var env []string
	for _, kv := range environ {
		// GIT_CONFIG_* also covers config passed through the environment
		// (GIT_CONFIG_COUNT and its keys), which can name a credential helper.
		if key, _, _ := strings.Cut(kv, "="); !credentialEnv[key] && !strings.HasPrefix(key, "GIT_CONFIG_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GH_CONFIG_DIR="+emptyGHConfig,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		// No git over ssh: a key in ~/.ssh needs no agent.
		"GIT_SSH_COMMAND=false",
	)
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
	// A resumed session already has the input; it is told what to do next.
	prompt, resume := "/"+pluginName+":"+spec.Name+" "+string(input), []string(nil)
	if call.Resume != "" {
		if strings.TrimSpace(call.Message) == "" {
			return failed("the call resumes a session with nothing to tell it")
		}
		prompt, resume = call.Message, []string{"--resume", call.Resume}
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
	// refusal is why the runner itself ended the call early and failed it,
	// if it did.
	var refusal error
	callCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	timer := time.AfterFunc(spec.Timeout, func() { stop(context.DeadlineExceeded) })
	defer timer.Stop()

	// The prompt goes in on stdin, not as an argument: an input can be a
	// whole issue body, and one argument is capped near 128 KB.
	cmd := exec.CommandContext(callCtx, binary, append([]string{
		"-p",
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
	}, resume...)...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = call.Dir
	cmd.Env = CredentialFree(environ(), ghConfig)
	// Kill the whole process group. Killing claude alone leaves its children
	// holding stdout open, and the reader below would never return.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = killGrace
	var stderr, stray capped
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
	// refuse fails the call and kills it: a Step must not carry on with
	// tools it should not have, or with a stream the runner cannot follow.
	refuse := func(err error) {
		if refusal == nil {
			refusal = err
			stop(err)
		}
	}
	initChecked := false
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 1<<20), maxLine)
	for lines.Scan() {
		var message struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(lines.Bytes(), &message) != nil {
			// Not a stream message. A dying claude prints its reason plainly.
			stray.Write(append(lines.Bytes(), '\n'))
			continue
		}
		if message.Error != "" {
			stray.WriteString(message.Error + "\n")
		}
		switch {
		case message.Type == "system" && message.Subtype == "init":
			if initChecked || result != nil {
				continue // only the call's own init, the first, is checked
			}
			var init initMessage
			if err := json.Unmarshal(lines.Bytes(), &init); err != nil {
				refuse(fmt.Errorf("the init message could not be read, so the Step's tools cannot be checked: %v", err))
				continue
			}
			run.SessionID, run.Model, run.PermissionMode, run.Tools = init.SessionID, init.Model, init.PermissionMode, init.Tools
			if err := checkInit(spec, init); err != nil {
				refuse(err)
				continue
			}
			initChecked = true
		case message.Type == "rate_limit_event":
			run.RateLimits = append(run.RateLimits, append(json.RawMessage(nil), lines.Bytes()...))
		case message.Type == "result":
			var parsed resultMessage
			if json.Unmarshal(lines.Bytes(), &parsed) == nil && result == nil {
				result = &parsed
				linger := time.AfterFunc(lingerGrace, func() { stop(errLingering) })
				defer linger.Stop()
			}
		}
	}
	if err := lines.Err(); err != nil {
		refuse(fmt.Errorf("the stream could not be read: %v", err))
	}
	cmd.Wait()
	run.Elapsed = time.Since(start).Round(time.Millisecond)
	run.ExitCode = cmd.ProcessState.ExitCode()
	if result != nil {
		run.NumTurns, run.CostUSD = result.NumTurns, result.TotalCostUSD
		run.RawUsage, run.PermissionDenials = result.Usage, result.PermissionDenials
		json.Unmarshal(result.Usage, &run.Usage)
	}

	// What a failure left behind, most telling first: stderr, then anything
	// on stdout that was not a stream message.
	evidence := stderr.String() + "\n" + stray.String()
	run.Output = evidence
	if result != nil && (result.IsError || result.Subtype != "success") {
		run.Output = result.Result + "\n" + evidence
	}
	switch cause := context.Cause(callCtx); {
	case refusal != nil:
		run.Outcome, run.Detail = Failed, refusal.Error()
	case result != nil:
		// The stream says the call finished. A child that then lingers
		// until it is killed does not undo the answer it gave.
		run.Outcome, run.Detail = classify(result, run.RateLimits, run.ExitCode, evidence)
		if (run.Outcome == OK || run.Outcome == Malformed) && !initChecked {
			// Fail closed: with no init message nothing says which tools
			// the call could reach.
			run.Outcome, run.Detail = Failed, "the call sent no init message, so the Step's tools were never checked"
		}
		if run.Outcome == OK {
			run.Result = result.StructuredOutput
		}
	case ctx.Err() != nil:
		run.Outcome, run.Detail = Failed, "the call was cancelled: "+ctx.Err().Error()
	case errors.Is(cause, context.DeadlineExceeded):
		run.Outcome, run.Detail = TimedOut, "the call outran the Step's timeout of "+spec.Timeout.String()
	default:
		run.Outcome, run.Detail = classify(nil, run.RateLimits, run.ExitCode, evidence)
	}
	return run
}
