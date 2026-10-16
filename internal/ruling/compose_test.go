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

// prototypeSection is the nth section of the prototype after its request,
// without the blank lines at its edges.
func prototypeSection(t *testing.T, n int) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/prototypes/ruling-request-pr722.md")
	if err != nil {
		t.Fatal(err)
	}
	sections := strings.Split(string(data), "<!-- =====")
	_, section, _ := strings.Cut(sections[n], "-->\n")
	return strings.Trim(section, "\n")
}

// Ported from lib/ruling.test.sh, "the applied comment and the thread
// replies match the prototype (AC 1, 5)".
func TestApplied(t *testing.T) {
	results := []Applied{
		{N: 1, Letter: "A", Summary: "Fill time stays.", Thread: "useSmokeScreenBinding.ts:88"},
		{N: 2, Letter: "B", Summary: "Serve Plan written with the card hidden.", SHA: "9f3c1a0", Thread: "extractionContract.ts:912"},
	}
	t.Run("the applied comment is byte-identical to the prototype's", func(t *testing.T) {
		if got, want := strings.TrimRight(ComposeApplied(results, "9f3c1a0", "1A 2B", "CI green, manual 6/6. Ready to merge."), "\n"), prototypeSection(t, 1); got != want {
			t.Errorf("got\n%s\n\nwant\n%s", got, want)
		}
	})
	t.Run("a no-change thread reply reads as the prototype's in-thread line", func(t *testing.T) {
		if got := ThreadReply(1, "A", "Fill time stays.", ""); got != "Ruling 1A: Fill time stays. Resolving, no change." {
			t.Errorf("got %q", got)
		}
	})
	t.Run("a fixed thread reply names the commit", func(t *testing.T) {
		if got := ThreadReply(2, "B", "Serve Plan written with the card hidden.", "9f3c1a0"); got != "Ruling 2B: Serve Plan written with the card hidden. Fixed in `9f3c1a0`; resolving." {
			t.Errorf("got %q", got)
		}
	})
	t.Run("the prototype's in-thread section carries exactly that marker and the lib's reply line", func(t *testing.T) {
		if got := prototypeSection(t, 2); got != "<!-- auto-agent:ruling -->\n"+ThreadReply(1, "A", "Fill time stays.", "") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("a decision on no thread with no fix says no change, and nothing re-run says so", func(t *testing.T) {
		got := ComposeApplied([]Applied{{N: 1, Letter: "A", Summary: "Once."}, {N: 2, Letter: "B", Summary: "Always.", SHA: "abc1234"}}, "abc1234", "1A 2B", "")
		for _, want := range []string{"1. **A** — Once. No change.\n", "2. **B** — Always. Fixed in `abc1234`.\n", "Verification was not re-run on `abc1234`"} {
			if !strings.Contains(got, want) {
				t.Errorf("lacks %q:\n%s", want, got)
			}
		}
	})
}

// Ported from lib/ruling.test.sh, "an invalid one gets one marked nudge".
func TestNudge(t *testing.T) {
	t.Run("the nudge carries its marker, says what was expected per decision, and touches no label", func(t *testing.T) {
		got := Nudge(pr722(t), "free text is not read")
		for _, want := range []string{NudgeMarker + "\n", "(free text is not read)", "for example `1A 2A`", "- decision 1: A, B or C", "- decision 2: A or B"} {
			if !strings.Contains(got, want) {
				t.Errorf("lacks %q:\n%s", want, got)
			}
		}
	})
}
