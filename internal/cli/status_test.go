package cli

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/journal"
)

// statusBody is a running Daemon's /api/status: every section filled, the
// open PRs stale.
const statusBody = `{
  "generatedAt": "2026-09-23T14:10:00Z",
  "host": {"bind": "127.0.0.1", "port": 8090, "stateDir": "/state", "target": "/srv/widgets", "repo": "acme/widgets", "summaryEnabled": false},
  "budget": {
    "rc": 0, "verdict": {"authMode": "login", "sensor": "usage-endpoint", "state": "ok", "remainPct": 62, "resetAt": "2026-09-23T15:00:00Z", "shouldFire": true},
    "authMode": "login", "message": null,
    "tiles": [{"key": "session", "label": "Session", "percent": 38, "resetsAt": "2026-09-23T15:00:00Z"}],
    "lastFire": null, "asOf": "2026-09-23T14:10:00Z", "stale": false, "error": null
  },
  "daemon": {
    "unit": {"active": "active", "mainPid": 1234, "since": "Wed 2026-09-23 09:00:00 UTC"},
    "state": "held", "stateDetail": "held by the Budget gate until 2026-09-23T15:00:00Z",
    "stateAt": "2026-09-23T11:01:31Z", "resetAt": "2026-09-23T15:00:00Z",
    "fail": {"count": 1, "cap": null}, "parked": null, "tail": [], "tailError": null,
    "asOf": "2026-09-23T14:10:00Z", "stale": false, "error": null
  },
  "fires": {
    "items": [
      {"id": "20260923T140000Z-105", "kind": "pick", "badge": "slice", "startedAt": "2026-09-23T14:00:00Z", "endedAt": null, "inFlight": true,
       "summary": "in flight", "issue": 39, "outcome": null, "costUsd": null, "steps": []},
      {"id": "20260923T130000Z-104", "kind": "reconcile", "badge": "reconcile", "startedAt": "2026-09-23T13:00:00Z", "endedAt": "2026-09-23T13:30:00Z", "inFlight": false,
       "summary": "review #38 · PR #56 · incomplete", "issue": 38, "outcome": "OK", "costUsd": 0.42, "steps": []}
    ],
    "current": {"id": "20260923T140000Z-105", "kind": "pick", "badge": "slice", "startedAt": "2026-09-23T14:00:00Z", "endedAt": null, "inFlight": true,
       "summary": "in flight", "issue": 39, "outcome": null, "costUsd": null, "steps": []},
    "unreadable": [], "asOf": "2026-09-23T14:10:00Z", "stale": false, "error": null
  },
  "bootstrap": {"warning": null, "fireId": null, "notes": []},
  "pipeline": {"scan": null, "asOf": null, "stale": false, "error": null},
  "openPrs": {
    "items": [
      {"number": 56, "title": "feat(deps): the deps-land lane", "url": "https://github.com/acme/widgets/pull/56",
       "branch": "feat/issue-36", "labels": ["AFK:ruling"], "mergeable": "CONFLICTING", "isDraft": true, "docsOnly": false}
    ],
    "parked": 2, "asOf": "2026-09-23T14:05:00Z", "stale": true, "error": "GitHub answered 502"
  },
  "rulings": [{"pr": 56, "decisions": 2, "url": "https://github.com/acme/widgets/pull/56#issuecomment-9001"}],
  "maps": {
    "items": [
      {"number": 1, "title": "Wayfinder: reusable harness", "url": "https://github.com/acme/widgets/issues/1", "destination": "A spec ready for slicing.",
       "frontier": [{"number": 5, "title": "Research: X", "url": null, "type": "research", "badge": "AFK"}], "partial": false}
    ],
    "total": 1, "truncated": false, "asOf": "2026-09-23T14:10:00Z", "stale": false, "error": null
  },
  "wayfinder": {"maps": 1, "frontier": 1, "afk": 2, "unknown": false, "truncated": false, "queueSlices": 2, "queueWayfinder": 1},
  "fireSummary": {"enabled": false, "forFire": "20260923T140000Z-105", "title": "implement #39", "description": null, "issue": null,
    "asOf": "2026-09-23T14:10:00Z", "stale": false, "error": null}
}`

// statusFixture is a Host whose Host config names the Dashboard's address.
type statusFixture struct {
	stdout, stderr bytes.Buffer
	environ        map[string]string
	origin         string
}

func newStatusFixture(t *testing.T) *statusFixture {
	t.Helper()
	return &statusFixture{
		origin:  "git@github.com:acme/widgets.git",
		environ: map[string]string{"HOME": t.TempDir(), "AUTO_AGENT_STATE_DIR": t.TempDir()},
	}
}

// listen points the Host config at addr.
func (f *statusFixture) listen(t *testing.T, addr string) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	f.environ["AUTO_AGENT_DASHBOARD_BIND"] = host
	f.environ["AUTO_AGENT_DASHBOARD_PORT"] = port
}

// daemon is a running Daemon's Dashboard, answering body.
func (f *statusFixture) daemon(t *testing.T, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	f.listen(t, ts.Listener.Addr().String())
	return ts
}

// noDaemon points the Host config at an address nothing listens on.
func (f *statusFixture) noDaemon(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	f.listen(t, addr)
}

func (f *statusFixture) run(args ...string) int {
	return Run(args, Env{
		Stdout: &f.stdout, Stderr: &f.stderr,
		LookupEnv: func(k string) (string, bool) { v, ok := f.environ[k]; return v, ok },
		OriginURL: func(string) (string, error) { return f.origin, nil },
		Now:       func() time.Time { return time.Date(2026, 9, 23, 14, 10, 0, 0, time.UTC) },
	})
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestStatus(t *testing.T) {
	t.Run("a Daemon answering: every section printed, exit 0", func(t *testing.T) {
		f := newStatusFixture(t)
		f.daemon(t, statusBody)
		if code := f.run("status"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		out := f.stdout.String()
		wantLines(t, out,
			// Daemon state and reset.
			"Daemon: held — held by the Budget gate until 2026-09-23T15:00:00Z",
			"reset at 2026-09-23T15:00:00Z",
			"1 Fire in a row failed",
			"unit active, pid 1234",
			// Budget.
			"Budget: login, 62% left, resets 2026-09-23T15:00:00Z",
			"Session 38% used, resets 2026-09-23T15:00:00Z",
			// The Fire in flight and its Step.
			"In flight: 20260923T140000Z-105 — in flight (#39)",
			"Step: implement #39",
			// The last Fires, with cost.
			"20260923T130000Z-104  OK  $0.42  review #38 · PR #56 · incomplete",
			// Open PRs with parked and Ruling counts; this one stale.
			"Open PRs: 1, 2 parked, 1 waiting on a Ruling",
			"stale: GitHub answered 502",
			"#56 feat(deps): the deps-land lane [draft, conflicting, Ruling: 2 decisions]",
			// Map frontiers.
			"Maps: 1 open",
			"#1 Wayfinder: reusable harness — 1 ready",
			"#5 Research: X (research, AFK)",
		)
	})

	t.Run("--json prints the route's body verbatim", func(t *testing.T) {
		f := newStatusFixture(t)
		f.daemon(t, statusBody)
		if code := f.run("status", "--json"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if got := f.stdout.String(); got != statusBody {
			t.Errorf("--json printed\n%s\nwant the body verbatim", got)
		}
	})

	t.Run("no Daemon: says so, prints the Fire history from disk, exit 1", func(t *testing.T) {
		f := newStatusFixture(t)
		f.noDaemon(t)
		exit, issue, pr := 0, 36, 55
		ended := time.Date(2026, 9, 23, 13, 30, 0, 0, time.UTC)
		j := journal.Journal{StateDir: f.environ["AUTO_AGENT_STATE_DIR"]}
		for _, r := range []journal.Record{
			{FireID: "20260923T120000Z-103", StartedAt: ended.Add(-90 * time.Minute), EndedAt: &ended, Exit: &exit, Phase: "done", Kind: "idle", Action: "none"},
			{FireID: "20260923T130000Z-104", StartedAt: ended.Add(-30 * time.Minute), EndedAt: &ended, Exit: &exit, Phase: "done", Kind: "pick", Action: "implement", Issue: &issue, PR: &pr},
		} {
			if err := j.Write(r); err != nil {
				t.Fatal(err)
			}
		}
		if code := f.run("status"); code != 1 {
			t.Fatalf("exit %d, want 1: %s", code, f.stderr.String())
		}
		wantLines(t, f.stdout.String()+f.stderr.String(),
			"no Daemon answers at http://"+f.environ["AUTO_AGENT_DASHBOARD_BIND"]+":"+f.environ["AUTO_AGENT_DASHBOARD_PORT"]+"/api/status",
			"Fires (from "+f.environ["AUTO_AGENT_STATE_DIR"],
			"20260923T130000Z-104  OK  implement #36 · PR #55",
			"20260923T120000Z-103  OK  no work — queue empty",
		)
	})

	t.Run("--repo picks the Host config as run does", func(t *testing.T) {
		f := newStatusFixture(t)
		f.daemon(t, statusBody)
		f.origin = "not a repo"
		if code := f.run("status", "--repo", "acme/widgets", "--json"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
	})

	t.Run("an unspecified bind is asked on loopback", func(t *testing.T) {
		f := newStatusFixture(t)
		ts := f.daemon(t, statusBody)
		f.environ["AUTO_AGENT_DASHBOARD_BIND"] = "0.0.0.0"
		f.environ["AUTO_AGENT_DASHBOARD_PORT"] = strconv.Itoa(ts.Listener.Addr().(*net.TCPAddr).Port)
		if code := f.run("status", "--json"); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
	})

	t.Run("a stray argument is a usage error", func(t *testing.T) {
		f := newStatusFixture(t)
		if code := f.run("status", "a", "b"); code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})

	t.Run("the usage text lists status", func(t *testing.T) {
		if !strings.Contains(usage, "\n  status ") {
			t.Errorf("usage lacks status:\n%s", usage)
		}
	})
}
