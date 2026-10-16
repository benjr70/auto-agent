package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/step/arbitrate"
	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/review"
	"github.com/benjr70/auto-agent/internal/step/rulingrequest"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// reviewThread is a review thread on PR #55 as the fake GitHub keeps it.
type reviewThread struct {
	id       string
	path     string
	line     int
	resolved bool
	// stubborn makes GitHub accept resolving it, and leave it open.
	stubborn bool
	comments []map[string]any
}

// keepThreads makes the fake GitHub keep PR #55's review threads whole:
// the threads the review opens, the replies posted in them, and which are
// resolved; and the labels taken off the PR. GitHub lists the PR as the
// Daemon's, carrying whatever labels it has now.
func (f *fireFixture) keepThreads() {
	f.gh.HandleFunc("POST", "/repos/acme/widgets/pulls/55/comments", func(r githubtest.Request) githubtest.Reply {
		var sent map[string]any
		json.Unmarshal([]byte(r.Body), &sent)
		id := 9000 + f.commentCount()
		comment := map[string]any{"databaseId": id, "body": sent["body"], "author": map[string]any{"login": "machine-user"}}
		if to, ok := sent["in_reply_to"].(float64); ok {
			for _, t := range f.reviewThreads {
				if t.comments[0]["databaseId"] == int(to) {
					t.comments = append(t.comments, comment)
					if f.onReply != nil {
						f.onReply(fmt.Sprint(sent["body"]))
					}
					return githubtest.JSON(201, map[string]any{"id": id})
				}
			}
			return githubtest.JSON(404, map[string]any{"message": "Not Found"})
		}
		f.pr.threads = append(f.pr.threads, sent)
		line, _ := sent["line"].(float64)
		f.reviewThreads = append(f.reviewThreads, &reviewThread{id: fmt.Sprintf("RT_%d", len(f.reviewThreads)+1), path: fmt.Sprint(sent["path"]), line: int(line), comments: []map[string]any{comment}})
		return githubtest.JSON(201, map[string]any{"id": id})
	})
	f.gh.HandleFunc("POST", "/graphql", func(r githubtest.Request) githubtest.Reply {
		var sent struct {
			Query     string
			Variables map[string]any
		}
		json.Unmarshal([]byte(r.Body), &sent)
		switch {
		case strings.Contains(sent.Query, "resolveReviewThread"):
			for _, t := range f.reviewThreads {
				if t.id == sent.Variables["threadId"] && !t.stubborn {
					t.resolved = true
				}
			}
			return githubtest.JSON(200, map[string]any{"data": map[string]any{}})
		case strings.Contains(sent.Query, "reviewThreads"):
			nodes := []map[string]any{}
			for _, t := range f.reviewThreads {
				nodes = append(nodes, map[string]any{"id": t.id, "isResolved": t.resolved, "path": t.path, "line": t.line,
					"comments": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": t.comments}})
			}
			return githubtest.JSON(200, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
				"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes}}}}})
		}
		return githubtest.JSON(400, map[string]any{"message": "unexpected GraphQL " + sent.Query})
	})
	for _, label := range []string{"AFK:revise", "AFK:ruling"} {
		f.gh.HandleFunc("DELETE", "/repos/acme/widgets/issues/55/labels/"+label, func(githubtest.Request) githubtest.Reply {
			f.pr.labels = slices.DeleteFunc(f.pr.labels, func(l string) bool { return l == label })
			return githubtest.Text(200, `[]`)
		})
	}
	listed := func() map[string]any { return openPR(55, "feat/issue-20", "2026-10-12T09:00:00Z", f.pr.labels...) }
	f.gh.HandleFunc("GET", pullsPath, func(githubtest.Request) githubtest.Reply {
		if !f.prOpen {
			return githubtest.JSON(200, []map[string]any{})
		}
		return githubtest.JSON(200, []map[string]any{listed()})
	})
	f.gh.HandleFunc("GET", "/repos/acme/widgets/pulls/55", func(githubtest.Request) githubtest.Reply {
		return githubtest.JSON(200, merges(listed(), true))
	})
	f.gh.Handle("GET", "/repos/acme/widgets/pulls/55/files", githubtest.JSON(200, []map[string]string{{"filename": "greeting.txt"}}))
}

// commentCount is how many comments PR #55's threads hold.
func (f *fireFixture) commentCount() int {
	n := 0
	for _, t := range f.reviewThreads {
		n += len(t.comments)
	}
	return n
}

// humanThread opens a thread on PR #55 the way a person does, with any
// replies after it.
func (f *fireFixture) humanThread(bodies ...string) *reviewThread {
	t := &reviewThread{id: fmt.Sprintf("RT_%d", len(f.reviewThreads)+1), path: "greeting.txt", line: 1}
	f.reviewThreads = append(f.reviewThreads, t)
	for _, body := range bodies {
		t.comments = append(t.comments, map[string]any{"databaseId": 9000 + f.commentCount(), "body": body, "author": map[string]any{"login": "a-human"}})
	}
	return t
}

// reviewedWithThread runs the Fire that implements ticket #20, opens PR #55
// and reviews it, leaving one defect thread and AFK:revise on the PR; GitHub
// then lists the PR.
func (f *fireFixture) reviewedWithThread() {
	f.t.Helper()
	f.keepThreads()
	f.answer(finished)
	f.correctness.Script(found(review.CorrectnessName, realBug))
	f.mustFire(0)
	if len(f.reviewThreads) != 1 || !slices.Contains(f.pr.labels, "AFK:revise") {
		f.t.Fatalf("the review left threads %v, labels %v", f.reviewThreads, f.pr.labels)
	}
	f.prOpen = true
}

// answers is the fake Fix Step changing the checkout and answering threads.
func answers(run string, answered ...fix.Answer) steptest.Reply {
	return steptest.Reply{Run: run, Lines: []string{
		steptest.Init("fix-1", "claude-opus", append([]string{"StructuredOutput"}, fix.Spec().Tools...), []string{"factory:fix"}),
		steptest.Success(fix.Result{Summary: "answered the threads", CommitMessage: "fix(a): answer the review", Threads: append([]fix.Answer{}, answered...)}),
	}}
}

// ruled is the fake Arbiter answering with these verdicts.
func ruled(given ...arbitrate.Given) steptest.Reply {
	return steptest.Reply{Lines: []string{
		steptest.Init("arbitrate-1", "claude-opus", append([]string{"StructuredOutput"}, arbitrate.Spec(implement.Commands{}).Tools...), []string{"factory:arbitrate"}),
		steptest.Success(arbitrate.Result{Verdicts: append([]arbitrate.Given{}, given...)}),
	}}
}

// wrote is the fake Write Ruling request Step writing a decision for each
// of n items.
func wrote(n int) steptest.Reply {
	var written []rulingrequest.Written
	for i := 1; i <= n; i++ {
		written = append(written, rulingrequest.Written{Item: i, Decision: ruling.Decision{
			Title: fmt.Sprintf("Question %d", i), Scenario: "a user greets", Now: "hello", Wants: "Hello", Why: "the ticket is silent",
			Options: []ruling.Option{{Letter: "A", Text: "keep hello", Cost: "nothing", Recommended: true}, {Letter: "B", Text: "say Hello", Cost: "a fix", Fix: true}},
		}})
	}
	return steptest.Reply{Lines: []string{
		steptest.Init("write-1", "claude-opus", append([]string{"StructuredOutput"}, rulingrequest.Spec(implement.Commands{}).Tools...), []string{"factory:" + rulingrequest.Name}),
		steptest.Success(rulingrequest.Result{Decisions: written}),
	}}
}

// rulingRequests are the Ruling requests on PR #55.
func (f *fireFixture) rulingRequests() []string {
	var requests []string
	for _, c := range f.pr.comments {
		if strings.HasPrefix(c, ruling.RequestMarker) {
			requests = append(requests, c)
		}
	}
	return requests
}

// lastBody is the last comment of a thread.
func (t *reviewThread) lastBody() string { return fmt.Sprint(t.comments[len(t.comments)-1]["body"]) }

// The Fix answers review threads: Go commits and pushes, replies in each
// thread, resolves what was fixed and leaves Disputes open.
func TestFireRevise(t *testing.T) {
	t.Run("a fixed thread gets the reply and is resolved after the push (behaviour 1)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		before := f.remoteRef("refs/heads/feat/issue-20")
		f.fixer.Script(answers("echo Hello > greeting.txt", fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "the greeting is capitalised"}))
		f.mustFire(0)
		head := f.remoteRef("refs/heads/feat/issue-20")
		thread := f.reviewThreads[0]
		if head == before || !thread.resolved || thread.lastBody() != "<!-- auto-agent:fix -->\nfixed in "+head[:7]+": the greeting is capitalised" {
			t.Errorf("head %s (was %s), thread %+v", head, before, thread)
		}
		if slices.Contains(f.pr.labels, "AFK:revise") {
			t.Errorf("AFK:revise is still on: %v", f.pr.labels)
		}
		records := f.records()
		record := records[len(records)-1]
		rev, _ := record["revise"].(map[string]any)
		if record["kind"] != "reconcile" || record["reconcile"] != "revise" || record["phase"] != "done" || namesOf(record) != "fix" || rev["rounds"] != 1.0 || fmt.Sprint(rev["fixed"]) != "[RT_1]" || record["commit"] != head {
			t.Errorf("record = %v", record)
		}
		// The push came before the reply: the reply names the pushed commit.
		writes := f.writes()
		reply, resolve := strings.LastIndex(writes, "POST /repos/acme/widgets/pulls/55/comments"), strings.LastIndex(writes, "POST /graphql")
		if reply < 0 || resolve < reply || !strings.HasSuffix(writes, "DELETE /repos/acme/widgets/issues/55/labels/AFK:revise\nDELETE "+unlockPath) {
			t.Errorf("GitHub received\n%s", writes)
		}
	})
	t.Run("a disputed thread gets the reply and stays open, its Dispute recorded (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		human := f.humanThread("please rename greeting.txt")
		before := f.remoteRef("refs/heads/feat/issue-20")
		f.fixer.Script(answers("true",
			fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "AC 1 asks for hello in lowercase"},
			fix.Answer{Thread: "RT_2", Action: fix.Disputed, Reply: "the file name is the ticket's"}))
		f.writer.Script(wrote(2))
		f.mustFire(0)
		bot := f.reviewThreads[0]
		if bot.resolved || human.resolved || bot.lastBody() != "<!-- auto-agent:dispute -->\ndisputed: AC 1 asks for hello in lowercase" || f.remoteRef("refs/heads/feat/issue-20") != before {
			t.Errorf("threads %+v %+v", bot, human)
		}
		if slices.Contains(f.pr.labels, "AFK:revise") {
			t.Errorf("every thread is answered, and AFK:revise is still on: %v", f.pr.labels)
		}
		records := f.records()
		rev, _ := records[len(records)-1]["revise"].(map[string]any)
		disputes, _ := rev["disputes"].([]any)
		if len(disputes) != 2 || fmt.Sprint(disputes[0]) != "map[authored:bot line:1 path:greeting.txt reply:AC 1 asks for hello in lowercase thread:RT_1]" || disputes[1].(map[string]any)["authored"] != "human" {
			t.Errorf("revise = %v", rev)
		}
		// The Arbiter left both unruled, so both are asked in a Ruling
		// request; with AFK:revise put back, the next Fire has nothing for a
		// Fix, and nothing for the Arbiter: they wait on the human.
		if requests := f.rulingRequests(); len(requests) != 1 || !strings.Contains(requests[0], "Ruling request · 2 decisions") {
			t.Fatalf("requests %v", requests)
		}
		f.pr.labels = append(f.pr.labels, "AFK:revise")
		f.mustFire(0)
		if len(f.fixer.Calls()) != 1 || len(f.arbiter.Calls()) != 1 || len(f.rulingRequests()) != 1 || strings.Join(f.pr.labels, ",") != "AFK:ruling" {
			t.Errorf("fix calls %d, Arbiter calls %d, labels %v", len(f.fixer.Calls()), len(f.arbiter.Calls()), f.pr.labels)
		}
		// Still recorded, for whoever answers them.
		records = f.records()
		waiting, _ := records[len(records)-1]["revise"].(map[string]any)["waiting"].([]any)
		if len(waiting) != 2 || waiting[0].(map[string]any)["thread"] != "RT_1" || waiting[1].(map[string]any)["authored"] != "human" {
			t.Errorf("waiting = %v", waiting)
		}
	})
	t.Run("tr_resolve_with_reply never resolves when the reply failed", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.gh.Handle("POST", "/repos/acme/widgets/pulls/55/comments", githubtest.JSON(502, map[string]any{"message": "bad gateway"}))
		f.fixer.Script(answers("echo Hello > greeting.txt", fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"}))
		f.mustFire(1)
		if f.reviewThreads[0].resolved || strings.Contains(f.writes(), "POST /graphql") || !slices.Contains(f.pr.labels, "AFK:revise") {
			t.Errorf("thread %+v, labels %v, writes\n%s", f.reviewThreads[0], f.pr.labels, f.writes())
		}
	})
	t.Run("a human reply on a thread reaches the Fix brief (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		// The human answers the review's thread, and opens one of their own
		// with a follow-up.
		f.reviewThreads[0].comments = append(f.reviewThreads[0].comments, map[string]any{"databaseId": 9100, "body": "agreed, and keep the exclamation mark", "author": map[string]any{"login": "a-human"}})
		f.humanThread("this leaks the handle", "and the test should cover it")
		f.fixer.Script(answers("echo Hello! > greeting.txt",
			fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised, mark kept"},
			fix.Answer{Thread: "RT_2", Action: fix.Fixed, Reply: "closed the handle, with a test"}))
		f.mustFire(0)
		stdin := f.fixer.Calls()[0].Stdin
		for _, want := range []string{`"kind":"review-threads"`, `"base":"origin/trunk"`,
			`{"id":"RT_1","path":"greeting.txt","line":1,"authored":"bot","comments":[{"from":"review","body":"\u003c!-- pr-review-bot --\u003e`,
			`{"from":"human","body":"agreed, and keep the exclamation mark"}`,
			`{"id":"RT_2","path":"greeting.txt","line":1,"authored":"human","comments":[{"from":"human","body":"this leaks the handle"},{"from":"human","body":"and the test should cover it"}]}`} {
			if !strings.Contains(stdin, want) {
				t.Errorf("the Fix was not sent %s:\n%s", want, stdin)
			}
		}
		if !f.reviewThreads[0].resolved || !f.reviewThreads[1].resolved {
			t.Errorf("threads %+v", f.reviewThreads)
		}
	})
	t.Run("a human ruling on the Daemon's reply is applied, and its reply is the Ruling's", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.humanThread("rename it to count", "<!-- auto-agent:dispute -->\ndisputed: the name is the glossary's", "rename it anyway")
		// A Dispute on the ruled thread is refused, and corrected once.
		f.fixer.Script(answers("echo Hello > greeting.txt",
			fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"},
			fix.Answer{Thread: "RT_2", Action: fix.Disputed, Reply: "no"}),
			answers("true",
				fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"},
				fix.Answer{Thread: "RT_2", Action: fix.Fixed, Reply: "renamed it to count"}))
		f.mustFire(0)
		if calls := f.fixer.Calls(); len(calls) != 2 || !strings.Contains(calls[0].Stdin, `"ruling":"rename it anyway"`) || !strings.Contains(calls[1].Stdin, "but the human ruled it") {
			t.Errorf("fix calls %d", len(calls))
		}
		head := f.remoteRef("refs/heads/feat/issue-20")
		if ruled := f.reviewThreads[1]; !ruled.resolved || ruled.lastBody() != "<!-- auto-agent:ruling -->\nruling applied in "+head[:7]+": renamed it to count" {
			t.Errorf("ruled thread %+v", ruled)
		}
	})
	t.Run("the cap parks the PR (behaviour 4)", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"revise":1}}`)
		f.reviewedWithThread()
		f.reviewThreads[0].stubborn = true
		f.fixer.Script(answers("echo Hello > greeting.txt", fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"}))
		f.mustFire(0)
		thread := f.reviewThreads[0]
		if len(f.fixer.Calls()) != 1 || thread.lastBody() != "<!-- auto-agent:escalate -->\nStill open after 1 round(s) of fixes: parked for a human. Reply here to rule it." {
			t.Errorf("fix calls %d, thread %+v", len(f.fixer.Calls()), thread)
		}
		if strings.Join(f.pr.labels, ",") != "AFK:revise-failed" || !strings.Contains(f.pr.comments[len(f.pr.comments)-1], "1 review thread(s) still open after 1 round(s) of fixes") {
			t.Errorf("labels %v, comments %v", f.pr.labels, f.pr.comments)
		}
		records := f.records()
		record := records[len(records)-1]
		if rev, _ := record["revise"].(map[string]any); record["phase"] != "done" || fmt.Sprint(rev["failing"]) != "[RT_1]" || record["error"] != "" {
			t.Errorf("record = %v", record)
		}
		// Parked: the next Fire leaves it alone.
		f.mustFire(0)
		if len(f.fixer.Calls()) != 1 {
			t.Error("a parked PR was worked")
		}
	})
	t.Run("rounds run to the cap while threads stay open", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"revise":2}}`)
		f.reviewedWithThread()
		f.reviewThreads[0].stubborn = true
		f.fixer.Script(answers("echo Hello > greeting.txt", fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"}))
		f.mustFire(0)
		records := f.records()
		if rev, _ := records[len(records)-1]["revise"].(map[string]any); len(f.fixer.Calls()) != 2 || rev["rounds"] != 2.0 || strings.Join(f.pr.labels, ",") != "AFK:revise-failed" {
			t.Errorf("fix calls %d, revise %v, labels %v", len(f.fixer.Calls()), rev, f.pr.labels)
		}
	})
	t.Run("a Fix that fails its attempts parks the PR for a human", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.fixer.Script(crashes)
		for range 3 {
			f.fire()
		}
		if f.labelled() != "AFK:failed" || strings.Join(f.pr.labels, ",") != "AFK:revise-failed" || !strings.Contains(f.pr.comments[len(f.pr.comments)-1], "could not be answered") {
			t.Errorf("ticket %q, PR labels %v, comments %v", f.labelled(), f.pr.labels, f.pr.comments)
		}
		if f.reviewThreads[0].resolved || len(f.reviewThreads[0].comments) != 1 {
			t.Errorf("a thread nobody answered was replied to or resolved: %+v", f.reviewThreads[0])
		}
	})
	t.Run("the dry run names the threads it would answer", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.stdout.Reset()
		if code := f.run("run", "--once", "--dry-run", "--repo", "acme/widgets", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if next, _ := f.report(t)["next"].(map[string]any); next["action"] != "revise" || next["pr"] != 55.0 || next["issue"] != 20.0 {
			t.Errorf("next = %v", next)
		}
	})
}

// revise is the revise record of the last Fire.
func (f *fireFixture) revise() map[string]any {
	records := f.records()
	rev, _ := records[len(records)-1]["revise"].(map[string]any)
	return rev
}

// The Arbiter rules the Disputes a Fix leaves: Go acts on each verdict.
func TestFireArbiter(t *testing.T) {
	t.Run("each verdict leads to its action, and the ruled round is guaranteed under a cap of 1 (behaviour 1)", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"revise":1}}`)
		f.reviewedWithThread()
		f.reviewThreads = append(f.reviewThreads, &reviewThread{id: "RT_2", path: "greeting.txt", line: 1, comments: []map[string]any{{"databaseId": 8002, "body": "<!-- pr-review-bot -->\n🤖 the file has no newline", "author": map[string]any{"login": "machine-user"}}}})
		human := f.humanThread("say hi, not hello")
		f.fixer.Script(
			answers("true",
				fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "AC 1 asks for hello in lowercase"},
				fix.Answer{Thread: "RT_2", Action: fix.Disputed, Reply: "echo adds the newline"},
				fix.Answer{Thread: human.id, Action: fix.Disputed, Reply: "the ticket says hello"}),
			answers("echo Hello > greeting.txt", fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised, as the Arbiter ruled"}))
		f.arbiter.Script(ruled(
			arbitrate.Given{Thread: "RT_1", Verdict: arbitrate.Fix, Text: "capitalise the greeting in greeting.txt"},
			arbitrate.Given{Thread: "RT_2", Verdict: arbitrate.Dismiss, Text: "echo writes the newline"},
			arbitrate.Given{Thread: human.id, Verdict: arbitrate.Ambiguity, Decision: &ruling.Decision{Title: "hi or hello?"}}))
		f.mustFire(0)
		fixThread, dismissed := f.reviewThreads[0], f.reviewThreads[1]
		head := f.remoteRef("refs/heads/feat/issue-20")
		if !fixThread.resolved || fmt.Sprint(fixThread.comments[2]["body"]) != "<!-- auto-agent:arbiter -->\narbiter: fix — capitalise the greeting in greeting.txt" || fixThread.lastBody() != "<!-- auto-agent:fix -->\nfixed in "+head[:7]+": capitalised, as the Arbiter ruled" {
			t.Errorf("fix thread %+v", fixThread)
		}
		if !dismissed.resolved || dismissed.lastBody() != "<!-- auto-agent:arbiter -->\narbiter: dismissed — echo writes the newline" {
			t.Errorf("dismissed thread %+v", dismissed)
		}
		if human.resolved || !strings.HasPrefix(human.lastBody(), "<!-- auto-agent:dispute -->") {
			t.Errorf("the human's thread %+v", human)
		}
		if calls := f.fixer.Calls(); len(calls) != 2 || !strings.Contains(calls[1].Stdin, `"arbiter_fix":"capitalise the greeting in greeting.txt"`) || strings.Contains(calls[1].Stdin, `"id":"RT_2"`) {
			t.Errorf("fix calls %d", len(calls))
		}
		rev := f.revise()
		decisions, _ := rev["decisions"].([]any)
		if rev["rounds"] != 2.0 || fmt.Sprint(rev["dismissed"]) != "[RT_2]" || len(decisions) != 1 || decisions[0].(map[string]any)["verdict"] != "ambiguity" || decisions[0].(map[string]any)["thread"] != human.id {
			t.Errorf("revise = %v", rev)
		}
		if slices.Contains(f.pr.labels, "AFK:revise") || slices.Contains(f.pr.labels, "AFK:revise-failed") {
			t.Errorf("labels %v", f.pr.labels)
		}
		if names := namesOf(f.records()[len(f.records())-1]); names != "fix,arbitrate,fix,write-ruling-request" {
			t.Errorf("steps %s", names)
		}
	})
	t.Run("the Arbiter is told each Finding and Dispute, and nothing else the Fix said", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.fixer.Script(answers("true", fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "AC 1 asks for hello in lowercase"}))
		f.mustFire(0)
		stdin := f.arbiter.Calls()[0].Stdin
		if !strings.Contains(stdin, `"disputes":[{"thread":"RT_1","path":"greeting.txt","line":1,"authored":"bot","finding":"\u003c!-- pr-review-bot --\u003e`) ||
			!strings.Contains(stdin, `"dispute":"disputed: AC 1 asks for hello in lowercase"}]`) || strings.Contains(stdin, "answered the threads") || !strings.Contains(stdin, `"base":"origin/trunk"`) {
			t.Errorf("the Arbiter was sent %s", stdin)
		}
		if allowed, _ := f.arbiter.Calls()[0].Arg("--allowedTools"); strings.Contains(allowed, "Edit") || strings.Contains(allowed, "Write") {
			t.Errorf("--allowedTools %q", allowed)
		}
	})
	t.Run("a human thread is never dismissed: a dismissal or fix on one is unruled (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		human := f.humanThread("say hi")
		f.fixer.Script(answers("echo Hello > greeting.txt",
			fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"},
			fix.Answer{Thread: human.id, Action: fix.Disputed, Reply: "the ticket says hello"}))
		f.arbiter.Script(ruled(arbitrate.Given{Thread: human.id, Verdict: arbitrate.Dismiss, Text: "the human is wrong"}))
		f.mustFire(0)
		if human.resolved || len(human.comments) != 2 {
			t.Errorf("the human's thread %+v", human)
		}
		decisions, _ := f.revise()["decisions"].([]any)
		if len(decisions) != 1 || decisions[0].(map[string]any)["verdict"] != "unruled" || !strings.Contains(fmt.Sprint(decisions[0]), "human-authored thread: dismiss refused") {
			t.Errorf("decisions = %v", decisions)
		}
	})
	t.Run("a missing verdict is unruled, not dismissed (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.fixer.Script(answers("true", fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "no"}))
		f.arbiter.Script(ruled(arbitrate.Given{Thread: "RT_9", Verdict: arbitrate.Dismiss, Text: "not a thread it was given"}))
		f.mustFire(0)
		thread := f.reviewThreads[0]
		decisions, _ := f.revise()["decisions"].([]any)
		if thread.resolved || len(thread.comments) != 2 || len(decisions) != 1 || decisions[0].(map[string]any)["reason"] != "no verdict" {
			t.Errorf("thread %+v, decisions %v", thread, decisions)
		}
	})
	t.Run("a Fix that cannot apply the Arbiter's fix takes it to the human, and the Arbiter runs once per Fire", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.fixer.Script(
			answers("true", fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "AC 1 asks for hello in lowercase"}),
			answers("true", fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "capitalising breaks AC 1"}))
		f.arbiter.Script(ruled(arbitrate.Given{Thread: "RT_1", Verdict: arbitrate.Fix, Text: "capitalise it"}))
		f.mustFire(0)
		decisions, _ := f.revise()["decisions"].([]any)
		if len(f.arbiter.Calls()) != 1 || len(decisions) != 1 || decisions[0].(map[string]any)["verdict"] != "cannot" || !strings.Contains(fmt.Sprint(decisions[0]), "the Arbiter's fix: capitalise it") || f.reviewThreads[0].resolved {
			t.Errorf("Arbiter calls %d, decisions %v", len(f.arbiter.Calls()), decisions)
		}
	})
	t.Run("the guaranteed round is one round: a fix ruled before round 1 runs in it, and a cap of 1 then parks", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"revise":1}}`)
		f.reviewedWithThread()
		f.reviewThreads[0].comments = append(f.reviewThreads[0].comments, map[string]any{"databaseId": 9100, "body": "<!-- auto-agent:dispute -->\ndisputed: lowercase is the ticket's", "author": map[string]any{"login": "machine-user"}})
		f.reviewThreads[0].stubborn = true
		f.arbiter.Script(ruled(arbitrate.Given{Thread: "RT_1", Verdict: arbitrate.Fix, Text: "capitalise it"}))
		f.fixer.Script(answers("echo Hello > greeting.txt", fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"}))
		f.mustFire(0)
		if len(f.fixer.Calls()) != 1 || strings.Join(f.pr.labels, ",") != "AFK:revise-failed" {
			t.Errorf("fix calls %d, labels %v", len(f.fixer.Calls()), f.pr.labels)
		}
	})
	t.Run("a Dispute after the Arbiter ran this Fire is a decision for the human: nothing is left to rule it", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.fixer.Script(
			answers("true", fix.Answer{Thread: "RT_1", Action: fix.Disputed, Reply: "lowercase is the ticket's"}),
			answers("true", fix.Answer{Thread: "RT_1", Action: fix.Fixed, Reply: "capitalised"}, fix.Answer{Thread: "RT_2", Action: fix.Disputed, Reply: "the name is the ticket's"}))
		f.arbiter.Script(ruled(arbitrate.Given{Thread: "RT_1", Verdict: arbitrate.Fix, Text: "capitalise it"}))
		// A human opens a thread as the Arbiter's ruling lands.
		f.onReply = func(body string) {
			if strings.Contains(body, "arbiter: fix") {
				f.humanThread("rename greeting.txt")
			}
		}
		f.mustFire(0)
		decisions, _ := f.revise()["decisions"].([]any)
		if len(f.arbiter.Calls()) != 1 || len(decisions) != 1 || decisions[0].(map[string]any)["thread"] != "RT_2" || decisions[0].(map[string]any)["reason"] != "disputed after the Arbiter ran this Fire" {
			t.Errorf("Arbiter calls %d, decisions %v", len(f.arbiter.Calls()), decisions)
		}
	})
	t.Run("a dismissal whose resolve was cut short is resolved by the next Fire, never handed to a Fix", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.reviewThreads[0].comments = append(f.reviewThreads[0].comments,
			map[string]any{"databaseId": 9100, "body": "<!-- auto-agent:dispute -->\ndisputed: lowercase is the ticket's", "author": map[string]any{"login": "machine-user"}},
			map[string]any{"databaseId": 9101, "body": "<!-- auto-agent:arbiter -->\narbiter: dismissed — AC 1 says hello", "author": map[string]any{"login": "machine-user"}})
		f.mustFire(0)
		if !f.reviewThreads[0].resolved || len(f.fixer.Calls())+len(f.arbiter.Calls()) != 0 || slices.Contains(f.pr.labels, "AFK:revise") {
			t.Errorf("thread %+v, Step calls %d", f.reviewThreads[0], len(f.fixer.Calls())+len(f.arbiter.Calls()))
		}
	})
	t.Run("Disputes an earlier Fire left are ruled when they are all there is", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedWithThread()
		f.reviewThreads[0].comments = append(f.reviewThreads[0].comments, map[string]any{"databaseId": 9100, "body": "<!-- auto-agent:dispute -->\ndisputed: lowercase is the ticket's", "author": map[string]any{"login": "machine-user"}})
		f.arbiter.Script(ruled(arbitrate.Given{Thread: "RT_1", Verdict: arbitrate.Dismiss, Text: "AC 1 says hello"}))
		f.mustFire(0)
		if !f.reviewThreads[0].resolved || len(f.fixer.Calls()) != 0 || slices.Contains(f.pr.labels, "AFK:revise") {
			t.Errorf("thread %+v, fix calls %d", f.reviewThreads[0], len(f.fixer.Calls()))
		}
		if names := namesOf(f.records()[len(f.records())-1]); names != "arbitrate" {
			t.Errorf("steps %s", names)
		}
	})
}
