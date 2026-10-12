package budget

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The files the Usage sensor keeps in the State dir.
const (
	rateLimitsFile = "rate-limits.json"  // the last rate-limit event
	rateLimitsLog  = "rate-limits.jsonl" // every rate-limit event
	sensorFile     = "usage-sensor.json" // the sensor's own memory
)

// Where a Record's windows came from.
const (
	// SourceWindows: the event's unifiedWindows object, which is not
	// documented.
	SourceWindows = "windows"
	// SourceBinding: that object was absent, so the one binding window was
	// rebuilt from the documented top-level fields.
	SourceBinding = "binding"
)

// Record is one rate-limit event of a Step's stream, as the sensor keeps it.
type Record struct {
	ObservedAt string `json:"observedAt"`
	FireID     string `json:"fireId"`
	SessionID  Text   `json:"sessionId"`
	// Model is the model the call ran on: the unnamed per-model window is
	// that model's.
	Model         Text   `json:"model"`
	Status        Text   `json:"status"`
	RateLimitType Text   `json:"rateLimitType"`
	ResetsAt      *int64 `json:"resetsAt"`
	ResetsAtIso   Text   `json:"resetsAtIso"`
	// Utilization is the event's top-level figure, 0 to 1, when present.
	Utilization    *float64          `json:"utilization"`
	IsUsingOverage bool              `json:"isUsingOverage"`
	OverageStatus  Text              `json:"overageStatus"`
	Source         string            `json:"source"`
	Windows        map[string]Window `json:"windows"`
}

// Window is one limit window of a Record.
type Window struct {
	// UsedPct is 0 to 100, rounded.
	UsedPct     *float64 `json:"usedPct"`
	ResetsAt    *int64   `json:"resetsAt"`
	ResetsAtIso Text     `json:"resetsAtIso"`
}

// eventWindow is a window as the event carries it.
type eventWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *int64   `json:"resetsAt"`
}

func (w eventWindow) window() Window {
	out := Window{ResetsAt: w.ResetsAt}
	if w.Utilization != nil {
		pct := math.Round(*w.Utilization * 100)
		out.UsedPct = &pct
	}
	if w.ResetsAt != nil {
		out.ResetsAtIso = Text(iso(time.Unix(*w.ResetsAt, 0)))
	}
	return out
}

// RecordOf reads one stream line as a rate-limit event. ok is false for a
// line that is not one. Every field is optional: what an event lacks is null
// in the record, never a failure.
func RecordOf(line json.RawMessage, fireID, model string, observed time.Time) (record Record, ok bool) {
	var event struct {
		Type      string `json:"type"`
		SessionID Text   `json:"session_id"`
		Info      *struct {
			Status         Text            `json:"status"`
			RateLimitType  Text            `json:"rateLimitType"`
			ResetsAt       *int64          `json:"resetsAt"`
			Utilization    *float64        `json:"utilization"`
			IsUsingOverage bool            `json:"isUsingOverage"`
			OverageStatus  Text            `json:"overageStatus"`
			UnifiedWindows json.RawMessage `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &event) != nil || event.Type != "rate_limit_event" || event.Info == nil {
		return Record{}, false
	}
	info := event.Info
	record = Record{
		ObservedAt: iso(observed), FireID: fireID, SessionID: event.SessionID, Model: Text(model),
		Status: info.Status, RateLimitType: info.RateLimitType, ResetsAt: info.ResetsAt,
		Utilization: info.Utilization, IsUsingOverage: info.IsUsingOverage, OverageStatus: info.OverageStatus,
		Source: SourceWindows, Windows: map[string]Window{},
	}
	if info.ResetsAt != nil {
		record.ResetsAtIso = Text(iso(time.Unix(*info.ResetsAt, 0)))
	}
	// The windows object is not documented: whatever of it cannot be read
	// is left out, and the binding window below stands in for all of it.
	var windows map[string]json.RawMessage
	json.Unmarshal(info.UnifiedWindows, &windows)
	for name, raw := range windows {
		var w eventWindow
		if json.Unmarshal(raw, &w) == nil {
			record.Windows[name] = w.window()
		}
	}
	if len(record.Windows) == 0 {
		name := string(info.RateLimitType)
		if name == "" {
			name = "unknown"
		}
		record.Source = SourceBinding
		record.Windows[name] = eventWindow{Utilization: info.Utilization, ResetsAt: info.ResetsAt}.window()
	}
	return record, true
}

// windowNames lists the record's windows, the account's first.
func (r Record) windowNames() []string {
	names := make([]string, 0, len(r.Windows))
	for name := range r.Windows {
		names = append(names, name)
	}
	rank := func(name string) int {
		switch name {
		case "five_hour":
			return 0
		case "seven_day":
			return 1
		}
		return 2
	}
	sort.Slice(names, func(a, b int) bool {
		if rank(names[a]) != rank(names[b]) {
			return rank(names[a]) < rank(names[b])
		}
		return names[a] < names[b]
	})
	return names
}

// saveRecords appends every record to the log and keeps the last one as the
// seed of the next verdict. With no records it touches nothing.
func saveRecords(stateDir string, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(stateDir, rateLimitsLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := log.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(stateDir, rateLimitsFile), records[len(records)-1])
}

// writeJSON writes v aside and moves it into place, so a reader never sees
// half a file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readJSON reads path into v and reports whether it held one.
func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, v) == nil
}
