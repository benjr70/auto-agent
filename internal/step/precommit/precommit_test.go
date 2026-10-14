package precommit

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

var commands = implement.Commands{Install: "make deps", Test: "make test", Lint: "make lint"}

// Ported from the review round rules lib/runbook-check.sh holds the dispatch
// skill and the reviewer agent to, under their rule names.
func TestReviewRound(t *testing.T) {
	spec := Spec(commands)
	// agents/reviewer.md: tools
	t.Run("tools: the reviewer has read access and a shell, and no tool that edits", func(t *testing.T) {
		if !reflect.DeepEqual(spec.Tools, []string{"Read", "Glob", "Grep", "Bash"}) {
			t.Errorf("Tools = %v", spec.Tools)
		}
		for _, rule := range spec.Allowed {
			if strings.HasPrefix(rule, "Edit") || strings.HasPrefix(rule, "Write") || rule == "Bash" {
				t.Errorf("the reviewer is allowed %q", rule)
			}
		}
	})
	t.Run("tools: its shell runs read-only git and exactly the test and lint commands", func(t *testing.T) {
		want := []string{"Read", "Glob", "Grep", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(make test)", "Bash(make lint)"}
		if !reflect.DeepEqual(spec.Allowed, want) {
			t.Errorf("Allowed = %v", spec.Allowed)
		}
		if got := Spec(implement.Commands{Test: "go test ./..."}).Allowed; slices.Contains(got, "Bash()") || got[len(got)-1] != "Bash(go test ./...)" {
			t.Errorf("with no lint command, Allowed = %v", got)
		}
		if strings.Contains(strings.Join(spec.Allowed, ","), "make deps") {
			t.Error("the reviewer may run the install command")
		}
	})
	// skills/afk-dispatch/SKILL.md: review-verdicts (change-request, approved);
	// agents/reviewer.md: verdicts
	t.Run("review-verdicts: the reviewer answers approved or change-request, and nothing else", func(t *testing.T) {
		var s struct {
			Properties struct {
				Verdict struct {
					Enum []string `json:"enum"`
				} `json:"verdict"`
			} `json:"properties"`
		}
		json.Unmarshal([]byte(schema), &s)
		if !reflect.DeepEqual(s.Properties.Verdict.Enum, []string{Approved, ChangeRequest}) {
			t.Errorf("verdicts = %v", s.Properties.Verdict.Enum)
		}
	})
	// agents/reviewer.md's boundaries: no Edit, no Write, no git add or commit.
	t.Run("the prompt forbids changing anything and names no git write", func(t *testing.T) {
		if command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|AFK:").FindAllString(prompt, -1); command != nil {
			t.Errorf("the prompt names %q", command)
		}
		for _, rule := range []string{"You change\nnothing", "never create, change or delete a file", "never write the fix", "Mocks of internal collaborators"} {
			if !strings.Contains(prompt, rule) {
				t.Errorf("the prompt lacks %q", rule)
			}
		}
	})
}

func TestSpec(t *testing.T) {
	t.Run("the result schema names exactly the fields of the result type, all required", func(t *testing.T) {
		var s struct {
			Required             []string                   `json:"required"`
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal([]byte(schema), &s); err != nil {
			t.Fatal(err)
		}
		var want, got []string
		for i := range reflect.TypeFor[Result]().NumField() {
			want = append(want, reflect.TypeFor[Result]().Field(i).Tag.Get("json"))
		}
		for name := range s.Properties {
			got = append(got, name)
		}
		sort.Strings(want)
		sort.Strings(got)
		sort.Strings(s.Required)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(s.Required, want) || s.AdditionalProperties == nil || *s.AdditionalProperties {
			t.Errorf("properties %v, required %v, want %v", got, s.Required, want)
		}
	})
	t.Run("the runner accepts the Step, sends the typed input and decodes the typed result", func(t *testing.T) {
		spec := Spec(commands)
		fake := steptest.New(t)
		answer := Result{Verdict: ChangeRequest, ChangeRequests: []string{"cover the empty-list case"}}
		fake.Script(steptest.Reply{Lines: []string{
			steptest.Init("s1", "m", append([]string{"StructuredOutput"}, spec.Tools...), []string{"factory:pre-commit-review"}),
			steptest.Success(answer),
		}})
		input := Input{Issue: implement.Issue{Number: 7, Title: "T", Body: "b"}, Branch: "feat/issue-7", Base: "origin/main", Commands: commands, CommitScopes: []string{"a"}, Round: 2, Cap: 10}
		run := (&step.Claude{Binary: fake.Path}).Run(context.Background(), spec, step.Call{Dir: t.TempDir(), Model: "m", Input: input})
		got, err := step.Decode[Result](run)
		if err != nil || !reflect.DeepEqual(got, answer) {
			t.Fatalf("result = %+v, err = %v (%s: %s)", got, err, run.Outcome, run.Detail)
		}
		want := `/factory:pre-commit-review {"issue":{"number":7,"title":"T","body":"b"},"branch":"feat/issue-7","base":"origin/main","commands":{"install":"make deps","test":"make test","lint":"make lint"},"commit_scopes":["a"],"round":2,"cap":10}`
		if stdin := fake.Calls()[0].Stdin; stdin != want {
			t.Errorf("the Step was sent\n%s\nwant\n%s", stdin, want)
		}
		if mode, _ := fake.Calls()[0].Arg("--allowedTools"); !strings.Contains(mode, "Bash(make test)") || strings.Contains(mode, "Edit") {
			t.Errorf("--allowedTools %q", mode)
		}
	})
}

func TestCheck(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result Result
		want   string
	}{
		{"an approval asking for nothing passes", Result{Verdict: Approved, ChangeRequests: []string{}}, ""},
		{"a change request with asks passes", Result{Verdict: ChangeRequest, ChangeRequests: []string{"x"}}, ""},
		{"an approval with asks is rejected", Result{Verdict: Approved, ChangeRequests: []string{"x"}}, "an approval carries change requests"},
		{"a change request asking for nothing is rejected", Result{Verdict: ChangeRequest}, "asks for nothing"},
		{"a blank ask is rejected", Result{Verdict: ChangeRequest, ChangeRequests: []string{"x", "  "}}, "change request 2 is empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check()
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}
