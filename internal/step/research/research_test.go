package research

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// input is the input the recorded fixture was made for: ticket #13 of Map #9.
var input = Input{
	Ticket:   Ticket{Number: 13, Title: "Research: Claude auth modes", URL: "https://github.com/acme/widgets/issues/13"},
	Question: "Which auth modes does Claude Code support headless?",
	Map: &Map{Number: 9, Title: "Reusable auto-agent", URL: "https://github.com/acme/widgets/issues/9",
		Destination: "A Daemon any project can run.", Notes: "Primary sources: the Claude Code docs."},
	Path: "docs/research/reusable-auto-agent/research-claude-auth-modes.md",
	Date: "2026-10-12",
}

func TestPrompt(t *testing.T) {
	t.Run("the prompt carries the research rules of the resolve skill", func(t *testing.T) {
		for _, rule := range []string{
			"primary sources only",
			"Never\nanswer from memory",
			"Cite every claim",
			"`no-sources`",
			"Ticket: [#<ticket.number>]",
			"Researched on <date>",
			"Sources: ",
			"## TL;DR",
			"You write exactly one file, `path`, and nothing else",
		} {
			if !strings.Contains(prompt, rule) {
				t.Errorf("the prompt lacks %q", rule)
			}
		}
	})
	t.Run("the prompt carries no GitHub and no git-write instruction", func(t *testing.T) {
		command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|--add-label|--remove-label|AFK:")
		if found := command.FindAllString(prompt, -1); found != nil {
			t.Errorf("the prompt names %q", found)
		}
		if !strings.Contains(prompt, "Never commit, push, stage") || !strings.Contains(prompt, "Never read or write the issue tracker") {
			t.Error("the prompt does not forbid committing, pushing or the tracker")
		}
	})
}

func TestSpec(t *testing.T) {
	t.Run("the result schema names exactly the fields of the result type, all required", func(t *testing.T) {
		var s struct {
			Required             []string                   `json:"required"`
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal([]byte(schema), &s); err != nil {
			t.Fatal(err)
		}
		var want, got []string
		for i := range reflect.TypeFor[Result]().NumField() {
			want = append(want, reflect.TypeFor[Result]().Field(i).Tag.Get("json"))
		}
		for name := range s.Properties {
			got = append(got, name)
		}
		sort.Strings(want)
		sort.Strings(got)
		sort.Strings(s.Required)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(s.Required, want) || s.AdditionalProperties == nil || *s.AdditionalProperties {
			t.Errorf("properties %v, required %v, want %v and no others allowed", got, s.Required, want)
		}
	})
	t.Run("the Step may fetch and search the web and edit files, and has no shell", func(t *testing.T) {
		tools := Spec().Tools
		for _, tool := range []string{"WebFetch", "WebSearch", "Write", "Read"} {
			if !strings.Contains(strings.Join(tools, ","), tool) {
				t.Errorf("the Step lacks %s: %v", tool, tools)
			}
		}
		for _, tool := range tools {
			if tool == "Bash" {
				t.Errorf("the Step has a shell: %v", tools)
			}
		}
	})
}

// Against a fake claude replaying the recorded stream of one Research call:
// the Step is sent its typed input, its result decodes, and the file the
// call wrote passes Go's re-check.
func TestRecordedCall(t *testing.T) {
	stream, err := os.ReadFile("testdata/stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	findings, err := filepath.Abs("testdata/findings.md")
	if err != nil {
		t.Fatal(err)
	}
	fake := steptest.New(t)
	fake.Script(steptest.Reply{
		Run:   "mkdir -p docs/research/reusable-auto-agent && cp '" + findings + "' " + input.Path,
		Lines: strings.Split(strings.TrimSpace(string(stream)), "\n"),
	})
	dir := t.TempDir()
	run := (&step.Claude{Binary: fake.Path}).Run(context.Background(), Spec(), step.Call{Dir: dir, Model: "claude-opus-5-5", Input: input})
	got, err := step.Decode[Result](run)
	if err != nil {
		t.Fatalf("%v (%s: %s)", err, run.Outcome, run.Detail)
	}
	want := Result{Outcome: Found, Title: "Claude Code's headless auth modes", Description: "claude code headless auth modes",
		Gist:    "Claude Code runs headless on either a Claude subscription login or an API key; the login is the one the Daemon uses, and its expiry is reported in the stream as an authentication failure.",
		Sources: []string{"https://docs.anthropic.com/en/docs/claude-code/iam"}}
	if !reflect.DeepEqual(got, want) || got.Check() != nil {
		t.Errorf("result = %+v (check %v)", got, got.Check())
	}
	if run.SessionID != "7f3c1d2e-research" || run.NumTurns != 14 || run.Usage.CacheReadInputTokens != 121000 {
		t.Errorf("run = %+v", run)
	}
	sent := `/factory:research {"ticket":{"number":13,"title":"Research: Claude auth modes","url":"https://github.com/acme/widgets/issues/13"},` +
		`"question":"Which auth modes does Claude Code support headless?",` +
		`"map":{"number":9,"title":"Reusable auto-agent","url":"https://github.com/acme/widgets/issues/9","destination":"A Daemon any project can run.","notes":"Primary sources: the Claude Code docs."},` +
		`"path":"docs/research/reusable-auto-agent/research-claude-auth-modes.md","date":"2026-10-12"}`
	if stdin := fake.Calls()[0].Stdin; stdin != sent {
		t.Errorf("the Step was sent\n%s\nwant\n%s", stdin, sent)
	}
	if err := CheckFindings(dir, input, "docs/research/", []string{input.Path}); err != nil {
		t.Errorf("CheckFindings = %v", err)
	}
}

func TestCheck(t *testing.T) {
	found := Result{Outcome: Found, Title: "T", Description: "auth modes", Gist: "One paragraph.", Sources: []string{"https://x"}}
	with := func(change func(*Result)) Result {
		r := found
		change(&r)
		return r
	}
	tests := []struct {
		name   string
		result Result
		want   string // a fragment of the error; empty means it passes
	}{
		{"a finding with a title, description, gist and a source passes", found, ""},
		{"a no-sources result needs the sources tried and what is missing", Result{Outcome: NoSources, Sources: []string{"https://x"}, Missing: "the vendor's pricing"}, ""},
		{"a no-sources result that does not say what is missing is rejected", Result{Outcome: NoSources, Sources: []string{"https://x"}}, "what is missing"},
		{"a result with no source is rejected", with(func(r *Result) { r.Sources = nil }), "no source"},
		{"a finding with no title is rejected", with(func(r *Result) { r.Title = " " }), "title"},
		{"a description spanning lines is rejected", with(func(r *Result) { r.Description = "a\nb" }), "description"},
		{"a description ending with a period is rejected", with(func(r *Result) { r.Description = "auth modes." }), "period"},
		{"a description naming the ticket is rejected", with(func(r *Result) { r.Description = "auth modes (#13)" }), "issue number"},
		{"a finding with no gist is rejected", with(func(r *Result) { r.Gist = "" }), "gist is empty"},
		{"a gist of two paragraphs is rejected", with(func(r *Result) { r.Gist = "One.\n\nTwo." }), "one paragraph"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.Check()
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCheckFindings(t *testing.T) {
	header := "# Finding\n\nTicket: [#13](u) (part of wayfinder map\n[#9](u) — Reusable auto-agent).\nResearched on 2026-10-12.\n\nSources: https://x\n\n## TL;DR\n\n- yes\n"
	write := func(t *testing.T, content string) string {
		dir := t.TempDir()
		path := filepath.Join(dir, filepath.FromSlash(input.Path))
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Run("a file with the four header facts and a TL;DR, changed alone, passes", func(t *testing.T) {
		if err := CheckFindings(write(t, header), input, "docs/research/", []string{input.Path}); err != nil {
			t.Error(err)
		}
	})
	t.Run("a Step that also edits README.md is told which file is outside the research prefix", func(t *testing.T) {
		err := CheckFindings(write(t, header), input, "docs/research/", []string{"README.md", input.Path})
		if err == nil || !strings.Contains(err.Error(), "outside the research prefix docs/research/: README.md") {
			t.Errorf("CheckFindings = %v", err)
		}
	})
	t.Run("a second file inside the prefix is still one file too many", func(t *testing.T) {
		err := CheckFindings(write(t, header), input, "docs/research/", []string{input.Path, "docs/research/scratch.md"})
		if err == nil || !strings.Contains(err.Error(), "not the findings file: docs/research/scratch.md") {
			t.Errorf("CheckFindings = %v", err)
		}
	})
	t.Run("a file that was not written is named", func(t *testing.T) {
		err := CheckFindings(t.TempDir(), input, "docs/research/", nil)
		if err == nil || !strings.Contains(err.Error(), "was not written") {
			t.Errorf("CheckFindings = %v", err)
		}
	})
	for _, tt := range []struct{ name, drop, want string }{
		{"the ticket", "Ticket: [#13](u) (part of wayfinder map\n", "the ticket"},
		{"the map", "[#9](u) — Reusable auto-agent).\n", "the map"},
		{"the date", "Researched on 2026-10-12.\n", "the date"},
		{"the sources", "Sources: https://x\n", "the sources"},
		{"the TL;DR", "## TL;DR\n", "TL;DR"},
		{"the title", "# Finding\n", "heading"},
	} {
		t.Run("a file missing "+tt.name+" is rejected, saying so", func(t *testing.T) {
			err := CheckFindings(write(t, strings.Replace(header, tt.drop, "", 1)), input, "docs/research/", []string{input.Path})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("CheckFindings = %v, want %q", err, tt.want)
			}
		})
	}
	t.Run("a header fact after the TL;DR does not count", func(t *testing.T) {
		moved := strings.Replace(header, "Sources: https://x\n", "", 1) + "\nSources: https://x\n"
		if err := CheckFindings(write(t, moved), input, "docs/research/", []string{input.Path}); err == nil || !strings.Contains(err.Error(), "the sources") {
			t.Errorf("CheckFindings = %v", err)
		}
	})
	t.Run("an unmapped ticket's file says so in place of the map", func(t *testing.T) {
		unmapped := input
		unmapped.Map = nil
		body := "# Finding\n\nTicket: [#13](u) (unmapped).\nResearched on 2026-10-12.\n\nSources: https://x\n\n## TL;DR\n\n- yes\n"
		if err := CheckFindings(write(t, body), unmapped, "docs/research/", []string{input.Path}); err != nil {
			t.Error(err)
		}
		if err := CheckFindings(write(t, header), unmapped, "docs/research/", []string{input.Path}); err == nil {
			t.Error("a file naming a map for an unmapped ticket passed")
		}
	})
}

// Ported from the slugify of plugin/skills/afk-pickup §2b and
// plugin/skills/afk-resolve §1.
func TestSlug(t *testing.T) {
	for _, tt := range []struct{ title, want string }{
		{"Research: Claude auth modes", "research-claude-auth-modes"},
		{"  --Which merge gate?--  ", "which-merge-gate"},
		{"Ünïcode & symbols: v2.0!", "n-code-symbols-v2-0"},
		{strings.Repeat("abc ", 20), "abc-abc-abc-abc-abc-abc-abc-abc-abc-abc-abc-abc-abc-abc-abc-"},
	} {
		if got := Slug(tt.title); got != tt.want {
			t.Errorf("Slug(%q) = %q, want %q", tt.title, got, tt.want)
		}
	}
	t.Run("a title with no ASCII letter or digit falls back to its number", func(t *testing.T) {
		if got := SlugOr("数据库选型？", "ticket-31"); got != "ticket-31" {
			t.Errorf("SlugOr = %q", got)
		}
		if got := SlugOr("Which gate?", "ticket-31"); got != "which-gate" {
			t.Errorf("SlugOr = %q", got)
		}
		if got := TicketSlug("数据库选型？", 31); got != "ticket-31" {
			t.Errorf("TicketSlug = %q", got)
		}
		if got := MapSlug("！！", 4); got != "map-4" {
			t.Errorf("MapSlug = %q", got)
		}
		if got := MapSlug("Reusable auto-agent", 4); got != "reusable-auto-agent" {
			t.Errorf("MapSlug = %q", got)
		}
	})
	t.Run("the findings path is <prefix><map-slug>/<ticket-slug>.md, and the branch research/<ticket-slug>", func(t *testing.T) {
		if got := Path("docs/research/", Slug("Reusable auto-agent"), "x"); got != "docs/research/reusable-auto-agent/x.md" {
			t.Errorf("Path = %q", got)
		}
		if got := Path("notes/", Unmapped, "x"); got != "notes/unmapped/x.md" {
			t.Errorf("Path = %q", got)
		}
		if got := Branch("x"); got != "research/x" {
			t.Errorf("Branch = %q", got)
		}
	})
}

// Ported from lib/docs-research-paths.test.sh: docs_research_only. The
// prefix comes from the Harness config, whose loader has the cases of
// docs_research_prefix; a set the bash could not read as JSON is a type Go
// does not let in.
func TestDocsOnly(t *testing.T) {
	for _, tt := range []struct {
		name  string
		paths []string
		want  bool
	}{
		{"every path under the prefix, at least one: true", []string{"docs/research/a.md", "docs/research/b/c.md"}, true},
		{"one path outside the prefix poisons the whole set", []string{"docs/research/a.md", "apps/backend/x.ts"}, false},
		{"an empty set is not docs-only", []string{}, false},
		{"the prefix is a prefix, not a substring", []string{"x/docs/research/a.md"}, false},
	} {
		if got := DocsOnly(tt.paths, "docs/research/"); got != tt.want {
			t.Errorf("%s: DocsOnly(%q) = %v", tt.name, tt.paths, got)
		}
	}
}

func TestQuestion(t *testing.T) {
	t.Run("the ## Question section, verbatim, up to the next heading", func(t *testing.T) {
		body := "## Question\n\nWhich gate?\n\n- a\n- b\n\n## Notes\n\nnot this"
		if got := Question(body); got != "Which gate?\n\n- a\n- b" {
			t.Errorf("Question = %q", got)
		}
	})
	t.Run("a body with no ## Question is the question whole", func(t *testing.T) {
		if got := Question("\nWhich gate?\n"); got != "Which gate?" {
			t.Errorf("Question = %q", got)
		}
	})
	t.Run("a Map's Destination and Notes", func(t *testing.T) {
		body := "## Destination\n\nA Daemon.\n\n## Notes\n\nDocs first.\n\n## Decisions so far\n\n- x"
		if Section(body, "Destination") != "A Daemon." || Section(body, "Notes") != "Docs first." || Section(body, "Fog") != "" {
			t.Errorf("sections = %q, %q", Section(body, "Destination"), Section(body, "Notes"))
		}
	})
}

func TestPullRequest(t *testing.T) {
	result := Result{Outcome: Found, Title: "T", Description: "claude code headless auth modes", Gist: "A login or a key.", Sources: []string{"https://x"}}
	body := PullRequestBody(input, result, "research-claude-auth-modes", time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC))
	t.Run("the title is docs(research): <short description> (#N)", func(t *testing.T) {
		if got := Subject(result, 13); got != "docs(research): claude code headless auth modes (#13)" {
			t.Errorf("Subject = %q", got)
		}
	})
	t.Run("the body has the question, the answer, the ticket it resolves and the marker", func(t *testing.T) {
		for _, part := range []string{
			"## Question\n\nWhich auth modes does Claude Code support headless?\n",
			"## Answer\n\nA login or a key.\n",
			"Resolves ticket #13 — Research: Claude auth modes\n",
			"\n<!-- afk-resolve ticket:#13 map:#9 slug:research-claude-auth-modes -->\n",
		} {
			if !strings.Contains(body, part) {
				t.Errorf("the body lacks %q:\n%s", part, body)
			}
		}
	})
	t.Run("the body never closes the ticket on merge: no closing keyword before an issue", func(t *testing.T) {
		// GitHub's closing keywords, as its docs list them.
		closing := regexp.MustCompile(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?)\b:?\s+#\d`)
		if found := closing.FindString(body); found != "" {
			t.Errorf("the body closes the ticket with %q", found)
		}
	})
	t.Run("an unmapped ticket's marker says so", func(t *testing.T) {
		if got := Marker(13, 0, "s"); got != "<!-- afk-resolve ticket:#13 map:unmapped slug:s -->" {
			t.Errorf("Marker = %q", got)
		}
	})
}
