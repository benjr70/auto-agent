package fire

import (
	"context"
	"fmt"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/daemon"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/pick"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/graduate"
	"github.com/benjr70/auto-agent/internal/step/research"
)

// PhaseGraduate: the Graduate fog Step and its writes, after a resolve.
const PhaseGraduate = "graduate"

// graduated is a proposal on its way to a ticket.
type graduated struct {
	proposal graduate.Proposal
	// entry is what the Fire record says of it; entry.Issue is 0 when the
	// ticket could not be created.
	entry journal.Graduated
	id    int64
}

// graduate advances a resolved ticket's Map, as plugin/skills/afk-resolve
// §6b did: the Graduate fog Step proposes at most graduate.Max new tickets,
// and Go creates each one, makes it the Map's sub-issue, wires its blocked_by
// edges and labels it, routing it AFK only as graduate.Route allows; then
// the Map is edited once, with the ticket's Decisions so far line and the
// fog each new ticket cleared; then the AFK tickets whose edges are all
// wired go on the pick signal, last. A new ticket is never worked in this
// Fire. Every failure is a warning: the ticket stays resolved.
func (f *Fire) graduate(ctx context.Context, record *journal.Record, harness config.Harness, issue github.Issue, l landing) {
	if l.mapNumber == 0 {
		record.Warnings = append(record.Warnings, fmt.Sprintf("#%d is unmapped: no Map has its Decisions so far appended, and no fog is graduated", issue.Number))
		return
	}
	record.Phase = PhaseGraduate
	tickets := f.graduateFog(ctx, record, issue, l)
	f.editMap(ctx, record, issue, l, tickets)
	f.publish(ctx, record, harness, tickets)
	for _, t := range tickets {
		if t.entry.Issue != 0 {
			record.Resolve.Graduated = append(record.Resolve.Graduated, t.entry)
		}
	}
}

// graduateFog asks the Graduate fog Step, and creates, parents, wires and
// labels the tickets it proposes. Each AFK ticket is left off the pick
// signal; one whose edges could not all be wired is HITL.
func (f *Fire) graduateFog(ctx context.Context, record *journal.Record, issue github.Issue, l landing) []*graduated {
	owner, name := f.Repo.Owner, f.Repo.Name
	warn := func(format string, args ...any) {
		record.Warnings = append(record.Warnings, fmt.Sprintf(format, args...))
	}
	m, err := f.GitHub.Issue(ctx, owner, name, l.mapNumber)
	if err != nil {
		warn("the Map #%d could not be read, so no fog is graduated: %v", l.mapNumber, err)
		return nil
	}
	subs, err := f.GitHub.SubIssues(ctx, owner, name, l.mapNumber)
	if err != nil {
		warn("the tickets of Map #%d could not be read, so no fog is graduated: %v", l.mapNumber, err)
		return nil
	}
	url := func(number int) string { return fmt.Sprintf("https://github.com/%s/issues/%d", f.Repo.Slug(), number) }
	in := graduate.Input{
		Ticket:   graduate.Ticket{Number: issue.Number, Title: issue.Title, URL: url(issue.Number), Question: research.Question(issue.Body)},
		Answer:   strings.TrimSpace(l.gist),
		Findings: l.path,
		Map: graduate.Map{
			Number: m.Number, Title: m.Title, URL: url(m.Number),
			Destination: research.Section(m.Body, "Destination"), Notes: research.Section(m.Body, "Notes"),
			Decisions: research.Section(m.Body, "Decisions so far"), NotYetSpecified: research.Section(m.Body, "Not yet specified"),
			OutOfScope: research.Section(m.Body, "Out of scope"),
		},
		Children: []graduate.Child{},
		Max:      graduate.Max,
	}
	ids := map[int]int64{}
	for _, child := range subs {
		if child.State == "open" && child.Number != issue.Number {
			in.Children = append(in.Children, graduate.Child{Number: child.Number, Title: child.Title, Type: state.WayfinderType(child.Labels)})
			ids[child.Number] = child.ID
		}
	}
	// Go follows the chain itself: what the Step says of depth is not asked.
	depth, err := graduate.Depth(issue.Body, func(number int) (string, error) {
		ancestor, err := f.GitHub.Issue(ctx, owner, name, number)
		return ancestor.Body, err
	})
	if err != nil {
		warn("the Spawned by chain of #%d could not be followed, so every ticket it spawns is %s: %v", issue.Number, state.LabelHITL, err)
		depth = graduate.MaxAFKDepth
	}
	depth++

	spec := graduate.Spec()
	verdict, ok, err := f.gate(ctx, record, spec.Name)
	if !ok {
		why := "the Budget gate said to wait"
		if err != nil {
			why = err.Error()
		}
		warn("the Graduate fog Step did not run, so no fog is graduated: %s", why)
		return nil
	}
	call := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: in}
	result, run := callStep(ctx, f, record, spec, call, *verdict, func(r graduate.Result) error { return r.Check(in) })
	if run.Outcome != step.OK {
		warn("the Graduate fog Step ended %s, so no fog is graduated: %s", run.Outcome, run.Detail)
		return nil
	}
	if len(result.Proposals) == 0 {
		return nil
	}
	if _, _, err := daemon.EnsureLabels(ctx, f.GitHub, owner, name); err != nil {
		warn("the harness labels could not all be ensured before graduating fog: %v", err)
	}
	tickets := make([]*graduated, len(result.Proposals))
	for i, p := range result.Proposals {
		t := &graduated{proposal: p, entry: journal.Graduated{Type: p.Type, Depth: depth, Route: graduate.Route(p.Type, p.HumanFree, depth)}}
		tickets[i] = t
		labels := []string{state.WayfinderLabel(p.Type)}
		if t.entry.Route == state.LabelHITL {
			labels = append(labels, state.LabelHITL)
		}
		created, err := f.GitHub.CreateIssue(ctx, owner, name, github.NewIssue{Title: strings.TrimSpace(p.Title), Body: graduate.Body(p, issue.Number), Labels: labels})
		if err != nil {
			warn("the ticket %q spawned by #%d could not be created: %v", p.Title, issue.Number, err)
			continue
		}
		t.entry.Issue, t.id = created.Number, created.ID
		t.entry.Wired = f.wire(ctx, warn, l.mapNumber, t, tickets[:i], ids)
		if !t.entry.Wired && t.entry.Route == state.LabelAFK {
			// An AFK ticket goes on the pick signal only once wired: one
			// that cannot be is a human's to finish.
			t.entry.Route = state.LabelHITL
			if err := f.GitHub.AddLabels(ctx, owner, name, created.Number, state.LabelHITL); err != nil {
				warn("#%d could not be wired, and %s could not be put on it: %v", created.Number, state.LabelHITL, err)
			}
		}
	}
	return tickets
}

// wire makes a new ticket the Map's sub-issue and adds its blocked_by
// edges, on open children by their database id and on the proposals before
// it. It reports whether every one landed.
func (f *Fire) wire(ctx context.Context, warn func(string, ...any), mapNumber int, t *graduated, before []*graduated, ids map[int]int64) bool {
	owner, name, number := f.Repo.Owner, f.Repo.Name, t.entry.Issue
	wired := true
	if err := f.GitHub.AddSubIssue(ctx, owner, name, mapNumber, t.id); err != nil {
		warn("#%d could not be made a sub-issue of Map #%d: %v", number, mapNumber, err)
		wired = false
	}
	block := func(blocker int, id int64) {
		if err := f.GitHub.AddBlockedBy(ctx, owner, name, number, id); err != nil {
			warn("#%d could not be marked blocked by #%d: %v", number, blocker, err)
			wired = false
		}
	}
	for _, blocker := range t.proposal.BlockedBy {
		block(blocker, ids[blocker])
	}
	for _, b := range t.proposal.BlockedByProposals {
		blocker := before[b-1]
		if blocker.entry.Issue == 0 {
			warn("#%d could not be marked blocked by %q, which was not created", number, blocker.proposal.Title)
			wired = false
			continue
		}
		block(blocker.entry.Issue, blocker.id)
	}
	return wired
}

// editMap edits the Map once, read fresh so that no edit made since is
// lost: the resolved ticket's line appended to its Decisions so far, and the
// fog each created ticket clears cut out of its Not yet specified. Fog the
// Map does not hold is left, with a warning. Nothing else is edited.
func (f *Fire) editMap(ctx context.Context, record *journal.Record, issue github.Issue, l landing, tickets []*graduated) {
	owner, name := f.Repo.Owner, f.Repo.Name
	warn := func(format string, args ...any) {
		record.Warnings = append(record.Warnings, fmt.Sprintf(format, args...))
	}
	m, err := f.GitHub.Issue(ctx, owner, name, l.mapNumber)
	if err != nil {
		warn("the Map #%d append failed: %v", l.mapNumber, err)
		return
	}
	line := research.DecisionLine(issue.Title, fmt.Sprintf("https://github.com/%s/issues/%d", f.Repo.Slug(), issue.Number), l.gist)
	body, ok := research.AppendDecision(m.Body, line)
	if !ok {
		warn("the Map #%d append failed: it has no Decisions so far", l.mapNumber)
	}
	for _, t := range tickets {
		if t.entry.Issue == 0 || strings.TrimSpace(t.proposal.Clears) == "" {
			continue
		}
		cleared, ok := graduate.ClearFog(body, t.proposal.Clears)
		if !ok {
			warn("#%d clears %q, which is not in Map #%d's Not yet specified, so the fog was not edited", t.entry.Issue, t.proposal.Clears, l.mapNumber)
			continue
		}
		body = cleared
	}
	if body == m.Body {
		return
	}
	if err := f.GitHub.EditIssueBody(ctx, owner, name, l.mapNumber, body); err != nil {
		warn("the Map #%d append failed: %v", l.mapNumber, err)
	}
}

// publish puts each AFK ticket whose edges are all wired on the pick
// signal, with the second Priority of the configured order under a Project
// pick (the first when the order has one), as plugin/skills/wayfinder has
// it. A HITL ticket never goes on it; an AFK ticket that cannot be put on
// it is labelled HITL instead.
func (f *Fire) publish(ctx context.Context, record *journal.Record, harness config.Harness, tickets []*graduated) {
	priority := graduatedPriority(harness.Pick)
	harness.Repo = f.Repo
	for _, t := range tickets {
		if t.entry.Issue == 0 || t.entry.Route != state.LabelAFK || !t.entry.Wired {
			continue
		}
		if _, err := pick.Add(ctx, f.GitHub, harness, t.entry.Issue, priority); err != nil {
			// Neither AFK nor HITL would leave it nobody's.
			t.entry.Route = state.LabelHITL
			record.Warnings = append(record.Warnings, fmt.Sprintf("#%d was not put on the pick signal, so it is %s: %v", t.entry.Issue, state.LabelHITL, err))
			if err := f.GitHub.AddLabels(ctx, f.Repo.Owner, f.Repo.Name, t.entry.Issue, state.LabelHITL); err != nil {
				record.Warnings = append(record.Warnings, fmt.Sprintf("#%d could not be labelled %s: %v", t.entry.Issue, state.LabelHITL, err))
			}
			continue
		}
		t.entry.Picked = true
	}
}

// graduatedPriority is the Priority a graduated AFK ticket is put on the
// board with: the second of the configured order, or the first when the
// order has one. It is empty under a label-only pick, which has none.
func graduatedPriority(p config.Pick) string {
	if p.Shape != config.PickProject || p.Project == nil || len(p.Project.Order) == 0 {
		return ""
	}
	return p.Project.Order[min(1, len(p.Project.Order)-1)]
}
