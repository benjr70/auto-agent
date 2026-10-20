package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/daemon"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/state"
)

// RefreshEvery is how often a live section asks its source again: the
// Budget gate, GitHub and the pick. Between refreshes a page reload is
// answered from memory.
const RefreshEvery = time.Minute

// UnitRefreshEvery is how often systemd is asked about the Daemon's unit.
const UnitRefreshEvery = 15 * time.Second

// fetchTimeout bounds one refresh, so that a source that hangs makes its
// section stale instead of holding the route.
const fetchTimeout = 30 * time.Second

// Gate is the Budget gate (budget.Sensor).
type Gate interface {
	Verdict(ctx context.Context) (budget.Verdict, error)
}

// Daemon is what the always-on Daemon is doing (daemon.Status).
type Daemon interface {
	Now() daemon.State
	Tail() []string
}

// GitHub is the reads the open PRs and their Ruling requests take
// (github.Client, whose ETag cache makes a quiet repo's refresh 304s).
type GitHub interface {
	OpenPullRequests(ctx context.Context, owner, repo string) ([]github.PullRequest, error)
	PullRequest(ctx context.Context, owner, repo string, number int) (github.PullRequest, error)
	Comments(ctx context.Context, owner, repo string, number int) ([]github.Comment, error)
}

// cache is one live section's last good value, and how its last refresh
// went. A failed refresh keeps the value, and the section says so.
type cache[T any] struct {
	mu    sync.Mutex
	asked time.Time
	good  *T
	asOf  string
	err   error
}

// read is the section's value, refreshed first when every has passed since
// it was last asked, whether that ask failed or not. It is nil until a
// refresh has been good.
//
// The refresh is the section's, not the request's: it runs detached from
// ctx's cancellation, bounded by fetchTimeout alone, so that a client gone
// mid-refresh is never cached as the source's answer (a killed `claude auth
// status` would otherwise read as a dead credential for a full refresh).
func (c *cache[T]) read(ctx context.Context, now time.Time, every time.Duration, fetch func(context.Context) (T, error)) (*T, Section) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.asked.IsZero() || now.Sub(c.asked) >= every {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		value, err := fetch(ctx)
		cancel()
		c.asked, c.err = now, err
		if err == nil {
			c.good, c.asOf = &value, now.Format(time.RFC3339)
		}
	}
	section := Section{Stale: c.err != nil}
	if c.good != nil {
		asOf := c.asOf
		section.AsOf = &asOf
	}
	if c.err != nil {
		section.Error = textOrNull(c.err.Error())
	}
	return c.good, section
}

// gateRead is one answer of the Budget gate.
type gateRead struct {
	verdict budget.Verdict
	// refusal is the gate's reason the Step must not run whatever the
	// budget; nil when it gave none.
	refusal error
}

// refusals are the gate's own answers that no Step may run; any other
// error is a gate that could not answer.
var refusals = []error{budget.ErrAuthDead, budget.ErrModeMismatch, budget.ErrSpendPacing}

func (s *Server) askGate(ctx context.Context) (gateRead, error) {
	verdict, err := s.Gate.Verdict(ctx)
	for _, refusal := range refusals {
		if errors.Is(err, refusal) {
			return gateRead{verdict: verdict, refusal: err}, nil
		}
	}
	if err != nil {
		return gateRead{}, fmt.Errorf("the Budget gate gave no verdict: %w", err)
	}
	return gateRead{verdict: verdict}, nil
}

// rc is the gate's answer as the usage sensor's exit code: 0, or 4, 5 or 6
// for a dead credential, a mismatched auth mode, and spend pacing.
func rc(refusal error) int {
	switch {
	case errors.Is(refusal, budget.ErrAuthDead):
		return 4
	case errors.Is(refusal, budget.ErrModeMismatch):
		return 5
	case errors.Is(refusal, budget.ErrSpendPacing):
		return 6
	}
	return 0
}

// budgetSection is the Budget gate's verdict, verbatim, with the line the
// tile shows beside it.
func (s *Server) budgetSection(ctx context.Context, now time.Time) Budget {
	b := Budget{Tiles: []Tile{}}
	if s.Gate == nil {
		return b
	}
	read, section := s.gate.read(ctx, now, RefreshEvery, s.askGate)
	b.Section = section
	var verdict *budget.Verdict
	var refusal error
	if read != nil {
		verdict, refusal = &read.verdict, read.refusal
		code := rc(refusal)
		b.RC, b.Verdict = &code, verdict
		b.Tiles = tiles(read.verdict)
	}
	mode := s.AuthMode
	if verdict != nil && verdict.AuthMode != "" {
		mode = string(verdict.AuthMode)
	}
	b.AuthMode = textOrNull(mode)
	b.Message = message(verdict, refusal, s.AuthMode)
	return b
}

// limitLabels name the account-wide limits; every other scope is a model's
// weekly limit.
var limitLabels = map[string]string{budget.ScopeSession: "Session", budget.ScopeWeekly: "Weekly · all models"}

// tiles are the verdict's limits, one tile each.
func tiles(v budget.Verdict) []Tile {
	out := []Tile{}
	for _, limit := range v.Limits {
		label, ok := limitLabels[limit.Scope]
		if !ok {
			label = "Weekly · " + limit.Scope
		}
		out = append(out, Tile{Key: limit.Scope, Label: label, Percent: limit.Utilization, ResetsAt: textOrNull(string(limit.ResetsAt))})
	}
	return out
}

// message is the one line the budget tile shows beside the verdict, per
// auth mode and the gate's refusal; nil when the gate spoke normally and
// there is nothing to add. declared is the Host config's auth mode, which
// speaks when the verdict does not name one.
func message(v *budget.Verdict, refusal error, declared string) *string {
	mode, sensor, verdictState := declared, "", ""
	if v != nil {
		if v.AuthMode != "" {
			mode = string(v.AuthMode)
		}
		sensor, verdictState = v.Sensor, v.State
	}
	var m string
	switch {
	case errors.Is(refusal, budget.ErrAuthDead) || verdictState == budget.StateAuthDead:
		m = "credential dead — the Daemon is parked until the login is renewed"
	case errors.Is(refusal, budget.ErrModeMismatch):
		m = "auth mode mismatch — CLAUDE_AUTH_MODE contradicts the credential; no Fire until fixed"
	case errors.Is(refusal, budget.ErrSpendPacing) || mode == budget.ModeAPIKey:
		m = "api-key auth mode has no spend pacing yet — the Daemon refuses to start"
	case mode == budget.ModeSetupToken:
		m = "no usage sensor in this auth mode"
	case mode == budget.ModeLogin && sensor != "" && sensor != budget.SensorEndpoint:
		m = "usage endpoint unavailable — the verdict comes from the last Step instead"
	case mode == budget.ModeLogin && verdictState == budget.StateStale:
		m = "usage endpoint unreachable — holding the last good reading"
	case mode == budget.ModeLogin:
		return nil
	default:
		m = "no auth mode declared in the Host config (CLAUDE_AUTH_MODE)"
	}
	return &m
}

// daemonSection is what the Daemon is doing, from the run process's memory,
// with its unit as systemd has it.
func (s *Server) daemonSection(ctx context.Context, now time.Time) DaemonSection {
	d := DaemonSection{State: "unknown", Tail: []string{}}
	if s.Unit != nil {
		unit, section := s.unit.read(ctx, now, UnitRefreshEvery, func(ctx context.Context) (*Unit, error) {
			out, err := s.Unit(ctx)
			if err != nil {
				return nil, fmt.Errorf("systemctl --user show: %w", err)
			}
			return parseUnit(out), nil
		})
		d.Section = section
		if unit != nil {
			d.Unit = *unit
		}
	}
	if s.Daemon == nil {
		return d
	}
	if tail := s.Daemon.Tail(); tail != nil {
		d.Tail = tail
	}
	current := s.Daemon.Now()
	if current.State == "" {
		return d
	}
	d.State, d.StateDetail = current.State, textOrNull(current.Detail)
	if !current.At.IsZero() {
		at := current.At.UTC().Format(time.RFC3339)
		d.StateAt = &at
	}
	d.ResetAt = textOrNull(current.ResetAt)
	if current.Fails > 0 {
		d.Fail = &Fail{Count: current.Fails}
	}
	if p := current.Parked; p != nil {
		d.Parked = &Parked{Issue: p.Issue, Reason: p.Reason, ProbeAt: p.ProbeAt.UTC().Format(time.RFC3339)}
	}
	return d
}

// parseUnit reads `systemctl show`'s properties; nil when the unit is not
// installed.
func parseUnit(out []byte) *Unit {
	props := map[string]string{}
	lines := bufio.NewScanner(bytes.NewReader(out))
	for lines.Scan() {
		if key, value, ok := strings.Cut(lines.Text(), "="); ok {
			props[key] = value
		}
	}
	if load := props["LoadState"]; load == "" || load == "not-found" {
		return nil
	}
	unit := &Unit{Active: props["ActiveState"], Since: textOrNull(props["ActiveEnterTimestamp"])}
	if unit.Active == "" {
		unit.Active = "unknown"
	}
	if pid, err := strconv.Atoi(props["MainPID"]); err == nil && pid != 0 {
		unit.MainPID = &pid
	}
	return unit
}

// prsRead is one good read of the open PRs.
type prsRead struct {
	items   []PRItem
	parked  int
	rulings []Ruling
}

// readPRs lists the open PRs, reads each alone for whether it merges, and
// reads the Ruling request of each labelled for one.
func (s *Server) readPRs(ctx context.Context) (prsRead, error) {
	owner, name, _ := strings.Cut(s.Host.Repo, "/")
	listed, err := s.GitHub.OpenPullRequests(ctx, owner, name)
	if err != nil {
		return prsRead{}, fmt.Errorf("listing the open PRs: %w", err)
	}
	read := prsRead{items: []PRItem{}, rulings: []Ruling{}}
	for _, pr := range listed {
		mergeable := pr.Mergeable
		// A listing does not say whether a PR merges; one read alone does.
		// One that cannot be read stays unknown.
		if alone, err := s.GitHub.PullRequest(ctx, owner, name, pr.Number); err == nil {
			mergeable = alone.Mergeable
		}
		read.items = append(read.items, PRItem{
			Number: pr.Number, Title: pr.Title, URL: textOrNull(pr.URL), Branch: pr.HeadRef, Labels: pr.Labels,
			Mergeable: string(mergeable), IsDraft: pr.Draft, DocsOnly: state.IsResearch(pr.HeadRef),
		})
		// A PR waiting on a Ruling needs one line from the human, not a
		// repair: it is listed under rulings and never counted as parked,
		// whatever else it carries.
		if slices.Contains(pr.Labels, state.LabelRuling) {
			if r := s.pendingRuling(ctx, owner, name, pr); r != nil {
				read.rulings = append(read.rulings, *r)
			}
			continue
		}
		// What parks a PR is PR Triage's rule, asked; no copy of it lives
		// here (ADR 0006).
		if state.Ours(pr, s.Login) && state.Parked(pr) {
			read.parked++
		}
	}
	return read, nil
}

// pendingRuling is the Ruling request pr waits on, or nil when none is
// outstanding. A request that cannot be read is still listed: the label
// says the PR waits, its count unknown and its link the PR itself.
func (s *Server) pendingRuling(ctx context.Context, owner, name string, pr github.PullRequest) *Ruling {
	unread := &Ruling{PR: pr.Number, URL: textOrNull(pr.URL)}
	comments, err := s.GitHub.Comments(ctx, owner, name, pr.Number)
	if err != nil {
		return unread
	}
	pending, err := ruling.Read(comments, s.Login)
	if err != nil {
		return unread
	}
	if pending.Request == nil {
		return nil
	}
	n := len(pending.Request.Decisions)
	r := &Ruling{PR: pr.Number, Decisions: &n, URL: unread.URL}
	if pr.URL != "" {
		r.URL = textOrNull(fmt.Sprintf("%s#issuecomment-%d", pr.URL, pending.Request.Comment))
	}
	return r
}

// openPRs is the open PRs section and the rulings that ride its refresh.
func (s *Server) openPRs(ctx context.Context, now time.Time) (OpenPRs, []Ruling) {
	section, rulings := OpenPRs{Items: []PRItem{}}, []Ruling{}
	if s.GitHub == nil {
		return section, rulings
	}
	read, sec := s.prs.read(ctx, now, RefreshEvery, s.readPRs)
	section.Section = sec
	if read != nil {
		parked := read.parked
		// Unknown is not zero: null until the rule could be asked.
		section.Items, section.Parked, rulings = read.items, &parked, read.rulings
	}
	return section, rulings
}

// pipeline is the pick's queue: what the next Fire would take, from the
// derivation the Fire itself runs.
func (s *Server) pipeline(ctx context.Context, now time.Time) Pipeline {
	if s.Scan == nil {
		return Pipeline{}
	}
	scan, section := s.scan.read(ctx, now, RefreshEvery, s.Scan)
	p := Pipeline{Section: section}
	if scan != nil {
		p.Scan = *scan
	}
	return p
}
