// Package dashboard serves the Dashboard: the read-only status page one Host
// serves for its own Daemon (ADR 0006). Its JSON seam, /api/status, is
// built from the Fire records in the State dir and re-implements nothing
// the Daemon owns. The shape is documented in README.md beside this file,
// and the tests check a live response against it.
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
)

// FiresShown is how many Fires the history lists.
const FiresShown = 10

// recordsRead is how many records a refresh reads: the history, and a few
// more for the newest finished one when the newest are in flight.
const recordsRead = FiresShown + 2

// InFlightMax is how old a record with no end may be and still be the Fire
// in flight: a crashed Fire leaves one behind, and past this age it is
// history.
const InFlightMax = 3 * time.Hour

// Host is what the Dashboard says about the Host it runs on.
type Host struct {
	Bind     string
	Port     int
	StateDir string
	// Target is the Target Project's checkout; empty when there is none.
	Target string
	// Repo is the Target Project, as owner/name.
	Repo string
}

// Server builds /api/status and serves it with the page.
type Server struct {
	Host Host
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu sync.Mutex
	// good is the fires section's last good refresh.
	good *firesRead
}

// firesRead is one good read of the Fire records.
type firesRead struct {
	records    []journal.Record
	unreadable []string
	asOf       string
}

func (s *Server) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// Handler is the Dashboard's routes: the page and /api/status, read-only.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html", "/api/status", "/favicon.ico":
		default:
			http.NotFound(w, r)
			return
		}
		// There is no route that changes anything.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		switch r.URL.Path {
		case "/", "/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, placeholderPage)
		case "/api/status":
			body, err := json.Marshal(s.Status())
			if err != nil {
				// Never a 500: the shape is all plain values, so this is
				// unreachable, but the page still gets an answer.
				body = []byte(`{"error":` + strconv.Quote(err.Error()) + `}`)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		case "/favicon.ico":
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// placeholderPage is the page until the page Slice lands.
const placeholderPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>auto-agent</title></head>
<body>
<h1>auto-agent</h1>
<p>The page is on its way. The status is at <a href="/api/status">/api/status</a>.</p>
</body>
</html>
`

// Listen opens the Dashboard's address. An error is an address it cannot
// have, most often one another process holds.
func Listen(host Host) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort(host.Bind, strconv.Itoa(host.Port)))
}

// Serve serves the Dashboard on ln until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	})
	defer stop()
	if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Status is /api/status now.
func (s *Server) Status() Status {
	now := s.now()
	asOf := now.Format(time.RFC3339)
	read, readErr := s.fires(asOf)
	status := Status{
		GeneratedAt: asOf,
		Host: HostSection{
			Bind: s.Host.Bind, Port: s.Host.Port, StateDir: s.Host.StateDir,
			Target: textOrNull(s.Host.Target), Repo: textOrNull(s.Host.Repo),
		},
		Budget: Budget{Tiles: []any{}},
		Daemon: Daemon{
			Unit: Unit{Active: "unknown"}, State: "unknown", Tail: []string{},
		},
		Pipeline:    Pipeline{},
		OpenPRs:     OpenPRs{Items: []any{}},
		Rulings:     []any{},
		Maps:        Maps{Items: []any{}},
		Wayfinder:   Wayfinder{Unknown: true},
		FireSummary: FireSummary{},
	}
	fires := Fires{Items: []Fire{}, Unreadable: []string{}}
	if read != nil {
		fires.Unreadable = read.unreadable
		fires.AsOf = &read.asOf
		for i, record := range read.records {
			if i == FiresShown {
				break
			}
			fires.Items = append(fires.Items, fireItem(record, now, i == 0))
		}
		if len(fires.Items) > 0 && fires.Items[0].InFlight {
			fires.Current = &fires.Items[0]
		}
		status.Bootstrap = bootstrap(read.records)
		status.Budget.LastFire = lastFire(read.records)
		status.FireSummary = fireSummary(fires.Current, read.records)
	} else {
		status.Bootstrap = Bootstrap{Notes: []string{}}
	}
	if readErr != nil {
		fires.Stale, fires.Error = true, textOrNull(readErr.Error())
	}
	status.Fires = fires
	return status
}

// fires reads the Fire records: the last good read, flagged by the error,
// when this one fails. It is nil when no read has ever been good.
func (s *Server) fires(asOf string) (*firesRead, error) {
	records, unreadable, err := readRecords(s.Host.StateDir)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		return s.good, err
	}
	s.good = &firesRead{records: records, unreadable: unreadable, asOf: asOf}
	return s.good, nil
}

// readRecords reads the newest Fire records, newest first, with the files
// among them that would not parse. A State dir with no Fire yet has none.
func readRecords(stateDir string) ([]journal.Record, []string, error) {
	dir := filepath.Join(stateDir, "fires")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []journal.Record{}, []string{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	// A Fire id starts with the second the Fire started.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	records, unreadable := []journal.Record{}, []string{}
	for _, name := range names {
		if len(records) == recordsRead {
			break
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		var record journal.Record
		if err != nil || json.Unmarshal(data, &record) != nil || record.FireID == "" {
			unreadable = append(unreadable, name)
			continue
		}
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].StartedAt.After(records[j].StartedAt)
		}
		return records[i].FireID > records[j].FireID
	})
	sort.Strings(unreadable)
	return records, unreadable, nil
}

// fireItem is how the history reads one record: what the Fire did. Only the
// newest record can be in flight: the Single-flight lock allows one Fire per
// State dir, so a record with no end and a newer one after it crashed.
func fireItem(r journal.Record, now time.Time, newest bool) Fire {
	item := Fire{
		ID: r.FireID, Kind: textOrNull(r.Kind), Badge: badge(r),
		StartedAt: r.StartedAt.UTC().Format(time.RFC3339), Exit: r.Exit,
		Phase: textOrNull(r.Phase), Issue: r.Issue, PR: r.PR,
		NoWork: r.Action == string(state.ActionNone), ResetAt: textOrNull(r.ResetAt),
		Notes: r.Warnings, Steps: []StepItem{},
	}
	if r.EndedAt != nil {
		ended := r.EndedAt.UTC().Format(time.RFC3339)
		item.EndedAt = &ended
	} else {
		item.InFlight = newest && now.Sub(r.StartedAt) < InFlightMax
	}
	if item.Notes == nil {
		item.Notes = []string{}
	}
	if r.Kind != "" {
		line := r.Kind + ": " + what(r)
		item.Line = &line
	}
	item.Summary = summary(r, item.InFlight)
	item.Outcome = outcome(r)
	for _, s := range r.Steps {
		item.Steps = append(item.Steps, stepItem(s))
		if s.Outcome == "" {
			continue
		}
		cost := s.CostUSD
		if item.CostUSD != nil {
			cost += *item.CostUSD
		}
		item.CostUSD = &cost
		if s.Model != "" {
			model := s.Model
			item.Model = &model
		}
	}
	return item
}

// badge is what the Fire was: slice, reconcile, resolve, noop, or null
// when it found no work.
func badge(r journal.Record) *string {
	var b string
	switch {
	case r.Action == "" || r.Action == string(state.ActionNone):
		return nil
	case r.Kind == string(state.KindPick) || r.Kind == string(state.KindResume):
		b = "slice"
	case r.Kind == string(state.KindReconcile):
		b = "reconcile"
	case r.Kind == string(state.KindPickDecision):
		b = "resolve"
	default:
		b = "noop"
	}
	return &b
}

// what is what the Fire set out to do, on what: "implement #36 · PR #55".
func what(r journal.Record) string {
	parts := []string{r.Action}
	if r.Action == "" {
		parts[0] = "nothing yet"
	}
	if r.Issue != nil {
		parts[0] += " #" + strconv.Itoa(*r.Issue)
	}
	if r.PR != nil {
		parts = append(parts, "PR #"+strconv.Itoa(*r.PR))
	}
	if r.Reconcile != "" {
		parts = append(parts, r.Reconcile)
	}
	return strings.Join(parts, " · ")
}

// summary is the one line the history row reads as.
func summary(r journal.Record, inFlight bool) string {
	exit := exitOf(r)
	switch {
	case inFlight:
		return "in flight"
	case r.EndedAt == nil:
		return "crashed — no end recorded"
	case r.Phase == "preflight":
		return fmt.Sprintf("preflight failed — exit %d", exit)
	case r.Park != nil:
		return "credential dead — Daemon parked · " + what(r)
	case exhausted(r, exit):
		return "paused " + what(r) + " (usage exhausted)"
	case exit != 0:
		return fmt.Sprintf("FAILED — exit %d · %s", exit, what(r))
	case r.Action == string(state.ActionNone) && r.Kind == string(state.KindIdle):
		return "no work — queue empty"
	case r.Action == string(state.ActionNone):
		return "no work — " + r.Kind
	case r.NotWorked != "":
		return "not worked — " + r.NotWorked
	}
	return what(r)
}

// exhausted is whether the Fire stopped on the usage limit: paused, or told
// by the Budget gate to wait before a Step, which leaves only resetAt behind.
func exhausted(r journal.Record, exit int) bool {
	return r.Phase == "paused" || (r.ResetAt != "" && exit == 0)
}

func exitOf(r journal.Record) int {
	if r.Exit == nil {
		return 0
	}
	return *r.Exit
}

// outcome is how the Fire ended: OK, FAILED, EXHAUSTED (paused on the
// usage limit) or AUTH_DEAD (parked); null while it has not.
func outcome(r journal.Record) *string {
	var o step.Outcome
	switch {
	case r.EndedAt == nil:
		return nil
	case r.Park != nil:
		o = step.AuthDead
	case exhausted(r, exitOf(r)):
		o = step.Exhausted
	case r.Exit != nil && *r.Exit != 0:
		o = step.Failed
	default:
		o = step.OK
	}
	text := string(o)
	return &text
}

// stepItem is one Step of a Fire, with the tokens of its own usage.
func stepItem(s journal.Step) StepItem {
	item := StepItem{
		Name: s.Name, Model: textOrNull(s.Model), Outcome: textOrNull(string(s.Outcome)),
		StartedAt: s.StartedAt.UTC().Format(time.RFC3339), ElapsedMS: s.ElapsedMS,
		Turns: s.NumTurns, CostUSD: s.CostUSD,
	}
	var usage struct {
		Input         *int64 `json:"input_tokens"`
		Output        *int64 `json:"output_tokens"`
		CacheRead     *int64 `json:"cache_read_input_tokens"`
		CacheCreation *int64 `json:"cache_creation_input_tokens"`
	}
	if len(s.Usage) > 0 && json.Unmarshal(s.Usage, &usage) == nil {
		item.InputTokens, item.OutputTokens = usage.Input, usage.Output
		item.CacheReadTokens, item.CacheCreationTokens = usage.CacheRead, usage.CacheCreation
	}
	return item
}

// lastFinished is the newest record of a Fire that ended.
func lastFinished(records []journal.Record) *journal.Record {
	for i := range records {
		if records[i].EndedAt != nil {
			return &records[i]
		}
	}
	return nil
}

// bootstrap is the warning the newest finished Fire that ran a
// Verification round recorded, with its notes.
func bootstrap(records []journal.Record) Bootstrap {
	for _, r := range records {
		if r.EndedAt == nil || r.Verify == nil {
			continue
		}
		warning, id := r.Verify.Bootstrap, r.FireID
		notes := r.Warnings
		if notes == nil {
			notes = []string{}
		}
		return Bootstrap{Warning: &warning, FireID: &id, Notes: notes}
	}
	return Bootstrap{Notes: []string{}}
}

// lastFire is the newest finished Fire, with the last Gate verdict it was
// given.
func lastFire(records []journal.Record) *LastFire {
	r := lastFinished(records)
	if r == nil {
		return nil
	}
	ended := r.EndedAt.UTC().Format(time.RFC3339)
	last := &LastFire{FireID: r.FireID, EndedAt: &ended}
	for i := len(r.Steps) - 1; i >= 0; i-- {
		if r.Steps[i].Gate != nil {
			last.Gate = r.Steps[i].Gate
			break
		}
	}
	return last
}

// fireSummary names the Fire in flight and the Step it is on. No model is
// asked, so it is never enabled.
func fireSummary(current *Fire, records []journal.Record) FireSummary {
	if current == nil {
		return FireSummary{}
	}
	id := current.ID
	summary := FireSummary{ForFire: &id}
	title := "Fire " + id
	for _, r := range records {
		if r.FireID != id {
			continue
		}
		if r.Kind != "" {
			title = r.Kind + ": " + what(r)
		}
		if n := len(r.Steps); n > 0 {
			title += " — " + r.Steps[n-1].Name
		}
		if r.Issue != nil {
			issue := "issue #" + strconv.Itoa(*r.Issue)
			summary.Issue = &issue
		}
	}
	summary.Title = &title
	return summary
}

func textOrNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Status is /api/status. README.md documents it.
type Status struct {
	GeneratedAt string      `json:"generatedAt"`
	Host        HostSection `json:"host"`
	Budget      Budget      `json:"budget"`
	Daemon      Daemon      `json:"daemon"`
	Fires       Fires       `json:"fires"`
	Bootstrap   Bootstrap   `json:"bootstrap"`
	Pipeline    Pipeline    `json:"pipeline"`
	OpenPRs     OpenPRs     `json:"openPrs"`
	Rulings     []any       `json:"rulings"`
	Maps        Maps        `json:"maps"`
	Wayfinder   Wayfinder   `json:"wayfinder"`
	FireSummary FireSummary `json:"fireSummary"`
}

// Section is what every refreshed section carries: when its value was
// read, and whether the last refresh failed, keeping that value.
type Section struct {
	AsOf  *string `json:"asOf"`
	Stale bool    `json:"stale"`
	Error *string `json:"error"`
}

type HostSection struct {
	Bind     string  `json:"bind"`
	Port     int     `json:"port"`
	StateDir string  `json:"stateDir"`
	Target   *string `json:"target"`
	Repo     *string `json:"repo"`
	// SummaryEnabled is always false: no model writes a summary.
	SummaryEnabled bool `json:"summaryEnabled"`
}

type Budget struct {
	RC       *int      `json:"rc"`
	Verdict  any       `json:"verdict"`
	AuthMode *string   `json:"authMode"`
	Message  *string   `json:"message"`
	Tiles    []any     `json:"tiles"`
	LastFire *LastFire `json:"lastFire"`
	Section
}

type LastFire struct {
	FireID  string          `json:"fireId"`
	EndedAt *string         `json:"endedAt"`
	Gate    *budget.Verdict `json:"gate"`
}

type Daemon struct {
	Unit        Unit     `json:"unit"`
	State       string   `json:"state"`
	StateDetail *string  `json:"stateDetail"`
	StateAt     *string  `json:"stateAt"`
	ResetAt     *string  `json:"resetAt"`
	Fail        any      `json:"fail"`
	Parked      any      `json:"parked"`
	Tail        []string `json:"tail"`
	TailError   *string  `json:"tailError"`
	Section
}

type Unit struct {
	Active  string  `json:"active"`
	MainPID *int    `json:"mainPid"`
	Since   *string `json:"since"`
}

type Fires struct {
	Items      []Fire   `json:"items"`
	Current    *Fire    `json:"current"`
	Unreadable []string `json:"unreadable"`
	Section
}

// Fire is one Fire of the history.
type Fire struct {
	ID        string     `json:"id"`
	Kind      *string    `json:"kind"`
	Badge     *string    `json:"badge"`
	StartedAt string     `json:"startedAt"`
	EndedAt   *string    `json:"endedAt"`
	InFlight  bool       `json:"inFlight"`
	Exit      *int       `json:"exit"`
	Phase     *string    `json:"phase"`
	Summary   string     `json:"summary"`
	Issue     *int       `json:"issue"`
	PR        *int       `json:"pr"`
	NoWork    bool       `json:"noWork"`
	Outcome   *string    `json:"outcome"`
	ResetAt   *string    `json:"resetAt"`
	CostUSD   *float64   `json:"costUsd"`
	Model     *string    `json:"model"`
	Line      *string    `json:"line"`
	Notes     []string   `json:"notes"`
	Steps     []StepItem `json:"steps"`
}

// StepItem is one Step of a Fire, as its own record has it.
type StepItem struct {
	Name      string  `json:"name"`
	Model     *string `json:"model"`
	Outcome   *string `json:"outcome"`
	StartedAt string  `json:"startedAt"`
	ElapsedMS int64   `json:"elapsedMs"`
	Turns     int     `json:"turns"`
	CostUSD   float64 `json:"costUsd"`
	// The tokens are null when the Step's call reported no usage.
	InputTokens         *int64 `json:"inputTokens"`
	OutputTokens        *int64 `json:"outputTokens"`
	CacheReadTokens     *int64 `json:"cacheReadTokens"`
	CacheCreationTokens *int64 `json:"cacheCreationTokens"`
}

type Bootstrap struct {
	Warning *bool    `json:"warning"`
	FireID  *string  `json:"fireId"`
	Notes   []string `json:"notes"`
}

type Pipeline struct {
	Scan any `json:"scan"`
	Section
}

type OpenPRs struct {
	Items  []any `json:"items"`
	Parked *int  `json:"parked"`
	Section
}

type Maps struct {
	Items     []any `json:"items"`
	Total     *int  `json:"total"`
	Truncated bool  `json:"truncated"`
	Section
}

type Wayfinder struct {
	Maps           *int `json:"maps"`
	Frontier       *int `json:"frontier"`
	AFK            *int `json:"afk"`
	Unknown        bool `json:"unknown"`
	Truncated      bool `json:"truncated"`
	QueueSlices    *int `json:"queueSlices"`
	QueueWayfinder *int `json:"queueWayfinder"`
}

type FireSummary struct {
	Enabled     bool    `json:"enabled"`
	ForFire     *string `json:"forFire"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Issue       *string `json:"issue"`
	Section
}
