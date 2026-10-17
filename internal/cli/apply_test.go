package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/state"
)

// waitingOnRuling makes PR #55 wait on a Ruling request of two decisions:
// one from the review's thread, RT_1, and one from a human's thread, whose
// option B changes code. The human then replies.
func (f *fireFixture) waitingOnRuling(t *testing.T, replies ...string) (human *reviewThread) {
	t.Helper()
	f.reviewedWithThread()
	human = f.humanThread("say hi")
	decisions := []ruling.Decision{*answerable("Greet how?"), *answerable("Hi or hello?")}
	decisions[0].Thread = []byte(`{"threadId":"RT_1","commentDatabaseId":9000,"path":"greeting.txt","line":1}`)
	decisions[1].Thread = []byte(fmt.Sprintf(`{"threadId":%q,"commentDatabaseId":%v,"path":"greeting.txt","line":1}`, human.id, human.comments[0]["databaseId"]))
	request, err := ruling.Compose(decisions, f.remoteRef("refs/heads/feat/issue-20")[:7], "CI green", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.pr.comments = append(f.pr.comments, request)
	f.reply(replies...)
	f.pr.labels = []string{"AFK:ruling"}
	return human
}

// reply posts comments on PR #55 as the human.
func (f *fireFixture) reply(bodies ...string) {
	if f.pr.humans == nil {
		f.pr.humans = map[int]bool{}
	}
	for _, body := range bodies {
		f.pr.humans[len(f.pr.comments)] = true
		f.pr.comments = append(f.pr.comments, body)
	}
}

// applied is PR #55's applied comment; empty when none was posted.
func (f *fireFixture) applied() string {
	for _, c := range f.pr.comments {
		if strings.HasPrefix(c, ruling.AppliedMarker) {
			return c
		}
	}
	return ""
}

// nudges counts PR #55's nudges.
func (f *fireFixture) nudges() int {
	n := 0
	for _, c := range f.pr.comments {
		if strings.HasPrefix(c, ruling.NudgeMarker) {
			n++
		}
	}
	return n
}

// Applying a Ruling: the reconcile Lane takes the PR whose human replied.
// Ported from lib/ruling.test.sh, "the applied comment removes AFK:ruling":
// "posted, then the label dropped" is behaviour 1's last check.
func TestFireApplyRuling(t *testing.T) {
	t.Run("1A 2B is applied and the label removed (behaviour 1)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		human := f.waitingOnRuling(t, "1a 2B")
		f.fixer.Script(answers("echo Hi > greeting.txt"))
		f.mustFire(0)
		head := f.remoteRef("refs/heads/feat/issue-20")[:7]
		// The Fix was briefed with the chosen option that changes code, and
		// nothing else.
		calls := f.fixer.Calls()
		if len(calls) != 1 || !strings.Contains(calls[0].Stdin, `"ruling":[{"decision":2,"title":"Hi or hello?","scenario":"a user greets","now":"hello","chosen":"say Hi","cost":"a fix","path":"greeting.txt","line":1}]`) || strings.Contains(calls[0].Stdin, "keep hello") {
			t.Fatalf("fix calls %d: %v", len(calls), calls)
		}
		bot := f.reviewThreads[0]
		if !bot.resolved || bot.lastBody() != "<!-- auto-agent:ruling -->\nRuling 1A: keep hello. Resolving, no change." {
			t.Errorf("RT_1 %+v", bot)
		}
		if !human.resolved || human.lastBody() != "<!-- auto-agent:ruling -->\nRuling 2B: say Hi. Fixed in `"+head+"`; resolving." {
			t.Errorf("the human's thread %+v", human)
		}
		applied := f.applied()
		for _, want := range []string{`<!-- auto-agent:ruling-applied head=` + head + ` ruling="1A 2B" -->`, "1. **A** — keep hello. Thread on `greeting.txt:1` resolved, no change.", "2. **B** — say Hi. Fixed in `" + head + "`; thread on `greeting.txt:1` resolved."} {
			if !strings.Contains(applied, want) {
				t.Errorf("the applied comment lacks %q:\n%s", want, applied)
			}
		}
		if len(f.pr.labels) != 0 {
			t.Errorf("labels %v", f.pr.labels)
		}
		// The label came off after the comment, and the lock was held for
		// the Fix and released.
		writes := f.writes()
		if !strings.HasSuffix(writes, "POST "+reviewedPath+"\nDELETE /repos/acme/widgets/issues/55/labels/AFK:ruling") || !strings.Contains(writes, "POST "+lockPath) || !strings.Contains(writes, "DELETE "+unlockPath) {
			t.Errorf("GitHub received\n%s", writes)
		}
		record := f.records()[len(f.records())-1]
		if a, _ := record["applied"].(map[string]any); record["reconcile"] != "ruling" || record["phase"] != "done" || a["ruling"] != "1A 2B" || a["status"] != "full" || fmt.Sprint(a["fixed"]) != "[2]" {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a partial reply is nudged once, and the PR keeps waiting (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.waitingOnRuling(t, "1A")
		f.mustFire(0)
		if f.nudges() != 1 || !strings.Contains(f.pr.comments[len(f.pr.comments)-1], "(no letter for decision 2)") || !slices.Contains(f.pr.labels, "AFK:ruling") || len(f.fixer.Calls()) != 0 {
			t.Errorf("comments %v, labels %v", f.pr.comments, f.pr.labels)
		}
		// Never two for the same reply.
		f.mustFire(0)
		if f.nudges() != 1 || f.applied() != "" {
			t.Errorf("%d nudges", f.nudges())
		}
		// The rest of the Ruling completes it.
		f.reply("2A")
		f.mustFire(0)
		if f.applied() == "" || len(f.pr.labels) != 0 {
			t.Errorf("not applied: labels %v", f.pr.labels)
		}
	})
	t.Run("an invalid reply is nudged once, and nothing is applied", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.waitingOnRuling(t, "I agree, resolve it")
		f.mustFire(0)
		f.mustFire(0)
		// The Fire after the nudge owes the PR its Verification round, and
		// runs it; the reply is not read again.
		nudge := slices.IndexFunc(f.pr.comments, func(c string) bool { return strings.Contains(c, "free text is not read") })
		if f.nudges() != 1 || nudge < 0 || !state.IsVerifyRound(f.pr.comments[len(f.pr.comments)-1]) || f.reviewThreads[0].resolved || f.applied() != "" {
			t.Errorf("comments %v", f.pr.comments)
		}
	})
	t.Run("a Ruling with no code change resolves without a Fix (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		human := f.waitingOnRuling(t, "1A 2A")
		before := f.remoteRef("refs/heads/feat/issue-20")
		f.mustFire(0)
		if len(f.fixer.Calls()) != 0 || f.remoteRef("refs/heads/feat/issue-20") != before || !f.reviewThreads[0].resolved || !human.resolved || len(f.pr.labels) != 0 {
			t.Errorf("fix calls %d, labels %v", len(f.fixer.Calls()), f.pr.labels)
		}
		if !strings.Contains(f.applied(), "Verification was not re-run on `"+before[:7]+"`") {
			t.Errorf("applied\n%s", f.applied())
		}
		// No Fix, so no lock: the lock was taken once, by the first Fire.
		if strings.Count(f.writes(), "POST "+lockPath) != 1 {
			t.Errorf("GitHub received\n%s", f.writes())
		}
	})
	t.Run("a Fix that changes nothing has not applied the Ruling: a failed attempt, nothing posted", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		human := f.waitingOnRuling(t, "1A 2B")
		f.fixer.Script(answers("true"))
		f.mustFire(1)
		if human.resolved || f.applied() != "" || !slices.Contains(f.pr.labels, "AFK:ruling") || !strings.Contains(f.records()[len(f.records())-1]["error"].(string), "the Fix changed nothing to apply the Ruling") {
			t.Errorf("labels %v, record %v", f.pr.labels, f.records()[len(f.records())-1])
		}
	})
	t.Run("a Ruling an earlier Fire applied part way is finished: no thread gets a second reply", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		human := f.waitingOnRuling(t, "1A 2B")
		f.fixer.Script(answers("echo Hi > greeting.txt"))
		// The applied comment is refused once: the Fire is cut off after the
		// push, the replies and the resolves.
		posts := 0
		f.gh.HandleFunc("POST", "/repos/acme/widgets/issues/55/comments", func(r githubtest.Request) githubtest.Reply {
			if posts++; posts == 1 {
				return githubtest.JSON(502, map[string]any{"message": "bad gateway"})
			}
			var sent struct{ Body string }
			json.Unmarshal([]byte(r.Body), &sent)
			f.pr.comments = append(f.pr.comments, sent.Body)
			return githubtest.JSON(201, map[string]any{"id": 1})
		})
		f.mustFire(1)
		replies := len(human.comments)
		// The next Fire's Fix finds the Ruling on the branch already.
		f.fixer.Script(answers("true"))
		f.mustFire(0)
		if len(human.comments) != replies || f.applied() == "" || len(f.pr.labels) != 0 || !strings.Contains(f.applied(), "Fixed in `"+f.remoteRef("refs/heads/feat/issue-20")[:7]+"`") {
			t.Errorf("replies %d then %d, labels %v, applied\n%s", replies, len(human.comments), f.pr.labels, f.applied())
		}
	})
	t.Run("a Ruling the ticket's attempts could not apply is a human's: AFK:ruling off, the Lane takes the PR no more", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.waitingOnRuling(t, "1A 2B")
		f.fixer.Script(crashes)
		for range 3 {
			f.fire()
		}
		if f.labelled() != "AFK:failed" || slices.Contains(f.pr.labels, "AFK:ruling") || !strings.Contains(f.pr.comments[len(f.pr.comments)-1], "The Ruling could not be applied") {
			t.Errorf("ticket %q, PR labels %v", f.labelled(), f.pr.labels)
		}
		calls := len(f.fixer.Calls())
		f.mustFire(0)
		if len(f.fixer.Calls()) != calls {
			t.Error("the PR was taken again")
		}
	})
	t.Run("a Fix that fails leaves the Ruling unapplied: threads open, AFK:ruling on", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		human := f.waitingOnRuling(t, "1A 2B")
		f.fixer.Script(crashes)
		f.mustFire(1)
		if human.resolved || f.reviewThreads[0].resolved || f.applied() != "" || !slices.Contains(f.pr.labels, "AFK:ruling") {
			t.Errorf("labels %v", f.pr.labels)
		}
	})
}
