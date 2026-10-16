package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/review"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// reviewOf is the review in a Fire record.
func reviewOf(record map[string]any) map[string]any {
	r, _ := record["review"].(map[string]any)
	return r
}

// done is the comment that says PR #55 was reviewed; empty when none was
// posted.
func (f *fireFixture) reviewed() string {
	for _, c := range f.pr.comments {
		if finding.IsDone(c) {
			return c
		}
	}
	return ""
}

// listOurPR makes GitHub list PR #55, the Daemon's, on ticket #20's branch,
// and read it alone as merging.
func (f *fireFixture) listOurPR() {
	pr := openPR(55, "feat/issue-20", "2026-10-12T09:00:00Z")
	f.gh.Handle("GET", pullsPath, githubtest.JSON(200, []map[string]any{pr}))
	f.gh.Handle("GET", "/repos/acme/widgets/pulls/55", githubtest.JSON(200, merges(pr, true)))
	f.gh.Handle("GET", "/repos/acme/widgets/pulls/55/files", githubtest.JSON(200, []map[string]string{{"filename": "greeting.txt"}}))
}

var (
	crashes  = steptest.Reply{Run: "echo half > greeting.txt", Stderr: "segfault", ExitCode: 139}
	realBug  = review.Found{Kind: finding.Defect, Category: "bug", Path: "greeting.txt", Line: 1, Severity: "high", Summary: "the greeting is lowercase", FailureScenario: "a user greeted at startup sees hello, not Hello"}
	sameBug  = review.Found{Kind: finding.Defect, Category: "spec-mismatch", Path: "greeting.txt", Line: 1, Severity: "medium", Summary: "AC 1 says Hello", QuotedRequirement: "it greets"}
	aNote    = review.Found{Kind: finding.Defect, Category: "naming", Path: "greeting.txt", Line: 1, Severity: "low", Summary: "the file name is vague", FailureScenario: "nothing"}
	unsure   = review.Found{Kind: finding.ProductAmbiguity, Category: "ambiguity", Path: "greeting.txt", Line: 1, Severity: "medium", Summary: "greet once or every time?", FailureScenario: "once: quiet; every time: noisy", QuotedRequirement: "silent"}
	nothingN = review.Found{Kind: finding.Defect, Category: "bug", Path: "greeting.txt", Line: 1, Severity: "low", Summary: "might be fragile", FailureScenario: "n/a"}
)

// The one-time review of an Agent PR: two Review Steps, the posting bar,
// threads, AFK:revise and the comment that says it ran, all Go's.
func TestFireReview(t *testing.T) {
	t.Run("the Fire that opens the PR reviews it: Findings across both Steps are deduped and only Defects become threads (behaviour 1)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.correctness.Script(found(review.CorrectnessName, realBug, aNote, nothingN))
		f.specReview.Script(found(review.SpecName, sameBug, unsure))
		f.mustFire(0)
		if len(f.pr.threads) != 1 {
			t.Fatalf("threads = %v", f.pr.threads)
		}
		thread := f.pr.threads[0]
		body := fmt.Sprint(thread["body"])
		head := f.remoteRef("refs/heads/feat/issue-20")
		if thread["path"] != "greeting.txt" || thread["line"] != 1.0 || thread["side"] != "RIGHT" || thread["commit_id"] != head ||
			!strings.HasPrefix(body, finding.ThreadMarker+"\n") || strings.Index(body, "lowercase") > strings.Index(body, "AC 1 says Hello") || !strings.Contains(body, "**Contradicts:** > it greets") {
			t.Errorf("thread = %v", thread)
		}
		done := f.reviewed()
		for _, want := range []string{"reviewed=" + head, "5 findings, 0 fixed", "1 defect thread(s), 2 review note(s), 1 product ambiguity(ies)", "the file name is vague", "might be fragile", "greet once or every time?"} {
			if !strings.Contains(done, want) {
				t.Errorf("the review's comment lacks %q:\n%s", want, done)
			}
		}
		record := f.record()
		rev := reviewOf(record)
		if record["phase"] != "done" || rev["threads"] != 1.0 || rev["notes"] != 2.0 || rev["findings"] != 5.0 || len(rev["ambiguities"].([]any)) != 1 {
			t.Errorf("record = %v", record)
		}
		// The product ambiguity is the human's, asked once the review ran.
		if namesOf(record) != "implement,pre-commit-review,review-correctness,review-spec,write-ruling-request" {
			t.Errorf("steps %s", namesOf(record))
		}
	})
	t.Run("a Defect posted applies AFK:revise, and the comment that says the review ran comes last (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.correctness.Script(found(review.CorrectnessName, realBug))
		f.mustFire(0)
		if strings.Join(f.pr.labels, ",") != "AFK:revise" {
			t.Errorf("PR labels %v", f.pr.labels)
		}
		writes := strings.Split(f.writes(), "\n")
		if want := []string{"POST /repos/acme/widgets/pulls/55/comments", "POST /repos/acme/widgets/issues/55/labels", "POST " + reviewedPath, "DELETE " + unlockPath}; strings.Join(writes[len(writes)-4:], "\n") != strings.Join(want, "\n") {
			t.Errorf("GitHub received\n%s", f.writes())
		}
	})
	t.Run("a notes-only review applies no AFK:revise and opens no thread; its product ambiguity is asked in a Ruling request", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.correctness.Script(found(review.CorrectnessName, aNote, nothingN))
		f.specReview.Script(found(review.SpecName, unsure))
		f.mustFire(0)
		if strings.Join(f.pr.labels, ",") != "AFK:ruling" || len(f.pr.threads) != 0 || !strings.Contains(f.reviewed(), "0 defect thread(s), 2 review note(s), 1 product ambiguity(ies)") {
			t.Errorf("labels %v, threads %v, comment\n%s", f.pr.labels, f.pr.threads, f.reviewed())
		}
	})
	t.Run("the Review Steps read the PR on its branch, with the ticket and its parent Spec", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.gh.Handle("GET", "/repos/acme/widgets/issues/20", githubtest.JSON(200, map[string]any{"number": 20, "title": "Tracer", "body": "## Parent\n\nSpec: #7\n\n" + ticketBody, "state": "open"}))
		f.gh.Handle("GET", "/repos/acme/widgets/issues/7", githubtest.JSON(200, map[string]any{"number": 7, "title": "The Spec", "body": "Greet people.", "state": "open"}))
		f.answer(finished)
		f.mustFire(0)
		for _, fake := range []*steptest.Fake{f.correctness, f.specReview} {
			stdin := fake.Calls()[0].Stdin
			if !strings.Contains(stdin, `{"pr":55,"branch":"feat/issue-20","base":"origin/trunk","issue":{"number":20,`) || !strings.Contains(stdin, `"spec":{"number":7,"title":"The Spec","body":"Greet people."}`) {
				t.Errorf("a Review Step was sent %s", stdin)
			}
			if allowed, _ := fake.Calls()[0].Arg("--allowedTools"); strings.Contains(allowed, "Edit") || strings.Contains(allowed, "Write") {
				t.Errorf("--allowedTools %q", allowed)
			}
		}
	})
	t.Run("a reviewed PR is not reviewed again (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.pr.comments = []string{finding.Done(0, 0, "abc", finding.Routes{}, "")}
		f.mustFire(0)
		if len(f.correctness.Calls())+len(f.specReview.Calls()) != 0 || len(f.pr.comments) != 1 {
			t.Errorf("reviewed again: %d comments", len(f.pr.comments))
		}
	})
	t.Run("a PR opened without a review is reviewed by the next Fire, in the reconcile Lane (behaviour 4)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.correctness.Script(crashes)
		f.mustFire(1)
		if f.reviewed() != "" {
			t.Fatal("a review that crashed said it ran")
		}
		// GitHub now lists the PR the first Fire opened.
		f.listOurPR()
		f.correctness.Script(found(review.CorrectnessName, realBug))
		f.mustFire(0)
		record := f.record()
		if record["kind"] != "reconcile" || record["pr"] != 55.0 || record["issue"] != 20.0 || record["phase"] != "done" || namesOf(record) != "review-correctness,review-spec" {
			t.Errorf("record = %v", record)
		}
		if f.reviewed() == "" || len(f.pr.threads) != 1 || strings.Join(f.pr.labels, ",") != "AFK:revise" || f.labelled() != "" {
			t.Errorf("comments %v, threads %v, PR labels %v, ticket labels %q", f.pr.comments, f.pr.threads, f.pr.labels, f.labelled())
		}
		if len(f.claude.Calls()) != 1 {
			t.Error("the ticket was implemented again")
		}
		// The lock was held while it ran, and is off.
		if !strings.Contains(f.writes(), "POST "+lockPath) || !strings.HasSuffix(f.writes(), "DELETE "+unlockPath) {
			t.Errorf("GitHub received\n%s", f.writes())
		}
	})
	t.Run("a review the usage limit cuts off pauses the ticket, and the next Fire reviews the PR and takes the pause off", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.specReview.Script(steptest.Reply{ExitCode: 1, Stderr: "You've hit your session limit · resets 10:50pm (America/New_York)",
			Lines: []string{steptest.Init("spec-1", "claude-opus", append([]string{"StructuredOutput"}, review.Spec(implement.Commands{}).Tools...), []string{"factory:review-spec"})}},
			found(review.SpecName))
		f.mustFire(0)
		if f.labelled() != "AFK:paused" || f.reviewed() != "" {
			t.Fatalf("labels %q, comments %v", f.labelled(), f.pr.comments)
		}
		f.listOurPR()
		f.gh.Handle("GET", "/repos/acme/widgets/issues/20", githubtest.JSON(200, map[string]any{"number": 20, "title": "Tracer", "body": ticketBody, "state": "open", "labels": []map[string]string{{"name": "AFK:paused"}}}))
		f.later(18 * time.Hour)
		f.mustFire(0)
		records := f.records()
		if record := records[len(records)-1]; record["kind"] != "reconcile" || namesOf(record) != "review-correctness,review-spec" || f.labelled() != "" || f.reviewed() == "" {
			t.Errorf("record %v, labels %q", record, f.labelled())
		}
	})
	t.Run("the dry run names the review it would run", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.listOurPR()
		f.stdout.Reset()
		if code := f.run("run", "--once", "--dry-run", "--repo", "acme/widgets", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		next, _ := f.report(t)["next"].(map[string]any)
		if next["action"] != "review" || next["pr"] != 55.0 || next["issue"] != 20.0 {
			t.Errorf("next = %v", next)
		}
	})
	t.Run("a PR whose review ran but not its verification is not reviewed again: that is Verify's", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.listOurPR()
		f.pr.comments = []string{finding.Done(0, 0, "abc", finding.Routes{}, "")}
		f.mustFire(0)
		if record := f.record(); record["kind"] != "reconcile" || record["reconcile"] != "incomplete" || !strings.Contains(fmt.Sprint(record["notWorked"]), "Verify") || f.writes() != "" {
			t.Errorf("record = %v, writes %q", record, f.writes())
		}
	})
	t.Run("a review that keeps failing is given up at the attempt cap, and the PR is handed to a human", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.correctness.Script(crashes)
		f.mustFire(1)
		f.listOurPR()
		f.mustFire(1)
		f.mustFire(1)
		if f.labelled() != "AFK:failed" || !strings.Contains(f.reviewed(), "The automated review could not run") {
			t.Errorf("ticket labels %q, comments %v", f.labelled(), f.pr.comments)
		}
		// The reconcile Lane takes the PR no more.
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "incomplete" || len(f.correctness.Calls()) != 3 {
			t.Errorf("record = %v, %d reviews", record, len(f.correctness.Calls()))
		}
	})
	t.Run("a review cut off after posting some threads does not post them twice", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		other := realBug
		other.Line, other.Summary = 9, "a second bug"
		f.correctness.Script(found(review.CorrectnessName, realBug, other))
		// The second thread fails, as a GitHub outage would.
		posts := 0
		f.gh.HandleFunc("POST", "/repos/acme/widgets/pulls/55/comments", func(r githubtest.Request) githubtest.Reply {
			if posts++; posts == 2 {
				return githubtest.JSON(502, map[string]string{"message": "Bad Gateway"})
			}
			var sent map[string]any
			json.Unmarshal([]byte(r.Body), &sent)
			f.pr.threads = append(f.pr.threads, sent)
			return githubtest.JSON(201, map[string]any{"id": posts})
		})
		f.mustFire(1)
		if len(f.pr.threads) != 1 || f.reviewed() != "" || len(f.pr.labels) != 0 {
			t.Fatalf("threads %v, comments %v", f.pr.threads, f.pr.comments)
		}
		f.listOurPR()
		f.mustFire(0)
		if len(f.pr.threads) != 2 || !strings.Contains(f.reviewed(), "2 defect thread(s)") || strings.Join(f.pr.labels, ",") != "AFK:revise" {
			t.Errorf("threads %v, comment %s", f.pr.threads, f.reviewed())
		}
	})
	// review-poster: "unanchorable defect is posted on the file's first
	// changed line"; "rp_post_defect falls back only on a 422"
	t.Run("unanchorable defect is posted on the file's first changed line", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		off := realBug
		off.Line = 77
		f.correctness.Script(found(review.CorrectnessName, off))
		f.gh.HandleFunc("POST", "/repos/acme/widgets/pulls/55/comments", func(r githubtest.Request) githubtest.Reply {
			var sent map[string]any
			json.Unmarshal([]byte(r.Body), &sent)
			if sent["line"] == 77.0 {
				return githubtest.JSON(422, map[string]string{"message": "Validation Failed: line must be part of the diff"})
			}
			f.pr.threads = append(f.pr.threads, sent)
			return githubtest.JSON(201, map[string]any{"id": 1})
		})
		f.mustFire(0)
		if len(f.pr.threads) != 1 || f.pr.threads[0]["line"] != 1.0 || f.pr.threads[0]["side"] != "RIGHT" || !strings.Contains(fmt.Sprint(f.pr.threads[0]["body"]), "**Intended location:** `greeting.txt:77`") {
			t.Errorf("threads %v", f.pr.threads)
		}
	})
	t.Run("a thread posted on the first changed line is not posted again when the review runs again", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		off := realBug
		off.Line = 77
		f.correctness.Script(found(review.CorrectnessName, off))
		f.gh.HandleFunc("POST", "/repos/acme/widgets/pulls/55/comments", func(r githubtest.Request) githubtest.Reply {
			var sent map[string]any
			json.Unmarshal([]byte(r.Body), &sent)
			if sent["line"] == 77.0 {
				return githubtest.JSON(422, map[string]string{"message": "Validation Failed"})
			}
			f.pr.threads = append(f.pr.threads, sent)
			return githubtest.JSON(201, map[string]any{"id": 1})
		})
		// The comment that says the review ran fails, once.
		failed := false
		f.gh.HandleFunc("POST", reviewedPath, func(r githubtest.Request) githubtest.Reply {
			if !failed {
				failed = true
				return githubtest.JSON(502, map[string]string{"message": "Bad Gateway"})
			}
			var sent struct{ Body string }
			json.Unmarshal([]byte(r.Body), &sent)
			f.pr.comments = append(f.pr.comments, sent.Body)
			return githubtest.JSON(201, map[string]any{"id": 1})
		})
		f.mustFire(1)
		f.listOurPR()
		f.mustFire(0)
		if len(f.pr.threads) != 1 || !strings.Contains(f.reviewed(), "1 defect thread(s)") {
			t.Errorf("threads %v, comment %s", f.pr.threads, f.reviewed())
		}
	})
	t.Run("rp_post_defect falls back only on a 422", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.correctness.Script(found(review.CorrectnessName, realBug))
		f.gh.Handle("POST", "/repos/acme/widgets/pulls/55/comments", githubtest.JSON(502, map[string]string{"message": "Bad Gateway"}))
		f.mustFire(1)
		if posts := strings.Count(f.writes(), "POST /repos/acme/widgets/pulls/55/comments"); posts != 1 || f.reviewed() != "" {
			t.Errorf("%d posts; comment %q", posts, f.reviewed())
		}
	})
	t.Run("a defect on a file the PR does not change is listed as unanchored, never lost", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		elsewhere := realBug
		elsewhere.Path = "README.md"
		f.correctness.Script(found(review.CorrectnessName, elsewhere))
		f.gh.Handle("POST", "/repos/acme/widgets/pulls/55/comments", githubtest.JSON(422, map[string]string{"message": "Validation Failed"}))
		f.mustFire(0)
		if done := f.reviewed(); !strings.Contains(done, "**Could not anchor**") || !strings.Contains(done, "`README.md:1` — the greeting is lowercase") || len(f.pr.labels) != 0 {
			t.Errorf("labels %v, comment\n%s", f.pr.labels, done)
		}
	})

	// bootstrap-state: "config-touched" (×2) and "that same PR is flagged for
	// a human, because it changes its own verification (AC 4)"
	t.Run("every config path, sorted, nothing else: a PR that changes the Harness config is flagged AFK:verify-human", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(steptest.Reply{
			Run: "echo hello > greeting.txt; echo note > .auto-agent/verifier-runbook.md; mkdir -p app && echo x > app/server.py",
			Lines: []string{
				implementInit("session-1"),
				steptest.Result(map[string]any{"structured_output": finished}),
			},
		})
		f.mustFire(0)
		if strings.Join(f.pr.labels, ",") != "AFK:verify-human" {
			t.Errorf("PR labels %v", f.pr.labels)
		}
		done := f.reviewed()
		if !strings.Contains(done, "**Harness config changed — human review required**\n\n- `.auto-agent/verifier-runbook.md`\n\n") || strings.Contains(done, "app/server.py") {
			t.Errorf("comment\n%s", done)
		}
		if changed := reviewOf(f.record())["configChanged"]; fmt.Sprint(changed) != "[.auto-agent/verifier-runbook.md]" {
			t.Errorf("configChanged %v", changed)
		}
	})
	t.Run("a PR that touches no config path answers nothing", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.mustFire(0)
		if len(f.pr.labels) != 0 || strings.Contains(f.reviewed(), "Harness config changed") {
			t.Errorf("labels %v, comment\n%s", f.pr.labels, f.reviewed())
		}
	})
}
