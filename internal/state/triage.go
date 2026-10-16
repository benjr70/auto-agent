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
)

var reasonRank = []Reason{ReasonRevise, ReasonRuling, ReasonConflict, ReasonDocsMerge, ReasonIncomplete}

// OpenPR is an open pull request with what the PR Triage reads about it
// beyond the listing. A signal that could not be read is nil, and reads the
// safe way: never as a reason to merge, nor to start work.
type OpenPR struct {
	github.PullRequest
	// ReviewDone: a comment carries the one-time review's marker.
	ReviewDone *bool
	// VerifyDone: a comment is a round of manual verification.
	VerifyDone *bool
	// DocsOnly: it changes at least one file, every one under the research
	// docs prefix.
	DocsOnly *bool
	// RulingReplied: the human answered its Ruling request with a Ruling.
	RulingReplied *bool
}

// Reconcile is the Agent PR the reconcile Lane takes, and why.
type Reconcile struct {
	PR     int    `json:"pr"`
	Branch string `json:"branch"`
	// Issue is the ticket the PR is for; 0 when neither its branch nor its
	// title names one.
	Issue  int    `json:"issue,omitempty"`
	Reason Reason `json:"reason"`
}

// Ours says a PR is the Daemon's to reconcile: one of its branch shapes, and
// opened by login. With no known login, the shape alone decides.
func Ours(pr github.PullRequest, login string) bool {
	if !sliceBranch.MatchString(pr.HeadRef) && !researchBranch.MatchString(pr.HeadRef) {
		return false
	}
	return login == "" || strings.EqualFold(pr.Author, login)
}

// Parked says a PR waits on a human: a draft, or a park label. A parked PR
// earns no reason but the human's own Ruling. It is the one definition: the
// Dashboard's count of parked PRs is to ask it too.
func Parked(pr github.PullRequest) bool {
	return pr.Draft || slices.Contains(pr.Labels, LabelReviseFailed) || slices.Contains(pr.Labels, LabelRebaseFailed) || slices.Contains(pr.Labels, LabelDepsFailed)
}

// ruled says the human answered the PR's Ruling request with a Ruling.
func ruled(pr OpenPR) bool {
	return slices.Contains(pr.Labels, LabelRuling) && pr.RulingReplied != nil && *pr.RulingReplied
}

// ReasonOf is the reason the reconcile Lane would take an open Agent PR for;
// empty when there is none. A PR that is not ours never has one.
func ReasonOf(pr OpenPR, login string) Reason {
	if !Ours(pr.PullRequest, login) {
		return ""
	}
	switch {
	case Parked(pr.PullRequest):
		// Hidden, except to the Ruling its human gave.
		if ruled(pr) {
			return ReasonRuling
		}
		return ""
	case slices.Contains(pr.Labels, LabelRevise):
		return ReasonRevise
	case ruled(pr):
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
		ra, rb := slices.Index(reasonRank, candidates[a].reason), slices.Index(reasonRank, candidates[b].reason)
		if ra != rb {
			return ra < rb
		}
		return candidates[a].pr.CreatedAt < candidates[b].pr.CreatedAt
	})
	first := candidates[0]
	return &Reconcile{PR: first.pr.Number, Branch: first.pr.HeadRef, Issue: TicketOf(first.pr.PullRequest), Reason: first.reason}
}

// TicketOf is the ticket an Agent PR is for: from a Slice branch, a research
// branch that starts with it, or else the first #N in the title; 0 when none
// says.
func TicketOf(pr github.PullRequest) int {
	for _, m := range [][]string{sliceBranch.FindStringSubmatch(pr.HeadRef), researchTicket.FindStringSubmatch(pr.HeadRef), titleTicket.FindStringSubmatch(pr.Title)} {
		if m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	return 0
}

// reconcileNext is what the reconcile Lane does with the PR it took. A Slice
// PR handed back with AFK:revise has its threads answered, and one that
// never had its review is reviewed; the rest is not handled by this build
// yet.
func reconcileNext(r Reconcile, prs []OpenPR) Next {
	next := Next{Action: ActionNone, Issue: r.Issue, PR: r.PR, Reason: fmt.Sprintf("the reconcile Lane's %s is not handled by this build yet", r.Reason)}
	if !sliceBranch.MatchString(r.Branch) {
		return next
	}
	if r.Reason == ReasonRevise {
		return Next{Action: ActionRevise, Issue: r.Issue, PR: r.PR}
	}
	if r.Reason != ReasonIncomplete {
		return next
	}
	for _, pr := range prs {
		if pr.Number == r.PR && isFalse(pr.ReviewDone) {
			return Next{Action: ActionReview, Issue: r.Issue, PR: r.PR}
		}
	}
	next.Reason = "the reconcile Lane's incomplete is not handled by this build yet: the review ran, and the verification after it is Verify's"
	return next
}
