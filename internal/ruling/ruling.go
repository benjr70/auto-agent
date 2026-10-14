// Package ruling reads the Ruling request: the one comment through which
// everything on an Agent PR that needs the human reaches them, and the
// one-line Ruling (`1A 2B`) they answer with. It is pure: comments in,
// what they say out. Composing and posting a request are the Fire's.
package ruling

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/benjr70/auto-agent/internal/github"
)

// The markers that open the loop's own comments. They are the harness's
// fixed vocabulary, shared with the bash until cut-over.
const (
	RequestMarker   = "<!-- auto-agent:ruling-request"
	AppliedMarker   = "<!-- auto-agent:ruling-applied"
	DecisionsMarker = "<!-- auto-agent:ruling-decisions"
	NudgeMarker     = "<!-- auto-agent:ruling-nudge -->"
)

// Decision is one question a Ruling request puts to the human.
type Decision struct {
	Title    string          `json:"title"`
	Scenario string          `json:"scenario"`
	Now      string          `json:"now"`
	Wants    string          `json:"wants"`
	Why      string          `json:"why"`
	Thread   json.RawMessage `json:"thread,omitempty"`
	Options  []Option        `json:"options"`
}

// Option is one answer a decision offers.
type Option struct {
	// Letter is the option's own; empty means its position's (A, B, …).
	Letter      string `json:"letter,omitempty"`
	Text        string `json:"text"`
	Cost        string `json:"cost"`
	Recommended bool   `json:"recommended"`
	// Fix marks an option that changes code when chosen.
	Fix bool `json:"fix"`
}

// letters are each decision's option letters, upper-case: an option's own
// letter, else its position's.
func letters(decisions []Decision) [][]string {
	all := make([][]string, len(decisions))
	for i, d := range decisions {
		for j, o := range d.Options {
			letter := strings.ToUpper(o.Letter)
			if letter == "" && j < 26 {
				letter = string(rune('A' + j))
			}
			all[i] = append(all[i], letter)
		}
	}
	return all
}

// Status is how a reply reads as a Ruling.
type Status string

const (
	// Full answers every decision.
	Full Status = "full"
	// Partial answers some: apply what is named, ask again for the rest.
	Partial Status = "partial"
	// Invalid is not a Ruling, and applies nothing.
	Invalid Status = "invalid"
)

// Parsed is a reply read against the decisions it answers.
type Parsed struct {
	Status Status `json:"status"`
	// Answers are the letters chosen, by decision number from 1.
	Answers map[int]string `json:"answers"`
	// Missing are the decisions not answered.
	Missing []int `json:"missing"`
	// Ruling is the canonical form: "1A 2B".
	Ruling string `json:"ruling"`
	// Reason says why a reply is invalid.
	Reason string `json:"reason"`
}

var (
	pairsOnly = regexp.MustCompile(`^([0-9]+[A-Z])+$`)
	pair      = regexp.MustCompile(`([0-9]+)([A-Z])`)
)

// Parse reads a reply as a Ruling. After case and spacing are dropped it
// must be only decision-letter pairs (`1a 2B`, `1A2B` and `1A 2B` are the
// same Ruling), each naming a decision and one of its letters, with no
// decision answered twice with different letters.
func Parse(decisions []Decision, reply string) Parsed {
	n := len(decisions)
	invalid := func(reason string) Parsed {
		missing := make([]int, n)
		for i := range missing {
			missing[i] = i + 1
		}
		return Parsed{Status: Invalid, Answers: map[int]string{}, Missing: missing, Reason: reason}
	}
	compact := strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, reply))
	switch {
	case compact == "":
		return invalid("empty reply; expected decision-letter pairs such as 1A 2B")
	case !pairsOnly.MatchString(compact):
		return invalid("not only decision-letter pairs; free text is not read")
	}
	options := letters(decisions)
	answers := map[int]string{}
	var unknown, badLetter, contradicted []string
	for _, m := range pair.FindAllStringSubmatch(compact, -1) {
		number, _ := strconv.Atoi(m[1])
		letter := m[2]
		switch {
		case number < 1 || number > n:
			unknown = appendOnce(unknown, strconv.Itoa(number))
		case !slices.Contains(options[number-1], letter):
			badLetter = appendOnce(badLetter, m[1]+letter)
		case answers[number] != "" && answers[number] != letter:
			contradicted = appendOnce(contradicted, strconv.Itoa(number))
		default:
			answers[number] = letter
		}
	}
	switch {
	case len(unknown) > 0:
		return invalid(fmt.Sprintf("no decision %s; decisions are 1..%d", strings.Join(unknown, ", "), n))
	case len(badLetter) > 0:
		return invalid(fmt.Sprintf("no such option %s; the letters are in the request", strings.Join(badLetter, ", ")))
	case len(contradicted) > 0:
		return invalid(fmt.Sprintf("decision %s answered twice with different letters", strings.Join(contradicted, ", ")))
	}
	return complete(n, answers)
}

// complete is the Ruling the answers make, full or partial.
func complete(n int, answers map[int]string) Parsed {
	parsed := Parsed{Status: Full, Answers: answers, Missing: []int{}}
	var named []string
	for number := 1; number <= n; number++ {
		if letter, ok := answers[number]; ok {
			named = append(named, strconv.Itoa(number)+letter)
		} else {
			parsed.Missing = append(parsed.Missing, number)
		}
	}
	if len(parsed.Missing) > 0 {
		parsed.Status = Partial
	}
	parsed.Ruling = strings.Join(named, " ")
	return parsed
}

func appendOnce(list []string, item string) []string {
	if slices.Contains(list, item) {
		return list
	}
	return append(list, item)
}

// Request is the Ruling request a PR is waiting on.
type Request struct {
	// Comment is the request comment's id.
	Comment int64 `json:"id"`
	// Head is the commit the request was asked about.
	Head      string     `json:"head"`
	Decisions []Decision `json:"decisions"`
}

// Reply is the human's answer to a request, read from every comment after
// it that is not the loop's own.
type Reply struct {
	// Comment is the latest such comment's id; Body its text.
	Comment int64  `json:"id"`
	Body    string `json:"body"`
	Parsed
	// Nudged says the one nudge this request may get has gone out.
	Nudged bool `json:"nudged"`
}

// Pending is what a PR's comments say about its Ruling request.
type Pending struct {
	// Request is the latest request not yet followed by an applied comment;
	// nil when none is waiting.
	Request *Request `json:"request"`
	// Reply is nil when nothing after the request could be the human's.
	Reply *Reply `json:"reply"`
}

// Replied says the human has answered the waiting request with a Ruling,
// full or partial: what re-admits a PR to the reconcile Lane.
func (p Pending) Replied() bool {
	return p.Request != nil && p.Reply != nil && p.Reply.Status != Invalid
}

var (
	headField      = regexp.MustCompile(`head=([^ ]+)`)
	decisionsBlock = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(DecisionsMarker) + ` ([A-Za-z0-9+/=]+) -->[ \t\r]*$`)
)

// firstLine is a comment's first line, without trailing space.
func firstLine(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	return strings.TrimRightFunc(line, unicode.IsSpace)
}

// Decisions reads back the decisions a request comment carries.
func Decisions(body string) ([]Decision, error) {
	m := decisionsBlock.FindStringSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("no decisions block in that comment")
	}
	data, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return nil, fmt.Errorf("reading the decisions block: %w", err)
	}
	var decisions []Decision
	if err := json.Unmarshal(data, &decisions); err != nil {
		return nil, fmt.Errorf("reading the decisions block: %w", err)
	}
	return decisions, nil
}

// Read finds the Ruling request in a PR's comments, oldest first, and the
// human's answer to it. login is the machine user's: its comments are the
// loop's own and never a reply, bar one that is nothing but a valid Ruling,
// which is the human answering on a shared account. A comment carrying a
// hidden marker on its first line is the loop's under any login. Every
// comment that parses as a Ruling adds its pairs, a later letter for a
// decision replacing an earlier one, so `1A` then `2B` is the one Ruling
// `1A 2B`; the reply is invalid only when none parses.
func Read(comments []github.Comment, login string) (Pending, error) {
	if login == "" {
		return Pending{}, fmt.Errorf("cannot tell the machine user's comments from the human's without its login")
	}
	at := -1
	for i, c := range comments {
		switch line := firstLine(c.Body); {
		case strings.HasPrefix(line, RequestMarker):
			at = i
		case strings.HasPrefix(line, AppliedMarker):
			at = -1
		}
	}
	if at < 0 {
		return Pending{}, nil
	}
	decisions, err := Decisions(comments[at].Body)
	if err != nil {
		return Pending{}, err
	}
	request := &Request{Comment: comments[at].ID, Decisions: decisions}
	if m := headField.FindStringSubmatch(firstLine(comments[at].Body)); m != nil {
		request.Head = m[1]
	}
	pending := Pending{Request: request}
	var (
		reply   *Reply
		answers = map[int]string{}
		nudged  bool
	)
	for _, c := range comments[at+1:] {
		line := firstLine(c.Body)
		if line == NudgeMarker {
			nudged = true
		}
		if strings.HasPrefix(line, "<!--") {
			continue
		}
		parsed := Parse(decisions, c.Body)
		if parsed.Status == Invalid && strings.EqualFold(c.Author, login) {
			continue
		}
		for number, letter := range parsed.Answers {
			answers[number] = letter
		}
		reply = &Reply{Comment: c.ID, Body: c.Body, Parsed: parsed}
	}
	if reply == nil {
		return pending, nil
	}
	if len(answers) > 0 {
		reply.Parsed = complete(len(decisions), answers)
	}
	reply.Nudged = nudged
	pending.Reply = reply
	return pending, nil
}
