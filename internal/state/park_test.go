package state

import (
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github"
)

func TestParked(t *testing.T) {
	queue := []github.QueueIssue{labelIssue(20, "clean", "2026-02-01")}

	t.Run("an open park issue parks the Daemon: no ticket is worked, the credential is probed", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: labelsPick, Queue: queue, Parked: 77, Held: []int{20}, Paused: []Paused{{Issue: 21, Pauses: 1}}})
		if got.Kind != KindParked || got.Parked != 77 || got.Next.Action != ActionProbe || got.Next.Issue != 0 || got.Pick != nil || got.Paused != nil {
			t.Errorf("Derive = %+v", got)
		}
	})
	t.Run("a live Fire is in flight, parked or not: it is the one probing", func(t *testing.T) {
		if got := Derive(Facts{Parked: 77, LiveFire: true}); got.Kind != KindInFlight || got.Next.Action != ActionNone {
			t.Errorf("Derive = %+v", got)
		}
	})
	t.Run("with no park issue the Daemon is not parked", func(t *testing.T) {
		if got := Derive(Facts{Login: login, Pick: labelsPick, Queue: queue}); got.Kind != KindPick || got.Parked != 0 {
			t.Errorf("Derive = %+v", got)
		}
	})
}

func TestParkIssue(t *testing.T) {
	at := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	issue := ParkIssue("host-1", at, "authentication_failed in Fire f1", time.Hour)
	if issue.Title != "Daemon parked on host-1: Claude credential dead" || strings.Join(issue.Labels, ",") != LabelNeedsHuman {
		t.Errorf("issue = %+v", issue)
	}
	for _, want := range []string{"The Daemon on `host-1` parked at 2026-09-15T00:00:00Z: authentication_failed in Fire f1.", "/login", "CLAUDE_CODE_OAUTH_TOKEN", "every 1h", "closes this issue itself"} {
		if !strings.Contains(issue.Body, want) {
			t.Errorf("the body lacks %q:\n%s", want, issue.Body)
		}
	}
	if !strings.HasPrefix(issue.Body, "<!-- auto-agent:parked -->\n") || !IsPark(issue.Body) || IsPark("unrelated needs-human issue") {
		t.Errorf("the marker: %q", issue.Body)
	}
	if got := ParkAgainComment("host-1", at, "401 from the usage endpoint", 90*time.Minute); !strings.HasPrefix(got, "Parked again at 2026-09-15T00:00:00Z on host-1: 401 from the usage endpoint.") || !strings.Contains(got, "every 1h30m") {
		t.Errorf("ParkAgainComment = %q", got)
	}
	if got := UnparkComment(at); got != "Un-parked at 2026-09-15T00:00:00Z: the credential probe passes again. Fires resume." {
		t.Errorf("UnparkComment = %q", got)
	}
	// A pause on a dead credential is not the ticket's: it is never counted
	// against the pause cap.
	if got := CredentialPauseComment(at); got != "Fire paused at 2026-09-15T00:00:00Z: credential dead mid-Fire. Branch kept for resume." || IsPause(got) {
		t.Errorf("CredentialPauseComment = %q", got)
	}
}
