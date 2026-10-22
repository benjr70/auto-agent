package research

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestTaskPrompt(t *testing.T) {
	t.Run("the task prompt carries the task rules of the resolve skill", func(t *testing.T) {
		for _, rule := range []string{
			"`done`",
			"`needs-code`",
			"`needs-human`",
			"every fact later tickets need",
			"Never change product code",
		} {
			if !strings.Contains(taskPrompt, rule) {
				t.Errorf("the task prompt lacks %q", rule)
			}
		}
	})
	t.Run("the task prompt carries no GitHub and no git-write instruction", func(t *testing.T) {
		command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|--add-label|--remove-label|AFK:")
		if found := command.FindAllString(taskPrompt, -1); found != nil {
			t.Errorf("the task prompt names %q", found)
		}
		if !strings.Contains(taskPrompt, "Never commit, push, stage") || !strings.Contains(taskPrompt, "Never read or write the issue tracker") {
			t.Error("the task prompt does not forbid committing, pushing or the tracker")
		}
	})
}

func TestTaskSpec(t *testing.T) {
	t.Run("the task result schema names exactly the fields of the task result type, all required", func(t *testing.T) {
		var s struct {
			Required             []string                   `json:"required"`
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal([]byte(taskSchema), &s); err != nil {
			t.Fatal(err)
		}
		var want, got []string
		for i := range reflect.TypeFor[TaskResult]().NumField() {
			want = append(want, reflect.TypeFor[TaskResult]().Field(i).Tag.Get("json"))
		}
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
	t.Run("a task is the Research Step in task mode: the same Step, with a shell to do the work", func(t *testing.T) {
		spec := TaskSpec()
		if spec.Name != Name || spec.Prompt != taskPrompt || spec.Schema != taskSchema {
			t.Errorf("TaskSpec is %s with another prompt or schema", spec.Name)
		}
		for _, tool := range []string{"Bash", "Read", "Write", "Edit", "WebFetch", "WebSearch"} {
			if !strings.Contains(","+strings.Join(spec.Tools, ",")+",", ","+tool+",") {
				t.Errorf("the task Step lacks %s: %v", tool, spec.Tools)
			}
		}
	})
}

func TestTaskCheck(t *testing.T) {
	done := TaskResult{Outcome: TaskDone, Summary: "Signed up for the sandbox.", Facts: []string{"the key is in the vault at ci/sandbox"}}
	with := func(change func(*TaskResult)) TaskResult {
		r := done
		change(&r)
		return r
	}
	tests := []struct {
		name   string
		result TaskResult
		want   string // a fragment of the error; empty means it passes
	}{
		{"done with what was done and its facts passes", done, ""},
		{"done with no facts passes", with(func(r *TaskResult) { r.Facts = []string{} }), ""},
		{"needs-code saying what it would take passes", TaskResult{Outcome: TaskNeedsCode, Summary: "A new flag in the CLI.", Facts: []string{}}, ""},
		{"needs-human saying what is needed passes", TaskResult{Outcome: TaskNeedsHuman, Summary: "An admin must grant the app.", Facts: []string{}}, ""},
		{"an empty summary is rejected", with(func(r *TaskResult) { r.Summary = " " }), "summary is empty"},
		{"a summary of two paragraphs is rejected", with(func(r *TaskResult) { r.Summary = "One.\n\nTwo." }), "one paragraph"},
		{"a fact spanning lines is rejected", with(func(r *TaskResult) { r.Facts = []string{"a\nb"} }), "fact"},
		{"an empty fact is rejected", with(func(r *TaskResult) { r.Facts = []string{" "} }), "fact"},
		{"facts on a needs-code result are rejected", TaskResult{Outcome: TaskNeedsCode, Summary: "x", Facts: []string{"y"}}, "only a done task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check()
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestTaskResolution(t *testing.T) {
	t.Run("what was done, then every fact later tickets need, one to a line", func(t *testing.T) {
		got := TaskResolution(TaskResult{Outcome: TaskDone, Summary: "Signed up for the sandbox.\n", Facts: []string{"key: vault ci/sandbox", " URL: https://sandbox.example.com "}})
		if want := "Signed up for the sandbox.\n\nFacts for later tickets:\n\n- key: vault ci/sandbox\n- URL: https://sandbox.example.com"; got != want {
			t.Errorf("TaskResolution =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("a task with no facts is what was done alone", func(t *testing.T) {
		if got := TaskResolution(TaskResult{Outcome: TaskDone, Summary: "Moved the rows.", Facts: []string{}}); got != "Moved the rows." {
			t.Errorf("TaskResolution = %q", got)
		}
	})
	t.Run("the needs-code comment is the resolve skill's, with what it would take", func(t *testing.T) {
		got := NeedsCodeComment("A new flag in the CLI.\n")
		want := "afk-resolve: this task needs a change to product code, which a Decision ticket never carries — relabelled HITL for a human to route (as a Slice off a Spec, or by re-scoping the ticket). What it would take: A new flag in the CLI."
		if got != want {
			t.Errorf("NeedsCodeComment =\n%s\nwant\n%s", got, want)
		}
	})
}
