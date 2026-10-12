package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
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
	gate := budget.Verdict{AuthMode: "login", Sensor: budget.SensorEndpoint, State: budget.StateOK, ShouldFire: true}
	t.Run("a Step the Budget gate stopped has its verdict and no Outcome", func(t *testing.T) {
		data, _ := json.Marshal(NotRun("implement", started, budget.Verdict{Sensor: budget.SensorStreamEvents, ResetAt: "2026-10-12T10:30:00Z"}))
		if !strings.Contains(string(data), `"resetAt":"2026-10-12T10:30:00Z","shouldFire":false`) || !strings.Contains(string(data), `"outcome":""`) {
			t.Errorf("entry = %s", data)
		}
	})
	t.Run("the entry lists the Step with its Outcome, session id and usage", func(t *testing.T) {
		run := step.Run{
			Outcome: step.OK, SessionID: "s1", Model: "opus", NumTurns: 4, CostUSD: 0.5, ExitCode: 0, Elapsed: 1500 * time.Millisecond,
			RawUsage: json.RawMessage(`{"input_tokens":10}`), RateLimits: []json.RawMessage{json.RawMessage(`{"a":1}`)},
			PermissionDenials: []step.PermissionDenial{{ToolName: "Bash"}},
		}
		data, _ := json.Marshal(StepOf("implement", started, gate, run))
		want := `{"name":"implement","gate":{"authMode":"login","sensor":"usage-endpoint","state":"ok","remainPct":null,"resetAt":null,"shouldFire":true,"observedAt":"","limits":null,"warnings":null,"fireModel":null,"fireModelUntil":null},"startedAt":"2026-10-12T09:30:00Z","outcome":"OK","detail":"","sessionId":"s1","resume":"","model":"opus","numTurns":4,"costUsd":0.5,"usage":{"input_tokens":10},"rateLimits":[{"a":1}],"permissionDenials":1,"exitCode":0,"elapsedMs":1500}`
		if string(data) != want {
			t.Errorf("entry =\n%s\nwant\n%s", data, want)
		}
	})
	t.Run("a call that never produced usage records null and an empty list", func(t *testing.T) {
		data, _ := json.Marshal(StepOf("implement", started, gate, step.Run{Outcome: step.Failed, Detail: "no result message"}))
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

func TestSession(t *testing.T) {
	entry := func(name string, outcome step.Outcome, session string) Step {
		return Step{Name: name, Outcome: outcome, SessionID: session}
	}
	write := func(t *testing.T, j Journal, id string, issue int, steps ...Step) {
		t.Helper()
		if err := j.Write(Record{FireID: id, Repo: "acme/widgets", Issue: &issue, Steps: steps}); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		write func(t *testing.T, j Journal)
		want  string
	}{
		{"a Step the usage limit cut off is resumed by its session", func(t *testing.T, j Journal) {
			write(t, j, "20261012T090000Z-1", 7, entry("implement", step.Exhausted, "s1"))
		}, "s1"},
		{"the newest Fire for the ticket decides", func(t *testing.T, j Journal) {
			write(t, j, "20261012T090000Z-1", 7, entry("implement", step.Exhausted, "old"))
			write(t, j, "20261012T100000Z-1", 7, entry("implement", step.Exhausted, "new"))
		}, "new"},
		{"another ticket's Fire is not this one's", func(t *testing.T, j Journal) {
			write(t, j, "20261012T090000Z-1", 7, entry("implement", step.Exhausted, "s1"))
			write(t, j, "20261012T100000Z-1", 8, entry("implement", step.Exhausted, "other"))
		}, "s1"},
		{"a Fire the Budget gate stopped ran no call, so the one before it decides", func(t *testing.T, j Journal) {
			write(t, j, "20261012T090000Z-1", 7, entry("implement", step.Exhausted, "s1"))
			write(t, j, "20261012T100000Z-1", 7, entry("implement", "", ""))
		}, "s1"},
		{"the last call of the Fire decides, not an earlier one", func(t *testing.T, j Journal) {
			write(t, j, "20261012T090000Z-1", 7, entry("implement", step.Failed, ""), entry("implement", step.Exhausted, "s2"))
		}, "s2"},
		{"a Step that ended any other way has no session to resume", func(t *testing.T, j Journal) {
			write(t, j, "20261012T090000Z-1", 7, entry("implement", step.Exhausted, "s1"))
			write(t, j, "20261012T100000Z-1", 7, entry("implement", step.Failed, "s2"))
		}, ""},
		{"another Step's session is not this Step's", func(t *testing.T, j Journal) {
			write(t, j, "20261012T090000Z-1", 7, entry("fix", step.Exhausted, "s1"))
		}, ""},
		{"no record, or one that cannot be read, is no session", func(t *testing.T, j Journal) {
			os.MkdirAll(filepath.Dir(j.Path("x")), 0o755)
			os.WriteFile(j.Path("20261012T110000Z-1"), []byte("{half"), 0o644)
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := Journal{StateDir: t.TempDir()}
			tt.write(t, j)
			if got := j.Session("acme/widgets", 7, "implement"); got != tt.want {
				t.Errorf("Session = %q, want %q", got, tt.want)
			}
			if got := j.Session("acme/other", 7, "implement"); got != "" {
				t.Errorf("another Target Project's session = %q", got)
			}
		})
	}
}
