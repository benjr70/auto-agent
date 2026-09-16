---
name: wayfinder
description:
  Plan a huge chunk of work (more than one agent session can hold) as a shared
  map of decision tickets on your issue tracker, and resolve them one at a time
  until the way to the destination is clear. This is the auto-agent plugin
  fork: same flow, publishing through the Target Project's Harness config pick
  signal.
disable-model-invocation: true
---

A loose idea has arrived, too big for one agent session, and wrapped in fog: the
way from here to the **destination** isn't visible yet. Wayfinding is about
finding that way, not charging at the destination. This skill charts the way as
a **shared map** on the repo's issue tracker, then works its **decision
tickets** (questions whose resolution is a decision, not slices of a build to
execute) one at a time until the route is clear.

The destination varies per effort, and naming it is the first act of charting:
it shapes every ticket. It might be a spec to hand off and iterate on, a
decision to lock before planning starts, or a change made in place like a
data-structure migration. The map is domain-agnostic: engineering work, course
content, whatever fits the shape.

This is the auto-agent plugin fork of the upstream skill: same flow, plus the
harness labels, publication onto the Target Project's pick signal (see
[Labels, pick signal and priority](#labels-pick-signal-and-priority)), and
chart-time research fired as `/auto-agent:afk-resolve` subagents. The tracker
commands live in [Tracker operations](#tracker-operations) below; vocabulary
(Map, Decision ticket, Spec, Slice, AFK, HITL) in the Target Project's
`CONTEXT.md` when it has one.

## Harness context

Every repo fact comes from the Harness config (ADR 0002); nothing below names a
repo, a board or a path from memory. Inside a Fire the wrapper exported
`AUTO_AGENT_ROOT` and `HARNESS_CONFIG_JSON`; invoked interactively, the Harness
install is the one the plugin was loaded from:

```bash
AA="${AUTO_AGENT_ROOT:-${CLAUDE_PLUGIN_ROOT%/plugin}}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")                  # the Target Project, from its origin remote
OWNER=$(jq -r .repo.owner <<<"$CFG")
NAME=$(jq -r .repo.name <<<"$CFG")
PICK_SHAPE=$(jq -r .pick.shape <<<"$CFG")           # project | labels
RESEARCH_PREFIX=$(jq -r .docs_research_prefix <<<"$CFG")   # where findings are persisted
```

The libs referenced below live under the Harness install's `lib/`, reached
through `"$AA" <command>`; never hand-roll what they own (label bootstrap,
pick-signal publication).

## Plan, don't do

Wayfinder is **planning** by default: each ticket resolves a decision, and the
map is done when the way is clear, with nothing left to decide before someone
goes and does the thing. The pull to just do the work is usually the signal
you've reached the edge of the map and it's time to hand off. An effort can
override this in its **Notes**, carrying execution into the map itself, but
absent that, produce decisions, not deliverables.

## Refer by name

Every map and ticket is an issue, so it has a **name**: its title. In everything
the human reads (narration, the map's Decisions-so-far), refer to it by that
name, never by a bare id, number, or slug. A wall of `#42, #43, #44` is
illegible; names read at a glance. The id and URL don't vanish; a name wraps its
link, but they ride _inside_ the name, never stand in for it.

## The Map

The map is a single issue on this repo's issue tracker, labelled
`wayfinder:map`, the canonical artifact. Its tickets are child issues of the
map.

The map is an **index**, not a store. It lists the decisions made and points at
the tickets that hold their detail; a decision lives in exactly one place, its
ticket, so the map never restates it, only gists it and links.

**Where the map, its child tickets, blocking, and frontier queries physically
live is tracker-specific.** The Target Project's tracker is GitHub, reached
through `gh` against `$REPO`: the exact commands (map label, sub-issues, native
dependencies, frontier query, claim, resolve) are in
[Tracker operations](#tracker-operations).

### The map body

The whole map at low resolution, loaded once per session. Open tickets are
**not** listed: they are open child issues, found by query.

```markdown
## Destination

<what reaching the end of this map looks like: the spec, decision, or change
this effort is finding its way to. One or two lines; every session orients to it
before choosing a ticket.>

## Notes

<domain; skills every session should consult; standing preferences for this
effort>

## Decisions so far

<!-- the index: one line per closed ticket, enough to judge relevance, then zoom the link for the detail the ticket holds -->

- [<closed ticket title>](link): <one-line gist of the answer>

## Not yet specified

<!-- see "Fog of war": in-scope fog you can't ticket yet; graduates as the frontier advances -->

## Out of scope

<!-- see "Out of scope": work ruled beyond the destination; closed, never graduates -->
```

### Tickets

Each ticket is a **child issue** of the map; the tracker's issue id is its
identity. Its body is the question, sized to one 100K token agent session:

```markdown
## Question

<the decision or investigation this ticket resolves>
```

Each ticket carries a `wayfinder:<type>` label, one of `research`, `prototype`,
`grilling`, `task` (see [Ticket Types](#ticket-types)), plus `AFK` or `HITL` per
[Labels, pick signal and priority](#labels-pick-signal-and-priority).

A session **claims** a ticket by assigning it to the dev driving the map,
**first**, before any work, so concurrent sessions skip it. That assignee _is_
the claim: an open, unassigned ticket is unclaimed.

Blocking uses the tracker's **native** dependency relationship: essential
because it renders the frontier _visually_ in the tracker's own UI, so the human
sees what's takeable without opening the map. Only a tracker that lacks native
blocking falls back to a body convention. A ticket is **unblocked** when every
ticket blocking it is closed; the **frontier** is the open, unblocked, unclaimed
children, the edge of the known.

The answer isn't part of the body; it's recorded on resolution (see
[Work through the map](#work-through-the-map)). Assets created while resolving a
ticket are linked from the issue, not pasted in.

## Ticket Types

Every ticket is either **HITL** (human in the loop, worked _with_ a human who
speaks for themselves) or **AFK**, driven by the agent alone. A HITL ticket only
resolves through that live exchange; the agent never stands in for the human's
side of it (a grilling agent that answers its own questions has broken this).

- **Research** (AFK): Reading documentation, third-party APIs, or local
  resources like knowledge bases to surface a fact a decision waits on. Resolved
  by an `/auto-agent:afk-resolve` subagent (which calls the Skill tool with
  `auto-agent:research` and persists its findings under `$RESEARCH_PREFIX`).
  Use when knowledge outside the current working directory is required.
- **Prototype** (HITL): Raise the fidelity of the discussion by making a cheap,
  rough, concrete artifact to react to (an outline, a rough take, a stub, or
  UI/logic code) by calling the Skill tool with "prototype". Links the prototype
  as an asset. Use when "how should it look" or "how should it behave" is the
  key question.
- **Grilling** (HITL): Conversation. The default case. Always call the Skill
  tool twice, for `auto-agent:grilling` and `auto-agent:domain-modeling`.
- **Task** (HITL or AFK): Manual work that must happen before a _decision_ can
  be made: nothing to decide, prototype, or research, but the discussion is
  blocked until it's done. Signing up for a service so its API can be judged,
  provisioning access, moving data so its shape can be seen. This is the one
  type that _does_ rather than decides, and it earns its place by unblocking a
  decision, not by delivering the destination. The agent drives it alone where
  it can (AFK); otherwise it hands the human a precise checklist (HITL).
  Resolved when the work is done; the answer records what was done and any
  resulting facts (credentials location, new URLs, row counts) later tickets
  depend on.

## Labels, pick signal and priority

Type decides who may take the ticket, and that routing is what the Daemon reads:

| Ticket type         | Labels                         | Pick signal                                                              |
| ------------------- | ------------------------------ | ------------------------------------------------------------------------ |
| `research`          | `wayfinder:research` + `AFK`   | published via `pick-publish`, default Priority the order's second entry  |
| `task`, human-free  | `wayfinder:task` + `AFK`       | published via `pick-publish`, default Priority the order's second entry  |
| `task`, needs human | `wayfinder:task` + `HITL`      | never published                                                          |
| `grilling`          | `wayfinder:grilling` + `HITL`  | never published                                                          |
| `prototype`         | `wayfinder:prototype` + `HITL` | never published                                                          |

The pick signal is whatever the Harness config's `pick` block declares, and the
Daemon reads nothing else: under a Project pick an `AFK` ticket missing from the
configured Project is silently never picked, and a published `HITL` ticket gets
picked when it must not be; under a label-only pick the `AFK` label itself is
the signal and tickets are taken oldest first. The `pick-publish` lib owns the
difference, so the prose here never branches on the shape:

```bash
gh issue create --repo "$REPO" --label wayfinder:research --label AFK ...

# per AFK ticket, after it exists
"$AA" pick-publish publish --issue <N> --priority "$PRIORITY"
```

Under a Project pick it adds the item and sets the configured priority field;
it exits 1 with a `.reason` when the Priority did not land, and an item with no
Priority is read as the lowest value, so **report that failure**, never assume
it landed. Under a label-only pick the same call is a no-op printing
`projected: false`. `"$AA" pick-publish unpublish --issue <N>` takes a ticket
off the signal again (a ticket re-routed to `HITL`).

**Priority quiz, once per batch, only when `PICK_SHAPE` is `project`.** Ask with
AskUserQuestion; the options are the configured order
(`jq -r '.pick.project.order[]' <<<"$CFG"`), the default its second entry, and
that one answer applies to every AFK ticket in the batch. Under a label-only
pick there is no quiz: there is nothing to rank. Running autonomously (no human
in the session), skip the quiz and use the default.

**Labels are bootstrapped once, before any issue is created**, by the harness
lib, create-if-missing with curated colours, never `gh label create --force`
(which rewrites the colour and description of a label that already exists):

```bash
"$AA" labels-ensure    # AFK, HITL, spec, the AFK:* states, wayfinder:map/grilling/prototype/research/task
```

## Fog of war

The map is _deliberately_ incomplete: don't chart what you can't yet see. Beyond
the live tickets lies the **fog of war**: the dim view of decisions and
investigations you can tell are coming but can't yet pin down, because they hang
on questions still open. Resolving a ticket clears the fog ahead of it,
graduating whatever's now specifiable into fresh tickets, one at a time, until
the way to the destination is clear and no tickets remain.

The map's **Not yet specified** section is where that dim view is written down:
the suspected question, the area to revisit later. It's the undiscovered
frontier _toward_ the destination: everything here is in scope, just not sharp
enough to ticket. Write as loosely or as fully as the view allows; it doubles as
a signpost for collaborators reading where the effort is headed.

**Fog or ticket?** The test is whether you can state the question precisely now,
_not_ whether you can answer it now.

- **Ticket when** the question is already sharp, even if it's blocked and you
  can't act on it yet.
- **Not yet specified when** you can't yet phrase it that sharply. Don't
  pre-slice the fog into ticket-sized pieces: it's coarser than a ticket, and
  one patch may graduate into several tickets, or none, once the frontier
  reaches it.

**Not yet specified** excludes what's already decided (Decisions so far), what's
already a live ticket, and what's out of scope (the next section).

## Out of scope

Fog only ever gathers _toward_ the destination. The destination fixes the scope,
so work beyond it is **out of scope**: it isn't fog, and it doesn't belong in
**Not yet specified**. It gets its own **Out of scope** section on the map: work
you've consciously ruled out of _this_ effort. Scope, not sharpness, lands it
here.

Out-of-scope work never graduates (the frontier stops at the destination), so it
returns only if the destination is redrawn, and then as a fresh effort, not a
resumption.

Ruling something out of scope is a scoping act, not a step on the route. When a
ticket that already exists turns out to sit past the destination (mis-scoped in
while charting, or exposed by a resolution), **close it** (a closed ticket is
unambiguously off the frontier) and leave one line in the **Out of scope**
section: the gist plus why it's out of scope, linking the closed ticket. It
stays out of **Decisions so far**, which records the route actually walked; a
scope boundary isn't a step on it.

## Tracker operations

The Target Project's tracker is GitHub Issues on `$REPO`. Every command below
carries `--repo "$REPO"` (or the `repos/$REPO/...` API path) so it works from
any cwd, including a Harness install beside the checkout.

- **Map**: one issue labelled `wayfinder:map`, holding the body above.
  `gh issue create --repo "$REPO" --label wayfinder:map --title ... --body-file ...`.
- **Child ticket**: an issue linked to the map as a GitHub **sub-issue**. Both
  the sub-issue and the dependency endpoints take the child's numeric
  **database id**, not the `#number` and not the `node_id`:

  ```bash
  gh api "repos/$REPO/issues/<n>" --jq .id                                    # -> database id
  gh api -X POST "repos/$REPO/issues/<map>/sub_issues" -F sub_issue_id=<child-database-id>
  ```

  Labels: `wayfinder:<type>` plus `AFK` or `HITL`. Once claimed, the ticket is
  assigned to the driving dev.
- **Blocking**: GitHub's **native issue dependencies**, the canonical,
  UI-visible representation. Add an edge on the **blocked** issue:

  ```bash
  gh api -X POST "repos/$REPO/issues/<blocked>/dependencies/blocked_by" -F issue_id=<blocker-database-id>
  ```

  GitHub reports `issue_dependencies_summary.blocked_by` (open blockers only,
  the live gate). A duplicate edge returns an error that is safe to ignore;
  report any other failure rather than retrying blindly. A ticket is unblocked
  when every blocker is closed. Body prose (`Blocked by #N`) is never parsed.
- **Frontier query**: the map's open children (`gh issue list --repo "$REPO"
  --state open`, scoped to the map's sub-issues), dropping any with
  `issue_dependencies_summary.blocked_by > 0` or an assignee; first in map
  order wins.
- **Claim**: `gh issue edit <n> --repo "$REPO" --add-assignee @me`, the
  session's first write.
- **Resolve**: `gh issue comment <n> --repo "$REPO" --body "<answer>"`, then
  `gh issue close <n> --repo "$REPO"`, then read the map body, append the
  context pointer (gist + link) to its **Decisions so far**, and write it back
  with `gh issue edit <map> --repo "$REPO" --body-file <file>`.
- **Spec and Slices**: a Spec is a child of the map (`/auto-agent:to-spec`
  adds it as a sub-issue); Slices are children of the Spec
  (`/auto-agent:to-tickets`). A Spec carries `spec` and never `AFK`.

## Invocation

Two modes. Either way, **never resolve more than one ticket per session**, with
the exception of research tickets.

### Chart the map

User invokes with a loose idea.

1. **Name the destination.** Call the Skill tool twice, for
   `auto-agent:grilling` and `auto-agent:domain-modeling`, to pin down what this
   map is finding its way to: the spec, decision, or change. The destination
   fixes the scope, so it's settled first.
2. **Map the frontier.** Grill again, **breadth-first** this time: fan out
   across the whole space rather than deep on any one thread, surfacing the open
   decisions and the first steps takeable now. **If this surfaces no fog** (the
   way to the destination is already clear, the whole journey small enough for
   one session), you don't need a map. Stop and ask the user how they'd like to
   proceed.
3. **Create the map** (label `wayfinder:map`, after `"$AA" labels-ensure`):
   Destination and Notes filled in, Decisions-so-far empty, the fog sketched
   into **Not yet specified**.
4. **Create the tickets you can specify now** as child issues of the map,
   labelled and published per
   [Labels, pick signal and priority](#labels-pick-signal-and-priority), then
   wire blocking edges in a **second pass** (issues need ids before they can
   reference each other). Wiring sorts them into the frontier and the blocked;
   everything you can't yet specify stays in the fog: the **Not yet specified**
   section.
5. **Fire the research subagents.** For each `research` ticket you just created,
   spin up an `/auto-agent:afk-resolve --issue <N> --type research` subagent
   (Agent tool, `general-purpose`, no model pin) to resolve it in parallel. It
   follows the same persistence protocol the Daemon uses: findings land as a
   docs-only PR under `$RESEARCH_PREFIX<map-slug>/<ticket-slug>.md`, never on
   a throwaway branch. Its protocol is owned by `/auto-agent:afk-resolve`.
6. Stop: charting is one session's work; it hand-resolves nothing.

### Work through the map

User invokes with a map (URL or number). A ticket is **optional**: without one,
you pick the next decision, not the user.

1. Load the **map**: the low-res view, not every ticket body.
2. Choose the ticket. If the user named one, use it. Otherwise take the first
   frontier ticket in order. **Claim it**: assign it to yourself before any
   work.
3. Resolve it. **Zoom as needed**: fetch the full body of any related or closed
   ticket on demand; call the Skill tool for whichever skills the `## Notes`
   block names. If in doubt, call the Skill tool twice, for
   `auto-agent:grilling` and `auto-agent:domain-modeling`.
4. Record the resolution: post the answer as a **resolution comment**, **close**
   the issue, and **append a context pointer** to the map's Decisions-so-far.
5. Add newly-surfaced tickets (create-then-wire, published per the table);
   graduate any fog the answer has made specifiable, clearing each graduated
   patch from **Not yet specified** so it lives only as its new ticket. If the
   answer reveals that a ticket (this one or another) sits beyond the
   destination, **rule it out of scope** rather than resolving it on the route.
   If the decision invalidates other parts of the map, update or delete those
   tickets.

The user may run unblocked tickets in parallel, so expect other sessions to be
editing the tracker concurrently.
