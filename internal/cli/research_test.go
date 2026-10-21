package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/research"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// These tests run whole Fires on ticket #20 as a research Decision ticket,
// a sub-issue of Map #9. Its Research Step is the fixture's fake claude.
//
// Ported here and beside: lib/pickup-triage.test.sh's wayfinder routing is
// internal/state's pick-decision cases; lib/docs-research-paths.test.sh's
// docs_research_only is internal/step/research's TestDocsOnly, its
// docs_research_prefix the Harness config loader's. Of lib/fire.test.sh:
//   - test_exhausted_resolve_restarts is replaced by the pause and resume
//     below: a cut-off Research Step resumes by its session, like any Step.
//   - test_crashed_resolve_respects_its_terminal_line is dropped: there is
//     no terminal line to scrape. A crash is a failed attempt by the Go rule
//     for every Step, and the DONE, task and HITL ends it read are later
//     Slices'.
//   - test_resolve_dry_run_* are dropped: Go has no --resolve-dry-run;
//     run --once --dry-run shows the research action a Fire would take.

const (
	researchQueued = `{"number":20,"title":"Which merge gate?","createdAt":"2026-02-01T00:00:00Z","labels":{"nodes":[{"name":"AFK"},{"name":"wayfinder:research"}]},
	"blockedBy":{"nodes":[],"pageInfo":{"hasNextPage":false}},"assignees":{"nodes":[]}}`
	assignPath    = "/repos/acme/widgets/issues/20/assignees"
	researchSlug  = "which-merge-gate"
	researchRef   = "refs/heads/research/" + researchSlug
	findingsPath  = "docs/research/reusable-auto-agent/" + researchSlug + ".md"
	unmappedPath  = "docs/research/unmapped/" + researchSlug + ".md"
	researchTitle = "docs(research): the docs-only merge gate (#20)"
)

var mapParent = githubtest.JSON(200, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"parent": map[string]any{
	"number": 9, "title": "Reusable auto-agent", "body": "## Destination\n\nA Daemon any project can run.\n\n## Notes\n\nPrimary sources first.\n\n## Decisions so far\n",
}}}}})

var answered = research.Result{
	Outcome: research.Found, Title: "The merge gate", Description: "the docs-only merge gate",
	Gist: "A research PR is merged by the docs-only gate once CI is green.", Sources: []string{"https://docs.github.com/en/rest/pulls"},
}

// newResearchFixture is the research world, its Map read with parent. The
// fake GitHub keeps research PR #55 as the Daemon opens it, at the head the
// remote's research branch is at, and merges it into trunk as GitHub would:
// only at that head. It keeps ticket #20's state and Map #9's body.
func newResearchFixture(t *testing.T, parent githubtest.Reply) *fireFixture {
	t.Helper()
	return newResearchFixtureWith(t, labelsConfig, parent)
}

// newResearchFixtureWith is the research world on the given Harness config.
func newResearchFixtureWith(t *testing.T, harness string, parent githubtest.Reply) *fireFixture {
	t.Helper()
	f := newFireFixture(t, harness)
	f.queue = []githubtest.Reply{queueReply(researchQueued)}
	f.research = &researchWorld{ticketState: "open", mapBody: mapBody}
	w := f.research
	f.gh.HandleFunc("GET", "/repos/acme/widgets/issues/20", func(githubtest.Request) githubtest.Reply {
		return githubtest.JSON(200, map[string]any{
			"number": 20, "title": "Which merge gate?", "body": "## Question\n\nWhich merge gate lands a research PR?\n", "state": w.ticketState,
			"labels": []map[string]string{{"name": "AFK"}, {"name": "wayfinder:research"}},
		})
	})
	f.gh.HandleFunc("PATCH", "/repos/acme/widgets/issues/20", func(r githubtest.Request) githubtest.Reply {
		var sent struct{ State string }
		json.Unmarshal([]byte(r.Body), &sent)
		w.ticketState = sent.State
		return githubtest.JSON(200, map[string]any{"number": 20})
	})
	f.gh.HandleFunc("DELETE", "/repos/acme/widgets/issues/20/labels/AFK:failed", func(githubtest.Request) githubtest.Reply {
		delete(f.labels, "AFK:failed")
		return githubtest.Text(200, `[]`)
	})
	f.gh.HandleFunc("GET", "/repos/acme/widgets/issues/9", func(githubtest.Request) githubtest.Reply {
		return githubtest.JSON(200, map[string]any{"number": 9, "title": "Reusable auto-agent", "body": w.mapBody, "state": "open"})
	})
	f.gh.HandleFunc("PATCH", "/repos/acme/widgets/issues/9", func(r githubtest.Request) githubtest.Reply {
		if w.mapRefused {
			return githubtest.Text(500, `{"message":"boom"}`)
		}
		var sent struct{ Body string }
		json.Unmarshal([]byte(r.Body), &sent)
		w.mapBody = sent.Body
		return githubtest.JSON(200, map[string]any{"number": 9})
	})
	f.gh.HandleGraphQL("parent { number title body }", parent)
	f.gh.Handle("POST", assignPath, githubtest.JSON(201, map[string]any{"number": 20}))
	f.gh.HandleFunc("POST", pullsPath, func(r githubtest.Request) githubtest.Reply {
		var sent struct{ Title, Body string }
		json.Unmarshal([]byte(r.Body), &sent)
		w.title, f.pr.body = sent.Title, sent.Body
		return githubtest.JSON(201, map[string]any{"number": 55, "head": map[string]any{"ref": "research/" + researchSlug}})
	})
	f.gh.HandleFunc("GET", "/repos/acme/widgets/pulls/55", func(githubtest.Request) githubtest.Reply {
		return githubtest.JSON(200, f.researchPR())
	})
	f.gh.HandleFunc("PUT", "/repos/acme/widgets/pulls/55/merge", func(r githubtest.Request) githubtest.Reply {
		var sent struct {
			SHA string `json:"sha"`
		}
		json.Unmarshal([]byte(r.Body), &sent)
		if w.moved != "" {
			// Someone pushed between the gate's read and the merge.
			git(t, f.remote, "update-ref", researchRef, w.moved)
		}
		if head := f.remoteRef(researchRef); sent.SHA != head {
			return githubtest.JSON(409, map[string]any{"message": "Head branch was modified. Review and try the merge again."})
		}
		f.merge()
		return githubtest.JSON(200, map[string]any{"sha": w.merged, "merged": true})
	})
	f.gh.HandleFunc("GET", "/repos/acme/widgets/pulls/55/files", func(githubtest.Request) githubtest.Reply {
		files := []map[string]string{}
		for _, path := range w.files {
			files = append(files, map[string]string{"filename": path})
		}
		return githubtest.JSON(200, files)
	})
	f.gh.HandleFunc("GET", pullsPath, func(r githubtest.Request) githubtest.Reply {
		if r.Query.Get("state") == "closed" {
			return githubtest.JSON(200, w.closed)
		}
		return githubtest.JSON(200, w.open)
	})
	return f
}

// mapBody is Map #9's body before any ticket of it is resolved.
const mapBody = "## Destination\n\nA Daemon any project can run.\n\n## Notes\n\nPrimary sources first.\n\n## Decisions so far\n\n- [Old](https://github.com/acme/widgets/issues/3): old\n\n## Out of scope\n\n- Webhooks.\n"

// researchWorld is what the fake GitHub keeps of the research Lane.
type researchWorld struct {
	ticketState string
	// title is research PR #55's; merged the commit it landed as, once it
	// has; moved, when set, is where the research branch moves to just
	// before a merge.
	title, merged, moved string
	mapBody              string
	// mapRefused makes GitHub refuse to edit the Map.
	mapRefused bool
	// open and closed are what GitHub lists as the open and the closed
	// pull requests; files what it lists as research PR #55's files.
	open, closed []map[string]any
	files        []string
}

// researchPR is research PR #55 as GitHub has it now, with the labels the
// Daemon has put on it.
func (f *fireFixture) researchPR() map[string]any {
	pr := openPR(55, "research/"+researchSlug, "2026-10-12T09:00:00Z", f.pr.labels...)
	pr["title"], pr["body"] = f.research.title, f.pr.body
	pr["head"].(map[string]any)["sha"] = f.remoteRef(researchRef)
	pr["mergeable"] = true
	if f.research.merged != "" {
		pr["state"], pr["merged"], pr["merged_at"], pr["merge_commit_sha"] = "closed", true, "2026-10-12T09:40:00Z", f.research.merged
	}
	return pr
}

// merge lands the research branch on trunk, as a squash-merge would: one
// commit on trunk with the branch's tree.
func (f *fireFixture) merge() {
	f.t.Helper()
	tree := git(f.t, f.remote, "rev-parse", researchRef+"^{tree}")
	commit := git(f.t, f.remote, "commit-tree", tree, "-p", "trunk", "-m", f.research.title+" (#55)")
	git(f.t, f.remote, "update-ref", "refs/heads/trunk", commit)
	f.research.merged = commit
}

func researchInit(session string) string {
	return steptest.Init(session, "claude-opus", append([]string{"StructuredOutput"}, research.Spec().Tools...), []string{"factory:research"})
}

// findings is a findings file for ticket #20, researched on date: mapped to
// Map #9, or unmapped.
func findings(date string, mapped bool) string {
	ticket := "Ticket: [#20](https://github.com/acme/widgets/issues/20) (part of wayfinder map\n[#9](https://github.com/acme/widgets/issues/9) — Reusable auto-agent)."
	if !mapped {
		ticket = "Ticket: [#20](https://github.com/acme/widgets/issues/20) (unmapped)."
	}
	return "# The merge gate\n\n" + ticket + "\nResearched on " + date + ".\n\nSources: https://docs.github.com/en/rest/pulls\n\n## TL;DR\n\n- the docs-only gate\n"
}

// researched is the fake Research Step writing content to path, then running
// extra, and answering result.
func (f *fireFixture) researched(session, path, content, extra string, result any) steptest.Reply {
	f.t.Helper()
	source := filepath.Join(f.out, fmt.Sprintf("findings-%d.md", time.Now().UnixNano()))
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return steptest.Reply{
		Run:   fmt.Sprintf("mkdir -p '%s' && cp '%s' '%s'; %s", filepath.Dir(path), source, path, extra),
		Lines: []string{researchInit(session), steptest.Success(result)},
	}
}

// noOtherStep fails the test when any Step but the Research Step was called.
func (f *fireFixture) noOtherStep() {
	f.t.Helper()
	for name, fake := range map[string]*steptest.Fake{"review": f.review, "fix": f.fixer, "correctness": f.correctness, "spec": f.specReview, "arbiter": f.arbiter, "writer": f.writer, "verifier": f.verifier} {
		if calls := fake.Calls(); len(calls) != 0 {
			f.t.Errorf("the %s Step was called %d times", name, len(calls))
		}
	}
	for _, call := range f.claude.Calls() {
		if !strings.HasPrefix(call.Stdin, "/factory:research ") && !strings.Contains(strings.Join(call.Args, " "), "--resume") {
			f.t.Errorf("a call that is not the Research Step: %s", call.Stdin)
		}
	}
}

// landed are the writes that land research PR #55 and resolve ticket #20:
// the merge, the resolution comment, the close, AFK:done on and AFK:failed
// off, the Map's new line, and the lock released.
const landed = "PUT /repos/acme/widgets/pulls/55/merge\nPOST " + commentsPath + "\nPATCH /repos/acme/widgets/issues/20\nPOST " + lockPath +
	"\nDELETE /repos/acme/widgets/issues/20/labels/AFK:failed\nPATCH /repos/acme/widgets/issues/9\nDELETE " + unlockPath

// changedBy are the paths the remote's commit changes against the trunk it
// started from.
func changedBy(t *testing.T, f *fireFixture, commit string) string {
	t.Helper()
	return git(t, f.remote, "diff", "--name-only", "trunk..."+commit)
}

// resolveOf is the resolve block of a Fire record.
func resolveOf(record map[string]any) map[string]any {
	resolve, _ := record["resolve"].(map[string]any)
	return resolve
}

func TestFireResearch(t *testing.T) {
	// Behaviour 1.
	t.Run("a research ticket under a Map: claimed, researched, the PR opened, green, through the gate, merged, and the ticket resolved, in one Fire", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		if want := "POST " + assignPath + "\nPOST " + lockPath + "\nPOST " + pullsPath + "\n" + landed; f.writes() != want {
			t.Fatalf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		writes := f.gh.Writes()
		if writes[0].Body != `{"assignees":["machine-user"]}` || writes[1].Body != `{"labels":["AFK:in-progress"]}` {
			t.Errorf("the claim was %s, then %s", writes[0].Body, writes[1].Body)
		}
		var pr struct{ Title, Head, Base, Body string }
		json.Unmarshal([]byte(writes[2].Body), &pr)
		if pr.Title != researchTitle || pr.Head != "research/"+researchSlug || pr.Base != "trunk" {
			t.Errorf("the PR was %+v", pr)
		}
		for _, part := range []string{
			"## Question\n\nWhich merge gate lands a research PR?\n",
			"## Answer\n\nA research PR is merged by the docs-only gate once CI is green.\n",
			"Resolves ticket #20 — Which merge gate?\n",
			"\n<!-- afk-resolve ticket:#20 map:#9 slug:" + researchSlug + " -->\n",
		} {
			if !strings.Contains(pr.Body, part) {
				t.Errorf("the PR body lacks %q:\n%s", part, pr.Body)
			}
		}
		// The PR never closes the ticket on merge.
		if closing := regexp.MustCompile(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?)\b:?\s+#\d`).FindString(pr.Body); closing != "" {
			t.Errorf("the PR body closes the ticket with %q", closing)
		}
		head := f.remoteRef(researchRef)
		if head == "" || git(t, f.remote, "log", "-1", "--format=%s", head) != researchTitle ||
			changedBy(t, f, head) != findingsPath || git(t, f.remote, "show", head+":"+findingsPath)+"\n" != findings("2026-10-12", true) {
			t.Errorf("the remote's branch is %q", head)
		}
		var merge struct{ SHA, MergeMethod, CommitTitle string }
		json.Unmarshal([]byte(strings.NewReplacer("merge_method", "MergeMethod", "commit_title", "CommitTitle").Replace(writes[3].Body)), &merge)
		if merge.SHA != head || merge.MergeMethod != "squash" || merge.CommitTitle != researchTitle+" (#55)" {
			t.Errorf("the merge was %s", writes[3].Body)
		}
		if got := git(t, f.remote, "show", "trunk:"+findingsPath) + "\n"; got != findings("2026-10-12", true) {
			t.Errorf("trunk's findings file is %q", got)
		}
		record := f.record()
		resolve := resolveOf(record)
		gate, _ := resolve["gate"].(map[string]any)
		if record["kind"] != "pick-decision" || record["action"] != "research" || record["phase"] != "done" || record["pr"] != 55.0 || record["branch"] != "research/"+researchSlug || record["commit"] != head ||
			resolve["type"] != "research" || resolve["slug"] != researchSlug || resolve["findings"] != findingsPath || resolve["pr"] != 55.0 || resolve["failed"] != "" ||
			resolve["merged"] != f.research.merged || resolve["resolved"] != true || gate["sha"] != head || gate["docsOnly"] != true || gate["reason"] != nil {
			t.Errorf("record = %v", record)
		}
		if steps := stepsOf(record); len(steps) != 1 || steps[0]["name"] != "research" || steps[0]["outcome"] != "OK" {
			t.Errorf("steps = %v", steps)
		}
		if f.labelled() != "AFK:done" || f.research.ticketState != "closed" {
			t.Errorf("the ticket is left %s, %q", f.research.ticketState, f.labelled())
		}
		t.Run("the resolution comment is the gist, linking the findings on the default branch, merged in the PR", func(t *testing.T) {
			want := "A research PR is merged by the docs-only gate once CI is green.\n\nFull findings: [" + findingsPath + "](https://github.com/acme/widgets/blob/trunk/" + findingsPath + ") (merged in #55)."
			if len(f.comments) != 1 || f.comments[0].Body != want {
				t.Errorf("comments = %+v", f.comments)
			}
			if strings.Contains(f.comments[0].Body, "research/"+researchSlug) || strings.Contains(f.comments[0].Body, "/tree/") {
				t.Error("the comment links the research branch, which the merge deletes")
			}
		})
		t.Run("the Map has the new line under its Decisions so far; its Destination and Out of scope are as they were", func(t *testing.T) {
			line := "- [Which merge gate?](https://github.com/acme/widgets/issues/20): A research PR is merged by the docs-only gate once CI is green."
			if want := strings.Replace(mapBody, "issues/3): old\n", "issues/3): old\n"+line+"\n", 1); f.research.mapBody != want {
				t.Errorf("the Map is\n%s", f.research.mapBody)
			}
			for _, heading := range []string{"Destination", "Out of scope"} {
				if research.Section(f.research.mapBody, heading) != research.Section(mapBody, heading) {
					t.Errorf("the Map's %s was edited", heading)
				}
			}
		})
		t.Run("no model call beyond the Research Step, which was told the question, the Map and the path", func(t *testing.T) {
			f.noOtherStep()
			calls := f.claude.Calls()
			if len(calls) != 1 {
				t.Fatalf("%d calls", len(calls))
			}
			var in research.Input
			json.Unmarshal([]byte(strings.TrimPrefix(calls[0].Stdin, "/factory:research ")), &in)
			if in.Question != "Which merge gate lands a research PR?" || in.Path != findingsPath || in.Date != "2026-10-12" || in.Ticket.URL != "https://github.com/acme/widgets/issues/20" ||
				in.Map == nil || in.Map.Number != 9 || in.Map.Destination != "A Daemon any project can run." || in.Map.Notes != "Primary sources first." {
				t.Errorf("the Step was told %+v (map %+v)", in, in.Map)
			}
		})
	})
	t.Run("a Map that cannot be read leaves the ticket unmapped, and does not block it", func(t *testing.T) {
		f := newResearchFixture(t, githubtest.Text(502, "bad gateway"))
		f.claude.Script(f.researched("research-1", unmappedPath, findings("2026-10-12", false), "", answered))
		f.mustFire(0)
		var pr struct{ Body string }
		json.Unmarshal([]byte(f.gh.Writes()[2].Body), &pr)
		record := f.record()
		if resolveOf(record)["findings"] != unmappedPath || !strings.Contains(pr.Body, "<!-- afk-resolve ticket:#20 map:unmapped slug:"+researchSlug+" -->") ||
			!strings.Contains(fmt.Sprint(record["warnings"]), "the Map of #20 could not be read") {
			t.Errorf("record = %v, body %s", record, pr.Body)
		}
	})

	// Behaviour 2.
	t.Run("a Step that also edits README.md is corrected once, then the attempt fails", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "echo edited >> README.md", answered))
		f.mustFire(1)
		calls := f.claude.Calls()
		if len(calls) != 2 {
			t.Fatalf("%d calls, want the Step and its one correction", len(calls))
		}
		if resume, _ := calls[1].Arg("--resume"); resume != "research-1" || !strings.Contains(calls[1].Stdin, "outside the research prefix docs/research/: README.md") {
			t.Errorf("the correction resumed %q with %q", resume, calls[1].Stdin)
		}
		record := f.record()
		if !strings.Contains(record["error"].(string), "failed attempt 1 of 3") || f.remoteRef(researchRef) != "" || strings.Contains(f.writes(), "POST "+pullsPath) {
			t.Errorf("record = %v, writes %s", record, f.writes())
		}
		if f.labelled() != "" || len(f.comments) != 1 || !strings.Contains(f.comments[0].Body, "auto-agent:attempts step=research count=1") {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
		f.noOtherStep()

		t.Run("when the attempts run out the ticket is AFK:failed, open, with the resolve's comment", func(t *testing.T) {
			f.mustFire(1)
			f.mustFire(1)
			record := f.record()
			if f.labelled() != "AFK:failed" || resolveOf(record)["failed"] != "research-error" || record["phase"] != "failed" {
				t.Fatalf("labels %q, record %v", f.labelled(), record)
			}
			last := f.comments[len(f.comments)-1].Body
			if !strings.HasPrefix(last, "afk-resolve FAILED at 2026-10-12T09:30:00Z — research-error: the Research Step was asked the ticket's question, for "+findingsPath+"; its last attempt came back MALFORMED: ") ||
				!strings.HasSuffix(last, ". Ticket left open for human triage.") {
				t.Errorf("the last comment is %q", last)
			}
			if strings.Contains(f.writes(), "PATCH /repos/acme/widgets/issues/20") {
				t.Error("the ticket was closed")
			}
		})
	})
	t.Run("a Step that undoes its stray edit on correction opens the PR", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.claude.Script(
			f.researched("research-1", findingsPath, findings("2026-10-12", true), "echo edited >> README.md", answered),
			steptest.Reply{Run: "git checkout -- README.md 2>/dev/null || rm -f README.md", Lines: []string{researchInit("research-1"), steptest.Success(answered)}},
		)
		f.mustFire(0)
		if head := f.remoteRef(researchRef); head == "" || changedBy(t, f, head) != findingsPath || f.record()["pr"] != 55.0 {
			t.Errorf("branch %q, record %v", head, f.record())
		}
	})
	t.Run("a file missing a header fact is corrected once, then the attempt fails", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.claude.Script(f.researched("research-1", findingsPath, strings.Replace(findings("2026-10-12", true), "Sources: https://docs.github.com/en/rest/pulls\n", "", 1), "", answered))
		f.mustFire(1)
		if calls := f.claude.Calls(); len(calls) != 2 || !strings.Contains(calls[1].Stdin, "lacks the sources") {
			t.Errorf("calls = %d, correction %q", len(calls), calls[len(calls)-1].Stdin)
		}
	})

	// Behaviour 3.
	t.Run("no-sources fails the ticket with the comment; the ticket stays open", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.claude.Script(steptest.Reply{Lines: []string{researchInit("research-1"), steptest.Success(research.Result{
			Outcome: research.NoSources, Sources: []string{"https://docs.github.com/en/rest", "the repo's docs/adr/"}, Missing: "which gate the maintainers want",
		})}})
		f.mustFire(1)
		if want := "POST " + assignPath + "\nPOST " + lockPath + "\nPOST " + lockPath + "\nPOST " + commentsPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		want := "afk-resolve FAILED at 2026-10-12T09:30:00Z — no-sources: tried https://docs.github.com/en/rest, the repo's docs/adr/; missing: which gate the maintainers want. Ticket left open for human triage."
		if f.labelled() != "AFK:failed" || len(f.comments) != 1 || f.comments[0].Body != want {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
		if record := f.record(); resolveOf(record)["failed"] != "no-sources" || record["phase"] != "failed" || f.remoteRef(researchRef) != "" {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("an interrupted Research Step is paused, and the next Fire resumes it by its session", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		source := filepath.Join(f.out, "partial.md")
		os.WriteFile(source, []byte("# The merge gate\n"), 0o644)
		f.claude.Script(
			steptest.Reply{
				Run: fmt.Sprintf("mkdir -p '%s' && cp '%s' '%s'", filepath.Dir(findingsPath), source, findingsPath), ExitCode: 1,
				Lines: []string{researchInit("research-9")}, Stderr: "You've hit your session limit · resets 10:50pm (America/New_York)",
			},
			f.researched("research-9", findingsPath, findings("2026-10-13", true), "", answered),
		)
		f.mustFire(0)
		if f.labelled() != "AFK:paused" || git(t, f.remote, "log", "-1", "--format=%s", researchRef) != "wip: freeze partial work on #20 (usage exhausted)" || f.record()["resumeSession"] != "research-9" {
			t.Fatalf("labels %q, record %v", f.labelled(), f.record())
		}
		f.later(18 * time.Hour)
		f.mustFire(0)
		calls := f.claude.Calls()
		if resume, _ := calls[1].Arg("--resume"); len(calls) != 2 || resume != "research-9" {
			t.Errorf("the second Fire's call resumed %q", resume)
		}
		record := f.record()
		if record["kind"] != "resume" || record["pr"] != 55.0 || f.labelled() != "AFK:done" || f.research.ticketState != "closed" ||
			git(t, f.remote, "log", "-1", "--format=%s", researchRef) != researchTitle || changedBy(t, f, f.remoteRef(researchRef)) != findingsPath {
			t.Errorf("labels %q, record %v", f.labelled(), record)
		}
	})
}

// failedQueued is ticket #20 left AFK:failed by a gate that refused its PR.
const failedQueued = `{"number":20,"title":"Which merge gate?","createdAt":"2026-02-01T00:00:00Z","labels":{"nodes":[{"name":"AFK"},{"name":"AFK:failed"},{"name":"wayfinder:research"}]},
	"blockedBy":{"nodes":[],"pageInfo":{"hasNextPage":false}},"assignees":{"nodes":[]}}`

// pushResearch is research PR #55 as an earlier Fire left it: the research
// branch pushed with files on top of trunk, the PR opened with its title,
// body and marker. It returns the branch's head.
func (f *fireFixture) pushResearch(files map[string]string) string {
	f.t.Helper()
	dir := f.t.TempDir()
	git(f.t, dir, "clone", "--quiet", f.remote, ".")
	git(f.t, dir, "checkout", "--quiet", "-b", "research/"+researchSlug)
	for path, content := range files {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	git(f.t, dir, "add", "-A")
	git(f.t, dir, "commit", "--quiet", "-m", researchTitle)
	git(f.t, dir, "push", "--quiet", "origin", "HEAD:"+researchRef)
	in := research.Input{Ticket: research.Ticket{Number: 20, Title: "Which merge gate?"}, Question: "Which merge gate lands a research PR?", Path: findingsPath,
		Map: &research.Map{Number: 9, Title: "Reusable auto-agent"}}
	f.research.title, f.pr.body = researchTitle, research.PullRequestBody(in, answered, researchSlug, fireClock)
	return f.remoteRef(researchRef)
}

// drafted says the Daemon asked GitHub to make a PR a draft.
func (f *fireFixture) drafted() bool {
	for _, r := range f.gh.Requests() {
		if strings.Contains(r.Body, "convertPullRequestToDraft") {
			return true
		}
	}
	return false
}

// refusedOn says ticket #20 was handed to a human for a refused gate, with
// why on it and in the Fire record.
func (f *fireFixture) refusedOn(t *testing.T, why string) {
	t.Helper()
	record := f.record()
	resolve := resolveOf(record)
	if f.labelled() != "AFK:failed" || f.research.ticketState != "open" || resolve["failed"] != "gate-refused" || record["phase"] != "failed" || resolve["resolved"] != false {
		t.Errorf("labels %q, ticket %s, record %v", f.labelled(), f.research.ticketState, record)
	}
	if !strings.Contains(fmt.Sprint(record["error"]), why) {
		t.Errorf("the record's error %q does not say %q", record["error"], why)
	}
	last := f.comments[len(f.comments)-1].Body
	if !strings.HasPrefix(last, "afk-resolve FAILED at ") || !strings.Contains(last, "gate-refused: the docs-only gate refused research PR https://github.com/acme/widgets/pull/55 ("+why) ||
		!strings.Contains(last, "The PR is open: merging it finishes this ticket") {
		t.Errorf("the ticket's comment is %q", last)
	}
	if strings.Contains(f.writes(), "PUT /repos/acme/widgets/pulls/55/merge") && f.research.moved == "" {
		t.Error("a refused PR was merged")
	}
	if f.research.merged != "" {
		t.Error("trunk has the research")
	}
}

func TestFireDocsMerge(t *testing.T) {
	// Behaviour 2.
	t.Run("a check that went red by the time of the gate: the PR is left open, the ticket AFK:failed, saying merging it finishes it", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.ci = [][]map[string]any{{checkRun(1, "build", "completed", "success")}, {checkRun(1, "build", "completed", "failure")}}
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(1)
		f.refusedOn(t, "checks-not-green: 1 check(s) are failing or pending")
		if gate, _ := resolveOf(f.record())["gate"].(map[string]any); gate["reason"] != "checks-not-green" || gate["docsOnly"] != true {
			t.Errorf("gate = %v", gate)
		}
		if f.drafted() {
			t.Error("the PR the Fire opened was parked")
		}
	})
	t.Run("a required check that never passed: checks-missing", func(t *testing.T) {
		f := newResearchFixtureWith(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"required_checks":["title"]}`, mapParent)
		f.ci = [][]map[string]any{{checkRun(1, "build", "completed", "success"), checkRun(2, "title", "completed", "skipped")}}
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(1)
		f.refusedOn(t, "checks-missing: a required check did not run and pass: title")
	})
	t.Run("an open research PR whose real diff leaves the prefix: refused not-docs-only, though GitHub's file list said docs only, and parked", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.pushResearch(map[string]string{findingsPath: findings("2026-10-12", true), "README.md": "edited\n"})
		f.research.files = []string{findingsPath}
		f.research.open = []map[string]any{f.researchPR()}
		f.labels["AFK:failed"] = true
		f.mustFire(1)
		f.refusedOn(t, "not-docs-only: README.md")
		record := f.record()
		if record["kind"] != "reconcile" || record["reconcile"] != "docs-merge" || record["action"] != "docs-merge" || record["pr"] != 55.0 {
			t.Errorf("record = %v", record)
		}
		if !f.drafted() || len(f.pr.comments) != 1 || !strings.Contains(f.pr.comments[0], "refused this research PR: not-docs-only: README.md") {
			t.Errorf("drafted %v, PR comments %q", f.drafted(), f.pr.comments)
		}
		if strings.Contains(f.writes(), "PUT ") || len(f.claude.Calls()) != 0 {
			t.Errorf("writes %s, %d calls", f.writes(), len(f.claude.Calls()))
		}
	})
	t.Run("an open research PR the gate approves is merged and its ticket resolved, no Step called", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		head := f.pushResearch(map[string]string{findingsPath: findings("2026-10-12", true)})
		f.research.files = []string{findingsPath}
		f.research.open = []map[string]any{f.researchPR()}
		f.labels["AFK:failed"] = true
		f.mustFire(0)
		if want := "POST " + lockPath + "\n" + landed; f.writes() != want {
			t.Errorf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		// The gist is the merged file's TL;DR.
		if f.labelled() != "AFK:done" || f.research.ticketState != "closed" || len(f.comments) != 1 || !strings.HasPrefix(f.comments[0].Body, "- the docs-only gate\n\nFull findings: [") {
			t.Errorf("labels %q, ticket %s, comments %+v", f.labelled(), f.research.ticketState, f.comments)
		}
		if resolve := resolveOf(f.record()); resolve["merged"] != f.research.merged || resolve["gate"].(map[string]any)["sha"] != head || resolve["resolved"] != true {
			t.Errorf("resolve = %v", resolve)
		}
		f.noOtherStep()
		if len(f.claude.Calls()) != 0 {
			t.Errorf("%d calls", len(f.claude.Calls()))
		}
	})
	t.Run("the merge is pinned to the head the gate read: a head moved since is not merged", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		head := f.pushResearch(map[string]string{findingsPath: findings("2026-10-12", true)})
		f.research.moved = git(t, f.remote, "commit-tree", head+"^{tree}", "-p", head, "-m", "pushed by someone")
		f.research.files = []string{findingsPath}
		f.research.open = []map[string]any{f.researchPR()}
		f.mustFire(1)
		var merge struct{ SHA string }
		json.Unmarshal([]byte(f.gh.Writes()[1].Body), &merge)
		if merge.SHA != head {
			t.Errorf("the merge was pinned to %q, not the head read %s", merge.SHA, head)
		}
		f.refusedOn(t, "merge-failed: ")
		if !strings.Contains(f.comments[len(f.comments)-1].Body, "Head branch was modified") {
			t.Errorf("the comment is %q", f.comments[len(f.comments)-1].Body)
		}
	})
	t.Run("a Map append that fails is a warning; the ticket is still resolved", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.research.mapRefused = true
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		record := f.record()
		if f.labelled() != "AFK:done" || f.research.ticketState != "closed" || record["phase"] != "done" || resolveOf(record)["resolved"] != true ||
			!strings.Contains(fmt.Sprint(record["warnings"]), "the Map #9 append failed") || f.research.mapBody != mapBody {
			t.Errorf("labels %q, ticket %s, record %v", f.labelled(), f.research.ticketState, record)
		}
	})

	// Behaviour 3.
	t.Run("a marked research PR merged by a human: the next Fire resolves its ticket from the marker and the TL;DR; run again, it does nothing", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.pushResearch(map[string]string{findingsPath: findings("2026-10-12", true)})
		f.merge()
		f.research.closed = []map[string]any{f.researchPR()}
		f.labels["AFK:failed"] = true
		f.queue = []githubtest.Reply{queueReply(failedQueued)}
		f.mustFire(0)
		if want := "POST " + lockPath + "\nPOST " + commentsPath + "\nPATCH /repos/acme/widgets/issues/20\nPOST " + lockPath +
			"\nDELETE /repos/acme/widgets/issues/20/labels/AFK:failed\nPATCH /repos/acme/widgets/issues/9\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		want := "- the docs-only gate\n\nFull findings: [" + findingsPath + "](https://github.com/acme/widgets/blob/trunk/" + findingsPath + ") (merged in #55)."
		if f.labelled() != "AFK:done" || f.research.ticketState != "closed" || len(f.comments) != 1 || f.comments[0].Body != want ||
			!strings.Contains(f.research.mapBody, "- [Which merge gate?](https://github.com/acme/widgets/issues/20): the docs-only gate\n") {
			t.Errorf("labels %q, ticket %s, comments %+v, map %s", f.labelled(), f.research.ticketState, f.comments, f.research.mapBody)
		}
		record := f.record()
		if record["kind"] != "finish" || record["action"] != "finish" || record["pr"] != 55.0 || record["phase"] != "done" ||
			resolveOf(record)["merged"] != f.research.merged || resolveOf(record)["findings"] != findingsPath || resolveOf(record)["gate"] != nil {
			t.Errorf("record = %v", record)
		}
		if len(f.claude.Calls()) != 0 {
			t.Errorf("%d calls: a finish does no research", len(f.claude.Calls()))
		}

		writes := len(f.gh.Writes())
		f.mustFire(0)
		if len(f.gh.Writes()) != writes || len(f.comments) != 1 || f.record()["phase"] != "done" {
			t.Errorf("the second Fire wrote %s", f.writes())
		}
	})
	t.Run("a human-merged research PR whose findings file has no TL;DR: the ticket and the PR are AFK:failed, and the next Fire picks on", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.pushResearch(map[string]string{findingsPath: strings.Replace(findings("2026-10-12", true), "## TL;DR", "## Summary", 1)})
		f.merge()
		f.research.closed = []map[string]any{f.researchPR()}
		// GitHub queues the ticket as each Fire finds it.
		f.queue = []githubtest.Reply{queueReply(researchQueued), queueReply(failedQueued)}
		f.mustFire(1)
		record := f.record()
		if f.labelled() != "AFK:failed" || f.research.ticketState != "open" || record["kind"] != "finish" || record["phase"] != "failed" || resolveOf(record)["failed"] != "finish-failed" {
			t.Errorf("labels %q, ticket %s, record %v", f.labelled(), f.research.ticketState, record)
		}
		if strings.Join(f.pr.labels, ",") != "AFK:failed" {
			t.Errorf("PR labels %q", f.pr.labels)
		}
		if last := f.comments[len(f.comments)-1].Body; !strings.HasPrefix(last, "afk-resolve FAILED at ") ||
			!strings.Contains(last, "finish-failed: research PR https://github.com/acme/widgets/pull/55 is merged, but this ticket cannot be resolved from it: the findings file "+findingsPath+" has no TL;DR") {
			t.Errorf("the ticket's comment is %q", last)
		}

		// GitHub lists the merged PR with its label now: the ticket is a
		// human's, and no longer holds the Lane.
		f.research.closed = []map[string]any{f.researchPR()}
		writes := len(f.gh.Writes())
		f.mustFire(0)
		if record := f.record(); record["kind"] != "idle" || len(f.gh.Writes()) != writes {
			t.Errorf("the next Fire wrote %s; record %v", f.writes(), record)
		}
	})
	t.Run("a merged research PR whose ticket is closed is no work at all", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.pushResearch(map[string]string{findingsPath: findings("2026-10-12", true)})
		f.merge()
		f.research.closed = []map[string]any{f.researchPR()}
		f.queue = []githubtest.Reply{queueReply()}
		f.mustFire(0)
		if f.writes() != "" || f.record()["kind"] != "idle" {
			t.Errorf("writes %s, record %v", f.writes(), f.record())
		}
	})
}
