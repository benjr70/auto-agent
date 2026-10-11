// PROTOTYPE, throwaway. Answers issue #96: what is the contract for one agent
// Step, driven from Go? One real Step (Review: correctness) end to end.
//
//	go run . -checkout <dir at head> -base <sha> -head <sha> [-model sonnet]
//	go run . -claude stubs/<case>.sh ...     # failure cases, no model call
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ---- The Step's typed input and result -------------------------------------

type ReviewInput struct {
	Base       string `json:"base"`
	Head       string `json:"head"`
	IssueTitle string `json:"issue_title"`
	IssueBody  string `json:"issue_body"`
}

type Finding struct {
	Kind              string `json:"kind"` // defect | product-ambiguity | review-note
	Path              string `json:"path"`
	Line              int    `json:"line"`
	Summary           string `json:"summary"`
	FailureScenario   string `json:"failure_scenario"`
	QuotedRequirement string `json:"quoted_requirement"`
}

type ReviewResult struct {
	Findings []Finding `json:"findings"`
}

const reviewSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["findings"],
  "properties": {
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["kind", "path", "line", "summary", "failure_scenario", "quoted_requirement"],
        "properties": {
          "kind": {"enum": ["defect", "product-ambiguity", "review-note"]},
          "path": {"type": "string"},
          "line": {"type": "integer"},
          "summary": {"type": "string"},
          "failure_scenario": {"type": "string"},
          "quoted_requirement": {"type": "string"}
        }
      }
    }
  }
}`

// What the schema cannot say. A result that passes the schema but fails here
// is malformed too.
func (r ReviewResult) check(checkout string) error {
	for i, f := range r.Findings {
		if f.Kind == "defect" && f.FailureScenario == "" && f.QuotedRequirement == "" {
			return fmt.Errorf("finding %d: a defect needs a failure scenario or a quoted requirement", i)
		}
		if _, err := os.Stat(filepath.Join(checkout, f.Path)); err != nil {
			return fmt.Errorf("finding %d: path %q is not in the checkout", i, f.Path)
		}
	}
	return nil
}

// ---- The step runner: the one place that calls Claude Code -----------------

type StepSpec struct {
	Name    string   // the plugin skill, e.g. factory:review-correctness
	Schema  string   // JSON Schema the result must satisfy
	Tools   []string // built-in tools the Step can see at all (bare names)
	Allowed []string // permission rules: what it may do with them
	Model   string
	Timeout time.Duration
}

type Outcome string

const (
	OK        Outcome = "OK"
	Exhausted Outcome = "EXHAUSTED" // usage limit: pause, sleep to the reset
	AuthDead  Outcome = "AUTH_DEAD" // credential dead: park
	Malformed Outcome = "MALFORMED" // ran, but no usable result: failed attempt
	TimedOut  Outcome = "TIMED_OUT" // failed attempt
	Failed    Outcome = "FAILED"    // anything else: failed attempt
)

type StepRun struct {
	Outcome      Outcome
	Detail       string
	Result       json.RawMessage // structured_output, verbatim
	SessionID    string          // journal: lets a cut-off Step resume
	PluginLoaded bool
	ToolsSeen    json.RawMessage // from the init message: what the Step could actually reach
	Model        string
	PermMode     string
	NumTurns     int
	CostUSD      float64
	Usage        json.RawMessage
	RateLimits   []json.RawMessage // every rate_limit_event, verbatim
	Denials      json.RawMessage
	ExitCode     int
	Elapsed      time.Duration
}

func RunStep(ctx context.Context, claudeBin, pluginDir, checkout string, spec StepSpec, input any) StepRun {
	in, _ := json.Marshal(input)
	args := []string{
		"-p", "/" + spec.Name + " " + string(in),
		"--output-format", "stream-json", "--verbose",
		"--json-schema", spec.Schema,
		"--plugin-dir", pluginDir,
		"--permission-mode", "dontAsk",
		"--tools", strings.Join(spec.Tools, ","),
		"--allowedTools", strings.Join(spec.Allowed, ","),
		"--strict-mcp-config",   // no --mcp-config given: the account's connectors (mail, calendar) stay out
		"--setting-sources", "", // no user, project or local settings: only what Go passes
		"--model", spec.Model,
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudeBin, args...)
	cmd.Dir = checkout
	cmd.Env = stepEnv()
	// Kill the whole process group on timeout. Killing only claude leaves its
	// children holding stdout open, and the reader below never returns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, _ := cmd.StdoutPipe()

	run := StepRun{}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		run.Outcome, run.Detail = Failed, err.Error()
		return run
	}
	var result map[string]json.RawMessage
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ev map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue // not an event line; the transcript keeps it, the runner ignores it
		}
		switch str(ev["type"]) {
		case "system":
			if str(ev["subtype"]) == "init" {
				run.SessionID = str(ev["session_id"])
				run.Model = str(ev["model"])
				run.PermMode = str(ev["permissionMode"])
				run.ToolsSeen = ev["tools"]
				run.PluginLoaded = bytes.Contains(ev["slash_commands"], []byte(spec.Name)) ||
					bytes.Contains(ev["skills"], []byte(spec.Name))
			}
		case "rate_limit_event":
			run.RateLimits = append(run.RateLimits, append(json.RawMessage{}, sc.Bytes()...))
		case "result":
			result = ev
		}
	}
	err := cmd.Wait()
	run.Elapsed = time.Since(start).Round(time.Millisecond)
	run.ExitCode = cmd.ProcessState.ExitCode()

	// Classify from the stream and the result, never from the exit code alone.
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		run.Outcome, run.Detail = TimedOut, "wall-clock timeout "+spec.Timeout.String()
	case !run.PluginLoaded && run.SessionID != "":
		run.Outcome, run.Detail = Failed, "the step's skill did not load from --plugin-dir"
	case result == nil:
		run.Outcome, run.Detail = classifyNoResult(run, stderr.String(), err)
	default:
		run.NumTurns = num(result["num_turns"])
		json.Unmarshal(result["total_cost_usd"], &run.CostUSD)
		run.Usage = result["usage"]
		run.Denials = result["permission_denials"]
		text := str(result["result"])
		switch {
		case rateLimitRejected(run) || strings.Contains(text, "usage limit") || strings.Contains(text, "hit your limit"):
			run.Outcome, run.Detail = Exhausted, text
		case strings.Contains(text, "Invalid API key") || strings.Contains(text, "/login"):
			run.Outcome, run.Detail = AuthDead, text
		case str(result["is_error"]) == "true" || str(result["subtype"]) != "success":
			run.Outcome, run.Detail = Failed, str(result["subtype"])+": "+text
		case len(result["structured_output"]) == 0 || string(result["structured_output"]) == "null":
			run.Outcome, run.Detail = Malformed, "success with no structured_output"
		default:
			run.Outcome, run.Result = OK, result["structured_output"]
		}
	}
	return run
}

func classifyNoResult(run StepRun, stderr string, err error) (Outcome, string) {
	if rateLimitRejected(run) {
		return Exhausted, "stream ended on a rejected rate-limit event"
	}
	return Failed, fmt.Sprintf("no result message (exit %d, %v): %s", run.ExitCode, err, firstLine(stderr))
}

func rateLimitRejected(run StepRun) bool {
	if len(run.RateLimits) == 0 {
		return false
	}
	var ev struct {
		Info struct{ Status string } `json:"rate_limit_info"`
	}
	json.Unmarshal(run.RateLimits[len(run.RateLimits)-1], &ev)
	return ev.Info.Status == "rejected"
}

// A Step holds no GitHub write credential: no token in the env, and gh pointed
// at an empty config dir. Git keeps working locally; a push has nothing to
// authenticate with.
func stepEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k := strings.SplitN(kv, "=", 2)[0]
		switch k {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GH_CONFIG_DIR", "GIT_ASKPASS", "SSH_AUTH_SOCK":
			continue
		}
		env = append(env, kv)
	}
	empty, _ := os.MkdirTemp("", "step-gh-")
	return append(env, "GH_CONFIG_DIR="+empty, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null")
}

func str(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(r))
}
func num(r json.RawMessage) int { var n int; json.Unmarshal(r, &n); return n }
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- The caller: what the state machine would do with the Step -------------

func main() {
	claudeBin := flag.String("claude", "claude", "claude binary (a stub for the failure cases)")
	checkout := flag.String("checkout", ".", "checkout at head")
	base := flag.String("base", "", "base commit")
	head := flag.String("head", "", "head commit")
	title := flag.String("title", "", "issue title")
	body := flag.String("body", "", "issue body")
	model := flag.String("model", "sonnet", "model")
	timeout := flag.Duration("timeout", 10*time.Minute, "wall-clock timeout")
	flag.Parse()
	here, _ := os.Getwd()

	spec := StepSpec{
		Name:    "factory:review-correctness",
		Schema:  reviewSchema,
		Tools:   []string{"Read", "Grep", "Glob", "Bash"},
		Allowed: []string{"Read", "Grep", "Glob", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)"},
		Model:   *model,
		Timeout: *timeout,
	}
	run := RunStep(context.Background(), *claudeBin, filepath.Join(here, "plugin"), *checkout, spec,
		ReviewInput{Base: *base, Head: *head, IssueTitle: *title, IssueBody: *body})

	fmt.Printf("outcome        %s\n", run.Outcome)
	if run.Detail != "" {
		fmt.Printf("detail         %s\n", run.Detail)
	}
	fmt.Printf("session        %s\nmodel          %s\npermission     %s\nplugin loaded  %v\n", run.SessionID, run.Model, run.PermMode, run.PluginLoaded)
	fmt.Printf("tools seen     %s\n", run.ToolsSeen)
	fmt.Printf("exit / turns   %d / %d\nelapsed        %s\ncost           $%.4f\n", run.ExitCode, run.NumTurns, run.Elapsed, run.CostUSD)
	fmt.Printf("usage          %s\n", run.Usage)
	fmt.Printf("denials        %s\n", run.Denials)
	for _, rl := range run.RateLimits {
		fmt.Printf("rate limit     %s\n", rl)
	}
	if run.Outcome != OK {
		fmt.Println("\n=> the state machine counts a failed attempt, or pauses, or parks; nothing is posted")
		os.Exit(1)
	}

	var res ReviewResult
	dec := json.NewDecoder(bytes.NewReader(run.Result))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		fmt.Printf("\n=> MALFORMED (Go decode): %v\n", err)
		os.Exit(1)
	}
	if err := res.check(*checkout); err != nil {
		fmt.Printf("\n=> MALFORMED (Go check): %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n%d findings; Go would now dedupe, post a thread per defect, list the notes:\n", len(res.Findings))
	for _, f := range res.Findings {
		fmt.Printf("\n  [%s] %s:%d\n    %s\n", f.Kind, f.Path, f.Line, f.Summary)
		if f.FailureScenario != "" {
			fmt.Printf("    fails when: %s\n", f.FailureScenario)
		}
		if f.QuotedRequirement != "" {
			fmt.Printf("    requirement: %s\n", f.QuotedRequirement)
		}
	}
}
