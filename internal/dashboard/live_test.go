package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/daemon"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/ruling"
)

var loginVerdict = budget.Verdict{
	AuthMode: "login", Sensor: "usage-endpoint", State: "ok", RemainPct: ptr(55.5), ResetAt: "2026-09-23T15:00:00Z",
	ShouldFire: true, ObservedAt: "2026-09-23T13:30:00Z",
	Limits: []budget.Limit{
		{Scope: "session", Utilization: ptr(44.5), ResetsAt: "2026-09-23T15:00:00Z"},
		{Scope: "weekly", Utilization: ptr(20.0), ResetsAt: "2026-09-28T00:00:00Z"},
		{Scope: "fable", Utilization: ptr(12.0), ResetsAt: "2026-09-28T00:00:00Z"},
	},
	Warnings: []string{},
}

var setupTokenVerdict = budget.Verdict{
	AuthMode: "setup-token", Sensor: "none", State: "unavailable", ShouldFire: true,
	ObservedAt: "2026-09-23T13:31:00Z", Limits: []budget.Limit{}, Warnings: []string{},
}

// fakeGate answers with a verdict and an error, and counts the asks.
type fakeGate struct {
	mu      sync.Mutex
	verdict budget.Verdict
	err     error
	asked   int
}

func (g *fakeGate) Verdict(context.Context) (budget.Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked++
	return g.verdict, g.err
}

// fakeDaemon is what a running Daemon would say it is doing.
type fakeDaemon struct {
	state daemon.State
	tail  []string
}

func (d fakeDaemon) Now() daemon.State { return d.state }
func (d fakeDaemon) Tail() []string    { return d.tail }

type pull struct {
	number      int
	title, ref  string
	labels      []string
	draft       bool
	author      string
	conflicting bool
}

// openPulls is server.test.py's PRS: the Machine user's Agent PRs, and a
// human's draft.
var openPulls = []pull{
	{number: 56, title: "feat(deps): the deps-land lane (#36)", ref: "feat/issue-36", labels: []string{"AFK:verify-human"}},
	{number: 57, title: "docs(research): gh deps (#3)", ref: "research/gh-deps", conflicting: true},
	// Waiting on a Ruling: a request of two decisions is outstanding.
	{number: 58, title: "feat(pick): the pick signal (#40)", ref: "feat/issue-40", labels: []string{"AFK:ruling"}},
	// Parked: fixes still failing at the round cap.
	{number: 59, title: "feat(gate): the docs gate (#41)", ref: "feat/issue-41", labels: []string{"AFK:revise-failed"}, draft: true},
	// Waiting on a Ruling beside a park; its request cannot be read.
	{number: 60, title: "feat(fire): the Fire record (#42)", ref: "feat/issue-42", labels: []string{"AFK:checks-failed", "AFK:ruling"}, draft: true},
	// The label outlived its request.
	{number: 61, title: "feat(ci): the workflow (#43)", ref: "feat/issue-43", labels: []string{"AFK:ruling"}},
	// Not parked: AFK:checks-failed on a PR no longer a draft.
	{number: 62, title: "feat(probe): the Work Probe (#44)", ref: "feat/issue-44", labels: []string{"AFK:checks-failed"}},
	// Parked: the tail exhausted and drafted it.
	{number: 63, title: "feat(lock): the issue lock (#45)", ref: "feat/issue-45", labels: []string{"AFK:checks-failed"}, draft: true},
	// Not parked: a human's own draft is not the loop's.
	{number: 64, title: "wip: a human draft", ref: "ben/wip", draft: true, author: "ben"},
}

func (p pull) json(alone bool) map[string]any {
	labels := []map[string]any{}
	for _, l := range p.labels {
		labels = append(labels, map[string]any{"name": l})
	}
	author := p.author
	if author == "" {
		author = "agent-bot"
	}
	out := map[string]any{
		"number": p.number, "title": p.title, "state": "open", "draft": p.draft,
		"head": map[string]any{"ref": p.ref, "sha": "abc", "repo": map[string]any{"full_name": "acme/widgets"}},
		"base": map[string]any{"ref": "main"}, "user": map[string]any{"login": author}, "labels": labels,
		"created_at": "2026-09-20T00:00:00Z", "html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", p.number),
	}
	if alone {
		out["mergeable"] = !p.conflicting
	}
	return out
}

func rulingRequest(t *testing.T) string {
	t.Helper()
	option := func(letter, text string, recommended bool) ruling.Option {
		return ruling.Option{Letter: letter, Text: text, Cost: "small", Recommended: recommended}
	}
	decisions := []ruling.Decision{
		{Title: "First-open trigger", Scenario: "s", Now: "n", Wants: "w", Why: "y", Options: []ruling.Option{option("A", "a", true), option("B", "b", false)}},
		{Title: "Named here", Scenario: "s", Now: "n", Wants: "w", Why: "y", Options: []ruling.Option{option("A", "a", true), option("B", "b", false)}},
	}
	body, err := ruling.Compose(decisions, "abc1234", "CI green", 0)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// fakeGitHub serves pulls as the open pull requests, each read alone too,
// with an ETag on every answer, and the comments on each.
func fakeGitHub(t *testing.T, pulls []pull) *githubtest.Server {
	t.Helper()
	gh := githubtest.New(t)
	list := []map[string]any{}
	for _, p := range pulls {
		list = append(list, p.json(false))
	}
	gh.Handle("GET", "/repos/acme/widgets/pulls", githubtest.JSON(200, list).WithETag(`"pulls"`))
	alone := regexp.MustCompile(`^/repos/acme/widgets/pulls/([0-9]+)$`)
	gh.HandleMatch("GET", alone, func(r githubtest.Request) githubtest.Reply {
		n := alone.FindStringSubmatch(r.Path)[1]
		for _, p := range pulls {
			if fmt.Sprint(p.number) == n {
				return githubtest.JSON(200, p.json(true)).WithETag(`"pull-` + n + `"`)
			}
		}
		return githubtest.Text(404, `{"message":"Not Found"}`)
	})
	request := rulingRequest(t)
	comments := regexp.MustCompile(`^/repos/acme/widgets/issues/([0-9]+)/comments$`)
	gh.HandleMatch("GET", comments, func(r githubtest.Request) githubtest.Reply {
		switch n := comments.FindStringSubmatch(r.Path)[1]; n {
		case "58":
			return githubtest.JSON(200, []map[string]any{{"id": 9001, "body": request, "user": map[string]any{"login": "agent-bot"}}}).WithETag(`"c58"`)
		case "60":
			return githubtest.Text(500, `{"message":"boom"}`)
		default:
			return githubtest.JSON(200, []map[string]any{}).WithETag(`"c` + n + `"`)
		}
	})
	return gh
}

// gitHubDown answers every request 500.
func gitHubDown(t *testing.T) *githubtest.Server {
	gh := githubtest.New(t)
	gh.HandleMatch("GET", regexp.MustCompile(`.`), func(githubtest.Request) githubtest.Reply {
		return githubtest.Text(500, `{"message":"GitHub is down"}`)
	})
	return gh
}

var scan = map[string]any{"repo": "acme/widgets", "state": "pick", "pick": map[string]any{"issue": 38.0, "title": "Setup engine"}}

// clock is a test's clock, moved by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

const unitShow = "LoadState=loaded\nActiveState=active\nMainPID=4242\nActiveEnterTimestamp=Wed 2026-09-23 09:00:00 UTC\n"

// live is the Dashboard of a running Daemon: every source set.
func live(t *testing.T, dir string, gate *fakeGate, gh *githubtest.Server) (*Server, *httptest.Server, *clock) {
	t.Helper()
	c := &clock{now: now}
	s := &Server{
		Host: Host{Bind: "127.0.0.1", Port: 8090, StateDir: dir, Target: "/srv/widgets", Repo: "acme/widgets"}, Now: c.Now,
		Gate: gate, AuthMode: string(gate.verdict.AuthMode),
		Daemon: fakeDaemon{
			state: daemon.State{State: daemon.StateFiring, Detail: "firing", At: at(14)},
			tail:  []string{`{"fireId":"x"}`, "[daemon 2026-09-23T14:00:00Z] firing"},
		},
		Unit:   func(context.Context) ([]byte, error) { return []byte(unitShow), nil },
		GitHub: github.New(github.Options{BaseURL: gh.URL, Token: "t"}), Login: "agent-bot",
		Scan: func(context.Context) (any, error) { return scan, nil },
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, c
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	json.Unmarshal(data, &out)
	return out
}

func TestLiveSections(t *testing.T) {
	gate := &fakeGate{verdict: loginVerdict}
	gh := fakeGitHub(t, openPulls)
	_, ts, _ := live(t, stateDir(t), gate, gh)
	st := status(t, ts)

	t.Run("a live response is the documented shape", func(t *testing.T) {
		if errs := shapeErrors(documentedShape(t), st, "$"); len(errs) != 0 {
			t.Errorf("the response departs from README.md:\n%s", strings.Join(errs, "\n"))
		}
	})

	t.Run("test_budget_is_the_sensor_verdict_verbatim", func(t *testing.T) {
		b := st["budget"].(map[string]any)
		if !reflect.DeepEqual(b["verdict"], asJSON(t, loginVerdict)) {
			t.Errorf("verdict = %v", b["verdict"])
		}
		if b["rc"] != 0.0 || b["authMode"] != "login" || b["message"] != nil || b["stale"] != false || b["asOf"] == nil {
			t.Errorf("rc %v, authMode %v, message %v, stale %v, asOf %v", b["rc"], b["authMode"], b["message"], b["stale"], b["asOf"])
		}
		var tiles []string
		for _, tile := range b["tiles"].([]any) {
			tile := tile.(map[string]any)
			tiles = append(tiles, fmt.Sprintf("%v %v %v %v", tile["key"], tile["label"], tile["percent"], tile["resetsAt"]))
		}
		want := []string{"session Session 44.5 2026-09-23T15:00:00Z", "weekly Weekly · all models 20 2026-09-28T00:00:00Z", "fable Weekly · fable 12 2026-09-28T00:00:00Z"}
		if !reflect.DeepEqual(tiles, want) {
			t.Errorf("tiles = %q", tiles)
		}
	})

	t.Run("test_daemon_state_is_the_state_file_and_the_journal_only_a_tail", func(t *testing.T) {
		// In Go the state and the tail are the run process's memory, and the
		// unit is systemctl's.
		d := st["daemon"].(map[string]any)
		if d["state"] != "firing" || d["stateDetail"] != "firing" || d["stateAt"] != "2026-09-23T14:00:00Z" || d["resetAt"] != nil || d["fail"] != nil || d["parked"] != nil {
			t.Errorf("daemon = %v", d)
		}
		if !reflect.DeepEqual(d["unit"], any(map[string]any{"active": "active", "mainPid": 4242.0, "since": "Wed 2026-09-23 09:00:00 UTC"})) {
			t.Errorf("unit = %v", d["unit"])
		}
		if tail := d["tail"].([]any); len(tail) != 2 || tail[1] != "[daemon 2026-09-23T14:00:00Z] firing" {
			t.Errorf("tail = %v", tail)
		}
	})

	t.Run("test_queue_prs_and_maps_come_from_the_config_repo", func(t *testing.T) {
		// The queue and PR part; the maps are another Slice's.
		if !reflect.DeepEqual(st["pipeline"].(map[string]any)["scan"], any(scan)) {
			t.Errorf("scan = %v", st["pipeline"])
		}
		var got []string
		for _, item := range st["openPrs"].(map[string]any)["items"].([]any) {
			p := item.(map[string]any)
			got = append(got, fmt.Sprintf("%v %v %v %v", p["number"], p["docsOnly"], p["mergeable"], p["isDraft"]))
		}
		want := []string{"56 false MERGEABLE false", "57 true CONFLICTING false", "58 false MERGEABLE false", "59 false MERGEABLE true", "60 false MERGEABLE true",
			"61 false MERGEABLE false", "62 false MERGEABLE false", "63 false MERGEABLE true", "64 false MERGEABLE true"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("open PRs = %q", got)
		}
		first := st["openPrs"].(map[string]any)["items"].([]any)[0]
		wantFirst := map[string]any{"number": 56.0, "title": "feat(deps): the deps-land lane (#36)", "url": "https://github.com/acme/widgets/pull/56",
			"branch": "feat/issue-36", "labels": []any{"AFK:verify-human"}, "mergeable": "MERGEABLE", "isDraft": false, "docsOnly": false}
		if !reflect.DeepEqual(first, any(wantFirst)) {
			t.Errorf("first = %v", first)
		}
		for _, r := range gh.Requests() {
			if !strings.HasPrefix(r.Path, "/repos/acme/widgets/") {
				t.Errorf("a request outside the config's repo: %s", r.Path)
			}
		}
	})

	t.Run("test_rulings_are_the_pending_requests_of_the_prs_labelled_for_one", func(t *testing.T) {
		want := []any{
			map[string]any{"pr": 58.0, "decisions": 2.0, "url": "https://github.com/acme/widgets/pull/58#issuecomment-9001"},
			// The request could not be read: the wait is still listed, the
			// count unknown, the link the PR itself.
			map[string]any{"pr": 60.0, "decisions": nil, "url": "https://github.com/acme/widgets/pull/60"},
		}
		if !reflect.DeepEqual(st["rulings"], any(want)) {
			t.Errorf("rulings = %v", st["rulings"])
		}
		for _, r := range gh.Requests() {
			if r.Path == "/repos/acme/widgets/issues/56/comments" {
				t.Error("the comments of a PR not labelled for a Ruling were read")
			}
		}
	})

	t.Run("test_the_parked_count_excludes_prs_waiting_on_a_ruling", func(t *testing.T) {
		// 59 is parked; 60 is parked too but waits on a Ruling. 63 is parked
		// (a draft the tail left); 62 carries the same label on a PR that is
		// no longer a draft, and 64 is a human's draft: neither is.
		if got := st["openPrs"].(map[string]any)["parked"]; got != 2.0 {
			t.Errorf("parked = %v, want 2", got)
		}
	})
}

func TestParkedIsPRTriagesRuleNotACopyOfIt(t *testing.T) {
	t.Run("test_parked_is_pr_triages_rule_not_a_copy_of_it", func(t *testing.T) {
		// ADR 0006: the Dashboard re-implements none of the Daemon's rules.
		files, err := filepath.Glob("*.go")
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, needle := range []string{"AFK:revise-failed", "AFK:rebase-failed", "AFK:deps-failed", "AFK:checks-failed",
				"LabelReviseFailed", "LabelRebaseFailed", "LabelDepsFailed", "LabelChecksFailed"} {
				if strings.Contains(string(src), needle) {
					t.Errorf("%s names %s: a copy of PR Triage's parked rule", file, needle)
				}
			}
		}
	})
	t.Run("test_parked_is_unknown_not_zero_when_the_rule_cannot_be_asked", func(t *testing.T) {
		_, ts, _ := live(t, t.TempDir(), &fakeGate{verdict: loginVerdict}, gitHubDown(t))
		if got := status(t, ts)["openPrs"].(map[string]any); got["parked"] != nil || got["stale"] != true {
			t.Errorf("openPrs = %v", got)
		}
	})
}

// Behaviour 2: two open Agent PRs, one draft with AFK:checks-failed and one
// with AFK:ruling.
func TestOneParkedAndOneRuling(t *testing.T) {
	pulls := []pull{
		{number: 70, title: "feat(a): a (#1)", ref: "feat/issue-1", labels: []string{"AFK:checks-failed"}, draft: true},
		{number: 58, title: "feat(b): b (#2)", ref: "feat/issue-2", labels: []string{"AFK:ruling"}},
	}
	_, ts, _ := live(t, t.TempDir(), &fakeGate{verdict: loginVerdict}, fakeGitHub(t, pulls))
	st := status(t, ts)
	if got := st["openPrs"].(map[string]any)["parked"]; got != 1.0 {
		t.Errorf("parked = %v, want 1", got)
	}
	rulings := st["rulings"].([]any)
	if len(rulings) != 1 || rulings[0].(map[string]any)["pr"] != 58.0 || rulings[0].(map[string]any)["decisions"] != 2.0 {
		t.Errorf("rulings = %v", rulings)
	}
}

// Behaviour 1: a Daemon held by the gate.
func TestADaemonHeldByTheGate(t *testing.T) {
	s, ts, _ := live(t, t.TempDir(), &fakeGate{verdict: loginVerdict}, fakeGitHub(t, nil))
	s.Daemon = fakeDaemon{state: daemon.State{State: daemon.StateHeld, Detail: "held by the Budget gate until 2026-09-23T15:00:00Z", At: at(14), ResetAt: "2026-09-23T15:00:00Z", Fails: 2}}
	d := status(t, ts)["daemon"].(map[string]any)
	if d["state"] != "held" || d["resetAt"] != "2026-09-23T15:00:00Z" || !reflect.DeepEqual(d["fail"], any(map[string]any{"count": 2.0, "cap": nil})) {
		t.Errorf("daemon = %v", d)
	}
	probe := at(15)
	s.Daemon = fakeDaemon{state: daemon.State{State: daemon.StateParked, Detail: "parked", At: at(14), Parked: &journal.Park{Issue: ptr(77), Reason: "401", ProbeAt: probe}}}
	d = status(t, ts)["daemon"].(map[string]any)
	if d["state"] != "parked" || !reflect.DeepEqual(d["parked"], any(map[string]any{"issue": 77.0, "reason": "401", "probeAt": "2026-09-23T15:00:00Z"})) {
		t.Errorf("daemon = %v", d)
	}
}

func TestTheDaemonsUnit(t *testing.T) {
	s, ts, c := live(t, t.TempDir(), &fakeGate{verdict: loginVerdict}, fakeGitHub(t, nil))
	s.Unit = func(context.Context) ([]byte, error) {
		return []byte("LoadState=not-found\nActiveState=inactive\nMainPID=0\nActiveEnterTimestamp=\n"), nil
	}
	if d := status(t, ts)["daemon"].(map[string]any); d["unit"] != nil || d["stale"] != false {
		t.Errorf("with no unit installed: %v", d)
	}
	s.Unit = func(context.Context) ([]byte, error) { return nil, errors.New("systemctl: no user bus") }
	c.add(time.Minute)
	if d := status(t, ts)["daemon"].(map[string]any); d["stale"] != true || !strings.Contains(fmt.Sprint(d["error"]), "no user bus") || d["state"] != "firing" {
		t.Errorf("with systemctl failing: %v", d)
	}
}

// Behaviour 3: GitHub returns 500.
func TestSensorWithoutAVerdictAndGitHubDown(t *testing.T) {
	t.Run("test_sensor_without_a_verdict_and_gh_down", func(t *testing.T) {
		gate := &fakeGate{err: errors.New("the usage endpoint answered 500")}
		s, ts, _ := live(t, stateDir(t), gate, gitHubDown(t))
		s.AuthMode = "setup-token"
		s.Scan = func(context.Context) (any, error) { return nil, errors.New("GitHub answered 500") }
		code, body := get(t, ts, "/api/status")
		if code != 200 {
			t.Fatalf("GET /api/status = %d", code)
		}
		var st map[string]any
		json.Unmarshal([]byte(body), &st)
		b := st["budget"].(map[string]any)
		if b["stale"] != true || !strings.Contains(fmt.Sprint(b["error"]), "500") || b["verdict"] != nil {
			t.Errorf("budget = %v", b)
		}
		// No verdict: the Host config's auth mode still picks the message,
		// and the last Fire's verdict is still shown.
		if b["message"] != "no usage sensor in this auth mode" || b["lastFire"].(map[string]any)["fireId"] != "20260923T130000Z-104" {
			t.Errorf("message %v, lastFire %v", b["message"], b["lastFire"])
		}
		for _, name := range []string{"openPrs", "pipeline"} {
			if section := st[name].(map[string]any); section["stale"] != true || section["error"] == nil {
				t.Errorf("%s = %v", name, section)
			}
		}
		if rulings := st["rulings"].([]any); len(rulings) != 0 {
			t.Errorf("rulings = %v", rulings)
		}
		if n := len(items(st)); n != 5 {
			t.Errorf("%d Fires, want 5", n)
		}
		if errs := shapeErrors(documentedShape(t), st, "$"); len(errs) != 0 {
			t.Errorf("a degraded response departs from README.md:\n%s", strings.Join(errs, "\n"))
		}
	})
}

func TestAFailedRefreshKeepsTheLastGoodValue(t *testing.T) {
	gate := &fakeGate{verdict: loginVerdict}
	gh := fakeGitHub(t, openPulls)
	s, ts, c := live(t, t.TempDir(), gate, gh)
	good := status(t, ts)
	gate.mu.Lock()
	gate.err = errors.New("the usage endpoint answered 500")
	gate.mu.Unlock()
	s.GitHub = github.New(github.Options{BaseURL: gitHubDown(t).URL, Token: "t"})
	c.add(time.Minute)
	st := status(t, ts)
	for _, name := range []string{"budget", "openPrs"} {
		section := st[name].(map[string]any)
		if section["stale"] != true || section["error"] == nil || section["asOf"] != good[name].(map[string]any)["asOf"] {
			t.Errorf("%s = stale %v, error %v, asOf %v", name, section["stale"], section["error"], section["asOf"])
		}
	}
	if st["openPrs"].(map[string]any)["parked"] != 2.0 || len(st["rulings"].([]any)) != 2 || st["budget"].(map[string]any)["verdict"] == nil {
		t.Errorf("the last good values were not kept: %v %v", st["openPrs"], st["rulings"])
	}
}

func TestRefreshes(t *testing.T) {
	t.Run("the gate is asked at most once a minute", func(t *testing.T) {
		gate := &fakeGate{verdict: loginVerdict}
		_, ts, c := live(t, t.TempDir(), gate, fakeGitHub(t, nil))
		status(t, ts)
		c.add(59 * time.Second)
		status(t, ts)
		if gate.asked != 1 {
			t.Errorf("asked %d times within a minute", gate.asked)
		}
		c.add(time.Second)
		status(t, ts)
		if gate.asked != 2 {
			t.Errorf("asked %d times after a minute", gate.asked)
		}
	})
	t.Run("GitHub reads use the client's ETag cache: a refresh on a quiet repo costs 304s", func(t *testing.T) {
		gh := fakeGitHub(t, openPulls[:3])
		_, ts, c := live(t, t.TempDir(), &fakeGate{verdict: loginVerdict}, gh)
		status(t, ts)
		first := len(gh.Requests())
		c.add(time.Minute)
		status(t, ts)
		again := gh.Requests()[first:]
		if len(again) != first {
			t.Fatalf("the refresh made %d requests, the first read %d", len(again), first)
		}
		for _, r := range again {
			if r.Header.Get("If-None-Match") == "" {
				t.Errorf("%s %s asked with no ETag", r.Method, r.Path)
			}
		}
	})
}

func TestMessage(t *testing.T) {
	// msg is the budget's message on a Host whose gate answers v and
	// refusal, its Host config declaring declared.
	msg := func(v budget.Verdict, refusal error, declared string) any {
		s := &Server{Host: Host{StateDir: t.TempDir()}, Gate: &fakeGate{verdict: v, err: refusal}, AuthMode: declared}
		if m := s.Status(context.Background()).Budget.Message; m != nil {
			return *m
		}
		return nil
	}
	with := func(change func(*budget.Verdict)) budget.Verdict {
		v := loginVerdict
		change(&v)
		return v
	}
	t.Run("test_per_auth_mode", func(t *testing.T) {
		if got := msg(loginVerdict, nil, ""); got != nil {
			t.Errorf("login = %v", got)
		}
		if got := msg(setupTokenVerdict, nil, ""); got != "no usage sensor in this auth mode" {
			t.Errorf("setup-token = %v", got)
		}
		stale := setupTokenVerdict
		stale.Sensor, stale.State = "stream-events", "stale"
		if got := msg(stale, nil, ""); got != "no usage sensor in this auth mode" {
			t.Errorf("setup-token, stream events = %v", got)
		}
		for prefix, got := range map[string]any{
			"api-key auth mode": msg(with(func(v *budget.Verdict) {
				v.AuthMode, v.Sensor, v.State, v.ShouldFire = "api-key", "spend", "unavailable", false
			}), budget.ErrSpendPacing, ""),
			"credential dead":            msg(with(func(v *budget.Verdict) { v.State, v.ShouldFire = "auth-dead", false }), budget.ErrAuthDead, ""),
			"auth mode mismatch":         msg(with(func(v *budget.Verdict) { v.ShouldFire = false }), budget.ErrModeMismatch, ""),
			"usage endpoint unreachable": msg(with(func(v *budget.Verdict) { v.State = "stale" }), nil, ""),
			"usage endpoint unavailable": msg(with(func(v *budget.Verdict) { v.Sensor, v.State = "stream-events", "stale" }), nil, ""),
			"no auth mode":               msg(with(func(v *budget.Verdict) { v.AuthMode = "" }), nil, ""),
		} {
			if s, _ := got.(string); !strings.HasPrefix(s, prefix) {
				t.Errorf("want %q…, got %v", prefix, got)
			}
		}
	})
	t.Run("test_host_env_mode_when_there_is_no_verdict", func(t *testing.T) {
		// The gate could not answer: the Host config's auth mode speaks.
		if got := msg(budget.Verdict{}, errors.New("the usage endpoint answered 500"), "setup-token"); got != "no usage sensor in this auth mode" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("a refusal is the gate's verdict, with its exit code", func(t *testing.T) {
		s := &Server{Host: Host{StateDir: t.TempDir()}, Gate: &fakeGate{verdict: with(func(v *budget.Verdict) { v.ShouldFire = false }), err: budget.ErrModeMismatch}}
		b := s.Status(context.Background()).Budget
		if b.RC == nil || *b.RC != 5 || b.Verdict == nil || b.Stale {
			t.Errorf("rc %v, verdict %v, stale %v", b.RC, b.Verdict, b.Stale)
		}
	})
}

func TestSetupTokenHost(t *testing.T) {
	t.Run("test_message_and_last_fire_verdict", func(t *testing.T) {
		_, ts, _ := live(t, stateDir(t), &fakeGate{verdict: setupTokenVerdict}, fakeGitHub(t, nil))
		b := status(t, ts)["budget"].(map[string]any)
		if b["message"] != "no usage sensor in this auth mode" || !reflect.DeepEqual(b["verdict"], asJSON(t, setupTokenVerdict)) {
			t.Errorf("message %v, verdict %v", b["message"], b["verdict"])
		}
		last := b["lastFire"].(map[string]any)
		if last["fireId"] != "20260923T130000Z-104" || last["endedAt"] != "2026-09-23T13:30:00Z" || last["gate"] == nil {
			t.Errorf("lastFire = %v", last)
		}
	})
}
