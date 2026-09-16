---
name: to-tickets
description:
  Break a Spec, plan, or the current conversation into tracer-bullet Slices and
  publish them as GitHub issues with native blocking edges, sub-issue links,
  AFK/HITL labels and, when the Harness config's pick block names a Project,
  Project membership with a Priority; label-only otherwise. Use when a Spec
  needs cutting into implementation tickets.
disable-model-invocation: true
---

# To Tickets

Break a Spec, plan, or conversation into **Slices**: tracer-bullet vertical
slices, each declaring the Slices that **block** it. This is the harness fork
of the upstream skill: same flow, but it publishes tickets the Daemon can
actually pick up (`AFK` / `HITL` labels, native GitHub dependencies, sub-issues
of the Spec, and membership on the Target Project's pick signal, whatever shape
the Harness config declares).

Vocabulary (Spec, Slice, AFK, HITL, Map): the Target Project's `CONTEXT.md` and
`docs/adr/` when it has them.

## Invocation

```
/auto-agent:to-tickets [<spec-issue-number|url>] [--dry-run]
```

- `--dry-run` — print the planned issues (titles, type, blockers, full bodies)
  and the commands that would run, then stop. Mutates nothing: no issues, no
  labels, no pick-signal membership, no dependency edges.

## Harness context

Every repo fact comes from the Harness config (ADR 0002); nothing below names a
repo, a board or a path from memory. This skill also runs interactively outside
a Fire, so the install root falls back to the plugin's parent:

```bash
AA="${AUTO_AGENT_ROOT:-${CLAUDE_PLUGIN_ROOT%/plugin}}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")                  # the Target Project, from its origin remote
PICK_SHAPE=$(jq -r .pick.shape <<<"$CFG")           # project | labels
PRIORITY_ORDER=$(jq -c '.pick.project.order' <<<"$CFG")   # e.g. ["P0","P1","P2"]; null under a label-only pick
```

The libs referenced here live under `$AUTO_AGENT_ROOT/lib/` and are reached
through `"$AA" <command>`; never hand-roll what they own.

## Process

### 1. Gather context

Work from what is already in context. If the user passed a Spec (path, issue
number, URL), fetch its full body and comments:
`gh issue view <n> --repo "$REPO" --comments`.

### 2. Explore the codebase

If you have not already explored the codebase, do so. Read the Target Project's
`CONTEXT.md` for the domain glossary and the relevant `docs/adr/` entries when
it has them: Slice titles and bodies use that vocabulary and respect those
decisions.

Look for prefactoring that makes the implementation easier. "Make the change
easy, then make the easy change."

### 3. Draft vertical slices

<vertical-slice-rules>

- Each Slice cuts a narrow but COMPLETE path through every layer (schema, API,
  UI, tests): vertical, NOT a horizontal slice of one layer
- A completed Slice is demoable or verifiable on its own
- Each Slice is sized to fit in a single fresh context window
- Any prefactoring is its own Slice, first

</vertical-slice-rules>

Give each Slice its **blocking edges**: the Slices that must close before it can
start. A Slice with no blockers can start immediately.

Mark each Slice **AFK** or **HITL**. AFK Slices are implementable and mergeable
without a human in the loop; HITL Slices need live human judgement
(architectural decision, infra cutover, design review, credentials). Prefer AFK.

**Wide refactors are the exception to vertical slicing.** A wide refactor is one
mechanical change (rename a column, retype a shared symbol) whose blast radius
fans across the codebase, so a single edit breaks thousands of call sites and no
vertical slice can land green. Sequence it as **expand–contract**: expand (add
the new form beside the old), then migrate call sites in batches sized by blast
radius (each batch its own Slice blocked by the expand, CI green batch to
batch), then contract (delete the old form, blocked by every batch). When even
the batches cannot stay green alone, let them share an integration branch that
all block a final integrate-and-verify Slice; green is promised only there.

### 4. Quiz the user

Present the breakdown as a numbered list. Per Slice: **Title**, **Type** (AFK /
HITL), **Blocked by**, **What it delivers**.

Ask:

- Does the granularity feel right? (too coarse / too fine)
- Are the blocking edges correct — does each Slice depend only on Slices that
  genuinely gate it?
- Should any Slices be merged or split further?
- Are the right Slices marked HITL?

Iterate until the user approves.

Then the **Priority quiz**, only when `PICK_SHAPE` is `project`, and **once per
batch, not per Slice** — with AskUserQuestion: what Priority should the AFK
Slices in this batch carry on the configured Project? The options are the
values of `$PRIORITY_ORDER`; the **default is its last entry** (the lowest
priority). One answer applies to every AFK Slice in the batch. Under a
label-only pick there is no Priority and no quiz: the `AFK` label itself is the
signal and tickets are taken oldest first.

**Running autonomously** (no human in the session): skip the quiz and use the
default. Never stall waiting for a user who is not there.

### 5. Bootstrap labels (idempotent)

Run before creating any issue, one call:

```bash
"$AA" labels-ensure
```

It creates `AFK`, `HITL`, `spec`, every `AFK:*` run-state label and the
`wayfinder:*` labels, **create-if-missing only**, never `--force`: `--force`
rewrites the colour and description of a label that already exists, so a
bootstrap that used it would silently overwrite curated metadata on every run.
A non-zero exit names the label it could not create; report it rather than
creating issues that will fail to label.

### 6. First pass — create the issues

Create one issue per Slice, in dependency order (blockers first) so the
informational `## Blocked by` mirror can link real issues. Use the body template
below verbatim, section order included.

- **AFK Slice** → `gh issue create --repo "$REPO" --label AFK ...`
- **HITL Slice** → `gh issue create --repo "$REPO" --label HITL ...`

Never `Spawned by` on a Slice — that line belongs to wayfinder fog graduation.
Do NOT close or modify the Spec issue beyond adding sub-issue links (§7).

<issue-template>

## Parent

Spec: #<spec-issue-number>

## What to build

The end-to-end behaviour this Slice makes work, from the user's perspective, not
layer-by-layer implementation. Reference sections of the Spec instead of
duplicating them.

## Acceptance criteria

- [ ] Criterion 1
- [ ] Criterion 2

## User stories addressed

Numbers from the Spec's User Stories list: 3, 7, 11

## Interface changes

The modules, services or interfaces created or modified. Specific about what
changes; no file paths, no code snippets.

- Module/interface 1: what changes
- Module/interface 2: what changes

## Behaviors to test

Observable behaviours verified through public interfaces, each mapped to an
acceptance criterion.

1. Behaviour description (AC 1)
2. Behaviour description (AC 2)

## Testing priority

- **Critical**: behaviours 1, 2 (must have tests)
- **Nice-to-have**: behaviour 3 (test if time permits)

## Blocked by

- [Blocking slice title](https://github.com/<owner>/<repo>/issues/N)

Or "None — can start immediately".

</issue-template>

The `## Blocked by` section is an **informational name mirror** for human
readers: names wrapping links, never bare numbers. The picker does not parse it
— the gate is the native edge wired in §7.

Avoid file paths and code snippets: they go stale fast. Exception: a prototype
snippet that encodes a decision more precisely than prose can (state machine,
reducer, schema, type shape). Inline the decision-rich part only, and say it
came from a prototype.

### 7. Second pass — wire edges and sub-issues

Issues need ids before they can reference each other, so this runs after every
Slice exists. Both APIs take the **numeric database id**, not the `#number` and
not the `node_id`:

```bash
gh api "repos/$REPO/issues/<n>" --jq .id     # -> database id
```

For each blocking pair, add the native dependency on the **blocked** issue:

```bash
gh api -X POST "repos/$REPO/issues/<blocked-number>/dependencies/blocked_by" \
  -F issue_id=<blocker-database-id>
```

For each Slice, add it as a **sub-issue of the Spec**:

```bash
gh api -X POST "repos/$REPO/issues/<spec-number>/sub_issues" \
  -F sub_issue_id=<slice-database-id>
```

Both endpoints are idempotent-ish: a duplicate edge returns an error that is
safe to ignore. Report any other failure to the user rather than retrying
blindly.

### 8. Third pass — publish to the pick signal (AFK only)

The publishing shape follows the pick block, and the lib owns the difference.
For each AFK Slice, with the Priority answered in the §4 quiz (omit
`--priority` under a label-only pick, or to take the default):

```bash
"$AA" pick-publish publish --issue <N> --priority <P>
```

- **Project pick**: the lib adds the issue to the configured Project and sets
  the configured priority field to `<P>`. An AFK Slice that is not on the
  Project is silently invisible to `/auto-agent:afk-pickup`, and an item whose
  Priority never landed is read as the lowest value, so the lib exits 1 with a
  `.reason` rather than pretending. `priority-edit-failed` means the item IS on
  the board with no Priority: report it to the user, never move on as if it
  landed. Any other non-zero `.reason` (`item-add-failed`, `option-missing`,
  `project-unreadable`, …) is reported the same way.
- **Label-only pick**: the lib is a no-op and prints `projected: false`; the
  `AFK` label applied in §6 already is the signal, and the picker takes AFK
  tickets oldest first.

**HITL Slices are never published** — pick-signal membership is what the Daemon
reads, and a published HITL Slice would be picked up.

### 9. Report

Print the created Slices as names with links, their type, Priority (Project
pick only), and the edges wired. Say which Slices are on the frontier (no open
blockers) — those are takeable now.
