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
		{"a failed-verification brief naming its item", FailedVerification([]Verification{{Item: "the smoke", Detail: "1/2 failed", Log: "GET /api 500"}}, nil), ""},
		{"a failed-verification brief demanding a spec only", FailedVerification(nil, []DemandedSpec{{ID: 1, Item: "TLS in prod", Spec: "curl -sI prod"}}), ""},
		{"a failed-verification brief naming no item", Brief{Kind: KindFailedVerification}, "names no item"},
		{"a failed-verification brief with a blank item", FailedVerification([]Verification{{Item: " "}}, nil), "names no item"},
		{"a demanded spec out of place", FailedVerification(nil, []DemandedSpec{{ID: 2, Item: "TLS in prod", Spec: "curl"}}), "demanded spec 2 is not item 1"},
		{"a review-threads brief with a thread", ReviewThreads([]Thread{{ID: "RT_1", Authored: "bot"}}), ""},
		{"a review-threads brief with no thread", ReviewThreads(nil), "has no thread"},
		{"a review-threads brief carrying a Ruling", Ruling([]Ruled{{Decision: 2, Title: "t", Chosen: "write it"}}), ""},
		{"a ci-failure brief naming its failed checks", CIFailure([]FailedCheck{{Name: "unit", Log: "FAIL"}}), ""},
		{"a ci-failure brief naming none", CIFailure(nil), "names no failed check"},
		{"a rebase-conflict brief naming its files", RebaseConflict([]string{"a.go"}), ""},
		{"a rebase-conflict brief naming none", RebaseConflict(nil), "names no conflicted file"},
		{"a kind this build does not define", Brief{Kind: "deploy"}, `"deploy" is not a kind`},
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
	for _, rule := range []string{"Never commit, push, stage", "Address every item", "never weaken a test", "`change-request`", "`failed-verification`", "`review-threads`", "Read each thread whole, human replies", "may not be disputed", "`no-change`", "It is binding", "Apply exactly the chosen option", "`ci-failure`", "never the check", "`flake`", "`rebase-conflict`", "never continue, abort or skip it", "`specs`", "`post_deploy`", "`dispute`", "never edit an acceptance criterion", "name its path in the check"} {
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
		{"a flake named for another kind of brief is rejected", Result{Summary: "s", CommitMessage: "fix: x", Flake: "runner died"}, "a flake was named"},
		{"threads answered for another kind of brief are rejected", Result{Summary: "s", CommitMessage: "fix: x", Threads: []Answer{{Thread: "RT_1", Action: Fixed, Reply: "r"}}}, "threads were answered"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check(ChangeRequest([]string{"x"}), []string{"a"})
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCheckThreads(t *testing.T) {
	brief := ReviewThreads([]Thread{{ID: "RT_1", Authored: "bot"}, {ID: "RT_2", Authored: "human", Ruling: "keep it as is"}})
	answers := func(a ...Answer) Result { return Result{Summary: "s", CommitMessage: "fix(a): x", Threads: a} }
	fixed1, ruled2 := Answer{Thread: "RT_1", Action: Fixed, Reply: "guarded the call"}, Answer{Thread: "RT_2", Action: NoChange, Reply: "kept as ruled"}
	for _, tt := range []struct {
		name   string
		result Result
		want   string
	}{
		{"every thread answered once passes", answers(fixed1, ruled2), ""},
		{"a dispute on an unruled thread passes", answers(Answer{Thread: "RT_1", Action: Disputed, Reply: "the guard is upstream"}, ruled2), ""},
		{"a thread left unanswered fails the result check", answers(fixed1), "RT_2 is not answered"},
		{"a thread answered twice fails", answers(fixed1, fixed1, ruled2), "RT_1 is answered twice"},
		{"a thread not in the brief fails", answers(fixed1, ruled2, Answer{Thread: "RT_9", Action: Fixed, Reply: "r"}), "RT_9 is answered, but is not in the brief"},
		{"an empty reply fails", answers(Answer{Thread: "RT_1", Action: Fixed, Reply: " "}, ruled2), "RT_1 has no reply"},
		{"a dispute on a ruled thread is refused", answers(fixed1, Answer{Thread: "RT_2", Action: Disputed, Reply: "no"}), "the human ruled it"},
		{"no-change on an unruled thread is refused", answers(Answer{Thread: "RT_1", Action: NoChange, Reply: "r"}, ruled2), "only a thread the human ruled"},
		{"an unknown action is refused", answers(Answer{Thread: "RT_1", Action: "skipped", Reply: "r"}, ruled2), `answered "skipped"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check(brief, []string{"a"})
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

// The answer to a failed-verification brief: each demanded spec answered
// once with a check that fits in its tag, or a dispute and nothing else.
func TestCheckVerification(t *testing.T) {
	brief := FailedVerification([]Verification{{Item: "the greeting says hello", Detail: "`./greet` printed hi"}},
		[]DemandedSpec{{ID: 1, Item: "TLS terminates in prod", Spec: "GET https://prod/ is 200 over TLS"}})
	answer := func(dispute string, p ...PostDeploy) Result {
		return Result{Summary: "s", CommitMessage: "fix(a): say hello", PostDeploy: p, Dispute: dispute}
	}
	check := PostDeploy{Spec: 1, Check: "scripts/prod-tls.sh"}
	for _, tt := range []struct {
		name   string
		result Result
		want   string
	}{
		{"every spec answered once passes", answer("", check), ""},
		{"a dispute with no post-deploy check passes", answer("the criterion asks for hi, not hello"), ""},
		{"a spec left unanswered is refused", answer(""), `spec 1 ("TLS terminates in prod") is not answered`},
		{"a spec answered twice is refused", answer("", check, check), "spec 1 is answered twice"},
		{"a check answering no spec is refused", answer("", check, PostDeploy{Spec: 2, Check: "x"}), "answers no spec"},
		{"a check that would close its tag is refused", answer("", PostDeploy{Spec: 1, Check: "a --> b"}), "fits in a post-deploy tag"},
		{"a check of two lines is refused", answer("", PostDeploy{Spec: 1, Check: "a\nb"}), "fits in a post-deploy tag"},
		{"a dispute that also answers a spec is refused", answer("wrong criterion", check), "a dispute changes nothing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check(brief, []string{"a"})
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
	t.Run("a dispute or a post-deploy check for another kind of brief is refused", func(t *testing.T) {
		for _, r := range []Result{answer("no"), answer("", check)} {
			if err := r.Check(ChangeRequest([]string{"x"}), []string{"a"}); err == nil || !strings.Contains(err.Error(), "for a change-request brief") {
				t.Errorf("Check = %v", err)
			}
		}
	})
}
