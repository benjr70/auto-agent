package checklist

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var stamp = time.Date(2026, 9, 22, 10, 15, 0, 0, time.UTC)

// Ported from lib/evidence.test.sh: one directory per PR and round, under
// the State dir, created on demand.
func TestDir(t *testing.T) {
	state := t.TempDir()
	cases := []struct {
		name      string
		pr, round int
		want      string
	}{
		{"dir: named and created", 77, 0, filepath.Join(state, "verify", "pr-77", "20260922T101500Z")},
		{"dir: per round", 77, 2, filepath.Join(state, "verify", "pr-77", "20260922T101500Z-r2")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Dir(state, c.pr, c.round, stamp.In(time.FixedZone("x", 3600)))
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
			if info, err := os.Stat(got); err != nil || !info.IsDir() {
				t.Errorf("not created: %v", err)
			}
		})
	}
	t.Run("dir: usage", func(t *testing.T) {
		if _, err := Dir(state, 0, 0, stamp); err == nil {
			t.Error("accepted PR 0")
		}
	})
	t.Run("dir: a negative round is refused", func(t *testing.T) {
		if _, err := Dir(state, 77, -1, stamp); err == nil {
			t.Error("accepted round -1")
		}
	})
}

// Ported from lib/evidence.test.sh: the one legal screenshot filename.
func TestPrune(t *testing.T) {
	stamp := time.Date(2026, 10, 8, 12, 31, 14, 0, time.UTC)
	t.Run("a round that left nothing, its browser profile torn down, is gone and so is its PR's directory", func(t *testing.T) {
		state := t.TempDir()
		dir, _ := Dir(state, 87, 1, stamp)
		os.MkdirAll(filepath.Join(dir, "run", "profiles"), 0o755)
		if got := Prune(dir); got != "" {
			t.Errorf("Prune = %q, want empty", got)
		}
		if _, err := os.Stat(filepath.Join(state, "verify", "pr-87")); err == nil {
			t.Error("pr-87 was left behind")
		}
	})
	t.Run("a round with an app log is kept, and an earlier round beside it is untouched", func(t *testing.T) {
		state := t.TempDir()
		earlier, _ := Dir(state, 87, 1, stamp)
		dir, _ := Dir(state, 87, 2, stamp.Add(time.Hour))
		os.MkdirAll(filepath.Join(dir, "run"), 0o755)
		os.WriteFile(filepath.Join(dir, "run", "panel.log"), []byte("crashed\n"), 0o644)
		if got := Prune(dir); got != dir {
			t.Errorf("Prune = %q, want %q", got, dir)
		}
		if got := Prune(earlier); got != "" {
			t.Errorf("Prune(earlier) = %q", got)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Error(err)
		}
	})
}

func TestName(t *testing.T) {
	cases := []struct {
		name, surface string
		index         int
		slug, want    string
	}{
		{"name: padded", "web", 1, "home-page", "web-01-home-page.png"},
		{"name: two digits", "panel", 12, "smoke-screen", "panel-12-smoke-screen.png"},
		{"name: a padded index is not read as octal", "web", 8, "late", "web-08-late.png"},
		{"name: index must be a number", "web", -1, "home", ""},
		{"name: slug must be a slug", "web", 1, "Home Page", ""},
		{"name: surface must be a Surface name", "Web UI", 1, "home", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Name(c.surface, c.index, c.slug)
			if c.want == "" {
				if err == nil {
					t.Errorf("accepted %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

// Ported from lib/evidence.test.sh: the tour in reading order, a caption
// read out of each name.
func TestShots(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"web-02-item-list.png", "web-01-home-page.png", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("shots: order and captions", func(t *testing.T) {
		got, err := Shots(dir)
		want := []Shot{
			{"web — home page", filepath.Join(dir, "web-01-home-page.png")},
			{"web — item list", filepath.Join(dir, "web-02-item-list.png")},
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("got %v, %v; want %v", got, err, want)
		}
	})
	t.Run("shots: missing directory", func(t *testing.T) {
		if _, err := Shots(filepath.Join(dir, "nope")); err == nil {
			t.Error("no error")
		}
	})
}

// The manual-verifier's `ui-shot:` lines are the tour, in reading order.
func TestTour(t *testing.T) {
	report := `#### 1. the list — PASS
ui-shot: web-02-item-list.png — the list with three items
some prose ui-shot: not-a-line.png — ignored
ui-shot: web-01-home-page.png - the home page, populated
ui-shot: https://example.test/u.png — an uploaded shot
ui-shot: panel-03-gauge.png
ui-shot: web-01-home-page.png — said twice
`
	got := Tour(report, "/state/verify/pr-7/r1")
	want := []Shot{
		{"panel — gauge", "/state/verify/pr-7/r1/panel-03-gauge.png"},
		{"web — the home page, populated", "/state/verify/pr-7/r1/web-01-home-page.png"},
		{"web — the list with three items", "/state/verify/pr-7/r1/web-02-item-list.png"},
		{"an uploaded shot", "https://example.test/u.png"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%v\nwant\n%v", got, want)
	}
	if got := Tour("no shots here\n", "/d"); got != nil {
		t.Errorf("got %v", got)
	}
}

// injectBody is the PR body lib/evidence.test.sh injects into.
const injectBody = `Closes #12

## Summary

What this does.

## Manual verification

- [ ] the item list shows every posted item

---

Generated by something.
`

func index(body, line string) int {
	for i, l := range strings.Split(body, "\n") {
		if l == line {
			return i
		}
	}
	return -1
}

// Ported from lib/evidence.test.sh: the tour is one PR-body section,
// rewritten in place (behaviour 3).
func TestInject(t *testing.T) {
	url := []Shot{{"web — home page", "https://example.test/a.png"}}
	t.Run("inject: before the checklist", func(t *testing.T) {
		out := Inject(injectBody, url)
		if s, m := index(out, "## Screenshots"), index(out, "## Manual verification"); s < 0 || s > m {
			t.Errorf("section at %d, checklist at %d:\n%s", s, m, out)
		}
	})
	t.Run("inject: a URL is an image", func(t *testing.T) {
		if out := Inject(injectBody, url); !strings.Contains(out, "![web — home page](https://example.test/a.png)") {
			t.Errorf("no image:\n%s", out)
		}
	})
	t.Run("inject: a path is cited", func(t *testing.T) {
		out := Inject(injectBody, []Shot{{"web — home page", "/state/web-01-home-page.png"}})
		if !strings.Contains(out, "`/state/web-01-home-page.png` — on the Host") {
			t.Errorf("no path:\n%s", out)
		}
	})
	round1 := Inject(injectBody, url)
	round2 := Inject(round1, []Shot{{"web — home page", "https://example.test/b.png"}})
	t.Run("inject: idempotent refresh", func(t *testing.T) {
		if strings.Count(round2, "## Screenshots\n") != 1 || !strings.Contains(round2, "b.png") || strings.Contains(round2, "a.png") {
			t.Errorf("round 2:\n%s", round2)
		}
		if again := Inject(round2, []Shot{{"web — home page", "https://example.test/b.png"}}); again != round2 {
			t.Errorf("the same tour twice changed the body:\n%s", again)
		}
	})
	t.Run("inject: the body is preserved", func(t *testing.T) {
		want := "Closes #12\n\n## Summary\n\nWhat this does.\n\n## Screenshots\n"
		tail := "\n## Manual verification\n\n- [ ] the item list shows every posted item\n\n---\n\nGenerated by something.\n"
		if !strings.HasPrefix(round2, want) || !strings.HasSuffix(round2, tail) {
			t.Errorf("round 2:\n%s", round2)
		}
	})
	t.Run("inject: empty input is a no-op", func(t *testing.T) {
		if got := Inject(round2, nil); got != round2 {
			t.Errorf("changed:\n%s", got)
		}
	})
	t.Run("inject: a hand-written section survives", func(t *testing.T) {
		human := "## Screenshots\n\nA human's own words and their own image.\n\n## Manual verification\n\n- [ ] something\n"
		out := Inject(human, []Shot{{"web — home", "https://example.test/c.png"}})
		if !strings.Contains(out, "A human's own words") || strings.Count(out, "## Screenshots\n") != 2 {
			t.Errorf("got:\n%s", out)
		}
	})
	t.Run("inject: appended", func(t *testing.T) {
		out := Inject("Just a sentence.\n", []Shot{{"web — home", "https://example.test/c.png"}})
		if !strings.HasPrefix(out, "Just a sentence.\n\n## Screenshots\n") || !strings.HasSuffix(out, "c.png)\n") {
			t.Errorf("got:\n%s", out)
		}
		if again := Inject(out, []Shot{{"web — home", "https://example.test/d.png"}}); strings.Count(again, "## Screenshots") != 1 || strings.Contains(again, "c.png") {
			t.Errorf("an appended tour was not replaced:\n%s", again)
		}
	})
	t.Run("inject: rewriting an end-of-body tour is idempotent", func(t *testing.T) {
		shots := []Shot{{"web — home", "https://example.test/c.png"}}
		round1 := Inject("Just a sentence.\n", shots)
		body := round1
		for round := 2; round <= 4; round++ {
			if body = Inject(body, shots); body != round1 {
				t.Fatalf("round %d changed the body:\n%q\nwant:\n%q", round, body, round1)
			}
		}
	})
	t.Run("inject: before the trailing rule when there is no checklist", func(t *testing.T) {
		out := Inject("Body.\n\n---\n\nFooter.\n", url)
		if s, r := index(out, "## Screenshots"), index(out, "---"); s < 0 || s > r {
			t.Errorf("got:\n%s", out)
		}
	})
}
