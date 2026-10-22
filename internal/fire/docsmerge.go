package fire

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/docsgate"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step/research"
	"github.com/benjr70/auto-agent/internal/workspace"
)

// The phases of landing a research PR.
const (
	// PhaseDocsGate: the docs-only gate, on the research PR's head.
	PhaseDocsGate = "docs-gate"
	// PhaseMerge: the squash-merge the gate approved.
	PhaseMerge = "merge"
	// PhaseResolve: the ticket's resolution comment, close and Map line.
	PhaseResolve = "resolve"
)

// How a research ticket fails after its PR is open, in the words of
// plugin/skills/afk-resolve §7.
const (
	// PRWatchExhausted: CI stayed red when no fix round remained.
	PRWatchExhausted = "pr-watch-exhausted"
	// GateRefused: the docs-only gate refused the PR, or the merge it
	// approved did not land.
	GateRefused = "gate-refused"
	// FinishFailed: the research PR is merged, but its ticket cannot be
	// resolved from it: it names no single findings file, or that file is
	// gone from the default branch or has no TL;DR.
	FinishFailed = "finish-failed"
)

// errUnfinishable is a merged research PR its ticket cannot be resolved
// from, however often it is read again: only a human's edit changes it.
var errUnfinishable = errors.New("unfinishable")

// landing is a research PR on its way to the default branch, and what its
// ticket is resolved with once it is there.
type landing struct {
	pr int
	// path is the findings file; gist the answer in one paragraph, empty
	// to read it from the findings file's TL;DR.
	path, gist string
	// mapNumber is the ticket's Map; 0 when it is unmapped.
	mapNumber int
	// reconciled: an earlier Fire opened the PR. A refusal parks it as a
	// draft, so that the reconcile Lane does not take it again until a
	// human asks.
	reconciled bool
}

// land carries an open research PR to a resolved ticket, as
// plugin/skills/afk-resolve §5–§6 did: its CI waited on and fixed within
// rounds.pr_watch, as a Slice's is, then the docs-only gate, then the merge
// the gate approved, pinned to the head it read, then the ticket resolved.
// A research PR gets no review and no Verification round. The ticket's lock
// is held.
func (f *Fire) land(ctx context.Context, record *journal.Record, t *ticket, l landing) error {
	head, err := f.Workspace.Head(ctx)
	if err != nil {
		return fmt.Errorf("reading the research PR's head: %w", err)
	}
	wait, err := f.waitCI(ctx, record, t.harness.RequiredChecks, head, ciReads)
	if err != nil {
		return err
	}
	var finished bool
	if _, finished, err = f.fixCI(ctx, record, t, l.pr, wait); !finished {
		return err
	}
	if record.Phase == PhaseChecksFailed {
		detail := fmt.Sprintf("CI on research PR %s stayed red after %d fix rounds; the PR is a draft with %s", f.prURL(l.pr), t.harness.Rounds.PRWatch, state.LabelChecksFailed)
		return f.failResearch(ctx, record, t.issue.Number, PRWatchExhausted, detail)
	}

	record.Phase = PhaseDocsGate
	pr, verdict, err := f.docsGate(ctx, t.harness, l.pr)
	if err != nil {
		return err
	}
	record.Resolve.Gate = &verdict
	merged := pr.MergeSHA
	if !pr.Merged {
		if !verdict.Approved() {
			return f.refused(ctx, record, t, l, verdict.String())
		}
		record.Phase = PhaseMerge
		// The squash commit is titled as GitHub titles one, with the PR.
		merged, err = f.GitHub.MergePullRequest(ctx, f.Repo.Owner, f.Repo.Name, l.pr, verdict.SHA, fmt.Sprintf("%s (#%d)", pr.Title, l.pr))
		if err != nil {
			return f.refused(ctx, record, t, l, "merge-failed: "+err.Error())
		}
	}
	record.Resolve.Merged = merged
	if l.path == "" {
		l.path = findingsIn(verdict.Changed, t.harness.DocsResearchPrefix)
	}
	if l.gist == "" {
		if l.gist, err = f.tldr(ctx, l.path); err != nil {
			return f.unfinishable(ctx, record, t.issue.Number, l.pr, err)
		}
	}
	return f.resolve(ctx, record, t.harness, t.issue, l)
}

// docsGate reads what the docs-only gate decides on, for the PR as it is
// now, and asks it. Only the head GitHub says the PR is at is diffed and has
// its checks read: a merge on the verdict is pinned to it.
func (f *Fire) docsGate(ctx context.Context, harness config.Harness, number int) (github.PullRequest, docsgate.Verdict, error) {
	pr, err := f.GitHub.PullRequest(ctx, f.Repo.Owner, f.Repo.Name, number)
	if err != nil {
		return pr, docsgate.Verdict{}, fmt.Errorf("reading research PR #%d: %w", number, err)
	}
	facts := docsgate.Facts{SHA: pr.HeadSHA, Prefix: harness.DocsResearchPrefix, Required: harness.RequiredChecks}
	if facts.Changed, err = f.Workspace.ChangedAt(ctx, pr.HeadSHA); err != nil && !errors.Is(err, workspace.ErrHeadMissing) {
		return pr, docsgate.Verdict{}, fmt.Errorf("reading the diff of research PR #%d: %w", number, err)
	}
	if facts.Changed != nil {
		facts.Checks, facts.ChecksErr = f.GitHub.Checks(ctx, f.Repo.Owner, f.Repo.Name, pr.HeadSHA)
	}
	return pr, docsgate.Decide(facts), nil
}

// refused leaves a research PR the gate refused open, and hands its ticket
// to a human, saying that merging the PR finishes it. A PR an earlier Fire
// opened is parked as a draft as well.
func (f *Fire) refused(ctx context.Context, record *journal.Record, t *ticket, l landing, why string) error {
	if l.reconciled {
		if err := f.parkResearch(ctx, record, l.pr, why); err != nil {
			record.Warnings = append(record.Warnings, err.Error())
		}
	}
	detail := fmt.Sprintf("the docs-only gate refused research PR %s (%s). The PR is open: merging it finishes this ticket", f.prURL(l.pr), why)
	return f.failResearch(ctx, record, t.issue.Number, GateRefused, detail)
}

// parkResearch makes a research PR the gate refused a draft, with a comment
// saying why: a draft is not taken by the reconcile Lane again.
func (f *Fire) parkResearch(ctx context.Context, record *journal.Record, number int, why string) error {
	pr, err := f.GitHub.PullRequest(ctx, f.Repo.Owner, f.Repo.Name, number)
	if err != nil {
		return fmt.Errorf("parking research PR #%d: %w", number, err)
	}
	if !pr.Draft {
		if err := f.GitHub.MarkDraft(ctx, pr); err != nil {
			return fmt.Errorf("parking research PR #%d: marking it draft: %w", number, err)
		}
	}
	body := fmt.Sprintf("The docs-only gate refused this research PR: %s. Parked as a draft for a human: merge it to finish its ticket, or mark it ready to have the gate asked again.", why)
	if err := f.GitHub.CreateComment(ctx, f.Repo.Owner, f.Repo.Name, number, body); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("research PR #%d is a draft, but the comment saying why was not posted: %v", number, err))
	}
	return nil
}

// resolve resolves a research ticket whose findings are on the default
// branch, as plugin/skills/afk-resolve §6 did: the gist and the findings
// file linked on the default branch, the ticket closed and AFK:done, then
// its fog graduated and one line in its Map's Decisions so far. The
// graduation and the Map line are best-effort: the ticket carries the
// answer, and the Map is an index.
func (f *Fire) resolve(ctx context.Context, record *journal.Record, harness config.Harness, issue github.Issue, l landing) error {
	owner, name, number := f.Repo.Owner, f.Repo.Name, issue.Number
	record.Phase = PhaseResolve
	record.Resolve.Findings = cmp.Or(record.Resolve.Findings, l.path)
	if err := f.GitHub.CreateComment(ctx, owner, name, number, research.Resolution(l.gist, f.Repo.Slug(), f.Repo.DefaultBranch, l.path, l.pr)); err != nil {
		return fmt.Errorf("research PR #%d is merged, but #%d's resolution comment was not posted: %w", l.pr, number, err)
	}
	if err := f.GitHub.CloseIssue(ctx, owner, name, number); err != nil {
		return fmt.Errorf("research PR #%d is merged, but #%d was not closed: %w", l.pr, number, err)
	}
	if err := f.GitHub.AddLabels(ctx, owner, name, number, state.LabelDone); err != nil {
		return fmt.Errorf("#%d is resolved, but %s could not be applied: %w", number, state.LabelDone, err)
	}
	// A refusal in an earlier Fire left it failed.
	if err := f.GitHub.RemoveLabel(ctx, owner, name, number, state.LabelFailed); err != nil {
		return fmt.Errorf("#%d is resolved, but %s could not be taken off: %w", number, state.LabelFailed, err)
	}
	record.Resolve.Resolved = true
	f.graduate(ctx, record, harness, issue, l)
	record.Phase = PhaseDone
	return nil
}

// tldr is the TL;DR of a findings file on the default branch, which is the
// answer once the research PR is merged.
func (f *Fire) tldr(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: the research PR names no single findings file to resolve its ticket with", errUnfinishable)
	}
	data, err := f.GitHub.FileContent(ctx, f.Repo.Owner, f.Repo.Name, path)
	if github.IsNotFound(err) {
		return "", fmt.Errorf("%w: the findings file %s is not on the default branch", errUnfinishable, path)
	}
	if err != nil {
		return "", fmt.Errorf("reading the findings file %s on the default branch: %w", path, err)
	}
	gist := research.Section(string(data), "TL;DR")
	if gist == "" {
		return "", fmt.Errorf("%w: the findings file %s has no TL;DR to resolve its ticket with", errUnfinishable, path)
	}
	return gist, nil
}

// findingsIn is the one Markdown file under the prefix among paths; empty
// when there is not exactly one.
func findingsIn(paths []string, prefix string) string {
	found := ""
	for _, path := range paths {
		if strings.HasPrefix(path, prefix) && strings.HasSuffix(path, ".md") {
			if found != "" {
				return ""
			}
			found = path
		}
	}
	return found
}

// unfinishable hands a ticket whose merged research PR it cannot be
// resolved from to a human, when err says no later read would do better: the
// PR is labelled AFK:failed, so that it is no longer finished, and the ticket
// AFK:failed with a comment saying why. Any other err is returned as it is,
// for the next Fire to try again.
func (f *Fire) unfinishable(ctx context.Context, record *journal.Record, number, pr int, err error) error {
	if !errors.Is(err, errUnfinishable) {
		return err
	}
	why := strings.TrimPrefix(err.Error(), errUnfinishable.Error()+": ")
	if err := f.GitHub.AddLabels(ctx, f.Repo.Owner, f.Repo.Name, pr, state.LabelFailed); err != nil {
		return fmt.Errorf("research PR #%d is merged, but #%d cannot be resolved from it (%s), and %s could not be put on the PR: %w", pr, number, why, state.LabelFailed, err)
	}
	detail := fmt.Sprintf("research PR %s is merged, but this ticket cannot be resolved from it: %s. Fix the findings file and take %s off the PR to have the next Fire finish it, or resolve this ticket by hand", f.prURL(pr), why, state.LabelFailed)
	return f.failResearch(ctx, record, number, FinishFailed, detail)
}

func (f *Fire) prURL(number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", f.Repo.Slug(), number)
}

// reconcileDocsMerge lands a research PR an earlier Fire left open: its
// gate refused, or the Fire ended before it. Its ticket's lock is held for
// the landing; a PR that names no ticket is not landed, as there is nothing
// to resolve with it.
func (f *Fire) reconcileDocsMerge(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) (err error) {
	number := next.Issue
	pr, err := f.GitHub.PullRequest(ctx, f.Repo.Owner, f.Repo.Name, next.PR)
	if err != nil {
		return fmt.Errorf("reading research PR #%d: %w", next.PR, err)
	}
	marked, _ := research.ParseMarker(pr.Body)
	if number == 0 {
		number = marked.Ticket
	}
	if number == 0 {
		record.Phase = PhaseDone
		record.NotWorked = fmt.Sprintf("research PR #%d names no ticket to resolve", next.PR)
		return nil
	}
	issue, err := f.GitHub.Issue(ctx, f.Repo.Owner, f.Repo.Name, number)
	if err != nil {
		return err
	}
	branch, err := f.Workspace.Open(ctx, pr.HeadRef)
	if err != nil {
		return fmt.Errorf("opening the branch: %w", err)
	}
	record.Branch, record.NotWorked = branch, ""
	record.Resolve = &journal.Resolve{Type: "research", Slug: marked.Slug, Findings: research.FindingsOf(pr.Body), PR: &next.PR}
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	t := &ticket{issue: issue, branch: branch, harness: harness}
	return f.land(ctx, record, t, landing{pr: next.PR, path: record.Resolve.Findings, mapNumber: marked.Map, reconciled: true})
}

// finish resolves the ticket of a research PR that was merged with the
// ticket left open, from the PR's marker and the merged file's TL;DR, as
// plugin/skills/afk-resolve §5b did. It does no research. A ticket already
// closed is left as it is.
func (f *Fire) finish(ctx context.Context, record *journal.Record, harness config.Harness, next state.Next) (err error) {
	number, owner, name := next.Issue, f.Repo.Owner, f.Repo.Name
	record.PR = &next.PR
	issue, err := f.GitHub.Issue(ctx, owner, name, number)
	if err != nil {
		return err
	}
	if issue.State == "closed" {
		record.Phase = PhaseDone
		return nil
	}
	pr, err := f.GitHub.PullRequest(ctx, owner, name, next.PR)
	if err != nil {
		return fmt.Errorf("reading research PR #%d: %w", next.PR, err)
	}
	marked, _ := research.ParseMarker(pr.Body)
	path := research.FindingsOf(pr.Body)
	if path == "" {
		files, err := f.GitHub.Files(ctx, owner, name, next.PR)
		if err != nil {
			return fmt.Errorf("reading the files of research PR #%d: %w", next.PR, err)
		}
		path = findingsIn(files, harness.DocsResearchPrefix)
	}
	record.Resolve = &journal.Resolve{Type: "research", Slug: marked.Slug, Findings: path, PR: &next.PR, Merged: pr.MergeSHA}
	gist, err := f.tldr(ctx, path)
	if err != nil {
		return f.unfinishable(ctx, record, number, next.PR, err)
	}
	if err := f.lock(ctx, number); err != nil {
		return err
	}
	defer f.unlock(ctx, record, number, &err)
	return f.resolve(ctx, record, harness, issue, landing{pr: next.PR, path: path, gist: gist, mapNumber: marked.Map})
}
