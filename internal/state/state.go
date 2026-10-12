// Package state derives what the Daemon should do next from GitHub facts
// alone. Nothing here is stored and nothing here writes: the same facts
// always give the same state (ADR 0012).
package state

import (
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

// Facts are what one pass reads from GitHub before deciding anything.
type Facts struct {
	// Login is the account the Daemon acts as; empty when it is not known.
	Login string
	// Pick is the Target Project's pick signal.
	Pick config.Pick
	// InFlight is how many open issues hold the Single-flight lock.
	InFlight int
	// LockUnreadable is set when the lock could not be read. It counts as
	// held: the Daemon never starts work it cannot prove is alone.
	LockUnreadable bool
	// Queue is the open AFK tickets, as GitHub listed them.
	Queue []github.QueueIssue
}

// Kind names a derived state.
type Kind string

const (
	// KindInFlight: the Single-flight lock is held, so this Fire does nothing.
	KindInFlight Kind = "in-flight"
	// KindPick: a Slice is eligible and would be implemented.
	KindPick Kind = "pick"
	// KindPickDecision: the eligible ticket is a Decision ticket.
	KindPickDecision Kind = "pick-decision"
	// KindIdle: nothing to do.
	KindIdle Kind = "idle"
)

// The actions a Fire can take next.
const (
	ActionNone      = "none"
	ActionImplement = "implement"
)

// State is what the Daemon would do now, and why.
type State struct {
	Kind     Kind `json:"state"`
	InFlight int  `json:"inFlight"`
	// Pick is the ticket the queue yields; nil when there is none.
	Pick *Picked `json:"pick"`
	Next Next    `json:"next"`
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
	Action string `json:"action"`
	Issue  int    `json:"issue,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Skip is one ticket passed over, with the reason.
type Skip struct {
	Issue  int    `json:"issue"`
	Reason string `json:"reason"`
}

// notHandled lists the Lanes later Slices add.
var notHandled = []string{"reconcile", "resume", "deployed"}

// Derive decides whether a Fire may start and which ticket it would take.
// It is pure: it reads only facts and changes nothing, facts included.
func Derive(facts Facts) State {
	state := State{Kind: KindIdle, Next: Next{Action: ActionNone, Reason: "no eligible AFK ticket"}, Skipped: []Skip{}, NotHandled: slices.Clone(notHandled)}

	if facts.InFlight > 0 || facts.LockUnreadable {
		state.Kind = KindInFlight
		state.InFlight = max(facts.InFlight, 1)
		state.Next.Reason = "the Single-flight lock is held"
		if facts.InFlight == 0 {
			state.Next.Reason = "the Single-flight lock could not be read"
		}
		return state
	}

	candidates, incomplete := ordered(facts)
	state.Skipped = append(state.Skipped, incomplete...)
	for _, c := range candidates {
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
		if isDecision {
			state.Kind = KindPickDecision
			state.Next = Next{Action: ActionNone, Issue: c.issue.Number, Reason: "the Decision ticket Lane is not handled by this build yet"}
		} else {
			state.Kind = KindPick
			state.Next = Next{Action: ActionImplement, Issue: c.issue.Number}
		}
		return state
	}
	return state
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
