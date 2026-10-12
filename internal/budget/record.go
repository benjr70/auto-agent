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
	Utilization number `json:"utilization"`
	ResetsAt    epoch  `json:"resetsAt"`
}

func (w eventWindow) window() Window {
	out := Window{ResetsAt: w.ResetsAt.value}
	if w.Utilization.value != nil {
		pct := math.Round(*w.Utilization.value * 100)
		out.UsedPct = &pct
	}
	if out.ResetsAt != nil {
		out.ResetsAtIso = Text(iso(time.Unix(*out.ResetsAt, 0)))
	}
	return out
}

// UnmarshalJSON never fails: a window the sensor's own file holds in a shape
// it cannot read is an empty window, not an unreadable record.
func (w *Window) UnmarshalJSON(data []byte) error {
	var raw struct {
		UsedPct     number `json:"usedPct"`
		ResetsAt    epoch  `json:"resetsAt"`
		ResetsAtIso Text   `json:"resetsAtIso"`
	}
	json.Unmarshal(data, &raw)
	*w = Window{UsedPct: raw.UsedPct.value, ResetsAt: raw.ResetsAt.value, ResetsAtIso: raw.ResetsAtIso}
	return nil
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
			ResetsAt       epoch           `json:"resetsAt"`
			Utilization    number          `json:"utilization"`
			IsUsingOverage flag            `json:"isUsingOverage"`
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
		Status: info.Status, RateLimitType: info.RateLimitType, ResetsAt: info.ResetsAt.value,
		Utilization: info.Utilization.value, IsUsingOverage: bool(info.IsUsingOverage), OverageStatus: info.OverageStatus,
		Source: SourceWindows, Windows: map[string]Window{},
	}
	if record.ResetsAt != nil {
		record.ResetsAtIso = Text(iso(time.Unix(*record.ResetsAt, 0)))
	}
	// The windows object is not documented: whatever of it cannot be read
	// is left out, and the binding window below stands in for all of it.
	var windows map[string]json.RawMessage
	json.Unmarshal(info.UnifiedWindows, &windows)
	for name, raw := range windows {
		var w eventWindow
		if json.Unmarshal(raw, &w) == nil && (w.Utilization.value != nil || w.ResetsAt.value != nil) {
			record.Windows[name] = w.window()
		}
	}
	if len(record.Windows) == 0 {
		name := string(info.RateLimitType)
		if name == "" {
			name = unknownWindow
		}
		record.Source = SourceBinding
		record.Windows[name] = eventWindow{Utilization: info.Utilization, ResetsAt: info.ResetsAt}.window()
	}
	return record, true
}

// The decoders below never fail: a field of an unexpected type reads as
// absent, so that it cannot cost the rest of an event.

// number is a JSON number, or nothing.
type number struct{ value *float64 }

func (n *number) UnmarshalJSON(data []byte) error {
	var f *float64
	if json.Unmarshal(data, &f) == nil {
		n.value = f
	}
	return nil
}

// epoch is a JSON number read as whole seconds, or nothing.
type epoch struct{ value *int64 }

func (e *epoch) UnmarshalJSON(data []byte) error {
	var f *float64
	if json.Unmarshal(data, &f) == nil && f != nil {
		whole := int64(*f)
		e.value = &whole
	}
	return nil
}

// flag is a JSON boolean, or false.
type flag bool

func (f *flag) UnmarshalJSON(data []byte) error {
	var b bool
	json.Unmarshal(data, &b)
	*f = flag(b)
	return nil
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
