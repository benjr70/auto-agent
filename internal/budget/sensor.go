package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
)

// DefaultUsageURL is the usage endpoint a login Host is gated on.
const DefaultUsageURL = "https://api.anthropic.com/api/oauth/usage"

// The secrets whose presence the auth mode is cross-checked against. The
// sensor never reads their values.
const (
	envOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
	envAPIKey     = "ANTHROPIC_API_KEY"
)

// Why the sensor refuses to give a verdict a Step may go on. Each comes
// with a verdict that says so and never fires.
var (
	// ErrAuthDead: Claude's credential is dead. The Daemon parks.
	ErrAuthDead = errors.New("Claude's credential is dead")
	// ErrModeMismatch: the auth mode contradicts the secrets present or
	// what `claude auth status` reports. Nothing runs until it is fixed.
	ErrModeMismatch = errors.New("auth mode mismatch")
	// ErrSpendPacing: an API key is billed by spend, which is not paced yet.
	ErrSpendPacing = errors.New("api-key mode refuses to start until spend pacing exists")
)

// ClaudeAuthStatus asks the claude CLI how it is logged in. The error is
// non-nil when the command fails, which is what a dead credential does.
func ClaudeAuthStatus(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "claude", "auth", "status", "--json").Output()
}

// Sensor is the Usage sensor: the one implementation that reads the
// account's limits and emits the Gate verdict.
type Sensor struct {
	// StateDir is where the sensor keeps its memory.
	StateDir string
	Policy   Policy
	// DeclaredMode is the auth mode the Host config declares.
	DeclaredMode string
	// LookupEnv reads the environment, as os.LookupEnv does.
	LookupEnv func(string) (string, bool)
	// AuthStatus is ClaudeAuthStatus outside tests.
	AuthStatus func(context.Context) ([]byte, error)
	// UsageURL is the usage endpoint; empty means DefaultUsageURL.
	UsageURL string
	// HTTP makes the endpoint request; nil means a client with a timeout.
	HTTP *http.Client
	// DaemonID keys the mark a 403 from the endpoint leaves, so that one
	// Daemon process stops asking. With none, a 403 holds for one call.
	DaemonID string
	Now      func() time.Time
}

// memory is the sensor's own file in the State dir.
type memory struct {
	Endpoint  *endpointMemory `json:"endpoint,omitempty"`
	ModelHold *modelHold      `json:"modelHold,omitempty"`
	LastStep  *lastStep       `json:"lastStep,omitempty"`
}

// endpointMemory is the last good endpoint verdict (the stale hold) and the
// 403 mark.
type endpointMemory struct {
	LastVerdict     *Verdict `json:"lastVerdict,omitempty"`
	LastGoodAt      string   `json:"lastGoodAt,omitempty"`
	ForbiddenAt     string   `json:"forbiddenAt,omitempty"`
	ForbiddenDaemon string   `json:"forbiddenDaemon,omitempty"`
}

// modelHold remembers the Model policy's switch until the reset it named.
type modelHold struct {
	Model string `json:"model"`
	Until string `json:"until"`
}

// lastStep is how the last Step ended, and when.
type lastStep struct {
	Report  StepReport `json:"report"`
	EndedAt string     `json:"endedAt"`
}

func (s *Sensor) memoryPath() string { return filepath.Join(s.StateDir, sensorFile) }

func (s *Sensor) remember(change func(*memory)) error {
	var m memory
	readJSON(s.memoryPath(), &m)
	change(&m)
	return writeJSON(s.memoryPath(), m)
}

func (s *Sensor) env(name string) string {
	value, _ := s.LookupEnv(name)
	return value
}

// Verdict is the Gate verdict for the next Step. It always returns a
// verdict; the error is one of ErrAuthDead, ErrModeMismatch and
// ErrSpendPacing when the Step must not run whatever the budget.
func (s *Sensor) Verdict(ctx context.Context) (Verdict, error) {
	var m memory
	readJSON(s.memoryPath(), &m)
	mode, verdict, err := s.sense(ctx, m)
	verdict.AuthMode = Text(mode)
	// The login-expiry notice the last Step printed rides on every verdict
	// until a Step runs without it.
	if m.LastStep != nil {
		carried := append([]string{}, m.LastStep.Report.Warnings...)
		if m.LastStep.Report.Outcome == step.AuthDead {
			carried = append(carried, "the last Step found Claude's credential dead at "+m.LastStep.EndedAt)
		}
		verdict.Warnings = append(carried, verdict.Warnings...)
	}
	if err != nil {
		return verdict, err
	}
	return s.withModelHold(m, verdict), nil
}

// withModelHold keeps the Model policy's switch alive. A Step on the
// fallback model never refreshes the primary's window, so the switch is
// remembered until the reset it named and applied to every verdict till then.
func (s *Sensor) withModelHold(m memory, verdict Verdict) Verdict {
	if verdict.FireModel != "" && verdict.FireModelUntil != "" {
		if err := s.remember(func(m *memory) {
			m.ModelHold = &modelHold{Model: string(verdict.FireModel), Until: string(verdict.FireModelUntil)}
		}); err != nil {
			verdict.Warnings = append(verdict.Warnings, "the model switch could not be remembered: "+err.Error())
		}
		return verdict
	}
	if hold := m.ModelHold; hold != nil && verdict.FireModel == "" {
		if until, err := time.Parse(time.RFC3339, hold.Until); err == nil && until.After(s.Now()) {
			verdict.FireModel, verdict.FireModelUntil = Text(hold.Model), Text(hold.Until)
			verdict.Warnings = append(verdict.Warnings,
				fmt.Sprintf("model policy: %s limit spent until %s, Steps run on %s", ModelFamily(s.Policy.Primary), hold.Until, hold.Model))
		}
	}
	return verdict
}

// authStatus is what `claude auth status --json` reports.
type authStatus struct {
	LoggedIn     bool   `json:"loggedIn"`
	AuthMethod   string `json:"authMethod"`
	APIKeySource string `json:"apiKeySource"`
}

// mode cross-checks the auth mode the Host config declares against the
// secrets present and against what `claude auth status` reports. It is
// never inferred: a secret left behind in the environment would make the
// inference wrong, and the declaration is the witness that catches it (ADR
// 0008). reason is empty when everything agrees.
func (s *Sensor) mode(status authStatus, alive bool) (mode, reason string) {
	mode = s.DeclaredMode
	if mode == "" {
		return "", "the auth mode is not declared: set CLAUDE_AUTH_MODE in the Host config to login, setup-token or api-key"
	}
	token, key := s.env(envOAuthToken) != "", s.env(envAPIKey) != ""
	switch mode {
	case ModeLogin:
		if token {
			return mode, "the auth mode is login but " + envOAuthToken + " is set"
		}
		if key {
			return mode, "the auth mode is login but " + envAPIKey + " is set"
		}
	case ModeSetupToken:
		if !token {
			return mode, "the auth mode is setup-token but " + envOAuthToken + " is not set"
		}
		if key {
			return mode, "the auth mode is setup-token but " + envAPIKey + " is set"
		}
	case ModeAPIKey:
		if !key {
			return mode, "the auth mode is api-key but " + envAPIKey + " is not set"
		}
	default:
		return "", fmt.Sprintf("the auth mode must be one of login, setup-token and api-key (got %q)", mode)
	}
	if !alive {
		return mode, ""
	}
	switch mode {
	case ModeLogin:
		if status.APIKeySource != "" {
			return mode, fmt.Sprintf("the auth mode is login but claude auth status reports an API key (%s)", status.APIKeySource)
		}
		if status.AuthMethod != "claude.ai" {
			return mode, fmt.Sprintf("the auth mode is login but claude auth status reports authMethod %q", status.AuthMethod)
		}
	case ModeSetupToken:
		if status.AuthMethod != "oauth_token" {
			return mode, fmt.Sprintf("the auth mode is setup-token but claude auth status reports authMethod %q", status.AuthMethod)
		}
	case ModeAPIKey:
		if status.APIKeySource == "" {
			return mode, "the auth mode is api-key but claude auth status reports no API key"
		}
	}
	return mode, ""
}

// Auth reads what `claude auth status --json` answered (raw, or the error
// running it) against the declared auth mode: whether the credential is
// alive, and why the two disagree (empty when they agree). Logged out is
// dead too, and a dead credential is never a mismatch.
func (s *Sensor) Auth(raw []byte, statusErr error) (mode string, alive bool, reason string) {
	var status authStatus
	alive = statusErr == nil && json.Unmarshal(raw, &status) == nil && status.LoggedIn
	mode, reason = s.mode(status, alive)
	return mode, alive, reason
}

// sense is the per-mode decision.
func (s *Sensor) sense(ctx context.Context, m memory) (mode string, verdict Verdict, err error) {
	now, p := s.Now(), s.Policy
	raw, statusErr := s.AuthStatus(ctx)
	mode, alive, reason := s.Auth(raw, statusErr)
	if reason != "" {
		return mode, p.plain(now, SensorNone, StateUnavailable, false, "mode mismatch: "+reason), fmt.Errorf("%w: %s", ErrModeMismatch, reason)
	}
	if mode == ModeAPIKey {
		return mode, p.plain(now, SensorSpend, StateUnavailable, false, "api-key mode refuses to start until spend pacing is implemented (ADR 0008)"), ErrSpendPacing
	}
	if !alive {
		why := "claude auth status: not logged in; the credential is dead"
		if statusErr != nil {
			why = fmt.Sprintf("claude auth status failed (%v); the credential is dead", statusErr)
		}
		return mode, p.plain(now, SensorNone, StateAuthDead, false, why), fmt.Errorf("%w: %s", ErrAuthDead, why)
	}
	if mode == ModeSetupToken {
		return mode, s.seed(m), nil
	}

	// login: the endpoint, with its stale hold and its 403 mark.
	endpoint := m.Endpoint
	if endpoint == nil {
		endpoint = &endpointMemory{}
	}
	if endpoint.ForbiddenAt != "" && s.DaemonID != "" && endpoint.ForbiddenDaemon == s.DaemonID {
		return mode, s.seed(m, fmt.Sprintf("usage endpoint refused this Daemon with 403 at %s; running as a setup-token Host", endpoint.ForbiddenAt)), nil
	}
	code, body := s.fetch(ctx)
	switch code {
	case "200":
		verdict := p.EndpointVerdict(body, now)
		if verdict.State == StateOK {
			kept := verdict
			if err := s.remember(func(m *memory) {
				if m.Endpoint == nil {
					m.Endpoint = &endpointMemory{}
				}
				m.Endpoint.LastVerdict, m.Endpoint.LastGoodAt = &kept, iso(now)
			}); err != nil {
				verdict.Warnings = append(verdict.Warnings, "the verdict could not be remembered: "+err.Error())
			}
		}
		return mode, verdict, nil
	case "401":
		why := "usage endpoint answered 401; the login credential is dead"
		return mode, p.plain(now, SensorNone, StateAuthDead, false, why), fmt.Errorf("%w: %s", ErrAuthDead, why)
	case "403":
		warnings := []string{"usage endpoint answered 403 (the credential lacks user:profile); unavailable for this Daemon, running as a setup-token Host"}
		if err := s.remember(func(m *memory) {
			if m.Endpoint == nil {
				m.Endpoint = &endpointMemory{}
			}
			m.Endpoint.ForbiddenAt, m.Endpoint.ForbiddenDaemon = iso(now), s.DaemonID
		}); err != nil {
			warnings = append(warnings, "the 403 could not be remembered: "+err.Error())
		}
		return mode, s.seed(m, warnings...), nil
	case noCredential:
		why := "no login credential in " + s.credentialsFile()
		return mode, p.plain(now, SensorNone, StateAuthDead, false, why), fmt.Errorf("%w: %s", ErrAuthDead, why)
	}
	// 429, 5xx or no answer: hold the last good verdict for a while.
	if lastGood, err := time.Parse(time.RFC3339, endpoint.LastGoodAt); err == nil && endpoint.LastVerdict != nil && now.Sub(lastGood) <= p.StaleMax {
		held := *endpoint.LastVerdict
		held.State = StateStale
		held.Warnings = append(append([]string{}, held.Warnings...), fmt.Sprintf("usage endpoint answered %s; holding the verdict observed at %s", code, endpoint.LastGoodAt))
		return mode, held, nil
	}
	return mode, s.seed(m, fmt.Sprintf("usage endpoint answered %s and the last good verdict is older than %ds; running as a setup-token Host", code, int(p.StaleMax.Seconds()))), nil
}

// seed is the verdict of a Host with no endpoint to ask: the last tapped
// rate-limit event, or the last Step's limit-string outcome when that is not
// older. With neither, the Step goes optimistically.
func (s *Sensor) seed(m memory, warnings ...string) Verdict {
	now, p := s.Now(), s.Policy
	var record Record
	haveEvent := readJSON(filepath.Join(s.StateDir, rateLimitsFile), &record)
	prepend := func(v Verdict) Verdict {
		v.Warnings = append(append([]string{}, warnings...), v.Warnings...)
		return v
	}
	// An exhaustion read from a rejected event is that event's to report:
	// the record knows which model's window it was. Only one read from a
	// limit string has anything to add.
	if last := m.LastStep; last != nil && last.Report.Outcome == step.Exhausted && last.Report.Source == SensorLimitStrings &&
		(!haveEvent || last.EndedAt >= record.ObservedAt) {
		// An outcome whose reset has passed says nothing: fall back to the
		// event.
		if verdict := p.ReportVerdict(last.Report, now); verdict.Sensor != SensorNone || !haveEvent {
			return prepend(verdict)
		}
	}
	if haveEvent {
		return prepend(p.EventVerdict(record, now))
	}
	return p.plain(now, SensorNone, StateUnavailable, true, append([]string{"no sensor has spoken yet; going optimistically"}, warnings...)...)
}

// noCredential stands in for an HTTP status when there is no login
// credential to ask the endpoint with.
const noCredential = "nocreds"

func (s *Sensor) credentialsFile() string {
	dir := s.env("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(s.env("HOME"), ".claude")
	}
	return filepath.Join(dir, ".credentials.json")
}

// fetch asks the usage endpoint with the login credential and returns the
// HTTP status as text ("000" when there was no answer) and the body.
func (s *Sensor) fetch(ctx context.Context) (code string, body []byte) {
	var credentials struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if !readJSON(s.credentialsFile(), &credentials) || credentials.OAuth.AccessToken == "" {
		return noCredential, nil
	}
	url := s.UsageURL
	if url == "" {
		url = DefaultUsageURL
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "000", nil
	}
	request.Header.Set("Authorization", "Bearer "+credentials.OAuth.AccessToken)
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	response, err := client.Do(request)
	if err != nil {
		return "000", nil
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "000", nil
	}
	return fmt.Sprintf("%03d", response.StatusCode), body
}

// Observe records what a Step's run says about the budget: every rate-limit
// event of its stream, and how it ended. They seed the next verdict.
func (s *Sensor) Observe(fireID string, run step.Run) error {
	now := s.Now()
	var records []Record
	for _, event := range run.RateLimits {
		if record, ok := RecordOf(event, fireID, run.Model, now); ok {
			records = append(records, record)
		}
	}
	// Each is kept whatever happens to the other: losing a limit's reset
	// because a log line could not be written would let the next Step go.
	return errors.Join(
		saveRecords(s.StateDir, records),
		s.remember(func(m *memory) { m.LastStep = &lastStep{Report: ReportOf(run, now), EndedAt: iso(now)} }),
	)
}
