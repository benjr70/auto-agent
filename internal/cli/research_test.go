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

// newResearchFixture is the research world, its Map read with parent.
func newResearchFixture(t *testing.T, parent githubtest.Reply) *fireFixture {
	t.Helper()
	f := newFireFixture(t, labelsConfig)
	f.queue = []githubtest.Reply{queueReply(researchQueued)}
	f.gh.Handle("GET", "/repos/acme/widgets/issues/20", githubtest.JSON(200, map[string]any{
		"number": 20, "title": "Which merge gate?", "body": "## Question\n\nWhich merge gate lands a research PR?\n", "state": "open",
		"labels": []map[string]string{{"name": "AFK"}, {"name": "wayfinder:research"}},
	}))
	f.gh.HandleGraphQL("parent { number title body }", parent)
	f.gh.Handle("POST", assignPath, githubtest.JSON(201, map[string]any{"number": 20}))
	f.gh.Handle("POST", pullsPath, githubtest.JSON(201, map[string]any{"number": 55, "head": map[string]any{"ref": "research/" + researchSlug}}))
	return f
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

// resolveOf is the resolve block of a Fire record.
func resolveOf(record map[string]any) map[string]any {
	resolve, _ := record["resolve"].(map[string]any)
	return resolve
}

func TestFireResearch(t *testing.T) {
	// Behaviour 1.
	t.Run("a research ticket under a Map: claimed, researched, the file committed and the PR opened with its title, body and marker", func(t *testing.T) {
		f := newResearchFixture(t, mapParent)
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		if want := "POST " + assignPath + "\nPOST " + lockPath + "\nPOST " + pullsPath + "\nDELETE " + unlockPath; f.writes() != want {
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
			git(t, f.remote, "diff", "--name-only", "trunk", head) != findingsPath || git(t, f.remote, "show", head+":"+findingsPath)+"\n" != findings("2026-10-12", true) {
			t.Errorf("the remote's branch is %q", head)
		}
		record := f.record()
		resolve := resolveOf(record)
		if record["kind"] != "pick-decision" || record["action"] != "research" || record["phase"] != "done" || record["pr"] != 55.0 || record["branch"] != "research/"+researchSlug || record["commit"] != head ||
			resolve["type"] != "research" || resolve["slug"] != researchSlug || resolve["findings"] != findingsPath || resolve["pr"] != 55.0 || resolve["failed"] != "" {
			t.Errorf("record = %v", record)
		}
		if steps := stepsOf(record); len(steps) != 1 || steps[0]["name"] != "research" || steps[0]["outcome"] != "OK" {
			t.Errorf("steps = %v", steps)
		}
		if f.labelled() != "" {
			t.Errorf("the ticket is left %q", f.labelled())
		}
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
		if head := f.remoteRef(researchRef); head == "" || git(t, f.remote, "diff", "--name-only", "trunk", head) != findingsPath || f.record()["pr"] != 55.0 {
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
		if record["kind"] != "resume" || record["pr"] != 55.0 || f.labelled() != "" ||
			git(t, f.remote, "log", "-1", "--format=%s", researchRef) != researchTitle || git(t, f.remote, "diff", "--name-only", "trunk", researchRef) != findingsPath {
			t.Errorf("labels %q, record %v", f.labelled(), record)
		}
	})
}
