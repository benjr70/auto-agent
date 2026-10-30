package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/sweep"
)

// The Sweep through the whole-Fire seam: what the State dir holds after a
// Fire, or a run of the Daemon.
func TestSweep(t *testing.T) {
	// old is a Fire record of acme/widgets, stamped that long before the
	// fixture's clock, as another process left it.
	old := func(f *fireFixture, age time.Duration) string {
		id := fireClock.Add(-age).Format("20060102T150405Z") + "-7"
		journal.Journal{StateDir: f.state}.Write(journal.Record{FireID: id, Repo: "acme/widgets", StartedAt: fireClock.Add(-age)})
		return filepath.Join(f.state, "fires", id+".json")
	}
	idle := func(t *testing.T) *fireFixture {
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		f.queue = []githubtest.Reply{queueReply()}
		return f
	}

	t.Run("a Fire sweeps at its start: a record at 31 days goes, at 29 days stays", func(t *testing.T) {
		f := idle(t)
		gone, stays := old(f, 31*24*time.Hour), old(f, 29*24*time.Hour)
		f.mustFire(0)
		if _, err := os.Stat(gone); err == nil {
			t.Error("the 31-day record was kept")
		}
		if _, err := os.Stat(stays); err != nil {
			t.Error("the 29-day record was removed")
		}
	})
	t.Run("the Daemon sweeps as it starts", func(t *testing.T) {
		f := idle(t)
		gone := old(f, 31*24*time.Hour)
		if code := f.daemon(1); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if _, err := os.Stat(gone); err == nil {
			t.Error("the 31-day record was kept")
		}
	})
	t.Run("a Sweep that fails is a warning on the Fire record, and the Fire runs on", func(t *testing.T) {
		f := idle(t)
		// The clone's transcripts directory is a file: it cannot be listed.
		claudeDir := t.TempDir()
		f.environ["CLAUDE_CONFIG_DIR"] = claudeDir
		transcripts := sweep.TranscriptsDir(claudeDir, filepath.Join(f.state, "clone"))
		os.MkdirAll(filepath.Dir(transcripts), 0o755)
		os.WriteFile(transcripts, nil, 0o644)
		f.mustFire(0)
		record := f.record()
		warnings, _ := record["warnings"].([]any)
		if record["error"] != "" || record["kind"] != "idle" || len(warnings) != 1 || !strings.HasPrefix(warnings[0].(string), "the Sweep: listing the transcripts") {
			t.Errorf("record = %v", record)
		}
	})
	t.Run("an idle Fire whose Sweep failed keeps its own record: the next idle Fire does not erase the warning", func(t *testing.T) {
		f := idle(t)
		claudeDir := t.TempDir()
		f.environ["CLAUDE_CONFIG_DIR"] = claudeDir
		transcripts := sweep.TranscriptsDir(claudeDir, filepath.Join(f.state, "clone"))
		os.MkdirAll(filepath.Dir(transcripts), 0o755)
		os.WriteFile(transcripts, nil, 0o644)
		f.mustFire(0)
		os.Remove(transcripts)
		f.mustFire(0)
		warned := 0
		for _, record := range f.records() {
			if warnings, _ := record["warnings"].([]any); len(warnings) == 1 && strings.HasPrefix(warnings[0].(string), "the Sweep: ") {
				warned++
			}
		}
		if warned != 1 {
			t.Errorf("records = %v, want the Sweep's warning on one", f.records())
		}
	})
	t.Run("the transcripts of the clone's Steps past the window go", func(t *testing.T) {
		f := idle(t)
		transcripts := sweep.TranscriptsDir(filepath.Join(f.environ["HOME"], ".claude"), filepath.Join(f.state, "clone"))
		os.MkdirAll(transcripts, 0o755)
		gone, stays := filepath.Join(transcripts, "a.jsonl"), filepath.Join(transcripts, "b.jsonl")
		for path, age := range map[string]time.Duration{gone: 31 * 24 * time.Hour, stays: 29 * 24 * time.Hour} {
			os.WriteFile(path, nil, 0o644)
			os.Chtimes(path, fireClock.Add(-age), fireClock.Add(-age))
		}
		f.mustFire(0)
		if _, err := os.Stat(gone); err == nil {
			t.Error("the 31-day transcript was kept")
		}
		if _, err := os.Stat(stays); err != nil {
			t.Error("the 29-day transcript was removed")
		}
	})
	t.Run("100 idle passes of the Daemon leave one record, counting 100", func(t *testing.T) {
		f := idle(t)
		if code := f.daemon(100); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if logged := f.logged(); len(logged) != 100 {
			t.Errorf("%d Fires logged, want 100", len(logged))
		}
		files := f.recordFiles()
		if len(files) != 1 {
			t.Fatalf("records = %v, want one", files)
		}
		idle, _ := f.record()["idle"].(map[string]any)
		if idle["fires"] != 100.0 {
			t.Errorf("the run = %v", f.record())
		}
	})
}
