package checklist

import (
	"slices"
	"strings"
	"testing"
)

// body is the PR body lib/checklist.test.sh reads: a box in a section that
// is not a verification section, a ticked box, and both verification
// sections.
const body = `Closes #12

## Summary

What this does.

## Acceptance criteria

- [ ] an acceptance box nobody verifies
- [x] a ticked acceptance box

## Manual verification

- [ ] the item list shows every posted item
- [ ] the health route answers while the process is up
- [x] already signed off by a human

## Human verification required

- [ ] the panel shows the reading on real hardware

## Notes

Nothing to see.
`

// Ported from lib/checklist.test.sh: only the unticked items of the two
// verification sections are read (behaviour 1).
func TestParse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []Item
	}{
		{"parse: sections and tags", body, []Item{
			{Section: Manual, Text: "the item list shows every posted item"},
			{Section: Manual, Text: "the health route answers while the process is up"},
			{Section: Human, Text: "the panel shows the reading on real hardware"},
		}},
		{"parse: nothing to verify", "## Summary\n\n- [ ] not a verification item\n", nil},
		{"parse: a post-deploy tag is kept with its item",
			"## Manual verification\n\n- [ ] the dashboard loads in prod <!-- post-deploy: e2e/prod.spec.ts -->\n", []Item{
				{Section: Manual, Text: "the dashboard loads in prod <!-- post-deploy: e2e/prod.spec.ts -->", PostDeploy: "e2e/prod.spec.ts"},
			}},
		{"parse: a CRLF body reads the same items",
			"## Manual verification\r\n\r\n- [ ] first item\r\n  * [ ] second item  \r\n", []Item{
				{Section: Manual, Text: "first item"},
				{Section: Manual, Text: "second item"},
			}},
		{"parse: a heading that is not a verification section closes the section",
			"## Manual verification\n- [ ] in\n### Details\n- [ ] out\n", []Item{
				{Section: Manual, Text: "in"},
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Parse(c.body); !slices.Equal(got, c.want) {
				t.Errorf("got %#v\nwant %#v", got, c.want)
			}
		})
	}
}

func TestLabel(t *testing.T) {
	item := Item{Text: "the dashboard loads <!-- post-deploy: e2e/prod.spec.ts -->"}
	if got := item.Label(); got != "the dashboard loads" {
		t.Errorf("got %q", got)
	}
}

// Ported from lib/checklist.test.sh: only the boxes handed in as passed are
// ticked, and nothing is ever un-ticked (behaviour 2).
func TestTick(t *testing.T) {
	ticked := Tick(body, []string{"the item list shows every posted item"})
	cases := []struct {
		name string
		got  string
		want []string
		not  []string
	}{
		{name: "tick: the passing box", got: ticked, want: []string{"\n- [x] the item list shows every posted item\n"}},
		{name: "tick: an item that did not pass keeps its box", got: ticked, want: []string{"\n- [ ] the health route answers while the process is up\n"}},
		{name: "tick: a deferred item keeps its box", got: ticked, want: []string{"\n- [ ] the panel shows the reading on real hardware\n"}},
		{name: "tick: other sections are untouched", got: ticked, want: []string{"\n- [ ] an acceptance box nobody verifies\n"}},
		{name: "tick: a ticked box stays ticked", got: Tick(body, []string{"nothing that appears anywhere"}),
			want: []string{"\n- [x] already signed off by a human\n", "\n- [x] a ticked acceptance box\n"}},
		{name: "tick: only the verification copy",
			got:  Tick("## Acceptance criteria\n\n- [ ] the same words\n\n## Manual verification\n\n- [ ] the same words\n", []string{"the same words"}),
			want: []string{"## Acceptance criteria\n\n- [ ] the same words\n\n## Manual verification\n\n- [x] the same words\n"}},
		{name: "tick: marker and indent",
			got:  Tick("## Manual verification\n\n  * [ ] an indented star item\n", []string{"an indented star item"}),
			want: []string{"\n  * [x] an indented star item\n"}},
		{name: "tick: a post-deploy item is ticked by its verbatim text",
			got:  Tick("## Manual verification\n- [ ] loads <!-- post-deploy: x -->\n", []string{"loads <!-- post-deploy: x -->"}),
			want: []string{"- [x] loads <!-- post-deploy: x -->\n"}},
		{name: "tick: a CRLF body keeps its line endings",
			got:  Tick("## Manual verification\r\n- [ ] first item\r\n- [ ] second\r\n", []string{" first item "}),
			want: []string{"## Manual verification\r\n- [x] first item\r\n- [ ] second\r\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, w := range c.want {
				if !strings.Contains(c.got, w) {
					t.Errorf("lacks %q:\n%s", w, c.got)
				}
			}
		})
	}
	t.Run("tick: empty pass list", func(t *testing.T) {
		if got := Tick(body, nil); got != body {
			t.Errorf("changed the body:\n%s", got)
		}
	})
	t.Run("tick: nothing else in the body changes", func(t *testing.T) {
		want := strings.Replace(body, "- [ ] the item list", "- [x] the item list", 1)
		if ticked != want {
			t.Errorf("got\n%s\nwant\n%s", ticked, want)
		}
	})
	t.Run("tick: ticking twice is a no-op", func(t *testing.T) {
		if again := Tick(ticked, []string{"the item list shows every posted item"}); again != ticked {
			t.Errorf("second tick changed the body:\n%s", again)
		}
	})
}
