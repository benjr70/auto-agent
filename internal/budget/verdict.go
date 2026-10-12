// Package budget is the Budget gate: before every Step the Daemon asks it
// whether it may go. The Usage sensor reads the account's real limits where
// the auth mode allows it and never guesses; the answer is a Gate verdict
// (ADR 0008 and its addendum).
package budget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
)

// The auth modes.
const (
	ModeLogin      = "login"
	ModeSetupToken = "setup-token"
	ModeAPIKey     = "api-key"
)

// The sensors a verdict can come from.
const (
	SensorEndpoint     = "usage-endpoint"
	SensorStreamEvents = "stream-events"
	SensorLimitStrings = "limit-strings"
	SensorSpend        = "spend"
	SensorNone         = "none"
)

// The states of a verdict.
const (
	StateOK          = "ok"
	StateStale       = "stale"
	StateUnavailable = "unavailable"
	StateAuthDead    = "auth-dead"
)

// The scopes of the two account-wide limits. Every other scope is a model
// family, and a per-model limit never gates: it changes the model.
const (
	ScopeSession = "session" // the 5-hour window
	ScopeWeekly  = "weekly"  // the 7-day window
)

// Text is a string that is null in JSON when empty.
type Text string

func (t Text) MarshalJSON() ([]byte, error) {
	if t == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(t))
}

// UnmarshalJSON never fails: anything that is not a string reads as empty.
// The payloads this decodes are not all documented, and one field of an
// unexpected type must not cost the rest.
func (t *Text) UnmarshalJSON(data []byte) error {
	var s string
	*t = ""
	if json.Unmarshal(data, &s) == nil {
		*t = Text(s)
	}
	return nil
}

// Verdict is the Gate verdict: one object for the gate, the Fire record and
// the Dashboard.
type Verdict struct {
	AuthMode Text   `json:"authMode"`
	Sensor   string `json:"sensor"`
	State    string `json:"state"`
	// RemainPct is what is left of the binding account limit, 0 to 100;
	// null when no sensor has spoken.
	RemainPct *float64 `json:"remainPct"`
	// ResetAt is when the binding limit frees.
	ResetAt    Text `json:"resetAt"`
	ShouldFire bool `json:"shouldFire"`
	// ObservedAt is when the sensor's number was read.
	ObservedAt string   `json:"observedAt"`
	Limits     []Limit  `json:"limits"`
	Warnings   []string `json:"warnings"`
	// FireModel is the Model policy's switch; null when the primary runs.
	FireModel Text `json:"fireModel"`
	// FireModelUntil is the reset the switch lasts until.
	FireModelUntil Text `json:"fireModelUntil"`
}

// Limit is one limit of the account, as the verdict lists it.
type Limit struct {
	// Scope is "session", "weekly" or a model family ("opus").
	Scope string `json:"scope"`
	// Utilization is 0 to 100; null when the sensor gave none.
	Utilization *float64 `json:"utilization"`
	ResetsAt    Text     `json:"resetsAt"`
	// resetsEpoch is when the limit resets, when the sensor said so as a
	// number: a limit past it is expired. Zero means it never expires.
	resetsEpoch int64
}

// Policy is what the Host config says about gating and the Model policy.
type Policy struct {
	// MinPct is the remaining percent at or above which a Step may start.
	MinPct float64
	// StaleMax is how long the usage endpoint's last good verdict is held
	// when the endpoint fails.
	StaleMax time.Duration
	// Primary is the model the Steps run on; Fallback the one a spent
	// primary switches to (empty: never switch).
	Primary, Fallback string
	// SwitchPct is the per-model utilization that switches.
	SwitchPct float64
}

var familyWord = regexp.MustCompile(`^[a-z]+$`)

// ModelFamily is the scope a per-model limit carries: "claude-fable-5-1" and
// "Fable" are both "fable".
func ModelFamily(model string) string {
	for _, part := range strings.Split(strings.TrimPrefix(strings.ToLower(model), "claude-"), "-") {
		if familyWord.MatchString(part) {
			return part
		}
	}
	return ""
}

const (
	overageWindow = "seven_day_overage_included"
	// unknownWindow is the name the binding window is kept under when the
	// event did not name it.
	unknownWindow = "unknown"
)

// scopeOf names the limit a rate-limit window is. The unnamed per-model
// weekly window belongs to the model the call ran on.
func scopeOf(window, model string) string {
	switch {
	case window == "five_hour":
		return ScopeSession
	case window == "seven_day":
		return ScopeWeekly
	case window == overageWindow:
		if model != "" {
			return ModelFamily(model)
		}
	case strings.HasPrefix(window, "seven_day_"):
		return ModelFamily(strings.TrimPrefix(window, "seven_day_"))
	}
	return window
}

func isAccount(scope string) bool { return scope == ScopeSession || scope == ScopeWeekly }

func (l Limit) unexpired(now time.Time) bool {
	return l.resetsEpoch == 0 || l.resetsEpoch > now.Unix()
}

// rejection is a limit that refused the last call, and when it frees.
type rejection struct {
	scope    string
	resetsAt string
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// build assembles a verdict from the limits a sensor reported. The binding
// limit is the fuller of the two account limits; a rejection on an account
// limit holds the Step until it frees; a per-model limit only switches the
// model.
func (p Policy) build(now time.Time, sensor, state string, limits []Limit, rejected *rejection, observed string, warnings []string) Verdict {
	v := Verdict{Sensor: sensor, State: state, ObservedAt: observed, Limits: []Limit{}, Warnings: append([]string{}, warnings...)}
	var binding *Limit
	for i, l := range limits {
		v.Limits = append(v.Limits, l)
		if isAccount(l.Scope) && l.Utilization != nil && l.unexpired(now) && (binding == nil || *l.Utilization >= *binding.Utilization) {
			binding = &limits[i]
		}
	}
	if binding != nil {
		remain := min(max(100-*binding.Utilization, 0), 100)
		v.RemainPct = &remain
		v.ResetAt = binding.ResetsAt
	}
	held := rejected != nil && isAccount(rejected.scope)
	if held {
		v.ResetAt = Text(rejected.resetsAt)
	}
	v.ShouldFire = !held && (v.RemainPct == nil || *v.RemainPct >= p.MinPct)

	primary := ModelFamily(p.Primary)
	rejectedPrimary := rejected != nil && rejected.scope == primary
	spent := rejectedPrimary
	for _, l := range limits {
		if !isAccount(l.Scope) && l.Scope == primary {
			spent = spent || (l.unexpired(now) && l.Utilization != nil && *l.Utilization >= p.SwitchPct)
			if !rejectedPrimary && string(l.ResetsAt) > string(v.FireModelUntil) {
				v.FireModelUntil = l.ResetsAt
			}
		}
	}
	if p.Fallback != "" && spent {
		v.FireModel = Text(p.Fallback)
		if rejectedPrimary {
			v.FireModelUntil = Text(rejected.resetsAt)
		}
		v.Warnings = append(v.Warnings, fmt.Sprintf("model policy: %s limit spent, Steps run on %s", primary, p.Fallback))
	} else {
		v.FireModelUntil = ""
	}
	if held {
		v.Warnings = append(v.Warnings, fmt.Sprintf("%s limit rejected the last Step, holding until %s", rejected.scope, rejected.resetsAt))
	}
	return v
}

// plain is a verdict with no limits behind it.
func (p Policy) plain(now time.Time, sensor, state string, fire bool, warnings ...string) Verdict {
	v := p.build(now, sensor, state, nil, nil, iso(now), warnings)
	v.ShouldFire = fire
	return v
}

// endpointWindow is one window of the usage endpoint's payload.
type endpointWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    Text     `json:"resets_at"`
}

// EndpointVerdict is the verdict from the usage endpoint's payload: the
// pre-Step sensor of a login Host.
func (p Policy) EndpointVerdict(payload []byte, now time.Time) Verdict {
	var limits []Limit
	add := func(scope string, utilization *float64, resetsAt Text) {
		if scope == "" || utilization == nil {
			return
		}
		for _, l := range limits {
			if l.Scope == scope {
				return // the first source to name a scope wins
			}
		}
		// The endpoint is read now, so nothing in it has expired.
		limits = append(limits, Limit{Scope: scope, Utilization: utilization, ResetsAt: resetsAt})
	}
	keys, fields := orderedObject(payload)
	window := func(key string) (w endpointWindow, ok bool) {
		raw := bytes.TrimSpace(fields[key])
		return w, len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, &w) == nil
	}
	for _, named := range [][2]string{{"five_hour", ScopeSession}, {"seven_day", ScopeWeekly}} {
		if w, ok := window(named[0]); ok {
			add(named[1], w.Utilization, w.ResetsAt)
		}
	}
	for _, key := range keys {
		if strings.HasPrefix(key, "seven_day_") && key != overageWindow {
			if w, ok := window(key); ok {
				add(ModelFamily(strings.TrimPrefix(key, "seven_day_")), w.Utilization, w.ResetsAt)
			}
		}
	}
	// The scoped list names per-model limits the windows above do not.
	var scoped []json.RawMessage
	json.Unmarshal(fields["limits"], &scoped)
	for _, raw := range scoped {
		var entry struct {
			Percent  *float64 `json:"percent"`
			ResetsAt Text     `json:"resets_at"`
			Scope    struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		}
		if json.Unmarshal(raw, &entry) == nil {
			add(ModelFamily(entry.Scope.Model.DisplayName), entry.Percent, entry.ResetsAt)
		}
	}
	for _, l := range limits {
		if isAccount(l.Scope) {
			return p.build(now, SensorEndpoint, StateOK, limits, nil, iso(now), nil)
		}
	}
	v := p.build(now, SensorEndpoint, StateUnavailable, limits, nil, iso(now), []string{"usage endpoint payload carries no account window"})
	v.ShouldFire = false
	return v
}

// orderedObject reads a JSON object's fields, keeping the order of its keys.
func orderedObject(data []byte) (keys []string, fields map[string]json.RawMessage) {
	fields = map[string]json.RawMessage{}
	dec := json.NewDecoder(bytes.NewReader(data))
	if open, err := dec.Token(); err != nil || open != json.Delim('{') {
		return nil, fields
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			break
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			break
		}
		name, _ := key.(string)
		keys = append(keys, name)
		fields[name] = value
	}
	return keys, fields
}

// EventVerdict is the verdict seeded from the last rate-limit event a Step's
// stream carried: what a setup-token Host knows before a Step.
func (p Policy) EventVerdict(record Record, now time.Time) Verdict {
	model := string(record.Model)
	var limits []Limit
	live := 0
	var degraded []string
	if record.Source == SourceBinding {
		degraded = append(degraded, "the rate-limit event carried no unifiedWindows: only its binding window is known")
	}
	for _, name := range record.windowNames() {
		w := record.Windows[name]
		scope := scopeOf(name, model)
		if name == unknownWindow {
			scope = ScopeSession
			if record.Status != "rejected" {
				degraded = append(degraded, "the rate-limit event named no limit (no rateLimitType): treated as the session limit")
			}
		}
		l := Limit{Scope: scope, Utilization: w.UsedPct, ResetsAt: w.ResetsAtIso}
		if w.ResetsAt != nil {
			l.resetsEpoch = *w.ResetsAt
		}
		if w.UsedPct == nil {
			degraded = append(degraded, fmt.Sprintf("the rate-limit event gave no utilization for its %s window", name))
		}
		if !l.unexpired(now) {
			continue
		}
		limits = append(limits, l)
		if isAccount(l.Scope) {
			live++
		}
	}
	var rejected *rejection
	if record.Status == "rejected" && record.ResetsAt != nil && *record.ResetsAt > now.Unix() {
		rejected = &rejection{scope: scopeOf(string(record.RateLimitType), model), resetsAt: string(record.ResetsAtIso)}
		if record.RateLimitType == "" {
			// The event did not say which limit refused the call. Holding
			// for a limit that would only have switched the model costs a
			// wait; not holding for the session limit costs a failed Step.
			rejected.scope = ScopeSession
			degraded = append(degraded, "the rate-limit event named no limit (no rateLimitType): treated as the session limit")
		}
	}
	if live == 0 && rejected == nil {
		return p.build(now, SensorNone, StateUnavailable, nil, nil, iso(now),
			[]string{fmt.Sprintf("the last rate-limit event (Fire %s) has reset its windows; going optimistically", record.FireID)})
	}
	warnings := append([]string{fmt.Sprintf("seeded from the rate-limit event of Fire %s", record.FireID)}, degraded...)
	return p.build(now, SensorStreamEvents, StateStale, limits, rejected, record.ObservedAt, warnings)
}

var fraction = regexp.MustCompile(`\.[0-9]+Z$`)

// ReportVerdict is the verdict seeded from the last Step's EXHAUSTED
// outcome when only a limit string said so: no number, a reset to hold
// until, and a model switch when the string named the primary.
func (p Policy) ReportVerdict(report StepReport, now time.Time) Verdict {
	reset, err := time.Parse(time.RFC3339, fraction.ReplaceAllString(report.ResetAt, "Z"))
	if report.Outcome != step.Exhausted || err != nil || !reset.After(now) {
		return p.build(now, SensorNone, StateUnavailable, nil, nil, iso(now), []string{"no un-expired limit verdict; going optimistically"})
	}
	scope := strings.ToLower(string(report.LimitType))
	switch {
	case scope == "":
		scope = ScopeSession
	case !isAccount(scope):
		scope = ModelFamily(scope)
	}
	observed := report.ObservedAt
	if observed == "" {
		observed = iso(now)
	}
	return p.build(now, SensorLimitStrings, StateStale, nil, &rejection{scope: scope, resetsAt: iso(reset)}, observed,
		[]string{fmt.Sprintf("seeded from the limit string of the last Step (%s)", scope)})
}
