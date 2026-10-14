package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// parkBody is a park issue's body, as the Daemon opens it.
const parkBody = "<!-- auto-agent:parked -->\nThe Daemon on `host` parked at 2026-10-01T00:00:00Z: dead."

// parkOf is the Parked state a Fire record carries; nil when it carries none.
func parkOf(record map[string]any) map[string]any {
	park, _ := record["park"].(map[string]any)
	return park
}

// Ported from lib/daemon-park.test.sh, under its case names. parked.json is
// gone: the Daemon is Parked while its own park issue is open (ADR 0012), so
// a park is entered by the Fire that finds the credential dead and left by
// the Fire whose probe finds it alive.
func TestFireParked(t *testing.T) {
	dead := errors.New("exit status 1")

	// test_status_when_not_parked
	t.Run("status and reprobe when not parked", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.mustFire(0)
		if record := f.record(); record["kind"] != "pick" || record["park"] != nil || len(f.parks) != 0 {
			t.Errorf("record = %v, parks %+v", record, f.parks)
		}
		for _, r := range f.gh.Writes() {
			if !strings.HasPrefix(r.Path, "/repos/acme/widgets/issues/20/") && r.Path != pullsPath {
				t.Errorf("a Fire that is not Parked wrote %s %s", r.Method, r.Path)
			}
		}
	})

	// test_enter_opens_the_issue
	t.Run("park enter opens the needs-human issue and writes parked.json (AC 4)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.authErr = dead
		f.mustFire(1)
		if f.writes() != "POST /repos/acme/widgets/issues" || len(f.parks) != 1 {
			t.Fatalf("GitHub received\n%s", f.writes())
		}
		issue := f.parks[0]
		if !strings.HasPrefix(issue.Title, "Daemon parked on ") || !strings.HasSuffix(issue.Title, ": Claude credential dead") ||
			!strings.HasPrefix(issue.Body, "<!-- auto-agent:parked -->\n") || !strings.Contains(issue.Body, "Claude's credential is dead") || !strings.Contains(issue.Body, "every 1h") {
			t.Errorf("issue = %+v", issue)
		}
		record := f.record()
		park := parkOf(record)
		if record["phase"] != "parked" || park["issue"] != 77.0 || park["probeAt"] != "2026-10-12T10:30:00Z" ||
			!strings.Contains(park["reason"].(string), "Claude's credential is dead") || !strings.Contains(record["error"].(string), "Claude's credential is dead") {
			t.Errorf("record = %v", record)
		}
		// Entering again is idempotent: no second issue, no write at all.
		f.later(time.Hour)
		f.mustFire(0)
		records := f.records()
		second := records[len(records)-1]
		if len(f.parks) != 1 || f.writes() != "POST /repos/acme/widgets/issues" || len(issue.comments) != 0 {
			t.Errorf("a second Parked Fire wrote\n%s\nparks %+v", f.writes(), f.parks)
		}
		if park := parkOf(second); second["kind"] != "parked" || second["phase"] != "parked" || second["error"] != "" || second["issue"] != nil ||
			park["issue"] != 77.0 || park["probeAt"] != "2026-10-12T11:30:00Z" {
			t.Errorf("second record = %v", second)
		}
	})

	// test_enter_reuses_the_open_issue
	t.Run("park enter reuses the open needs-human issue with the marker (AC 4)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.park(41, "machine-user", "unrelated needs-human issue")
		f.park(52, "machine-user", parkBody)
		f.authErr = dead
		f.mustFire(0)
		if len(f.parks) != 2 || f.writes() != "" {
			t.Errorf("GitHub received\n%s\nparks %+v", f.writes(), f.parks)
		}
		if park := parkOf(f.record()); park["issue"] != 52.0 {
			t.Errorf("park = %v", park)
		}
	})
	t.Run("a park issue opened since the Fire read GitHub is reused with a comment, never a second", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.authErr = dead
		reads := 0
		f.gh.HandleFunc("GET", "/repos/acme/widgets/issues", func(r githubtest.Request) githubtest.Reply {
			if r.Query.Get("labels") != "AFK:needs-human" {
				return githubtest.Text(200, `[]`)
			}
			if reads++; reads == 2 {
				f.park(52, "machine-user", parkBody)
			}
			return githubtest.JSON(200, f.openParks())
		})
		f.mustFire(1)
		if len(f.parks) != 1 || f.writes() != "POST /repos/acme/widgets/issues/52/comments" ||
			!strings.HasPrefix(f.parks[0].comments[0], "Parked again at 2026-10-12T09:30:00Z on ") {
			t.Errorf("GitHub received\n%s\nparks %+v", f.writes(), f.parks)
		}
		if park := parkOf(f.record()); park["issue"] != 52.0 {
			t.Errorf("park = %v", park)
		}
	})
	t.Run("a park issue anyone else opened does not park the Daemon", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.park(52, "a-passer-by", parkBody)
		f.answer(finished)
		f.mustFire(0)
		if record := f.record(); record["kind"] != "pick" || record["pr"] != 55.0 || f.parks[0].State != "open" {
			t.Errorf("record = %v", record)
		}
	})

	// test_enter_without_gh_still_parks
	t.Run("parking never depends on gh; the issue is retried on the next re-probe", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.authErr, f.failPark = dead, true
		f.mustFire(1)
		record := f.record()
		park := parkOf(record)
		if record["phase"] != "parked" || park == nil || park["issue"] != nil || !strings.Contains(record["error"].(string), "the AFK:needs-human issue could not be opened") {
			t.Errorf("record = %v", record)
		}
		f.failPark = false
		f.later(time.Hour)
		f.mustFire(1)
		records := f.records()
		if park := parkOf(records[len(records)-1]); len(f.openParks()) != 1 || park["issue"] != 77.0 || len(f.claude.Calls()) != 0 {
			t.Errorf("parks %+v, record %v", f.parks, records[len(records)-1])
		}
	})

	// test_tick_unparks_when_the_probe_passes
	t.Run("the hourly tick un-parks when claude auth status passes (AC 4)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.authErr = dead
		f.mustFire(1)
		f.later(time.Hour)
		f.mustFire(0)
		if f.parks[0].State != "open" || len(f.claude.Calls()) != 0 {
			t.Fatalf("the issue is %s while Parked; %d Step calls", f.parks[0].State, len(f.claude.Calls()))
		}
		before := len(f.gh.Writes())
		f.authErr = nil
		f.answer(finished)
		f.later(time.Hour)
		f.mustFire(0)
		issue := f.parks[0]
		if issue.State != "closed" || len(issue.comments) != 1 || issue.comments[0] != "Un-parked at 2026-10-12T11:30:00Z: the credential probe passes again. Fires resume." {
			t.Errorf("issue = %+v", issue)
		}
		// Un-parked first, then the Fire carries on with the ticket.
		var writes []string
		for _, r := range f.gh.Writes()[before:] {
			writes = append(writes, r.Method+" "+r.Path)
		}
		want := []string{"POST /repos/acme/widgets/issues/77/comments", "PATCH /repos/acme/widgets/issues/77", "POST " + lockPath, "POST " + pullsPath, "DELETE " + unlockPath}
		if strings.Join(writes, "\n") != strings.Join(want, "\n") {
			t.Errorf("the un-parking Fire sent\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
		}
		records := f.records()
		if third := records[2]; third["kind"] != "parked" || third["phase"] != "done" || third["park"] != nil || third["pr"] != 55.0 {
			t.Errorf("third record = %v", third)
		}
		// One probe per Parked Fire, and the un-parked Fire asked the gate
		// once more before each of its Steps: Implement and the review.
		if f.probes != 5 {
			t.Errorf("claude auth status was asked %d times, want 5", f.probes)
		}
	})

	// test_probe_and_leave. Dropped: "leave when not parked is a no-op" and
	// "unknown command: 2" test the bash `park` subcommand, which Go has no
	// counterpart of: a Fire that is not Parked never un-parks (the first
	// case above), and nothing but a Fire parks or un-parks.
	for _, tt := range []struct {
		name       string
		status     string
		err        error
		stillDead  bool
		wantClosed string
	}{
		{"probe: exit 1 from auth status is a failed probe", `{"loggedIn":false}`, dead, true, "open"},
		{"probe: loggedIn false is a failed probe even on exit 0", `{"loggedIn":false}`, nil, true, "open"},
		{"probe ok", `{"loggedIn":true,"authMethod":"oauth_token"}`, nil, false, "closed"},
		// Not a bash case: an auth mode the gate cannot vouch for is no
		// passing probe, and the issue stays open for a human.
		{"a probe that finds an auth mode mismatch keeps the Daemon Parked", `{"loggedIn":true,"authMethod":"claude.ai"}`, nil, true, "open"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFireFixture(t, labelsConfig)
			f.park(52, "machine-user", parkBody)
			f.authStatus, f.authErr = tt.status, tt.err
			f.queue = []githubtest.Reply{queueReply()}
			f.mustFire(0)
			if f.parks[0].State != tt.wantClosed || (parkOf(f.record()) != nil) != tt.stillDead {
				t.Errorf("the issue is %s; record %v", f.parks[0].State, f.record())
			}
		})
	}

	t.Run("no Step call while Parked, and no ticket or queue read (AC 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.park(52, "machine-user", parkBody)
		f.labels["AFK:in-progress"] = true // a crash, left for after the park
		f.authErr = dead
		f.answer(finished)
		f.mustFire(0)
		if len(f.claude.Calls()) != 0 || f.writes() != "" {
			t.Errorf("%d Step calls; GitHub received\n%s", len(f.claude.Calls()), f.writes())
		}
		for _, r := range f.gh.Requests() {
			if r.Path == "/graphql" || (r.Path == "/repos/acme/widgets/issues" && r.Query.Get("labels") != "AFK:needs-human") {
				t.Errorf("a Parked Fire read %s %s?%s", r.Method, r.Path, r.Query.Encode())
			}
		}
	})
	t.Run("the probe interval comes from the Host config", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.environ["AUTO_AGENT_PARK_REPROBE_SECS"] = "900"
		f.authErr = dead
		f.mustFire(1)
		if park := parkOf(f.record()); park["probeAt"] != "2026-10-12T09:45:00Z" || !strings.Contains(f.parks[0].Body, "every 15m") {
			t.Errorf("park = %v, body %q", park, f.parks[0].Body)
		}
	})
	t.Run("every one of the Daemon's own park issues is closed once the credential is alive", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.park(52, "machine-user", parkBody)
		f.park(53, "machine-user", parkBody)
		f.queue = []githubtest.Reply{queueReply()}
		f.mustFire(0)
		if len(f.openParks()) != 0 || f.probes != 1 {
			t.Errorf("open parks %+v, %d probes", f.openParks(), f.probes)
		}
	})
	t.Run("a park issue that will not close fails the Fire, and no ticket is worked", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.park(52, "machine-user", parkBody)
		f.gh.Handle("PATCH", "/repos/acme/widgets/issues/52", githubtest.Text(500, `{"message":"boom"}`))
		f.answer(finished)
		f.mustFire(1)
		if record := f.record(); !strings.Contains(record["error"].(string), "un-parking: closing #52") || len(f.claude.Calls()) != 0 {
			t.Errorf("record = %v", record)
		}
	})
}

// Ported from lib/fire.test.sh: test_auth_dead_fire_pauses_and_parks.
func TestFireAuthDeadStep(t *testing.T) {
	t.Run("an AUTH_DEAD Fire pauses its work and parks the Daemon (issue #30 AC 4)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(steptest.Reply{
			Run: "echo half > greeting.txt", ExitCode: 1,
			Lines: []string{implementInit("session-9")}, Stderr: "Failed to authenticate: OAuth session expired and could not be refreshed",
		}, f.done("session-9"))
		f.mustFire(1)
		// The work is paused, never failed (not the ticket's fault).
		if f.labelled() != "AFK:paused" || len(f.comments) != 1 || f.comments[0].Body != "Fire paused at 2026-10-12T09:30:00Z: credential dead mid-Fire. Branch kept for resume." {
			t.Errorf("labels %q, comments %+v", f.labelled(), f.comments)
		}
		head := f.remoteRef("refs/heads/feat/issue-20")
		if head == "" || git(t, f.remote, "log", "-1", "--format=%s", head) != "wip: freeze partial work on #20 (credential dead)" {
			t.Errorf("the remote's branch is %q", head)
		}
		// The needs-human issue is opened, and the record names the Fire.
		record := f.record()
		park := parkOf(record)
		if len(f.openParks()) != 1 || park["issue"] != 77.0 || !strings.Contains(park["reason"].(string), "the implement Step of Fire "+record["fireId"].(string)+" ended AUTH_DEAD") {
			t.Errorf("parks %+v, record %v", f.parks, record)
		}
		if record["phase"] != "parked" || record["resetAt"] != "" || record["resumeSession"] != "session-9" || stepsOf(record)[0]["outcome"] != "AUTH_DEAD" || record["exit"] != 1.0 {
			t.Errorf("record = %v", record)
		}
		if strings.Contains(f.writes(), "AFK:failed") {
			t.Errorf("GitHub received\n%s", f.writes())
		}

		// A login is back: the next Fire un-parks and resumes the session.
		f.later(time.Hour)
		f.mustFire(0)
		calls := f.claude.Calls()
		if session, _ := calls[len(calls)-1].Arg("--resume"); len(calls) != 2 || session != "session-9" || f.labelled() != "" || f.parks[0].State != "closed" {
			t.Errorf("%d calls, the last resumed %q, labels %q, park %+v", len(calls), session, f.labelled(), f.parks[0])
		}
		// The pause on a dead credential was never counted against the cap.
		for _, c := range f.comments {
			if state.IsPause(c.Body) {
				t.Errorf("a pause was counted: %q", c.Body)
			}
		}
	})
}

func TestRunOnceDryRunParked(t *testing.T) {
	t.Run("a Parked Daemon: verdict parked, with its park issue, and neither the lock nor the queue is read", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.HandleFunc("GET", "/repos/acme/widgets/issues", func(r githubtest.Request) githubtest.Reply {
			if r.Query.Get("labels") == "AFK:needs-human" {
				return githubtest.JSON(200, []map[string]any{{"number": 52, "body": parkBody, "user": map[string]any{"login": "machine-user"}}})
			}
			return githubtest.Text(200, `[{"number":9}]`)
		})
		if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		report := f.report(t)
		if next := report["next"].(map[string]any); report["state"] != "parked" || report["parked"] != 52.0 || next["action"] != "probe" || f.probes != 0 {
			t.Errorf("report = %s", f.stdout.String())
		}
		if requests := f.gh.Requests(); len(requests) != 3 || len(f.gh.Writes()) != 0 {
			t.Errorf("requests = %+v", requests)
		}
	})
}
