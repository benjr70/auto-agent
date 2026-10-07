---
status: accepted
---

# The correctness review is the plugin's own skill, behind a posting bar

The one-time review of an Agent PR has a correctness axis that was written to
invoke Claude Code's bundled review by its bare name. On the first Host that
name resolved to a personal Standards-and-Spec skill of the same name, so for
every recorded Fire the correctness axis ran a style reviewer and tagged its
duplication and test-structure notes as bugs; every surviving finding became
an inline thread the reconciler had to fix or dispute, and six PRs in a row
parked for a human who then sided with the implementer on all thirteen threads
(Spec #74). We decided the plugin **ships its own bug-hunting review as a
skill**, `/auto-agent:correctness-review`, and calls it by that namespaced
name: plugin skills sit outside the enterprise-over-personal-over-project
shadowing order, so every Host runs the same prompt and the bar lives in one
file this repo owns and tests. We did not reach the bundled review through its
alias, because an alias is itself a bare name and ties the review to the
Claude Code version; and we did not embed the prompt in the wrapper, because a
skill is checked by the runbook tables and can be invoked by a human alone.
The same decision fixes **what may open a thread**: a Finding is a defect, a
product ambiguity or a Review note, and only a defect (a concrete failure with
its inputs, or a written requirement contradicted and quoted) opens one.
Severity is not the bar. Standards findings never open a thread and no
Standards pass runs in the Agent PR loop at all; a Review note is listed once,
collapsed, in the done-marker comment and nobody acts on it; a product
ambiguity rides in the done-marker as structured data for the Ruling request
and never becomes a thread; a review that produced only notes applies no
`AFK:revise`. The ten-thread cap is gone: every defect gets a thread, and a
defect GitHub refuses to anchor is posted on its file's first changed line
with the intended location named, never folded into a summary. The reviewer's
sorting is trusted; the orchestrator (`lib/review-poster.sh`) enforces only
the mechanical part, demoting a defect with no failure scenario and no quoted
requirement to a Review note. The cost we accept is a hand-maintained review
prompt that will not improve when the bundled one does, and a review that
deliberately says nothing about style.
