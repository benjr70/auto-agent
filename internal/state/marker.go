package state

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// MaxAttempts is how many failed attempts at one Step a ticket gets before
// it is handed to a human.
const MaxAttempts = 3

// Attempts is the count of failed attempts at a Step, as the hidden marker
// on the ticket holds it.
type Attempts struct {
	Step  string
	Count int
}

var attemptsMarker = regexp.MustCompile(`<!-- auto-agent:attempts step=([a-z0-9-]+) count=([0-9]+) -->`)

// ParseAttempts reads the marker out of a comment; false when the comment
// carries none.
func ParseAttempts(comment string) (Attempts, bool) {
	m := attemptsMarker.FindStringSubmatch(comment)
	if m == nil {
		return Attempts{}, false
	}
	count, err := strconv.Atoi(m[2])
	if err != nil {
		return Attempts{}, false
	}
	return Attempts{Step: m[1], Count: count}, true
}

// After is the count once one more attempt at step has failed. Attempts at
// another Step do not carry over, and neither does a spent count: a human
// who takes AFK:failed off is asking for three more.
func (a Attempts) After(step string) Attempts {
	if a.Step != step || a.Spent() {
		a = Attempts{Step: step}
	}
	a.Count++
	return a
}

// Spent reports whether the ticket has used its attempts at the Step.
func (a Attempts) Spent() bool { return a.Count >= MaxAttempts }

// Comment is the ticket comment that holds the count: the hidden marker,
// then the same thing for a person.
func (a Attempts) Comment(reason string) string {
	text := fmt.Sprintf("<!-- auto-agent:attempts step=%s count=%d -->\nAttempt %d of %d at the %s Step failed: %s", a.Step, a.Count, a.Count, MaxAttempts, a.Step, reason)
	if a.Spent() {
		text += fmt.Sprintf("\n\n`%s` applied. A human takes over; taking the label off gives the Daemon %d more attempts.", LabelFailed, MaxAttempts)
	}
	return text
}

// pauseComment is the shape of the comment every pause leaves; the pauses of
// a ticket are counted by it, the bash harness's "Run paused" included.
var pauseComment = regexp.MustCompile(`(Run|Fire) paused at .*usage exhausted`)

// IsPause reports whether a ticket comment records a pause.
func IsPause(comment string) bool { return pauseComment.MatchString(comment) }

// PauseComment is the comment a pause leaves on the ticket. resetAt is when
// the budget comes back; empty when that is not known.
func PauseComment(at time.Time, resetAt string) string {
	text := fmt.Sprintf("Fire paused at %s: usage exhausted mid-Fire. Branch kept for resume.", at.UTC().Format(time.RFC3339))
	if resetAt != "" {
		text += " Budget resets at " + resetAt + "."
	}
	return text
}

// PauseCapComment is the comment left on a ticket failed at the pause cap.
func PauseCapComment(pauses int) string {
	return fmt.Sprintf("Paused %d times on the usage limit, the cap (`rounds.pause_resume`). `%s` applied: the ticket is too big for one budget window, and a human takes over.", pauses, LabelFailed)
}
