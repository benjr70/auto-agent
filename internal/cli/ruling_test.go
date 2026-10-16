package cli

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/step/arbitrate"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/review"
)

// rulingOf is the Ruling request in a Fire record.
func rulingOf(record map[string]any) map[string]any {
	r, _ := record["ruling"].(map[string]any)
	return r
}

// answerable is a decision the human can answer as it stands, as the
// Arbiter drafts one.
func answerable(title string) *ruling.Decision {
	return &ruling.Decision{Title: title, Scenario: "a user greets", Now: "hello", Wants: "Hi", Why: "the ticket is silent", Options: []ruling.Option{
		{Letter: "A", Text: "keep hello", Cost: "nothing", Recommended: true}, {Letter: "B", Text: "say Hi", Cost: "a fix", Fix: true}}}
}

// The Ruling request: everything on an Agent PR that needs the human, in one
// comment Go composes and posts, then AFK:ruling and the wait.
func TestFireRulingRequest(t *testing.T) {
	t.Run("ambiguities and human Disputes become one comment with numbered decisions (behaviour 1)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		human := f.humanThread("say hi, not hello")
		f.fixer.Script(answers("true",
			fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "AC 1 asks for hello"},
			fix.Answer{Thread: human.id, Action: fix.Disputed, Reply: "the ticket says hello"}))
		f.arbiter.Script(ruled(arbitrate.Given{Thread: human.id, Verdict: arbitrate.Ambiguity, Decision: answerable("hi or hello?")}))
		f.writer.Script(wrote(2))
		f.mustFire(0)
		requests := f.rulingRequests()
		if len(requests) != 1 {
			t.Fatalf("requests %v", requests)
		}
		request := requests[0]
		head := f.remoteRef("refs/heads/feat/issue-20")
		for _, want := range []string{"<!-- auto-agent:ruling-request head=" + head[:7] + " decisions=2 -->\n", "## 🧑‍⚖️ Ruling request · 2 decisions", "### 1. Question 1", "### 2. Question 2", "`1A 2A`",
			"Head `" + head[:7] + "`: CI green, manual verification not run by this build."} {
			if !strings.Contains(request, want) {
				t.Errorf("the request lacks %q:\n%s", want, request)
			}
		}
		decisions, err := ruling.Decisions(request)
		if err != nil || len(decisions) != 2 || !strings.Contains(string(decisions[0].Thread), `"threadId":"RT_1"`) || !strings.Contains(string(decisions[1].Thread), `"threadId":"`+human.id+`"`) {
			t.Errorf("decisions %+v, %v", decisions, err)
		}
		// The Step was told each item whole: what was found, the Dispute, the
		// Arbiter's reason or draft.
		stdin := f.writer.Calls()[0].Stdin
		for _, want := range []string{`"kind":"unruled","path":"greeting.txt","line":1,"authored":"bot","finding":"\u003c!-- pr-review-bot --\u003e`, `"dispute":"disputed: AC 1 asks for hello","note":"no verdict"`,
			`"kind":"ambiguity","path":"greeting.txt","line":1,"authored":"human","finding":"say hi, not hello"`, `"draft":{"title":"hi or hello?"`} {
			if !strings.Contains(stdin, want) {
				t.Errorf("the Step was not sent %s:\n%s", want, stdin)
			}
		}
		if allowed, _ := f.writer.Calls()[0].Arg("--allowedTools"); strings.Contains(allowed, "Edit") || strings.Contains(allowed, "Write") {
			t.Errorf("--allowedTools %q", allowed)
		}
		record := f.records()[len(f.records())-1]
		if r := rulingOf(record); record["phase"] != "done" || r["decisions"] != 2.0 || r["new"] != 2.0 || r["written"] != true || r["head"] != head[:7] {
			t.Errorf("record %v", record)
		}
	})
	// Ported from lib/ruling.test.sh, "posting a request applies AFK:ruling
	// and never AFK:revise-failed (AC 4)": a review with a product ambiguity.
	ambiguous := func(t *testing.T) *fireFixture {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.specReview.Script(found(review.SpecName, unsure))
		f.mustFire(0)
		return f
	}
	t.Run("AFK:ruling is added on the PR, AFK:revise-failed is not touched", func(t *testing.T) {
		f := ambiguous(t)
		if strings.Join(f.pr.labels, ",") != "AFK:ruling" || strings.Contains(f.writes(), "revise-failed") || !strings.HasPrefix(f.pr.comments[len(f.pr.comments)-1], ruling.RequestMarker) {
			t.Errorf("labels %v, GitHub received\n%s", f.pr.labels, f.writes())
		}
		// The product ambiguity was asked about on no thread.
		decisions, _ := ruling.Decisions(f.rulingRequests()[0])
		if len(decisions) != 1 || decisions[0].Thread != nil || !strings.Contains(f.writer.Calls()[0].Stdin, `"kind":"product-ambiguity","path":"greeting.txt","line":1,"finding":"greet once or every time?\n\nReadings: once: quiet; every time: noisy\n\nRequirement: silent"`) {
			t.Errorf("decisions %+v; the Step was sent %s", decisions, f.writer.Calls()[0].Stdin)
		}
	})
	t.Run("the label follows the comment, never precedes it", func(t *testing.T) {
		writes := ambiguous(t).writes()
		if comment, label := strings.LastIndex(writes, "POST "+reviewedPath), strings.LastIndex(writes, "POST /repos/acme/widgets/issues/55/labels"); comment < 0 || label < comment {
			t.Errorf("GitHub received\n%s", writes)
		}
	})
	t.Run("the Fire ends waiting: the PR is not worked again until the human replies (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.keepThreads()
		f.answer(finished)
		f.specReview.Script(found(review.SpecName, unsure))
		f.mustFire(0)
		f.prOpen = true
		before := len(f.gh.Writes())
		f.mustFire(0)
		record := f.records()[len(f.records())-1]
		// Its CI is read, and nothing else is done: no Step, no write.
		if len(f.gh.Writes()) != before || len(f.writer.Calls()) != 1 || record["kind"] != "reconcile" || record["ci"] == nil || len(stepsOf(record)) != 0 {
			t.Errorf("record %v, writes\n%s", record, f.writes())
		}
	})
	t.Run("a second request supersedes the first, carrying its decisions first (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		first, _ := ruling.Compose([]ruling.Decision{*answerable("greet once?")}, "abc1234", "CI green", 0)
		f.pr.comments = append(f.pr.comments, first)
		firstID := 5500 + len(f.pr.comments) - 1
		f.fixer.Script(answers("true", fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "AC 1 asks for hello"}))
		f.mustFire(0)
		requests := f.rulingRequests()
		if len(requests) != 2 {
			t.Fatalf("requests %v", requests)
		}
		later := requests[1]
		decisions, _ := ruling.Decisions(later)
		if !strings.Contains(later, fmt.Sprintf("This request supersedes the earlier one (comment %d)", firstID)) || len(decisions) != 2 || decisions[0].Title != "greet once?" || decisions[1].Title != "Question 1" {
			t.Errorf("the later request\n%s", later)
		}
		if r := rulingOf(f.records()[len(f.records())-1]); r["supersedes"] != float64(firstID) || r["decisions"] != 2.0 || r["new"] != 1.0 {
			t.Errorf("ruling %v", r)
		}
		// The bash, until cut-over, reads only the latest request.
		if pending, err := ruling.Read(asComments(f.pr.comments), "machine-user"); err != nil || pending.Request == nil || len(pending.Request.Decisions) != 2 {
			t.Errorf("pending %+v, %v", pending, err)
		}
	})
	t.Run("the Arbiter's drafts stand when the human can answer each: no Step is run", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.fixer.Script(answers("true", fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "AC 1 asks for hello"}))
		f.arbiter.Script(ruled(arbitrate.Given{Thread: "RT_1", Verdict: arbitrate.Ambiguity, Decision: answerable("hi or hello?")}))
		f.mustFire(0)
		decisions, _ := ruling.Decisions(f.rulingRequests()[0])
		if len(f.writer.Calls()) != 0 || len(decisions) != 1 || decisions[0].Title != "hi or hello?" || rulingOf(f.records()[len(f.records())-1])["written"] != false {
			t.Errorf("writer calls %d, decisions %+v", len(f.writer.Calls()), decisions)
		}
	})
	t.Run("decisions left when the rounds run out are asked, on the parked PR", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"revise":1}}`)
		f.reviewedWithThread()
		human := f.humanThread("say hi")
		f.reviewThreads[0].stubborn = true
		f.fixer.Script(answers("echo Hello > greeting.txt",
			fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"},
			fix.Answer{Thread: human.id, Action: fix.Disputed, Reply: "the ticket says hello"}))
		f.arbiter.Script(ruled(arbitrate.Given{Thread: human.id, Verdict: arbitrate.Ambiguity, Decision: answerable("hi or hello?")}))
		f.mustFire(0)
		if strings.Join(f.pr.labels, ",") != "AFK:revise-failed,AFK:ruling" || len(f.rulingRequests()) != 1 {
			t.Errorf("labels %v, requests %d", f.pr.labels, len(f.rulingRequests()))
		}
	})
	t.Run("a thread the open request asks about already is not asked twice", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		asked := *answerable("hi or hello?")
		asked.Thread = []byte(`{"threadId":"RT_1","commentDatabaseId":9000,"path":"greeting.txt","line":1}`)
		first, _ := ruling.Compose([]ruling.Decision{asked}, "abc1234", "CI green", 0)
		f.pr.comments = append(f.pr.comments, first)
		f.reviewThreads[0].comments = append(f.reviewThreads[0].comments, map[string]any{"databaseId": 9100, "body": "<!-- auto-agent:dispute -->\ndisputed: lowercase", "author": map[string]any{"login": "machine-user"}})
		f.mustFire(0)
		rev := f.revise()
		if len(f.arbiter.Calls())+len(f.fixer.Calls())+len(f.writer.Calls()) != 0 || len(f.rulingRequests()) != 1 || fmt.Sprint(rev["asked"]) != "[RT_1]" || slices.Contains(f.pr.labels, "AFK:revise") {
			t.Errorf("Step calls %d, requests %d, revise %v", len(f.arbiter.Calls())+len(f.fixer.Calls())+len(f.writer.Calls()), len(f.rulingRequests()), rev)
		}
	})
	t.Run("a request the human answered is the next Fire's to apply: nothing is asked over it", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		first, _ := ruling.Compose([]ruling.Decision{*answerable("greet once?")}, "abc1234", "CI green", 0)
		f.pr.comments = append(f.pr.comments, first, "1A")
		f.fixer.Script(answers("true", fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "AC 1 asks for hello"}))
		f.mustFire(0)
		record := f.records()[len(f.records())-1]
		if len(f.rulingRequests()) != 1 || len(f.writer.Calls()) != 0 || !strings.Contains(fmt.Sprint(record["warnings"]), "answered and not yet applied") {
			t.Errorf("requests %d, record %v", len(f.rulingRequests()), record)
		}
	})
	t.Run("a failed post exits non-zero and applies no label", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.specReview.Script(found(review.SpecName, unsure))
		posts := 0
		f.gh.HandleFunc("POST", "/repos/acme/widgets/issues/55/comments", func(r githubtest.Request) githubtest.Reply {
			if posts++; posts == 1 {
				f.pr.comments = append(f.pr.comments, finding.Done(0, 0, "x", finding.Routes{}, ""))
				return githubtest.JSON(201, map[string]any{"id": 1})
			}
			return githubtest.JSON(502, map[string]any{"message": "bad gateway"})
		})
		f.mustFire(1)
		if slices.Contains(f.pr.labels, "AFK:ruling") {
			t.Errorf("labels %v", f.pr.labels)
		}
	})
}

// asComments are PR #55's comments as the fake GitHub lists them.
func asComments(bodies []string) []github.Comment {
	comments := make([]github.Comment, len(bodies))
	for i, body := range bodies {
		comments[i] = github.Comment{ID: int64(5500 + i), Author: "machine-user", Body: body}
	}
	return comments
}
