package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
)

var started = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)

// Ported from lib/fire-record.test.sh: test_write_and_path.
func TestWriteAndPath(t *testing.T) {
	t.Run("the path is <state-dir>/fires/<fire-id>.json", func(t *testing.T) {
		if got := (Journal{StateDir: "/s"}).Path("f1"); got != "/s/fires/f1.json" {
			t.Errorf("Path = %q", got)
		}
	})
	t.Run("write creates the directory and stores the record", func(t *testing.T) {
		j := Journal{StateDir: filepath.Join(t.TempDir(), "state")}
		issue := 7
		if err := j.Write(Record{FireID: "f1", Repo: "acme/widgets", StartedAt: started, Phase: "implement", Issue: &issue}); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		data, _ := os.ReadFile(j.Path("f1"))
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got["fireId"] != "f1" || got["issue"] != 7.0 || got["startedAt"] != "2026-10-12T09:30:00Z" {
			t.Errorf("record = %s", data)
		}
	})
	t.Run("a Fire in flight has a null end and exit, and its lists are never null", func(t *testing.T) {
		j := Journal{StateDir: t.TempDir()}
		j.Write(Record{FireID: "f1", StartedAt: started})
		data, _ := os.ReadFile(j.Path("f1"))
		for _, want := range []string{`"endedAt": null`, `"exit": null`, `"issue": null`, `"pr": null`, `"steps": []`, `"warnings": []`} {
			if !strings.Contains(string(data), want) {
				t.Errorf("the record lacks %s:\n%s", want, data)
			}
		}
	})
	t.Run("a second write replaces the first and leaves no temporary file", func(t *testing.T) {
		j := Journal{StateDir: t.TempDir()}
		j.Write(Record{FireID: "f1", StartedAt: started, Phase: "preflight"})
		exit, ended := 0, started.Add(time.Minute)
		j.Write(Record{FireID: "f1", StartedAt: started, Phase: "pr", EndedAt: &ended, Exit: &exit})
		data, _ := os.ReadFile(j.Path("f1"))
		files, _ := os.ReadDir(filepath.Dir(j.Path("f1")))
		if !strings.Contains(string(data), `"phase": "pr"`) || !strings.Contains(string(data), `"exit": 0`) || len(files) != 1 {
			t.Errorf("record = %s, %d files", data, len(files))
		}
	})
	t.Run("a State dir that cannot be written is an error", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		os.WriteFile(blocker, nil, 0o644)
		if err := (Journal{StateDir: blocker}).Write(Record{FireID: "f1"}); err == nil {
			t.Error("Write succeeded under a file")
		}
	})
}

func TestStepOf(t *testing.T) {
	t.Run("the entry lists the Step with its Outcome, session id and usage", func(t *testing.T) {
		run := step.Run{
			Outcome: step.OK, SessionID: "s1", Model: "opus", NumTurns: 4, CostUSD: 0.5, ExitCode: 0, Elapsed: 1500 * time.Millisecond,
			RawUsage: json.RawMessage(`{"input_tokens":10}`), RateLimits: []json.RawMessage{json.RawMessage(`{"a":1}`)},
			PermissionDenials: []step.PermissionDenial{{ToolName: "Bash"}},
		}
		data, _ := json.Marshal(StepOf("implement", started, run))
		want := `{"name":"implement","startedAt":"2026-10-12T09:30:00Z","outcome":"OK","detail":"","sessionId":"s1","model":"opus","numTurns":4,"costUsd":0.5,"usage":{"input_tokens":10},"rateLimits":[{"a":1}],"permissionDenials":1,"exitCode":0,"elapsedMs":1500}`
		if string(data) != want {
			t.Errorf("entry =\n%s\nwant\n%s", data, want)
		}
	})
	t.Run("a call that never produced usage records null and an empty list", func(t *testing.T) {
		data, _ := json.Marshal(StepOf("implement", started, step.Run{Outcome: step.Failed, Detail: "no result message"}))
		if !strings.Contains(string(data), `"usage":null,"rateLimits":[]`) || !strings.Contains(string(data), `"outcome":"FAILED","detail":"no result message"`) {
			t.Errorf("entry = %s", data)
		}
	})
	t.Run("a Fire id names the second it started", func(t *testing.T) {
		if id := NewFireID(started); !strings.HasPrefix(id, "20261012T093000Z-") {
			t.Errorf("id = %q", id)
		}
	})
}
