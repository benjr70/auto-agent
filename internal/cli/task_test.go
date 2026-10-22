package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/research"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// These tests run whole Fires on ticket #20 as a task Decision ticket, a
// sub-issue of Map #9. Its Research Step, in task mode, is the fixture's
// fake claude.
//
// Ported: lib/pickup-triage.test.sh's "wayfinder:task pick: type task" is
// internal/state's case of that name. Of lib/fire.test.sh's
// test_crashed_resolve_respects_its_terminal_line, the task cases "settled
// DONE: AFK:done restored, no comment" and "settled HITL: nothing touched"
// are dropped: they scraped the skill's terminal resolve: line after a
// crash, and Go has none to scrape. A Go task settles on GitHub before the
// lock comes off: a done task is closed, and a HITL one holds no AFK and no
// lock, so neither is a crashed Fire for the next one to clear up.

const taskQueued = `{"number":20,"title":"Provision the sandbox key","createdAt":"2026-02-01T00:00:00Z","labels":{"nodes":[{"name":"AFK"},{"name":"wayfinder:task"}]},
	"blockedBy":{"nodes":[],"pageInfo":{"hasNextPage":false}},"assignees":{"nodes":[]}}`

// newTaskFixture is the research world with ticket #20 a task.
func newTaskFixture(t *testing.T) *fireFixture {
	t.Helper()
	f := newResearchFixture(t, mapParent)
	f.queue = []githubtest.Reply{queueReply(taskQueued)}
	f.research.ticketBody = "## Question\n\nSign up for the sandbox and store its key.\n"
	w := f.research
	f.gh.HandleFunc("GET", "/repos/acme/widgets/issues/20", func(githubtest.Request) githubtest.Reply {
		labels := []map[string]string{{"name": "AFK"}, {"name": "wayfinder:task"}}
		for label := range f.labels {
			labels = append(labels, map[string]string{"name": label})
		}
		return githubtest.JSON(200, map[string]any{"number": 20, "title": "Provision the sandbox key", "body": w.ticketBody, "state": w.ticketState, "labels": labels})
	})
	f.gh.HandleFunc("DELETE", "/repos/acme/widgets/issues/20/labels/AFK", func(githubtest.Request) githubtest.Reply {
		return githubtest.Text(200, `[]`)
	})
	return f
}

func taskInit(session string) string {
	return steptest.Init(session, "claude-opus", append([]string{"StructuredOutput"}, research.TaskSpec().Tools...), []string{"factory:research"})
}

// tasked is the fake Step in task mode running run in the clone, then
// answering result.
func tasked(session, run string, result research.TaskResult) steptest.Reply {
	return steptest.Reply{Run: run, Lines: []string{taskInit(session), steptest.Success(result)}}
}

var taskDone = research.TaskResult{
	Outcome: research.TaskDone, Summary: "Signed up for the sandbox and stored its key.",
	Facts: []string{"the key lives in the vault at ci/sandbox", "the API is at https://sandbox.example.com/v2"},
}

// remoteRefs are every ref the remote holds.
func (f *fireFixture) remoteRefs() string {
	f.t.Helper()
	return git(f.t, f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
}

func TestFireTask(t *testing.T) {
	t.Run("a task ticket picked: Step run, then the comment, the close and AFK:done, with no branch and no PR", func(t *testing.T) {
		f := newTaskFixture(t)
		refs := f.remoteRefs()
		f.claude.Script(tasked("task-1", "", taskDone))
		f.mustFire(0)
		if want := "POST " + assignPath + "\nPOST " + lockPath + "\nPOST " + commentsPath + "\nPATCH /repos/acme/widgets/issues/20\nPOST " + lockPath +
			"\nPATCH /repos/acme/widgets/issues/9\nDELETE " + unlockPath; f.writes() != want {
			t.Fatalf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		want := "Signed up for the sandbox and stored its key.\n\nFacts for later tickets:\n\n- the key lives in the vault at ci/sandbox\n- the API is at https://sandbox.example.com/v2"
		if len(f.comments) != 1 || f.comments[0].Body != want {
			t.Errorf("comments = %+v", f.comments)
		}
		if f.labelled() != "AFK:done" || f.research.ticketState != "closed" {
			t.Errorf("the ticket is left %s, %q", f.research.ticketState, f.labelled())
		}
		if got := f.remoteRefs(); got != refs {
			t.Errorf("the remote's refs went from\n%s\nto\n%s", refs, got)
		}
		line := "- [Provision the sandbox key](https://github.com/acme/widgets/issues/20): Signed up for the sandbox and stored its key. Facts for later tickets: the key lives in the vault at ci/sandbox the API is at https://sandbox.example.com/v2"
		if !strings.Contains(f.research.mapBody, "issues/3): old\n"+line+"\n") {
			t.Errorf("the Map is\n%s", f.research.mapBody)
		}
		record := f.record()
		resolve := resolveOf(record)
		if record["kind"] != "pick-decision" || record["action"] != "task" || record["phase"] != "done" || record["pr"] != nil || record["branch"] != "" ||
			resolve["type"] != "task" || resolve["resolved"] != true || resolve["hitl"] != false || resolve["failed"] != "" || resolve["pr"] != nil {
			t.Errorf("record = %v", record)
		}
		f.noOtherStep()
		calls := f.claude.Calls()
		if len(calls) != 1 {
			t.Fatalf("%d calls", len(calls))
		}
		var in map[string]any
		json.Unmarshal([]byte(strings.TrimPrefix(calls[0].Stdin, "/factory:research ")), &in)
		if _, hasPath := in["path"]; hasPath || in["question"] != "Sign up for the sandbox and store its key." || in["map"] == nil {
			t.Errorf("the Step was told %v", in)
		}
	})

	t.Run("the clone is reset afterwards; nothing the task Step changed is pushed", func(t *testing.T) {
		f := newTaskFixture(t)
		refs := f.remoteRefs()
		run := "echo edited >> README.md && mkdir -p secrets && echo s3cret > secrets/key && printf 'secrets/\\n' > .gitignore && echo new > new.txt && " +
			"git -c user.name=x -c user.email=x@x add -A && git -c user.name=x -c user.email=x@x commit --quiet -m 'task work' && git checkout --quiet -b elsewhere && echo more > more.txt"
		f.claude.Script(tasked("task-1", run, taskDone))
		f.mustFire(0)
		if got := f.remoteRefs(); got != refs {
			t.Errorf("the remote's refs went from\n%s\nto\n%s", refs, got)
		}
		clone := filepath.Join(f.state, "clone")
		if status := git(t, clone, "status", "--porcelain", "--ignored"); status != "" {
			t.Errorf("the clone still has\n%s", status)
		}
		if head, trunk := git(t, clone, "rev-parse", "HEAD"), git(t, f.remote, "rev-parse", "trunk"); head != trunk || git(t, clone, "rev-parse", "--abbrev-ref", "HEAD") != "trunk" {
			t.Errorf("the clone is at %s on %s, want trunk at %s", head, git(t, clone, "rev-parse", "--abbrev-ref", "HEAD"), trunk)
		}
		if f.research.ticketState != "closed" {
			t.Errorf("the ticket is %s", f.research.ticketState)
		}
	})

	t.Run("needs-code relabels HITL, comments and unpublishes", func(t *testing.T) {
		f := newTaskFixture(t)
		f.claude.Script(tasked("task-1", "", research.TaskResult{Outcome: research.TaskNeedsCode, Summary: "A --sandbox flag in the CLI, and its client.", Facts: []string{}}))
		f.mustFire(0)
		if want := "POST " + assignPath + "\nPOST " + lockPath + "\nPOST " + lockPath + "\nDELETE /repos/acme/widgets/issues/20/labels/AFK\nPOST " + commentsPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Fatalf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		if writes := f.gh.Writes(); writes[2].Body != `{"labels":["HITL"]}` {
			t.Errorf("the relabel was %s", writes[2].Body)
		}
		want := "afk-resolve: this task needs a change to product code, which a Decision ticket never carries — relabelled HITL for a human to route (as a Slice off a Spec, or by re-scoping the ticket). What it would take: A --sandbox flag in the CLI, and its client."
		if len(f.comments) != 1 || f.comments[0].Body != want {
			t.Errorf("comments = %+v", f.comments)
		}
		if f.labelled() != "HITL" || f.research.ticketState != "open" {
			t.Errorf("the ticket is left %s, %q", f.research.ticketState, f.labelled())
		}
		record := f.record()
		if resolve := resolveOf(record); record["phase"] != "done" || resolve["hitl"] != true || resolve["resolved"] != false || resolve["failed"] != "" {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("needs-human fails the ticket with the comment; the ticket stays open", func(t *testing.T) {
		f := newTaskFixture(t)
		f.claude.Script(tasked("task-1", "", research.TaskResult{Outcome: research.TaskNeedsHuman, Summary: "An org admin must approve the sandbox app.", Facts: []string{}}))
		f.mustFire(1)
		if want := "POST " + assignPath + "\nPOST " + lockPath + "\nPOST " + lockPath + "\nPOST " + commentsPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Fatalf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		want := "afk-resolve FAILED at 2026-10-12T09:30:00Z — needs-human: An org admin must approve the sandbox app. Ticket left open for human triage."
		if f.labelled() != "AFK:failed" || len(f.comments) != 1 || f.comments[0].Body != want || f.research.ticketState != "open" {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
		if record := f.record(); resolveOf(record)["failed"] != "needs-human" || record["phase"] != "failed" {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("an answer that fails its check is corrected once, then the attempt fails", func(t *testing.T) {
		f := newTaskFixture(t)
		f.claude.Script(tasked("task-1", "", research.TaskResult{Outcome: research.TaskDone, Summary: "One.\n\nTwo.", Facts: []string{}}))
		f.mustFire(1)
		calls := f.claude.Calls()
		if resume, _ := calls[len(calls)-1].Arg("--resume"); len(calls) != 2 || resume != "task-1" || !strings.Contains(calls[1].Stdin, "more than one paragraph") {
			t.Errorf("%d calls, the correction resumed %q", len(calls), resume)
		}
		if record := f.record(); !strings.Contains(fmt.Sprint(record["error"]), "failed attempt 1 of 3") || f.research.ticketState != "open" {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("an interrupted task Step is paused with nothing pushed, and the next Fire resumes it by its session", func(t *testing.T) {
		f := newTaskFixture(t)
		refs := f.remoteRefs()
		f.claude.Script(
			steptest.Reply{Run: "echo partial > partial.txt", ExitCode: 1, Lines: []string{taskInit("task-9")}, Stderr: "You've hit your session limit · resets 10:50pm (America/New_York)"},
			tasked("task-9", "", taskDone),
		)
		f.mustFire(0)
		if f.labelled() != "AFK:paused" || f.record()["resumeSession"] != "task-9" || f.remoteRefs() != refs {
			t.Fatalf("labels %q, record %v, refs\n%s", f.labelled(), f.record(), f.remoteRefs())
		}
		if status := git(t, filepath.Join(f.state, "clone"), "status", "--porcelain", "--ignored"); status != "" {
			t.Errorf("the paused Fire left the clone with\n%s", status)
		}
		f.later(18 * time.Hour)
		f.mustFire(0)
		calls := f.claude.Calls()
		if resume, _ := calls[1].Arg("--resume"); len(calls) != 2 || resume != "task-9" {
			t.Errorf("the second Fire's call resumed %q", resume)
		}
		if record := f.record(); record["kind"] != "resume" || record["action"] != "resume" || f.labelled() != "AFK:done" || f.research.ticketState != "closed" || f.remoteRefs() != refs {
			t.Errorf("labels %q, record %v", f.labelled(), record)
		}
	})
}
