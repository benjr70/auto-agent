package budget

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
)

// Ported from lib/exhaustion-classifier.test.sh. The runner decides the
// status (its own suite holds those cases); what is ported here is what the
// classifier scraped for the Budget gate: the reset, the source, the limit
// and the login-expiry warning.

const rejectedEvent = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789449000,"rateLimitType":"five_hour","utilization":1},"session_id":"s1"}`

func TestReportOf(t *testing.T) {
	limited := func(text string, events ...string) step.Run {
		run := step.Run{Outcome: step.Exhausted, Output: text}
		for _, e := range events {
			run.RateLimits = append(run.RateLimits, json.RawMessage(e))
		}
		return run
	}
	tests := []struct {
		group, name string
		run         step.Run
		want        string // status, resetAt, source, limitType
	}{
		{"exit 0 is OK whatever the transcript says", "prose mentioning rate-limit on a finished Step: OK, no source",
			step.Run{Outcome: step.OK, Output: "docker manifest inspect fail for rate-limit/network reason"}, `["OK","",null,null]`},
		{"exit 0 is OK whatever the transcript says", "a rejected event cannot override a completed Step",
			step.Run{Outcome: step.OK, RateLimits: []json.RawMessage{json.RawMessage(rejectedEvent)}}, `["OK","",null,null]`},

		{"a rejected rate-limit record is the authoritative exhaustion signal (stream-events)", "EXHAUSTED from the event with its reset and window",
			limited("some unrelated stack trace", rejectedEvent), `["EXHAUSTED","2026-09-15T05:10:00Z","stream-events","five_hour"]`},
		{"a rejected rate-limit record is the authoritative exhaustion signal (stream-events)", "the event beats the string's scraped reset",
			limited("You've hit your session limit · resets 10:50pm (America/New_York)", rejectedEvent), `["EXHAUSTED","2026-09-15T05:10:00Z","stream-events","five_hour"]`},
		{"a rejected rate-limit record is the authoritative exhaustion signal (stream-events)", "a malformed event is ignored",
			limited("You've hit your session limit · resets 10:50pm (America/New_York)", "not json"), `["EXHAUSTED","2026-09-15T02:50:00.000Z","limit-strings","session"]`},

		// 10:50pm New York on 2026-09-14 (EDT) is 02:50Z on the 15th.
		{"the documented limit strings stay as the text-mode fallback (limit-strings)", "session limit with a wall-clock reset",
			limited("You've hit your session limit · resets 10:50pm (America/New_York)"), `["EXHAUSTED","2026-09-15T02:50:00.000Z","limit-strings","session"]`},
		{"the documented limit strings stay as the text-mode fallback (limit-strings)", "weekly limit names the weekly scope",
			limited("You've hit your weekly limit · resets Sep 18, 7pm (America/New_York)"), `["EXHAUSTED","2026-09-18T23:00:00.000Z","limit-strings","weekly"]`},
		{"the documented limit strings stay as the text-mode fallback (limit-strings)", "a per-model limit string names the model and scrapes the ISO reset",
			limited("You've hit your Fable limit · resets 2026-09-18T19:00:00Z"), `["EXHAUSTED","2026-09-18T19:00:00.000Z","limit-strings","fable"]`},
		{"the documented limit strings stay as the text-mode fallback (limit-strings)", "the pipe-delimited epoch form",
			limited("Claude AI usage limit reached|1789449000"), `["EXHAUSTED","2026-09-15T05:10:00.000Z","limit-strings",null]`},
		{"the documented limit strings stay as the text-mode fallback (limit-strings)", "Claude's own 429 rate_limit_error without a reset: EXHAUSTED, empty resetAt",
			limited(`API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"..."}}`), `["EXHAUSTED","","limit-strings",null]`},
		{"the documented limit strings stay as the text-mode fallback (limit-strings)", "a bare epoch on a line that says reset",
			limited("usage limit will reset at 1789449000"), `["EXHAUSTED","2026-09-15T05:10:00.000Z","limit-strings",null]`},

		{"credential death is AUTH_DEAD, never exhaustion, never a ticket failure", "the documented login-expiry result",
			step.Run{Outcome: step.AuthDead, Output: "Failed to authenticate: OAuth session expired and could not be refreshed"}, `["AUTH_DEAD","",null,null]`},
		{"a genuine failure is FAILED", "no signature: FAILED",
			step.Run{Outcome: step.Failed, Output: "Error: ENOENT no such file\nTraceback"}, `["FAILED","",null,null]`},
	}
	for _, tt := range tests {
		t.Run(tt.group+"/"+tt.name, func(t *testing.T) {
			got := ReportOf(tt.run, now)
			if have := show(got.Outcome, got.ResetAt, got.Source, got.LimitType); have != tt.want {
				t.Errorf("got %s, want %s", have, tt.want)
			}
			if got.ObservedAt != "2026-09-15T00:00:00Z" {
				t.Errorf("observedAt = %q", got.ObservedAt)
			}
		})
	}

	t.Run("a wall-clock reset is the next time the clock reads so: 12am and 12pm, with and without minutes or a zone", func(t *testing.T) {
		for line, want := range map[string]string{
			"session limit · resets 12am":                 "2026-09-16T00:00:00.000Z", // midnight UTC is now, so the next one
			"session limit · resets 12pm (Europe/Berlin)": "2026-09-15T10:00:00.000Z",
			"session limit · resets 1:05 AM":              "2026-09-15T01:05:00.000Z",
			"session limit · resets soon":                 "",
			"session limit · resets 13pm":                 "", // no clock shows it
			"session limit · resets 10:75pm":              "",
			"weekly limit · resets Sep 14, 7pm":           "2027-09-14T19:00:00.000Z", // that day has passed: next year's
			"weekly limit · resets September 18 at 7am":   "2026-09-18T07:00:00.000Z",
		} {
			if got := ReportOf(limited(line), now).ResetAt; got != want {
				t.Errorf("%q: resetAt = %q, want %q", line, got, want)
			}
		}
	})

	t.Run("a time the zone skips resolves to the later reading", func(t *testing.T) {
		// 2026-03-08 02:30 does not exist in New York: clocks go 02:00 -> 03:00.
		at := time.Date(2026, 3, 8, 5, 30, 0, 0, time.UTC)
		if got := ReportOf(limited("session limit · resets 2:30am (America/New_York)"), at).ResetAt; got != "2026-03-08T07:30:00.000Z" {
			t.Errorf("resetAt = %q", got)
		}
	})

	// Ported: test_expiry_warning.
	t.Run("the login-expiry notice rides along as a warning", func(t *testing.T) {
		got := ReportOf(step.Run{Outcome: step.OK, Output: "  Your login expires in 3 days. Run /login to renew.\ndone\n"}, now)
		if show(got.Outcome, got.Warnings) != `["OK",["Your login expires in 3 days. Run /login to renew."]]` {
			t.Errorf("outcome = %+v", got)
		}
		if got := ReportOf(step.Run{Outcome: step.OK, Output: "done\n"}, now); show(got.Warnings) != `[[]]` {
			t.Errorf("warnings = %v", got.Warnings)
		}
		noisy := strings.Repeat("your login will expire\n", 5) + "rate limit: login expired\n"
		if got := ReportOf(step.Run{Outcome: step.Failed, Output: noisy}, now); len(got.Warnings) != 3 {
			t.Errorf("warnings = %v, want the first three and no rate-limit line", got.Warnings)
		}
	})
}

// Ported from lib/rate-limits-tap.test.sh. The tap was a filter on the
// stream; here the runner hands over the events and Observe records them.

const (
	eventFull = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789449000,"rateLimitType":"five_hour","overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.24,"resetsAt":1789449000},"seven_day":{"utilization":0.06,"resetsAt":1789758000},"seven_day_overage_included":{"utilization":0.11,"resetsAt":1789758000}}},"uuid":"u1","session_id":"s1"}`
	eventBare = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789449000,"rateLimitType":"seven_day","utilization":0.97},"uuid":"u2","session_id":"s1"}`
)

func TestRateLimitRecords(t *testing.T) {
	events := func(lines ...string) step.Run {
		run := step.Run{Outcome: step.OK, Model: "claude-fable-5-1"}
		for _, l := range lines {
			run.RateLimits = append(run.RateLimits, json.RawMessage(l))
		}
		return run
	}
	lines := func(w *world) []string {
		data, _ := os.ReadFile(filepath.Join(w.sensor.StateDir, rateLimitsLog))
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	last := func(w *world) Record {
		var r Record
		readJSON(filepath.Join(w.sensor.StateDir, rateLimitsFile), &r)
		return r
	}
	t.Run("rate-limits.json is the last event, rate-limits.jsonl every event", func(t *testing.T) {
		w := newWorld(t)
		w.sensor.Observe("fire-9", events(eventFull, eventBare))
		if n := len(lines(w)); n != 2 {
			t.Errorf("jsonl has %d lines, want one per event", n)
		}
		if r := last(w); show(r.Status, r.FireID, r.SessionID, r.Model) != `["rejected","fire-9","s1","claude-fable-5-1"]` {
			t.Errorf("the last record = %+v", r)
		}
		var first Record
		json.Unmarshal([]byte(lines(w)[0]), &first)
		check(t, "a full event records every window as usedPct with ISO resets",
			show(first.Source, first.windowNames(), first.Windows[overageWindow].UsedPct, first.Windows["five_hour"].ResetsAtIso),
			`["windows",["five_hour","seven_day","seven_day_overage_included"],11,"2026-09-15T05:10:00Z"]`)
		w.sensor.Observe("fire-10", events(eventFull))
		if n := len(lines(w)); n != 3 || last(w).FireID != "fire-10" {
			t.Errorf("a later Fire did not append and replace: %d lines, last %s", n, last(w).FireID)
		}
	})
	t.Run("an event without unifiedWindows degrades to the binding window", func(t *testing.T) {
		r, ok := RecordOf(json.RawMessage(eventBare), "f1", "", now)
		got, _ := json.Marshal(map[string]any{"source": r.Source, "status": r.Status, "rateLimitType": r.RateLimitType, "resetsAt": r.ResetsAt, "utilization": r.Utilization, "model": r.Model, "windows": r.Windows})
		want := `{"model":null,"rateLimitType":"seven_day","resetsAt":1789449000,"source":"binding","status":"rejected","utilization":0.97,"windows":{"seven_day":{"usedPct":97,"resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z"}}}`
		if !ok || string(got) != want {
			t.Errorf("record = %s", got)
		}
		r, ok = RecordOf(json.RawMessage(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"session_id":"s"}`), "f1", "", now)
		got, _ = json.Marshal(r.Windows)
		if !ok || r.Source != SourceBinding || string(got) != `{"unknown":{"usedPct":null,"resetsAt":null,"resetsAtIso":null}}` {
			t.Errorf("an event with only a status: %s", got)
		}
	})
	t.Run("a missing undocumented field degrades to the documented ones, with a warning in the verdict", func(t *testing.T) {
		w := newWorld(t).setupToken()
		w.sensor.Observe("f1", events(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789449000,"rateLimitType":"five_hour","utilization":0.4}}`))
		v := w.must()
		check(t, "no unifiedWindows: the binding window only", show(v.Sensor, v.RemainPct, v.ShouldFire, warns(v, "no unifiedWindows")), `["stream-events",60,true,true]`)
		w.sensor.Observe("f2", events(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789449000,"rateLimitType":"five_hour"}}`))
		v = w.must()
		check(t, "no utilization either: no number, the Step still goes", show(v.Sensor, v.RemainPct, v.ShouldFire, warns(v, "no utilization for its five_hour window")), `["stream-events",null,true,true]`)
		w.sensor.Observe("f0", events(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789449000,"utilization":1}}`))
		v = w.must()
		check(t, "no rateLimitType on a rejected event: held as the session limit", show(v.Sensor, v.ShouldFire, v.ResetAt, warns(v, "no rateLimitType")), `["stream-events",false,"2026-09-15T05:10:00Z",true]`)
		w.sensor.Observe("f0", events(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789449000,"utilization":0.95}}`))
		v = w.must()
		check(t, "no rateLimitType on an allowed event: its number still gates", show(v.Sensor, v.RemainPct, v.ShouldFire, warns(v, "no rateLimitType")), `["stream-events",5,false,true]`)
		w.sensor.Observe("f0", events(`{"type":"rate_limit_event","session_id":5,"rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1789449000.5,"isUsingOverage":"yes","overageStatus":7}}`))
		v = w.must()
		check(t, "fields of the wrong type never cost the event: still held", show(v.Sensor, v.ShouldFire, v.ResetAt), `["stream-events",false,"2026-09-15T05:10:00Z"]`)
		w.sensor.Observe("f3", events(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789449000,"rateLimitType":"five_hour","utilization":0.4,"unifiedWindows":"nonsense"}}`))
		v = w.must()
		check(t, "a windows object of the wrong type is the same as none", show(v.Sensor, v.RemainPct, warns(v, "no unifiedWindows")), `["stream-events",60,true]`)
		w.sensor.Observe("f4", events(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.3,"resetsAt":1789449000},"seven_day":"junk"}}}`))
		v = w.must()
		check(t, "one unreadable window does not lose the others", show(v.Sensor, v.RemainPct, len(v.Limits)), `["stream-events",70,1]`)
	})
	t.Run("rate_limits_record returns 1 for a line that is not a rate_limit_event", func(t *testing.T) {
		for _, line := range []string{`{"type":"system","subtype":"init","session_id":"s1"}`, `{"type":"rate_limit_event"}`, `{"type":"rate_limit_event" broken`} {
			if _, ok := RecordOf(json.RawMessage(line), "f1", "", now); ok {
				t.Errorf("%s was read as an event", line)
			}
		}
	})
	t.Run("a stream without events leaves the State dir untouched", func(t *testing.T) {
		w := newWorld(t)
		if err := saveRecords(w.sensor.StateDir, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(w.sensor.StateDir); !os.IsNotExist(err) {
			t.Error("the State dir was created")
		}
	})
	t.Run("a malformed event line is forwarded and skipped", func(t *testing.T) {
		w := newWorld(t)
		w.sensor.Observe("f1", events(`{"type":"rate_limit_event" broken`, eventFull))
		if n := len(lines(w)); n != 1 {
			t.Errorf("%d events recorded, want 1", n)
		}
	})
}
