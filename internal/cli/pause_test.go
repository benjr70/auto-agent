package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// These tests run whole Fires, one after another, against a fake GitHub that
// keeps the ticket's labels and comments: what one Fire leaves is what the
// next one derives its state from.

func implementInit(session string) string {
	return steptest.Init(session, "claude-opus", append([]string{"StructuredOutput"}, implement.Spec().Tools...), []string{"factory:implement"})
}

// The replies of the fake Step.
var (
	// cutOff: the usage limit stops the Step after it has edited a file.
	cutOff = steptest.Reply{
		Run: "echo half > greeting.txt", ExitCode: 1,
		Lines:  []string{implementInit("session-9")},
		Stderr: "You've hit your session limit · resets 10:50pm (America/New_York)",
	}
	crash = steptest.Reply{Stderr: "segfault", ExitCode: 139}
)

// done is the Step finishing, having noted in the out dir what the checkout
// held when it started.
func (f *fireFixture) done(session string) steptest.Reply {
	return steptest.Reply{
		Run:   fmt.Sprintf("cat greeting.txt > '%s/seen' 2>/dev/null; echo hello > greeting.txt", f.out),
		Lines: []string{implementInit(session), steptest.Success(finished)},
	}
}

// fixedRunner is a Step that always ends the same way.
type fixedRunner struct{ run step.Run }

func (r fixedRunner) Run(context.Context, step.Spec, step.Call) step.Run { return r.run }

func (f *fireFixture) mustFire(want int) {
	f.t.Helper()
	f.stderr.Reset()
	if code := f.fire(); code != want {
		f.t.Fatalf("exit %d, want %d (stderr %q)", code, want, f.stderr.String())
	}
}

// stepsOf is the Step entries of a Fire record.
func stepsOf(record map[string]any) []map[string]any {
	var steps []map[string]any
	for _, entry := range record["steps"].([]any) {
		steps = append(steps, entry.(map[string]any))
	}
	return steps
}

func TestFirePauseAndResume(t *testing.T) {
	const pauseComment = "Fire paused at 2026-10-12T09:30:00Z: usage exhausted mid-Fire. Branch kept for resume. Budget resets at 2026-10-13T02:50:00.000Z."

	// Ported from lib/fire.test.sh: test_exhausted_pick_pauses. The bash
	// wrapper exited with claude's code; a paused Fire here exits 0.
	t.Run("an EXHAUSTED pick Fire pauses its work: wip commit, AFK:in-progress -> AFK:paused, exit 0", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(cutOff)
		f.mustFire(0)
		// The pause label goes on before the lock comes off: the ticket is
		// never without both.
		if want := "POST " + lockPath + "\nPOST " + lockPath + "\nPOST " + commentsPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		if f.labelled() != "AFK:paused" || len(f.comments) != 1 || f.comments[0].Body != pauseComment {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
		head := f.remoteRef("refs/heads/feat/issue-20")
		if head == "" || git(t, f.remote, "log", "-1", "--format=%s", head) != "wip: freeze partial work on #20 (usage exhausted)" || git(t, f.remote, "show", head+":greeting.txt") != "half" {
			t.Errorf("the remote's branch is %q", head)
		}
		record := f.record()
		entry := stepsOf(record)[0]
		if record["exit"] != 0.0 || record["error"] != "" || record["phase"] != "paused" || record["commit"] != head || record["resetAt"] != "2026-10-13T02:50:00.000Z" ||
			entry["outcome"] != "EXHAUSTED" || entry["sessionId"] != "session-9" {
			t.Errorf("record = %v", record)
		}
	})
	t.Run("a clean tree pauses without a commit", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(steptest.Reply{ExitCode: 1, Lines: []string{implementInit("session-9")}, Stderr: "Claude AI usage limit reached|1791800000"})
		f.mustFire(0)
		if f.remoteRef("refs/heads/feat/issue-20") != "" || f.labelled() != "AFK:paused" || f.record()["commit"] != "" {
			t.Errorf("branch %q, labels %q", f.remoteRef("refs/heads/feat/issue-20"), f.labelled())
		}
	})
	// Ported from lib/fire.test.sh: test_rejected_event_is_the_outcome.
	t.Run("a rejected rate-limit event in this Fire's stream is the exhaustion signal, its resetsAt the reset", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(steptest.Reply{ExitCode: 1, Stderr: "API Error: something went wrong", Lines: []string{implementInit("session-9"), steptest.RateLimit("rejected", 1791800000, "seven_day")}})
		f.mustFire(0)
		if record := f.record(); f.labelled() != "AFK:paused" || record["resetAt"] != "2026-10-12T10:13:20Z" || !strings.Contains(f.comments[0].Body, "Budget resets at 2026-10-12T10:13:20Z.") {
			t.Errorf("labels %q, record %v, comments %+v", f.labelled(), record, f.comments)
		}
	})

	t.Run("the next Fire resumes the same session by the id in the Fire record, and finishes the ticket", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(cutOff, f.done("session-9"))
		f.mustFire(0)
		f.later(18 * time.Hour) // past the reset
		f.mustFire(0)

		calls := f.claude.Calls()
		if len(calls) != 2 {
			t.Fatalf("%d Step calls, want 2", len(calls))
		}
		if session, ok := calls[1].Arg("--resume"); !ok || session != "session-9" || !strings.Contains(calls[1].Stdin, "This Step was cut off, by the usage limit") || strings.Contains(calls[1].Stdin, "/factory:implement") {
			t.Errorf("--resume = %q %v, stdin = %q", session, ok, calls[1].Stdin)
		}
		// The resumed session finds its partial work in the checkout.
		if seen, _ := os.ReadFile(filepath.Join(f.out, "seen")); strings.TrimSpace(string(seen)) != "half" {
			t.Errorf("the resumed Step saw %q in the checkout", seen)
		}
		records := f.records()
		second := records[len(records)-1]
		entry := stepsOf(second)[0]
		if second["kind"] != "resume" || second["pr"] != 55.0 || second["issue"] != 20.0 || entry["resume"] != "paused" || entry["outcome"] != "OK" || f.labelled() != "" {
			t.Errorf("second Fire = %v, labels %q", second, f.labelled())
		}
		// The second Fire: the lock on, the pause off, the PR, the lock off.
		writes := strings.Split(f.writes(), "\n")
		if want := "POST " + lockPath + "\nDELETE " + unpausePath + "\nPOST " + pullsPath + "\nDELETE " + unlockPath; strings.Join(writes[len(writes)-4:], "\n") != want {
			t.Errorf("the second Fire sent\n%s\nwant\n%s", strings.Join(writes[len(writes)-4:], "\n"), want)
		}
	})

	t.Run("a paused ticket is resumed before any new pick, and the queue is not read", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(cutOff, f.done("session-9"))
		f.mustFire(0)
		before := len(f.gh.Requests())
		f.later(18 * time.Hour)
		f.mustFire(0)
		for _, r := range f.gh.Requests()[before:] {
			if r.Path == "/graphql" {
				t.Error("the resume Fire read the queue")
			}
		}
	})

	t.Run("when the session id is unavailable the Step is re-run from the last pushed commit", func(t *testing.T) {
		for name, lose := range map[string]func(*fireFixture){
			"the Fire record is gone": func(f *fireFixture) { os.RemoveAll(filepath.Join(f.state, "fires")) },
			"Claude no longer has the session": func(f *fireFixture) {
				f.claude.Script(steptest.Reply{ExitCode: 1, Stderr: "No conversation found with session ID: session-9"}, f.done("session-10"))
			},
		} {
			t.Run(name, func(t *testing.T) {
				f := newFireFixture(t, labelsConfig)
				f.claude.Script(cutOff, f.done("session-10"))
				f.mustFire(0)
				lose(f)
				f.later(18 * time.Hour)
				f.mustFire(0)
				calls := f.claude.Calls()
				last := calls[len(calls)-1]
				if _, resumed := last.Arg("--resume"); resumed || !strings.HasPrefix(last.Stdin, "/factory:implement {") {
					t.Errorf("the last call resumed a session: stdin %q", last.Stdin)
				}
				if seen, _ := os.ReadFile(filepath.Join(f.out, "seen")); strings.TrimSpace(string(seen)) != "half" {
					t.Errorf("the re-run Step saw %q in the checkout, want the pushed partial work", seen)
				}
				records := f.records()
				if second := records[len(records)-1]; second["pr"] != 55.0 || f.labelled() != "" {
					t.Errorf("second Fire = %v, labels %q", second, f.labelled())
				}
				if name == "Claude no longer has the session" {
					steps := stepsOf(records[len(records)-1])
					if len(calls) != 3 || len(steps) != 2 || steps[0]["resume"] != "paused" || steps[0]["outcome"] != "FAILED" || steps[1]["resume"] != "" || steps[1]["outcome"] != "OK" {
						t.Errorf("%d calls, steps = %v", len(calls), steps)
					}
				}
			})
		}
	})

	t.Run("a Budget gate that says wait leaves a paused ticket paused", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(cutOff)
		f.mustFire(0)
		writes := f.writes()
		f.later(time.Minute) // long before the reset
		f.mustFire(0)
		records := f.records()
		if second := records[1]; f.writes() != writes || f.labelled() != "AFK:paused" || len(f.claude.Calls()) != 1 || second["kind"] != "resume" || second["resetAt"] == "" {
			t.Errorf("writes\n%s\nlabels %q, second Fire %v", f.writes(), f.labelled(), second)
		}
	})

	// lib/pause-resume.test.sh: "the cap from the config (3rd arg) lowers the
	// limit", as whole Fires.
	t.Run("the pause cap is the Harness config's rounds.pause_resume: at the cap the ticket gets AFK:failed", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"pause_resume":2}}`)
		f.claude.Script(cutOff)
		f.mustFire(0) // the first pause
		f.later(18 * time.Hour)
		f.mustFire(0) // resumed, and cut off again: the second pause
		if f.labelled() != "AFK:paused" || len(f.claude.Calls()) != 2 {
			t.Fatalf("after two Fires: labels %q, %d Step calls", f.labelled(), len(f.claude.Calls()))
		}
		f.later(24 * time.Hour)
		f.mustFire(0)
		if f.labelled() != "AFK:failed" || len(f.claude.Calls()) != 2 {
			t.Errorf("labels %q, %d Step calls", f.labelled(), len(f.claude.Calls()))
		}
		last := f.comments[len(f.comments)-1].Body
		if len(f.comments) != 3 || !strings.Contains(last, "Paused 2 times") || !strings.Contains(last, "`AFK:failed` applied") {
			t.Errorf("comments = %+v", f.comments)
		}
		records := f.records()
		if third := records[2]; third["kind"] != "pause-cap" || third["exit"] != 0.0 || third["issue"] != 20.0 || len(stepsOf(third)) != 0 {
			t.Errorf("third Fire = %v", third)
		}
	})
	// lib/pause-resume.test.sh: "reaching the cap fails the issue (default cap 3)".
	t.Run("reaching the cap fails the issue (default cap 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.labels["AFK:paused"] = true
		for i := 0; i < 3; i++ {
			f.comment("machine-user", pauseComment)
		}
		f.mustFire(0)
		if want := "POST " + lockPath + "\nDELETE " + unpausePath + "\nPOST " + commentsPath; f.writes() != want || f.labelled() != "AFK:failed" || len(f.claude.Calls()) != 0 {
			t.Errorf("GitHub received\n%s\nlabels %q", f.writes(), f.labelled())
		}
	})

	t.Run("a resumed Step stopped by a dead credential is still a paused ticket, and the Fire after resumes the same session", func(t *testing.T) {
		for name, dead := range map[string]steptest.Reply{
			"after the session started":  {ExitCode: 1, Lines: []string{implementInit("session-9")}, Stderr: "Failed to authenticate: OAuth session expired"},
			"before the session started": {ExitCode: 1, Stderr: "Failed to authenticate: OAuth session expired"},
		} {
			t.Run(name, func(t *testing.T) {
				f := newFireFixture(t, labelsConfig)
				f.claude.Script(cutOff, dead, f.done("session-9"))
				f.mustFire(0)
				f.later(18 * time.Hour)
				f.mustFire(1)
				// No second pause is counted: it was not the usage limit.
				if f.labelled() != "AFK:paused" || len(f.comments) != 2 || state.IsPause(f.comments[1].Body) || len(f.openParks()) != 1 {
					t.Fatalf("labels %q, comments %+v", f.labelled(), f.comments)
				}
				f.later(time.Hour)
				f.mustFire(0)
				calls := f.claude.Calls()
				if session, _ := calls[len(calls)-1].Arg("--resume"); len(calls) != 3 || session != "session-9" || f.labelled() != "" {
					t.Errorf("%d calls, the last resumed %q, labels %q", len(calls), session, f.labelled())
				}
			})
		}
	})
	t.Run("a resume Fire that fails before its Step leaves the ticket paused", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(cutOff, f.done("session-9"))
		f.mustFire(0)
		f.later(18 * time.Hour)
		remote := f.environ[EnvGitRemote]
		os.Rename(remote, remote+".away")
		f.mustFire(1)
		os.Rename(remote+".away", remote)
		if f.labelled() != "AFK:paused" || len(f.claude.Calls()) != 1 {
			t.Fatalf("labels %q, %d Step calls", f.labelled(), len(f.claude.Calls()))
		}
		f.later(time.Hour)
		f.mustFire(0)
		if calls := f.claude.Calls(); len(calls) != 2 || f.labelled() != "" {
			t.Errorf("%d calls, labels %q", len(calls), f.labelled())
		} else if session, _ := calls[1].Arg("--resume"); session != "session-9" {
			t.Errorf("the last call resumed %q", session)
		}
	})
	t.Run("pause comments someone else wrote are not held against the cap", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(cutOff, f.done("session-9"))
		f.mustFire(0)
		for i := 0; i < 3; i++ {
			f.comment("a-passer-by", pauseComment)
		}
		f.later(18 * time.Hour)
		f.mustFire(0)
		if f.labelled() != "" || len(f.claude.Calls()) != 2 {
			t.Errorf("labels %q, %d Step calls", f.labelled(), len(f.claude.Calls()))
		}
	})
	t.Run("a paused ticket whose Agent PR is open only has its label taken off", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.labels["AFK:paused"] = true
		f.gh.Handle("GET", pullsPath, githubtest.Text(200, `[{"number":55,"head":{"ref":"feat/issue-20","repo":{"full_name":"acme/widgets"}}}]`))
		f.mustFire(0)
		if f.labelled() != "" || len(f.claude.Calls()) != 0 {
			t.Errorf("labels %q, %d Step calls", f.labelled(), len(f.claude.Calls()))
		}
	})
}

func TestFireFailedAttempts(t *testing.T) {
	badScope := implement.Result{Finished: true, Summary: "s", CommitMessage: "feat(nope): x", Checklist: []string{"c"}}
	malformed := steptest.Reply{Run: "echo hello > greeting.txt", Lines: []string{implementInit("session-9"), steptest.Success(badScope)}}

	t.Run("a MALFORMED result is resumed once in the same session with the check's error, and a corrected result is used", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(malformed, steptest.Reply{Lines: []string{implementInit("session-9"), steptest.Success(finished)}})
		f.mustFire(0)
		calls := f.claude.Calls()
		if len(calls) != 2 {
			t.Fatalf("%d Step calls, want 2", len(calls))
		}
		if session, ok := calls[1].Arg("--resume"); !ok || session != "session-9" || !strings.Contains(calls[1].Stdin, `scope "nope"`) {
			t.Errorf("--resume = %q %v, stdin = %q", session, ok, calls[1].Stdin)
		}
		record := f.record()
		steps := stepsOf(record)
		if record["pr"] != 55.0 || len(steps) != 2 || steps[0]["outcome"] != "MALFORMED" || steps[0]["resume"] != "" || steps[1]["outcome"] != "OK" || steps[1]["resume"] != "correction" {
			t.Errorf("record = %v", record)
		}
		// It cost no attempt.
		if len(f.comments) != 0 || f.labelled() != "" {
			t.Errorf("comments %+v, labels %q", f.comments, f.labelled())
		}
	})
	t.Run("a result with no structured output is resumed the same way", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(
			steptest.Reply{Lines: []string{implementInit("session-9"), steptest.Result(map[string]any{"result": "all done!"})}},
			steptest.Reply{Run: "echo hello > greeting.txt", Lines: []string{implementInit("session-9"), steptest.Success(finished)}},
		)
		f.mustFire(0)
		if calls := f.claude.Calls(); len(calls) != 2 || !strings.Contains(calls[1].Stdin, "no structured result") || f.record()["pr"] != 55.0 {
			t.Errorf("%d calls, record %v", len(calls), f.record())
		}
	})
	t.Run("one correction resume, then a counted attempt: a correction that fails is not corrected again", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(malformed)
		f.mustFire(1)
		if len(f.claude.Calls()) != 2 || len(f.comments) != 1 || f.labelled() != "" {
			t.Fatalf("%d Step calls, comments %+v, labels %q", len(f.claude.Calls()), f.comments, f.labelled())
		}
		want := "<!-- auto-agent:attempts step=implement count=1 -->\nAttempt 1 of 3 at the implement Step failed: the implement Step ended MALFORMED: "
		if !strings.HasPrefix(f.comments[0].Body, want) || !strings.Contains(f.stderr.String(), "(failed attempt 1 of 3)") {
			t.Errorf("comment = %q, stderr %q", f.comments[0].Body, f.stderr.String())
		}
		if f.remoteRef("refs/heads/feat/issue-20") != "" {
			t.Error("a result that was never accepted was pushed")
		}
	})
	t.Run("a call that left no session cannot be corrected: it is a failed attempt at once", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(crash)
		f.mustFire(1)
		if len(f.claude.Calls()) != 1 || len(f.comments) != 1 {
			t.Errorf("%d Step calls, comments %+v", len(f.claude.Calls()), f.comments)
		}
	})

	t.Run("three failed attempts at the same Step apply AFK:failed and release the lock", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(crash)
		for attempt := 1; attempt <= 3; attempt++ {
			f.later(time.Minute)
			f.mustFire(1)
			// One marker, kept up to date; never a second comment.
			if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0].Body, fmt.Sprintf("<!-- auto-agent:attempts step=implement count=%d -->", attempt)) {
				t.Fatalf("after attempt %d: comments %+v", attempt, f.comments)
			}
			if want := map[bool]string{false: "", true: "AFK:failed"}[attempt == 3]; f.labelled() != want {
				t.Fatalf("after attempt %d: labels %q, want %q", attempt, f.labelled(), want)
			}
		}
		if !strings.Contains(f.comments[0].Body, "`AFK:failed` applied") || !strings.Contains(f.stderr.String(), "failed attempt 3 of 3: AFK:failed applied") {
			t.Errorf("comment %q, stderr %q", f.comments[0].Body, f.stderr.String())
		}
		records := f.records()
		if last := records[2]; len(records) != 3 || last["phase"] != "failed" || last["exit"] != 1.0 {
			t.Errorf("%d records, last %v", len(records), records[len(records)-1])
		}
		writes := strings.Split(f.writes(), "\n")
		// The label before the count that says it is on.
		if want := "POST " + lockPath + "\nPOST " + lockPath + "\nDELETE " + unpausePath + "\nPATCH " + commentPath + "1\nDELETE " + unlockPath; strings.Join(writes[len(writes)-5:], "\n") != want {
			t.Errorf("the third Fire sent\n%s\nwant\n%s", strings.Join(writes[len(writes)-5:], "\n"), want)
		}
	})
	// Story 42: a timed-out Step is retried like a crashed one.
	t.Run("a TIMED_OUT Step is a failed attempt", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.fixture.runner = fixedRunner{step.Run{Outcome: step.TimedOut, Detail: "the call outran the Step's timeout of 2h0m0s", SessionID: "session-9"}}
		f.mustFire(1)
		if len(f.comments) != 1 || !strings.Contains(f.comments[0].Body, "count=1 -->\nAttempt 1 of 3 at the implement Step failed: the implement Step ended TIMED_OUT") || f.labelled() != "" {
			t.Errorf("comments %+v, labels %q", f.comments, f.labelled())
		}
	})
	t.Run("an AFK:failed that cannot be applied does not start the count again", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.comment("machine-user", "<!-- auto-agent:attempts step=implement count=2 -->")
		f.claude.Script(crash)
		f.failLabel = true
		f.mustFire(1)
		f.later(time.Minute)
		f.mustFire(1)
		if !strings.Contains(f.comments[0].Body, "count=2 -->") || f.labelled() != "" || !strings.Contains(f.stderr.String(), "AFK:failed could not be applied") {
			t.Fatalf("comments %+v, labels %q, stderr %q", f.comments, f.labelled(), f.stderr.String())
		}
		f.failLabel = false
		f.later(time.Minute)
		f.mustFire(1)
		if !strings.Contains(f.comments[0].Body, "count=3 -->") || f.labelled() != "AFK:failed" {
			t.Errorf("comments %+v, labels %q", f.comments, f.labelled())
		}
	})
	t.Run("a marker someone else wrote is not the Daemon's count", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.comment("a-passer-by", "<!-- auto-agent:attempts step=implement count=2 -->")
		f.claude.Script(crash)
		f.mustFire(1)
		if f.labelled() != "" || len(f.comments) != 2 || !strings.Contains(f.comments[1].Body, "count=1 -->") {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
	})
	t.Run("an attempt that cannot be counted still fails the Fire and releases the lock", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.gh.Handle("GET", commentsPath, githubtest.Text(500, `{"message":"boom"}`))
		f.claude.Script(crash)
		f.mustFire(1)
		if f.labelled() != "" || !strings.Contains(f.stderr.String(), "the attempt was not counted") {
			t.Errorf("labels %q, stderr %q", f.labelled(), f.stderr.String())
		}
	})
}

func TestFireCrashRecovery(t *testing.T) {
	t.Run("a held AFK:in-progress with no live Fire is a crash: the attempt is counted, state is re-derived and work continues", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.labels["AFK:in-progress"] = true
		f.answer(finished)
		f.mustFire(0)
		// The stale lock off, the crash counted; then a Fire like any other.
		want := "DELETE " + unlockPath + "\nPOST " + commentsPath + "\nPOST " + lockPath + "\nPOST " + pullsPath + "\nDELETE " + unlockPath
		if f.writes() != want || f.labelled() != "" {
			t.Errorf("GitHub received\n%s\nwant\n%s\nlabels %q", f.writes(), want, f.labelled())
		}
		record := f.record()
		if record["kind"] != "crashed" || record["pr"] != 55.0 || record["issue"] != 20.0 || !strings.Contains(f.comments[0].Body, "count=1 -->\nAttempt 1 of 3 at the implement Step failed: the Fire working it ended without releasing") {
			t.Errorf("record = %v, comments %+v", record, f.comments)
		}
	})
	t.Run("a crashed Fire's pushed work is carried on from", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(cutOff, f.done("session-10"))
		f.mustFire(0) // leaves a wip commit on the branch
		delete(f.labels, "AFK:paused")
		f.labels["AFK:in-progress"] = true // as a Fire that died mid-Step leaves it
		f.later(18 * time.Hour)
		f.mustFire(0)
		if seen, _ := os.ReadFile(filepath.Join(f.out, "seen")); strings.TrimSpace(string(seen)) != "half" {
			t.Errorf("the Step saw %q in the checkout", seen)
		}
		records := f.records()
		if calls := f.claude.Calls(); len(calls) != 2 || records[1]["kind"] != "crashed" || records[1]["pr"] != 55.0 || f.labelled() != "" {
			t.Errorf("%d calls, second Fire %v, labels %q", len(calls), records[1], f.labelled())
		} else if _, resumed := calls[1].Arg("--resume"); resumed {
			t.Error("a ticket that is not paused had a session resumed")
		}
	})
	t.Run("a Fire that died with both labels on leaves a paused ticket: its session is resumed, and one PR is opened", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(cutOff, f.done("session-9"))
		f.mustFire(0)
		f.labels["AFK:in-progress"] = true // killed while resuming, or while pausing
		f.later(18 * time.Hour)
		f.mustFire(0)
		calls := f.claude.Calls()
		if session, _ := calls[len(calls)-1].Arg("--resume"); len(calls) != 2 || session != "session-9" || f.labelled() != "" {
			t.Fatalf("%d calls, the last resumed %q, labels %q", len(calls), session, f.labelled())
		}
		// The next Fire finds nothing paused and nothing held.
		f.gh.Handle("GET", pullsPath, githubtest.Text(200, `[{"number":55,"head":{"ref":"feat/issue-20","repo":{"full_name":"acme/widgets"}}}]`))
		f.later(time.Hour)
		f.mustFire(0)
		if len(f.claude.Calls()) != 2 || strings.Count(f.writes(), "POST "+pullsPath) != 1 {
			t.Errorf("%d Step calls\n%s", len(f.claude.Calls()), f.writes())
		}
	})
	t.Run("a held lock while another Fire is live is in flight: nothing is touched, the clone least of all", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.labels["AFK:in-progress"] = true
		holdFireLock(t, f.state)
		f.mustFire(0)
		if record := f.record(); record["kind"] != "in-flight" || f.writes() != "" || f.labelled() != "AFK:in-progress" {
			t.Errorf("record = %v, writes %q", record, f.writes())
		}
		if _, err := os.Stat(filepath.Join(f.state, "clone")); err == nil {
			t.Error("the clone was prepared under a live Fire")
		}
	})
	t.Run("a crashed ticket whose Agent PR is open only has the lock released", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.labels["AFK:in-progress"] = true
		f.gh.Handle("GET", pullsPath, githubtest.Text(200, `[{"number":55,"head":{"ref":"feat/issue-20","repo":{"full_name":"acme/widgets"}}}]`))
		f.mustFire(0)
		if f.writes() != "DELETE "+unlockPath+"\nDELETE "+unpausePath || f.labelled() != "" || len(f.claude.Calls()) != 0 || len(f.comments) != 0 {
			t.Errorf("GitHub received\n%s\nlabels %q", f.writes(), f.labelled())
		}
	})
	t.Run("a third crash at the same Step applies AFK:failed and runs no Step", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.labels["AFK:in-progress"] = true
		f.comment("machine-user", "<!-- auto-agent:attempts step=implement count=2 -->\nAttempt 2 of 3")
		f.mustFire(1)
		if f.labelled() != "AFK:failed" || len(f.claude.Calls()) != 0 || !strings.Contains(f.comments[0].Body, "count=3 -->") {
			t.Errorf("labels %q, %d Step calls, comments %+v", f.labelled(), len(f.claude.Calls()), f.comments)
		}
		if record := f.record(); record["phase"] != "failed" || !strings.Contains(record["error"].(string), "AFK:failed applied") {
			t.Errorf("record = %v", record)
		}
	})
	t.Run("a crashed ticket the Budget gate will not start is left unlocked, and its crash is counted once", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.labels["AFK:in-progress"] = true
		f.authErr = fmt.Errorf("exit status 1") // the gate refuses, and parks
		f.mustFire(1)
		f.later(time.Minute)
		f.mustFire(0) // still Parked
		if f.labelled() != "" || len(f.comments) != 1 || !strings.Contains(f.comments[0].Body, "count=1 -->") || len(f.openParks()) != 1 {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
	})
}
