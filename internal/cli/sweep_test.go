package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/pidfile"
	"github.com/benjr70/auto-agent/internal/sweep"
	"github.com/benjr70/auto-agent/internal/verify/providertest"
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
	t.Run("after a kill -9 mid-round, the Daemon's start removes the round's tmp directory and kills its app", func(t *testing.T) {
		f := idle(t)
		round := filepath.Join(f.state, "verify", "pr-55", fireClock.Format("20060102T150405Z")+"-r1")
		os.MkdirAll(filepath.Join(round, "tmp"), 0o755)
		os.MkdirAll(filepath.Join(round, "run"), 0o755)
		os.WriteFile(filepath.Join(round, "tmp", "compose.yml"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(round, "run", "desktop.log"), []byte("started\n"), 0o644)
		app := exec.Command("sleep", "60")
		app.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := app.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { app.Wait(); close(exited) }()
		t.Cleanup(func() { app.Process.Kill(); <-exited })
		if err := pidfile.Write(filepath.Join(round, "run", "desktop.pid"), app.Process.Pid); err != nil {
			t.Fatal(err)
		}
		if code := f.daemon(1); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("the app the killed round left is still running")
		}
		if _, err := os.Stat(filepath.Join(round, "tmp")); !os.IsNotExist(err) {
			t.Errorf("the round's tmp directory is still there: %v", err)
		}
		if _, err := os.Stat(filepath.Join(round, "run", "desktop.pid")); !os.IsNotExist(err) {
			t.Errorf("the app's pid file is still there: %v", err)
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
	t.Run("a Fire sweeps the evidence of a closed PR past its window, and keeps an open PR's", func(t *testing.T) {
		f := idle(t)
		f.gh.Handle("GET", pullsPath, githubtest.Text(200, `[{"number":55,"head":{"ref":"feat/issue-20","repo":{"full_name":"acme/widgets"}}}]`))
		closed, open := evidence(f, 12, 8*24*time.Hour), evidence(f, 55, 60*24*time.Hour)
		f.mustFire(0)
		if _, err := os.Stat(closed); err == nil {
			t.Error("the closed PR's 8-day round was kept")
		}
		if _, err := os.Stat(open); err != nil {
			t.Error("the open PR's 60-day round was removed")
		}
	})
	t.Run("evidence inside its window costs no GitHub call", func(t *testing.T) {
		f := idle(t)
		stays := evidence(f, 12, 6*24*time.Hour)
		f.mustFire(0)
		if _, err := os.Stat(stays); err != nil {
			t.Error("the 6-day round was removed")
		}
		listed := 0
		for _, r := range f.gh.Requests() {
			if r.Method == "GET" && r.Path == pullsPath {
				listed++
			}
		}
		if listed != 1 {
			t.Errorf("the open PRs were listed %d times, want once, by the Fire's own read", listed)
		}
	})
}

// evidence is a round's evidence directory of PR pr in the fixture's State
// dir, stamped that long before the fixture's clock.
func evidence(f *fireFixture, pr int, age time.Duration) string {
	dir := filepath.Join(f.state, "verify", "pr-"+strconv.Itoa(pr), fireClock.Add(-age).Format("20060102T150405Z")+"-r1")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "web-01-home.png"), []byte("png"), 0o644)
	return dir
}

// The Sweep's environment step: the Environment provider's down --all at
// the start of every Fire, before anything is picked. The provider is the
// source of what is up.
func TestSweepEnvironments(t *testing.T) {
	healthy := providertest.Healthy("WEB_URL=http://127.0.0.1:9/\n")
	// left is the fake provider holding one live environment: a round's,
	// whose Fire was killed with kill -9.
	left := func(f *fireFixture, s providertest.Script) {
		s.Live = []int{7}
		f.provider.Script(s)
	}
	warnings := func(record map[string]any) []string {
		var got []string
		for _, w := range record["warnings"].([]any) {
			got = append(got, w.(string))
		}
		return got
	}

	t.Run("a Daemon started after a killed round tears its environment down before any pick", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		left(f, healthy)
		f.answer(finished)
		if code := f.daemon(1); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		// The smoke's own down --all comes after the implement Step; the
		// Sweep's is the first call of all, before the pick.
		calls := f.provider.Calls()
		if len(calls) < 2 || calls[0] != "down --all" || calls[1] != "down --all" || len(f.claude.Calls()) == 0 {
			t.Errorf("calls = %q", calls)
		}
		if live := f.provider.Live(); len(live) != 0 {
			t.Errorf("live environments %v, want none", live)
		}
	})
	t.Run("an idle Fire sweeps the environments too, and stays an Idle Fire", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.allLabels()
		f.queue = []githubtest.Reply{queueReply()}
		left(f, healthy)
		f.mustFire(0)
		if calls := f.provider.Calls(); !reflect.DeepEqual(calls, []string{"down --all"}) {
			t.Errorf("calls = %q", calls)
		}
		if live := f.provider.Live(); len(live) != 0 {
			t.Errorf("live environments %v, want none", live)
		}
		if record := f.record(); record["kind"] != "idle" || len(warnings(record)) != 0 {
			t.Errorf("record = %v", record)
		}
	})
	t.Run("a down --all that fails is a warning on the Fire record and the Dashboard, and the next Sweep runs it again", func(t *testing.T) {
		f, answers, errs := idleDaemon(t, "127.0.0.1", "/api/status")
		failing := healthy
		failing.DownAll = []R{{Exit: 1, Stderr: "down: volume pr-7-data is in use\n"}, {}}
		left(f, failing)
		if code := f.daemon(2); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		want := "the Sweep: the Environment provider: down --all exited 1, want 0 (nothing it owns remains): down: volume pr-7-data is in use"
		records := f.records()
		var warned []string
		for _, record := range records {
			warned = append(warned, warnings(record)...)
		}
		if !reflect.DeepEqual(warned, []string{want}) {
			t.Errorf("warnings = %q, want only %q", warned, want)
		}
		// Retried by the second Fire's Sweep, which left nothing up.
		if calls := f.provider.Calls(); !reflect.DeepEqual(calls, []string{"down --all", "down --all"}) {
			t.Errorf("calls = %q", calls)
		}
		if live := f.provider.Live(); len(live) != 0 {
			t.Errorf("live environments %v, want none", live)
		}
		if len(errs) != 0 {
			t.Fatalf("the Dashboard did not answer: %v", errs)
		}
		var status struct {
			Fires struct {
				Items []struct{ Notes []string }
			}
		}
		body, _ := strings.CutPrefix(answers["/api/status"], "200 ")
		if err := json.Unmarshal([]byte(body), &status); err != nil || len(status.Fires.Items) == 0 || !reflect.DeepEqual(status.Fires.Items[0].Notes, []string{want}) {
			t.Errorf("GET /api/status = %s", answers["/api/status"])
		}
	})
	t.Run("a Target Project in the Bootstrap state has no provider to sweep", func(t *testing.T) {
		f := newFireFixture(t, bootstrapConfig)
		f.allLabels()
		f.queue = []githubtest.Reply{queueReply()}
		f.mustFire(0)
		if calls := f.provider.Calls(); len(calls) != 0 {
			t.Errorf("calls %q", calls)
		}
	})
}
