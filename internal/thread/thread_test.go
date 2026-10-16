package thread

import (
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
)

// raw is a thread as GitHub lists it: every comment under the same login, as
// the Daemon replies to its own review under the review's login.
func raw(id string, bodies ...string) github.ReviewThread {
	t := github.ReviewThread{ID: id, Path: "src/a.ts", Line: 12}
	for i, body := range bodies {
		t.Comments = append(t.Comments, github.ThreadComment{ID: int64(9100 + i), Author: "machine-user", Body: body})
	}
	return t
}

const bot = "<!-- pr-review-bot -->\n🤖 "

// Ported from lib/thread-reconciler.test.sh: authorship by marker and the
// ruling across interleaved replies.
func TestRead(t *testing.T) {
	cases := []struct {
		name     string
		thread   github.ReviewThread
		authored string
		daemon   []bool
		ruling   string
	}{
		{"first comment carrying the pr-review marker => authored: bot", raw("RT_BOT", bot+"off-by-one"), Bot, nil, ""},
		{"first comment lacking the marker => authored: human, whatever the login", raw("RT_HUMAN", "please rename this"), Human, nil, ""},
		{"a human first comment that QUOTES the pr-review marker below its first line => authored: human",
			raw("RT_QUOTED_BOT", "re the bot's earlier note:\n> <!-- pr-review-bot -->\n> 🤖 off-by-one\nI disagree, the range is inclusive"), Human, nil, ""},
		{"a human reply after the loop's marked reply is the thread's ruling",
			raw("RT_RULED", bot+"missing null check", "<!-- auto-agent:fix -->\nfixed in abc123: guarded the call", "I agree with the fix, please resolve"), Bot, []bool{true, false}, "I agree with the fix, please resolve"},
		{"a loop reply after the human's leaves ruling null",
			raw("RT_LOOPLAST", bot+"wrong default", "keep the default as is", "<!-- auto-agent:ruling -->\nruling applied: no change"), Bot, []bool{false, true}, ""},
		{"a human thread with a second human comment and no loop reply has a null ruling (it is just more of the thread)",
			raw("RT_HUMAN_TWICE", "this leaks the handle", "and the test should cover it"), Human, []bool{false}, ""},
		{"a human reply on a bot thread BEFORE the loop ever replied is not a ruling",
			raw("RT_HUMAN_FIRST", bot+"unchecked cast", "actually this cast is fine, see the ADR"), Bot, []bool{false}, ""},
		{"a human reply that QUOTES the loop's marker below its first line is not agent, and is the ruling",
			raw("RT_QUOTED_AGENT", bot+"missing await", "<!-- auto-agent:fix -->\nfixed in def456: awaited the call", "> <!-- auto-agent:fix -->\n> fixed in def456\nno, revert this, the call is fire-and-forget"),
			Bot, []bool{true, false}, "> <!-- auto-agent:fix -->\n> fixed in def456\nno, revert this, the call is fire-and-forget"},
		{"the escalation reply is the loop's voice (agent), so the human's answer to it is the ruling",
			raw("RT_ESCALATED", bot+"unbounded retry", "<!-- auto-agent:escalate -->\ncould not auto-resolve", "cap it at 5"), Bot, []bool{true, false}, "cap it at 5"},
		{"a human answer to the Daemon's Dispute is the ruling",
			raw("RT_DISPUTED", "rename it to count", "<!-- auto-agent:dispute -->\nthe name is the glossary's", "rename it anyway"), Human, []bool{true, false}, "rename it anyway"},
		{"a thread with no replies has an empty replies list and a null ruling", raw("RT_EMPTY", bot+"x"), Bot, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Read(c.thread)
			if got.Authored != c.authored || got.ID != c.thread.ID || got.Path != "src/a.ts" || got.Line != 12 {
				t.Errorf("read %+v", got)
			}
			if len(got.Replies) != len(c.daemon) {
				t.Fatalf("replies %+v", got.Replies)
			}
			for i, want := range c.daemon {
				if got.Replies[i].Daemon != want || got.Replies[i].ID != int64(9101+i) {
					t.Errorf("reply %d = %+v, want daemon %v", i, got.Replies[i], want)
				}
			}
			ruling, ok := got.Ruling()
			if ruling != c.ruling || ok != (c.ruling != "") {
				t.Errorf("ruling = %q, %v", ruling, ok)
			}
		})
	}
	t.Run("the first comment is never a reply, and body stays the first comment's", func(t *testing.T) {
		got := Read(raw("RT_RULED", bot+"missing null check", "<!-- auto-agent:fix -->\nfixed"))
		if got.First.Body != bot+"missing null check" || got.First.ID != 9100 || len(got.Replies) != 1 {
			t.Errorf("read %+v", got)
		}
	})
}

func TestDisputed(t *testing.T) {
	for _, c := range []struct {
		name   string
		thread github.ReviewThread
		want   bool
	}{
		{"the Daemon's Dispute, unanswered, waits", raw("A", bot+"x", "<!-- auto-agent:dispute -->\nthe call is guarded upstream"), true},
		{"a Dispute the human answered is ruled, not waiting", raw("B", bot+"x", "<!-- auto-agent:dispute -->\nno", "fix it"), false},
		{"a fixed thread is not disputed", raw("C", bot+"x", "<!-- auto-agent:fix -->\nfixed in abc"), false},
		{"a thread with no reply is not disputed", raw("D", bot+"x"), false},
		{"a quoted dispute marker is a human's", raw("E", bot+"x", "> <!-- auto-agent:dispute -->"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Read(c.thread).Disputed(); got != c.want {
				t.Errorf("Disputed() = %v", got)
			}
		})
	}
}

// Ported from lib/thread-reconciler.test.sh: every reply carries one marker.
func TestBody(t *testing.T) {
	t.Run("tr_reply prefixes the marker it is given", func(t *testing.T) {
		if body, err := Body(MarkerFix, "fixed in abc123: guarded the call"); err != nil || body != "<!-- auto-agent:fix -->\nfixed in abc123: guarded the call" {
			t.Errorf("body = %q, %v", body, err)
		}
	})
	t.Run("an unknown marker is refused with exit 2, a stderr line, and no post", func(t *testing.T) {
		for _, marker := range []string{"", "<!-- auto-agent:bogus -->", "plain text"} {
			if body, err := Body(marker, "text"); err == nil || body != "" {
				t.Errorf("%q: body = %q", marker, body)
			}
		}
	})
	t.Run("the marker list is derived from the constants: every TR_MARKER_* is accepted and flagged agent", func(t *testing.T) {
		for _, marker := range []string{MarkerFix, MarkerArbiter, MarkerRuling, MarkerEscalate, MarkerDispute} {
			body, err := Body(marker, "text")
			if err != nil || !Read(raw("X", bot+"x", body)).Replies[0].Daemon || !strings.HasPrefix(marker, "<!-- auto-agent:") {
				t.Errorf("%q: %v", marker, err)
			}
		}
	})
	t.Run("the Ruling marker is the third voice the lib owns", func(t *testing.T) {
		if MarkerRuling != "<!-- auto-agent:ruling -->" || MarkerArbiter != "<!-- auto-agent:arbiter -->" {
			t.Error("the bash Thread Reconciler reads these markers until cut-over")
		}
	})
	t.Run("the Escalate marker is the voice that parks a thread for human triage", func(t *testing.T) {
		if MarkerEscalate != "<!-- auto-agent:escalate -->" {
			t.Error("the bash Thread Reconciler reads this marker until cut-over")
		}
	})
}

func TestArbiterReplies(t *testing.T) {
	fixed, _ := Body(MarkerArbiter, ArbiterFix("move the check before the READY branch"))
	dismissed, _ := Body(MarkerArbiter, ArbiterDismissed("the guard is upstream"))
	disputed, _ := Body(MarkerDispute, "disputed: the range is inclusive")
	t.Run("the Arbiter's fix is the binding instruction while it is the last word", func(t *testing.T) {
		th := Read(raw("A", bot+"x", disputed, fixed))
		if got, ok := th.Instruction(); !ok || got != "move the check before the READY branch" || th.Disputed() {
			t.Errorf("Instruction() = %q, %v", got, ok)
		}
		if _, ok := Read(raw("A", bot+"x", disputed, fixed, "do it differently")).Instruction(); ok {
			t.Error("a human reply after the Arbiter's fix is a ruling, not the instruction")
		}
		if _, ok := Read(raw("A", bot+"x", disputed, dismissed)).Instruction(); ok {
			t.Error("a dismissal is no instruction")
		}
		if dismissed != "<!-- auto-agent:arbiter -->\narbiter: dismissed — the guard is upstream" {
			t.Errorf("dismissal %q", dismissed)
		}
	})
	t.Run("the Dispute a thread waits on is its text, without the marker", func(t *testing.T) {
		if got := Read(raw("A", bot+"x", disputed)).Dispute(); got != "disputed: the range is inclusive" {
			t.Errorf("Dispute() = %q", got)
		}
		if got := Read(raw("A", bot+"x", disputed, fixed)).Dispute(); got != "" {
			t.Errorf("Dispute() = %q", got)
		}
	})
}

// Ported from lib/arbiter-verdicts.test.sh: av_next_round.
func TestNextRound(t *testing.T) {
	for _, c := range []struct {
		name                     string
		done, rounds, ruledFixes int
		next                     int
	}{
		{"cap 1, round 1, nothing ruled fix: no round remains", 1, 1, 0, 0},
		{"cap 1, round 1, one ruled fix: the ruled round is guaranteed (round 2 runs)", 1, 1, 1, 2},
		{"cap 1, after the ruled round 2: no round remains — the guarantee is one round, not a new cap", 2, 1, 0, 0},
		{"cap 3, round 1 -> 2", 1, 3, 0, 2},
		{"cap 3, round 2 -> 3", 2, 3, 0, 3},
		{"cap 3, round 3 -> cap", 3, 3, 0, 0},
		{"a Fire with one implementer round and one Arbiter run has used one round: the Arbiter and its dismissals are not inputs, so the next round is 2 under a cap of 2", 1, 2, 0, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			next, ok := NextRound(c.done, c.rounds, c.ruledFixes)
			if next != c.next || ok != (c.next != 0) {
				t.Errorf("NextRound = %d, %v", next, ok)
			}
		})
	}
}
