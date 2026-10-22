package fire

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/research"
)

// PhaseResearch: the Research Step, on a Decision ticket.
const PhaseResearch = "research"

// ResearchError is how a research ticket fails once its attempts at the
// Research Step ran out.
const ResearchError = "research-error"

// research works a research Decision ticket to a resolved ticket, as
// plugin/skills/afk-resolve §1–§6 did: claimed, researched by the Research
// Step into one findings file, the file re-checked, committed to
// research/<slug>, the PR opened and then landed. Go does every write. paused is set when
// the ticket carries AFK:paused from a Research Step the usage limit cut off:
// it is resumed by its session, like any Step.
func (f *Fire) research(ctx context.Context, record *journal.Record, harness config.Harness, issue github.Issue, paused bool) (err error) {
	number, owner, name := issue.Number, f.Repo.Owner, f.Repo.Name
	in := f.researchInput(ctx, record, harness, issue)
	slug := research.TicketSlug(issue.Title, issue.Number)
	resolve := &journal.Resolve{Type: "research", Slug: slug, Findings: in.Path}
	record.Resolve = resolve
	branch, err := f.Workspace.Open(ctx, research.Branch(slug))
	if err != nil {
		return fmt.Errorf("opening the branch: %w", err)
	}
	record.Branch = branch
	spec := research.Spec()

	// The Budget gate before the claim, as for a Slice: a Fire told to wait
	// leaves nothing behind.
	record.Phase = PhaseGate
	verdict, ok, err := f.gate(ctx, record, spec.Name)
	if !ok {
		return err
	}
	if f.Login != "" {
		if err := f.GitHub.AddAssignees(ctx, owner, name, number, f.Login); err != nil {
			return fmt.Errorf("claiming #%d: %w", number, err)
		}
	}
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness, paused: paused}

	session := ""
	if paused {
		session = f.Journal.Session(f.Repo.Slug(), number, spec.Name)
	}
	record.Phase = PhaseResearch
	if err := f.Journal.Write(*record); err != nil {
		return fmt.Errorf("writing the Fire record: %w", err)
	}
	check := func(r research.Result) error {
		if err := r.Check(); err != nil || r.Outcome == research.NoSources {
			return err
		}
		changed, err := f.Workspace.Changed(ctx)
		if err != nil {
			return fmt.Errorf("the clone could not be read: %w", err)
		}
		return research.CheckFindings(f.Workspace.Dir(), in, harness.DocsResearchPrefix, changed)
	}
	fresh := step.Call{Dir: f.Workspace.Dir(), Model: f.model(*verdict), Input: in}
	result, run := resumeOrCall(ctx, f, record, spec, fresh, *verdict, session, check)
	if stop, err := f.settle(ctx, record, t, spec.Name, run, cmp.Or(run.SessionID, session)); stop {
		if record.Phase == PhaseFailed {
			// The attempts ran out: the ticket is AFK:failed, and says why
			// in the resolve's words.
			resolve.Failed = ResearchError
			detail := fmt.Sprintf("the Research Step was asked the ticket's question, for %s; its last attempt came back %s: %s", in.Path, run.Outcome, run.Detail)
			f.resolveComment(ctx, record, number, ResearchError, detail)
		}
		return err
	}
	if result.Outcome == research.NoSources {
		detail := fmt.Sprintf("tried %s; missing: %s", strings.Join(result.Sources, ", "), strings.TrimSpace(result.Missing))
		return f.failResearch(ctx, record, number, research.NoSources, detail)
	}

	record.Phase = PhaseCommit
	commit, err := f.Workspace.CommitAndPush(ctx, branch, research.Subject(result, number)+"\n")
	if err != nil {
		return fmt.Errorf("committing and pushing: %w", err)
	}
	record.Commit = commit.SHA
	record.Phase = PhasePR
	pr, err := f.GitHub.CreatePullRequest(ctx, owner, name, github.NewPullRequest{
		Title: research.Subject(result, number), Head: branch, Base: f.Repo.DefaultBranch,
		Body: research.PullRequestBody(in, result, slug, f.Now()),
	})
	if err != nil {
		return fmt.Errorf("opening the research PR: %w", err)
	}
	record.PR, resolve.PR = &pr.Number, &pr.Number
	mapNumber := 0
	if in.Map != nil {
		mapNumber = in.Map.Number
	}
	return f.land(ctx, record, t, landing{pr: pr.Number, path: in.Path, gist: result.Gist, mapNumber: mapNumber})
}

// researchInput is what the Research Step is told about the ticket. Its Map
// is its GitHub parent, not what its body says; a Map that cannot be read
// leaves the ticket unmapped, and never blocks it.
func (f *Fire) researchInput(ctx context.Context, record *journal.Record, harness config.Harness, issue github.Issue) research.Input {
	url := func(number int) string { return fmt.Sprintf("https://github.com/%s/issues/%d", f.Repo.Slug(), number) }
	in := research.Input{
		Ticket:   research.Ticket{Number: issue.Number, Title: issue.Title, URL: url(issue.Number)},
		Question: research.Question(issue.Body),
		Date:     f.Now().UTC().Format(time.DateOnly),
	}
	mapSlug := research.Unmapped
	switch parent, ok, err := f.GitHub.Parent(ctx, f.Repo.Owner, f.Repo.Name, issue.Number); {
	case err != nil:
		record.Warnings = append(record.Warnings, fmt.Sprintf("the Map of #%d could not be read, so it is researched unmapped: %v", issue.Number, err))
	case !ok:
		record.Warnings = append(record.Warnings, fmt.Sprintf("#%d is no Map's sub-issue, so it is researched unmapped", issue.Number))
	default:
		in.Map = &research.Map{
			Number: parent.Number, Title: parent.Title, URL: url(parent.Number),
			Destination: research.Section(parent.Body, "Destination"), Notes: research.Section(parent.Body, "Notes"),
		}
		mapSlug = research.MapSlug(parent.Title, parent.Number)
	}
	in.Path = research.Path(harness.DocsResearchPrefix, mapSlug, research.TicketSlug(issue.Title, issue.Number))
	return in
}

// failResearch hands a research ticket to a human: AFK:failed and the
// resolve's comment, the ticket left open.
func (f *Fire) failResearch(ctx context.Context, record *journal.Record, number int, reason, detail string) error {
	record.Resolve.Failed = reason
	if err := f.GitHub.AddLabels(ctx, f.Repo.Owner, f.Repo.Name, number, state.LabelFailed); err != nil {
		return fmt.Errorf("#%d failed (%s), but %s could not be applied: %w", number, reason, state.LabelFailed, err)
	}
	record.Phase = PhaseFailed
	f.resolveComment(ctx, record, number, reason, detail)
	return fmt.Errorf("#%d failed: %s: %s", number, reason, detail)
}

// resolveComment says on the ticket why it failed, in the words of
// plugin/skills/afk-resolve §7. A comment that cannot be posted is a
// warning: the label already says the ticket is a human's.
func (f *Fire) resolveComment(ctx context.Context, record *journal.Record, number int, reason, detail string) {
	body := fmt.Sprintf("afk-resolve FAILED at %s — %s: %s. Ticket left open for human triage.", f.Now().UTC().Format(time.RFC3339), reason, detail)
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, number, body); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("#%d failed (%s), but the comment saying so was not posted: %v", number, reason, err))
	}
}
