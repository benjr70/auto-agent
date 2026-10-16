// Package checklist is the checklist protocol of a Verification round and its
// Evidence sink: what a round acts on in an Agent PR's body, the one mutation
// it may make to that body, where the round's artifacts live, the screenshot
// tour section, and the round's one evidence comment. Everything but the
// evidence directory is pure: text in, text out. Reading and writing the PR
// is the Fire's.
//
// The protocol, unchanged from the bash it replaces (lib/checklist.sh):
//   - only two sections hold verification items, `## Manual verification` and
//     `## Human verification required`; a `- [ ]` anywhere else (acceptance
//     criteria, a summary) is not a verification item and is never touched;
//   - only unticked items are acted on, so a re-run never re-verifies a box a
//     human already signed off;
//   - only items that passed are ticked; deferred and failed items keep their
//     empty box, and a ticked box is never un-ticked.
package checklist

import (
	"regexp"
	"strings"
)

// Section is which verification section an item sits in.
type Section string

const (
	// Manual: `## Manual verification`, items the verifier exercises.
	Manual Section = "manual"
	// Human: `## Human verification required`, items a human signs off.
	Human Section = "human"
)

// Item is one unticked verification item.
type Item struct {
	Section Section
	// Text is the item verbatim after its box, trailing space trimmed: what
	// Tick is handed back to tick it. A post-deploy tag stays in it.
	Text string
	// PostDeploy is the item's `<!-- post-deploy: … -->` tag content; empty
	// when it has none.
	PostDeploy string
}

var (
	heading    = regexp.MustCompile(`^\s*#+\s+(.*)$`)
	unticked   = regexp.MustCompile(`^(\s*[-*]\s+)\[\s\](\s+)(.*)$`)
	postDeploy = regexp.MustCompile(`\s*<!--\s*post-deploy:\s*(.*?)\s*-->`)
)

// Label is the item as a reader sees it: its text without the post-deploy
// tag, which a rendered body hides.
func (i Item) Label() string {
	return strings.TrimSpace(postDeploy.ReplaceAllString(i.Text, ""))
}

// sectionOf is the section a line opens, and whether it is a heading at all:
// any heading closes the current section, and only the two verification
// headings open one.
func sectionOf(line string) (Section, bool) {
	m := heading.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	text := strings.ToLower(strings.TrimRight(m[1], " \t\r"))
	switch {
	case strings.Contains(text, "manual verification"):
		return Manual, true
	case strings.Contains(text, "human verification"):
		return Human, true
	}
	return "", true
}

// Parse is every unticked item of the two verification sections, in body
// order. None is not an error: there is nothing to verify.
func Parse(body string) []Item {
	var items []Item
	var section Section
	for _, line := range strings.Split(body, "\n") {
		if s, ok := sectionOf(line); ok {
			section = s
			continue
		}
		if section == "" {
			continue
		}
		m := unticked.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		text := strings.TrimRight(m[3], " \t\r")
		if text == "" {
			continue
		}
		item := Item{Section: section, Text: text}
		if tag := postDeploy.FindStringSubmatch(text); tag != nil {
			item.PostDeploy = tag[1]
		}
		items = append(items, item)
	}
	return items
}

// Tick is the body with exactly the passed items' boxes ticked, matched by
// their Text, inside the two verification sections only. Every other byte of
// the body is kept, line endings included, and a ticked box is never touched,
// so ticking twice is a no-op.
func Tick(body string, passed []string) string {
	want := map[string]bool{}
	for _, p := range passed {
		if p = strings.TrimSpace(p); p != "" {
			want[p] = true
		}
	}
	if len(want) == 0 {
		return body
	}
	lines := strings.Split(body, "\n")
	var section Section
	for i, line := range lines {
		if s, ok := sectionOf(line); ok {
			section = s
			continue
		}
		if section == "" {
			continue
		}
		if m := unticked.FindStringSubmatch(line); m != nil && want[strings.TrimRight(m[3], " \t\r")] {
			lines[i] = m[1] + "[x]" + m[2] + m[3]
		}
	}
	return strings.Join(lines, "\n")
}
