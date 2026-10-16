// Package thread reads the review threads of an Agent PR as the Daemon
// works them: who opened each, which replies are the Daemon's own, whether
// the human has ruled one, and whether one is a Dispute waiting on its
// answer. It is pure: threads in, what they say out. Who wrote a comment is
// told by marker, never by login: the Daemon replies to its own review under
// the same login as the review.
package thread

import (
	"fmt"
	"slices"
	"strings"

	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/github"
)

// The Daemon's markers on the replies it posts. Every reply carries exactly
// one, as its first line, and each means one thing.
const (
	// MarkerFix: a fix, recorded with its commit.
	MarkerFix = "<!-- auto-agent:fix -->"
	// MarkerArbiter: an Arbiter dismissal.
	MarkerArbiter = "<!-- auto-agent:arbiter -->"
	// MarkerRuling: a Ruling applied, with or without a commit.
	MarkerRuling = "<!-- auto-agent:ruling -->"
	// MarkerEscalate: the thread parked for a human; it resolves nothing.
	MarkerEscalate = "<!-- auto-agent:escalate -->"
	// MarkerDispute: a Dispute, the thread left open for the Arbiter (a
	// bot-authored thread) or the Ruling request (a human's).
	MarkerDispute = "<!-- auto-agent:dispute -->"
)

// markers are every marker of the Daemon's: the one place they are listed.
var markers = []string{MarkerFix, MarkerArbiter, MarkerRuling, MarkerEscalate, MarkerDispute}

// Who opened a thread.
const (
	// Bot: the one-time review opened it.
	Bot = "bot"
	// Human: anyone else did.
	Human = "human"
)

// Thread is one unresolved review thread, read.
type Thread struct {
	ID   string
	Path string
	// Line is 0 when the line has left the diff.
	Line int
	// Authored is Bot or Human.
	Authored string
	// First is the comment that opened it.
	First github.ThreadComment
	// Replies are every later comment, oldest first.
	Replies []Reply
}

// Reply is one later comment of a thread.
type Reply struct {
	github.ThreadComment
	// Daemon is set on a reply that carries one of the Daemon's markers.
	Daemon bool
}

// firstLine is a body's first line, without trailing space: where a marker
// is, and only there. A marker further down is someone quoting it.
func firstLine(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	return strings.TrimRight(line, " \t\r")
}

// Read reads a thread as GitHub listed it.
func Read(raw github.ReviewThread) Thread {
	t := Thread{ID: raw.ID, Path: raw.Path, Line: raw.Line, Authored: Human}
	if len(raw.Comments) == 0 {
		return t
	}
	t.First = raw.Comments[0]
	if finding.IsThread(t.First.Body) {
		t.Authored = Bot
	}
	for _, c := range raw.Comments[1:] {
		t.Replies = append(t.Replies, Reply{ThreadComment: c, Daemon: slices.Contains(markers, firstLine(c.Body))})
	}
	return t
}

// Ruling is the human's ruling on the thread: the last reply, when a human
// wrote it after the Daemon had replied in the thread. A human reply to a
// thread the Daemon never answered is more of the thread, not a ruling; one
// the Daemon answered after was acted on already. ok is false when there is
// none.
func (t Thread) Ruling() (ruling string, ok bool) {
	n := len(t.Replies)
	if n == 0 || t.Replies[n-1].Daemon {
		return "", false
	}
	if !slices.ContainsFunc(t.Replies[:n-1], func(r Reply) bool { return r.Daemon }) {
		return "", false
	}
	return t.Replies[n-1].Body, true
}

// Disputed says the Daemon's last word on the thread is a Dispute nobody
// has answered: it waits on the Arbiter or the human, not on a Fix.
func (t Thread) Disputed() bool {
	n := len(t.Replies)
	return n > 0 && firstLine(t.Replies[n-1].Body) == MarkerDispute
}

// Body is a reply's body: the marker on its own first line, then the text.
// A marker that is not one of the Daemon's is refused, so no reply goes out
// unmarked.
func Body(marker, text string) (string, error) {
	if !slices.Contains(markers, marker) {
		return "", fmt.Errorf("thread: %q is not one of the Daemon's reply markers", marker)
	}
	return marker + "\n" + text, nil
}
