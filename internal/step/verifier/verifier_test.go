package verifier

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/checklist"
)

// flat is the prompt with its line breaks as spaces, so a rule is found
// wherever the text wraps.
func flat(prompt string) string { return strings.Join(strings.Fields(prompt), " ") }

var items = []checklist.Item{
	{Section: checklist.Manual, Text: "the health route answers"},
	{Section: checklist.Manual, Text: "TLS terminates in prod <!-- post-deploy: curl -sI https://prod/ -->", PostDeploy: "curl -sI https://prod/"},
	{Section: checklist.Human, Text: "the panel shows the reading"},
}

func answer(verdicts ...Verdict) Result { return Result{Items: verdicts, Tour: []Shot{}} }

// pass passes the item of that text in items, by its id.
func pass(text string) Verdict {
	return Verdict{ID: id(text), Text: text, Verdict: "PASS", Class: "local", Evidence: []string{"GET /health 200"}}
}

// id is the id the Step is told for the item of that text in items: its
// place in them, from 1; 0 for none.
func id(text string) int {
	for i, item := range items {
		if item.Text == text {
			return i + 1
		}
	}
	return 0
}

// Ported from the rules lib/runbook-check.sh holds
// plugin/agents/manual-verifier.md and plugin/skills/verify-pr/SKILL.md to,
// under their rule names, where the rule is the Step's. The rest are the
// Daemon's now, and tested where it keeps them: result-line, infra-error,
// only-tick-passed, one-comment and sandbox-degraded in internal/checklist
// and the Fire; tour-kinds, surfaces-from-config, viewport-from-config,
// boot-call, teardown, config-from-pr-head and head-flag in internal/verify
// and the Fire; tally in checklist.Round.Line, which Go counts from the
// verdicts rather than reading a line the verifier wrote. Dropped:
// deployed-round, deployed-read-only and deployed-exercised (the deployed
// round is the Deployed tier's Lane, build group 10); force-tour (the Bot PR
// Lane's, build group 10); verifier-subagent and output-discipline (a Step
// answers through its schema, not a subagent's message); evidence-sink's
// `evidence dir --pr` and checklist-protocol's `checklist parse` (CLI
// commands; the libs are internal/checklist); shots-line (the record and the
// PR body say whether a tour was posted); never-merges (a Step cannot reach
// GitHub at all).
func TestSpec(t *testing.T) {
	spec := Spec()
	prompt := flat(spec.Prompt)
	// tools, no-write: Read, Grep, Glob and Bash; no Write or Edit
	t.Run("tools, no-write: it reads and runs, and sees no tool that edits", func(t *testing.T) {
		if spec.Name != Name || !reflect.DeepEqual(spec.Tools, []string{"Read", "Glob", "Grep", "Bash"}) {
			t.Errorf("Name %q, Tools %v", spec.Name, spec.Tools)
		}
		for _, tool := range spec.Allowed {
			if tool == "Write" || tool == "Edit" {
				t.Errorf("Allowed %v", spec.Allowed)
			}
		}
	})
	t.Run("it writes nothing to GitHub: the prompt forbids it and names no write", func(t *testing.T) {
		if command := regexp.MustCompile(`\bgh \w|AFK:`).FindAllString(spec.Prompt, -1); command != nil {
			t.Errorf("the prompt names %q", command)
		}
		if !strings.Contains(prompt, "Never read or write the issue tracker or any pull request, and never tick a box") {
			t.Error("the prompt lets the Step write to GitHub")
		}
	})
	for _, rule := range []struct{ name, phrase string }{
		{"verdicts", "(`PASS`, `DEFER` or `FAIL`)"},
		{"unjustified", "An unjustified deferral is a `FAIL`, not a `DEFER`"},
		{"tour-naming", "`<surface>-NN-<slug>.png`"},
		{"viewport-not-yours", "it is not yours to choose"},
		{"display-truth", "Display truth comes only from the Host env's `DISPLAY`"},
		{"no-teardown", "Do not tear anything down"},
		{"no-install", "Install nothing"},
		{"never-fabricate", "Never fabricate a verdict"},
		{"sandbox-degraded", "A DEGRADED sandbox is a fact to state, not a blocker"},
		{"the declared Surfaces only, through the launchers' servers", "You drive only the declared Surfaces, through the servers you are given"},
	} {
		t.Run(rule.name, func(t *testing.T) {
			if !strings.Contains(prompt, rule.phrase) {
				t.Errorf("the prompt lacks %q", rule.phrase)
			}
		})
	}
}

func TestSchema(t *testing.T) {
	t.Run("the result schema names exactly the fields of the result types, all required", func(t *testing.T) {
		var s struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Items struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(Spec().Schema), &s); err != nil {
			t.Fatal(err)
		}
		for field, typ := range map[string]reflect.Type{"items": reflect.TypeFor[Verdict](), "tour": reflect.TypeFor[Shot]()} {
			var want []string
			for i := range typ.NumField() {
				want = append(want, typ.Field(i).Tag.Get("json"))
			}
			got := s.Properties[field].Items
			if !reflect.DeepEqual(got.Required, want) || len(got.Properties) != len(want) {
				t.Errorf("%s: required %v, want %v", field, got.Required, want)
			}
		}
	})
}

func TestCheck(t *testing.T) {
	deferred := Verdict{ID: 2, Text: items[1].Text, Verdict: "DEFER", Class: "deployed-env", Evidence: []string{}, SpecDemanded: "curl -sI https://prod/ gives 200"}
	hardware := Verdict{ID: 3, Text: items[2].Text, Verdict: "DEFER", Class: "hardware", Evidence: []string{}, Blocker: "a human reads the panel"}
	cases := []struct {
		name   string
		result Result
		err    string
	}{
		{"every item answered once is accepted", answer(pass(items[0].Text), deferred, hardware), ""},
		{"an item left out is refused", answer(pass(items[0].Text), deferred), `no verdict for item 3 "the panel shows the reading"`},
		{"an item answered twice is refused", answer(pass(items[0].Text), pass(items[0].Text), deferred, hardware), `item 1 "the health route answers" is answered twice`},
		{"a verdict whose text is not its item's is refused", answer(pass(items[0].Text), Verdict{ID: 2, Text: items[2].Text, Verdict: "FAIL", Class: "local", Evidence: []string{"x"}}, hardware), `item 2 is not "the panel shows the reading"`},
		{"an item nobody asked about is refused", answer(pass(items[0].Text), deferred, hardware, pass("something else")), `"something else" is not one of the items`},
		{"a PASS with no evidence is refused: concrete, or it did not happen", answer(Verdict{ID: 1, Text: items[0].Text, Verdict: "PASS", Class: "local"}, deferred, hardware), `"the health route answers" is PASS with no evidence`},
		{"a tour shot with a path is refused: it is a name in the evidence directory", Result{Items: []Verdict{pass(items[0].Text), deferred, hardware}, Tour: []Shot{{File: "../web-01-home.png"}}}, `tour shot "../web-01-home.png" is not a file name`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.result.Check(items)
			if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
				t.Errorf("Check = %v, want %q", err, c.err)
			}
		})
	}
	t.Run("two items of the same text are each answered, by id", func(t *testing.T) {
		twins := []checklist.Item{
			{Section: checklist.Manual, Text: "Settings page saves"},
			{Section: checklist.Human, Text: "Settings page saves"},
		}
		r := answer(
			Verdict{ID: 1, Text: "Settings page saves", Verdict: "PASS", Class: "local", Evidence: []string{"saved"}},
			Verdict{ID: 2, Text: "Settings page saves", Verdict: "FAIL", Class: "local", Evidence: []string{"500"}},
		)
		if err := r.Check(twins); err != nil {
			t.Fatalf("Check = %v", err)
		}
		got := r.Results(twins)
		if len(got) != 2 || got[0].Verdict != checklist.Pass || got[0].Item != twins[0] || got[1].Verdict != checklist.Fail || got[1].Item != twins[1] {
			t.Errorf("Results = %+v", got)
		}
	})
	t.Run("the Step is told each item's id, its place in the checklist from 1", func(t *testing.T) {
		told := Items(items)
		for i, item := range told {
			if item.ID != i+1 {
				t.Errorf("item %d told id %d", i+1, item.ID)
			}
		}
	})
}

// The Step's verdicts as the round counts them: an unjustified deferral is
// a fail.
func TestResults(t *testing.T) {
	cases := []struct {
		name     string
		verdict  Verdict
		want     checklist.Verdict
		class    string
		deferral string
	}{
		{"a pass is a pass", pass(items[0].Text), checklist.Pass, "local", ""},
		{"a deployed-env deferral demanding a spec is justified, and says the spec", Verdict{Verdict: "DEFER", Class: "deployed-env", SpecDemanded: "GET https://prod/ gives 200"}, checklist.Defer, "deployed-env", "needs a deployed environment; post-deploy check demanded: GET https://prod/ gives 200"},
		{"a hardware deferral naming its blocker is justified", Verdict{Verdict: "DEFER", Class: "hardware", Blocker: "the probe must be attached"}, checklist.Defer, "hardware", "needs hardware: the probe must be attached"},
		{"unjustified: a deferral of a local item is a fail", Verdict{Verdict: "DEFER", Class: "local", Evidence: []string{"no time"}}, checklist.Fail, "unjustified", ""},
		{"unjustified: a deployed-env deferral with no spec demanded is a fail", Verdict{Verdict: "DEFER", Class: "deployed-env"}, checklist.Fail, "unjustified", ""},
		{"unjustified: a hardware deferral with no blocker is a fail", Verdict{Verdict: "DEFER", Class: "hardware", Blocker: "  "}, checklist.Fail, "unjustified", ""},
		{"a fail is a fail", Verdict{Verdict: "FAIL", Class: "local", Evidence: []string{"500"}}, checklist.Fail, "local", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.verdict.ID, c.verdict.Text = 1, items[0].Text
			got := Result{Items: []Verdict{c.verdict}}.Results(items[:1])
			if len(got) != 1 || got[0].Verdict != c.want || got[0].Classification != c.class || got[0].Deferral != c.deferral || got[0].Item != items[0] {
				t.Fatalf("Results = %+v", got)
			}
			if c.class == "unjustified" && !strings.HasPrefix(got[0].Evidence[0], "unjustified deferral: ") {
				t.Errorf("evidence %v does not say why it is a fail", got[0].Evidence)
			}
		})
	}
	t.Run("the results come in the checklist's order, whatever the Step's", func(t *testing.T) {
		got := Result{Items: []Verdict{{ID: 2, Text: items[1].Text, Verdict: "FAIL"}, pass(items[0].Text)}}.Results(items[:2])
		if got[0].Item.Text != items[0].Text || got[1].Item.Text != items[1].Text {
			t.Errorf("Results = %+v", got)
		}
	})
}

func TestTourLines(t *testing.T) {
	r := Result{Tour: []Shot{{File: "web-01-home.png", Shows: "the home page"}, {File: "web-02-list.png"}}}
	if got := r.TourLines(); got != "ui-shot: web-01-home.png — the home page\nui-shot: web-02-list.png\n" {
		t.Errorf("TourLines = %q", got)
	}
	shots := checklist.Tour(r.TourLines(), "/ev")
	if len(shots) != 2 || shots[0].Caption != "web — the home page" || shots[0].Ref != "/ev/web-01-home.png" {
		t.Errorf("the tour reads back as %+v", shots)
	}
}
