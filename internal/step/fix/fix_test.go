package fix

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

func TestBrief(t *testing.T) {
	for _, tt := range []struct {
		name  string
		brief Brief
		want  string
	}{
		{"a change-request brief with asks", ChangeRequest([]string{"cover the empty-list case"}), ""},
		{"a change-request brief asking for nothing", ChangeRequest(nil), "asks for nothing"},
		{"a failed-verification brief naming its item", FailedVerification("the smoke", "1/2 failed", "GET /api 500"), ""},
		{"a failed-verification brief naming no item", Brief{Kind: KindFailedVerification}, "names no item"},
		{"a kind this build does not define", Brief{Kind: "ci-failure"}, `"ci-failure" is not a kind`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.brief.Check()
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPrompt(t *testing.T) {
	if command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|AFK:").FindAllString(prompt, -1); command != nil {
		t.Errorf("the prompt names %q", command)
	}
	for _, rule := range []string{"Never commit, push, stage", "Address every item", "never weaken a test", "`change-request`", "`failed-verification`"} {
		if !strings.Contains(prompt, rule) {
			t.Errorf("the prompt lacks %q", rule)
		}
	}
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
	t.Run("the runner accepts the Step, sends the typed brief and decodes the typed result", func(t *testing.T) {
		spec := Spec()
		fake := steptest.New(t)
		answer := Result{Summary: "covered it", CommitMessage: "test(a): cover the empty list"}
		fake.Script(steptest.Reply{Lines: []string{
			steptest.Init("s1", "m", append([]string{"StructuredOutput"}, spec.Tools...), []string{"factory:fix"}),
			steptest.Success(answer),
		}})
		input := Input{Issue: implement.Issue{Number: 7, Title: "T", Body: "b"}, Branch: "feat/issue-7", Commands: implement.Commands{Install: "i", Test: "t"},
			CommitScopes: []string{"a"}, Brief: ChangeRequest([]string{"cover the empty-list case"})}
		run := (&step.Claude{Binary: fake.Path}).Run(context.Background(), spec, step.Call{Dir: t.TempDir(), Model: "m", Input: input})
		got, err := step.Decode[Result](run)
		if err != nil || !reflect.DeepEqual(got, answer) {
			t.Fatalf("result = %+v, err = %v (%s: %s)", got, err, run.Outcome, run.Detail)
		}
		want := `/factory:fix {"issue":{"number":7,"title":"T","body":"b"},"branch":"feat/issue-7","commands":{"install":"i","test":"t"},"commit_scopes":["a"],"brief":{"kind":"change-request","change_requests":["cover the empty-list case"]}}`
		if stdin := fake.Calls()[0].Stdin; stdin != want {
			t.Errorf("the Step was sent\n%s\nwant\n%s", stdin, want)
		}
	})
}

func TestCheck(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result Result
		want   string
	}{
		{"a summary and a conventional subject pass", Result{Summary: "s", CommitMessage: "fix(a): cover it\n\nbody"}, ""},
		{"an empty summary is rejected", Result{Summary: " ", CommitMessage: "fix: x"}, "the summary is empty"},
		{"a subject that is not conventional is rejected", Result{Summary: "s", CommitMessage: "address review"}, "is not <type>(<scope>)"},
		{"a scope outside the Target Project's is rejected", Result{Summary: "s", CommitMessage: "fix(nope): x"}, `scope "nope"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check([]string{"a"})
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}
