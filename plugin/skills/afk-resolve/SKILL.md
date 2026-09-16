---
name: afk-resolve
description:
  Resolve one wayfinder Decision ticket end to end without a human: claim it,
  run the research (or do the human-free task), persist findings under the
  Harness config's research prefix behind a docs-only PR, merge it through
  the docs-only gate, post the resolution comment, close the ticket, append
  the Map's Decisions so far, and graduate fog into new tickets. Invoked by
  `/auto-agent:afk-pickup` §2b on verdict `pick-wayfinder`, by
  `/auto-agent:wayfinder` for chart-time research, by `/auto-agent:afk-pickup`
  §1.2 with `--finish-merged` after it merges a research PR out-of-band, and by
  `bin/auto-agent fire --resolve-dry-run <N>` with `--dry-run`. Takes the
  ticket number, optionally its type and slug.
disable-model-invocation: true
---

# AFK Resolve — Autonomous Decision-Ticket Resolver

You resolve **one** wayfinder Decision ticket per invocation. A Decision ticket
is a child of a **Map** whose resolution is a decision or a fact, never a change
to the product — so this skill writes no application code, opens no
`feat/issue-*` branch, and spawns no implementer.

Vocabulary (Map, Decision ticket, Frontier, Fog, Resolve) is the harness's own;
read the Target Project's `CONTEXT.md` when it has one. The Map's format, its
label rules and the tracker operations are owned by `/auto-agent:wayfinder` —
read that skill for anything about the Map itself; this skill owns only the
resolution protocol.

## Invocation

```
/auto-agent:afk-resolve --issue <N> [--type <research|task>] [--slug <slug>]
/auto-agent:afk-resolve --issue <N> --type research --finish-merged --pr <P>
/auto-agent:afk-resolve --issue <N> [--type research] --dry-run
```

`--issue` is required. `--type` comes from the picker's `.pick.type` (the
ticket's `wayfinder:research` / `wayfinder:task` label); when the caller did
not pass it, read it from the ticket's labels (§1) — never from the title.
`--slug` is the caller's already-computed ticket slug (`/auto-agent:afk-pickup`
§2b passes the one it printed on the `resolve:` marker line): when present it
is used **verbatim** for the branch, the file and every report line, so the
branch the Fire wrapper may have to delete is exactly the one the marker names.
`wayfinder:grilling` and `wayfinder:prototype` are HITL types and are **never**
resolvable here — refuse and exit if one arrives (§1). `--finish-merged` is
§5b; `--dry-run` is §9.

## Harness context

Every repo fact comes from the Harness config the Fire wrapper resolved and
exported (ADR 0002); nothing below names a repo, a branch, a board or a path
from memory. Invoked at chart time by `/auto-agent:wayfinder` outside a Fire,
the Harness install is the one the plugin was loaded from:

```bash
AA="${AUTO_AGENT_ROOT:-${CLAUDE_PLUGIN_ROOT%/plugin}}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")              # the Target Project, from its origin remote
OWNER=$(jq -r .repo.owner <<<"$CFG")            # the repo owner (gh project commands)
REPO_NAME=$(jq -r .repo.name <<<"$CFG")         # the repo name (GraphQL variables)
BASE=$(jq -r .repo.default_branch <<<"$CFG")    # detected from GitHub, never declared

RESEARCH_PREFIX=$(jq -r .docs_research_prefix <<<"$CFG")   # ends with a slash; the docs-only rule's prefix
PICK_SHAPE=$(jq -r .pick.shape <<<"$CFG")                  # project | labels (§6b, §8)
```

`AUTO_AGENT_ROOT`, `AUTO_AGENT_TARGET_DIR`, `AUTO_AGENT_STATE_DIR` and
`HARNESS_CONFIG_JSON` are exported by the Fire wrapper, so every `"$AA" …` call
below needs no target argument and makes no extra `gh repo view`. The libs
referenced here live under `$AUTO_AGENT_ROOT/lib/`; never hand-roll what they
own. The findings file is always `${RESEARCH_PREFIX}${MAP_SLUG}/${SLUG}.md`
under the Target Project checkout (`$AUTO_AGENT_TARGET_DIR`).

## Process

### 1. Claim (assignee + lock)

The claim is the first write, before any reading of sources, so a concurrent
session or Fire skips the ticket. **`--dry-run` skips every write in this
section** (see §9) and only performs the reads.

```bash
LOGIN=$(gh api user -q .login)
gh issue edit "$N" --repo "$REPO" --add-assignee "$LOGIN"
gh issue edit "$N" --repo "$REPO" --add-label AFK:in-progress    # idempotent: §2b already did it
```

Then load the ticket and its Map. The Map is the ticket's GitHub **parent**; the
parent number, not the body, is the truth:

```bash
TICKET=$(gh issue view "$N" --repo "$REPO" --json title,body,labels)
TICKET_TITLE=$(jq -r '.title' <<<"$TICKET")
QUESTION=$(jq -r '.body' <<<"$TICKET")
WF_LABELS=$(jq -r '[.labels[].name | select(startswith("wayfinder:"))] | join(" ")' <<<"$TICKET")
MAP=$(gh api graphql -f query='
  query($owner: String!, $name: String!, $n: Int!) {
    repository(owner: $owner, name: $name) {
      issue(number: $n) { parent { number title body } } } }' \
  -f owner="$OWNER" -f name="$REPO_NAME" -F n="$N" \
  --jq '.data.repository.issue.parent')
MAP_N=$(printf '%s' "$MAP"     | jq -r '.number')
MAP_TITLE=$(printf '%s' "$MAP" | jq -r '.title')
```

**Type.** When `--type` was not passed, `WF_LABELS` decides: exactly
`wayfinder:research` → `research`, exactly `wayfinder:task` → `task`. Any other
`wayfinder:*` type (`grilling`, `prototype`), or more than one `wayfinder:*`
label, is a HITL or ambiguous ticket that reached the AFK queue by mislabelling:
release the lock you just took (`--remove-label AFK:in-progress`, no other
label), print `resolve: FAILED — #<N> not-afk-type` and stop. A passed `--type`
of `grilling` or `prototype` is refused the same way.

Slugs are derived the same way everywhere (lowercase, non-alphanumerics to
hyphens, trimmed, 60 chars) — the branch, the file path and the `resolve:` log
line must all agree. The **ticket** slug is derived here only when the caller
did not pass one; `--slug` always wins, so a caller that already published the
slug (and whose cleanup deletes `research/<slug>`) can never disagree with the
branch this skill pushes:

```bash
slugify() { printf '%s' "$1" | tr 'A-Z' 'a-z' | sed -E 's/[^a-z0-9]+/-/g; s/^-+|-+$//g' | cut -c1-60; }
MAP_SLUG=$(slugify "$MAP_TITLE")
SLUG=${ARG_SLUG:-$(slugify "$TICKET_TITLE")}    # --slug verbatim when given
FINDINGS="${RESEARCH_PREFIX}${MAP_SLUG}/${SLUG}.md"
```

Emit the structured marker (`/auto-agent:afk-pickup` §2b prints the same line,
with the same `--slug`, before invoking; printing it here as well is harmless
and makes a `/auto-agent:wayfinder` chart-time invocation — which passes no
`--slug` — scrapeable). "Emit" means **write the line in your own assistant
message**, on its own line, verbatim — never `echo` it from a Bash call, whose
stdout the Fire wrapper never reads:

```
resolve: #<N> <research|task> <SLUG>
```

If the Map cannot be read, continue anyway with `MAP_SLUG=unmapped` and note it
in the resolution comment — a resolvable ticket is never blocked on its index.

**Task tickets** branch here: go to [§8 Task tickets](#8-task-tickets). Research
tickets continue.

### 2. Research

Call the Skill tool with **`auto-agent:research`** (the vendored upstream
skill), passing the ticket's `## Question` verbatim plus the Map's Destination
and Notes as context, and this persistence convention: findings go to
`${RESEARCH_PREFIX}${MAP_SLUG}/${SLUG}.md` under the Target Project checkout
(`$AUTO_AGENT_TARGET_DIR`), every claim cited to the primary source that owns
it. The skill spins up a background agent; **wait for it to finish** before
continuing — nothing below is valid until the file exists.

Never answer from memory. Primary sources only (official docs, source code,
specs, first-party APIs, live probes against the Target Project). A live probe
is a source: record the exact command and its output.

**No sources found** — the question cannot be answered from primary sources
without a human decision — is a failure, not an empty file: go to
[§7 Failure](#7-failure) with reason `no-sources`.

### 3. The findings file

One file per ticket, at `$FINDINGS`, opening with this header so the file
explains itself away from GitHub:

```markdown
# <Title of the finding, not the ticket>

Ticket: [#<N>](https://github.com/<REPO>/issues/<N>) (part of wayfinder map
[#<MAP_N>](https://github.com/<REPO>/issues/<MAP_N>) — <MAP_TITLE>).
Researched on <YYYY-MM-DD>.

Sources: <the primary sources consulted — doc URLs, files read, live probes run>

## TL;DR

- <the answer, in the fewest lines that survive being read alone>

## <sections with the evidence, each claim cited>
```

The four header facts — **ticket, map, date, sources** — are mandatory; a file
missing any of them is not done. Write nothing outside `$RESEARCH_PREFIX` — the
docs-only gate refuses the merge otherwise, and rightly.

### 4. Branch, commit, PR

```bash
git fetch origin "$BASE" --quiet
git checkout -B "research/$SLUG" "origin/$BASE"
git add "$FINDINGS"
git commit -m "docs(research): <short description> (#$N)"
git push -u origin "research/$SLUG"
```

The PR title is the fixed harness shape `docs(research): <short description>
(#<N>)` — `docs` type, `research` scope, the ticket number in parentheses — so
no title lint the Target Project runs needs consulting and the `commit_scopes`
list does not apply to it:

```bash
PR_TITLE="docs(research): <short description> (#$N)"
PR_URL=$(gh pr create --repo "$REPO" --base "$BASE" --head "research/$SLUG" \
  --title "$PR_TITLE" --body "$PR_BODY")
PR=$(printf '%s' "$PR_URL" | grep -oE '[0-9]+$')
```

`PR_BODY` states the question, the answer in one paragraph, and
`Resolves #<N> — <ticket title>`. Do **not** write `Closes #<N>`: this skill
closes the ticket itself in §6, with the resolution comment attached, and a
GitHub auto-close on merge would close it bare.

`PR_BODY` must also carry this machine marker on its own line — it is what lets
a **later** Fire that merges this PR find its way back to the ticket (§5b), so a
research PR that lands out-of-band never orphans its Decision ticket:

```markdown
<!-- afk-resolve ticket:#<N> map:#<MAP_N> slug:<SLUG> -->
```

### 5. Green it, then merge it through the gate

Hand CI to **`/auto-agent:pr-watch`** via the `Agent` tool
(`subagent_type: general-purpose`, no `model:` pin so the Fire's model policy
carries through, `run_in_background: false` — blocking; never proceed while it
is in flight):

> Invoke the /auto-agent:pr-watch skill with --pr \<PR> --branch
> research/\<SLUG> --issue \<N>. Return the terminal `pr-watch:` line verbatim.

`pr-watch: PASS` continues. `pr-watch: DRAFT` (fix loop exhausted) or
`pr-watch: ERROR` → [§7 Failure](#7-failure) with that line as the reason.

A research PR earns no `/auto-agent:pr-review` and no `/auto-agent:verify-pr`
round: it changes no code. Green CI plus the **docs-only gate**'s own re-read of
the real diff is the whole bar. The gate is the deep module
`$AUTO_AGENT_ROOT/lib/docs-only-gate.sh` (`"$AA" docs-only-gate`): it reads the
repo, the default branch, the research prefix and the required checks from the
Harness config, **decides**, and prints the exact merge command; running that
command verbatim is the only sanctioned way to land this PR. Never hand-roll a
`gh pr merge`.

```bash
HEAD_SHA=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid)
git fetch origin "$BASE" --quiet
git fetch origin "refs/pull/$PR/head" --quiet
GATE_JSON=$("$AA" docs-only-gate --head "$HEAD_SHA" --pr "$PR" --check-state)
GATE_RC=$?

if [ "$GATE_RC" -eq 0 ]; then
    eval "$(printf '%s' "$GATE_JSON" | jq -r '.mergeCmd')"
fi
```

On a successful merge emit the second structured marker (the Dashboard and the
Fire wrapper both read these lines):

```
docs-merge: PR #<PR> <HEAD_SHA>
```

A refusal is **not** a resolve failure to hide: the research is written and the
PR is open, it just did not land. Report the gate's `.reason` verbatim on the
`docs-merge:` line (`REFUSED — <reason>` for `not-docs-only`,
`checks-not-green`, `checks-missing`, `checks-unreadable`;
`ERROR — gate could not run: <reason>` for `head-missing`, `git-failed`,
`usage`, which are harness errors, never verdicts about the PR), leave the PR
open for the next Fire's §1.2 `docs-merge` triage, and go to
[§7 Failure](#7-failure) with that reason — the ticket stays open, because its
findings are not on the default branch yet and the resolution comment must link
the default branch.

The hand-off is only half the story: the Fire that eventually merges the PR
must come **back** here to finish the ticket, or the findings land on the
default branch with the ticket stuck `AFK:failed` forever — commented on by
nobody, closed by nobody, absent from the Map. That return trip is §5b, and the
`<!-- afk-resolve … -->` marker in the PR body (§4) is what makes it findable.
Say so on the ticket's failure comment: _the PR is open; merging it finishes
this ticket automatically._

### 5b. Finish a resolve whose PR merged later

```
/auto-agent:afk-resolve --issue <N> --type research --finish-merged --pr <P>
```

`/auto-agent:afk-pickup` §1.2 fires this immediately after its `docs-merge`
branch merges a PR whose body carries the `<!-- afk-resolve ticket:#<N> … -->`
marker — i.e. exactly the PRs this skill left behind on a `gate-refused`
failure. The research already exists and is now on the default branch, so this
mode does **no** research, writes no file, opens no PR and creates no branch:

1. Re-read the marker for `MAP_N` / `MAP_SLUG` / `SLUG` (the PR body is the
   record; do not re-derive them from titles), and read the merged file at
   `${RESEARCH_PREFIX}${MAP_SLUG}/${SLUG}.md` on `origin/$BASE` for its TL;DR.
2. Clear the stale failure:
   `gh issue edit "$N" --repo "$REPO" --remove-label AFK:failed`.
3. Resume at [§6 Resolve the ticket](#6-resolve-the-ticket) and run it to the
   end — resolution comment linking the default branch, close, `AFK:done`, Map
   append, §6b fog graduation. Terminal line is §6's usual
   `resolve: DONE — #<N> closed, PR #<P> merged <sha>`.

If the ticket is already closed, this mode is a no-op: print
`resolve: DONE — #<N> closed, PR #<P> merged <sha>` and stop. Re-running it is
therefore safe, which is what makes it callable from a best-effort tail.

### 6. Resolve the ticket

Only after the merge landed. The resolution comment is the **gist plus the
link** — the detail lives in the file, and the comment must point at the file on
the **default branch**, not at the branch (which is deleted on squash-merge):

```bash
gh issue comment "$N" --repo "$REPO" --body "$(cat <<EOC
<one-paragraph gist of the answer — enough to judge relevance without opening anything>

Full findings: [$FINDINGS](https://github.com/$REPO/blob/$BASE/$FINDINGS) (merged in #$PR).
EOC
)"
gh issue close "$N" --repo "$REPO"
gh issue edit "$N" --repo "$REPO" --remove-label AFK:in-progress --add-label AFK:done
```

Then append one line to the Map's **Decisions so far**, under the existing
entries, referring to the ticket **by name** (never a bare number):

```markdown
- [<ticket title>](https://github.com/<REPO>/issues/<N>):
  <one-line gist of the answer>
```

Read the Map body, insert the line, and write it back with
`gh issue edit "$MAP_N" --repo "$REPO" --body-file`. The append is
**best-effort**: a failure here is logged as
`resolve: WARN — map #<MAP_N> append failed: <reason>` and the resolve still
counts as done. The ticket carries the answer; the Map is an index, and an index
can be rebuilt.

### 6b. Fog graduation

An answer clears fog ahead of it. Graduate only what the answer made
**specifiable** — the test is whether you can state the question sharply now,
not whether you can answer it. Under these guardrails, which are hard bounds,
not preferences:

| Guardrail    | Rule                                                                                                                                                                                                                                                                                                                              |
| ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Volume       | At most **3** new tickets per resolve. Beyond that, leave the rest in the Map's **Not yet specified**.                                                                                                                                                                                                                            |
| Parentage    | Every new ticket is a **sub-issue of the Map** (`gh api -X POST repos/$REPO/issues/$MAP_N/sub_issues -F sub_issue_id=<child database id>`; the id is `gh api repos/$REPO/issues/<n> --jq .id`).                                                                                                                                  |
| Blocking     | Native dependencies only (`gh api -X POST repos/$REPO/issues/<child>/dependencies/blocked_by -F issue_id=<blocker database id>`), never body prose.                                                                                                                                                                              |
| Provenance   | The body carries a `Spawned by #<N>` line naming the ticket that surfaced it.                                                                                                                                                                                                                                                     |
| Depth        | Depth = the length of the `Spawned by` chain back to a human-created ticket. A ticket at depth ≤ 2 may be `AFK`.                                                                                                                                                                                                                  |
| Routing      | `AFK` only for `wayfinder:research` and human-free `wayfinder:task` at depth ≤ 2. Everything else gets `HITL`.                                                                                                                                                                                                                    |
| Priority     | Every new `AFK` ticket goes on the pick signal via `"$AA" pick-publish publish --issue <n> --priority "$P1"`, where `P1=$(jq -r '.pick.project.order[1] // .pick.project.order[0]' <<<"$CFG")` — the second entry of the configured order. Under a label-only pick the call is a no-op (`projected:false`). `HITL` tickets are never published. Check the exit status: a non-zero exit means the Priority did not land; report it, never assume. |
| Scope        | The Map's **Destination** and **Out of scope** sections are **never** edited here — redrawing scope is a human act.                                                                                                                                                                                                               |
| No recursion | Never resolve a ticket you just created, in this session or by firing another resolve. The next Fire picks it up.                                                                                                                                                                                                                 |

Depth is what stops a runaway: a research answer that spawns research that
spawns research is `HITL` at the third generation, so a human sees the branch
before it grows again. Count the chain by following `Spawned by #<n>` up the
ancestors; a ticket with no such line is depth 0.

New tickets carry `wayfinder:<type>` plus `AFK` or `HITL`; the labels exist
because `"$AA" labels-ensure` bootstraps every harness label create-if-missing
— run it once before the first `gh issue create` of a graduation batch.

Clear each graduated patch out of the Map's **Not yet specified** in the same
edit that appends Decisions so far, so a question lives either as fog or as a
ticket, never both.

### 7. Failure

Any of these leaves the ticket **open** for a human, labelled `AFK:failed` with
a comment naming what happened. Never invent an answer, and never close a ticket
you could not resolve.

| Reason               | Trigger                                                     | Comment says                                                                             |
| -------------------- | ----------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `research-error`     | the `auto-agent:research` skill errored or returned nothing usable | what it was asked, what came back                                                  |
| `no-sources`         | no primary source answers the question                      | which sources were tried, what is missing                                                |
| `pr-watch-exhausted` | `pr-watch: DRAFT` / `ERROR` on the research PR              | the verbatim `pr-watch:` line and the PR link                                            |
| `gate-refused`       | the docs-only gate refused or could not run                 | the verbatim `.reason`, the PR link, and that merging the PR finishes the ticket via §5b |

```bash
gh issue edit "$N" --repo "$REPO" --remove-label AFK:in-progress --add-label AFK:failed
gh issue comment "$N" --repo "$REPO" --body "afk-resolve FAILED at $(date -u +%FT%TZ) — <reason>: <detail>. Ticket left open for human triage."
```

Terminal line: `resolve: FAILED — #<N> <reason>`.

**Usage exhaustion** is not a failure and is not handled here: the run simply
stops mid-protocol. The Fire wrapper (`bin/auto-agent fire`) detects the
`resolve:` marker line, drops the `AFK:in-progress` lock, deletes any pushed
`research/<SLUG>` branch and applies **no** label, so the next Fire restarts the
resolve from scratch. That undo is bounded by the terminal line: once
`resolve: DONE …` has been printed the ticket is closed and answered, so a later
cutoff (or crash) in the best-effort tail — the Map append, fog graduation, the
caller's token accounting — only drops the lock and leaves `AFK:done`. This is
why §6 prints nothing else between closing the ticket and that terminal line.
Nothing in this skill should try to pre-empt that — in particular, never apply
`AFK:paused` to a Decision ticket.

### 8. Task tickets

A `wayfinder:task` ticket does the work rather than deciding: provisioning
access, moving data, signing up for a service so its API can be judged. Same
skeleton, minus the file and the PR — there is nothing to persist under
`$RESEARCH_PREFIX`.

1. Do the work with the tools available (§1's claim already happened).
2. Post a resolution comment recording **what was done** and every fact later
   tickets depend on: where credentials live, new URLs, row counts, versions.
3. Close the ticket, `AFK:in-progress → AFK:done`, append the Map's Decisions so
   far, graduate fog under §6b's guardrails.
4. Terminal line: `resolve: DONE — #<N> closed (task)`.

**A task that needs a code change is not a task.** Slices are cut from a Spec
and implemented by the core lane; a Decision ticket that turns out to demand
product code is mis-typed. Do not write the code:

```bash
gh issue edit "$N" --repo "$REPO" --remove-label AFK --remove-label AFK:in-progress --add-label HITL
gh issue comment "$N" --repo "$REPO" --body "afk-resolve: this task needs a change to product code, which a Decision ticket never carries — relabelled HITL for a human to route (as a Slice off a Spec, or by re-scoping the ticket). What it would take: <one paragraph>."
"$AA" pick-publish unpublish --issue "$N"
```

The `pick-publish unpublish` call takes the ticket off the pick signal
(membership of the configured Project is the Daemon's pick signal, so a
projected `HITL` ticket would be picked again; under a label-only pick the call
is a no-op). Terminal line: `resolve: DONE — #<N> relabelled HITL (needs code)`.

### 9. Dry run

```
/auto-agent:afk-resolve --issue <N> [--type research] --dry-run
```

The Fire seam `bin/auto-agent fire --resolve-dry-run <N>` prompts this mode; it
is what proves the resolve lane works on a Target Project (the fixture, a fresh
Host) without touching its tracker. A dry run **mutates nothing on GitHub or
git**: no claim, no label, no comment, no branch, no commit, no push, no PR, no
Map edit, no fog graduation. Because it only reads, it works on a closed ticket
too — any `wayfinder:research` ticket with a Map parent will do.

It **does**:

1. Run §1's reads only (ticket, labels, Map, type, `SLUG`, `FINDINGS`).
2. Run §2 — the real `auto-agent:research` call — and write the findings file
   (§3's header included) at `$FINDINGS` under the checkout. That file is the
   artifact a human inspects; it is left untracked in the working tree, and the
   wrapper's next plain Fire resets the checkout.
3. Once the file exists, **reply with exactly the two lines below and stop.**
   Write them yourself, in the assistant message that ends the run — do not
   `echo` them from Bash, do not wrap them in a summary, a heading, a checklist
   or a code block.

A task ticket is not dry-runnable (there is nothing to write and the work would
be the mutation): print `afk-resolve: would-skip #<N> task tickets have no dry
run` and stop. §7's `no-sources` and `research-error` still apply: report
`afk-resolve: would-fail #<N> <reason>` instead of the `would-open` line, with
no label and no comment.

**Output discipline.** The Fire wrapper reads ONLY the text you write in your
own assistant messages (and the final result), never the stdout of a Bash tool
call: a line that only ever appeared as `echo` output does not exist to it. The
final message of a dry run is therefore exactly two lines, verbatim, on their
own lines, nothing else — no summary, no "what would happen next", no ✅ list:

```
resolve: #<N> research <SLUG>
afk-resolve: would-open PR research/<SLUG> (<FINDINGS>)
```

`<FINDINGS>` is the file's path relative to the checkout. A dry run whose reply
carries no `afk-resolve: would-` line is reported by the wrapper as
`work=unknown` and fails.

## Output format

Emit the marker lines as they happen (they are the machine contract), then one
terminal line the caller pastes into its report:

```
resolve: #<N> <research|task> <slug>              (§1, before any interruptible work)
docs-merge: PR #<P> <sha>                          (§5, research only, on merge)
resolve: WARN — map #<M> append failed: <reason>   (§6, best-effort append only)
resolve: DONE — #<N> closed, PR #<P> merged <sha>  (research)
resolve: DONE — #<N> closed (task)                 (task)
resolve: DONE — #<N> relabelled HITL (needs code)  (task needing product code)
resolve: FAILED — #<N> <reason>                    (§7, or not-afk-type from §1)
afk-resolve: would-open PR research/<slug> (<path>)  (§9 dry run only)
afk-resolve: would-skip #<N> …  |  would-fail #<N> <reason>   (§9 dry run only)
```

Exactly one `DONE`/`FAILED` line per invocation (one `would-` line in a dry
run), and it is the last thing printed.

## Boundaries

- **One ticket per invocation.** Never resolve a second ticket, and never a
  ticket this run created (§6b) — the next Fire owns it.
- **Never leave a merged research PR without its ticket.** A `gate-refused`
  failure is a deferral, not an ending: the PR carries the
  `<!-- afk-resolve … -->` marker so whichever Fire merges it can call §5b and
  close the loop.
- **No product code, ever.** The only file this skill writes is
  `${RESEARCH_PREFIX}<map-slug>/<ticket-slug>.md`; the only branch it creates is
  `research/<ticket-slug>`. A ticket that demands code is relabelled `HITL`.
- **The gate merges, not you.** Land the PR only by running the docs-only gate's
  own `.mergeCmd` verbatim, and only after `pr-watch: PASS`.
- **Never edit the Map's Destination or Out of scope.** Appending Decisions so
  far and clearing graduated fog out of Not yet specified are the only Map
  edits.
- **Never stand in for a human.** `wayfinder:grilling` and `wayfinder:prototype`
  tickets are refused; a research question that only a human can answer fails as
  `no-sources` rather than being answered from opinion.
- **Never close a ticket without its answer recorded** — the resolution comment
  (and, for research, the merged file on the default branch) exists before the
  close.
- **Never apply `AFK:paused`** to a Decision ticket: an interrupted resolve is
  restarted, not resumed.
- **A dry run writes one file and nothing else** — no GitHub write, no git
  write; the `would-` line is its whole result.
