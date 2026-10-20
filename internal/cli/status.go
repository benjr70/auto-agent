package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/dashboard"
)

// statusTimeout is how long status waits on the Daemon's Dashboard.
const statusTimeout = 10 * time.Second

// firesPrinted is how many of the last Fires status prints.
const firesPrinted = 5

// statusCmd prints what the Dashboard shows, from the running Daemon's
// /api/status; with no Daemon answering, the Fire history from the State
// dir, built by the code the route uses, and exit 1.
func statusCmd(args []string, env Env) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "")
	repoFlag := flags.String("repo", "", "")
	var positional []string
	for rest := args; ; {
		err := flags.Parse(rest)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return exitOK
		}
		if err != nil {
			return usageError(env.Stderr, "status: "+err.Error())
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(positional) > 1 {
		return usageError(env.Stderr, "status takes at most one target dir")
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "auto-agent: status: %v\n", err)
		return exitFailed
	}

	slug := *repoFlag
	if slug == "" {
		targetDir := "."
		if len(positional) == 1 {
			targetDir = positional[0]
		}
		origin, err := env.OriginURL(targetDir)
		if err != nil {
			return fail(err)
		}
		slug = origin
	}
	owner, name, err := config.ParseRepo(slug)
	if err != nil {
		return fail(err)
	}
	// The Host config is keyed by the name GitHub gives the repo now, as
	// run keys it; a GitHub that cannot be asked leaves the name as given,
	// so the history on disk can still be read.
	repo := config.Repo{Owner: owner, Name: name}
	ghCtx, ghCancel := context.WithTimeout(context.Background(), statusTimeout)
	s, err := openSession(ghCtx, env, "", slug)
	ghCancel()
	if err == nil {
		repo = config.Repo{Owner: s.repo.Owner, Name: s.repo.Name}
	} else {
		fmt.Fprintf(env.Stderr, "auto-agent: status: cannot ask GitHub for the repo's current name (%v); using %s as given\n", err, repo.Slug())
	}
	host, err := config.LoadHost(repo, env.LookupEnv)
	if err != nil {
		return fail(err)
	}

	url := statusURL(host.Dashboard)
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	body, err := fetchStatus(ctx, url)
	if err != nil {
		fmt.Fprintf(env.Stderr, "auto-agent: status: no Daemon answers at %s: %v\n", url, err)
		// The history is on disk; the Dashboard's own code reads it.
		server := &dashboard.Server{Host: dashboard.Host{StateDir: host.StateDir}, Now: env.Now}
		fires := server.Status(context.Background()).Fires
		fmt.Fprintf(env.Stdout, "Fires (from %s/fires):\n", host.StateDir)
		printFires(env.Stdout, fires)
		return exitFailed
	}
	if *asJSON {
		env.Stdout.Write(body)
		return exitOK
	}
	var status dashboard.Status
	if err := json.Unmarshal(body, &status); err != nil {
		return fail(fmt.Errorf("%s did not answer with the Dashboard's JSON: %w", url, err))
	}
	printStatus(env.Stdout, status)
	return exitOK
}

// statusURL is the Dashboard's route at the Host config's address; an
// address bound on every interface is asked on loopback.
func statusURL(d config.Dashboard) string {
	bind := d.Bind
	if ip := net.ParseIP(bind); ip != nil && ip.IsUnspecified() {
		bind = "127.0.0.1"
		if ip.To4() == nil {
			bind = "::1"
		}
	}
	return "http://" + net.JoinHostPort(bind, strconv.Itoa(d.Port)) + "/api/status"
}

// fetchStatus is the route's body; an error is no Daemon answering it.
func fetchStatus(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("it answered %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// printStatus is the Dashboard as short plain-text sections.
func printStatus(w io.Writer, s dashboard.Status) {
	d := s.Daemon
	line := "Daemon: " + d.State
	if d.StateDetail != nil {
		line += " — " + *d.StateDetail
	}
	fmt.Fprintln(w, line)
	printSection(w, d.Section)
	if d.ResetAt != nil {
		fmt.Fprintf(w, "  reset at %s\n", *d.ResetAt)
	}
	if d.Fail != nil && d.Fail.Count > 0 {
		noun := "Fires"
		if d.Fail.Count == 1 {
			noun = "Fire"
		}
		fmt.Fprintf(w, "  %d %s in a row failed\n", d.Fail.Count, noun)
	}
	if p := d.Parked; p != nil {
		line := "  parked: " + p.Reason
		if p.Issue != nil {
			line += fmt.Sprintf(" (issue #%d)", *p.Issue)
		}
		fmt.Fprintf(w, "%s, next probe %s\n", line, p.ProbeAt)
	}
	if u := d.Unit; u != nil {
		line := "  unit " + u.Active
		if u.MainPID != nil && *u.MainPID != 0 {
			line += fmt.Sprintf(", pid %d", *u.MainPID)
		}
		if u.Since != nil {
			line += ", since " + *u.Since
		}
		fmt.Fprintln(w, line)
	}

	b := s.Budget
	line = "Budget:"
	if b.AuthMode != nil {
		line += " " + *b.AuthMode
	}
	if v := b.Verdict; v != nil {
		if v.RemainPct != nil {
			line += fmt.Sprintf(", %s%% left", percent(*v.RemainPct))
		}
		if v.ResetAt != "" {
			line += ", resets " + string(v.ResetAt)
		}
		if !v.ShouldFire {
			line += ", waiting"
		}
	}
	fmt.Fprintln(w, line)
	printSection(w, b.Section)
	for _, tile := range b.Tiles {
		line := "  " + tile.Label
		if tile.Percent != nil {
			line += fmt.Sprintf(" %s%% used", percent(*tile.Percent))
		}
		if tile.ResetsAt != nil {
			line += ", resets " + *tile.ResetsAt
		}
		fmt.Fprintln(w, line)
	}
	if b.Message != nil {
		fmt.Fprintf(w, "  %s\n", *b.Message)
	}

	if c := s.Fires.Current; c != nil {
		fmt.Fprintf(w, "In flight: %s — %s%s\n", c.ID, c.Summary, issueOf(c.Issue))
		if t := s.FireSummary.Title; t != nil {
			fmt.Fprintf(w, "  Step: %s\n", *t)
		}
	} else {
		fmt.Fprintln(w, "In flight: none")
	}

	fmt.Fprintln(w, "Fires:")
	printFires(w, s.Fires)

	waiting := map[int]dashboard.Ruling{}
	for _, r := range s.Rulings {
		waiting[r.PR] = r
	}
	line = fmt.Sprintf("Open PRs: %d", len(s.OpenPRs.Items))
	if s.OpenPRs.Parked != nil {
		line += fmt.Sprintf(", %d parked", *s.OpenPRs.Parked)
	}
	fmt.Fprintf(w, "%s, %d waiting on a Ruling\n", line, len(s.Rulings))
	printSection(w, s.OpenPRs.Section)
	for _, pr := range s.OpenPRs.Items {
		var marks []string
		if pr.IsDraft {
			marks = append(marks, "draft")
		}
		if pr.Mergeable == "CONFLICTING" {
			marks = append(marks, "conflicting")
		}
		if r, ok := waiting[pr.Number]; ok {
			mark := "Ruling"
			if r.Decisions != nil {
				mark += fmt.Sprintf(": %d decisions", *r.Decisions)
			}
			marks = append(marks, mark)
		}
		line := fmt.Sprintf("  #%d %s", pr.Number, pr.Title)
		if len(marks) > 0 {
			line += " [" + strings.Join(marks, ", ") + "]"
		}
		fmt.Fprintln(w, line)
	}

	m := s.Maps
	line = fmt.Sprintf("Maps: %d open", len(m.Items))
	if m.Total != nil {
		line = fmt.Sprintf("Maps: %d open", *m.Total)
	}
	if m.Truncated {
		line += " (cut off)"
	}
	fmt.Fprintln(w, line)
	printSection(w, m.Section)
	for _, item := range m.Items {
		ready := fmt.Sprintf("%d ready", len(item.Frontier))
		if item.Partial {
			ready += ", partial"
		}
		fmt.Fprintf(w, "  #%d %s — %s\n", item.Number, item.Title, ready)
		for _, f := range item.Frontier {
			var tags []string
			if f.Type != nil {
				tags = append(tags, *f.Type)
			}
			if f.Badge != nil {
				tags = append(tags, *f.Badge)
			}
			line := fmt.Sprintf("    #%d %s", f.Number, f.Title)
			if len(tags) > 0 {
				line += " (" + strings.Join(tags, ", ") + ")"
			}
			fmt.Fprintln(w, line)
		}
	}
}

// printFires is the last few Fires, newest first, one line each, with cost.
func printFires(w io.Writer, fires dashboard.Fires) {
	printSection(w, fires.Section)
	if len(fires.Items) == 0 {
		fmt.Fprintln(w, "  none yet")
	}
	for i, f := range fires.Items {
		if i == firesPrinted {
			break
		}
		outcome := "…"
		if f.Outcome != nil {
			outcome = *f.Outcome
		}
		line := fmt.Sprintf("  %s  %s", f.ID, outcome)
		if f.CostUSD != nil {
			line += fmt.Sprintf("  $%.2f", *f.CostUSD)
		}
		fmt.Fprintf(w, "%s  %s\n", line, f.Summary)
	}
	if n := len(fires.Unreadable); n > 0 {
		fmt.Fprintf(w, "  %d record(s) unreadable: %s\n", n, strings.Join(fires.Unreadable, ", "))
	}
}

// printSection prints a section's error, marked stale when it is.
func printSection(w io.Writer, s dashboard.Section) {
	switch {
	case s.Error == nil:
	case s.Stale:
		fmt.Fprintf(w, "  stale: %s\n", *s.Error)
	default:
		fmt.Fprintf(w, "  error: %s\n", *s.Error)
	}
}

func issueOf(issue *int) string {
	if issue == nil {
		return ""
	}
	return fmt.Sprintf(" (#%d)", *issue)
}

func percent(p float64) string {
	return strconv.FormatFloat(p, 'f', -1, 64)
}
