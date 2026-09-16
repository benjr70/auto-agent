---
name: to-spec
description:
  "Turn the current conversation into a Spec and publish it as a GitHub issue
  labelled `spec`: no interview, just synthesis of what you've already
  discussed. Use at a Map's destination or after a grilling session."
disable-model-invocation: true
---

# To Spec

Take the current conversation context and codebase understanding and produce a
**Spec**: the issue Slices are cut from and reviewed against. Do NOT run a
discovery interview — the decisions were made in the conversation that got you
here, so synthesize what you already know. The only round-trips are the two
short confirmations in steps 2 and 3 (seams, modules), and they exist to catch a
mis-synthesis, not to gather new requirements.

**Running autonomously** (no human in the session — the AFK/Daemon path): skip
both confirmations, write the seams and modules into the Spec as stated
assumptions, and say in `## Further Notes` that they are unconfirmed. Never
stall waiting for a user who is not there.

This is the harness fork of the mattpocock skill: same flow, plus a
`## Module design` section and the harness's labelling. Vocabulary (Spec, Slice,
Map, AFK, HITL) comes from the Target Project's `CONTEXT.md` and `docs/adr/`
when it has them.

## Harness context

Every repo fact comes from the Harness config (ADR 0002); nothing below names a
repo, a board or a path from memory. This skill also runs interactively, outside
a Fire, so the harness root falls back to the plugin's parent.

```bash
AA="${AUTO_AGENT_ROOT:-${CLAUDE_PLUGIN_ROOT%/plugin}}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")              # the Target Project, from its origin remote
```

The libs referenced here live under the harness install's `lib/`; never
hand-roll what they own.

## Process

1. Explore the repo to understand the current state of the codebase, if you have
   not already. Read the Target Project's `CONTEXT.md` for the glossary and the
   relevant `docs/adr/` entries when they exist; use that vocabulary throughout
   the Spec and respect those decisions.

2. Sketch the **seams** at which the feature will be tested. Prefer existing
   seams to new ones, and the highest seam possible. The fewer seams across the
   codebase, the better — the ideal number is one. Interactive session: check
   with the user that these seams match their expectations.

3. Sketch the **modules** to build or modify, looking for deep modules (a lot of
   functionality behind a simple, testable interface that rarely changes).
   Interactive session: check with the user that the modules match their
   expectations, and **which of them they want tests written for**. This becomes
   `## Module design`.

4. Write the Spec using the template below and publish it. `gh issue create`
   fails if the label does not exist yet (a fresh Target Project), so bootstrap
   the harness labels first. The lib creates every harness label
   create-if-missing with its curated colour and description, and never uses
   `--force` (which would rewrite the metadata of a label that already exists):

   ```bash
   "$AA" labels-ensure

   gh issue create --repo "$REPO" --label spec --title "<Spec title>" --body-file <spec.md>
   ```

   A Spec is **never** labelled `AFK` — it is not implementable work — and is
   never put on the pick signal (never published with `pick-publish`).

   If the Spec is born from a wayfinder Map, make `Part of #<map>` the **first
   line of the body**, then add it as a sub-issue of the Map. The endpoint takes
   the numeric **database id**, not the `#number` and not the `node_id`:

   ```bash
   gh api "repos/$REPO/issues/<spec>" --jq .id     # database id
   gh api -X POST "repos/$REPO/issues/<map>/sub_issues" \
     -F sub_issue_id=<spec-database-id>
   ```

5. Tell the user the Spec's name and link, and that
   `/auto-agent:to-tickets <spec>` cuts it into Slices.

<spec-template>

## Problem Statement

The problem the user is facing, from the user's perspective.

## Solution

The solution, from the user's perspective.

## User Stories

A LONG, numbered list, each in the form:

1. As an <actor>, I want a <feature>, so that <benefit>

Extremely extensive: cover every aspect of the feature. Slices reference these
by number, so the numbering is a contract — do not renumber after Slices exist.

## Implementation Decisions

The decisions already made: modules built or modified and their interfaces,
technical clarifications from the developer, architectural decisions, schema
changes, API contracts, specific interactions.

No file paths, no code snippets — they go stale fast. Exception: a prototype
snippet that encodes a decision more precisely than prose can (state machine,
reducer, schema, type shape); inline the decision-rich part and note it came
from a prototype.

## Module design

The deep modules this effort builds or modifies, one line each: the module, its
interface (what it takes and what it returns), whether it is new or deepened,
and **whether it gets tests**. Say explicitly which modules are not
unit-testable (prose, skills, infra) and how they are covered instead.

- **<module>** (new | existing, deepened): interface in one line. Tested / not
  tested, and why.

## Testing Decisions

What makes a good test here (external behaviour through the public interface,
never implementation details), which modules are tested, and prior art in the
codebase for each kind of test.

## Out of Scope

What is deliberately not part of this Spec.

## Further Notes

Anything else: rollout order, prerequisites, related Maps or research.

</spec-template>
