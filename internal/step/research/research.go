// Package research is the Research Step: one Decision ticket's question,
// answered from primary sources into one findings file under the research
// prefix. The Step writes the file and answers; Go re-checks the file,
// commits it, pushes it and opens the research PR from the answer. It never
// writes to GitHub: the runner gives it no credential.
package research

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
)

var (
	//go:embed prompt.md
	prompt string
	//go:embed schema.json
	schema string
)

// Name is the Step's name.
const Name = "research"

// timeout is the wall-clock limit of one call.
const timeout = time.Hour

// Spec is the Research Step. It reads the checkout and the web, and edits
// files: what keeps it to the one findings file is Go's re-check of the
// clone, and what keeps it off GitHub is the runner, which gives it no
// credential. It has no shell.
func Spec() step.Spec {
	tools := []string{"Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch"}
	return step.Spec{Name: Name, Prompt: prompt, Schema: schema, Tools: tools, Allowed: tools, Timeout: timeout}
}

// Input is what the Step is told.
type Input struct {
	Ticket Ticket `json:"ticket"`
	// Question is the ticket's `## Question`, verbatim.
	Question string `json:"question"`
	// Map is the ticket's Map, its GitHub parent; nil when it could not be
	// read.
	Map *Map `json:"map"`
	// Path is the findings file, relative to the checkout.
	Path string `json:"path"`
	// Date is the day of the research, YYYY-MM-DD.
	Date string `json:"date"`
}

// Ticket is the Decision ticket.
type Ticket struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
}

// Map is the Map a Decision ticket belongs to, as far as the Step reads it.
type Map struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Destination string `json:"destination"`
	Notes       string `json:"notes"`
}

// The outcomes a result names.
const (
	// Found: the file is written and the result carries the finding.
	Found = "finding"
	// NoSources: no primary source answers the question.
	NoSources = "no-sources"
)

// Result is what the Step answers.
type Result struct {
	Outcome string `json:"outcome"`
	// Title is the finding's title; Description the short description the
	// PR title carries; Gist the answer in one paragraph.
	Title       string `json:"title"`
	Description string `json:"description"`
	Gist        string `json:"gist"`
	// Sources are the sources the file cites, or, for no-sources, the ones
	// tried.
	Sources []string `json:"sources"`
	// Missing says what is missing, for no-sources.
	Missing string `json:"missing"`
}

// Check re-checks what the schema cannot express.
func (r Result) Check() error {
	if len(r.Sources) == 0 {
		return errors.New("the result names no source")
	}
	if r.Outcome == NoSources {
		if strings.TrimSpace(r.Missing) == "" {
			return errors.New("a no-sources result does not say what is missing")
		}
		return nil
	}
	switch {
	case strings.TrimSpace(r.Title) == "" || strings.ContainsAny(r.Title, "\r\n"):
		return errors.New("the title is not one line of text")
	case strings.TrimSpace(r.Description) == "" || strings.ContainsAny(r.Description, "\r\n"):
		return errors.New("the description is not one line of text")
	case strings.HasSuffix(strings.TrimSpace(r.Description), "."):
		return errors.New("the description ends with a period")
	case issueRef.MatchString(r.Description):
		return errors.New("the description names an issue number")
	case strings.TrimSpace(r.Gist) == "":
		return errors.New("the gist is empty")
	case paragraphBreak.MatchString(strings.TrimSpace(r.Gist)):
		return errors.New("the gist is more than one paragraph")
	}
	return nil
}

var (
	issueRef       = regexp.MustCompile(`#\d`)
	paragraphBreak = regexp.MustCompile(`\n\s*\n`)
	nonAlnum       = regexp.MustCompile(`[^a-z0-9]+`)
)

// Slug is a title as the bash derived it everywhere: lower case,
// non-alphanumerics to hyphens, trimmed of them, cut to 60 characters.
func Slug(title string) string {
	slug := strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 60 {
		slug = slug[:60]
	}
	return slug
}

// SlugOr is the slug of a title, or fallback when the title has no ASCII
// letter or digit to make one of: an empty slug would make the branch
// `research/`, which no ref can be, and a path git never reports.
func SlugOr(title, fallback string) string {
	if slug := Slug(title); slug != "" {
		return slug
	}
	return fallback
}

// TicketSlug is a ticket's slug: its title's, else ticket-<n>.
func TicketSlug(title string, number int) string {
	return SlugOr(title, fmt.Sprintf("ticket-%d", number))
}

// MapSlug is a Map's slug: its title's, else map-<n>.
func MapSlug(title string, number int) string {
	return SlugOr(title, fmt.Sprintf("map-%d", number))
}

// Unmapped is the Map slug of a ticket whose Map could not be read.
const Unmapped = "unmapped"

// Path is the findings file of a ticket: <prefix><map-slug>/<ticket-slug>.md.
func Path(prefix, mapSlug, slug string) string {
	return prefix + mapSlug + "/" + slug + ".md"
}

// Branch is the research PR's branch.
func Branch(slug string) string { return "research/" + slug }

// DocsOnly is the one definition of a research docs PR: it changes at least
// one file, and every one is under the research prefix.
func DocsOnly(paths []string, prefix string) bool {
	for _, path := range paths {
		if !strings.HasPrefix(path, prefix) {
			return false
		}
	}
	return len(paths) > 0
}

// Section is the body of a ticket's `## <heading>` section, trimmed; empty
// when it has none.
func Section(body, heading string) string {
	var lines []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if title, ok := strings.CutPrefix(line, "## "); ok {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(title), heading)
			continue
		}
		if in {
			lines = append(lines, line)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Question is what a Decision ticket asks: its `## Question`, or its whole
// body when it has no such section.
func Question(body string) string {
	if question := Section(body, "Question"); question != "" {
		return question
	}
	return strings.TrimSpace(body)
}

// CheckFindings re-checks the clone at dir after the Step, for what a schema
// cannot say: changed are the paths the branch changes, committed or not,
// and the findings file must be the only one; it must exist and open with
// the four header facts and a TL;DR.
func CheckFindings(dir string, in Input, prefix string, changed []string) error {
	var outside, other []string
	for _, path := range changed {
		switch {
		case path == in.Path:
		case !strings.HasPrefix(path, prefix):
			outside = append(outside, path)
		default:
			other = append(other, path)
		}
	}
	if len(outside) > 0 || len(other) > 0 {
		var parts []string
		if len(outside) > 0 {
			parts = append(parts, "outside the research prefix "+prefix+": "+strings.Join(outside, ", "))
		}
		if len(other) > 0 {
			parts = append(parts, "inside it, but not the findings file: "+strings.Join(other, ", "))
		}
		return fmt.Errorf("the Step changed files other than the findings file %s (%s); undo those changes", in.Path, strings.Join(parts, "; "))
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(in.Path)))
	if err != nil {
		return fmt.Errorf("the findings file %s was not written", in.Path)
	}
	header, hasTLDR := string(data), false
	if loc := tldr.FindStringIndex(header); loc != nil {
		header, hasTLDR = header[:loc[0]], true
	}
	var missing []string
	if !hasTLDR {
		missing = append(missing, "a `## TL;DR` section")
	}
	if !title.MatchString(header) {
		missing = append(missing, "its `# <title>` heading")
	}
	if !ticketFact(in.Ticket.Number).MatchString(header) {
		missing = append(missing, fmt.Sprintf("the ticket (`Ticket: [#%d](…)`)", in.Ticket.Number))
	}
	if in.Map != nil && !mapFact(in.Map.Number).MatchString(header) {
		missing = append(missing, fmt.Sprintf("the map (`[#%d](…)`)", in.Map.Number))
	}
	if in.Map == nil && !strings.Contains(header, "(unmapped)") {
		missing = append(missing, "the map (`(unmapped)`, as the ticket has none)")
	}
	if !strings.Contains(header, "Researched on "+in.Date) {
		missing = append(missing, "the date (`Researched on "+in.Date+"`)")
	}
	if !sources.MatchString(header) {
		missing = append(missing, "the sources (`Sources: …`)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the findings file %s lacks %s before its TL;DR", in.Path, strings.Join(missing, ", "))
	}
	return nil
}

var (
	tldr    = regexp.MustCompile(`(?m)^##\s*TL;DR\s*$`)
	title   = regexp.MustCompile(`\A\s*# \S`)
	sources = regexp.MustCompile(`(?m)^Sources:[ \t]*\S`)
)

func ticketFact(number int) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^Ticket:.*#` + strconv.Itoa(number) + `\b`)
}

func mapFact(number int) *regexp.Regexp {
	return regexp.MustCompile(`\[#` + strconv.Itoa(number) + `\]`)
}

// Subject is the research PR's title and its commit's subject:
// docs(research): <description> (#N).
func Subject(r Result, ticket int) string {
	return fmt.Sprintf("docs(research): %s (#%d)", strings.TrimSpace(r.Description), ticket)
}

// Marker is the research PR body's machine marker: what lets the Fire that
// merges the PR find its way back to the ticket.
func Marker(ticket int, mapNumber int, slug string) string {
	m := Unmapped
	if mapNumber != 0 {
		m = "#" + strconv.Itoa(mapNumber)
	}
	return fmt.Sprintf("<!-- afk-resolve ticket:#%d map:%s slug:%s -->", ticket, m, slug)
}

// PullRequestBody is the research PR's body: the question, the answer in
// one paragraph, the ticket it resolves and the marker. It never names the
// ticket after a closing keyword, so merging it does not close the ticket:
// the Daemon closes it, with the resolution comment.
func PullRequestBody(in Input, r Result, slug string, now time.Time) string {
	mapNumber := 0
	if in.Map != nil {
		mapNumber = in.Map.Number
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Question\n\n%s\n\n", strings.TrimSpace(in.Question))
	fmt.Fprintf(&b, "## Answer\n\n%s\n\nFull findings: `%s`\n\n", strings.TrimSpace(r.Gist), in.Path)
	fmt.Fprintf(&b, "## Resolves\n\nResolves ticket #%d — %s\n\n", in.Ticket.Number, in.Ticket.Title)
	fmt.Fprintf(&b, "%s\n\n---\n\nGenerated by `auto-agent` at %s\n", Marker(in.Ticket.Number, mapNumber, slug), now.UTC().Format(time.RFC3339))
	return b.String()
}
