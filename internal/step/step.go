// Package step runs one bounded agent Step: a typed input goes in, one
// headless Claude Code call is made, and a classified run comes out. It is
// the one place that calls Claude Code (ADR 0011).
package step

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// pluginName is the plugin the Step prompts are unpacked as, so a Step named
// "review" is the slash command /factory:review.
const pluginName = "factory"

// structuredOutputTool is the synthetic tool Claude Code adds when a call
// carries a result schema; without it a Step cannot answer.
const structuredOutputTool = "StructuredOutput"

// Spec declares a Step: what it is asked, what it must answer, and what it
// may touch.
type Spec struct {
	// Name is the Step's skill name: lower case, digits and dashes.
	Name string
	// Prompt is the skill body, embedded in the binary by the Step's package
	// and unpacked for each call.
	Prompt string
	// Schema is the JSON Schema the result must satisfy.
	Schema string
	// Tools are the built-in tools the Step can see at all, as bare names.
	Tools []string
	// Allowed are the permission rules: what it may do with those tools,
	// such as "Bash(git diff:*)". A tool with no rule is seen but denied.
	Allowed []string
	// Timeout is the wall-clock limit for one call.
	Timeout time.Duration
}

var (
	skillName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	toolName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	// A rule is a tool, optionally narrowed in one pair of parentheses. No
	// comma: the rules are passed comma-separated, so one would split it.
	allowRule = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)(\([^(),]+\))?$`)
)

// validate reports the first thing wrong with a Spec. It catches before any
// call the mistakes that otherwise fail silently, such as a permission rule
// passed as a tool, which removes the tool.
func (s Spec) validate() error {
	switch {
	case !skillName.MatchString(s.Name):
		return fmt.Errorf("the Step's name %q is not a skill name (lower case, digits, dashes)", s.Name)
	case strings.TrimSpace(s.Prompt) == "":
		return errors.New("the Step has no prompt")
	case !isJSONObject(s.Schema):
		return errors.New("the Step's result schema is not a JSON object")
	case s.Timeout <= 0:
		return errors.New("the Step has no timeout")
	}
	declared := map[string]bool{}
	for _, tool := range s.Tools {
		if !toolName.MatchString(tool) {
			return fmt.Errorf("%q is a permission rule, not a tool: Tools takes bare names", tool)
		}
		declared[tool] = true
	}
	for _, rule := range s.Allowed {
		parts := allowRule.FindStringSubmatch(rule)
		if parts == nil {
			return fmt.Errorf("%q is not a permission rule: a tool name, then at most one (…) with no comma in it", rule)
		}
		if !declared[parts[1]] {
			return fmt.Errorf("%q allows a tool the Step does not declare", rule)
		}
	}
	return nil
}

func isJSONObject(text string) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal([]byte(text), &object) == nil && object != nil
}

// Call is one use of a Step.
type Call struct {
	// Dir is the checkout the Step works in.
	Dir string
	// Model is the model to run on, chosen by the Model policy.
	Model string
	// Input is the Step's typed input, sent as JSON after the skill name.
	Input any
	// Resume is the session to take up again instead of starting one: a
	// Step the usage limit cut off, or one whose result was not accepted.
	// Message is then what the session is told, in place of the input.
	Resume  string
	Message string
}

// Outcome is how a Step's call ended.
type Outcome string

const (
	// OK: the Step returned a result that satisfies its schema.
	OK Outcome = "OK"
	// Exhausted: the usage budget ran out. Not the Step's fault.
	Exhausted Outcome = "EXHAUSTED"
	// AuthDead: Claude's credential is dead. Not the Step's fault.
	AuthDead Outcome = "AUTH_DEAD"
	// Malformed: the call succeeded but returned no usable result.
	Malformed Outcome = "MALFORMED"
	// TimedOut: the call outran the Step's timeout and was killed.
	TimedOut Outcome = "TIMED_OUT"
	// Failed: anything else.
	Failed Outcome = "FAILED"
)

// Run is one classified call of a Step: the Outcome, the result when there
// is one, and what the journal records of it.
type Run struct {
	Outcome Outcome
	// Detail says why, for every Outcome but OK.
	Detail string
	// Output is the text the call left outside its stream (stderr, stray
	// stdout) and, for a call that failed, its result text: where a usage
	// limit's reset time and Claude's login-expiry notice are read from.
	Output string
	// Result is the structured result, verbatim. Only an OK run has one.
	Result json.RawMessage
	// SessionID identifies the session, so a cut-off Step can be resumed.
	SessionID string
	// Model is the model the call actually ran on.
	Model          string
	PermissionMode string
	// Tools are the tools the call could reach, as the init message listed
	// them.
	Tools    []string
	NumTurns int
	CostUSD  float64
	Usage    Usage
	// RawUsage is the result's usage object, verbatim.
	RawUsage json.RawMessage
	// RateLimits are the rate-limit events of the stream, verbatim and in
	// order.
	RateLimits []json.RawMessage
	// PermissionDenials are the tool calls the Step attempted and was denied.
	// A denial does not fail the run.
	PermissionDenials []PermissionDenial
	ExitCode          int
	Elapsed           time.Duration
}

// Usage is the token usage of one call.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// PermissionDenial is one denied tool call.
type PermissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// Decode reads an OK run's result into the Step's result type. A field the
// type does not have is an error: the result is then not what was asked for.
func Decode[T any](run Run) (T, error) {
	var result T
	if run.Outcome != OK {
		return result, fmt.Errorf("the Step did not return a result: %s: %s", run.Outcome, run.Detail)
	}
	dec := json.NewDecoder(bytes.NewReader(run.Result))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return result, fmt.Errorf("the Step's result does not fit its type: %w", err)
	}
	return result, nil
}

// The stream messages the runner reads. Everything else passes by.
type initMessage struct {
	SessionID      string   `json:"session_id"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permissionMode"`
	Tools          []string `json:"tools"`
	SlashCommands  []string `json:"slash_commands"`
	Skills         []string `json:"skills"`
}

type resultMessage struct {
	Subtype           string             `json:"subtype"`
	IsError           bool               `json:"is_error"`
	Result            string             `json:"result"`
	StructuredOutput  json.RawMessage    `json:"structured_output"`
	NumTurns          int                `json:"num_turns"`
	TotalCostUSD      float64            `json:"total_cost_usd"`
	Usage             json.RawMessage    `json:"usage"`
	PermissionDenials []PermissionDenial `json:"permission_denials"`
}

// checkInit compares what the call can reach with what the Step declared.
// Claude Code drops a tool it cannot parse and loads an account's MCP
// connectors without being asked, both silently: the init message is the
// only place either shows.
func checkInit(spec Spec, init initMessage) error {
	skill := pluginName + ":" + spec.Name
	loaded := false
	for _, name := range append(append([]string{}, init.SlashCommands...), init.Skills...) {
		loaded = loaded || strings.TrimPrefix(name, "/") == skill
	}
	if !loaded {
		return fmt.Errorf("the Step's skill %q did not load", skill)
	}
	want := map[string]bool{structuredOutputTool: true}
	for _, tool := range spec.Tools {
		want[tool] = true
	}
	var missing, undeclared []string
	got := map[string]bool{}
	for _, tool := range init.Tools {
		got[tool] = true
		if !want[tool] {
			undeclared = append(undeclared, tool)
		}
	}
	for tool := range want {
		if !got[tool] {
			missing = append(missing, tool)
		}
	}
	if len(missing) == 0 && len(undeclared) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(undeclared)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "missing "+strings.Join(missing, ", "))
	}
	if len(undeclared) > 0 {
		parts = append(parts, "undeclared "+strings.Join(undeclared, ", "))
	}
	return errors.New("the tools the Step got differ from the tools it declared: " + strings.Join(parts, "; "))
}

// schemaGiveUp is the result subtype when Claude Code stops retrying a
// structured result that keeps failing its schema: the call ran, and there
// is no usable result.
const schemaGiveUp = "error_max_structured_output_retries"

// The signatures Claude emits when a call is cut off by a usage limit or a
// dead credential, as lib/exhaustion-classifier.sh has them. They are kept
// specific: a tool's own 429 in a failure is not exhaustion.
var (
	exhaustionText = regexp.MustCompile(`(?i)hit your [a-z0-9 .-]+ limit|usage limit reached|usage limit will reset|session limit|weekly limit|rate_limit_error|429[^\n]*rate.?limit`)
	authDeadText   = regexp.MustCompile(`(?i)authentication_failed|failed to authenticate|oauth (session|token) (has )?expired|not logged in.*(run|use) /login|please run /login`)
)

// classify decides the Outcome of a call that ran to its end, from the
// stream and the result and never from the exit code alone. output is the
// text a failure left behind outside the result: stderr, and anything on
// stdout that was not a stream message.
func classify(result *resultMessage, rateLimits []json.RawMessage, exitCode int, output string) (Outcome, string) {
	// A call that finished finished: whatever its prose says about limits
	// is prose.
	if result != nil && !result.IsError && result.Subtype == "success" {
		if len(result.StructuredOutput) == 0 || string(result.StructuredOutput) == "null" {
			return Malformed, "the call succeeded with no structured result"
		}
		return OK, ""
	}
	evidence := output
	if result != nil {
		evidence = result.Result + "\n" + output
	}
	switch {
	case result != nil && result.Subtype == schemaGiveUp:
		return Malformed, "the call could not produce a result that satisfies the schema"
	case lastRejected(rateLimits):
		return Exhausted, "the stream's last rate-limit event was rejected"
	case authDeadText.MatchString(evidence):
		return AuthDead, matchingLine(authDeadText, evidence)
	case exhaustionText.MatchString(evidence):
		return Exhausted, matchingLine(exhaustionText, evidence)
	case result != nil:
		return Failed, result.Subtype + ": " + excerpt(result.Result)
	default:
		return Failed, fmt.Sprintf("no result message (exit %d): %s", exitCode, excerpt(firstLine(output)))
	}
}

// lastRejected reports whether the most recent rate-limit event refused the
// call.
func lastRejected(rateLimits []json.RawMessage) bool {
	if len(rateLimits) == 0 {
		return false
	}
	var event struct {
		Info struct {
			Status string `json:"status"`
		} `json:"rate_limit_info"`
	}
	return json.Unmarshal(rateLimits[len(rateLimits)-1], &event) == nil && event.Info.Status == "rejected"
}

func matchingLine(pattern *regexp.Regexp, text string) string {
	for _, line := range strings.Split(text, "\n") {
		if pattern.MatchString(line) {
			return excerpt(strings.TrimSpace(line))
		}
	}
	return excerpt(firstLine(text))
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

// excerpt bounds how much of a child's text a Detail carries.
func excerpt(text string) string {
	const limit = 300
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// readOnlyGit are the git commands a read-only Step may run: they read the
// history and the tree, and change neither.
var readOnlyGit = []string{"Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)"}

// ReadOnly is a Step that changes nothing: it sees no tool that edits, and
// its shell may run read-only git and exactly the given commands, the
// Harness config's test and lint; an empty one is left out.
func ReadOnly(name, prompt, schema string, timeout time.Duration, commands ...string) Spec {
	allowed := append([]string{"Read", "Glob", "Grep"}, readOnlyGit...)
	for _, command := range commands {
		if command = strings.TrimSpace(command); command != "" {
			allowed = append(allowed, "Bash("+command+")")
		}
	}
	return Spec{Name: name, Prompt: prompt, Schema: schema, Tools: []string{"Read", "Glob", "Grep", "Bash"}, Allowed: allowed, Timeout: timeout}
}
