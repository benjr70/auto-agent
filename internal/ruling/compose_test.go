package ruling

import (
	"os"
	"strings"
	"testing"
)

// prototypeRequest is the request section of the prototype, as the bash
// test reads it: everything before the first separator, without the
// PROTOTYPE banner and the blank lines at its edges.
func prototypeRequest(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/prototypes/ruling-request-pr722.md")
	if err != nil {
		t.Fatal(err)
	}
	section, _, _ := strings.Cut(string(data), "<!-- =====")
	_, section, _ = strings.Cut(section, "-->\n")
	return strings.Trim(section, "\n")
}

// visible is a composed request without its hidden decisions block.
func visible(body string) string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if !strings.HasPrefix(line, DecisionsMarker+" ") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// Ported from lib/ruling.test.sh, "the rendered request matches the
// prototype section for section (AC 1)".
func TestCompose(t *testing.T) {
	decisions := pr722(t)
	rendered, err := Compose(decisions, "2e204022", "CI green, manual verification 6/6", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the request body (marker, heading, instructions, both decisions, footer) is byte-identical", func(t *testing.T) {
		if got, want := visible(rendered), prototypeRequest(t); got != want {
			t.Errorf("rendered\n%s\n\nwant\n%s", got, want)
		}
	})
	t.Run("the decisions ride inside the comment as a hidden block the lib can read back", func(t *testing.T) {
		back, err := Decisions(rendered)
		if err != nil || len(back) != 2 || back[0].Title != decisions[0].Title || back[1].Options[1].Letter != "B" || string(back[0].Thread) == "" {
			t.Errorf("read back %+v, %v", back, err)
		}
	})
	t.Run("a one-decision request says '1 decision' and shows the single all-recommended reply", func(t *testing.T) {
		one, _ := Compose(decisions[1:2], "abc1234", "CI green", 0)
		for _, want := range []string{"\n## 🧑‍⚖️ Ruling request · 1 decision\n", "A reply of `1A` leaves the PR ready to merge", "<!-- auto-agent:ruling-request head=abc1234 decisions=1 -->\n"} {
			if !strings.Contains(one, want) {
				t.Errorf("lacks %q:\n%s", want, one)
			}
		}
	})
	t.Run("a request that supersedes an earlier open one says so, names it, and asks for the reply here", func(t *testing.T) {
		later, _ := Compose(decisions, "abc1234", "CI green", 4242)
		if !strings.Contains(later, "\nThis request supersedes the earlier one (comment 4242)") || !strings.HasPrefix(later, "<!-- auto-agent:ruling-request head=abc1234 decisions=2 -->\n") {
			t.Errorf("rendered\n%s", later)
		}
	})
	t.Run("without a superseded request the line is absent", func(t *testing.T) {
		if strings.Contains(rendered, "supersedes") {
			t.Error("it is there")
		}
	})
	t.Run("when a recommended option is a fix, the footer says the all-recommended reply needs one more Fire", func(t *testing.T) {
		fix := pr722(t)
		fix[0].Options[0].Recommended, fix[0].Options[2].Recommended = false, true
		got, _ := Compose(fix, "abc1234", "CI green", 0)
		if !strings.Contains(got, "A reply of `1C 2A` applies the fix and re-runs verification in one more Fire") {
			t.Errorf("rendered\n%s", got)
		}
	})
	t.Run("an option with no letter of its own is lettered by its position", func(t *testing.T) {
		unlettered := pr722(t)
		for i := range unlettered[1].Options {
			unlettered[1].Options[i].Letter = ""
		}
		got, _ := Compose(unlettered, "x", "y", 0)
		if !strings.Contains(got, "| **B** | Write the Serve Plan value") {
			t.Errorf("rendered\n%s", got)
		}
	})
	t.Run("no decisions is an error, not an empty request", func(t *testing.T) {
		if _, err := Compose(nil, "x", "y", 0); err == nil {
			t.Error("composed")
		}
	})
}

func TestCheck(t *testing.T) {
	good := pr722(t)[0]
	for _, c := range []struct {
		name   string
		change func(*Decision)
		want   string
	}{
		{"the prototype's decision passes", func(*Decision) {}, ""},
		{"no title", func(d *Decision) { d.Title = " " }, "no title"},
		{"one option", func(d *Decision) { d.Options = d.Options[:1] }, "fewer than two options"},
		{"an option with no text", func(d *Decision) { d.Options[1].Text = "" }, "no text"},
		{"a letter used twice", func(d *Decision) { d.Options[1].Letter = "a" }, "the letter A twice"},
		{"a letter outside A to Z", func(d *Decision) { d.Options[1].Letter = "7" }, `lettered "7"`},
		{"none recommended", func(d *Decision) { d.Options[0].Recommended = false }, "recommends 0 options"},
		{"two recommended", func(d *Decision) { d.Options[1].Recommended = true }, "recommends 2 options"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := good
			d.Options = append([]Option{}, good.Options...)
			c.change(&d)
			err := d.Check()
			if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
				t.Errorf("Check = %v, want %q", err, c.want)
			}
		})
	}
}
