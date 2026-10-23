// Package depsgate is the deps gate: whether a Bot PR may be squash-merged
// with no human in the loop. A dependency bump is landed on the Daemon's
// own evidence, Tier A and Tier B, so the gate owns the rule rather than a
// judgement made mid-Fire: one function, no discretion. It decides and
// never mutates: it is a pure function of what Go read, and Go merges on
// its verdict, pinned to the head the tiers vouched for. It is the sibling
// of the docs-only gate, and shares its vocabulary of check reasons.
package depsgate

import (
	"fmt"
	"strings"

	"github.com/benjr70/auto-agent/internal/ci"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/state"
)

// Reason is why the gate refused.
type Reason string

const (
	// NotDependabot: the author or the head branch is not the Dependabot
	// app's.
	NotDependabot Reason = "not-dependabot"
	// DraftOrClosed: the PR is closed, merged, or a draft a human parked.
	DraftOrClosed Reason = "draft-or-closed"
	// MarkersStale: a tier marker is absent or for another head, or the PR
	// moved past the head it was gated on.
	MarkersStale Reason = "markers-stale"
	// ChecksNotGreen: a check is failing or still pending.
	ChecksNotGreen Reason = Reason(ci.NotGreen)
	// ChecksMissing: no check ran at all, or a required check did not run
	// and pass.
	ChecksMissing Reason = Reason(ci.Missing)
	// TitleNotDeps: the title is not a `fix(deps):` or `chore(deps):` one.
	TitleNotDeps Reason = "title-not-deps"
	// ChangesRequested: a human reviewed the bump and requested changes.
	ChangesRequested Reason = "changes-requested"
	// MajorUnapproved: a major bump no human approved at its head.
	MajorUnapproved Reason = "major-unapproved"
	// ChecksUnreadable: the PR, its reviews or its checks could not be
	// read. The gate could not run; it says nothing about the PR.
	ChecksUnreadable Reason = "checks-unreadable"
)

// Facts are what the gate decides on, as Go read them.
type Facts struct {
	// PR is the Bot PR as GitHub says it is now; PRErr why it could not be
	// read.
	PR    github.PullRequest
	PRErr error
	// Gated is the head the tiers vouched for: what a merge is pinned to.
	Gated string
	// History is what the Daemon's markers say of Gated.
	History state.DepsHistory
	// Checks are the checks on Gated; ChecksErr why they could not be
	// read.
	Checks    []github.Check
	ChecksErr error
	// Required are the Harness config's required_checks.
	Required []string
	// Reviewed is what the human reviews say of Gated; ReviewsErr why they
	// could not be read.
	Reviewed   state.Reviewed
	ReviewsErr error
	// Major: the bump moves a dependency across a breaking version.
	Major bool
}

// Verdict is what the gate decided.
type Verdict struct {
	// SHA is the head the verdict is about, and the one a merge is pinned
	// to.
	SHA string `json:"sha"`
	// Reason is why the gate refused, and Detail says more; both are empty
	// when it approved.
	Reason Reason `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Approved says the Bot PR may be merged, at SHA.
func (v Verdict) Approved() bool { return v.Reason == "" }

// Human says the refusal is one only a human, or a new head, answers: the
// Daemon retrying the same head would get the same answer forever.
func (v Verdict) Human() bool {
	return v.Reason == TitleNotDeps || v.Reason == ChangesRequested || v.Reason == MajorUnapproved
}

// String is the verdict as the Fire record says it.
func (v Verdict) String() string {
	if v.Approved() {
		return "approved at " + v.SHA
	}
	if v.Detail == "" {
		return string(v.Reason)
	}
	return string(v.Reason) + ": " + v.Detail
}

// Decide is the gate, in lib/deps-gate.sh's order: the PR's state and its
// author, then whether the tiers vouch for what would merge, then its
// checks, which stand in for the review the merge skips, then its title,
// then the humans. A rejection is named before the bump's size, so the
// refusal says what happened.
func Decide(f Facts) Verdict {
	v := Verdict{SHA: f.Gated}
	refuse := func(reason Reason, detail string) Verdict {
		v.Reason, v.Detail = reason, detail
		return v
	}
	pr := f.PR
	switch {
	case f.PRErr != nil:
		return refuse(ChecksUnreadable, "the PR could not be read: "+f.PRErr.Error())
	case pr.State != "open" || pr.Merged || pr.Draft:
		return refuse(DraftOrClosed, fmt.Sprintf("the PR is %s", stateOf(pr)))
	case !state.IsBotPR(pr):
		return refuse(NotDependabot, fmt.Sprintf("author %s on branch %s is not the Dependabot app's", pr.Author, pr.HeadRef))
	case pr.HeadSHA != f.Gated:
		return refuse(MarkersStale, fmt.Sprintf("the PR is at %s, not the %s it was gated on", pr.HeadSHA, f.Gated))
	case f.History.SHA != f.Gated || !f.History.TierA || !f.History.TierB:
		return refuse(MarkersStale, "no tierA=green and tierB=PASS markers for "+f.Gated)
	case f.ChecksErr != nil:
		return refuse(ChecksUnreadable, f.ChecksErr.Error())
	}
	if reason, detail := ci.Vouch(f.Checks, f.Required); reason != "" {
		return refuse(Reason(reason), detail)
	}
	switch {
	case !strings.HasPrefix(pr.Title, "fix(deps):") && !strings.HasPrefix(pr.Title, "chore(deps):"):
		return refuse(TitleNotDeps, "the title is not a deps title: "+pr.Title)
	case f.ReviewsErr != nil:
		return refuse(ChecksUnreadable, "the reviews could not be read: "+f.ReviewsErr.Error())
	case f.Reviewed.ChangesRequested:
		return refuse(ChangesRequested, "a human review requests changes")
	case f.Major && !f.Reviewed.Approved:
		return refuse(MajorUnapproved, "a major bump with no human approval of "+f.Gated)
	}
	return v
}

// stateOf is a refused PR's state, as a refusal says it.
func stateOf(pr github.PullRequest) string {
	switch {
	case pr.Merged:
		return "merged"
	case pr.State != "open":
		return pr.State
	}
	return "a draft"
}
