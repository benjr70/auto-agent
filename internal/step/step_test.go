package step

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// The toy Step: it echoes a word back. No real Step is defined in this
// package.
type toyInput struct {
	Word string `json:"word"`
}

type toyResult struct {
	Echo string `json:"echo"`
}

const toySchema = `{"type":"object","additionalProperties":false,"required":["echo"],"properties":{"echo":{"type":"string"}}}`

func toySpec() Spec {
	return Spec{
		Name:    "toy-echo",
		Prompt:  "Echo the word in the input back as `echo`.\n",
		Schema:  toySchema,
		Tools:   []string{"Read", "Bash"},
		Allowed: []string{"Read", "Bash(git log:*)"},
		Timeout: 30 * time.Second,
	}
}

var (
	toyTools  = []string{"Bash", "Read", "StructuredOutput"}
	toySkills = []string{"compact", "factory:toy-echo"}
)

func initLine() string { return steptest.Init("sess-1", "claude-test-1", toyTools, toySkills) }

type harness struct {
	fake   *steptest.Fake
	runner *Claude
	dir    string
}

func newHarness(t *testing.T, environ ...string) *harness {
	t.Helper()
	fake := steptest.New(t)
	return &harness{
		fake: fake,
		dir:  t.TempDir(),
		runner: &Claude{
			Binary: fake.Path,
			Environ: func() []string {
				return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=/home/someone"}, environ...)
			},
		},
	}
}

func (h *harness) run(spec Spec) Run {
	return h.runner.Run(context.Background(), spec, Call{Dir: h.dir, Model: "opus", Input: toyInput{Word: "hello"}})
}

func TestClassification(t *testing.T) {
	const limitText = "You've hit your session limit · resets 10:50pm (America/New_York)"
	tests := []struct {
		name       string
		reply      steptest.Reply
		want       Outcome
		wantDetail string
	}{
		{
			name:  "a success carrying structured output is OK",
			reply: steptest.Reply{Lines: []string{initLine(), steptest.Success(toyResult{Echo: "hello"})}},
			want:  OK,
		},
		{
			name:  "a success is OK whatever the exit code and whatever its prose says about limits",
			reply: steptest.Reply{ExitCode: 1, Lines: []string{initLine(), steptest.Result(map[string]any{"result": "the docs say: usage limit reached", "structured_output": toyResult{Echo: "x"}})}},
			want:  OK,
		},
		{
			name:       "a success with no structured result is MALFORMED",
			reply:      steptest.Reply{Lines: []string{initLine(), steptest.Result(map[string]any{"result": "here is some prose instead"})}},
			want:       Malformed,
			wantDetail: "no structured result",
		},
		{
			name:       "a success whose structured result is null is MALFORMED",
			reply:      steptest.Reply{Lines: []string{initLine(), steptest.Result(map[string]any{"structured_output": nil})}},
			want:       Malformed,
			wantDetail: "no structured result",
		},
		{
			name: "a rejected rate-limit event with an error result is EXHAUSTED",
			reply: steptest.Reply{ExitCode: 1, Lines: []string{
				initLine(), steptest.RateLimit("rejected", 1791590400, "five_hour"),
				steptest.Result(map[string]any{"is_error": true, "result": limitText}),
			}},
			want:       Exhausted,
			wantDetail: "rate-limit event",
		},
		{
			name:       "a stream that ends on a rejected rate-limit event is EXHAUSTED",
			reply:      steptest.Reply{ExitCode: 1, Lines: []string{initLine(), steptest.RateLimit("rejected", 1791590400, "five_hour")}},
			want:       Exhausted,
			wantDetail: "rate-limit event",
		},
		{
			name:       "the limit text alone, with no event, is EXHAUSTED",
			reply:      steptest.Reply{ExitCode: 1, Lines: []string{initLine(), steptest.Result(map[string]any{"is_error": true, "result": limitText})}},
			want:       Exhausted,
			wantDetail: "session limit",
		},
		{
			name:       "the limit text on stderr with no stream at all is EXHAUSTED",
			reply:      steptest.Reply{ExitCode: 1, Stderr: "Claude AI usage limit reached|1791590400\n"},
			want:       Exhausted,
			wantDetail: "usage limit reached",
		},
		{
			name: "an allowed rate-limit event does not make a failure EXHAUSTED",
			reply: steptest.Reply{ExitCode: 1, Lines: []string{
				initLine(), steptest.RateLimit("allowed", 1791590400, "five_hour"),
				steptest.Result(map[string]any{"subtype": "error_during_execution", "is_error": true, "result": "the tool crashed"}),
			}},
			want:       Failed,
			wantDetail: "error_during_execution: the tool crashed",
		},
		{
			name:       "a dead credential in the result is AUTH_DEAD",
			reply:      steptest.Reply{ExitCode: 1, Lines: []string{initLine(), steptest.Result(map[string]any{"is_error": true, "result": "Failed to authenticate: OAuth session expired and could not be refreshed"})}},
			want:       AuthDead,
			wantDetail: "Failed to authenticate",
		},
		{
			name:       "a dead credential before any stream is AUTH_DEAD",
			reply:      steptest.Reply{ExitCode: 1, Stderr: "Not logged in · Please run /login\n"},
			want:       AuthDead,
			wantDetail: "Please run /login",
		},
		{
			// lib/exhaustion-classifier.test.sh: a generic phrase and the
			// bare word are not signatures.
			name:  "a generic rate-limited phrase is not exhaustion: FAILED",
			reply: steptest.Reply{ExitCode: 1, Lines: []string{initLine()}, Stderr: "the docker pull was rate-limited"},
			want:  Failed,
		},
		{
			name:  "the bare word limit is not a signature",
			reply: steptest.Reply{ExitCode: 2, Lines: []string{initLine()}, Stderr: "the design limits the number of retries"},
			want:  Failed,
		},
		{
			name: "a dead credential outranks limit text",
			reply: steptest.Reply{ExitCode: 1, Lines: []string{initLine(), steptest.Result(map[string]any{
				"is_error": true, "result": "authentication_failed (and earlier: usage limit reached)"})}},
			want: AuthDead,
		},
		{
			name:       "giving up on the schema is MALFORMED: the call ran and there is no usable result",
			reply:      steptest.Reply{ExitCode: 1, Lines: []string{initLine(), steptest.Result(map[string]any{"subtype": "error_max_structured_output_retries", "is_error": true})}},
			want:       Malformed,
			wantDetail: "satisfies the schema",
		},
		{
			name: "a rejected rate-limit event outranks a dead credential and any other failure",
			reply: steptest.Reply{ExitCode: 1, Lines: []string{
				initLine(), steptest.RateLimit("rejected", 1791590400, "five_hour"),
				steptest.Result(map[string]any{"subtype": "error_max_turns", "is_error": true, "result": "authentication_failed"}),
			}},
			want:       Exhausted,
			wantDetail: "rate-limit event",
		},
		{
			name:  "a result that arrived before the child hung is kept: the stream says the call finished",
			reply: steptest.Reply{Hang: true, Lines: []string{initLine(), steptest.Success(toyResult{Echo: "hello"})}},
			want:  OK,
		},
		{
			name:       "a dead credential named only in a message's error field is AUTH_DEAD",
			reply:      steptest.Reply{ExitCode: 1, Lines: []string{initLine(), `{"type":"assistant","error":"authentication_failed","message":{"content":[]}}`, steptest.Result(map[string]any{"is_error": true, "result": ""})}},
			want:       AuthDead,
			wantDetail: "authentication_failed",
		},
		{
			name:       "a dead credential printed plainly on stdout is AUTH_DEAD",
			reply:      steptest.Reply{ExitCode: 1, Lines: []string{"Not logged in · Please run /login"}},
			want:       AuthDead,
			wantDetail: "Please run /login",
		},
		{
			name:       "an error result is FAILED with its subtype and text",
			reply:      steptest.Reply{ExitCode: 1, Lines: []string{initLine(), steptest.Result(map[string]any{"subtype": "error_max_turns", "is_error": true, "result": "ran out of turns"})}},
			want:       Failed,
			wantDetail: "error_max_turns: ran out of turns",
		},
		{
			name:  "a tool's own 429 in a failure is FAILED, not EXHAUSTED",
			reply: steptest.Reply{ExitCode: 1, Lines: []string{initLine(), steptest.Result(map[string]any{"is_error": true, "result": "gh: API rate limit exceeded (HTTP 403), too many requests"})}},
			want:  Failed,
		},
		{
			name:       "a child that dies with no result is FAILED with its exit code and stderr",
			reply:      steptest.Reply{ExitCode: 139, Lines: []string{initLine()}, Stderr: "segfault\nmore\n"},
			want:       Failed,
			wantDetail: "no result message (exit 139): segfault",
		},
		{
			name:       "an exit 0 with no result is FAILED, never OK",
			reply:      steptest.Reply{Lines: []string{initLine(), "this line is not JSON"}},
			want:       Failed,
			wantDetail: "no result message (exit 0)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.Script(tt.reply)
			spec := toySpec()
			if tt.reply.Hang {
				spec.Timeout = 400 * time.Millisecond
			}
			got := h.run(spec)
			if got.Outcome != tt.want || !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("Outcome = %s, Detail = %q; want %s and %q", got.Outcome, got.Detail, tt.want, tt.wantDetail)
			}
			if (got.Outcome == OK) != (got.Result != nil) {
				t.Errorf("Outcome %s with Result %s: only OK carries a result", got.Outcome, got.Result)
			}
		})
	}
}

func TestInitChecks(t *testing.T) {
	success := steptest.Success(toyResult{Echo: "hello"})
	tests := []struct {
		name       string
		tools      []string
		skills     []string
		wantDetail string
	}{
		{
			name:  "the Step fails when its skill is absent from the init message",
			tools: toyTools, skills: []string{"compact", "factory:other-step"},
			wantDetail: `the Step's skill "factory:toy-echo" did not load`,
		},
		{
			name:  "the Step fails when a declared tool is missing",
			tools: []string{"Read", "StructuredOutput"}, skills: toySkills,
			wantDetail: "missing Bash",
		},
		{
			name:  "the Step fails when it can reach a tool it did not declare",
			tools: []string{"Bash", "Read", "StructuredOutput", "mcp__claude_ai_Gmail__send_message", "Write"}, skills: toySkills,
			wantDetail: "undeclared Write, mcp__claude_ai_Gmail__send_message",
		},
		{
			name:  "the Step fails when it cannot return a structured result",
			tools: []string{"Bash", "Read"}, skills: toySkills,
			wantDetail: "missing StructuredOutput",
		},
	}
	t.Run("a result with no init message before it is refused: nothing says which tools the call had", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Lines: []string{success}})
		if got := h.run(toySpec()); got.Outcome != Failed || !strings.Contains(got.Detail, "no init message") || got.Result != nil {
			t.Errorf("Outcome = %s, Detail = %q", got.Outcome, got.Detail)
		}
	})
	t.Run("an init message in a shape the runner cannot read is refused, not waved through", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Lines: []string{
			`{"type":"system","subtype":"init","session_id":"s","tools":["Bash","Read","StructuredOutput","Write"],"skills":[{"name":"factory:toy-echo"}]}`, success,
		}})
		if got := h.run(toySpec()); got.Outcome != Failed || !strings.Contains(got.Detail, "could not be read") {
			t.Errorf("Outcome = %s, Detail = %q", got.Outcome, got.Detail)
		}
	})
	t.Run("only the call's own init message is checked: a later one cannot fail a finished Step", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Lines: []string{initLine(), success, steptest.Init("sub", "m", []string{"Write"}, nil)}})
		if got := h.run(toySpec()); got.Outcome != OK || got.SessionID != "sess-1" {
			t.Errorf("Outcome = %s, Detail = %q, SessionID = %q", got.Outcome, got.Detail, got.SessionID)
		}
	})
	t.Run("a skill listed with a leading slash has loaded", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Lines: []string{steptest.Init("s", "m", toyTools, []string{"/factory:toy-echo"}), success}})
		if got := h.run(toySpec()); got.Outcome != OK {
			t.Errorf("Outcome = %s: %s", got.Outcome, got.Detail)
		}
	})
	t.Run("a skill the init message lists under skills, not slash commands, has loaded", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Lines: []string{
			`{"type":"system","subtype":"init","session_id":"s","model":"m","permissionMode":"dontAsk","tools":["Bash","Read","StructuredOutput"],"slash_commands":["compact"],"skills":["factory:toy-echo"]}`,
			success,
		}})
		if got := h.run(toySpec()); got.Outcome != OK {
			t.Errorf("Outcome = %s: %s", got.Outcome, got.Detail)
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			// The fake would go on to succeed: the runner must not let it.
			h.fake.Script(steptest.Reply{Lines: []string{steptest.Init("sess-1", "m", tt.tools, tt.skills), success}})
			got := h.run(toySpec())
			if got.Outcome != Failed || !strings.Contains(got.Detail, tt.wantDetail) || got.Result != nil {
				t.Errorf("Outcome = %s, Detail = %q, Result = %s; want FAILED and %q", got.Outcome, got.Detail, got.Result, tt.wantDetail)
			}
			if got.SessionID != "sess-1" {
				t.Errorf("SessionID = %q: the failed run still records its session", got.SessionID)
			}
		})
	}
}

func TestTheCall(t *testing.T) {
	h := newHarness(t,
		"GH_TOKEN=ghp_secret", "GITHUB_TOKEN=ghp_secret", "GH_ENTERPRISE_TOKEN=ghp_secret", "GITHUB_ENTERPRISE_TOKEN=ghp_secret",
		"GH_CONFIG_DIR=/home/someone/.config/gh", "GIT_ASKPASS=/usr/bin/askpass", "SSH_ASKPASS=/usr/bin/askpass", "SSH_AUTH_SOCK=/run/agent.sock",
		"GIT_CONFIG_GLOBAL=/home/someone/.gitconfig", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=store",
		"GIT_SSH_COMMAND=ssh -i /home/someone/.ssh/id", "CLAUDE_CODE_OAUTH_TOKEN=keep-me", "LANG=C.UTF-8")
	h.fake.Script(steptest.Reply{Lines: []string{initLine(), steptest.Success(toyResult{Echo: "hello"})}})
	run := h.run(toySpec())
	if run.Outcome != OK {
		t.Fatalf("Outcome = %s: %s", run.Outcome, run.Detail)
	}
	calls := h.fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("claude was called %d times, want 1", len(calls))
	}
	call := calls[0]

	t.Run("the prompt is the Step's skill followed by its input as JSON", func(t *testing.T) {
		if call.Stdin != `/factory:toy-echo {"word":"hello"}` {
			t.Errorf("stdin = %q", call.Stdin)
		}
		if _, ok := call.Arg("-p"); !ok {
			t.Error("-p is not passed")
		}
		for _, arg := range call.Args {
			if strings.Contains(arg, "hello") {
				t.Errorf("the input is in the arguments (%q), where its size is capped and any user can read it", arg)
			}
		}
	})
	t.Run("the call asks for a stream, the schema, the tools, the rules and the model", func(t *testing.T) {
		for flag, want := range map[string]string{
			"--output-format": "stream-json", "--json-schema": toySchema, "--tools": "Read,Bash",
			"--allowedTools": "Read,Bash(git log:*)", "--model": "opus",
		} {
			if got, _ := call.Arg(flag); got != want {
				t.Errorf("%s = %q, want %q", flag, got, want)
			}
		}
		if _, ok := call.Arg("--verbose"); !ok {
			t.Error("--verbose is not passed; stream-json needs it")
		}
	})
	t.Run("the account's MCP connectors are excluded on every call; a permission mode is passed on every call", func(t *testing.T) {
		if _, ok := call.Arg("--strict-mcp-config"); !ok {
			t.Error("--strict-mcp-config is not passed")
		}
		if _, ok := call.Arg("--mcp-config"); ok {
			t.Error("an --mcp-config is passed though the Step declared none")
		}
		if mode, _ := call.Arg("--permission-mode"); mode != "dontAsk" {
			t.Errorf("--permission-mode = %q", mode)
		}
	})
	t.Run("the Step loads the Target Project's settings, never the user's", func(t *testing.T) {
		if sources, _ := call.Arg("--setting-sources"); sources != "project" {
			t.Errorf("--setting-sources = %q", sources)
		}
	})
	t.Run("the prompt is embedded in the binary and unpacked for the call", func(t *testing.T) {
		if got := call.Plugin["skills/toy-echo/SKILL.md"]; !strings.Contains(got, "Echo the word in the input back") || !strings.Contains(got, "name: toy-echo") {
			t.Errorf("SKILL.md at call time = %q", got)
		}
		var manifest struct{ Name string }
		if err := json.Unmarshal([]byte(call.Plugin[".claude-plugin/plugin.json"]), &manifest); err != nil || manifest.Name != "factory" {
			t.Errorf("plugin.json = %q (%v)", call.Plugin[".claude-plugin/plugin.json"], err)
		}
		dir, _ := call.Arg("--plugin-dir")
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the unpacked plugin %s is still there after the call", dir)
		}
	})
	t.Run("the Step runs in the checkout it was given", func(t *testing.T) {
		want, _ := filepath.EvalSymlinks(h.dir)
		got, _ := filepath.EvalSymlinks(call.Dir)
		if got != want {
			t.Errorf("working directory = %q, want %q", got, want)
		}
	})
	t.Run("the Step's environment carries no GitHub token", func(t *testing.T) {
		for key, value := range call.Env {
			if strings.Contains(value, "ghp_secret") {
				t.Errorf("%s still carries the token", key)
			}
		}
		for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_ASKPASS", "SSH_ASKPASS", "SSH_AUTH_SOCK"} {
			if _, set := call.Env[key]; set {
				t.Errorf("%s is set in the Step's environment", key)
			}
		}
	})
	t.Run("git config passed through the environment is dropped, and git cannot use ssh", func(t *testing.T) {
		for key := range call.Env {
			if strings.HasPrefix(key, "GIT_CONFIG_") && key != "GIT_CONFIG_GLOBAL" && key != "GIT_CONFIG_NOSYSTEM" {
				t.Errorf("%s reached the Step", key)
			}
		}
		if call.Env["GIT_SSH_COMMAND"] != "false" {
			t.Errorf("GIT_SSH_COMMAND = %q", call.Env["GIT_SSH_COMMAND"])
		}
	})
	t.Run("gh and git inside the Step find no stored credential either", func(t *testing.T) {
		if dir := call.Env["GH_CONFIG_DIR"]; dir == "" || dir == "/home/someone/.config/gh" {
			t.Errorf("GH_CONFIG_DIR = %q, want an empty directory of the call's own", dir)
		}
		if call.Env["GIT_CONFIG_GLOBAL"] != "/dev/null" || call.Env["GIT_CONFIG_NOSYSTEM"] != "1" || call.Env["GIT_TERMINAL_PROMPT"] != "0" {
			t.Errorf("git env = %q %q %q", call.Env["GIT_CONFIG_GLOBAL"], call.Env["GIT_CONFIG_NOSYSTEM"], call.Env["GIT_TERMINAL_PROMPT"])
		}
	})
	t.Run("the rest of the environment, Claude's own login included, is kept", func(t *testing.T) {
		if call.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "keep-me" || call.Env["LANG"] != "C.UTF-8" || call.Env["HOME"] != "/home/someone" {
			t.Errorf("env = %v", call.Env)
		}
	})
}

func TestRecordedFields(t *testing.T) {
	h := newHarness(t)
	allowed := steptest.RateLimit("allowed", 1791761400, "five_hour")
	warning := steptest.RateLimit("allowed_warning", 1791761400, "five_hour")
	h.fake.Script(steptest.Reply{Lines: []string{
		initLine(), allowed, `{"type":"assistant","message":{"content":[]}}`, warning,
		steptest.Result(map[string]any{
			"structured_output": toyResult{Echo: "hello"}, "num_turns": 7, "total_cost_usd": 0.0134,
			"usage":              map[string]any{"input_tokens": 10, "output_tokens": 11722, "cache_creation_input_tokens": 33502, "cache_read_input_tokens": 81099, "service_tier": "standard"},
			"permission_denials": []map[string]any{{"tool_name": "Bash", "tool_input": map[string]string{"command": "bash lib/x.test.sh"}}},
		}),
	}})
	got := h.run(toySpec())
	t.Run("the recorded fields match the scripted stream", func(t *testing.T) {
		if got.Outcome != OK || got.SessionID != "sess-1" || got.Model != "claude-test-1" || got.PermissionMode != "dontAsk" || got.NumTurns != 7 || got.CostUSD != 0.0134 || got.ExitCode != 0 {
			t.Errorf("run = %+v", got)
		}
		if !reflect.DeepEqual(got.Tools, toyTools) {
			t.Errorf("Tools = %q", got.Tools)
		}
		if want := (Usage{InputTokens: 10, OutputTokens: 11722, CacheCreationInputTokens: 33502, CacheReadInputTokens: 81099}); got.Usage != want {
			t.Errorf("Usage = %+v", got.Usage)
		}
		if !strings.Contains(string(got.RawUsage), `"service_tier":"standard"`) {
			t.Errorf("RawUsage = %s", got.RawUsage)
		}
	})
	t.Run("every rate-limit event is recorded, verbatim and in order", func(t *testing.T) {
		if len(got.RateLimits) != 2 || string(got.RateLimits[0]) != allowed || string(got.RateLimits[1]) != warning {
			t.Errorf("RateLimits = %s", got.RateLimits)
		}
	})
	t.Run("a denied tool call is recorded and does not fail the run", func(t *testing.T) {
		if len(got.PermissionDenials) != 1 || got.PermissionDenials[0].ToolName != "Bash" || !strings.Contains(string(got.PermissionDenials[0].ToolInput), "lib/x.test.sh") {
			t.Errorf("PermissionDenials = %+v", got.PermissionDenials)
		}
	})
	t.Run("the typed result decodes, and rejects a field the type does not have", func(t *testing.T) {
		result, err := Decode[toyResult](got)
		if err != nil || result.Echo != "hello" {
			t.Errorf("Decode = %+v, %v", result, err)
		}
		if _, err := Decode[struct{}](got); err == nil {
			t.Error("a result with an unknown field decoded")
		}
		if _, err := Decode[toyResult](Run{Outcome: Failed}); err == nil {
			t.Error("a run that is not OK decoded")
		}
	})
}

func TestOutput(t *testing.T) {
	t.Run("a failed call's output is its result text, its stderr and its stray stdout: where a limit's reset is read from", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{ExitCode: 1, Stderr: "resets 10:50pm (America/New_York)", Lines: []string{
			initLine(), "a stray line",
			steptest.Result(map[string]any{"subtype": "error_during_execution", "is_error": true, "result": "You've hit your session limit"}),
		}})
		got := h.run(toySpec())
		for _, want := range []string{"You've hit your session limit", "resets 10:50pm (America/New_York)", "a stray line"} {
			if !strings.Contains(got.Output, want) {
				t.Errorf("Output lacks %q: %q", want, got.Output)
			}
		}
	})
	t.Run("a finished call's output leaves its result text out: prose about limits is prose", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Stderr: "Your login expires in 3 days", Lines: []string{
			initLine(), steptest.Result(map[string]any{"result": "I hit your session limit test", "structured_output": map[string]string{"echo": "hello"}}),
		}})
		got := h.run(toySpec())
		if got.Outcome != OK || strings.Contains(got.Output, "session limit") || !strings.Contains(got.Output, "login expires in 3 days") {
			t.Errorf("Outcome %s, Output %q", got.Outcome, got.Output)
		}
	})
}

func TestTimeout(t *testing.T) {
	t.Run("a hung child is killed with its process group within the timeout", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Lines: []string{initLine()}, Hang: true})
		spec := toySpec()
		spec.Timeout = 400 * time.Millisecond
		start := time.Now()
		got := h.run(spec)
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("the run took %s: the reader waited on the child holding stdout", elapsed)
		}
		if got.Outcome != TimedOut || !strings.Contains(got.Detail, "400ms") || got.SessionID != "sess-1" {
			t.Errorf("Outcome = %s, Detail = %q, SessionID = %q", got.Outcome, got.Detail, got.SessionID)
		}
		calls := h.fake.Calls()
		if len(calls) != 1 || calls[0].ChildPID == 0 {
			t.Fatalf("the fake did not record its child: %+v", calls)
		}
		deadline := time.Now().Add(5 * time.Second)
		for syscall.Kill(calls[0].ChildPID, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("the child %d outlived the Step", calls[0].ChildPID)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	t.Run("a call that answers and then does not exit is ended soon after, with its answer", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Hang: true, Lines: []string{initLine(), steptest.Success(toyResult{Echo: "hello"})}})
		spec := toySpec()
		spec.Timeout = 5 * time.Minute
		start := time.Now()
		got := h.run(spec)
		if elapsed := time.Since(start); got.Outcome != OK || elapsed > lingerGrace+killGrace+5*time.Second {
			t.Errorf("Outcome = %s (%s) after %s", got.Outcome, got.Detail, elapsed)
		}
	})
	t.Run("a caller that gives up is not a timeout of the Step", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Script(steptest.Reply{Lines: []string{initLine()}, Hang: true})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		got := h.runner.Run(ctx, toySpec(), Call{Dir: h.dir, Model: "opus", Input: toyInput{Word: "x"}})
		if got.Outcome != Failed || !strings.Contains(got.Detail, "cancelled") {
			t.Errorf("Outcome = %s, Detail = %q", got.Outcome, got.Detail)
		}
	})
}

func TestBadSpecsAndCalls(t *testing.T) {
	tests := []struct {
		name       string
		change     func(*Spec)
		call       func(*Call)
		wantDetail string
	}{
		{name: "a Step with no name is refused", change: func(s *Spec) { s.Name = "" }, wantDetail: "name"},
		{name: "a Step whose name is not a skill name is refused", change: func(s *Spec) { s.Name = "Toy Echo" }, wantDetail: "name"},
		{name: "a Step with no prompt is refused", change: func(s *Spec) { s.Prompt = " \n" }, wantDetail: "prompt"},
		{name: "a Step whose schema is not JSON is refused", change: func(s *Spec) { s.Schema = "{" }, wantDetail: "schema"},
		{name: "a Step whose schema is JSON but no object is refused", change: func(s *Spec) { s.Schema = `"x"` }, wantDetail: "schema"},
		{name: "a rule with an unclosed parenthesis is refused", change: func(s *Spec) { s.Allowed = []string{"Bash(git diff:*"} }, wantDetail: "is not a permission rule"},
		{name: "a rule that smuggles a second rule past a comma is refused", change: func(s *Spec) { s.Allowed = []string{"Bash(git log:*),Write"} }, wantDetail: "is not a permission rule"},
		{name: "a Step whose name ends in a dash is refused", change: func(s *Spec) { s.Name = "toy-" }, wantDetail: "name"},
		{name: "a Step with no timeout is refused", change: func(s *Spec) { s.Timeout = 0 }, wantDetail: "timeout"},
		{name: "a tool written as a rule is refused: --tools takes bare names", change: func(s *Spec) { s.Tools = []string{"Read", "Bash(git diff:*)"} }, wantDetail: `"Bash(git diff:*)" is a permission rule`},
		{name: "a rule for a tool the Step cannot see is refused", change: func(s *Spec) { s.Allowed = append(s.Allowed, "Write(src/**)") }, wantDetail: `"Write(src/**)" allows a tool the Step does not declare`},
		{name: "a call with no model is refused", call: func(c *Call) { c.Model = "" }, wantDetail: "model"},
		{name: "a call with no checkout is refused", call: func(c *Call) { c.Dir = "" }, wantDetail: "checkout"},
		{name: "an input that cannot be JSON is refused", call: func(c *Call) { c.Input = func() {} }, wantDetail: "input"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			spec, call := toySpec(), Call{Dir: h.dir, Model: "opus", Input: toyInput{Word: "x"}}
			if tt.change != nil {
				tt.change(&spec)
			}
			if tt.call != nil {
				tt.call(&call)
			}
			got := h.runner.Run(context.Background(), spec, call)
			if got.Outcome != Failed || !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("Outcome = %s, Detail = %q; want FAILED and %q", got.Outcome, got.Detail, tt.wantDetail)
			}
			if calls := h.fake.Calls(); len(calls) != 0 {
				t.Errorf("claude was called %d times for a call that should never start", len(calls))
			}
		})
	}
	t.Run("a claude that is not installed is FAILED, naming it", func(t *testing.T) {
		h := newHarness(t)
		h.runner.Binary = filepath.Join(t.TempDir(), "no-such-claude")
		if got := h.run(toySpec()); got.Outcome != Failed || !strings.Contains(got.Detail, "no-such-claude") {
			t.Errorf("Outcome = %s, Detail = %q", got.Outcome, got.Detail)
		}
	})
}

// Runner is the one interface the claude call sits behind.
var _ Runner = (*Claude)(nil)
