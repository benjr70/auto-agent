package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/step"
)

// now is the clock of every test: the in-flight Fire started ten minutes
// before it.
var now = time.Date(2026, 9, 23, 14, 10, 0, 0, time.UTC)

const inFlightID = "20260923T140000Z-105"

func at(hour int) time.Time { return time.Date(2026, 9, 23, hour, 0, 0, 0, time.UTC) }

func ptr[T any](v T) *T { return &v }

// documentedShape is the block README.md documents.
func documentedShape(t *testing.T) any {
	t.Helper()
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)## `/api/status`.*?```json\n(.*?)\n```").FindSubmatch(data)
	if m == nil {
		t.Fatal("README.md documents no /api/status block")
	}
	var shape any
	if err := json.Unmarshal(m[1], &shape); err != nil {
		t.Fatalf("the documented block is not JSON: %v", err)
	}
	return shape
}

// shapeErrors is every place got departs from the documented example doc:
// the same keys in every object, list items shaped like the example's first
// item, scalars of the example's type or null. A null example accepts
// anything. It is server.test.py's shape_errors.
func shapeErrors(doc, got any, path string) []string {
	if doc == nil || got == nil {
		return nil
	}
	switch d := doc.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want object, got %T", path, got)}
		}
		var errs []string
		for k := range d {
			if _, ok := g[k]; !ok {
				errs = append(errs, fmt.Sprintf("%s: missing %s", path, k))
			}
		}
		for k, v := range g {
			if _, ok := d[k]; !ok {
				errs = append(errs, fmt.Sprintf("%s: extra %s", path, k))
				continue
			}
			errs = append(errs, shapeErrors(d[k], v, path+"."+k)...)
		}
		return errs
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want list, got %T", path, got)}
		}
		var errs []string
		if len(d) > 0 {
			for i, item := range g {
				errs = append(errs, shapeErrors(d[0], item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
		return errs
	}
	if reflect.TypeOf(doc) != reflect.TypeOf(got) {
		return []string{fmt.Sprintf("%s: want %T, got %T %v", path, doc, got, got)}
	}
	return nil
}

// usage is a Step's usage object, as claude reports it.
func usage(input, output, cacheRead, cacheCreation int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"service_tier":"standard"}`,
		input, output, cacheRead, cacheCreation))
}

var gate = budget.Verdict{AuthMode: "login", Sensor: "usage-endpoint", State: "ok", ShouldFire: true, Limits: []budget.Limit{}, Warnings: []string{}}

// stateDir is a State dir holding five Fire records, one of them in
// flight, and a file that is not one.
func stateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	j := journal.Journal{StateDir: dir}
	ended := func(hour int) *time.Time { return ptr(at(hour).Add(30 * time.Minute)) }
	records := []journal.Record{
		{
			FireID: "20260923T100000Z-101", Repo: "acme/widgets", StartedAt: at(10), EndedAt: ended(10), Exit: ptr(0),
			Phase: "done", Kind: "pick", Action: "implement", Issue: ptr(36), PR: ptr(55),
			Steps: []journal.Step{
				{Name: "implement", Gate: &gate, StartedAt: at(10).Add(time.Minute), Outcome: step.OK, Model: "claude-fable-5-1",
					NumTurns: 41, CostUSD: 0.42, Usage: usage(1200, 8400, 310000, 22000), ElapsedMS: 754000},
				{Name: "pre-commit-review", Gate: &gate, StartedAt: at(10).Add(15 * time.Minute), Outcome: step.OK, Model: "claude-sonnet-5-5",
					NumTurns: 6, CostUSD: 0.08, Usage: usage(300, 900, 40000, 5000), ElapsedMS: 61000},
			},
		},
		{
			FireID: "20260923T110000Z-102", Repo: "acme/widgets", StartedAt: at(11), EndedAt: ended(11), Exit: ptr(0),
			Phase: "done", Kind: "idle", Action: "none",
		},
		{
			FireID: "20260923T120000Z-103", Repo: "acme/widgets", StartedAt: at(12), EndedAt: ended(12), Exit: ptr(1),
			Phase: "failed", Kind: "pick", Action: "implement", Issue: ptr(37), Error: "the Implement Step failed",
			Steps: []journal.Step{{Name: "implement", Gate: &gate, StartedAt: at(12), Outcome: step.Failed, Model: "claude-fable-5-1",
				CostUSD: 0.10, Usage: json.RawMessage("null")}},
		},
		{
			FireID: "20260923T130000Z-104", Repo: "acme/widgets", StartedAt: at(13), EndedAt: ended(13), Exit: ptr(0),
			Phase: "done", Kind: "reconcile", Action: "review", Issue: ptr(38), PR: ptr(56), Reconcile: "incomplete",
			Verify:   &journal.Verify{Round: 1, Max: 3, Bootstrap: true},
			Warnings: []string{"deployed-lane: off — verification.deployed.enabled is false"},
			Steps: []journal.Step{
				{Name: "correctness-review", Gate: &gate, StartedAt: at(13), Outcome: step.OK, Model: "claude-fable-5-1", CostUSD: 0.2, Usage: usage(1, 2, 3, 4)},
				// The Budget gate stopped the last Step: it ran no call.
				{Name: "verify", Gate: &budget.Verdict{AuthMode: "login", State: "ok", ShouldFire: false, ResetAt: "2026-09-23T15:00:00Z"}, StartedAt: at(13).Add(20 * time.Minute)},
			},
		},
		{
			FireID: inFlightID, Repo: "acme/widgets", StartedAt: at(14), Phase: "implement", Kind: "pick", Action: "implement", Issue: ptr(39),
			Steps: []journal.Step{{Name: "implement", Gate: &gate, StartedAt: at(14)}},
		},
	}
	for _, r := range records {
		if err := j.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "fires", "20260923T090000Z-99.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serve is the Dashboard on a State dir, behind a test server.
func serve(t *testing.T, dir string) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{Host: Host{Bind: "127.0.0.1", Port: 8090, StateDir: dir, Target: "/srv/widgets", Repo: "acme/widgets"}, Now: func() time.Time { return now }}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// status is a live /api/status response, decoded as a reader would.
func status(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	code, body := get(t, ts, "/api/status")
	if code != 200 {
		t.Fatalf("GET /api/status = %d %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	return got
}

func items(st map[string]any) []map[string]any {
	var list []map[string]any
	for _, item := range st["fires"].(map[string]any)["items"].([]any) {
		list = append(list, item.(map[string]any))
	}
	return list
}

func TestStatusRoute(t *testing.T) {
	_, ts := serve(t, stateDir(t))
	st := status(t, ts)

	t.Run("test_route_answers_200_with_the_documented_shape", func(t *testing.T) {
		if errs := shapeErrors(documentedShape(t), st, "$"); len(errs) != 0 {
			t.Errorf("the response departs from README.md:\n%s", strings.Join(errs, "\n"))
		}
		if shapeErrors(documentedShape(t), map[string]any{"generatedAt": "now", "host": []any{}}, "$") == nil {
			t.Error("the shape check passes a response that is not the shape")
		}
		// An empty State dir answers in the same shape.
		_, empty := serve(t, t.TempDir())
		if errs := shapeErrors(documentedShape(t), status(t, empty), "$"); len(errs) != 0 {
			t.Errorf("an empty State dir departs from README.md:\n%s", strings.Join(errs, "\n"))
		}
	})

	t.Run("test_history_is_the_records_newest_first_and_skips_an_unreadable_one", func(t *testing.T) {
		var ids []string
		for _, f := range items(st) {
			ids = append(ids, f["id"].(string))
		}
		want := []string{inFlightID, "20260923T130000Z-104", "20260923T120000Z-103", "20260923T110000Z-102", "20260923T100000Z-101"}
		if !reflect.DeepEqual(ids, want) {
			t.Errorf("history = %v, want %v", ids, want)
		}
		if got := st["fires"].(map[string]any)["unreadable"]; !reflect.DeepEqual(got, []any{"20260923T090000Z-99.json"}) {
			t.Errorf("unreadable = %v", got)
		}
	})

	t.Run("test_the_record_with_no_end_is_the_current_fire", func(t *testing.T) {
		cur, _ := st["fires"].(map[string]any)["current"].(map[string]any)
		if cur == nil || cur["id"] != inFlightID || cur["inFlight"] != true || cur["summary"] != "in flight" {
			t.Errorf("current = %v", cur)
		}
	})

	t.Run("test_each_record_reads_as_what_the_fire_did", func(t *testing.T) {
		type read struct {
			badge, summary, outcome, line any
			exit                          any
			noWork                        bool
		}
		got := map[string]read{}
		for _, f := range items(st) {
			if f["id"] != inFlightID {
				got[f["id"].(string)] = read{f["badge"], f["summary"], f["outcome"], f["line"], f["exit"], f["noWork"].(bool)}
			}
		}
		want := map[string]read{
			"20260923T130000Z-104": {"reconcile", "review #38 · PR #56 · incomplete", "OK", "reconcile: review #38 · PR #56 · incomplete", 0.0, false},
			"20260923T120000Z-103": {"slice", "FAILED — exit 1 · implement #37", "FAILED", "pick: implement #37", 1.0, false},
			"20260923T110000Z-102": {nil, "no work — queue empty", "OK", "idle: none", 0.0, true},
			"20260923T100000Z-101": {"slice", "implement #36 · PR #55", "OK", "pick: implement #36 · PR #55", 0.0, false},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("history reads\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("fire items carry their steps, with each Step's own tokens and cost", func(t *testing.T) {
		first := items(st)[4]
		steps := first["steps"].([]any)
		if len(steps) != 2 {
			t.Fatalf("steps = %v", steps)
		}
		want := []map[string]any{
			{"name": "implement", "model": "claude-fable-5-1", "outcome": "OK", "startedAt": "2026-09-23T10:01:00Z", "elapsedMs": 754000.0,
				"turns": 41.0, "costUsd": 0.42, "inputTokens": 1200.0, "outputTokens": 8400.0, "cacheReadTokens": 310000.0, "cacheCreationTokens": 22000.0},
			{"name": "pre-commit-review", "model": "claude-sonnet-5-5", "outcome": "OK", "startedAt": "2026-09-23T10:15:00Z", "elapsedMs": 61000.0,
				"turns": 6.0, "costUsd": 0.08, "inputTokens": 300.0, "outputTokens": 900.0, "cacheReadTokens": 40000.0, "cacheCreationTokens": 5000.0},
		}
		for i, s := range steps {
			if !reflect.DeepEqual(s, any(want[i])) {
				t.Errorf("step %d = %v\nwant %v", i, s, want[i])
			}
		}
		if first["costUsd"] != 0.5 || first["model"] != "claude-sonnet-5-5" {
			t.Errorf("the Fire's cost and model = %v %v, want the Steps' sum and the last", first["costUsd"], first["model"])
		}
		// A call with no usage, and a Step the gate stopped, have no tokens.
		failed := items(st)[2]["steps"].([]any)[0].(map[string]any)
		stopped := items(st)[1]["steps"].([]any)[1].(map[string]any)
		if failed["inputTokens"] != nil || stopped["outcome"] != nil || stopped["inputTokens"] != nil {
			t.Errorf("failed %v, stopped %v", failed, stopped)
		}
	})

	t.Run("test_bootstrap_warning_is_the_newest_finished_record", func(t *testing.T) {
		want := map[string]any{"warning": true, "fireId": "20260923T130000Z-104",
			"notes": []any{"deployed-lane: off — verification.deployed.enabled is false"}}
		if !reflect.DeepEqual(st["bootstrap"], any(want)) {
			t.Errorf("bootstrap = %v", st["bootstrap"])
		}
	})

	t.Run("the budget's last Fire is the newest finished record, with its last Gate verdict", func(t *testing.T) {
		last := st["budget"].(map[string]any)["lastFire"].(map[string]any)
		gate, _ := last["gate"].(map[string]any)
		if last["fireId"] != "20260923T130000Z-104" || last["endedAt"] != "2026-09-23T13:30:00Z" || gate == nil || gate["shouldFire"] != false || gate["resetAt"] != "2026-09-23T15:00:00Z" {
			t.Errorf("lastFire = %v", last)
		}
	})

	t.Run("the Fire summary names the Fire in flight and its Step, with no model", func(t *testing.T) {
		got := st["fireSummary"].(map[string]any)
		if got["enabled"] != false || got["forFire"] != inFlightID || got["title"] != "pick: implement #39 — implement" || got["issue"] != "issue #39" {
			t.Errorf("fireSummary = %v", got)
		}
	})

	t.Run("the host is the Dashboard's address, State dir, target and repo", func(t *testing.T) {
		want := map[string]any{"bind": "127.0.0.1", "port": 8090.0, "stateDir": st["host"].(map[string]any)["stateDir"],
			"target": "/srv/widgets", "repo": "acme/widgets", "summaryEnabled": false}
		if !reflect.DeepEqual(st["host"], any(want)) {
			t.Errorf("host = %v", st["host"])
		}
	})
}

func TestACrashedFireIsHistory(t *testing.T) {
	dir := t.TempDir()
	if err := (journal.Journal{StateDir: dir}).Write(journal.Record{FireID: "20260923T100000Z-1", StartedAt: now.Add(-InFlightMax - time.Minute), Kind: "pick", Action: "implement", Issue: ptr(5)}); err != nil {
		t.Fatal(err)
	}
	_, ts := serve(t, dir)
	st := status(t, ts)
	fires := st["fires"].(map[string]any)
	item := items(st)[0]
	if fires["current"] != nil || item["inFlight"] != false || item["summary"] != "crashed — no end recorded" || item["outcome"] != nil {
		t.Errorf("a record with no end, older than %v: current %v, item %v", InFlightMax, fires["current"], item)
	}
	if summary := st["fireSummary"].(map[string]any); summary["forFire"] != nil {
		t.Errorf("fireSummary = %v", summary)
	}
}

func TestAFailingReadKeepsTheLastGoodValue(t *testing.T) {
	dir := stateDir(t)
	_, ts := serve(t, dir)
	good := status(t, ts)["fires"].(map[string]any)
	// The records can no longer be listed.
	fires := filepath.Join(dir, "fires")
	if err := os.Rename(fires, fires+".gone"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fires, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := status(t, ts)
	got := st["fires"].(map[string]any)
	if got["stale"] != true || got["error"] == nil || got["asOf"] != good["asOf"] || !reflect.DeepEqual(got["items"], good["items"]) {
		t.Errorf("after a failing read, fires = stale %v error %v asOf %v, %d items", got["stale"], got["error"], got["asOf"], len(got["items"].([]any)))
	}
	if st["bootstrap"].(map[string]any)["fireId"] != "20260923T130000Z-104" {
		t.Errorf("bootstrap = %v, want the last good read's", st["bootstrap"])
	}
	// A State dir that never had a good read still answers.
	broken := t.TempDir()
	os.WriteFile(filepath.Join(broken, "fires"), nil, 0o644)
	_, ts = serve(t, broken)
	if got := status(t, ts)["fires"].(map[string]any); got["stale"] != true || len(got["items"].([]any)) != 0 {
		t.Errorf("fires = %v", got)
	}
}

func TestRoutes(t *testing.T) {
	_, ts := serve(t, t.TempDir())

	t.Run("test_page_and_unknown_routes", func(t *testing.T) {
		code, page := get(t, ts, "/")
		if code != 200 || !strings.Contains(page, "/api/status") {
			t.Errorf("GET / = %d %q", code, page)
		}
		if code, _ := get(t, ts, "/nope"); code != 404 {
			t.Errorf("GET /nope = %d, want 404", code)
		}
		head, err := http.Head(ts.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		head.Body.Close()
		if head.StatusCode != 200 {
			t.Errorf("HEAD /api/status = %d", head.StatusCode)
		}
	})

	t.Run("there is no POST route", func(t *testing.T) {
		for path, want := range map[string]int{"/": 405, "/api/status": 405, "/nope": 404} {
			resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != want {
				t.Errorf("POST %s = %d, want %d", path, resp.StatusCode, want)
			}
		}
	})
}
