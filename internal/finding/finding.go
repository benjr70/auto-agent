// Package finding is what the one-time review of an Agent PR does with what
// its two Review Steps found: the posting bar that sorts each Finding into
// the route it takes, and the comments the Daemon posts for them. It is
// pure: Findings in, routes and comment bodies out. Only a defect opens a
// thread.
package finding

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Kind is the route a Finding takes.
type Kind string

const (
	// Defect opens a thread: a concrete failure with what triggers it, or a
	// written requirement contradicted and quoted.
	Defect Kind = "defect"
	// ProductAmbiguity never opens a thread: it goes to the human in the
	// Ruling request.
	ProductAmbiguity Kind = "product-ambiguity"
	// ReviewNote is listed once in the review's comment and acted on by
	// nobody.
	ReviewNote Kind = "review-note"
)

// The two axes of the review.
const (
	Correctness = "correctness"
	SpecAxis    = "spec"
)

// Finding is one thing a review found.
type Finding struct {
	Kind     Kind   `json:"kind"`
	Axis     string `json:"axis"`
	Category string `json:"category"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	// FailureScenario is "given X, the code does Y, and Z is seen"; for an
	// ambiguity, its readings.
	FailureScenario string `json:"failure_scenario"`
	// QuotedRequirement is the sentence a defect contradicts, verbatim;
	// empty for a concrete failure.
	QuotedRequirement string `json:"quoted_requirement"`
	// Demoted marks a defect the bar made a Review note.
	Demoted bool `json:"demoted,omitempty"`
}

// The Standards categories, and scope creep: a Review note whatever the
// reviewer called it. test-coverage is a note unless it quotes the sentence
// that asked for the test.
var (
	noteCategories   = regexp.MustCompile(`(?i)^(duplication|naming|test-structure|speculative-generality|page-object-bypass|style|note|scope-creep)$`)
	quoteGated       = regexp.MustCompile(`(?i)^test-coverage$`)
	noFailurePhrases = regexp.MustCompile(`(?i)^(none|n/?a|nothing( breaks| fails)?|no (concrete |runtime |observable |user-visible )?(failure|breakage|defect|bug)( scenario)?)[[:space:].!]*$`)
)

// Bar is the posting bar: it settles every Finding's kind. An ambiguity is
// never reclassified. A Standards category or scope creep is a Review note,
// and so is test-coverage that quotes no requirement. A defect whose failure
// scenario is empty or nothing but "none" / "n/a" / "no failure", and that
// quotes no requirement, is demoted to a Review note; an empty scenario with
// a quote stays a defect. A Finding with no kind is a defect.
func Bar(findings []Finding) []Finding {
	barred := make([]Finding, len(findings))
	for i, f := range findings {
		if f.Kind == "" {
			f.Kind = Defect
		}
		unquoted := strings.TrimSpace(f.QuotedRequirement) == ""
		scenario := strings.TrimSpace(f.FailureScenario)
		switch {
		case f.Kind == ProductAmbiguity:
		case noteCategories.MatchString(f.Category):
			f.Kind = ReviewNote
		case quoteGated.MatchString(f.Category) && unquoted:
			f.Kind = ReviewNote
		case f.Kind == Defect && unquoted && (scenario == "" || noFailurePhrases.MatchString(scenario)):
			f.Kind, f.Demoted = ReviewNote, true
		}
		barred[i] = f
	}
	return barred
}

// Routes are barred Findings by the route each takes.
type Routes struct {
	Defects     []Finding `json:"defects"`
	Notes       []Finding `json:"notes"`
	Ambiguities []Finding `json:"ambiguities"`
}

// Split sorts barred Findings into their routes. Only the defects open
// threads, and only defects earn AFK:revise.
func Split(barred []Finding) Routes {
	routes := Routes{Defects: []Finding{}, Notes: []Finding{}, Ambiguities: []Finding{}}
	for _, f := range barred {
		switch f.Kind {
		case Defect:
			routes.Defects = append(routes.Defects, f)
		case ProductAmbiguity:
			routes.Ambiguities = append(routes.Ambiguities, f)
		default:
			routes.Notes = append(routes.Notes, f)
		}
	}
	return routes
}

// near is how many lines apart two defects on one file may be and still be
// one thread.
const near = 3

var severityRank = map[string]int{"high": 0, "medium": 1, "low": 2}

// Thread is one thread to open: one defect, or several the two axes found
// at the same line.
type Thread struct {
	Path     string
	Line     int
	Findings []Finding
}

// Dedupe makes threads of defects: those at the same path and line share one
// thread, correctness first; of those on one path within three lines with the
// same summary, the most severe is kept. There is no cap: every other
// defect is a thread of its own.
func Dedupe(defects []Finding) []Thread {
	var threads []Thread
	for _, d := range defects {
		if d.Kind != Defect {
			// Only a defect opens a thread, whoever forgot the bar.
			continue
		}
		merged := false
		for i := range threads {
			t := &threads[i]
			switch {
			case t.Path == d.Path && t.Line == d.Line:
				t.Findings = append(t.Findings, d)
				sort.SliceStable(t.Findings, func(a, b int) bool {
					return t.Findings[a].Axis == Correctness && t.Findings[b].Axis != Correctness
				})
			case t.Path == d.Path && abs(t.Line-d.Line) <= near && slices.ContainsFunc(t.Findings, func(f Finding) bool { return sameSummary(f.Summary, d.Summary) }):
				if severityRank[d.Severity] < severityRank[t.Findings[0].Severity] {
					t.Line, t.Findings = d.Line, []Finding{d}
				}
			default:
				continue
			}
			merged = true
			break
		}
		if !merged {
			threads = append(threads, Thread{Path: d.Path, Line: d.Line, Findings: []Finding{d}})
		}
	}
	return threads
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// sameSummary says two summaries say the same thing, give or take case,
// spacing and punctuation.
func sameSummary(a, b string) bool { return words(a) == words(b) }

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

func words(s string) string {
	return strings.TrimSpace(nonWord.ReplaceAllString(strings.ToLower(s), " "))
}

// The markers on the review's comments. They are the harness's fixed
// vocabulary, shared with the bash until cut-over.
const (
	// ThreadMarker is the first line of every thread the review opens.
	ThreadMarker = "<!-- pr-review-bot -->"
	// DoneMarker opens the comment that says the review ran: the once-per-PR
	// gate.
	DoneMarker = "<!-- pr-review-done"
	// ambiguitiesMarker opens the structured block of product ambiguities
	// the Ruling request reads back.
	ambiguitiesMarker = "<!-- pr-review-ambiguities"
)

// IsThread says a review comment opened one of the review's threads: the
// marker is its first line, so a human quoting it further down is not.
func IsThread(body string) bool {
	line, _, _ := strings.Cut(body, "\n")
	return strings.TrimRightFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' }) == ThreadMarker
}

// Body is a thread's first comment: the marker, then each defect's header,
// summary, failure scenario and any requirement it contradicts, then how to
// dispute it.
func (t Thread) Body() string {
	var b strings.Builder
	b.WriteString(ThreadMarker + "\n")
	for i, f := range t.Findings {
		if i > 0 {
			b.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&b, "🤖 **pr-review** · defect · %s · %s · %s\n\n%s\n\n**Failure scenario:** %s\n", f.Axis, f.Category, f.Severity, f.Summary, f.FailureScenario)
		if q := strings.TrimSpace(f.QuotedRequirement); q != "" {
			fmt.Fprintf(&b, "\n**Contradicts:** > %s\n", q)
		}
	}
	b.WriteString("\n_Automated one-time review (the Fire's review round). A fix round follows; reply to dispute._")
	return b.String()
}

// FallbackBody is a thread's first comment posted on another line because
// GitHub refused to anchor it where it belongs: the location it was meant
// for is named after the header.
func (t Thread) FallbackBody() string {
	lines := strings.SplitN(t.Body(), "\n", 3)
	note := t.intended() + " (GitHub refused to anchor a comment there; posted on the file's first changed line)"
	return lines[0] + "\n" + lines[1] + "\n\n" + note + "\n" + lines[2]
}

// intended names the location a thread was meant for.
func (t Thread) intended() string {
	return fmt.Sprintf("**Intended location:** `%s:%d`", t.Path, t.Line)
}

// PostedAs says a review comment on path at line is this thread posted
// already: one of the review's threads saying the same thing, near its
// line, or posted elsewhere naming its line as the one it was meant for. A
// comment whose line has left the diff (line 0) is near nothing.
func (t Thread) PostedAs(path string, line int, body string) bool {
	if !IsThread(body) || path != t.Path || !strings.Contains(body, t.Findings[0].Summary) {
		return false
	}
	return (line > 0 && abs(line-t.Line) <= near) || strings.Contains(body, t.intended()+" ")
}

// IsDone says a PR comment is the one that says the review ran.
func IsDone(body string) bool { return strings.Contains(body, DoneMarker) }

// Done is the comment that says the review ran, posted last: the tally,
// the Review notes collapsed, the product ambiguities for the human and as
// a block the Ruling request reads back, then extra (anything else the
// review owes the human, as markdown). total counts every Finding, all three
// kinds; threads the defect threads the PR has; reviewed is the commit the
// review read.
func Done(total, threads int, reviewed string, routes Routes, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s reviewed=%s fixes=none -->\n", DoneMarker, reviewed)
	fmt.Fprintf(&b, "🤖 pr-review: %d findings, 0 fixed (reviewed %s) — %d defect thread(s), %d review note(s), %d product ambiguity(ies)\n",
		total, reviewed, threads, len(routes.Notes), len(routes.Ambiguities))
	if len(routes.Notes) > 0 {
		fmt.Fprintf(&b, "\n<details>\n<summary>Review notes, no action taken (%d)</summary>\n\n", len(routes.Notes))
		for _, n := range routes.Notes {
			fmt.Fprintf(&b, "- `%s:%d` · %s · %s — %s", n.Path, n.Line, n.Axis, n.Category, n.Summary)
			if n.Demoted {
				b.WriteString(" _(demoted: no concrete failure scenario, no quoted requirement)_")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n</details>\n")
	}
	if len(routes.Ambiguities) > 0 {
		fmt.Fprintf(&b, "\n**Product ambiguities (%d)** — no thread; routed to the Ruling request:\n\n", len(routes.Ambiguities))
		for _, a := range routes.Ambiguities {
			fmt.Fprintf(&b, "- `%s:%d` — %s\n", a.Path, a.Line, a.Summary)
		}
		// json.Marshal writes `>` as \u003e, so no `-->` in a string can end
		// the HTML comment early.
		data, _ := json.Marshal(routes.Ambiguities)
		fmt.Fprintf(&b, "\n%s\n%s\n-->\n", ambiguitiesMarker, data)
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		b.WriteString("\n" + extra + "\n")
	}
	return b.String()
}

// Ambiguities reads the product ambiguities back out of the comment Done
// wrote; none when it carries no block, or one that does not read.
func Ambiguities(body string) []Finding {
	_, after, found := strings.Cut(body, "\n"+ambiguitiesMarker+"\n")
	if !found {
		return []Finding{}
	}
	block, _, _ := strings.Cut(after, "\n-->")
	var ambiguities []Finding
	if json.Unmarshal([]byte(block), &ambiguities) != nil {
		return []Finding{}
	}
	return ambiguities
}
