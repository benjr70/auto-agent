package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/ruling"
)

// openPR is an open pull request as GitHub lists it: by the machine user,
// from the repository itself, unless the test says otherwise.
func openPR(number int, branch, created string, labels ...string) map[string]any {
	names := []map[string]string{}
	for _, l := range labels {
		names = append(names, map[string]string{"name": l})
	}
	return map[string]any{
		"number": number, "title": "feat(a): thing", "state": "open", "draft": false, "created_at": created,
		"user": map[string]any{"login": "machine-user"}, "labels": names,
		"head": map[string]any{"ref": branch, "sha": "abc", "repo": map[string]any{"full_name": "acme/widgets"}},
		"base": map[string]any{"ref": "trunk"},
	}
}

// merges is the pull request read alone, saying whether it merges: true,
// false (a conflict) or nil (GitHub is still working it out).
func merges(pr map[string]any, mergeable any) map[string]any {
	one := map[string]any{}
	for k, v := range pr {
		one[k] = v
	}
	one["mergeable"] = mergeable
	return one
}

// listPRs scripts the open pull requests, each read alone answering that it
// merges, and with no comments, unless the test scripts otherwise after.
func (f *fixture) listPRs(prs ...map[string]any) {
	f.gh.Handle("GET", "/repos/acme/widgets/pulls", githubtest.JSON(200, prs))
	for _, pr := range prs {
		n := pr["number"].(int)
		f.gh.Handle("GET", fmt.Sprintf("/repos/acme/widgets/pulls/%d", n), githubtest.JSON(200, merges(pr, true)))
		f.gh.Handle("GET", fmt.Sprintf("/repos/acme/widgets/issues/%d/comments", n), githubtest.JSON(200, []any{}))
		f.gh.Handle("GET", fmt.Sprintf("/repos/acme/widgets/pulls/%d/files", n), githubtest.JSON(200, []map[string]string{{"filename": "lib/x.go"}}))
	}
}

// prComments scripts the comments on PR n, each by its author.
func (f *fixture) prComments(n int, byAuthor ...[2]string) {
	list := []map[string]any{}
	for i, c := range byAuthor {
		list = append(list, map[string]any{"id": i + 1, "body": c[1], "user": map[string]any{"login": c[0]}})
	}
	f.gh.Handle("GET", fmt.Sprintf("/repos/acme/widgets/issues/%d/comments", n), githubtest.JSON(200, list))
}

// done are the comments of a PR whose review and verification finished.
var done = [][2]string{{"machine-user", "<!-- pr-review-done reviewed=x -->\n🤖 pr-review: 0 findings"}, {"machine-user", "### Manual verification — round 1/3"}}

// rulingRequest is a Ruling request comment with two decisions of two
// options each.
func rulingRequest(t *testing.T) string {
	t.Helper()
	decisions := []ruling.Decision{{Title: "One", Options: []ruling.Option{{Text: "a"}, {Text: "b"}}}, {Title: "Two", Options: []ruling.Option{{Text: "a"}, {Text: "b"}}}}
	data, err := json.Marshal(decisions)
	if err != nil {
		t.Fatal(err)
	}
	return ruling.RequestMarker + " head=abc decisions=2 -->\n## 🧑‍⚖️ Ruling request · 2 decisions\n" + ruling.DecisionsMarker + " " + base64.StdEncoding.EncodeToString(data) + " -->"
}

// reconcileOf is the dry run's reconcile verdict, as "pr reason issue", or
// "none".
func (f *fixture) reconcileOf(t *testing.T) string {
	t.Helper()
	f.stdout.Reset()
	if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr.String())
	}
	reconcile, _ := f.report(t)["reconcile"].(map[string]any)
	if reconcile == nil {
		return "none"
	}
	issue, _ := reconcile["issue"].(float64)
	return fmt.Sprintf("%v %v %v", reconcile["pr"], reconcile["reason"], issue)
}

// reads are the paths of the GET requests GitHub received.
func (f *fixture) reads() []string {
	var paths []string
	for _, r := range f.gh.Requests() {
		if r.Method == "GET" {
			paths = append(paths, r.Path)
		}
	}
	return paths
}

// The PR Triage's reads, ported from lib/pr-triage.test.sh's enrich and scan
// cases under the same names, through the dry run and a whole Fire.
func TestReconcileLane(t *testing.T) {
	t.Run("run --once --dry-run ends on the PR and reason it would take, ahead of an eligible ticket, and writes nothing (AC 2, 3)", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply(cleanSlice))
		f.listPRs(openPR(55, "feat/issue-19", "2026-10-01T00:00:00Z", "AFK:revise"))
		if got := f.reconcileOf(t); got != "55 revise 19" {
			t.Fatalf("reconcile = %s", got)
		}
		report := f.report(t)
		next, _ := report["next"].(map[string]any)
		if report["state"] != "reconcile" || report["pick"] != nil || next["action"] != "none" || next["pr"] != 55.0 ||
			!strings.Contains(fmt.Sprint(next["reason"]), "revise is not handled by this build yet") {
			t.Errorf("report = %s", f.stdout.String())
		}
		if writes := f.gh.Writes(); len(writes) != 0 {
			t.Errorf("writes: %+v", writes)
		}
		// The queue is not read: the reconcile Lane took the Fire.
		for _, r := range f.gh.Requests() {
			if r.Path == "/graphql" {
				t.Error("the queue was read")
			}
		}
	})

	t.Run("a real Fire on a reason not yet handled records it, takes no lock and makes no write (AC 4)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.listPRs(openPR(55, "feat/issue-19", "2026-10-01T00:00:00Z", "AFK:revise"))
		f.mustFire(0)
		if writes := f.writes(); writes != "" {
			t.Errorf("GitHub received\n%s", writes)
		}
		if len(f.claude.Calls()) != 0 {
			t.Error("a Step ran")
		}
		record := f.record()
		if record["kind"] != "reconcile" || record["pr"] != 55.0 || record["issue"] != 19.0 || record["reconcile"] != "revise" ||
			!strings.Contains(fmt.Sprint(record["notHandled"]), "not handled by this build yet") || record["phase"] != "done" || record["exit"] != 0.0 {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("a PR with nothing to reconcile leaves the pick to go ahead", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply(cleanSlice))
		f.listPRs(openPR(55, "feat/issue-19", "2026-10-01T00:00:00Z"))
		f.prComments(55, done...)
		if got := f.reconcileOf(t); got != "none" || f.report(t)["state"] != "pick" {
			t.Errorf("reconcile = %s, report %s", got, f.stdout.String())
		}
	})

	t.Run("a fork's PR on a Daemon-shaped branch is never reconciled", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		fork := openPR(55, "feat/issue-19", "2026-10-01T00:00:00Z", "AFK:revise")
		fork["head"] = map[string]any{"ref": "feat/issue-19", "sha": "abc", "repo": map[string]any{"full_name": "someone/widgets"}}
		f.listPRs(fork)
		if got := f.reconcileOf(t); got != "none" {
			t.Errorf("reconcile = %s", got)
		}
	})

	// scan: "scan polls UNKNOWN until resolved"
	t.Run("scan polls UNKNOWN until resolved", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		pr := openPR(310, "feat/issue-281", "2026-07-09T10:00:00Z")
		f.listPRs(pr)
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/310", githubtest.JSON(200, merges(pr, nil)), githubtest.JSON(200, merges(pr, nil)), githubtest.JSON(200, merges(pr, false)))
		if got := f.reconcileOf(t); got != "310 conflict 281" {
			t.Errorf("reconcile = %s", got)
		}
		if len(f.sleeps) != 2 || f.sleeps[0] != 20*time.Second {
			t.Errorf("slept %v", f.sleeps)
		}
	})
	t.Run("scan retry cap yields no-pick", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		pr := openPR(310, "feat/issue-281", "2026-07-09T10:00:00Z")
		f.listPRs(pr)
		f.prComments(310, done...)
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/310", githubtest.JSON(200, merges(pr, nil)))
		if got := f.reconcileOf(t); got != "none" {
			t.Errorf("reconcile = %s", got)
		}
		reads := 0
		for _, path := range f.reads() {
			if path == "/repos/acme/widgets/pulls/310" {
				reads++
			}
		}
		if len(f.sleeps) != 6 || reads != 7 {
			t.Errorf("slept %v, read the PR %d times", f.sleeps, reads)
		}
	})
	t.Run("scan picks a bot-incomplete PR", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.listPRs(openPR(400, "feat/issue-350", "2026-08-01T10:00:00Z"))
		f.prComments(400, [2]string{"machine-user", "<!-- pr-review-done reviewed=x -->"})
		if got := f.reconcileOf(t); got != "400 incomplete 350" {
			t.Errorf("reconcile = %s", got)
		}
	})
	t.Run("enrich probes only clean candidates", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		draft := openPR(402, "feat/issue-352", "2026-07-01T10:00:00Z")
		draft["draft"] = true
		human := openPR(403, "feat/issue-353", "2026-07-01T10:00:00Z")
		human["user"] = map[string]any{"login": "some-human"}
		conflicting := openPR(401, "feat/issue-351", "2026-07-01T10:00:00Z")
		clean := openPR(404, "feat/issue-354", "2026-07-01T10:00:00Z")
		f.listPRs(openPR(400, "feat/issue-350", "2026-07-01T10:00:00Z", "AFK:revise"), conflicting, draft, human, clean,
			openPR(405, "hotfix/by-hand", "2026-07-01T10:00:00Z"))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/401", githubtest.JSON(200, merges(conflicting, false)))
		f.prComments(404, done...)
		f.reconcileOf(t)
		reads := f.reads()
		for _, n := range []int{400, 401, 402, 403, 405} {
			for _, path := range []string{fmt.Sprintf("/repos/acme/widgets/issues/%d/comments", n), fmt.Sprintf("/repos/acme/widgets/pulls/%d/files", n)} {
				if slices.Contains(reads, path) {
					t.Errorf("read %s", path)
				}
			}
		}
		for _, n := range []int{400, 402, 403, 405} {
			if slices.Contains(reads, fmt.Sprintf("/repos/acme/widgets/pulls/%d", n)) {
				t.Errorf("read whether #%d merges", n)
			}
		}
		if !slices.Contains(reads, "/repos/acme/widgets/issues/404/comments") || !slices.Contains(reads, "/repos/acme/widgets/pulls/404/files") {
			t.Errorf("the clean candidate was not read: %v", reads)
		}
	})
	t.Run("enrich gh error fails safe as complete", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		f.listPRs(openPR(400, "feat/issue-350", "2026-08-01T10:00:00Z"))
		f.gh.Handle("GET", "/repos/acme/widgets/issues/400/comments", githubtest.JSON(500, map[string]string{"message": "boom"}))
		if got := f.reconcileOf(t); got != "none" {
			t.Errorf("reconcile = %s", got)
		}
		if !strings.Contains(fmt.Sprint(f.report(t)["warnings"]), "the comments of PR #400 could not be read") {
			t.Errorf("warnings = %v", f.report(t)["warnings"])
		}
	})
	t.Run("whether a PR merges that cannot be read is no conflict, and is said", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		f.listPRs(openPR(400, "feat/issue-350", "2026-08-01T10:00:00Z"))
		f.prComments(400, done...)
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/400", githubtest.JSON(500, map[string]string{"message": "boom"}))
		if got := f.reconcileOf(t); got != "none" || len(f.sleeps) != 0 {
			t.Errorf("reconcile = %s, slept %v", got, f.sleeps)
		}
		if !strings.Contains(fmt.Sprint(f.report(t)["warnings"]), "whether PR #400 merges could not be read") {
			t.Errorf("warnings = %v", f.report(t)["warnings"])
		}
	})
	t.Run("enrich flags docsOnly from the file list", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.listPRs(openPR(590, "feat/issue-577", "2026-08-28T10:00:00Z"), openPR(591, "feat/issue-578", "2026-08-20T10:00:00Z"))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/590/files", githubtest.JSON(200, []map[string]string{{"filename": "docs/research/577.md"}}))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/591/files", githubtest.JSON(200, []map[string]string{{"filename": "docs/research/578.md"}, {"filename": "apps/backend/src/x.ts"}}))
		f.prComments(591, done...)
		if got := f.reconcileOf(t); got != "590 docs-merge 577" {
			t.Errorf("reconcile = %s", got)
		}
	})
	t.Run("enrich docsOnly probe fails safe", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.listPRs(openPR(590, "feat/issue-577", "2026-08-28T10:00:00Z"))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/590/files", githubtest.JSON(500, map[string]string{"message": "boom"}))
		if got := f.reconcileOf(t); got != "590 incomplete 577" {
			t.Errorf("reconcile = %s", got)
		}
	})
	t.Run("enrich probes research-branch PRs", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.listPRs(openPR(594, "research/577-docs-only-merge", "2026-08-28T10:00:00Z"))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/594/files", githubtest.JSON(200, []map[string]string{{"filename": "docs/research/577.md"}}))
		if got := f.reconcileOf(t); got != "594 docs-merge 577" {
			t.Errorf("reconcile = %s", got)
		}
	})
	t.Run("docsOnly uses the configured docs_research_prefix", func(t *testing.T) {
		f := newFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"docs_research_prefix":"docs/findings/"}`)
		f.listPRs(openPR(590, "feat/issue-577", "2026-08-28T10:00:00Z"), openPR(591, "feat/issue-578", "2026-08-20T10:00:00Z"))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/590/files", githubtest.JSON(200, []map[string]string{{"filename": "docs/findings/577.md"}}))
		f.gh.Handle("GET", "/repos/acme/widgets/pulls/591/files", githubtest.JSON(200, []map[string]string{{"filename": "docs/research/578.md"}}))
		f.prComments(591, done...)
		if got := f.reconcileOf(t); got != "590 docs-merge 577" {
			t.Errorf("reconcile = %s", got)
		}
	})

	// The Ruling reply, read through ruling.Read.
	t.Run("one comments read, for the one labelled open Agent PR", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		labelled := openPR(610, "feat/issue-560", "2026-10-01T09:00:00Z", "AFK:ruling")
		human := openPR(615, "feat/issue-565", "2026-09-01T09:00:00Z", "AFK:ruling")
		human["user"] = map[string]any{"login": "some-human"}
		f.listPRs(labelled, human, openPR(616, "my-branch", "2026-09-01T09:00:00Z", "AFK:ruling"))
		f.prComments(610, [2]string{"machine-user", rulingRequest(t)}, [2]string{"the-human", "1A 2B"})
		if got := f.reconcileOf(t); got != "610 ruling 560" {
			t.Errorf("reconcile = %s", got)
		}
		var comments []string
		for _, path := range f.reads() {
			if strings.HasSuffix(path, "/comments") {
				comments = append(comments, path)
			}
		}
		if len(comments) != 1 || comments[0] != "/repos/acme/widgets/issues/610/comments" {
			t.Errorf("comments read: %v", comments)
		}
	})
	t.Run("scan: listing, comments read, verdict reason ruling", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		parked := openPR(613, "feat/issue-563", "2026-09-01T09:00:00Z", "AFK:checks-failed", "AFK:ruling")
		parked["draft"] = true
		f.listPRs(parked)
		f.prComments(613, [2]string{"machine-user", rulingRequest(t)}, [2]string{"the-human", "1a"})
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "ruling" || record["pr"] != 613.0 || f.writes() != "" {
			t.Errorf("record = %v, writes %q", record, f.writes())
		}
	})
	t.Run("a failed comments read is no candidate", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		f.listPRs(openPR(610, "feat/issue-560", "2026-10-01T09:00:00Z", "AFK:ruling"))
		f.gh.Handle("GET", "/repos/acme/widgets/issues/610/comments", githubtest.JSON(500, map[string]string{"message": "boom"}))
		if got := f.reconcileOf(t); got != "none" {
			t.Errorf("reconcile = %s", got)
		}
	})
	t.Run("a reply of free text is not a candidate", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		f.listPRs(openPR(610, "feat/issue-560", "2026-10-01T09:00:00Z", "AFK:ruling"))
		f.prComments(610, append([][2]string{{"machine-user", rulingRequest(t)}, {"the-human", "I agree, resolve it"}}, done...)...)
		if got := f.reconcileOf(t); got != "none" {
			t.Errorf("reconcile = %s", got)
		}
	})
	t.Run("a parked PR without AFK:ruling costs no read and is no candidate", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleGraphQL("issues(first: 100", queueReply())
		parked := openPR(613, "feat/issue-563", "2026-09-01T09:00:00Z", "AFK:checks-failed")
		parked["draft"] = true
		f.listPRs(parked)
		if got := f.reconcileOf(t); got != "none" {
			t.Errorf("reconcile = %s", got)
		}
		for _, path := range f.reads() {
			if strings.Contains(path, "/613") {
				t.Errorf("read %s", path)
			}
		}
	})
}
