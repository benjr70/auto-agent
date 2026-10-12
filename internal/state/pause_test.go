package state

import (
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github"
)

// Ported from lib/pause-resume.test.sh, under its case names. Go's types
// make three of its cases unwritable: see the comments below.
func TestPauseResume(t *testing.T) {
	queue := []github.QueueIssue{labelIssue(20, "clean", "2026-02-01")}
	tests := []struct {
		name string
		// issue 0 is no paused issue.
		issue, count, limit int
		kind                Kind
		action              Action
		wantCount           int
	}{
		{"a paused issue below the cap resumes", 290, 1, 0, KindResume, ActionResume, 1},
		{"no paused issue picks new", 0, 0, 0, KindPick, ActionImplement, 0},
		{"reaching the cap fails the issue (default cap 3)", 290, 3, 0, KindPauseCap, ActionFail, 3},
		{"just below the cap still resumes", 290, 2, 0, KindResume, ActionResume, 2},
		{"the cap from the config (3rd arg) lowers the limit", 290, 2, 2, KindPauseCap, ActionFail, 2},
		{"the cap from the config (3rd arg) raises the limit", 290, 3, 5, KindResume, ActionResume, 3},
		// bash: a cap of 'lots'. A typed cap can only be out of range.
		{"an unreadable cap falls back to the default", 290, 3, -1, KindPauseCap, ActionFail, 3},
		// bash: an empty count and a count of 'not-a-number'. A typed count
		// that could not be read is zero or less.
		{"an empty pause count resumes fail-safe", 290, 0, 0, KindResume, ActionResume, 1},
		{"a non-numeric pause count resumes fail-safe", 290, -1, 0, KindResume, ActionResume, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := Facts{Login: login, Pick: labelsPick, Queue: queue, PauseCap: tt.limit}
			if tt.issue != 0 {
				facts.Paused = []Paused{{Issue: tt.issue, Pauses: tt.count}}
			}
			got := Derive(facts)
			if got.Kind != tt.kind || got.Next.Action != tt.action {
				t.Fatalf("Derive = %+v", got)
			}
			if tt.issue == 0 {
				if got.Paused != nil {
					t.Errorf("Paused = %+v", got.Paused)
				}
				return
			}
			if got.Next.Issue != tt.issue || got.Paused == nil || got.Paused.Pauses != tt.wantCount || got.Pick != nil {
				t.Errorf("Derive = %+v, paused %+v", got, got.Paused)
			}
		})
	}
	t.Run("no env override: the documented cap comes from the config alone (ADR 0002)", func(t *testing.T) {
		t.Setenv("PAUSE_RESUME_CAP", "2")
		if got := Derive(Facts{Paused: []Paused{{Issue: 290, Pauses: 2}}}); got.Next.Action != ActionResume {
			t.Errorf("Derive = %+v", got)
		}
	})
	t.Run("a paused ticket whose Agent PR is open only has its labels taken off", func(t *testing.T) {
		got := Derive(Facts{Paused: []Paused{{Issue: 290, Pauses: 3}}, AgentPRs: map[int]int{290: 55}})
		if got.Next.Action != ActionRelease || got.Next.Issue != 290 || !strings.Contains(got.Next.Reason, "#55") {
			t.Errorf("Derive = %+v", got)
		}
	})
	t.Run("paused work comes before a new pick, and the facts are not changed", func(t *testing.T) {
		facts := Facts{Login: login, Pick: labelsPick, Queue: queue, Paused: []Paused{{Issue: 290}, {Issue: 300, Pauses: 3}}}
		if got := Derive(facts); got.Next.Issue != 290 || facts.Paused[0].Pauses != 0 {
			t.Errorf("Derive = %+v, facts %+v", got, facts.Paused)
		}
	})
	t.Run("the resume Lane is handled", func(t *testing.T) {
		if got := Derive(Facts{}); strings.Contains(strings.Join(got.NotHandled, ","), "resume") {
			t.Errorf("NotHandled = %v", got.NotHandled)
		}
	})
}

func TestDeriveCrashed(t *testing.T) {
	t.Run("a held lock with no live Fire is a crash to recover from, before anything else", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: labelsPick, Held: []int{7, 9}, Paused: []Paused{{Issue: 3}}})
		if got.Kind != KindCrashed || got.InFlight != 2 || got.Next.Action != ActionRecover || got.Next.Issue != 7 {
			t.Errorf("Derive = %+v", got)
		}
	})
	t.Run("a crashed ticket whose Agent PR is open only has the lock released", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: labelsPick, Held: []int{7}, AgentPRs: map[int]int{7: 55}})
		if got.Kind != KindCrashed || got.Next.Action != ActionRelease || got.Next.Issue != 7 || !strings.Contains(got.Next.Reason, "#55") {
			t.Errorf("Derive = %+v", got)
		}
	})
	t.Run("a live Fire is in flight, whether it has taken the lock yet or not", func(t *testing.T) {
		for _, held := range [][]int{nil, {7}} {
			got := Derive(Facts{Login: login, Pick: labelsPick, Held: held, LiveFire: true, Queue: []github.QueueIssue{labelIssue(20, "clean", "2026-02-01")}})
			if got.Kind != KindInFlight || got.InFlight != len(held) || got.Next.Action != ActionNone {
				t.Errorf("held %v: Derive = %+v", held, got)
			}
		}
	})
	t.Run("an unreadable lock is never a crash", func(t *testing.T) {
		if got := Derive(Facts{LockUnreadable: true}); got.Kind != KindInFlight || got.Next.Action != ActionNone {
			t.Errorf("Derive = %+v", got)
		}
	})
}

func TestAttempts(t *testing.T) {
	t.Run("the marker round-trips through a comment", func(t *testing.T) {
		comment := Attempts{Step: "implement", Count: 2}.Comment("it timed out")
		got, ok := ParseAttempts(comment)
		if !ok || got != (Attempts{Step: "implement", Count: 2}) || !strings.HasPrefix(comment, "<!-- auto-agent:attempts step=implement count=2 -->\n") ||
			!strings.Contains(comment, "Attempt 2 of 3 at the implement Step failed: it timed out") || strings.Contains(comment, LabelFailed) {
			t.Errorf("comment = %q, parsed %+v %v", comment, got, ok)
		}
	})
	t.Run("a comment with no marker is not a count", func(t *testing.T) {
		for _, comment := range []string{"", "Attempt 2 of 3", "<!-- auto-agent:attempts step=implement -->", "<!-- auto-agent:attempts step=implement count=99999999999999999999 -->"} {
			if _, ok := ParseAttempts(comment); ok {
				t.Errorf("%q parsed as a marker", comment)
			}
		}
	})
	t.Run("three failed attempts at the same Step spend the ticket's attempts", func(t *testing.T) {
		var a Attempts
		for want := 1; want <= MaxAttempts; want++ {
			a = a.After("implement")
			if a.Count != want || a.Spent() != (want == MaxAttempts) {
				t.Fatalf("after %d: %+v", want, a)
			}
		}
		if !strings.Contains(a.Comment("x"), "`AFK:failed` applied") {
			t.Errorf("comment = %q", a.Comment("x"))
		}
	})
	t.Run("attempts at another Step do not carry over", func(t *testing.T) {
		if got := (Attempts{Step: "implement", Count: 2}).After("fix"); got != (Attempts{Step: "fix", Count: 1}) {
			t.Errorf("After = %+v", got)
		}
	})
	t.Run("a ticket a human took AFK:failed off starts its count again", func(t *testing.T) {
		if got := (Attempts{Step: "implement", Count: 3}).After("implement"); got.Count != 1 {
			t.Errorf("After = %+v", got)
		}
	})
}

func TestPauseComment(t *testing.T) {
	at := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	t.Run("the pause comment is the one the pauses are counted by", func(t *testing.T) {
		got := PauseComment(at, "2026-10-13T02:50:00Z")
		if got != "Fire paused at 2026-10-12T09:30:00Z: usage exhausted mid-Fire. Branch kept for resume. Budget resets at 2026-10-13T02:50:00Z." || !IsPause(got) {
			t.Errorf("comment = %q", got)
		}
		if got := PauseComment(at, ""); strings.Contains(got, "resets") || !IsPause(got) {
			t.Errorf("comment = %q", got)
		}
	})
	t.Run("the bash harness's pauses count; other comments do not", func(t *testing.T) {
		if !IsPause("Run paused at 2026-09-01T00:00:00Z: usage exhausted mid-run.") {
			t.Error("a Run paused comment is not counted")
		}
		for _, comment := range []string{"Fire paused at 2026-09-01T00:00:00Z: credential dead mid-Fire.", PauseCapComment(3), Attempts{Step: "implement", Count: 1}.Comment("usage exhausted")} {
			if IsPause(comment) {
				t.Errorf("%q counts as a pause", comment)
			}
		}
	})
}
