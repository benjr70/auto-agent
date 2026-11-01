package dashboard

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The page is not unit-tested (Spec: "The JSON is tested; the page is
// not"): these check that / serves it, and carry over the cases of
// dashboard/server.test.py that test the served page.

// served is what GET / answers, from a directory with no page in it: the
// page is in the binary, and nothing is read from disk at run time.
func served(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	rec := httptest.NewRecorder()
	(&Server{Host: Host{StateDir: t.TempDir()}}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestRootServesTheEmbeddedPage(t *testing.T) {
	html := served(t)
	if html != string(page) {
		t.Errorf("GET / is not the embedded page")
	}
	if !strings.Contains(html, `fetch("/api/status")`) {
		t.Errorf("the page does not read /api/status")
	}
	// It reads nothing else from the server.
	if n := len(regexp.MustCompile(`fetch\(`).FindAllString(html, -1)); n != 1 {
		t.Errorf("the page fetches %d times, want only /api/status", n)
	}
}

func TestPage(t *testing.T) {
	t.Run("test_page_renders_the_message_and_the_last_verdict", func(t *testing.T) {
		html := served(t)
		for _, want := range []string{"b.message", "b.lastFire"} {
			if !strings.Contains(html, want) {
				t.Errorf("the page does not render %s", want)
			}
		}
	})

	t.Run("test_every_badge_colour_has_a_rule_and_no_project_literal", func(t *testing.T) {
		html := string(page)
		colours := map[string]bool{}
		for _, m := range regexp.MustCompile(`\w+:\s*\["[^"]*",\s*"(\w+)"`).FindAllStringSubmatch(html, -1) {
			colours[m[1]] = true
		}
		if !colours["purple"] {
			t.Errorf("no badge is purple: %v", colours)
		}
		for colour := range colours {
			if !regexp.MustCompile(`\.badge\.` + colour + `\s*\{`).MatchString(html) {
				t.Errorf("badge colour %q has no .badge.%s rule", colour, colour)
			}
		}
		if regexp.MustCompile(`(?i)smoker`).MatchString(html) {
			t.Errorf("the page names a project")
		}
	})

	t.Run("test_ruling_badge_links_the_comment_and_parked_is_counted_apart", func(t *testing.T) {
		html := render(t, map[string]any{
			"openPrs": map[string]any{"parked": 1, "items": []any{
				pr(58, "feat(pick): the pick signal", "feat/issue-40", "AFK:ruling", false),
				pr(59, "feat(gate): the docs gate", "feat/issue-41", "AFK:revise-failed", true),
				pr(62, "feat(ci): one decision", "feat/issue-44", "AFK:ruling", false),
			}},
			"rulings": []any{
				map[string]any{"pr": 58, "decisions": 2, "url": "https://gh.test/pull/58#issuecomment-9001"},
				map[string]any{"pr": 62, "decisions": 1, "url": "https://gh.test/pull/62#issuecomment-9002"},
			},
		})
		prs := html["prs"]
		if !regexp.MustCompile(`<a class="badge amber" href="https://gh\.test/pull/58#issuecomment-9001"[^>]*>Ruling · 2 decisions</a>`).MatchString(prs) {
			t.Errorf("no Ruling badge linking the request comment: %s", prs)
		}
		if !strings.Contains(prs, ">Ruling · 1 decision</a>") {
			t.Errorf("no singular Ruling badge: %s", prs)
		}
		if n := strings.Count(prs, ">Ruling · "); n != 2 {
			t.Errorf("%d Ruling badges, want 2", n)
		}
		// The badge is a link of its own, so the row is not one big anchor.
		for _, a := range strings.Split(prs, "<a ")[1:] {
			if !strings.Contains(a, "</a>") {
				t.Errorf("an anchor holds another: %s", prs)
			}
		}
		if !strings.Contains(prs, `href="https://gh.test/pull/59"`) {
			t.Errorf("PR #59 is not linked: %s", prs)
		}
		if got := html["prCounts"]; got != "1 parked · 2 awaiting a Ruling" {
			t.Errorf("prCounts = %q", got)
		}
	})

	t.Run("test_no_counts_when_nothing_is_parked_or_waiting", func(t *testing.T) {
		html := render(t, map[string]any{"openPrs": map[string]any{"parked": 0, "items": []any{}}, "rulings": []any{}})
		if got := html["prCounts"]; got != "" {
			t.Errorf("prCounts = %q, want none", got)
		}
	})

	t.Run("the page renders with every section empty", func(t *testing.T) {
		html := render(t, map[string]any{})
		for _, id := range []string{"daemon", "prs", "maps", "fires"} {
			if strings.Contains(html[id], "loading") || html[id] == "" {
				t.Errorf("#%s = %q, want it rendered", id, html[id])
			}
		}
	})

	t.Run("each Fire opens to its Steps, and the Fire in flight shows its current Step", func(t *testing.T) {
		step := func(name string, cost float64, in int) map[string]any {
			return map[string]any{
				"name": name, "model": "claude-fable-5-1", "outcome": "OK", "startedAt": "2026-09-23T13:01:00Z",
				"elapsedMs": 754000, "turns": 41, "costUsd": cost, "inputTokens": in, "outputTokens": 8400,
				"cacheReadTokens": 310000, "cacheCreationTokens": 22000,
			}
		}
		live := map[string]any{
			"id": "20260923T140000Z-105", "badge": "slice", "startedAt": "2026-09-23T14:00:00Z", "inFlight": true,
			"summary": "pick #38", "steps": []any{step("implement", 0.42, 1200), step("review", 0.05, 300)},
		}
		html := render(t, map[string]any{
			"fires": map[string]any{
				"items": []any{live, map[string]any{
					"id": "20260923T130000Z-104", "badge": "slice", "startedAt": "2026-09-23T13:00:00Z",
					"endedAt": "2026-09-23T13:30:00Z", "exit": 0, "outcome": "OK", "summary": "pick #37",
					"steps": []any{step("implement", 0.42, 1200)},
				}},
				"current": live,
			},
		})
		for _, want := range []string{"implement", "claude-fable-5-1", "12m 34s", "$0.42", "1,200 in", "8,400 out", "310,000 cache read"} {
			if !strings.Contains(html["fires"], want) {
				t.Errorf("the history does not show %q: %s", want, html["fires"])
			}
		}
		if !strings.Contains(html["fire"], "Step <b>review</b>") {
			t.Errorf("the Fire in flight does not show its current Step: %s", html["fire"])
		}
	})

	t.Run("a Fire with warnings carries a warning badge, and opens to them", func(t *testing.T) {
		fire := func(id string, notes ...any) map[string]any {
			return map[string]any{"id": id, "startedAt": "2026-09-23T13:00:00Z", "endedAt": "2026-09-23T13:00:01Z", "exit": 0,
				"noWork": true, "summary": "no work — queue empty", "notes": notes, "steps": []any{}}
		}
		warned := "the Sweep: the Environment provider: down --all exited 1, want 0 (nothing it owns remains): a volume remains"
		html := render(t, map[string]any{"fires": map[string]any{"items": []any{fire("a", warned), fire("b")}}})
		if n := strings.Count(html["fires"], `<span class="badge amber">warning</span>`); n != 1 {
			t.Errorf("%d warning badges, want 1: %s", n, html["fires"])
		}
		if !strings.Contains(html["fires"], "down --all exited 1") {
			t.Errorf("the history does not show the warning: %s", html["fires"])
		}
	})

	t.Run("a Daemon waiting for disk shows the state and both numbers", func(t *testing.T) {
		html := render(t, map[string]any{"daemon": map[string]any{"state": "disk", "disk": map[string]any{"free": 1 << 30, "need": 10 << 30}}})
		if !strings.Contains(html["daemon"], ">Waiting for disk</span>") || !strings.Contains(html["daemon"], "waiting for disk: <b>1.0 GB</b> free, <b>10.0 GB</b> needed") {
			t.Errorf("the Daemon card is %s", html["daemon"])
		}
	})

	t.Run("an opened Fire and the live tail stay open across the next render", func(t *testing.T) {
		status := map[string]any{
			"daemon": map[string]any{"state": "idle", "tail": []any{"line one"}},
			"fires": map[string]any{"items": []any{
				map[string]any{"id": "f-1", "startedAt": "2026-09-23T13:00:00Z", "exit": 0, "summary": "pick #37"},
				map[string]any{"id": "f-2", "startedAt": "2026-09-23T12:00:00Z", "exit": 0, "summary": "pick #36"},
			}},
		}
		html := render(t, status, `
listeners.toggle({target: {open: true, dataset: {key: "fire:f-1"}}});
listeners.toggle({target: {open: true, dataset: {key: "daemon-tail"}}});
render();`)
		if !strings.Contains(html["fires"], `data-key="fire:f-1" open`) {
			t.Errorf("the opened Fire closed on re-render: %s", html["fires"])
		}
		if strings.Contains(html["fires"], `data-key="fire:f-2" open`) {
			t.Errorf("a Fire nobody opened renders open: %s", html["fires"])
		}
		if !strings.Contains(html["daemon"], `data-key="daemon-tail" open`) {
			t.Errorf("the opened live tail closed on re-render: %s", html["daemon"])
		}
		html = render(t, status, `
listeners.toggle({target: {open: true, dataset: {key: "fire:f-1"}}});
listeners.toggle({target: {open: false, dataset: {key: "fire:f-1"}}});
render();`)
		if strings.Contains(html["fires"], `data-key="fire:f-1" open`) {
			t.Errorf("a Fire closed again renders open: %s", html["fires"])
		}
	})

	t.Run("a stale section shows its error", func(t *testing.T) {
		stale := func(extra map[string]any, msg string) map[string]any {
			extra["stale"], extra["error"] = true, msg
			return extra
		}
		html := render(t, map[string]any{
			"budget":  stale(map[string]any{}, "budget-broke"),
			"daemon":  stale(map[string]any{"state": "idle"}, "daemon-broke"),
			"fires":   stale(map[string]any{"items": []any{}}, "fires-broke"),
			"openPrs": stale(map[string]any{"items": []any{}}, "prs-broke"),
			"maps":    stale(map[string]any{"items": []any{}}, "maps-broke"),
		})
		all := ""
		for _, v := range html {
			all += v
		}
		for _, want := range []string{"budget-broke", "daemon-broke", "fires-broke", "prs-broke", "maps-broke"} {
			if !strings.Contains(all, want) {
				t.Errorf("the page does not show %q", want)
			}
		}
	})
}

func pr(number int, title, branch, label string, draft bool) map[string]any {
	return map[string]any{
		"number": number, "title": title, "url": "https://gh.test/pull/" + strconv.Itoa(number),
		"branch": branch, "labels": []any{label}, "mergeable": "MERGEABLE", "isDraft": draft, "docsOnly": false,
	}
}

// render is what the page's own render() writes for status: its script run
// under node against a stand-in document, every element's text or HTML by
// id. It is server.test.py's render_prs. Any then is script run after the
// first render, with the page's event listeners in listeners.
func render(t *testing.T, status map[string]any, then ...string) map[string]string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node renders the page's own script")
	}
	script := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(string(page))[1]
	data, _ := json.Marshal(status)
	harness := `
const els = {};
const el = id => els[id] ||= {style: {}, innerHTML: "", textContent: ""};
const listeners = {};
globalThis.document = {getElementById: el, title: "", addEventListener: (type, fn) => { listeners[type] = fn; }};
globalThis.fetch = async () => ({ok: true, json: async () => STATUS});
globalThis.setInterval = () => 0;
const STATUS = ` + string(data) + `;
` + script + `
setTimeout(() => {
` + strings.Join(then, "\n") + `
  const out = {};
  for (const [id, e] of Object.entries(els)) out[id] = e.innerHTML || e.textContent;
  console.log(JSON.stringify(out));
}, 50);
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var html map[string]string
	if err := json.Unmarshal(out, &html); err != nil {
		t.Fatalf("node printed %q: %v", out, err)
	}
	return html
}
