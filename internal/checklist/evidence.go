package checklist

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The Evidence sink, unchanged from the bash it replaces (lib/evidence.sh):
// one directory per round under the State dir (never inside a checkout,
// which the Daemon resets), one screenshot filename grammar, and one
// `## Screenshots` section of the PR body that a later round replaces rather
// than appends to.

// ScreenshotsMarker sits under the tour's heading: it is what tells the
// harness's section from a `## Screenshots` a human wrote, which is never
// touched.
const ScreenshotsMarker = "<!-- auto-agent-screenshots -->"

// Dir creates the round's evidence directory and says where it is:
// `<stateDir>/verify/pr-<pr>/<UTC stamp>[-r<round>]`. round 0 is a round with
// no number.
func Dir(stateDir string, pr, round int, now time.Time) (string, error) {
	if pr <= 0 {
		return "", fmt.Errorf("evidence dir: PR %d is not a PR number", pr)
	}
	if round < 0 {
		return "", fmt.Errorf("evidence dir: round %d is not a round number", round)
	}
	name := now.UTC().Format("20060102T150405Z")
	if round > 0 {
		name += fmt.Sprintf("-r%d", round)
	}
	dir := filepath.Join(stateDir, "verify", fmt.Sprintf("pr-%d", pr), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

var (
	surfaceName = regexp.MustCompile(`^[a-z0-9_-]+$`)
	slugName    = regexp.MustCompile(`^[a-z0-9-]+$`)
	shotName    = regexp.MustCompile(`^([a-z0-9_-]+)-([0-9]+)-(.+)\.png$`)
)

// Name is the one legal screenshot filename, `<surface>-NN-<slug>.png`. The
// index is zero-padded so a listing is the reading order, and the slug is
// what a reviewer reads as the caption, so it has to be a slug.
func Name(surface string, index int, slug string) (string, error) {
	switch {
	case !surfaceName.MatchString(surface):
		return "", fmt.Errorf("evidence name: surface %q is not a Surface name", surface)
	case index < 0:
		return "", fmt.Errorf("evidence name: index %d is not a screenshot number", index)
	case !slugName.MatchString(slug):
		return "", fmt.Errorf("evidence name: slug %q is not a lower-case slug", slug)
	}
	return fmt.Sprintf("%s-%02d-%s.png", surface, index, slug), nil
}

// Shot is one screenshot of the tour: its caption and where it is, a URL or
// a path on the Host.
type Shot struct {
	Caption string
	Ref     string
}

// caption reads a screenshot's name back as `<surface> — <slug in words>`,
// or as the name itself when it is not in the grammar.
func caption(file string) string {
	base := filepath.Base(file)
	if m := shotName.FindStringSubmatch(base); m != nil {
		return m[1] + " — " + strings.ReplaceAll(m[3], "-", " ")
	}
	return strings.TrimSuffix(base, ".png")
}

// Shots is the tour in a directory, in reading order.
func Shots(dir string) ([]Shot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("evidence shots: %w", err)
	}
	var shots []Shot
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".png") {
			shots = append(shots, Shot{caption(e.Name()), filepath.Join(dir, e.Name())})
		}
	}
	return shots, nil
}

func isURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// Tour is the tour a manual-verifier's report lists as
// `ui-shot: <file> — <what it shows>` lines: each file in dir unless it is a
// URL or an absolute path, captioned with the surface and what it shows, in
// reading order (by filename, a URL after every file). A file listed twice is
// shot once, as first listed.
func Tour(report, dir string) []Shot {
	var shots []Shot
	seen := map[string]bool{}
	for _, line := range strings.Split(report, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ui-shot:")
		if !ok {
			continue
		}
		file, what, found := strings.Cut(strings.TrimSpace(rest), " — ")
		if !found {
			file, what, _ = strings.Cut(strings.TrimSpace(rest), " - ")
		}
		file, what = strings.TrimSpace(file), strings.TrimSpace(what)
		if file == "" {
			continue
		}
		ref := file
		if !isURL(ref) && !filepath.IsAbs(ref) {
			ref = filepath.Join(dir, ref)
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		shot := Shot{Caption: caption(file), Ref: ref}
		switch m := shotName.FindStringSubmatch(filepath.Base(file)); {
		case what != "" && isURL(file):
			shot.Caption = what
		case what != "" && m != nil:
			shot.Caption = m[1] + " — " + what
		case what != "":
			shot.Caption = what
		}
		shots = append(shots, shot)
	}
	slices.SortStableFunc(shots, func(a, b Shot) int {
		if ua, ub := isURL(a.Ref), isURL(b.Ref); ua != ub {
			if ua {
				return 1
			}
			return -1
		} else if ua {
			return 0
		}
		return strings.Compare(filepath.Base(a.Ref), filepath.Base(b.Ref))
	})
	return shots
}

// section is the tour's text, heading and marker first.
func section(shots []Shot) []string {
	lines := []string{"## Screenshots", ScreenshotsMarker, "",
		"_Captured live by the verification round on the Host, one shot per screen the diff touches._"}
	for i, s := range shots {
		title, alt := s.Caption, s.Caption
		if title == "" {
			title, alt = fmt.Sprintf("Screenshot %d", i+1), "screenshot"
		}
		lines = append(lines, "", "**"+title+"**", "")
		if isURL(s.Ref) {
			lines = append(lines, fmt.Sprintf("![%s](%s)", alt, s.Ref))
		} else {
			lines = append(lines, fmt.Sprintf("`%s` — on the Host, in this round's evidence directory", s.Ref))
		}
	}
	return lines
}

var (
	screenshotsHeading = regexp.MustCompile(`^## Screenshots\s*$`)
	checklistHeading   = regexp.MustCompile(`^## (Manual verification|Human verification required)\s*$`)
	rule               = regexp.MustCompile(`^---\s*$`)
)

// strip is the lines without any tour the harness wrote: from its heading
// (the marker under it) to the next `## ` heading or rule. A hand-written
// `## Screenshots` with no marker under it is left alone.
func strip(lines []string) []string {
	var kept []string
	for i := 0; i < len(lines); i++ {
		if !screenshotsHeading.MatchString(lines[i]) || i+1 >= len(lines) || strings.TrimRight(lines[i+1], "\r") != ScreenshotsMarker {
			kept = append(kept, lines[i])
			continue
		}
		i += 2
		for i < len(lines) && !strings.HasPrefix(lines[i], "## ") && !rule.MatchString(lines[i]) {
			i++
		}
		i--
	}
	return kept
}

func blank(line string) bool { return strings.TrimSpace(line) == "" }

// squeeze collapses each run of blank lines to one, as `cat -s` did in the
// bash this replaces, so stripping a tour never leaves a blank line behind
// for the next rewrite to stack another on.
func squeeze(lines []string) []string {
	var out []string
	for i, line := range lines {
		if blank(line) && i > 0 && blank(lines[i-1]) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Inject is the body with its tour replaced by shots: the section goes
// before the checklist it belongs to, else before the trailing rule, else at
// the end. No shots leaves the body byte-identical, so a round that captured
// nothing never destroys what an earlier round posted.
func Inject(body string, shots []Shot) string {
	if len(shots) == 0 {
		return body
	}
	lines := squeeze(strip(strings.Split(strings.TrimRight(body, "\n"), "\n")))
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	at := -1
	for i, line := range lines {
		if checklistHeading.MatchString(line) {
			at = i
			break
		}
	}
	if at < 0 {
		for i, line := range lines {
			if rule.MatchString(line) {
				at = i
			}
		}
	}
	var out []string
	if at < 0 {
		out = append(append(lines, ""), section(shots)...)
	} else {
		head := lines[:at]
		for len(head) > 0 && blank(head[len(head)-1]) {
			head = head[:len(head)-1]
		}
		out = append(out, head...)
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(append(append(out, section(shots)...), ""), lines[at:]...)
	}
	for len(out) > 0 && blank(out[len(out)-1]) {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n") + "\n"
}
