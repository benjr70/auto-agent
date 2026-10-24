package state

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/benjr70/auto-agent/internal/github"
)

// The labels an Agent PR carries through the reconcile Lane.
const (
	// LabelRevise: a review handed the PR back to the agent.
	LabelRevise = "AFK:revise"
	// LabelRuling: a Ruling request waits on the human.
	LabelRuling = "AFK:ruling"
	// The park labels: each leaves the PR to a human.
	LabelReviseFailed = "AFK:revise-failed"
	LabelRebaseFailed = "AFK:rebase-failed"
	LabelDepsFailed   = "AFK:deps-failed"
	// LabelChecksFailed: CI still red when no fix round remained; the PR is
	// a draft again. The draft parks it, not the label: a human who makes it
	// ready again asks for another try.
	LabelChecksFailed = "AFK:checks-failed"
)

// ReviewDoneMarker opens the comment the one-time review leaves: how the PR
// Triage tells that the review after a PR opened finished.
const ReviewDoneMarker = "<!-- pr-review-done"

// verifyRound matches the heading each round of manual verification posts.
var verifyRound = regexp.MustCompile(`Manual verification — .*round`)

// IsReviewDone says a PR comment is the one-time review's.
func IsReviewDone(body string) bool { return strings.Contains(body, ReviewDoneMarker) }

// IsVerifyRound says a PR comment is a round of manual verification.
func IsVerifyRound(body string) bool { return verifyRound.MatchString(body) }

// The branch shapes the Daemon creates: a Slice's, and a Decision ticket's
// research branch. Anything else is a human's, and never reconciled.
var (
	sliceBranch    = regexp.MustCompile(`^feat/issue-([0-9]+)$`)
	researchBranch = regexp.MustCompile(`^research/[A-Za-z0-9._-]+$`)
	researchTicket = regexp.MustCompile(`^research/([0-9]+)`)
	titleTicket    = regexp.MustCompile(`#([0-9]+)`)
	titleSuffix    = regexp.MustCompile(`\(#([0-9]+)\)\s*$`)
)

// Reason is why the reconcile Lane takes a PR.
type Reason string

// The reasons, first to be worked first: a human waiting on the Daemon
// comes before anything the Daemon can do alone.
const (
	// ReasonRevise: AFK:revise, the PR handed back to the agent.
	ReasonRevise Reason = "revise"
	// ReasonRuling: the human answered a Ruling request with a Ruling.
	ReasonRuling Reason = "ruling"
	// ReasonConflict: the default branch moved under the PR.
	ReasonConflict Reason = "conflict"
	// ReasonDocsMerge: a research PR, every file under the research docs
	// prefix: merged by the docs-only gate, never reviewed.
	ReasonDocsMerge Reason = "docs-merge"
	// ReasonIncomplete: the review or the verification after the PR opened
	// never finished.
	ReasonIncomplete Reason = "incomplete"
	// ReasonDependabot: a Bot PR, taken by the Bot PR Lane. A conflicting
	// one is ReasonConflict, ranked with the Bot PRs.
	ReasonDependabot Reason = "dependabot"
)

var reasonRank = []Reason{ReasonRevise, ReasonRuling, ReasonConflict, ReasonDocsMerge, ReasonIncomplete}

// OpenPR is an open pull request with what the PR Triage reads about it
// beyond the listing. A signal that could not be read is nil, and reads the
// safe way: never as a reason to merge, nor to start work.
type OpenPR struct {
	github.PullRequest
	// ReviewDone: a comment carries the one-time review's marker.
	ReviewDone *bool
	// VerifyDone: the Daemon's last round of manual verification owes no
	// fix round.
	VerifyDone *bool
	// DocsOnly: it changes at least one file, every one under the research
	// docs prefix.
	DocsOnly *bool
	// RulingOwed: the human replied to its Ruling request, and is owed
	// an answer: a full Ruling to apply, or one nudge for a reply that is
	// not one.
	RulingOwed *bool
	// Bot is what was read about a Bot PR; nil when it was not read, which
	// it is not while the Bot PR Lane is off.
	Bot *BotSignals
}

// Reconcile is the Agent PR the reconcile Lane takes, and why.
type Reconcile struct {
	PR     int    `json:"pr"`
	Branch string `json:"branch"`
	// Issue is the ticket the PR is for; 0 when neither its branch nor its
	// title names one.
	Issue  int    `json:"issue,omitempty"`
	Reason Reason `json:"reason"`
	// Bot is set for a Bot PR: it has no ticket.
	Bot bool `json:"bot,omitempty"`
	// Security and AgentCommits are what the triage read about a Bot PR;
	// absent for an Agent PR.
	Security     *bool `json:"security,omitempty"`
	AgentCommits *bool `json:"agentCommits,omitempty"`
}

// Ours says a PR is the Daemon's to reconcile: one of its branch shapes, and
// opened by login. With no known login, the shape alone decides.
func Ours(pr github.PullRequest, login string) bool {
	if !sliceBranch.MatchString(pr.HeadRef) && !researchBranch.MatchString(pr.HeadRef) {
		return false
	}
	return login == "" || strings.EqualFold(pr.Author, login)
}

// IsResearch says a head branch is a Decision ticket's research branch: a
// PR from one is research docs, merged by the docs-only gate.
func IsResearch(headRef string) bool { return researchBranch.MatchString(headRef) }

// Parked says a PR waits on a human: a draft, or a park label. A parked PR
// earns no reason but the human's own Ruling. It is the one definition: the
// Dashboard's count of parked PRs is to ask it too.
func Parked(pr github.PullRequest) bool {
	return pr.Draft || slices.Contains(pr.Labels, LabelReviseFailed) || slices.Contains(pr.Labels, LabelRebaseFailed) || slices.Contains(pr.Labels, LabelDepsFailed)
}

// owed says the human's reply to the PR's Ruling request is owed an
// answer.
func owed(pr OpenPR) bool {
	return slices.Contains(pr.Labels, LabelRuling) && pr.RulingOwed != nil && *pr.RulingOwed
}

// ReasonOf is the reason the reconcile Lane would take an open Agent PR for;
// empty when there is none. A PR that is not ours never has one.
func ReasonOf(pr OpenPR, login string) Reason {
	if IsBotPR(pr.PullRequest) {
		return botReason(pr)
	}
	if !Ours(pr.PullRequest, login) {
		return ""
	}
	switch {
	case Parked(pr.PullRequest):
		// Hidden, except to the Ruling its human gave.
		if owed(pr) {
			return ReasonRuling
		}
		return ""
	case slices.Contains(pr.Labels, LabelRevise):
		return ReasonRevise
	case owed(pr):
		return ReasonRuling
	case pr.Mergeable == github.Conflicting:
		return ReasonConflict
	case pr.DocsOnly != nil && *pr.DocsOnly:
		// Ahead of incomplete: a research PR never gets a review, and would
		// be reconciled for ever.
		return ReasonDocsMerge
	case isFalse(pr.ReviewDone) || isFalse(pr.VerifyDone):
		return ReasonIncomplete
	}
	return ""
}

// botReason is the reason the Bot PR Lane would take a Bot PR for; empty
// when there is none. One that was not read is never taken, and neither is
// a draft, one parked with AFK:deps-failed, or one handed to a human with
// HITL whose head no review has approved.
func botReason(pr OpenPR) Reason {
	switch {
	case pr.Bot == nil || Parked(pr.PullRequest):
		return ""
	case slices.Contains(pr.Labels, LabelHITL) && !pr.Bot.Approved:
		return ""
	case pr.Mergeable == github.Conflicting:
		return ReasonConflict
	}
	return ReasonDependabot
}

// isFalse is a signal read as false: one that could not be read is not.
func isFalse(signal *bool) bool { return signal != nil && !*signal }

// Triage is the PR Triage: the one open Agent PR the reconcile Lane works
// this Fire, or nil. By reason first, then oldest first.
func Triage(prs []OpenPR, login string) *Reconcile {
	type candidate struct {
		pr     OpenPR
		reason Reason
	}
	var candidates []candidate
	for _, pr := range prs {
		if reason := ReasonOf(pr, login); reason != "" {
			candidates = append(candidates, candidate{pr, reason})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		ra, rb := rankOf(candidates[a].pr, candidates[a].reason), rankOf(candidates[b].pr, candidates[b].reason)
		if ra != rb {
			return ra < rb
		}
		return candidates[a].pr.CreatedAt < candidates[b].pr.CreatedAt
	})
	first := candidates[0]
	if first.pr.Bot != nil {
		// A Bot PR has no ticket: a #123 in Dependabot's text is another
		// repository's.
		security, agent := first.pr.Bot.Security, first.pr.Bot.AgentCommits
		return &Reconcile{PR: first.pr.Number, Branch: first.pr.HeadRef, Reason: first.reason, Bot: true, Security: &security, AgentCommits: &agent}
	}
	return &Reconcile{PR: first.pr.Number, Branch: first.pr.HeadRef, Issue: TicketOf(first.pr.PullRequest), Reason: first.reason}
}

// rankOf is where a PR taken for reason goes in the order. Every Bot PR
// comes after every Agent PR, whatever its reason: a human waiting on the
// Daemon is never queued behind a bot. Among Bot PRs a conflicting one goes
// first, as nothing else can be done with it until it is rebased, then a
// security bump before a version bump.
func rankOf(pr OpenPR, reason Reason) int {
	if pr.Bot == nil {
		return slices.Index(reasonRank, reason)
	}
	switch {
	case reason == ReasonConflict:
		return len(reasonRank)
	case pr.Bot.Security:
		return len(reasonRank) + 1
	}
	return len(reasonRank) + 2
}

// TicketOf is the ticket an Agent PR is for: from a Slice branch, the
// `(#N)` a research PR's title ends with, a research branch that starts with
// it, or else the first #N in the title; 0 when none says. The title's suffix
// comes before the branch: a research branch is research/<ticket-slug>, and
// a slug may start with digits that are no ticket ("2026 roadmap").
func TicketOf(pr github.PullRequest) int {
	for _, m := range [][]string{sliceBranch.FindStringSubmatch(pr.HeadRef), titleSuffix.FindStringSubmatch(pr.Title), researchTicket.FindStringSubmatch(pr.HeadRef), titleTicket.FindStringSubmatch(pr.Title)} {
		if m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	return 0
}

// reconcileNext is what the reconcile Lane does with the PR it took. A Bot
// PR goes through the Bot PR Lane, its conflict included. A
// research PR that is docs only is landed through the docs-only gate. A Slice
// PR handed back with AFK:revise has its threads answered, one whose human
// answered its Ruling request has the answer answered, and one that never
// had its review is reviewed; the rest is not handled by this build yet.
func reconcileNext(r Reconcile, prs []OpenPR) Next {
	if r.Bot {
		return Next{Action: ActionDeps, PR: r.PR}
	}
	next := Next{Action: ActionNone, Issue: r.Issue, PR: r.PR, Reason: fmt.Sprintf("the reconcile Lane's %s is not handled by this build yet", r.Reason)}
	if r.Reason == ReasonDocsMerge && researchBranch.MatchString(r.Branch) {
		return Next{Action: ActionDocsMerge, Issue: r.Issue, PR: r.PR}
	}
	if !sliceBranch.MatchString(r.Branch) {
		return next
	}
	switch r.Reason {
	case ReasonRevise:
		return Next{Action: ActionRevise, Issue: r.Issue, PR: r.PR}
	case ReasonRuling:
		return Next{Action: ActionRuling, Issue: r.Issue, PR: r.PR}
	case ReasonConflict:
		return Next{Action: ActionRebase, Issue: r.Issue, PR: r.PR}
	}
	if r.Reason != ReasonIncomplete {
		return next
	}
	for _, pr := range prs {
		if pr.Number == r.PR && isFalse(pr.ReviewDone) {
			return Next{Action: ActionReview, Issue: r.Issue, PR: r.PR}
		}
	}
	// The review ran: CI is waited on, and the verification after it is
	// Verify's.
	return Next{Action: ActionWaitCI, Issue: r.Issue, PR: r.PR}
}
