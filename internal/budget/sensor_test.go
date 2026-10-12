package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
)

// Ported from lib/usage-sensor.test.sh, with its fixtures.

// 2026-09-15T00:00:00Z
var now = time.Unix(1789430400, 0).UTC()

const (
	loginStatus  = `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	tokenStatus  = `{"loggedIn":true,"authMethod":"oauth_token"}`
	apiKeyStatus = `{"loggedIn":true,"authMethod":"claude.ai","apiKeySource":"ANTHROPIC_API_KEY"}`
	deadStatus   = `{"loggedIn":false,"authMethod":"none"}`
	// The endpoint's live shape: 19% session, 18% weekly, Fable at 97%.
	endpointBody = `{"five_hour":{"utilization":19,"resets_at":"2026-09-15T05:10:00+00:00"},"seven_day":{"utilization":18,"resets_at":"2026-09-18T19:00:00+00:00"},"seven_day_opus":{"utilization":40,"resets_at":"2026-09-18T19:00:00+00:00"},"limits":[{"kind":"session","percent":19,"is_active":true,"resets_at":"2026-09-15T05:10:00+00:00"},{"kind":"weekly_model","percent":97,"is_active":true,"resets_at":"2026-09-18T19:00:00+00:00","scope":{"model":{"display_name":"Fable"}}}]}`
	// The record of an allowed event from a Fable Step, and a rejected one.
	eventAllowed  = `{"observedAt":"2026-09-14T23:00:00Z","fireId":"f1","sessionId":"s1","model":"claude-fable-5-1","status":"allowed","rateLimitType":"five_hour","resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z","utilization":null,"isUsingOverage":false,"overageStatus":"rejected","source":"windows","windows":{"five_hour":{"usedPct":24,"resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z"},"seven_day":{"usedPct":6,"resetsAt":1789758000,"resetsAtIso":"2026-09-18T19:00:00Z"},"seven_day_overage_included":{"usedPct":11,"resetsAt":1789758000,"resetsAtIso":"2026-09-18T19:00:00Z"}}}`
	eventRejected = `{"observedAt":"2026-09-14T23:30:00Z","fireId":"f2","sessionId":"s2","model":"claude-fable-5-1","status":"rejected","rateLimitType":"five_hour","resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z","utilization":1,"isUsingOverage":false,"overageStatus":"rejected","source":"binding","windows":{"five_hour":{"usedPct":100,"resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z"}}}`
)

// world is a sensor with everything outside it scripted: claude's auth
// status, the usage endpoint, the environment and the clock.
type world struct {
	t          *testing.T
	sensor     *Sensor
	env        map[string]string
	authOut    string
	authErr    error
	httpCode   int
	body       string
	transport  bool // the endpoint cannot be reached
	calls      []*http.Request
	server     *httptest.Server
	credsFile  string
	clockShift time.Duration
}

// newWorld is a login Host under the fable-primary, opus-fallback policy the
// fixtures were written against.
func newWorld(t *testing.T) *world {
	t.Helper()
	home := t.TempDir()
	w := &world{t: t, env: map[string]string{"HOME": home}, authOut: loginStatus, httpCode: 200, body: endpointBody}
	w.credsFile = filepath.Join(home, ".claude", ".credentials.json")
	os.MkdirAll(filepath.Dir(w.credsFile), 0o755)
	os.WriteFile(w.credsFile, []byte(`{"claudeAiOauth":{"accessToken":"tok-1"}}`), 0o600)
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.calls = append(w.calls, r.Clone(context.Background()))
		if w.transport {
			panic(http.ErrAbortHandler)
		}
		rw.WriteHeader(w.httpCode)
		fmt.Fprint(rw, w.body)
	}))
	t.Cleanup(w.server.Close)
	w.sensor = &Sensor{
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Policy:     Policy{MinPct: 25, StaleMax: time.Hour, Primary: "fable", Fallback: "opus", SwitchPct: 95},
		LookupEnv:  func(k string) (string, bool) { v, ok := w.env[k]; return v, ok },
		AuthStatus: func(context.Context) ([]byte, error) { return []byte(w.authOut), w.authErr },
		UsageURL:   w.server.URL + "/api/oauth/usage",
		Now:        func() time.Time { return now.Add(w.clockShift) },
	}
	return w
}

// setupToken turns the world into a setup-token Host.
func (w *world) setupToken() *world {
	w.authOut, w.env[envOAuthToken] = tokenStatus, "t"
	return w
}

func (w *world) verdict() (Verdict, error) { return w.sensor.Verdict(context.Background()) }

func (w *world) must() Verdict {
	w.t.Helper()
	v, err := w.verdict()
	if err != nil {
		w.t.Fatalf("Verdict: %v", err)
	}
	return v
}

func (w *world) writeState(name, content string) {
	w.t.Helper()
	os.MkdirAll(w.sensor.StateDir, 0o755)
	if err := os.WriteFile(filepath.Join(w.sensor.StateDir, name), []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// edit returns the JSON document with change applied to it.
func edit(t *testing.T, document string, change func(map[string]any)) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(document), &v); err != nil {
		t.Fatal(err)
	}
	change(v)
	out, _ := json.Marshal(v)
	return string(out)
}

// show renders the parts of a verdict a case compares, as the bash checks did.
func show(parts ...any) string {
	out, _ := json.Marshal(parts)
	return string(out)
}

func scopes(v Verdict) string {
	var pairs [][]any
	for _, l := range v.Limits {
		pairs = append(pairs, []any{l.Scope, l.Utilization})
	}
	out, _ := json.Marshal(pairs)
	return string(out)
}

func warns(v Verdict, fragment string) bool {
	return strings.Contains(strings.Join(v.Warnings, "\n"), fragment)
}

func check(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n got %s\nwant %s", name, got, want)
	}
}

func TestLoginEndpointOK(t *testing.T) {
	t.Run("login: the usage endpoint is the pre-Fire sensor", func(t *testing.T) {
		w := newWorld(t)
		v := w.must()
		check(t, "usage-endpoint, ok, remainPct 81 (100 minus the binding session), session reset, fires",
			show(v.AuthMode, v.Sensor, v.State, v.RemainPct, v.ResetAt, v.ShouldFire, v.ObservedAt),
			`["login","usage-endpoint","ok",81,"2026-09-15T05:10:00+00:00",true,"2026-09-15T00:00:00Z"]`)
		check(t, "limits[] carries session, weekly and the per-model scopes without endpoint field names",
			scopes(v), `[["session",19],["weekly",18],["opus",40],["fable",97]]`)
		check(t, "the per-model Fable limit does not gate, it switches the model",
			show(v.ShouldFire, v.FireModel, v.FireModelUntil), `[true,"opus","2026-09-18T19:00:00+00:00"]`)
		if len(w.calls) != 1 || w.calls[0].Header.Get("Authorization") != "Bearer tok-1" || w.calls[0].URL.Path != "/api/oauth/usage" || w.calls[0].Header.Get("anthropic-beta") == "" {
			t.Errorf("the endpoint was not read with the login token: %+v", w.calls)
		}
		var m memory
		readJSON(w.sensor.memoryPath(), &m)
		check(t, "usage-sensor.json remembers the good verdict and the model hold",
			show(m.Endpoint.LastGoodAt, m.Endpoint.LastVerdict.RemainPct, m.ModelHold),
			`["2026-09-15T00:00:00Z",81,{"model":"opus","until":"2026-09-18T19:00:00+00:00"}]`)

		w.body = edit(t, endpointBody, func(v map[string]any) { v["five_hour"].(map[string]any)["utilization"] = 80 })
		v = w.must()
		check(t, "80% session used: remainPct 20, shouldFire false", show(v.RemainPct, v.ShouldFire, v.ResetAt), `[20,false,"2026-09-15T05:10:00+00:00"]`)
		w.sensor.Policy.MinPct = 10
		if v = w.must(); !v.ShouldFire {
			t.Error("the gate threshold does not move with the policy")
		}
		w.body = `{"five_hour":{"utilization":10,"resets_at":"2026-09-15T05:10:00+00:00"},"seven_day":{"utilization":5,"resets_at":"2026-09-18T19:00:00+00:00"},"limits":[{"percent":5,"scope":"x"},{"percent":7,"scope":null},"junk"]}`
		v = w.must()
		check(t, "odd limits[] entries are skipped, the verdict still computes", show(v.State, v.RemainPct, len(v.Limits)), `["ok",90,2]`)
	})
	t.Run("a payload with no account window is unavailable and never fires", func(t *testing.T) {
		w := newWorld(t)
		w.body = `{"seven_day_opus":{"utilization":40}}`
		v := w.must()
		check(t, "unavailable", show(v.Sensor, v.State, v.ShouldFire, warns(v, "no account window")), `["usage-endpoint","unavailable",false,true]`)
	})
}

func TestLoginStaleHoldAndFallthrough(t *testing.T) {
	t.Run("login: 429/5xx keeps the last verdict for 60 minutes, then the Host behaves like setup-token", func(t *testing.T) {
		w := newWorld(t)
		w.must()
		w.httpCode, w.clockShift = 429, 30*time.Minute
		v := w.must()
		check(t, "30 min later, 429: the held verdict, state stale, numbers kept",
			show(v.Sensor, v.State, v.RemainPct, v.ShouldFire, v.ObservedAt, warns(v, "429")),
			`["usage-endpoint","stale",81,true,"2026-09-15T00:00:00Z",true]`)
		w.clockShift = 3601 * time.Second
		v = w.must()
		check(t, "61 min later, still 429, no event: optimistic like a setup-token Host",
			show(v.Sensor, v.State, v.RemainPct, v.ShouldFire, warns(v, "older than 3600s")), `["none","unavailable",null,true,true]`)
		w.writeState(rateLimitsFile, eventAllowed)
		v = w.must()
		check(t, "61 min later with a tapped event: the event seeds the verdict", show(v.Sensor, v.State, v.RemainPct), `["stream-events","stale",76]`)
		w.transport, w.clockShift = true, 10*time.Minute
		v = w.must()
		check(t, "a transport failure is held like a 5xx", show(v.State, v.RemainPct), `["stale",81]`)
	})
}

func TestLogin403MarksTheEndpointUnavailable(t *testing.T) {
	t.Run("login: 403 marks the endpoint unavailable for the Daemon process", func(t *testing.T) {
		w := newWorld(t)
		w.httpCode, w.sensor.DaemonID = 403, "d1"
		v := w.must()
		check(t, "403: setup-token behaviour with a warning", show(v.Sensor, v.State, v.ShouldFire, warns(v, "403")), `["none","unavailable",true,true]`)
		var m memory
		readJSON(w.sensor.memoryPath(), &m)
		check(t, "the mark is written with the Daemon id", show(m.Endpoint.ForbiddenAt, m.Endpoint.ForbiddenDaemon), `["2026-09-15T00:00:00Z","d1"]`)
		w.httpCode = 200
		if v = w.must(); len(w.calls) != 1 || v.Sensor != SensorNone {
			t.Errorf("the same Daemon asked the endpoint again: %d calls, sensor %s", len(w.calls), v.Sensor)
		}
		w.sensor.DaemonID = "d2"
		if v = w.must(); len(w.calls) != 2 || v.Sensor != SensorEndpoint {
			t.Errorf("a new Daemon process did not try the endpoint again: %d calls, sensor %s", len(w.calls), v.Sensor)
		}
		// With no Daemon id the mark is never honoured: a by-hand 403 must
		// not poison the next call.
		w.sensor.DaemonID, w.httpCode = "", 403
		w.must()
		w.httpCode = 200
		if v = w.must(); len(w.calls) != 4 || v.Sensor != SensorEndpoint {
			t.Errorf("with no Daemon id a 403 outlived its call: %d calls, sensor %s", len(w.calls), v.Sensor)
		}
	})
}

func TestAuthDeadParks(t *testing.T) {
	t.Run("401, a failed auth status and a missing credential are auth-dead", func(t *testing.T) {
		w := newWorld(t)
		w.httpCode = 401
		v, err := w.verdict()
		if !errors.Is(err, ErrAuthDead) {
			t.Errorf("401: err = %v", err)
		}
		check(t, "401: state auth-dead, never fires, warning names it",
			show(v.Sensor, v.State, v.ShouldFire, v.RemainPct, warns(v, "401")), `["none","auth-dead",false,null,true]`)

		w.authOut, w.authErr, w.httpCode, w.calls = deadStatus, errors.New("exit status 1"), 200, nil
		if v, err = w.verdict(); !errors.Is(err, ErrAuthDead) || v.State != StateAuthDead || len(w.calls) != 0 {
			t.Errorf("a failed auth status: %v, %s, %d endpoint calls", err, v.State, len(w.calls))
		}
		w.authErr = nil
		if v, err = w.verdict(); !errors.Is(err, ErrAuthDead) || v.State != StateAuthDead {
			t.Errorf("auth status says not logged in: %v, %s", err, v.State)
		}
		w.env[envOAuthToken] = "t"
		w.sensor.DeclaredMode = ModeSetupToken
		if v, err = w.verdict(); !errors.Is(err, ErrAuthDead) || v.State != StateAuthDead || v.AuthMode != ModeSetupToken {
			t.Errorf("setup-token with a dead token: %v, %+v", err, v)
		}
		delete(w.env, envOAuthToken)
		w.sensor.DeclaredMode, w.authOut = "", loginStatus
		os.Remove(w.credsFile)
		if v, err = w.verdict(); !errors.Is(err, ErrAuthDead) || v.State != StateAuthDead {
			t.Errorf("no credentials file: %v, %s", err, v.State)
		}
	})
}

func TestModeMismatchFailsLoud(t *testing.T) {
	tests := []struct {
		name     string
		declared string
		status   string
		env      map[string]string
	}{
		{"login with CLAUDE_CODE_OAUTH_TOKEN set", ModeLogin, loginStatus, map[string]string{envOAuthToken: "t"}},
		{"login with ANTHROPIC_API_KEY set", ModeLogin, loginStatus, map[string]string{envAPIKey: "k"}},
		{"login but auth status reports oauth_token", ModeLogin, tokenStatus, nil},
		{"login but auth status reports an API key", ModeLogin, apiKeyStatus, nil},
		{"setup-token without CLAUDE_CODE_OAUTH_TOKEN", ModeSetupToken, tokenStatus, nil},
		{"setup-token but auth status reports claude.ai", ModeSetupToken, loginStatus, map[string]string{envOAuthToken: "t"}},
		{"api-key without ANTHROPIC_API_KEY", ModeAPIKey, apiKeyStatus, nil},
		{"an unknown mode", "bogus", loginStatus, nil},
		// With no declared mode it is read from claude auth status, and
		// still cross-checked against the secrets: never guessed.
		{"no declared mode: auth status says login but a token is set", "", loginStatus, map[string]string{envOAuthToken: "t"}},
		{"no declared mode: auth status says setup-token but no token is set", "", tokenStatus, nil},
		{"no declared mode: auth status says an API key but none is set", "", apiKeyStatus, nil},
		{"no declared mode: an auth method the sensor does not know", "", `{"loggedIn":true,"authMethod":"bedrock"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.sensor.DeclaredMode, w.authOut = tt.declared, tt.status
			for k, v := range tt.env {
				w.env[k] = v
			}
			v, err := w.verdict()
			if !errors.Is(err, ErrModeMismatch) || v.ShouldFire || v.State != StateUnavailable || !warns(v, "mode mismatch") {
				t.Errorf("err = %v, verdict = %+v", err, v)
			}
			if len(w.calls) != 0 {
				t.Error("a mismatch reached the endpoint")
			}
		})
	}
	t.Run("no declared mode: the mode is the one claude auth status reports", func(t *testing.T) {
		if v := newWorld(t).must(); v.AuthMode != ModeLogin {
			t.Errorf("login: authMode = %q", v.AuthMode)
		}
		if v := newWorld(t).setupToken().must(); v.AuthMode != ModeSetupToken {
			t.Errorf("setup-token: authMode = %q", v.AuthMode)
		}
	})
}

func TestAPIKeyRefusesToStart(t *testing.T) {
	t.Run("api-key is schema-only: sensor spend, never fires", func(t *testing.T) {
		w := newWorld(t)
		w.authOut, w.env[envAPIKey] = apiKeyStatus, "k"
		v, err := w.verdict()
		if !errors.Is(err, ErrSpendPacing) {
			t.Errorf("err = %v", err)
		}
		check(t, "spend, unavailable, shouldFire false, the warning names spend pacing",
			show(v.AuthMode, v.Sensor, v.State, v.ShouldFire, v.RemainPct, warns(v, "spend pacing")), `["api-key","spend","unavailable",false,null,true]`)
	})
}

func TestSetupTokenSeedsFromTheEvent(t *testing.T) {
	t.Run("setup-token seeds from the last tapped rate-limit event", func(t *testing.T) {
		w := newWorld(t).setupToken()
		v := w.must()
		check(t, "no event yet: none, unavailable, remainPct null, fires optimistically",
			show(v.AuthMode, v.Sensor, v.State, v.RemainPct, v.ResetAt, v.ShouldFire), `["setup-token","none","unavailable",null,null,true]`)
		w.writeState(rateLimitsFile, eventAllowed)
		v = w.must()
		check(t, "stream-events, stale, observedAt from that Fire, remainPct 76 from the session window",
			show(v.Sensor, v.State, v.ObservedAt, v.RemainPct, v.ResetAt, v.ShouldFire), `["stream-events","stale","2026-09-14T23:00:00Z",76,"2026-09-15T05:10:00Z",true]`)
		check(t, "the unnamed per-model window is keyed to the model that fired", scopes(v), `[["session",24],["weekly",6],["fable",11]]`)
		w.writeState(rateLimitsFile, eventRejected)
		v = w.must()
		check(t, "a rejected event sets resetAt and holds the Fire",
			show(v.Sensor, v.State, v.RemainPct, v.ResetAt, v.ShouldFire, warns(v, "session limit rejected")), `["stream-events","stale",0,"2026-09-15T05:10:00Z",false,true]`)
		w.clockShift = time.Unix(1789449001, 0).Sub(now)
		v = w.must()
		check(t, "once the rejected window has reset: optimistic again", show(v.Sensor, v.ShouldFire, v.ResetAt), `["none",true,null]`)
		w.clockShift = 0
		// A rejected named per-model window is that model's, not the Fire's.
		w.writeState(rateLimitsFile, edit(t, eventRejected, func(v map[string]any) {
			v["rateLimitType"], v["resetsAt"], v["resetsAtIso"] = "seven_day_opus", 1789758000, "2026-09-18T19:00:00Z"
			v["windows"] = map[string]any{"five_hour": map[string]any{"usedPct": 30, "resetsAt": 1789449000, "resetsAtIso": "2026-09-15T05:10:00Z"}}
		}))
		v = w.must()
		check(t, "rejected seven_day_opus on a Fable Fire: not held, no switch onto opus",
			show(v.ShouldFire, v.ResetAt, v.FireModel, warns(v, "opus")), `[true,"2026-09-15T05:10:00Z",null,false]`)
		w.writeState(rateLimitsFile, edit(t, eventAllowed, func(v map[string]any) { v["windows"] = nil }))
		v = w.must()
		check(t, "a record without windows still yields a verdict", show(v.Sensor, v.ShouldFire), `["none",true]`)
		if len(w.calls) != 0 {
			t.Error("the endpoint was asked in setup-token mode")
		}
	})
}

// exhausted is a Step cut off by a limit string, with no rate-limit event.
func exhausted(text string) step.Run {
	return step.Run{Outcome: step.Exhausted, Output: text}
}

func TestSetupTokenLimitStringOutcome(t *testing.T) {
	t.Run("setup-token: the last Fire's limit-string outcome seeds the verdict when newer than the event", func(t *testing.T) {
		w := newWorld(t).setupToken()
		w.writeState(rateLimitsFile, eventAllowed)
		w.clockShift = -15 * time.Minute // the Step ended at 23:45
		if err := w.sensor.Observe("f3", exhausted("You've hit your session limit · resets 10:50pm (America/New_York)")); err != nil {
			t.Fatal(err)
		}
		w.clockShift = 0
		v := w.must()
		check(t, "limit-strings, stale, held until the scraped reset",
			show(v.Sensor, v.State, v.RemainPct, v.ResetAt, v.ShouldFire, v.ObservedAt), `["limit-strings","stale",null,"2026-09-15T02:50:00Z",false,"2026-09-14T23:45:00Z"]`)

		// A later event makes the outcome the older of the two.
		w.writeState(rateLimitsFile, edit(t, eventAllowed, func(v map[string]any) { v["observedAt"] = "2026-09-14T23:50:00Z" }))
		v = w.must()
		check(t, "an outcome older than the event loses to the event", show(v.Sensor, v.ShouldFire), `["stream-events",true]`)

		w.clockShift = -10 * time.Minute
		w.sensor.Observe("f4", exhausted("You've hit your Fable limit · resets 2026-09-20T00:00:00Z"))
		w.clockShift = 0
		v = w.must()
		check(t, "a per-model limit string switches the model and does not hold the Fire",
			show(v.Sensor, v.ShouldFire, v.ResetAt, v.FireModel, v.FireModelUntil), `["limit-strings",true,null,"opus","2026-09-20T00:00:00Z"]`)
	})
	t.Run("a Step's events and its limit-string outcome are recorded together: the outcome is not the older one", func(t *testing.T) {
		w := newWorld(t).setupToken()
		run := exhausted("You've hit your session limit · resets 10:50pm (America/New_York)")
		run.RateLimits = []json.RawMessage{json.RawMessage(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789449000,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.9,"resetsAt":1789449000}}}}`)}
		w.sensor.Observe("f5", run)
		v := w.must()
		check(t, "held by the limit string", show(v.Sensor, v.ShouldFire, v.ResetAt), `["limit-strings",false,"2026-09-15T02:50:00Z"]`)
	})
	t.Run("a Step cut off by a rejected event is held by that event, on its account window", func(t *testing.T) {
		w := newWorld(t).setupToken()
		w.sensor.Observe("f8", step.Run{Outcome: step.Exhausted, Model: "claude-fable-5-1", RateLimits: []json.RawMessage{json.RawMessage(
			`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789449000,"rateLimitType":"five_hour","utilization":1}}`)}})
		v := w.must()
		check(t, "held until the event's reset", show(v.Sensor, v.ShouldFire, v.ResetAt, warns(v, "session limit rejected")), `["stream-events",false,"2026-09-15T05:10:00Z",true]`)
	})
	t.Run("the last outcome's login-expiry notice rides on the verdict until a Step runs without it", func(t *testing.T) {
		w := newWorld(t).setupToken()
		w.sensor.Observe("f6", step.Run{Outcome: step.OK, Output: "Your login expires in 3 days. Run /login to renew.\n"})
		if v := w.must(); !warns(v, "login expires in 3 days") {
			t.Errorf("warnings = %v", v.Warnings)
		}
		w.sensor.Observe("f7", step.Run{Outcome: step.OK})
		if v := w.must(); warns(v, "login expires") {
			t.Errorf("the notice outlived a clean Step: %v", v.Warnings)
		}
	})
}

func TestModelPolicyHold(t *testing.T) {
	t.Run("the model switch is remembered until its reset, whatever the next Fire's event shows", func(t *testing.T) {
		w := newWorld(t).setupToken()
		w.writeState(rateLimitsFile, edit(t, eventRejected, func(v map[string]any) {
			v["rateLimitType"], v["resetsAt"], v["resetsAtIso"] = "seven_day_overage_included", 1789758000, "2026-09-18T19:00:00Z"
			v["windows"] = map[string]any{
				"five_hour":                  map[string]any{"usedPct": 30, "resetsAt": 1789449000, "resetsAtIso": "2026-09-15T05:10:00Z"},
				"seven_day_overage_included": map[string]any{"usedPct": 100, "resetsAt": 1789758000, "resetsAtIso": "2026-09-18T19:00:00Z"},
			}
		}))
		v := w.must()
		check(t, "a rejected per-model window: fires on the fallback, not held",
			show(v.ShouldFire, v.FireModel, v.FireModelUntil, v.RemainPct), `[true,"opus","2026-09-18T19:00:00Z",70]`)
		// The next Step ran on opus: its event carries no fable window.
		w.writeState(rateLimitsFile, edit(t, eventAllowed, func(v map[string]any) {
			v["model"], v["observedAt"] = "claude-opus-5", "2026-09-15T01:00:00Z"
			delete(v["windows"].(map[string]any), "seven_day_overage_included")
		}))
		w.clockShift = 2 * time.Hour
		v = w.must()
		check(t, "the hold carries the switch across a Fire that could not see the primary's window",
			show(v.FireModel, v.FireModelUntil, warns(v, "until 2026-09-18T19:00:00Z")), `["opus","2026-09-18T19:00:00Z",true]`)
		w.clockShift = time.Unix(1789758001, 0).Sub(now)
		if v = w.must(); v.FireModel != "" {
			t.Errorf("after the reset the switch is still %q", v.FireModel)
		}
	})
	t.Run("the policy's knobs", func(t *testing.T) {
		w := newWorld(t)
		w.sensor.Policy.Fallback = ""
		v := w.must()
		check(t, "an empty fallback never switches", show(v.FireModel, v.ShouldFire), `[null,true]`)
		w.sensor.Policy = Policy{MinPct: 25, StaleMax: time.Hour, Primary: "opus", Fallback: "sonnet", SwitchPct: 30}
		v = w.must()
		check(t, "primary, fallback and switch percent come from the Host config", show(v.FireModel, v.FireModelUntil), `["sonnet","2026-09-18T19:00:00+00:00"]`)

		w = newWorld(t)
		w.sensor.Policy = Policy{MinPct: 25, StaleMax: time.Hour, Primary: "opus", SwitchPct: 95}
		w.body = edit(t, endpointBody, func(v map[string]any) { v["seven_day_opus"].(map[string]any)["utilization"] = 97 })
		v = w.must()
		check(t, "the defaults: primary opus, no fallback, a spent Opus window never switches", show(v.ShouldFire, v.FireModel, scopes(v)),
			`[true,null,"[[\"session\",19],[\"weekly\",18],[\"opus\",97],[\"fable\",97]]"]`)
		w.sensor.Policy.Primary, w.sensor.Policy.Fallback = "claude-opus-5-5", "sonnet"
		if v = w.must(); v.FireModel != "sonnet" {
			t.Errorf("a primary given as a full model id is not matched to its family: %+v", v)
		}
	})
}

func TestVerdictShape(t *testing.T) {
	t.Run("every verdict has the fixed keys, a boolean shouldFire and lists that are never null", func(t *testing.T) {
		w := newWorld(t)
		v := w.must()
		w.httpCode = 401
		dead, _ := w.verdict()
		for _, verdict := range []Verdict{v, dead} {
			data, _ := json.Marshal(verdict)
			var keys map[string]json.RawMessage
			json.Unmarshal(data, &keys)
			var names []string
			for _, k := range []string{"authMode", "sensor", "state", "remainPct", "resetAt", "shouldFire", "observedAt", "limits", "warnings", "fireModel", "fireModelUntil"} {
				if _, ok := keys[k]; ok {
					names = append(names, k)
				}
			}
			if len(names) != 11 || len(keys) != 11 || string(keys["limits"]) == "null" || string(keys["warnings"]) == "null" {
				t.Errorf("verdict = %s", data)
			}
		}
		// A verdict survives the sensor's own memory unchanged.
		var back Verdict
		data, _ := json.Marshal(v)
		json.Unmarshal(data, &back)
		if !reflect.DeepEqual(back, v) {
			t.Errorf("round trip changed the verdict:\n%+v\n%+v", v, back)
		}
	})
}
