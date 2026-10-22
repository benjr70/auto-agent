// Package graduate is the Graduate fog Step: once a Decision ticket is
// resolved, it proposes the new tickets its answer made specifiable, as
// plugin/skills/afk-resolve §6b did. The Step only proposes. Go holds the
// guardrails (how many, how deep, which are AFK) and does every write: the
// tickets, their parentage and blocking, their labels, the Map's edit and
// the pick signal.
package graduate

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
)

var (
	//go:embed prompt.md
	prompt string
	//go:embed schema.json
	schema string
)

// Name is the Step's name.
const Name = "graduate-fog"

// timeout is the wall-clock limit of one call.
const timeout = 20 * time.Minute

// Max is the most tickets one resolve graduates.
const Max = 3

// MaxAFKDepth is the deepest a new ticket may be and still be AFK: a ticket
// spawned by a ticket spawned by a ticket is a human's to look at first.
const MaxAFKDepth = 2

// Spec is the Step: read tools and read-only git on the checkout, so that it
// can read the findings file. It changes nothing.
func Spec() step.Spec {
	return step.ReadOnly(Name, prompt, schema, timeout)
}

// Input is what the Step is told.
type Input struct {
	Ticket Ticket `json:"ticket"`
	// Answer is the resolved ticket's answer, in one paragraph; Findings
	// the file on the default branch with its detail, empty when none.
	Answer   string `json:"answer"`
	Findings string `json:"findings"`
	Map      Map    `json:"map"`
	// Children are the Map's open tickets.
	Children []Child `json:"children"`
	Max      int     `json:"max"`
}

// Ticket is the resolved ticket.
type Ticket struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Question string `json:"question"`
}

// Map is the Map the ticket belongs to, section by section.
type Map struct {
	Number          int    `json:"number"`
	Title           string `json:"title"`
	URL             string `json:"url"`
	Destination     string `json:"destination"`
	Notes           string `json:"notes"`
	Decisions       string `json:"decisions"`
	NotYetSpecified string `json:"notYetSpecified"`
	OutOfScope      string `json:"outOfScope"`
}

// Child is one of the Map's open tickets.
type Child struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	// Type is its wayfinder type; empty when it has none.
	Type string `json:"type"`
}

// Result is what the Step answers. No proposal is a valid answer.
type Result struct {
	Proposals []Proposal `json:"proposals"`
}

// Proposal is one ticket to create.
type Proposal struct {
	Title    string `json:"title"`
	Question string `json:"question"`
	Type     string `json:"type"`
	// BlockedBy are open children it waits on, by number;
	// BlockedByProposals the proposals before it it waits on, from 1.
	BlockedBy          []int `json:"blockedBy"`
	BlockedByProposals []int `json:"blockedByProposals"`
	HumanFree          bool  `json:"humanFree"`
	// Clears is the text of the Map's Not yet specified it replaces; empty
	// when it clears none.
	Clears string `json:"clears"`
}

// Types are the wayfinder types a proposal may have.
var Types = []string{"research", "task", "grilling", "prototype"}

// Check re-checks what the schema cannot express, against what the Step was
// told: no more than the most it may propose, each a one-line title and a
// question, blocked only by open children and by proposals before it.
func (r Result) Check(in Input) error {
	if len(r.Proposals) > in.Max {
		return fmt.Errorf("the result proposes %d tickets; at most %d may be created per resolve: keep the %d the answer made sharpest, and leave the rest in Not yet specified", len(r.Proposals), in.Max, in.Max)
	}
	open := map[int]bool{}
	for _, child := range in.Children {
		open[child.Number] = true
	}
	for i, p := range r.Proposals {
		n := i + 1
		switch {
		case strings.TrimSpace(p.Title) == "" || strings.ContainsAny(p.Title, "\r\n"):
			return fmt.Errorf("proposal %d: the title is not one line of text", n)
		case strings.TrimSpace(p.Question) == "":
			return fmt.Errorf("proposal %d: the question is empty", n)
		case !slices.Contains(Types, p.Type):
			return fmt.Errorf("proposal %d: the type %q is not one of %s", n, p.Type, strings.Join(Types, ", "))
		}
		for _, b := range p.BlockedBy {
			if !open[b] {
				return fmt.Errorf("proposal %d is blocked by #%d, which is not one of the Map's open tickets", n, b)
			}
		}
		for _, b := range p.BlockedByProposals {
			if b < 1 || b >= n {
				return fmt.Errorf("proposal %d is blocked by proposal %d, which does not come before it: list a blocker before what it blocks", n, b)
			}
		}
	}
	return nil
}

// spawnedBy is a ticket body's provenance line.
var spawnedBy = regexp.MustCompile(`(?m)^Spawned by #([0-9]+)\b`)

// SpawnedBy is the ticket a ticket's body says spawned it; ok is false when
// it says none, as a ticket a human created does.
func SpawnedBy(body string) (number int, ok bool) {
	m := spawnedBy.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	number, _ = strconv.Atoi(m[1])
	return number, number > 0
}

// errCycle is a Spawned by chain that comes back on itself.
var errCycle = errors.New("the Spawned by chain loops")

// Depth is the length of the Spawned by chain from a ticket with body back to
// one a human created, which is depth 0. The chain is followed through read
// no further than it takes to tell whether a ticket the ticket spawns may be
// AFK: a depth past MaxAFKDepth is returned as MaxAFKDepth. A chain that
// cannot be read is an error, and so is one that loops.
func Depth(body string, read func(number int) (string, error)) (int, error) {
	seen := map[int]bool{}
	depth := 0
	for depth < MaxAFKDepth {
		parent, ok := SpawnedBy(body)
		if !ok {
			return depth, nil
		}
		if seen[parent] {
			return depth, errCycle
		}
		seen[parent] = true
		depth++
		var err error
		if body, err = read(parent); err != nil {
			return depth, fmt.Errorf("reading #%d, up the Spawned by chain: %w", parent, err)
		}
	}
	return depth, nil
}

// Route is the label a new ticket gets: AFK only for research, or a
// human-free task, at depth MaxAFKDepth or less. Everything else is HITL,
// whatever the Step said.
func Route(typ string, humanFree bool, depth int) string {
	if depth <= MaxAFKDepth && (typ == "research" || (typ == "task" && humanFree)) {
		return state.LabelAFK
	}
	return state.LabelHITL
}

// Body is a new ticket's body: its question, and the ticket that spawned it.
func Body(p Proposal, spawner int) string {
	return fmt.Sprintf("## Question\n\n%s\n\nSpawned by #%d\n", strings.TrimSpace(p.Question), spawner)
}

// fogHeading is the Map section graduated fog is cleared from.
var fogHeading = regexp.MustCompile(`(?i)^##\s*not yet specified\s*$`)

// ClearFog is the Map's body with text cut out of its Not yet specified, and
// the line it was on with it when nothing else is left there but a list
// marker. Nothing outside that section changes. ok is false, and the body is
// returned as it is, when the section does not hold the text.
func ClearFog(body, text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return body, false
	}
	lines := strings.SplitAfter(body, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		trimmed := strings.TrimRight(l, "\r\n")
		if start < 0 {
			if fogHeading.MatchString(trimmed) {
				start = i
			}
			continue
		}
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "# ") {
			end = i
			break
		}
	}
	if start < 0 {
		return body, false
	}
	before := strings.Join(lines[:start+1], "")
	section := strings.Join(lines[start+1:end], "")
	after := strings.Join(lines[end:], "")
	// A line that is the text, bullet and all, goes first: the same text
	// inside an earlier, longer bullet is a different fog item.
	offset := 0
	for _, l := range lines[start+1 : end] {
		item := strings.TrimSpace(bullet.ReplaceAllString(strings.TrimSpace(l), ""))
		if item == text {
			return before + section[:offset] + section[offset+len(l):] + after, true
		}
		offset += len(l)
	}
	at := strings.Index(section, text)
	if at < 0 {
		return body, false
	}
	from, to := at, at+len(text)
	// The whole line goes when only a list marker and blanks are left of it.
	lineStart := strings.LastIndex(section[:from], "\n") + 1
	lineEnd := len(section)
	if nl := strings.Index(section[to:], "\n"); nl >= 0 {
		lineEnd = to + nl + 1
	}
	rest := section[lineStart:from] + section[to:lineEnd]
	if strings.TrimSpace(bullet.ReplaceAllString(strings.TrimSpace(rest), "")) == "" {
		from, to = lineStart, lineEnd
	}
	return before + section[:from] + section[to:] + after, true
}

// bullet is a Markdown list item's marker.
var bullet = regexp.MustCompile(`^[-*+]\s*`)
