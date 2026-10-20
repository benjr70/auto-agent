package state

import (
	"slices"
	"strings"

	"github.com/benjr70/auto-agent/internal/github"
)

// The labels of the harness's vocabulary that nothing in the Daemon reads,
// but which the skills that write tickets put on them.
const (
	LabelHITL = "HITL"
	LabelSpec = "spec"
	LabelMap  = wayfinderPrefix + "map"
)

// HarnessLabels is the whole label vocabulary, as the Daemon creates it in a
// Target Project that lacks any of it: name, colour and description.
var HarnessLabels = []github.Label{
	{Name: LabelAFK, Color: "1D76DB", Description: "Agent-grabbable: the Daemon may pick it up"},
	{Name: LabelHITL, Color: "5319E7", Description: "Resolves only through live exchange with a human; never picked by the Daemon"},
	{Name: LabelSpec, Color: "0052CC", Description: "Spec issue produced by to-spec; parent of Slices"},
	{Name: LabelInProgress, Color: "FBCA04", Description: "Single-flight lock: a Fire is working it"},
	{Name: LabelDone, Color: "0E8A16", Description: "Completed by the Daemon"},
	{Name: LabelFailed, Color: "B60205", Description: "Daemon attempt failed; needs human triage"},
	{Name: LabelChecksFailed, Color: "D93F0B", Description: "Agent PR: CI or verification failed after the fix loop was exhausted"},
	{Name: LabelRevise, Color: "0052CC", Description: "Hand-back: the Daemon must address this PR's unresolved review comments"},
	{Name: LabelReviseFailed, Color: "B60205", Description: "Agent PR: fixes still failing at the revise round cap"},
	{Name: LabelRuling, Color: "FBCA04", Description: "Agent PR: a Ruling request awaits the human's one-line reply (1A 2B)"},
	{Name: LabelRebaseFailed, Color: "B60205", Description: "Agent PR: automatic rebase onto the default branch failed; human rebase required"},
	{Name: LabelPaused, Color: "FBCA04", Description: "Fire cut off by usage exhaustion; awaiting resume next window"},
	{Name: LabelDepsFailed, Color: "B60205", Description: "Dependabot PR: verify/fix loop exhausted; human triage required"},
	{Name: LabelVerifyHuman, Color: "C5DEF5", Description: "Agent PR opened in Bootstrap state: no Environment provider, a human verifies"},
	{Name: LabelNeedsHuman, Color: "E99695", Description: "The Daemon is parked: a human must act (dead credential, hand-off)"},
	{Name: LabelMap, Color: "0E8A16", Description: "Wayfinder map issue"},
	{Name: wayfinderPrefix + "grilling", Color: "FBCA04", Description: "Wayfinder grilling ticket (HITL)"},
	{Name: wayfinderPrefix + "prototype", Color: "D4C5F9", Description: "Wayfinder prototype ticket (HITL)"},
	{Name: wayfinderPrefix + "research", Color: "C5DEF5", Description: "Wayfinder research ticket (AFK)"},
	{Name: wayfinderPrefix + "task", Color: "BFD4F2", Description: "Wayfinder task ticket"},
}

// Taken is whether an AFK ticket's labels say the Daemon has already taken
// it somewhere, so that it is no longer a pick candidate.
func Taken(labels []string) bool {
	for _, label := range labels {
		if slices.Contains(stateLabels, label) {
			return true
		}
	}
	return false
}

// WayfinderType is the type a ticket's first wayfinder label names
// ("research", "grilling", …), in the order the labels are listed; empty
// when it carries none.
func WayfinderType(labels []string) string {
	for _, label := range labels {
		if kind, ok := strings.CutPrefix(label, wayfinderPrefix); ok {
			return kind
		}
	}
	return ""
}
