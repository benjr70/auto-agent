package rulingrequest

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// decision is one the human can answer: two options, A recommended.
func decision(title string) ruling.Decision {
	return ruling.Decision{Title: title, Scenario: "s", Now: "n", Wants: "w", Why: "y", Options: []ruling.Option{
		{Letter: "A", Text: "keep it", Cost: "nothing", Recommended: true}, {Letter: "B", Text: "change it", Cost: "a fix", Fix: true},
	}}
}

var items = []Item{{Kind: KindProductAmbiguity, Path: "a.go", Finding: "once or always?"}, {Kind: KindUnruled, Path: "b.go", Line: 3, Authored: "bot", Finding: "f", Dispute: "d", Note: "no verdict"}}

func TestCheck(t *testing.T) {
	broken := decision("broken")
	broken.Options[1].Recommended = true
	for _, c := range []struct {
		name    string
		written []Written
		want    string
	}{
		{"one decision per item, each answerable, passes", []Written{{2, decision("b")}, {1, decision("a")}}, ""},
		{"an item with no decision fails", []Written{{1, decision("a")}}, "item 2 has no decision"},
		{"an item with two fails", []Written{{1, decision("a")}, {1, decision("a")}, {2, decision("b")}}, "item 1 has two decisions"},
		{"a decision for an item not given fails", []Written{{1, decision("a")}, {2, decision("b")}, {3, decision("c")}}, "item 3; the items are 1 to 2"},
		{"a decision the human cannot answer in one letter fails", []Written{{1, decision("a")}, {2, broken}}, "item 2: decision \"broken\" recommends 2 options"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := Result{Decisions: c.written}.Check(items)
			if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
				t.Errorf("Check = %v, want %q", err, c.want)
			}
		})
	}
	t.Run("the decisions come back in the order of their items", func(t *testing.T) {
		got := Result{Decisions: []Written{{2, decision("b")}, {1, decision("a")}}}.Ordered()
		if got[0].Title != "a" || got[1].Title != "b" {
			t.Errorf("got %+v", got)
		}
	})
}

func TestDrafted(t *testing.T) {
	a, b := decision("a"), decision("b")
	unanswerable := decision("c")
	unanswerable.Options = unanswerable.Options[:1]
	for _, c := range []struct {
		name  string
		items []Item
		want  string
	}{
		{"every item drafted, each answerable: no Step is needed", []Item{{Draft: &a}, {Draft: &b}}, "a,b"},
		{"an item with no draft needs the Step", []Item{{Draft: &a}, {}}, ""},
		{"a draft the human could not answer needs the Step", []Item{{Draft: &a}, {Draft: &unanswerable}}, ""},
		{"no items is nothing drafted", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Drafted(c.items)
			var titles []string
			for _, d := range got {
				titles = append(titles, d.Title)
			}
			if strings.Join(titles, ",") != c.want || ok != (c.want != "") {
				t.Errorf("Drafted = %v, %v", titles, ok)
			}
		})
	}
}

func flat(text string) string { return strings.Join(strings.Fields(text), " ") }

func TestPrompt(t *testing.T) {
	text := flat(prompt)
	for _, rule := range []string{"You never write", "Never read or write the issue tracker or any pull request", "Exactly one is `recommended`", "One decision per item, every item once", "`product-ambiguity`", "`ambiguity`", "`unruled`", "`cannot`", "whether choosing it changes the code"} {
		if !strings.Contains(text, rule) {
			t.Errorf("the prompt lacks %q", rule)
		}
	}
	if command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|AFK:").FindAllString(prompt, -1); command != nil {
		t.Errorf("the prompt names %q", command)
	}
}

func TestSpec(t *testing.T) {
	t.Run("read-only, like the review Steps: it writes nothing to GitHub or the checkout", func(t *testing.T) {
		spec := Spec(implement.Commands{Install: "make deps", Test: "make test", Lint: "make lint"})
		want := []string{"Read", "Glob", "Grep", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(make test)", "Bash(make lint)"}
		if spec.Name != Name || !reflect.DeepEqual(spec.Tools, []string{"Read", "Glob", "Grep", "Bash"}) || !reflect.DeepEqual(spec.Allowed, want) {
			t.Errorf("spec = %+v", spec)
		}
	})
	t.Run("the schema's decision is the Ruling request's, every field required", func(t *testing.T) {
		var s struct {
			Properties struct {
				Decisions struct {
					Items struct {
						Required   []string                   `json:"required"`
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"items"`
				} `json:"decisions"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(schema), &s); err != nil {
			t.Fatal(err)
		}
		var props []string
		for name := range s.Properties.Decisions.Items.Properties {
			props = append(props, name)
		}
		sort.Strings(props)
		required := append([]string{}, s.Properties.Decisions.Items.Required...)
		sort.Strings(required)
		want := []string{"item", "now", "options", "scenario", "title", "wants", "why"}
		if !reflect.DeepEqual(props, want) || !reflect.DeepEqual(required, want) {
			t.Errorf("properties %v, required %v", props, required)
		}
	})
	t.Run("the runner accepts the Step, sends the items and decodes the typed decisions", func(t *testing.T) {
		spec := Spec(implement.Commands{Test: "make test"})
		fake := steptest.New(t)
		answer := Result{Decisions: []Written{{1, decision("once or always")}}}
		fake.Script(steptest.Reply{Lines: []string{
			steptest.Init("r1", "m", append([]string{"StructuredOutput"}, spec.Tools...), []string{"factory:" + Name}),
			steptest.Success(answer),
		}})
		input := Input{PR: 55, Branch: "feat/issue-20", Base: "origin/trunk", Issue: implement.Issue{Number: 20}, Items: items[:1]}
		run := (&step.Claude{Binary: fake.Path}).Run(context.Background(), spec, step.Call{Dir: t.TempDir(), Model: "m", Input: input})
		got, err := step.Decode[Result](run)
		if err != nil || !reflect.DeepEqual(got, answer) {
			t.Fatalf("result = %+v, err = %v (%s: %s)", got, err, run.Outcome, run.Detail)
		}
		if stdin := fake.Calls()[0].Stdin; !strings.Contains(stdin, `"items":[{"kind":"product-ambiguity","path":"a.go","finding":"once or always?"}]`) {
			t.Errorf("the Step was sent %s", stdin)
		}
	})
}
