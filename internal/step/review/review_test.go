package review

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// flat is a prompt with its line breaks as spaces, so a rule is found
// wherever the text wraps.
func flat(prompt string) string { return strings.Join(strings.Fields(prompt), " ") }

var commands = implement.Commands{Install: "make deps", Test: "make test", Lint: "make lint"}

// Ported from the rules lib/runbook-check.sh holds the pr-review and
// correctness-review skills to, under their rule names. The rules about
// posting (verdict lines, the done-marker, the labels, the anchoring
// fallback, the bar) are the Daemon's now: internal/finding and the Fire.
func TestReviewSteps(t *testing.T) {
	for name, spec := range map[string]step.Spec{CorrectnessName: Correctness(commands), SpecName: Spec(commands)} {
		t.Run(name+": read-only: its tools read, and its shell runs read-only git and exactly the test and lint commands", func(t *testing.T) {
			if !reflect.DeepEqual(spec.Tools, []string{"Read", "Glob", "Grep", "Bash"}) {
				t.Errorf("Tools = %v", spec.Tools)
			}
			want := []string{"Read", "Glob", "Grep", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(make test)", "Bash(make lint)"}
			if !reflect.DeepEqual(spec.Allowed, want) {
				t.Errorf("Allowed = %v", spec.Allowed)
			}
			if got := Correctness(implement.Commands{Test: "go test ./..."}).Allowed; got[len(got)-1] != "Bash(go test ./...)" || strings.Contains(strings.Join(got, ","), "Bash()") {
				t.Errorf("with no lint command, Allowed = %v", got)
			}
		})
		// correctness-review: read-only; pr-review: never-fixes, never-merges
		t.Run(name+": the prompt forbids changing anything and names no write", func(t *testing.T) {
			if command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|AFK:").FindAllString(spec.Prompt, -1); command != nil {
				t.Errorf("the prompt names %q", command)
			}
			for _, rule := range []string{"you never write", "never create, change or delete a file", "Never read or write the issue tracker or any pull request", "Never invent a line number"} {
				if !strings.Contains(flat(spec.Prompt), rule) {
					t.Errorf("the prompt lacks %q", rule)
				}
			}
		})
		// pr-review: kind-contract
		t.Run(name+": kind-contract: a Finding is a defect, a product ambiguity or a review note", func(t *testing.T) {
			var s struct {
				Properties struct {
					Findings struct {
						Items struct {
							Properties struct {
								Kind struct {
									Enum []finding.Kind `json:"enum"`
								} `json:"kind"`
							} `json:"properties"`
						} `json:"items"`
					} `json:"findings"`
				} `json:"properties"`
			}
			json.Unmarshal([]byte(spec.Schema), &s)
			if got := s.Properties.Findings.Items.Properties.Kind.Enum; !reflect.DeepEqual(got, []finding.Kind{finding.Defect, finding.ProductAmbiguity, finding.ReviewNote}) {
				t.Errorf("kinds = %v", got)
			}
		})
	}
	// correctness-review: bar (concrete failure, quoted, Standards)
	t.Run("bar: a defect is a concrete failure or a quoted requirement, and Standards findings are never defects", func(t *testing.T) {
		for _, rule := range []string{"concrete failure", "quoted requirement contradicted", "Standards findings are never defects"} {
			if !strings.Contains(flat(Correctness(commands).Prompt), rule) {
				t.Errorf("the correctness prompt lacks %q", rule)
			}
		}
	})
	// pr-review: no-scope-creep
	t.Run("no-scope-creep: code nobody asked for is not a finding of its own", func(t *testing.T) {
		if p := flat(Spec(commands).Prompt); !strings.Contains(p, "Code nobody asked for is not a finding of its own") || strings.Contains(p, "scope-creep") {
			t.Error("the Spec prompt lets scope creep be a category")
		}
	})
	t.Run("the two Steps have their own names and prompts", func(t *testing.T) {
		if Correctness(commands).Name == Spec(commands).Name || Correctness(commands).Prompt == Spec(commands).Prompt {
			t.Error("the two Review Steps are the same Step")
		}
	})
}

func TestSchema(t *testing.T) {
	t.Run("the result schema names exactly the fields of the result types, all required", func(t *testing.T) {
		var s struct {
			Required             []string `json:"required"`
			AdditionalProperties *bool    `json:"additionalProperties"`
			Properties           struct {
				Findings struct {
					Items struct {
						Required             []string                   `json:"required"`
						Properties           map[string]json.RawMessage `json:"properties"`
						AdditionalProperties *bool                      `json:"additionalProperties"`
					} `json:"items"`
				} `json:"findings"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(schema), &s); err != nil {
			t.Fatal(err)
		}
		items := s.Properties.Findings.Items
		var want, got []string
		for i := range reflect.TypeFor[Found]().NumField() {
			want = append(want, reflect.TypeFor[Found]().Field(i).Tag.Get("json"))
		}
		for name := range items.Properties {
			got = append(got, name)
		}
		sort.Strings(want)
		sort.Strings(got)
		sort.Strings(items.Required)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(items.Required, want) || items.AdditionalProperties == nil || *items.AdditionalProperties {
			t.Errorf("properties %v, required %v, want %v", got, items.Required, want)
		}
		if !reflect.DeepEqual(s.Required, []string{"findings"}) || s.AdditionalProperties == nil || *s.AdditionalProperties {
			t.Errorf("top level: required %v", s.Required)
		}
	})
	t.Run("the runner accepts the Step, sends the typed input and decodes the typed result", func(t *testing.T) {
		spec := Spec(commands)
		fake := steptest.New(t)
		answer := Result{Findings: []Found{{Kind: finding.Defect, Category: "missing-requirement", Path: "a.go", Line: 3, Severity: "high", Summary: "AC 2 is missing", QuotedRequirement: "AC 2"}}}
		fake.Script(steptest.Reply{Lines: []string{
			steptest.Init("s1", "m", append([]string{"StructuredOutput"}, spec.Tools...), []string{"factory:review-spec"}),
			steptest.Success(answer),
		}})
		input := Input{PR: 55, Branch: "feat/issue-7", Base: "origin/main", Issue: implement.Issue{Number: 7, Title: "T", Body: "b"}, Commands: commands}
		run := (&step.Claude{Binary: fake.Path}).Run(context.Background(), spec, step.Call{Dir: t.TempDir(), Model: "m", Input: input})
		got, err := step.Decode[Result](run)
		if err != nil || !reflect.DeepEqual(got, answer) {
			t.Fatalf("result = %+v, err = %v (%s: %s)", got, err, run.Outcome, run.Detail)
		}
		want := `/factory:review-spec {"pr":55,"branch":"feat/issue-7","base":"origin/main","issue":{"number":7,"title":"T","body":"b"},"spec":null,"commands":{"install":"make deps","test":"make test","lint":"make lint"}}`
		if stdin := fake.Calls()[0].Stdin; stdin != want {
			t.Errorf("the Step was sent\n%s\nwant\n%s", stdin, want)
		}
	})
}

func TestResult(t *testing.T) {
	ok := Found{Kind: finding.Defect, Category: "bug", Path: "a.go", Line: 1, Severity: "low", Summary: "s"}
	for _, tt := range []struct {
		name   string
		result Result
		want   string
	}{
		{"no findings passes", Result{Findings: []Found{}}, ""},
		{"a whole finding passes", Result{Findings: []Found{ok}}, ""},
		{"a finding with no file is rejected", Result{Findings: []Found{ok, {Kind: finding.Defect, Category: "bug", Path: " ", Line: 1, Summary: "s"}}}, "finding 2 names no file"},
		{"a blank summary is rejected", Result{Findings: []Found{{Kind: finding.Defect, Category: "bug", Path: "a", Line: 1, Summary: " "}}}, "empty summary"},
		{"a blank category is rejected", Result{Findings: []Found{{Kind: finding.Defect, Path: "a", Line: 1, Summary: "s"}}}, "no category"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check()
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
	t.Run("the Findings carry the axis they were found on", func(t *testing.T) {
		got := Result{Findings: []Found{ok}}.On(finding.SpecAxis)
		if len(got) != 1 || got[0].Axis != finding.SpecAxis || got[0].Path != "a.go" || got[0].Kind != finding.Defect {
			t.Errorf("got %+v", got)
		}
	})
}
