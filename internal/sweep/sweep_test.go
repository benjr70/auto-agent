package sweep

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/step"
)

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// daysAgo is a Fire id stamped that many days before now, by process pid.
func daysAgo(days int, pid string) string {
	return now.AddDate(0, 0, -days).Format("20060102T150405Z") + "-" + pid
}

func sweepOf(stateDir string) Sweep {
	return Sweep{StateDir: stateDir, Now: func() time.Time { return now }}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRecordsPastTheirWindow(t *testing.T) {
	t.Run("of records at 29, 30 and 31 days only the 31-day one goes", func(t *testing.T) {
		state := t.TempDir()
		j := journal.Journal{StateDir: state}
		for _, days := range []int{29, 30, 31} {
			if err := j.Write(journal.Record{FireID: daysAgo(days, "7"), Repo: "acme/widgets"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := sweepOf(state).Run(); err != nil {
			t.Fatal(err)
		}
		for days, want := range map[int]bool{29: true, 30: true, 31: false} {
			if got := exists(j.Path(daysAgo(days, "7"))); got != want {
				t.Errorf("the %d-day record exists = %v, want %v", days, got, want)
			}
		}
	})
	t.Run("a paused ticket's record at 40 days is kept, and the Journal still finds its session", func(t *testing.T) {
		state := t.TempDir()
		j := journal.Journal{StateDir: state}
		issue, other := 20, 21
		ran := func(name string) []journal.Step { return []journal.Step{{Name: name, Outcome: step.Exhausted}} }
		records := []journal.Record{
			// An older pause of the same ticket: the newer one is the one
			// the Journal finds.
			{FireID: daysAgo(45, "7"), Repo: "acme/widgets", Issue: &issue, ResumeSession: "s-old", Steps: ran("implement")},
			{FireID: daysAgo(40, "7"), Repo: "acme/widgets", Issue: &issue, ResumeSession: "s-1", Steps: ran("implement")},
			// A Fire the gate stopped before its call decides nothing.
			{FireID: daysAgo(35, "7"), Repo: "acme/widgets", Issue: &issue, Steps: []journal.Step{{Name: "implement"}}},
			// A ticket whose last call left nothing to resume.
			{FireID: daysAgo(38, "7"), Repo: "acme/widgets", Issue: &other, ResumeSession: "s-2", Steps: ran("implement")},
			{FireID: daysAgo(33, "7"), Repo: "acme/widgets", Issue: &other, Steps: []journal.Step{{Name: "implement", Outcome: step.OK}}},
		}
		for _, r := range records {
			j.Write(r)
		}
		if err := sweepOf(state).Run(); err != nil {
			t.Fatal(err)
		}
		for i, want := range []bool{false, true, false, false, false} {
			if got := exists(j.Path(records[i].FireID)); got != want {
				t.Errorf("record %s exists = %v, want %v", records[i].FireID, got, want)
			}
		}
		if got := j.Session("acme/widgets", 20, "implement"); got != "s-1" {
			t.Errorf("Session = %q, want s-1", got)
		}
	})
	t.Run("a State dir with no Fire yet sweeps clean", func(t *testing.T) {
		if err := sweepOf(filepath.Join(t.TempDir(), "state")).Run(); err != nil {
			t.Error(err)
		}
	})
}

func TestOrphans(t *testing.T) {
	t.Run("of the half-written records beside a live Fire, only the killed Fire's goes", func(t *testing.T) {
		state := t.TempDir()
		j := journal.Journal{StateDir: state}
		os.MkdirAll(j.Dir(), 0o755)
		// This process is the live Fire; no process has a pid past the
		// kernel's limit, so that Fire was killed.
		live := j.Path(journal.NewFireID(now)) + ".tmp"
		orphan := j.Path(daysAgo(0, "2147483646")) + ".tmp"
		for _, path := range []string{live, orphan} {
			os.WriteFile(path, []byte(`{"fireId":`), 0o644)
		}
		if err := sweepOf(state).Run(); err != nil {
			t.Fatal(err)
		}
		if !exists(live) || exists(orphan) {
			t.Errorf("the live Fire's .tmp exists = %v, the orphan's = %v", exists(live), exists(orphan))
		}
	})
}

func TestTranscripts(t *testing.T) {
	t.Run("transcripts at 31 days go, at 29 days stay, and a paused ticket's session stays", func(t *testing.T) {
		state, transcripts := t.TempDir(), t.TempDir()
		issue := 20
		journal.Journal{StateDir: state}.Write(journal.Record{
			FireID: daysAgo(40, "7"), Repo: "acme/widgets", Issue: &issue, ResumeSession: "paused",
			Steps: []journal.Step{{Name: "implement", Outcome: step.Exhausted}},
		})
		touch := func(name string, days int) string {
			path := filepath.Join(transcripts, name)
			if filepath.Ext(name) == ".jsonl" {
				os.WriteFile(path, []byte("{}\n"), 0o644)
			} else {
				os.MkdirAll(path, 0o755)
			}
			at := now.AddDate(0, 0, -days)
			os.Chtimes(path, at, at)
			return path
		}
		gone := []string{touch("old.jsonl", 31), touch("old", 31)}
		stay := []string{touch("new.jsonl", 29), touch("paused.jsonl", 40), touch("paused", 40), touch("memory", 60)}
		s := sweepOf(state)
		s.Transcripts = transcripts
		if err := s.Run(); err != nil {
			t.Fatal(err)
		}
		for _, path := range gone {
			if exists(path) {
				t.Errorf("%s was kept", filepath.Base(path))
			}
		}
		for _, path := range stay {
			if !exists(path) {
				t.Errorf("%s was removed", filepath.Base(path))
			}
		}
	})
	t.Run("the transcripts of a clone Claude never ran in sweep clean", func(t *testing.T) {
		s := sweepOf(t.TempDir())
		s.Transcripts = filepath.Join(t.TempDir(), "none")
		if err := s.Run(); err != nil {
			t.Error(err)
		}
	})
	t.Run("a transcripts directory that cannot be listed is the Sweep's error", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		os.WriteFile(blocker, nil, 0o644)
		s := sweepOf(t.TempDir())
		s.Transcripts = blocker
		if err := s.Run(); err == nil {
			t.Error("Run succeeded")
		}
	})
}

func TestTranscriptsDir(t *testing.T) {
	got := TranscriptsDir("/home/u/.claude", "/home/u/.local/state/auto-agent/acme/widgets/clone")
	if want := "/home/u/.claude/projects/-home-u--local-state-auto-agent-acme-widgets-clone"; got != want {
		t.Errorf("TranscriptsDir = %q, want %q", got, want)
	}
}

func TestRateLimits(t *testing.T) {
	t.Run("rate-limit events at 31 days go, at 29 days stay, and a line that is not one stays", func(t *testing.T) {
		state := t.TempDir()
		event := func(days int) string {
			return `{"observedAt":"` + now.AddDate(0, 0, -days).Format("2006-01-02T15:04:05Z") + `","fireId":"f"}`
		}
		log := filepath.Join(state, "rate-limits.jsonl")
		os.WriteFile(log, []byte(event(31)+"\n"+event(29)+"\nnot json\n"+event(30)+"\n"), 0o644)
		if err := sweepOf(state).Run(); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(log)
		if want := event(29) + "\nnot json\n" + event(30) + "\n"; string(data) != want {
			t.Errorf("the log is\n%s\nwant\n%s", data, want)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(state, "*.tmp")); len(leftovers) != 0 {
			t.Errorf("left %v", leftovers)
		}
	})
}
