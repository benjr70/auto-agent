// Package state derives what the Daemon should do next from GitHub facts
// alone. Nothing here is stored and nothing here writes: the same facts
// always give the same state (ADR 0012).
package state

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
)

// The harness's label vocabulary. It is fixed, never configured.
const (
	LabelAFK        = "AFK"
	LabelInProgress = "AFK:in-progress" // the Single-flight lock
	LabelDone       = "AFK:done"
	LabelFailed     = "AFK:failed"
	LabelPaused     = "AFK:paused"
	// LabelVerifyHuman marks an Agent PR a human verifies by hand: one
	// opened in the Bootstrap state, with no Environment provider to smoke.
	LabelVerifyHuman = "AFK:verify-human"
	// LabelNeedsHuman marks the issue a Parked Daemon keeps open.
	LabelNeedsHuman = "AFK:needs-human"

	wayfinderPrefix = "wayfinder:"
)

// stateLabels mark an AFK ticket the Daemon has already taken somewhere: it
// is no longer a pick candidate.
var stateLabels = []string{LabelInProgress, LabelDone, LabelFailed, LabelPaused}

// decisionTypes are the Decision ticket types the Daemon may resolve alone,
// by the label that marks them. Every other wayfinder type is HITL.
var decisionTypes = map[string]string{
	wayfinderPrefix + "research": "research",
	wayfinderPrefix + "task":     "task",
}

// Facts are what one pass reads before deciding anything: all of it from
// GitHub, but for LiveFire.
type Facts struct {
	// Login is the account the Daemon acts as; empty when it is not known.
	Login string
	// Pick is the Target Project's pick signal.
	Pick config.Pick
	// Held are the open issues holding the Single-flight lock, lowest first.
	Held []int
	// LiveFire is set when another Fire of this Daemon is running now: the
	// one fact that is the Host's and not GitHub's. A lock held while none
	// is running was left by a Fire that crashed.
	LiveFire bool
	// LockUnreadable is set when the lock could not be read. It counts as
	// held: the Daemon never starts work it cannot prove is alone.
	LockUnreadable bool
	// Queue is the open AFK tickets, as GitHub listed them.
	Queue []github.QueueIssue
	// AgentPRs are the open Agent PRs, by the ticket each one is for: of the
	// queue's tickets, or of the held or paused one.
	AgentPRs map[int]int
	// Paused are the open tickets labelled AFK:paused, lowest first.
	Paused []Paused
	// PauseCap is the Harness config's rounds.pause_resume; anything below
	// one is the schema default.
	PauseCap int
	// Parked is the Daemon's own open park issue, the lowest when there
	// are several; 0 when it is not Parked.
	Parked int
	// OpenPRs are the open pull requests from the repository's own
	// branches, with what the PR Triage read about each.
	OpenPRs []OpenPR
}

// Paused is a ticket a Step was cut off on by the usage limit.
type Paused struct {
	Issue int `json:"issue"`
	// Pauses is how many times it has paused; anything below one means the
	// count could not be read, and counts as one.
	Pauses int `json:"pauseCount"`
}

// DefaultPauseCap is the schema default of rounds.pause_resume.
const DefaultPauseCap = 3

// Kind names a derived state.
type Kind string

const (
	// KindParked: Claude's credential was found dead, and no Step runs
	// until a probe finds it alive again.
	KindParked Kind = "parked"
	// KindInFlight: the Single-flight lock is held by a live Fire, or cannot
	// be read, so this Fire does nothing.
	KindInFlight Kind = "in-flight"
	// KindCrashed: the Single-flight lock is held and no Fire is live.
	KindCrashed Kind = "crashed"
	// KindReconcile: an open Agent PR needs reconciling, ahead of any
	// ticket.
	KindReconcile Kind = "reconcile"
	// KindResume: a paused ticket is carried on with before any new pick.
	KindResume Kind = "resume"
	// KindPauseCap: a paused ticket has paused as often as it may.
	KindPauseCap Kind = "pause-cap"
	// KindPick: a Slice is eligible and would be implemented.
	KindPick Kind = "pick"
	// KindPickDecision: the eligible ticket is a Decision ticket.
	KindPickDecision Kind = "pick-decision"
	// KindIdle: nothing to do.
	KindIdle Kind = "idle"
)

// Action is what a Fire does next.
type Action string

const (
	ActionNone      Action = "none"
	ActionImplement Action = "implement"
	// ActionResume: take a paused ticket's Step up again.
	ActionResume Action = "resume"
	// ActionRecover: clear up after a Fire that crashed holding the lock,
	// then derive again.
	ActionRecover Action = "recover"
	// ActionRelease: take stale state labels off a ticket, and nothing else.
	ActionRelease Action = "release"
	// ActionFail: hand the ticket to a human with AFK:failed.
	ActionFail Action = "fail"
	// ActionProbe: probe Claude's credential, and un-park when it is alive.
	ActionProbe Action = "probe"
	// ActionReview: run the one-time review of an Agent PR that never had
	// it.
	ActionReview Action = "review"
	// ActionRevise: answer the open review threads of an Agent PR handed
	// back with AFK:revise.
	ActionRevise Action = "revise"
	// ActionRuling: answer the human's reply to a Ruling request: apply a
	// full Ruling, or nudge once.
	ActionRuling Action = "ruling"
	// ActionWaitCI: wait on the CI of an Agent PR whose review ran.
	ActionWaitCI Action = "wait-ci"
	// ActionRebase: rebase an Agent PR the default branch moved under.
	ActionRebase Action = "rebase"
	// ActionResearch: work a research Decision ticket as far as an open
	// research PR.
	ActionResearch Action = "research"
)

// State is what the Daemon would do now, and why.
type State struct {
	Kind     Kind `json:"state"`
	InFlight int  `json:"inFlight"`
	// Parked is the park issue; 0 when the Daemon is not Parked.
	Parked int `json:"parked"`
	// Pick is the ticket the queue yields; nil when there is none.
	Pick *Picked `json:"pick"`
	// Paused is the paused ticket this pass is about; nil when there is none.
	Paused *Paused `json:"paused"`
	// Reconcile is the Agent PR the reconcile Lane takes; nil when there is
	// none.
	Reconcile *Reconcile `json:"reconcile"`
	Next      Next       `json:"next"`
	// Skipped are AFK tickets passed over because they are mislabelled, or
	// because GitHub returned only part of them.
	Skipped []Skip `json:"skipped"`
	// NotHandled are the Lanes this build does not derive yet: work waiting
	// in them is invisible to it.
	NotHandled []string `json:"notHandled"`
}

// Picked is the ticket a Fire would take.
type Picked struct {
	Issue int    `json:"issue"`
	Title string `json:"title"`
	// Priority is the Project priority value; empty under a label-only pick.
	Priority string `json:"priority"`
	// Type is the Decision ticket type; empty for a Slice.
	Type string `json:"type"`
}

// Next is the action a Fire would take, or the reason it takes none.
type Next struct {
	Action Action `json:"action"`
	Issue  int    `json:"issue,omitempty"`
	// PR is the Agent PR the action is on, in the reconcile Lane.
	PR     int    `json:"pr,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Skip is one ticket passed over, with the reason.
type Skip struct {
	Issue  int    `json:"issue"`
	Reason string `json:"reason"`
}

// notHandled lists the Lanes later Slices add.
var notHandled = []string{"deployed"}

// Derive decides whether a Fire may start, and which Agent PR or ticket it
// would take.
// It is pure: it reads only facts and changes nothing, facts included.
func Derive(facts Facts) State {
	state := State{Kind: KindIdle, Next: Next{Action: ActionNone, Reason: "no eligible AFK ticket"}, Skipped: []Skip{}, NotHandled: slices.Clone(notHandled)}

	switch {
	case facts.LockUnreadable:
		state.Kind, state.InFlight = KindInFlight, max(len(facts.Held), 1)
		state.Next.Reason = "the Single-flight lock could not be read"
		return state
	case facts.LiveFire:
		state.Kind, state.InFlight = KindInFlight, len(facts.Held)
		state.Next.Reason = "another Fire of this Daemon is running"
		return state
	case facts.Parked > 0:
		// No ticket is touched while Parked, a crashed one included.
		state.Kind, state.Parked = KindParked, facts.Parked
		state.Next = Next{Action: ActionProbe, Reason: "Claude's credential was found dead: no Step runs until a probe finds it alive"}
		return state
	case len(facts.Held) > 0:
		// Nothing is running, so the Fire that took the lock died holding
		// it. Where its ticket stands is derived like any other.
		issue := facts.Held[0]
		state.Kind, state.InFlight = KindCrashed, len(facts.Held)
		if pr, open := facts.AgentPRs[issue]; open {
			state.Next = Next{Action: ActionRelease, Issue: issue, Reason: fmt.Sprintf("the Single-flight lock was left behind, and its Agent PR #%d is open: the ticket is the reconcile Lane's from here", pr)}
			return state
		}
		state.Next = Next{Action: ActionRecover, Issue: issue, Reason: "the Single-flight lock was left behind by a Fire that crashed"}
		return state
	}
	// A human waiting on an Agent PR is served before any ticket.
	if reconcile := Triage(facts.OpenPRs, facts.Login); reconcile != nil {
		state.Kind, state.Reconcile = KindReconcile, reconcile
		state.Next = reconcileNext(*reconcile, facts.OpenPRs)
		return state
	}
	switch {
	case len(facts.Paused) > 0:
		// In-flight work finishes before any new pick.
		paused := facts.Paused[0]
		var action Action
		action, paused.Pauses = pauseAction(paused.Pauses, facts.PauseCap)
		state.Paused = &paused
		switch pr, open := facts.AgentPRs[paused.Issue]; {
		case open:
			// Its work is done: only the label was left behind.
			state.Kind = KindResume
			state.Next = Next{Action: ActionRelease, Issue: paused.Issue, Reason: fmt.Sprintf("%s was left behind, and its Agent PR #%d is open: the ticket is the reconcile Lane's from here", LabelPaused, pr)}
		case action == ActionFail:
			state.Kind = KindPauseCap
			state.Next = Next{Action: ActionFail, Issue: paused.Issue, Reason: fmt.Sprintf("paused %d times, the cap", paused.Pauses)}
		default:
			state.Kind, state.Next = KindResume, Next{Action: ActionResume, Issue: paused.Issue}
		}
		return state
	}

	candidates, incomplete := ordered(facts)
	state.Skipped = append(state.Skipped, incomplete...)
	for _, c := range candidates {
		// A ticket with an open Agent PR is past the pick: it is the
		// reconcile Lane's from here.
		if pr, open := facts.AgentPRs[c.issue.Number]; open {
			state.Skipped = append(state.Skipped, Skip{c.issue.Number, fmt.Sprintf("its Agent PR #%d is open: the ticket is the reconcile Lane's from here", pr)})
			continue
		}
		wayfinder := wayfinderLabels(c.issue.Labels)
		decisionType, isDecision := "", false
		switch {
		case len(wayfinder) > 1:
			state.Skipped = append(state.Skipped, Skip{c.issue.Number, "ambiguous wayfinder labels (" + strings.Join(wayfinder, ",") + ")"})
			continue
		case len(wayfinder) == 1:
			if decisionType, isDecision = decisionTypes[wayfinder[0]]; !isDecision {
				state.Skipped = append(state.Skipped, Skip{c.issue.Number, wayfinder[0] + " is not an AFK ticket type"})
				continue
			}
		}
		state.Pick = &Picked{Issue: c.issue.Number, Title: c.issue.Title, Priority: c.priority, Type: decisionType}
		switch {
		case isDecision && decisionType == "research":
			state.Kind = KindPickDecision
			state.Next = Next{Action: ActionResearch, Issue: c.issue.Number}
		case isDecision:
			state.Kind = KindPickDecision
			state.Next = Next{Action: ActionNone, Issue: c.issue.Number, Reason: "the Decision ticket Lane's " + decisionType + " tickets are not handled by this build yet"}
		default:
			state.Kind = KindPick
			state.Next = Next{Action: ActionImplement, Issue: c.issue.Number}
		}
		return state
	}
	return state
}

// pauseAction is what to do with a ticket that has paused pauses times:
// resume it below the cap, fail it at the cap, so that a ticket too big for
// one budget window goes to a human instead of bouncing across windows for
// ever. It also returns the count it went by: one that could not be read is
// one pause, so that the ticket resumes rather than fails on a read glitch.
func pauseAction(pauses, limit int) (Action, int) {
	if limit < 1 {
		limit = DefaultPauseCap
	}
	if pauses < 1 {
		pauses = 1
	}
	if pauses >= limit {
		return ActionFail, pauses
	}
	return ActionResume, pauses
}

// candidate is an eligible ticket with its place in the order.
type candidate struct {
	issue    github.QueueIssue
	priority string
	rank     int
}

// ordered is the eligible tickets, first to be picked first: by the Project
// priority field where the pick signal is a board, then oldest first. A
// ticket GitHub returned only part of is never eligible: what is missing
// could be an open blocker or a human assignee. Those come back as skipped.
func ordered(facts Facts) (candidates []candidate, incomplete []Skip) {
	for _, issue := range facts.Queue {
		if issue.Incomplete != "" {
			incomplete = append(incomplete, Skip{issue.Number, "GitHub did not return its " + issue.Incomplete})
			continue
		}
		if !eligible(issue, facts.Login) {
			continue
		}
		c := candidate{issue: issue}
		if p := facts.Pick.Project; facts.Pick.Shape == config.PickProject && p != nil {
			item, onBoard := projectItem(issue, p.Number)
			if !onBoard {
				continue
			}
			c.priority = item.Priority
			if c.priority == "" && len(p.Order) > 0 {
				// An unset field counts as the last value of the order.
				c.priority = p.Order[len(p.Order)-1]
			}
			c.rank = rank(p.Order, c.priority)
		}
		candidates = append(candidates, c)
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		if candidates[a].rank != candidates[b].rank {
			return candidates[a].rank < candidates[b].rank
		}
		return candidates[a].issue.CreatedAt < candidates[b].issue.CreatedAt
	})
	return candidates, incomplete
}

// eligible applies the rules that do not depend on the pick signal.
func eligible(issue github.QueueIssue, login string) bool {
	for _, label := range issue.Labels {
		for _, taken := range stateLabels {
			if label == taken {
				return false
			}
		}
	}
	// Native blockers only, all closed. A truncated page could hide an open
	// one, so it fails safe.
	if issue.BlockersTruncated {
		return false
	}
	for _, blocker := range issue.Blockers {
		if blocker.State != "CLOSED" {
			return false
		}
	}
	// Never race a human: no assignee other than the Daemon itself. With no
	// known login, only an unassigned ticket qualifies.
	for _, assignee := range issue.Assignees {
		if login == "" || assignee != login {
			return false
		}
	}
	return true
}

func projectItem(issue github.QueueIssue, project int) (github.QueueProjectItem, bool) {
	for _, item := range issue.ProjectItems {
		if item.Project == project {
			return item, true
		}
	}
	return github.QueueProjectItem{}, false
}

// rank is a priority value's place in the configured order; a value outside
// it ranks after every value in it.
func rank(order []string, priority string) int {
	for i, value := range order {
		if value == priority {
			return i
		}
	}
	return len(order)
}

// wayfinderLabels are the issue's wayfinder labels, sorted so the result
// never depends on the order GitHub lists labels in.
func wayfinderLabels(labels []string) []string {
	var kinds []string
	for _, label := range labels {
		if strings.HasPrefix(label, wayfinderPrefix) {
			kinds = append(kinds, label)
		}
	}
	sort.Strings(kinds)
	return kinds
}
