package state

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/github"
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

// CredentialPauseComment is the comment a pause on a dead credential leaves
// on the ticket. It is not the ticket's fault, so IsPause does not count it.
func CredentialPauseComment(at time.Time) string {
	return fmt.Sprintf("Fire paused at %s: credential dead mid-Fire. Branch kept for resume.", at.UTC().Format(time.RFC3339))
}

// parkMarker is the hidden marker of the park issue.
const parkMarker = "<!-- auto-agent:parked -->"

// IsPark reports whether an issue body is a park issue's.
func IsPark(body string) bool { return strings.Contains(body, parkMarker) }

// interval is a probe interval as a person reads it: "1h", "1h30m", "45s".
func interval(d time.Duration) string {
	text := d.Round(time.Second).String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

// ParkIssue is the AFK:needs-human issue a Parked Daemon opens in the Target
// Project: what happened, and the one thing a human does about it.
func ParkIssue(host string, at time.Time, reason string, every time.Duration) github.NewIssue {
	return github.NewIssue{
		Title:  fmt.Sprintf("Daemon parked on %s: Claude credential dead", host),
		Labels: []string{LabelNeedsHuman},
		Body: fmt.Sprintf("%s\nThe Daemon on `%s` parked at %s: %s.\n\n"+
			"A dead credential is never exhaustion: no Step runs until a human logs in again on the Host (re-run `/login` over SSH, "+
			"or replace `CLAUDE_CODE_OAUTH_TOKEN` and restart the Daemon). The Daemon probes the credential every %s and closes this issue itself when the probe passes.",
			parkMarker, host, at.UTC().Format(time.RFC3339), reason, interval(every)),
	}
}

// ParkAgainComment is the comment on a park issue that was still open when
// the Daemon parked again.
func ParkAgainComment(host string, at time.Time, reason string, every time.Duration) string {
	return fmt.Sprintf("Parked again at %s on %s: %s. The Daemon probes the credential every %s and closes this issue when a login is back.",
		at.UTC().Format(time.RFC3339), host, reason, interval(every))
}

// UnparkComment is the comment the park issue is closed with.
func UnparkComment(at time.Time) string {
	return fmt.Sprintf("Un-parked at %s: the credential probe passes again. Fires resume.", at.UTC().Format(time.RFC3339))
}

// PreCommitCapComment is the comment left on a ticket failed at the
// pre-commit review cap, with what its last round said: the review's asks,
// or the smoke's failure.
func PreCommitCapComment(rounds int, lastRound []string) string {
	return fmt.Sprintf("The pre-commit review and the smoke did not pass in %d rounds, the cap. `%s` applied: a human takes over. The last round said:\n\n- %s",
		rounds, LabelFailed, strings.Join(lastRound, "\n- "))
}

// ReviseCapReply is the reply in each review thread still open at the
// round cap: the human's answer to it is the thread's ruling.
func ReviseCapReply(rounds int) string {
	return fmt.Sprintf("Still open after %d round(s) of fixes: parked for a human. Reply here to rule it.", rounds)
}

// ReviseCapComment is the comment on an Agent PR parked at the round cap,
// with threads still open.
func ReviseCapComment(threads, rounds int, now time.Time) string {
	return fmt.Sprintf("%d review thread(s) still open after %d round(s) of fixes at %s. `%s` applied: fixes still failing at the round cap. Re-apply `%s` to retry.",
		threads, rounds, now.UTC().Format(time.RFC3339), LabelReviseFailed, LabelRevise)
}

// ReviseGiveUpComment is the comment on an Agent PR whose ticket used its
// attempts answering the PR's threads.
func ReviseGiveUpComment(why error) string {
	return fmt.Sprintf("The review threads could not be answered (%v). `%s` applied: a human takes over. Re-apply `%s` to retry.", why, LabelReviseFailed, LabelRevise)
}

// RulingGiveUpComment is the comment on an Agent PR whose ticket used its
// attempts applying the human's Ruling.
func RulingGiveUpComment(why error) string {
	return fmt.Sprintf("The Ruling could not be applied (%v). `%s` taken off: a human takes over.", why, LabelRuling)
}

// ChecksFailedComment is the comment on the ticket of an Agent PR parked
// with CI still red when no fix round remained.
func ChecksFailedComment(rounds, pr int) string {
	return fmt.Sprintf("CI on PR #%d is still failing after %d fix round(s). Marked draft and labelled `%s`: a human takes over.", pr, rounds, LabelChecksFailed)
}

// ChecksGiveUpComment is the comment on the ticket of an Agent PR whose CI
// fixes used the ticket's attempts.
func ChecksGiveUpComment(pr int, why error) string {
	return fmt.Sprintf("CI on PR #%d could not be fixed (%v). Marked draft and labelled `%s`: a human takes over.", pr, why, LabelChecksFailed)
}
