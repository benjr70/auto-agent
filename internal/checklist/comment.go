package checklist

import (
	"fmt"
	"strings"
)

// Verdict is what a round found for one item.
type Verdict string

const (
	// Pass: exercised on a real Surface and it worked; the box is ticked.
	Pass Verdict = "PASS"
	// Defer: justified as not runnable here (deployed-env or hardware); the
	// box stays empty for a later round or a human.
	Defer Verdict = "DEFER"
	// Fail: exercised and it did not work, or deferred without a
	// justification; the box stays empty.
	Fail Verdict = "FAIL"
)

// Result is one item's outcome in a round, as the manual-verifier reported
// it.
type Result struct {
	Item    Item
	Verdict Verdict
	// Classification is how the verifier classed the item: local,
	// deployed-env or hardware.
	Classification string
	// Evidence is the concrete evidence, one line each: status codes,
	// request lines, log excerpts, screenshot filenames.
	Evidence []string
	// Deferral is why a deferred item was not run here: the deployed
	// environment it needs or the named hardware blocker.
	Deferral string
}

// Round is one Verification round's outcome, what its evidence comment
// says.
type Round struct {
	// N and Max are the round and the cap; N 0 is a round with no number.
	N, Max  int
	Results []Result
	// Dir is the round's evidence directory.
	Dir string
	// Sandbox is the detail of a DEGRADED app sandbox; empty when it was OK
	// or no app was launched.
	Sandbox string
}

func (r Round) tally() (pass, deferred, fail int) {
	for _, res := range r.Results {
		switch res.Verdict {
		case Pass:
			pass++
		case Defer:
			deferred++
		default:
			fail++
		}
	}
	return pass, deferred, fail
}

// Line is the round's `manual-verify:` line, the caller's contract.
func (r Round) Line() string {
	pass, deferred, fail := r.tally()
	line := fmt.Sprintf("manual-verify: %d/%d PASS, %d deferred, %d FAIL", pass, len(r.Results), deferred, fail)
	if r.Sandbox != "" {
		line += " (sandbox DEGRADED)"
	}
	return line
}

// Passed is the Text of every item that passed: what Tick is handed.
func (r Round) Passed() []string {
	var passed []string
	for _, res := range r.Results {
		if res.Verdict == Pass {
			passed = append(passed, res.Item.Text)
		}
	}
	return passed
}

// Comment is the round's one evidence comment: the heading PR Triage reads
// a round by, the tally, a DEGRADED sandbox when there was one, a section per
// item with its verdict, classification, evidence and, for a deferral, why
// and the post-deploy spec it carries, and last the evidence directory.
func Comment(r Round) string {
	var b strings.Builder
	b.WriteString("### Manual verification")
	if r.N > 0 {
		fmt.Fprintf(&b, " — round %d/%d", r.N, r.Max)
	}
	pass, deferred, fail := r.tally()
	fmt.Fprintf(&b, "\n\n**Result: %d/%d PASS, %d deferred, %d FAIL.**\n", pass, len(r.Results), deferred, fail)
	if r.Sandbox != "" {
		fmt.Fprintf(&b, "\n> **Sandbox DEGRADED:** %s\n", r.Sandbox)
	}
	for i, res := range r.Results {
		fmt.Fprintf(&b, "\n#### %d. %s — %s", i+1, res.Item.Label(), res.Verdict)
		if res.Classification != "" {
			fmt.Fprintf(&b, " (%s)", res.Classification)
		}
		b.WriteString("\n")
		var lines []string
		for _, e := range res.Evidence {
			lines = append(lines, "- "+e)
		}
		if res.Verdict == Defer && res.Deferral != "" {
			lines = append(lines, "- **Deferred:** "+res.Deferral)
		}
		if res.Item.PostDeploy != "" {
			lines = append(lines, "- **Post-deploy spec:** `"+res.Item.PostDeploy+"`")
		}
		if len(lines) > 0 {
			b.WriteString("\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	if r.Dir != "" {
		fmt.Fprintf(&b, "\nEvidence directory: `%s`\n", r.Dir)
	}
	return b.String()
}

// InfraComment is the comment of a round whose environment did not boot, or
// whose Surfaces could not be launched: an infrastructure error, with no
// verdict on any item, since nobody exercised one.
func InfraComment(n, max int, detail, dir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Manual verification — round %d/%d: infrastructure error\n\n", n, max)
	fmt.Fprintf(&b, "**No verdicts.** No item was exercised and no box is ticked: %s\n", detail)
	if dir != "" {
		fmt.Fprintf(&b, "\nEvidence directory: `%s`\n", dir)
	}
	return b.String()
}

// InfraLine is the `manual-verify:` line of a round that gave no verdict.
func InfraLine(detail string) string {
	return fmt.Sprintf("manual-verify: infra-error — %s (0 items verified)", detail)
}

// BootstrapComment is the one comment on a PR whose head declares no
// hermetic tier: there is no environment to boot and no round to run.
func BootstrapComment() string {
	return "### Manual verification — no round: Bootstrap state\n\n" +
		"The Harness config on this PR's head declares no hermetic tier, so there is no Environment provider to boot and no round to run. " +
		"`AFK:verify-human` is applied: a human verifies this PR by hand.\n"
}
