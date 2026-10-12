package implement

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

func TestPrompt(t *testing.T) {
	t.Run("the prompt carries the test-first rules of the dispatch skill and the implementer agent", func(t *testing.T) {
		for _, rule := range []string{
			"one test at a time",
			"Do not write all the tests first",
			"Vertical slices only",
			"Tests verify behaviour through public interfaces",
			"Mock only at system\n  boundaries",
			"never weaken a test",
			"`CONTEXT.md`",
			"`docs/adr/`",
			"Never write a\nquestion",
		} {
			if !strings.Contains(prompt, rule) {
				t.Errorf("the prompt lacks %q", rule)
			}
		}
	})
	t.Run("the prompt carries no GitHub and no git-write instruction", func(t *testing.T) {
		command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|--add-label|--remove-label|AFK:")
		if found := command.FindAllString(prompt, -1); found != nil {
			t.Errorf("the prompt names %q", found)
		}
		if !strings.Contains(prompt, "Never commit, push, stage") || !strings.Contains(prompt, "Never read or write the issue tracker") {
			t.Error("the prompt does not forbid committing, pushing or the tracker")
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
		var want []string
		for i := range reflect.TypeFor[Result]().NumField() {
			want = append(want, reflect.TypeFor[Result]().Field(i).Tag.Get("json"))
		}
		var got []string
		for name := range s.Properties {
			got = append(got, name)
		}
		sort.Strings(want)
		sort.Strings(got)
		sort.Strings(s.Required)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(s.Required, want) || s.AdditionalProperties == nil || *s.AdditionalProperties {
			t.Errorf("properties %v, required %v, want %v and no others allowed", got, s.Required, want)
		}
	})
	t.Run("the runner accepts the Step, sends the typed input and decodes the typed result", func(t *testing.T) {
		spec := Spec()
		fake := steptest.New(t)
		answer := Result{Finished: true, Summary: "did it", CommitMessage: "feat(a): thing", Checklist: []string{"open it"}}
		fake.Script(steptest.Reply{Lines: []string{
			steptest.Init("s1", "m", append([]string{"StructuredOutput"}, spec.Tools...), []string{"factory:implement"}),
			steptest.Success(answer),
		}})
		input := Input{
			Issue: Issue{Number: 7, Title: "Tracer", Body: "## Acceptance criteria\n- [ ] works"}, Branch: "feat/issue-7",
			Commands: Commands{Install: "make deps", Test: "make test"}, CommitScopes: []string{"a"},
		}
		run := (&step.Claude{Binary: fake.Path}).Run(context.Background(), spec, step.Call{Dir: t.TempDir(), Model: "m", Input: input})
		got, err := step.Decode[Result](run)
		if err != nil || !reflect.DeepEqual(got, answer) {
			t.Fatalf("result = %+v, err = %v (%s: %s)", got, err, run.Outcome, run.Detail)
		}
		want := `/factory:implement {"issue":{"number":7,"title":"Tracer","body":"## Acceptance criteria\n- [ ] works"},"branch":"feat/issue-7","commands":{"install":"make deps","test":"make test"},"commit_scopes":["a"]}`
		if stdin := fake.Calls()[0].Stdin; stdin != want {
			t.Errorf("the Step was sent\n%s\nwant\n%s", stdin, want)
		}
	})
}

func TestCheck(t *testing.T) {
	scopes := []string{"core", "web"}
	done := Result{Finished: true, Summary: "s", CommitMessage: "feat(core): add the thing\n\nBody.", Checklist: []string{"open it and see it"}}
	with := func(change func(*Result)) Result {
		r := done
		r.Checklist = append([]string(nil), done.Checklist...)
		change(&r)
		return r
	}
	tests := []struct {
		name   string
		result Result
		want   string // a fragment of the error; empty means it passes
	}{
		{"a finished result with a subject, a known scope and a checklist passes", done, ""},
		{"a subject with no scope passes", with(func(r *Result) { r.CommitMessage = "fix: the thing" }), ""},
		{"an unfinished result needs only its summary", Result{Summary: "half done: the parser remains"}, ""},
		{"an unfinished result with no summary is rejected", Result{Summary: " \n"}, "summary is empty"},
		{"a finished result with no summary is rejected", with(func(r *Result) { r.Summary = "" }), "summary is empty"},
		{"a subject that is not conventional is rejected", with(func(r *Result) { r.CommitMessage = "Closes #7: add the thing" }), "is not <type>(<scope>)"},
		{"a scope outside the Target Project's is rejected", with(func(r *Result) { r.CommitMessage = "feat(api): add" }), `scope "api"`},
		{"a finished result with no commit message is rejected", with(func(r *Result) { r.CommitMessage = "" }), "is not <type>(<scope>)"},
		{"a finished result with no checklist is rejected", with(func(r *Result) { r.Checklist = nil }), "no checklist item"},
		{"a checklist item spanning lines is rejected", with(func(r *Result) { r.Checklist = []string{"ok", "two\nlines"} }), "checklist item 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check(scopes)
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}
