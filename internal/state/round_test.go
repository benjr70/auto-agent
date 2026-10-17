package state

import (
	"testing"

	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/github"
)

// A round's verdict, read back from the PR's comments: what the Daemon wrote
// is what it reads.
func TestLastRound(t *testing.T) {
	results := []checklist.Result{
		{Item: checklist.Item{Text: "a"}, Verdict: checklist.Pass},
		{Item: checklist.Item{Text: "b"}, Verdict: checklist.Defer, Deferral: "the panel"},
		{Item: checklist.Item{Text: "c"}, Verdict: checklist.Fail},
	}
	mine := func(body string) github.Comment { return github.Comment{Body: body, Author: "machine-user"} }
	cases := []struct {
		name     string
		comments []github.Comment
		want     Round
		ok       bool
	}{
		{"no round yet", []github.Comment{mine("<!-- pr-review-done -->")}, Round{}, false},
		{"a round's tally and number", []github.Comment{mine(checklist.Comment(checklist.Round{N: 2, Max: 3, Results: results}))},
			Round{N: 2, Max: 3, Pass: 1, Total: 3, Deferred: 1, Fail: 1, Owed: 1}, true},
		{"the last round counts", []github.Comment{
			mine(checklist.Comment(checklist.Round{N: 1, Max: 3, Results: results})),
			mine(checklist.Comment(checklist.Round{N: 2, Max: 3, Results: results[:2]})),
		}, Round{N: 2, Max: 3, Pass: 1, Total: 2, Deferred: 1}, true},
		{"an infrastructure error is a round with no verdict", []github.Comment{mine(checklist.InfraComment(1, 3, "up exited 4", ""))},
			Round{N: 1, Max: 3, Infra: true}, true},
		{"the Bootstrap state's comment is no round, but it is the PR's answer", []github.Comment{mine(checklist.BootstrapComment())},
			Round{Bootstrap: true}, true},
		{"anyone else's comment is not the Daemon's round", []github.Comment{{Body: checklist.Comment(checklist.Round{N: 1, Max: 3}), Author: "a-human"}}, Round{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := LastRound(c.comments, "Machine-User")
			if got != c.want || ok != c.ok {
				t.Errorf("LastRound = %+v, %v; want %+v, %v", got, ok, c.want, c.ok)
			}
		})
	}
	t.Run("only a round with no FAIL, that ran, is done", func(t *testing.T) {
		for round, want := range map[Round]bool{
			{N: 1, Pass: 2, Total: 3, Deferred: 1}: true,
			{N: 1, Pass: 1, Total: 2, Fail: 1}:     false,
			{N: 1, Infra: true}:                    false,
			{Bootstrap: true}:                      false,
		} {
			if round.Done() != want {
				t.Errorf("%+v.Done() = %v", round, !want)
			}
		}
	})
	t.Run("every comment a round posts is one the PR Triage reads as a round", func(t *testing.T) {
		for _, body := range []string{checklist.Comment(checklist.Round{N: 1, Max: 3}), checklist.InfraComment(1, 3, "x", ""), checklist.BootstrapComment()} {
			if !IsVerifyRound(body) {
				t.Errorf("the triage misses\n%s", body)
			}
		}
	})
	t.Run("a round that owes a fix round did not finish the verification; the round after its fix does", func(t *testing.T) {
		owing := mine(checklist.Comment(checklist.Round{N: 1, Max: 3, Results: results}))
		passed := mine(checklist.Comment(checklist.Round{N: 2, Max: 3, Results: results[:2]}))
		if r, _ := LastRound([]github.Comment{owing}, "machine-user"); r.Owed != 1 || r.Finished() {
			t.Errorf("round 1 read as %+v", r)
		}
		for name, c := range map[string]struct {
			comments []github.Comment
			want     bool
		}{
			"no round":                        {nil, false},
			"a round owing a fix":             {[]github.Comment{owing}, false},
			"the round after the fix":         {[]github.Comment{owing, passed}, true},
			"an infrastructure error":         {[]github.Comment{mine(checklist.InfraComment(1, 3, "x", ""))}, true},
			"a round from before fix rounds":  {[]github.Comment{mine("### Manual verification — round 1/3\n\n**Result: 0/1 PASS, 0 deferred, 1 FAIL.**")}, true},
			"anyone else's round is not ours": {[]github.Comment{{Body: passed.Body, Author: "a-human"}}, false},
		} {
			if got := VerifyFinished(c.comments, "Machine-User"); got != c.want {
				t.Errorf("%s: VerifyFinished = %v", name, got)
			}
		}
	})
}
