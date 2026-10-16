package ruling

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
)

// pr722 are the decisions of the Ruling request prototype (Smart Smoker PR
// 722), the fixture lib/ruling.test.sh reads too.
func pr722(t *testing.T) []Decision {
	t.Helper()
	data, err := os.ReadFile("testdata/ruling-pr722.json")
	if err != nil {
		t.Fatal(err)
	}
	var decisions []Decision
	if err := json.Unmarshal(data, &decisions); err != nil {
		t.Fatal(err)
	}
	return decisions
}

// The cases carry over from lib/ruling.test.sh, "the grammar (AC 2)".
func TestParse(t *testing.T) {
	decisions := pr722(t)
	for _, reply := range []string{"1a 2B", "1A2B", "1A 2B", "  1A   2b  ", "1A\n2B"} {
		t.Run("'"+strings.ReplaceAll(reply, "\n", `\n`)+"' parses to the full Ruling 1A 2B", func(t *testing.T) {
			got := Parse(decisions, reply)
			if got.Status != Full || !reflect.DeepEqual(got.Answers, map[int]string{1: "A", 2: "B"}) || got.Ruling != "1A 2B" {
				t.Errorf("got %+v", got)
			}
		})
	}
	t.Run("'1A' alone is partial, naming decision 2 as missing", func(t *testing.T) {
		got := Parse(decisions, "1A")
		if got.Status != Partial || !reflect.DeepEqual(got.Answers, map[int]string{1: "A"}) || !reflect.DeepEqual(got.Missing, []int{2}) {
			t.Errorf("got %+v", got)
		}
	})
	for _, reply := range []string{"I agree, resolve it", "1Z", "", "3A", "1A 2B done", "1A 1B", "A1"} {
		t.Run("'"+reply+"' is invalid and applies nothing", func(t *testing.T) {
			got := Parse(decisions, reply)
			if got.Status != Invalid || len(got.Answers) != 0 || got.Reason == "" {
				t.Errorf("got %+v", got)
			}
		})
	}
	t.Run("a repeated pair with the same letter is not a contradiction", func(t *testing.T) {
		if got := Parse(decisions, "1A 1A 2B"); got.Status != Full {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("an option with no letter of its own answers to its position's", func(t *testing.T) {
		unlettered := []Decision{{Options: []Option{{Text: "keep"}, {Text: "change"}}}}
		if got := Parse(unlettered, "1b"); got.Status != Full || got.Ruling != "1B" {
			t.Errorf("got %+v", got)
		}
	})
}

// request is a Ruling request comment carrying decisions, as the bash
// composes it: its marker first, its decisions in a hidden block last.
func request(t *testing.T, decisions []Decision, head string) string {
	t.Helper()
	data, err := json.Marshal(decisions)
	if err != nil {
		t.Fatal(err)
	}
	return RequestMarker + " head=" + head + " decisions=2 -->\n## 🧑‍⚖️ Ruling request · 2 decisions\n\n…\n" +
		DecisionsMarker + " " + base64.StdEncoding.EncodeToString(data) + " -->"
}

// comments are comments by the human, ids from 1, in order.
func comments(bodies ...string) []github.Comment {
	list := make([]github.Comment, len(bodies))
	for i, body := range bodies {
		list[i] = github.Comment{ID: int64(i + 1), Author: "the-human", Body: body}
	}
	return list
}

// by re-attributes the comments with the given ids to login.
func by(list []github.Comment, login string, ids ...int64) []github.Comment {
	for i := range list {
		for _, id := range ids {
			if list[i].ID == id {
				list[i].Author = login
			}
		}
	}
	return list
}

// The cases carry over from lib/ruling.test.sh, "ruling_pending reads the
// open request, its decisions and the human's reply". The machine user's
// login is acme-bot.
func TestRead(t *testing.T) {
	decisions := pr722(t)
	req := request(t, decisions, "2e204022")
	read := func(t *testing.T, list []github.Comment) Pending {
		t.Helper()
		pending, err := Read(list, "acme-bot")
		if err != nil {
			t.Fatal(err)
		}
		return pending
	}

	t.Run("an unanswered request: request id/head/decisions back, reply null", func(t *testing.T) {
		got := read(t, comments("<!-- pr-review-done reviewed=x fixes=none -->\n🤖 pr-review: 0 findings", req))
		if got.Request == nil || got.Request.Comment != 2 || got.Request.Head != "2e204022" || len(got.Request.Decisions) != 2 || got.Reply != nil || got.Replied() {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("a full reply after the request is parsed against the request's own decisions", func(t *testing.T) {
		got := read(t, comments(req, "1a 2B"))
		if got.Reply == nil || got.Reply.Comment != 2 || got.Reply.Status != Full || got.Reply.Ruling != "1A 2B" || got.Reply.Nudged || !got.Replied() {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("an invalid reply already nudged: status invalid, nudged true (no second nudge is owed)", func(t *testing.T) {
		got := read(t, comments(req, "I agree, resolve it", NudgeMarker+"\nnot a Ruling"))
		if got.Reply == nil || got.Reply.Status != Invalid || !got.Reply.Nudged || got.Replied() {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("a second non-Ruling after the nudge: the latest reply is parsed, and nudged stays true — one nudge per request", func(t *testing.T) {
		got := read(t, comments(req, "I agree, resolve it", NudgeMarker+"\nnot a Ruling", "still not reading the instructions"))
		if got.Reply == nil || got.Reply.Comment != 4 || got.Reply.Status != Invalid || !got.Reply.Nudged {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("a Ruling after the nudge is read as the reply", func(t *testing.T) {
		got := read(t, comments(req, "I agree, resolve it", NudgeMarker+"\nnot a Ruling", "1A 2B"))
		if got.Reply == nil || got.Reply.Status != Full || got.Reply.Ruling != "1A 2B" {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("an applied comment after the request closes it: nothing pending", func(t *testing.T) {
		got := read(t, comments(req, "1A", AppliedMarker+" head=abc ruling=\"1A\" -->\n## ✅ Ruling applied"))
		if got.Request != nil || got.Reply != nil {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("the machine user's own unmarked comments (a round heading, a pr-reconcile notice, any plain text) are told by login — never a reply", func(t *testing.T) {
		got := read(t, by(comments(req, "### Manual verification — round 2/3\n\n- [x] item", "pr-reconcile: automatic rebase failed at now. Human rebase required.", "a plain-text notice from a skill nobody listed"), "acme-bot", 2, 3, 4))
		if got.Request == nil || got.Request.Comment != 1 || got.Reply != nil {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("free text by the machine user's login is never a reply", func(t *testing.T) {
		if got := read(t, by(comments(req, "looks fine to me"), "acme-bot", 2)); got.Reply != nil {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("the same comment by another login is the human's (invalid) reply", func(t *testing.T) {
		got, _ := Read(by(comments(req, "looks fine to me"), "acme-bot", 2), "someone-else")
		if got.Reply == nil || got.Reply.Status != Invalid {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("a shared account (the human answers under the machine user's login): a comment that is nothing but a valid Ruling is the human's, the Daemon's own plain text around it is still not", func(t *testing.T) {
		got := read(t, by(comments(req, "### Manual verification — round 2/3\n\n- [x] item", " 1a 2B ", "pr-reconcile: rebased, CI green"), "acme-bot", 2, 3, 4))
		if got.Reply == nil || got.Reply.Comment != 3 || got.Reply.Status != Full || got.Reply.Ruling != "1A 2B" || got.Reply.Nudged {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("under the machine user's login only a VALID Ruling counts: an unknown letter, an unknown decision and pairs with free text stay the Daemon's own (partial 1A, reply id 2)", func(t *testing.T) {
		got := read(t, by(comments(req, "1A", "1Z", "9A", "2B and ship it"), "acme-bot", 2, 3, 4, 5))
		if got.Reply == nil || got.Reply.Comment != 2 || got.Reply.Status != Partial || !reflect.DeepEqual(got.Reply.Answers, map[int]string{1: "A"}) {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("a Ruling under the machine user's login and free text under another login read together, in order", func(t *testing.T) {
		got := read(t, by(comments(req, "1A", "lets go with B for the second"), "acme-bot", 2))
		if got.Reply == nil || got.Reply.Comment != 3 || got.Reply.Status != Partial || !reflect.DeepEqual(got.Reply.Answers, map[int]string{1: "A"}) {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("no login anywhere: an error with a reason, never a guess by first line", func(t *testing.T) {
		if _, err := Read(comments(req, "looks fine to me"), ""); err == nil {
			t.Error("no error")
		}
	})
	t.Run("a partial answer split over two comments (1A, then 2B) is read as the one full Ruling 1A 2B", func(t *testing.T) {
		got := read(t, comments(req, "1A", "2B"))
		if got.Reply == nil || got.Reply.Comment != 3 || got.Reply.Status != Full || len(got.Reply.Missing) != 0 || got.Reply.Ruling != "1A 2B" {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("an earlier Ruling stands when a later comment is not one: partial {1:A}, missing [2], not invalid", func(t *testing.T) {
		got := read(t, comments(req, "1A", "actually, 1C"))
		if got.Reply == nil || got.Reply.Status != Partial || !reflect.DeepEqual(got.Reply.Answers, map[int]string{1: "A"}) || !reflect.DeepEqual(got.Reply.Missing, []int{2}) {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("a later comment's letter for the same decision replaces the earlier one", func(t *testing.T) {
		got := read(t, comments(req, "1A", "1C"))
		if got.Reply == nil || got.Reply.Status != Partial || !reflect.DeepEqual(got.Reply.Answers, map[int]string{1: "C"}) {
			t.Errorf("got %+v", got.Reply)
		}
	})
	t.Run("a human comment BEFORE the request is not a reply to it", func(t *testing.T) {
		got := read(t, comments("1A 2B", req))
		if got.Request == nil || got.Request.Comment != 2 || got.Reply != nil {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("no request at all: both null", func(t *testing.T) {
		if got := read(t, nil); got.Request != nil || got.Reply != nil {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("a request whose decisions block cannot be read is an error, not a Ruling", func(t *testing.T) {
		if _, err := Read(comments(RequestMarker+" head=x decisions=1 -->\nno block", "1A"), "acme-bot"); err == nil {
			t.Error("no error")
		}
	})
}

func TestOwed(t *testing.T) {
	request := &Request{Comment: 1}
	for _, c := range []struct {
		name    string
		pending Pending
		want    bool
	}{
		{"no request", Pending{}, false},
		{"a request with no reply waits", Pending{Request: request}, false},
		{"a full Ruling is owed its applying", Pending{Request: request, Reply: &Reply{Parsed: Parsed{Status: Full}}}, true},
		{"a full Ruling after a nudge is owed too", Pending{Request: request, Reply: &Reply{Parsed: Parsed{Status: Full}, Nudged: true}}, true},
		{"a partial reply not yet nudged is owed its nudge", Pending{Request: request, Reply: &Reply{Parsed: Parsed{Status: Partial}}}, true},
		{"an invalid reply not yet nudged is owed its nudge", Pending{Request: request, Reply: &Reply{Parsed: Parsed{Status: Invalid}}}, true},
		{"a partial reply nudged waits on the human", Pending{Request: request, Reply: &Reply{Parsed: Parsed{Status: Partial}, Nudged: true}}, false},
		{"an invalid reply nudged waits on the human", Pending{Request: request, Reply: &Reply{Parsed: Parsed{Status: Invalid}, Nudged: true}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.pending.Owed(); got != c.want {
				t.Errorf("Owed() = %v", got)
			}
		})
	}
}
