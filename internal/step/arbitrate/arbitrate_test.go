package arbitrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

var disputes = []Dispute{{Thread: "T1", Authored: Bot}, {Thread: "T2", Authored: Bot}, {Thread: "T3", Authored: Human}, {Thread: "T4", Authored: Bot}}

func given(thread, verdict, text string) Given {
	return Given{Thread: thread, Verdict: verdict, Text: text}
}

// verdicts is each verdict as "thread verdict".
func verdicts(vs []Verdict) string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Thread+" "+v.Verdict)
	}
	return strings.Join(out, ", ")
}

// Ported from lib/arbiter-verdicts.test.sh: av_parse_verdicts. The Step
// answers typed verdicts, so a verdict "line" is one entry of them.
func TestRead(t *testing.T) {
	decision := &Decision{Title: "Greet once?", Recommended: "A"}
	t.Run("fix, dismiss, ambiguity and a second dismiss are read with their text", func(t *testing.T) {
		got := Read(disputes, Result{Verdicts: []Given{
			given("T1", Fix, "move the check into useVoiceFill.tsx before the READY branch; done when the new test passes"),
			given("T2", Dismiss, "the Finding names a path the app never produces"),
			{Thread: "T3", Verdict: Ambiguity, Decision: decision},
			given("T4", Dismiss, "duplicate of T2"),
		}})
		if verdicts(got) != "T1 fix, T2 dismiss, T3 ambiguity, T4 dismiss" || got[0].Text != "move the check into useVoiceFill.tsx before the READY branch; done when the new test passes" || got[2].Decision != decision {
			t.Errorf("got %+v", got)
		}
		t.Run("the output carries authored through, so the caller can route the verdict", func(t *testing.T) {
			if got[0].Authored != Bot || got[2].Authored != Human || got[3].Authored != Bot {
				t.Errorf("got %+v", got)
			}
		})
	})
	t.Run("a doubled threadId, a verdict word outside the three and a missing line are each unruled with a reason", func(t *testing.T) {
		got := Read(disputes, Result{Verdicts: []Given{
			given("T1", Fix, "a"), given("T1", Dismiss, "b"), given("T2", "reject", "c"), {Thread: "T3", Verdict: Ambiguity}, given("T9", Dismiss, "not a thread we gave"),
		}})
		if verdicts(got) != "T1 unruled, T2 unruled, T3 ambiguity, T4 unruled" || got[0].Reason != "verdict doubled" || !strings.Contains(got[1].Reason, "reject") || got[3].Reason != "no verdict" {
			t.Errorf("got %+v", got)
		}
		t.Run("a verdict for a thread the Arbiter was not given is ignored", func(t *testing.T) {
			if len(got) != 4 {
				t.Errorf("got %d verdicts", len(got))
			}
		})
	})
	t.Run("dismiss on the human thread is refused (unruled, collected); the same word on a bot thread is a verdict", func(t *testing.T) {
		got := Read(disputes, Result{Verdicts: []Given{given("T1", Dismiss, "fine"), given("T2", Fix, "fine"), given("T3", Dismiss, "the human is wrong"), {Thread: "T4", Verdict: Ambiguity}}})
		if verdicts(got) != "T1 dismiss, T2 fix, T3 unruled, T4 ambiguity" || !strings.Contains(got[2].Reason, "human-authored") {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("fix on the human thread is refused too", func(t *testing.T) {
		if got := Read(disputes, Result{Verdicts: []Given{given("T3", Fix, "do it")}}); got[2].Verdict != Unruled {
			t.Errorf("got %+v", got[2])
		}
	})
	t.Run("an empty reply leaves every thread unruled", func(t *testing.T) {
		for _, v := range Read(disputes, Result{}) {
			if v.Verdict != Unruled {
				t.Errorf("got %+v", v)
			}
		}
	})
	t.Run("a fix or a dismissal with nothing to post is unruled, never posted empty", func(t *testing.T) {
		got := Read(disputes, Result{Verdicts: []Given{given("T1", Fix, " "), given("T2", Dismiss, "")}})
		if got[0].Verdict != Unruled || got[1].Verdict != Unruled || got[0].Reason != "fix with nothing to post" {
			t.Errorf("got %+v", got)
		}
	})
}

// fixtureDispute is one entry of the fixture: a dispute that parked an Agent
// PR for a human before the Arbiter existed, with what it is scored against.
type fixtureDispute struct {
	ID                string `json:"id"`
	Finding           string `json:"finding"`
	Dispute           string `json:"dispute"`
	RequirementQuoted string `json:"requirement_quoted"`
	SpecSilent        *bool  `json:"spec_silent_or_contradicting"`
	UserVisible       *bool  `json:"user_visible_difference"`
	Why               string `json:"why"`
	Expected          struct{ Outcome, Verdict string }
}

func loadFixture(t *testing.T) []fixtureDispute {
	t.Helper()
	data, err := os.ReadFile("testdata/arbiter-disputes.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Disputes []fixtureDispute }
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("fixture exists and parses: %v", err)
	}
	return f.Disputes
}

// fixtureInput is what the Fire sends the Arbiter for the fixture's
// disputes: each on a bot-authored thread, the requirement it quotes
// standing in for the ticket.
func fixtureInput(fixture []fixtureDispute) Input {
	in := Input{PR: 720, Branch: "feat/issue-705", Base: "origin/main", Issue: implement.Issue{Number: 705, Title: "Voice fill"}}
	var quoted []string
	for _, d := range fixture {
		in.Disputes = append(in.Disputes, Dispute{Thread: d.ID, Path: "src/app.ts", Line: 1, Authored: Bot, Finding: d.Finding, Dispute: d.Dispute})
		if d.RequirementQuoted != "" {
			quoted = append(quoted, d.RequirementQuoted)
		}
	}
	in.Issue.Body = "## Acceptance criteria\n\n" + strings.Join(quoted, "\n")
	return in
}

// allRuled says why the verdicts do not rule every dispute: a fix or a
// dismissal each, none an ambiguity nor unruled.
func allRuled(vs []Verdict) error {
	var escalated []string
	for _, v := range vs {
		if v.Verdict != Fix && v.Verdict != Dismiss {
			escalated = append(escalated, fmt.Sprintf("%s: %s — %s", v.Thread, v.Verdict, v.Reason))
		}
	}
	if len(escalated) > 0 {
		return fmt.Errorf("%d of %d not ruled: %s", len(escalated), len(vs), strings.Join(escalated, "; "))
	}
	return nil
}

// Ported from lib/arbiter-fixture.test.sh: the five technical disputes the
// Arbiter rules, every one of them.
func TestFixture(t *testing.T) {
	fixture := loadFixture(t)
	t.Run("fixture carries the five technical disputes", func(t *testing.T) {
		if len(fixture) != 5 {
			t.Errorf("got %d", len(fixture))
		}
	})
	t.Run("every dispute carries the Finding, the dispute line, the escalation scores and the expected verdict", func(t *testing.T) {
		for _, d := range fixture {
			if d.ID == "" || d.Finding == "" || d.Dispute == "" || d.SpecSilent == nil || d.UserVisible == nil ||
				(d.Expected.Outcome != "ruled" && d.Expected.Outcome != "ambiguity") || (d.Expected.Verdict != Fix && d.Expected.Verdict != Dismiss && d.Expected.Verdict != Ambiguity) {
				t.Errorf("malformed: %+v", d)
			}
		}
	})
	t.Run("dispute ids are unique", func(t *testing.T) {
		seen := map[string]bool{}
		for _, d := range fixture {
			if seen[d.ID] {
				t.Errorf("%s twice", d.ID)
			}
			seen[d.ID] = true
		}
	})
	t.Run("an entry's expected outcome is ambiguity iff both (a) and (b) are scored true, and a ruled entry's verdict is fix or dismiss", func(t *testing.T) {
		for _, d := range fixture {
			if (*d.SpecSilent && *d.UserVisible) != (d.Expected.Outcome == "ambiguity") || (d.Expected.Outcome == "ruled" && d.Expected.Verdict == Ambiguity) {
				t.Errorf("inconsistent: %s", d.ID)
			}
		}
	})
	t.Run("every one of the five is expected ruled (AC 4) — the agent run is what proves it; this pins the expectation", func(t *testing.T) {
		for _, d := range fixture {
			if d.Expected.Outcome != "ruled" {
				t.Errorf("%s expected %s", d.ID, d.Expected.Outcome)
			}
		}
	})
	input := fixtureInput(fixture)
	sent, _ := json.Marshal(input)
	t.Run("every dispute appears with its threadId and its dispute line verbatim", func(t *testing.T) {
		for i, d := range fixture {
			if got := input.Disputes[i]; got.Thread != d.ID || got.Dispute != d.Dispute || got.Finding != d.Finding || got.Authored != Bot {
				t.Errorf("sent %+v", got)
			}
		}
	})
	t.Run("the fixture's own rationale (why / expected) never reaches the prompt — the Arbiter sees the dispute line and nothing else of the arguing", func(t *testing.T) {
		for _, d := range fixture {
			if strings.Contains(string(sent), d.Why) || strings.Contains(string(sent), "expected") {
				t.Errorf("%s leaked", d.ID)
			}
		}
	})
	expected := Result{}
	for _, d := range fixture {
		expected.Verdicts = append(expected.Verdicts, given(d.ID, d.Expected.Verdict, "reason for "+d.ID))
	}
	t.Run("a reply ruling all five (the expected verdicts) passes and prints each verdict", func(t *testing.T) {
		if err := allRuled(Read(input.Disputes, expected)); err != nil {
			t.Error(err)
		}
	})
	t.Run("a reply escalating one of the five fails naming it", func(t *testing.T) {
		escalated := Result{Verdicts: append([]Given{}, expected.Verdicts...)}
		escalated.Verdicts[2] = Given{Thread: fixture[2].ID, Verdict: Ambiguity, Decision: &Decision{Title: "x"}}
		if err := allRuled(Read(input.Disputes, escalated)); err == nil || !strings.Contains(err.Error(), "pr721-button-after-finish: ambiguity") || !strings.Contains(err.Error(), "1 of 5") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a reply missing a thread fails: unruled is not ruled", func(t *testing.T) {
		short := Result{Verdicts: expected.Verdicts[1:]}
		if err := allRuled(Read(input.Disputes, short)); err == nil || !strings.Contains(err.Error(), "pr720-reviewrows-signature: unruled — no verdict") {
			t.Errorf("err = %v", err)
		}
	})
}

// TestLiveFixture runs the Arbiter on the fixture's five disputes against
// the real claude: every one is to be ruled, none escalated. CI never calls a
// model: this runs by hand when the prompt or the fixture changes, with
// AUTO_AGENT_LIVE_CLAUDE naming the model to spend on.
//
//	AUTO_AGENT_LIVE_CLAUDE=sonnet go test ./internal/step/arbitrate -run TestLiveFixture -v
func TestLiveFixture(t *testing.T) {
	model := os.Getenv("AUTO_AGENT_LIVE_CLAUDE")
	if model == "" {
		t.Skip("set AUTO_AGENT_LIVE_CLAUDE to a model to run the Arbiter on the fixture")
	}
	input := fixtureInput(loadFixture(t))
	run := (&step.Claude{}).Run(context.Background(), Spec(implement.Commands{}), step.Call{Dir: t.TempDir(), Model: model, Input: input})
	t.Logf("outcome %s (%s); turns %d; cost $%.4f; %s", run.Outcome, run.Detail, run.NumTurns, run.CostUSD, run.Elapsed)
	result, err := step.Decode[Result](run)
	if err != nil {
		t.Fatal(err)
	}
	read := Read(input.Disputes, result)
	for _, v := range read {
		t.Logf("%s: %s — %s", v.Thread, v.Verdict, v.Text)
	}
	if err := allRuled(read); err != nil {
		t.Error(err)
	}
}

func flat(prompt string) string { return strings.Join(strings.Fields(prompt), " ") }

// Ported from the rules lib/runbook-check.sh holds plugin/agents/arbiter.md
// to, under their rule names.
func TestPrompt(t *testing.T) {
	text := flat(prompt)
	for rule, phrases := range map[string][]string{
		"escalation-test":        {"silent or contradict", "a user of the Target Project would see", "both of these hold"},
		"unsure-default":         {"When unsure, rule"},
		"never-transcript":       {"never the Fix's transcript"},
		"human-never-dismissed":  {"A human-authored thread is never dismissed"},
		"no-write":               {"You never write", "Never read or write the issue tracker or any pull request"},
		"one-verdict-per-thread": {"one verdict per thread"},
		"verdicts":               {"`fix`", "`dismiss`", "`ambiguity`", "arbiter: dismissed — "},
	} {
		for _, phrase := range phrases {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s: the prompt lacks %q", rule, phrase)
			}
		}
	}
	if command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|AFK:").FindAllString(prompt, -1); command != nil {
		t.Errorf("the prompt names %q", command)
	}
	t.Run("the prompt does not point the live agent at this test fixture", func(t *testing.T) {
		if strings.Contains(prompt, "arbiter-disputes") {
			t.Error("it does")
		}
	})
}

func TestSpec(t *testing.T) {
	t.Run("tools: read-only, like the review Steps: read tools, read-only git and exactly the test and lint commands", func(t *testing.T) {
		spec := Spec(implement.Commands{Install: "make deps", Test: "make test", Lint: "make lint"})
		want := []string{"Read", "Glob", "Grep", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(make test)", "Bash(make lint)"}
		if spec.Name != Name || !reflect.DeepEqual(spec.Tools, []string{"Read", "Glob", "Grep", "Bash"}) || !reflect.DeepEqual(spec.Allowed, want) {
			t.Errorf("spec = %+v", spec)
		}
	})
	t.Run("the schema allows only the three verdicts, and a decision only as an object or null", func(t *testing.T) {
		var s struct {
			Properties struct {
				Verdicts struct {
					Items struct {
						Required   []string `json:"required"`
						Properties struct {
							Verdict struct {
								Enum []string `json:"enum"`
							} `json:"verdict"`
							Decision struct {
								Type []string `json:"type"`
							} `json:"decision"`
						} `json:"properties"`
					} `json:"items"`
				} `json:"verdicts"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(schema), &s); err != nil {
			t.Fatal(err)
		}
		items := s.Properties.Verdicts.Items
		if !reflect.DeepEqual(items.Properties.Verdict.Enum, []string{Fix, Dismiss, Ambiguity}) || !reflect.DeepEqual(items.Properties.Decision.Type, []string{"object", "null"}) ||
			!reflect.DeepEqual(items.Required, []string{"thread", "verdict", "text", "decision"}) {
			t.Errorf("items = %+v", items)
		}
	})
	t.Run("the runner accepts the Step, sends the disputes and decodes the typed verdicts", func(t *testing.T) {
		spec := Spec(implement.Commands{Test: "make test"})
		fake := steptest.New(t)
		answer := Result{Verdicts: []Given{given("T1", Dismiss, "the guard is upstream"), {Thread: "T2", Verdict: Ambiguity, Decision: &Decision{Title: "Once?", Options: []Option{{Letter: "A", Text: "once"}, {Letter: "B", Text: "always"}}, Recommended: "A"}}}}
		fake.Script(steptest.Reply{Lines: []string{
			steptest.Init("a1", "m", append([]string{"StructuredOutput"}, spec.Tools...), []string{"factory:arbitrate"}),
			steptest.Success(answer),
		}})
		input := Input{PR: 55, Branch: "feat/issue-20", Base: "origin/trunk", Issue: implement.Issue{Number: 20}, Disputes: []Dispute{{Thread: "T1", Path: "a.go", Line: 3, Authored: Bot, Finding: "f", Dispute: "d"}}}
		run := (&step.Claude{Binary: fake.Path}).Run(context.Background(), spec, step.Call{Dir: t.TempDir(), Model: "m", Input: input})
		got, err := step.Decode[Result](run)
		if err != nil || !reflect.DeepEqual(got, answer) {
			t.Fatalf("result = %+v, err = %v (%s: %s)", got, err, run.Outcome, run.Detail)
		}
		if stdin := fake.Calls()[0].Stdin; !strings.Contains(stdin, `"disputes":[{"thread":"T1","path":"a.go","line":3,"authored":"bot","finding":"f","dispute":"d"}]`) {
			t.Errorf("the Step was sent %s", stdin)
		}
	})
}
