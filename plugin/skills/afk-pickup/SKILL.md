---
name: afk-pickup
description:
  The core lane's entry skill, prompted by the Fire wrapper on every Fire.
  Reconcile an open Agent PR needing attention (merge conflict, a human's
  `AFK:revise` hand-back, an unfinished bot tail, a docs-only research PR) if
  one exists, else resume an `AFK:paused` issue (preserving its branch),
  otherwise pick the next eligible `AFK` ticket in the Target Project under the
  Harness config's pick signal (Project priority then oldest, or label-only
  oldest; blockers closed; no human assignee), invoke
  `/auto-agent:afk-dispatch --issue <N> [--resume]`, then open a PR on success
  or apply `AFK:failed` on failure. A picked issue carrying a
  `wayfinder:research` / `wayfinder:task` label is a Decision ticket and routes
  to `/auto-agent:afk-resolve`. One Fire = at most one unit of work. No
  arguments besides the optional `--dry-run`.
disable-model-invocation: true
---

# AFK Pickup — the core lane: one unit of work per Fire

You are the **pickup wrapper** around `/auto-agent:afk-dispatch`. One Fire = at
most one unit of work. Idempotent and silent when nothing is eligible. The Fire
wrapper (`bin/auto-agent fire`) prompted you; it reads the machine lines in §7
and owns everything you cannot: the checkout was already reset to the tip of the
default branch before you started, a crash after you took the lock is cleaned by
the wrapper from your `picked:` / `resolve:` line, and a usage cutoff is paused
by the wrapper, never by you.

## Invocation

```
/auto-agent:afk-pickup [--dry-run]
```

- No positional args.
- `--dry-run` — print the unit of work the Fire would do without mutating GitHub
  or git, then exit. The Fire wrapper's `--dry-run` passes it.

## Harness context

Every repo fact comes from the Harness config the Fire wrapper resolved and
exported (ADR 0002); nothing below names a repo, a branch or a path from memory.

```bash
AA="${AUTO_AGENT_ROOT:?the Fire wrapper exports AUTO_AGENT_ROOT}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")              # the Target Project, from its origin remote
BASE=$(jq -r .repo.default_branch <<<"$CFG")    # detected from GitHub, never declared

MANUAL_ROUNDS_MAX=$(jq -r .rounds.manual_verify <<<"$CFG")   # §6a.2/§6a.3 cap
PR_WATCH_ROUNDS_MAX=$(jq -r .rounds.pr_watch <<<"$CFG")      # named in the pr-watch prompt
COMMIT_SCOPES=$(jq -c .commit_scopes <<<"$CFG")              # allowed PR-title scopes (§6a)
HERMETIC=$(jq -c .verification.hermetic <<<"$CFG")           # null = Bootstrap state (§6a.2)
DEPS_LANE_ON=$(jq -r .lanes.deps_land.enabled <<<"$CFG")     # deps-land lane declared (§1.2)
RESEARCH_PREFIX=$(jq -r .docs_research_prefix <<<"$CFG")     # the docs-only rule's prefix
```

`AUTO_AGENT_ROOT`, `AUTO_AGENT_TARGET_DIR`, `AUTO_AGENT_STATE_DIR` and
`HARNESS_CONFIG_JSON` are exported by the Fire wrapper, so every `"$AA" …` call
below needs no target argument and makes no extra `gh repo view`. The libs
referenced here live under `$AUTO_AGENT_ROOT/lib/`; never hand-roll what they
own.

## Process

### 0. One-call triage (the whole §0–§2 read-only decision tree)

The whole read-only decision tree — gh auth and login, concurrency lock, PR
triage, paused probe, the pick with blocker check — runs as **one script call**.
Do NOT re-derive any of it with individual `gh` probes; every extra Bash call is
a full cache-read API turn, and this consolidation exists to eliminate exactly
those turns.

```bash
TRIAGE=$("$AA" pickup-triage); TRIAGE_RC=$?
VERDICT=$(printf '%s' "$TRIAGE" | jq -r '.verdict')
echo "afk-pickup: triage verdict=$VERDICT"
```

The script is **read-only** — it never touches labels, comments, branches, or
PRs. All mutations stay in the sections below. Branch on `$VERDICT`:

| verdict          | meaning                                        | go to                                                                                            |
| ---------------- | ---------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `no-config`      | the Harness config cannot be resolved          | `echo "afk-pickup: ERROR — no Harness config"`, `exit 1` (the wrapper's preflight should have caught this) |
| `wrong-login`    | gh is logged in as someone other than the machine user | `echo "afk-pickup: ERROR — gh login is not the machine user"`, `exit 1` (ADR 0005: never act as a human account) |
| `no-gh`          | gh unauthenticated                             | §0.5 MCP fallback (run §1.2/§1.5/§2 semantics via MCP tools)                                     |
| `in-flight`      | `AFK:in-progress` lock held                    | `echo "afk-pickup: skip — $(jq -r '.inflight' <<<"$TRIAGE") in flight"; exit 0`                  |
| `reconcile`      | a PR needs attention (or is docs-only / a Bot PR) | §1.2 (fields in `.reconcile`; reasons `docs-merge` / `dependabot` take their own branches)     |
| `resume`         | paused issue below the resume cap              | §1.5 resume path (fields in `.paused`)                                                           |
| `resume-cap`     | paused issue AT the cap                        | §1.5 fail path (fields in `.paused`)                                                             |
| `pick`           | eligible Slice found, blockers closed          | §3/§4 with `N=$(jq -r '.pick.issue' <<<"$TRIAGE")`, title in `.pick.title`                       |
| `pick-wayfinder` | the pick is a wayfinder Decision ticket        | §2b — `/auto-agent:afk-resolve`, never the implementer (type in `.pick.type`)                    |
| `pick-mcp`       | Project pick and the gh token lacks `project` scope | run §2's pick via the GitHub MCP GraphQL tool (same query/filters as the script — see its header) |
| `idle`           | nothing to do                                  | `echo "afk-pickup: no eligible issue"; exit 0`                                                   |

### 0.5. GitHub access strategy: gh first, MCP fallback

All GitHub operations in this skill default to `gh` / `gh api graphql`. If `gh`
is unavailable, unauthenticated, or missing a required scope (most commonly
`project` for a Project-shaped pick), fall back to the equivalent **GitHub MCP**
server tool (the `mcp__github__*`-style functions exposed in the available
toolset). The shapes are equivalent — issue listing, GraphQL, issue editing,
label management, PR creation. Pick whichever works in the current env; do
**not** abort the pickup just because `gh` is missing or under-scoped.

A hand-run pick is the **whole** of §2 including its routing: the exact GraphQL
query, the priority/age sort, the blocker and assignee rules and the wayfinder
routing all live in the header of `$AUTO_AGENT_ROOT/lib/pickup-triage.sh` —
read them from there rather than reconstructing from memory, and apply them
before §3/§4 ever sees a candidate. The fallback changes the transport, never
the decision.

### 1. Concurrency lock check (repo-wide)

`AFK:in-progress` is the single-flight lock. The check already ran inside §0's
triage call (verdict `in-flight`, gh errors fail SAFE toward locked) — there is
no separate probe to run here.

### 1.2. Reconcile a PR needing attention (before resume, before any new pick)

An already-open Agent PR that a human is waiting on outranks everything else:
finishing it is the shortest path to a merge. Three signals make a PR "needing
attention": its mergeable state is `CONFLICTING` (the default branch moved under
it — always auto-fixed, no label needed); it carries the **`AFK:revise`** label
(a human reviewed it and explicitly handed it back — or §6a.1b's
`/auto-agent:pr-review` posted 🤖 findings and applied the label itself); or it
is **bot-incomplete** — no conflict, no label, but its bot tail never finished:
the one-time review marker (`<!-- pr-review-done -->`) and/or any
`Manual verification — … round` comment is missing because a prior Fire died
mid-§6a → reason `incomplete`; or every file it changes is under the config's
research prefix (`$RESEARCH_PREFIX`) — a research PR, which carries no code risk
and never earns review/verify rounds → reason `docs-merge` (checked before the
tail markers, or a docs PR would be reconciled forever). While any ours-shaped
PR is still bot-incomplete this section fires and exits before §1.5/§2 — no new
issue is picked until every outstanding Agent PR is bot-complete (CI green,
one-time review done, a verification round posted). A bot-complete PR merely
awaiting a human merge triggers nothing — work-ahead stays. Detection is the
**PR Triage** deep module (`lib/pr-triage.sh`) — zero Claude usage when nothing
needs attention.

"Ours-shaped" covers both branch shapes the harness creates: `feat/issue-<N>`
(Slice PRs) and `research/<ticket-slug>` (`/auto-agent:afk-resolve` research PRs
— exactly the docs-only PRs `docs-merge` exists for). The ticket number is
derived from the branch, else from the PR title's `(#N)`; `.reconcile.issue` can
therefore be `null`, and when it is, skip the issue lock and the ticket comment
below (there is no ticket) — the PR is still worked.

**Reason `dependabot` — a Bot PR, not an Agent PR.** A PR authored by the
Dependabot app on a `dependabot/…` branch is triaged as a **Dependabot PR**: the
verdict carries its whole classification (`security`, `major`, the head `sha`
and the marker-derived resume state `tierA` / `tierB` / `attempts`) and its
`issue` is always `null`, so there is no issue lock and no ticket comment. It
ranks **below** every agent reason above — a human waiting on their own PR is
never queued behind a bot — with security bumps before version bumps, then
oldest, one per Fire. A conflicting Bot PR comes back as reason `conflict` with
an extra `agentCommits` flag instead — and ranks below every agent reason too.
The triage only ever emits a Bot PR verdict when the Harness config declares the
deps-land lane (`$DEPS_LANE_ON`); otherwise the verdict falls through to
§1.5/§2 inside the script and you never see it.

Both bot verdicts are dispatched to the **`/auto-agent:deps-land`** lane, not to
`/auto-agent:pr-reconcile`: the generic reconcile recipe would lock an issue
that does not exist and force-push a rebase onto a branch Dependabot owns. See
the `deps-land` branch below for the dispatch itself.

`pr_triage_scan` (inside the triage call) owns the `gh pr list` call and rides
out GitHub's async mergeability: a fresh push to the default branch leaves every
open PR `UNKNOWN` for a few seconds, and a plain one-shot listing would miss a
conflicted PR on this very Fire. The scan re-lists while any agent-shaped PR is
`UNKNOWN` (up to ~2 min), then triages.

The scan already ran inside §0's triage call. This section fires only on verdict
`reconcile` (`--dry-run`: print
`afk-pickup: would-reconcile PR #<P> (issue #<N|null>)` and exit 0):

```bash
RECON_PR=$(printf '%s' "$TRIAGE" | jq -r '.reconcile.pr')
RECON_BRANCH=$(printf '%s' "$TRIAGE" | jq -r '.reconcile.branch')
RECON_N=$(printf '%s' "$TRIAGE" | jq -r '.reconcile.issue')
RECON_REASON=$(printf '%s' "$TRIAGE" | jq -r '.reconcile.reason')
HAD_DONE=$(printf '%s' "$TRIAGE" | jq -r '.reconcile.hadDone')
# When the PR both conflicts AND carries AFK:revise, pass --reason both.
# Reason "incomplete" (bot tail never finished) passes through as-is.
# Reason "docs-merge" does NOT go to /auto-agent:pr-reconcile — see below.
# Reason "dependabot" (and any reason on a dependabot/ branch) goes to
# /auto-agent:deps-land — see that branch below.

# Single-flight lock: reuse the issue lock so §1's skip, the Daemon's pacing,
# and the Fire wrapper's crash cleanup all keep working unchanged. HAD_DONE
# (whether AFK:done was present) came from the triage read; restore it on exit.
# Skipped entirely when RECON_N is null — a Bot PR (and some research PRs)
# has no backing ticket, and "gh issue edit null" would error every Fire.
if [ "$RECON_N" != "null" ]; then
    gh issue edit "$RECON_N" --repo "$REPO" --remove-label AFK:done --add-label AFK:in-progress 2>/dev/null || true
fi
```

Emit the pick line (this exact shape — the Fire wrapper's crash cleanup scrapes
it):

```
picked:   reconcile PR #<RECON_PR> (issue #<RECON_N>)
```

**Reason `docs-merge` — merge it, do not reconcile it.** A docs-only PR skips
`/auto-agent:pr-review` and `/auto-agent:verify-pr` entirely: green CI plus the
gate's own re-check of the real diff are the whole bar. Run the **docs-only
gate** (deep module `lib/docs-only-gate.sh` — it owns the docs-only rule and the
merge recipe; never hand-roll either). The gate only ever **decides**: it prints
a verdict plus the exact merge command, and **this section runs that command** —
the one call site that can land a commit on the default branch stays here,
reviewable.

The gate diffs locally, so the head commit must actually be in this clone —
which it usually is not: the Fire's checkout fetches the default branch only,
and the PR branch may have been pruned or pushed from a different checkout.
Fetch the PR head explicitly (`refs/pull/<P>/head` works even when the branch is
gone) and pass the sha you fetched. The gate's base defaults to the detected
default branch and its repo to the configured slug; there are no `--repo` or
`--base` flags:

```bash
HEAD_SHA=$(gh pr view "$RECON_PR" --repo "$REPO" --json headRefOid -q .headRefOid)
git fetch origin "$BASE" --quiet
git fetch origin "refs/pull/$RECON_PR/head" --quiet   # or "$RECON_BRANCH"
GATE=$("$AA" docs-only-gate --head "$HEAD_SHA" --pr "$RECON_PR" --check-state \
    2>&1 >"$AUTO_AGENT_STATE_DIR/docs-gate.json")
GATE_RC=$?
GATE_JSON=$(cat "$AUTO_AGENT_STATE_DIR/docs-gate.json")   # stdout is the verdict; $GATE is stderr
```

The gate exits **0 (approved)** or **1 (refused)** — nothing else; `.reason`
(absent on 0) says why it refused. On `GATE_RC` 0, run the gate's own
`.mergeCmd` verbatim (it carries `--squash --admin --repo` and
`--match-head-commit "$HEAD_SHA"`, so a branch that moved under us fails the
merge instead of landing unreviewed code). Never retype it:

```bash
if [ "$GATE_RC" -eq 0 ]; then
    MERGE_CMD=$(printf '%s' "$GATE_JSON" | jq -r '.mergeCmd')
    if eval "$MERGE_CMD"; then
        [ "$RECON_N" != "null" ] && gh issue comment "$RECON_N" --repo "$REPO" \
            --body "docs-merge: PR #$RECON_PR squash-merged $HEAD_SHA at $(date -u +%FT%TZ)"
        # report, restore the lock, exit 0 — no agent is spawned, so the Fire
        # costs nothing beyond these calls.
    fi
fi
```

**A merged `/auto-agent:afk-resolve` PR still owes its Decision ticket.** A
research PR that this section merges may be one an earlier resolve left open
when the gate refused it: its findings are now on the default branch, but the
ticket is sitting `AFK:failed` with no resolution comment, no close and no Map
entry. The PR body's marker is the back-reference; when it is there, finish the
resolve before restoring the lock:

```bash
MARKER=$(gh pr view "$RECON_PR" --repo "$REPO" --json body -q .body \
    | grep -oE '<!-- afk-resolve ticket:#[0-9]+[^>]*-->' | head -1)
```

If `MARKER` is non-empty, take its `ticket:#<N>` and invoke the skill in-process
via the `Skill` tool —
`/auto-agent:afk-resolve --issue <N> --type research --finish-merged --pr <RECON_PR>`
— and record its terminal `resolve:` line in the §7 report alongside the
`docs-merge:` line. It does no research and opens nothing; it only posts the
resolution comment linking the default branch, closes the ticket, flips it to
`AFK:done` and appends the Map. It is idempotent, so a repeat on an
already-closed ticket costs one read. When `MARKER` is absent (an ordinary docs
PR), skip this entirely. When the marker is present but `/auto-agent:afk-resolve`
is not among this session's skills (Slice #29 not yet installed), report
`resolve: SKIPPED — /auto-agent:afk-resolve not installed` and leave the ticket
for a later Fire.

**Every refusal must show up in the §7 report** — a gate that could not run is a
harness bug and must never vanish into a silent reconcile. Derive the
`docs-merge:` line from `.reason`:

| outcome                                                     | `docs-merge:` line                                                 | then                        |
| ----------------------------------------------------------- | ------------------------------------------------------------------ | --------------------------- |
| `GATE_RC` 0, merge command succeeded                        | `PR #<P> squash-merged <sha> at <ISO ts>`                          | comment, restore lock, exit |
| `GATE_RC` 0, merge command failed                           | `REFUSED — merge-failed: <last line of the command's stderr>`      | fall through                |
| `not-docs-only`                                             | `REFUSED — not-docs-only: <comma-joined .changed from $GATE_JSON>` | fall through                |
| `checks-not-green` / `checks-missing` / `checks-unreadable` | `REFUSED — <.reason>`                                              | fall through                |
| `head-missing` / `git-failed` / `usage`                     | `ERROR — gate could not run: <.reason, else $GATE>`                | fall through                |

The first four rows are genuine refusals about the PR itself (`checks-missing`
means _no_ check ran — an admin merge bypasses branch protection, so an empty
check list vouches for nothing). The last row says nothing about the PR at all:
bad args, git unusable, or `head-missing` (the fetch above did not land). Every
refusal falls through to the `/auto-agent:pr-reconcile` path below with
`RECON_REASON=incomplete` so the PR still gets finished the normal way — a
red-CI docs PR gets its fix loop instead of sitting merged-never — but only the
refusal rows are expected in steady state; a recurring `ERROR —` line is a bug
to file.

**Reason `dependabot` (or `conflict` on a `dependabot/…` branch) — run the
lane.** If `/auto-agent:deps-land` is not among this session's skills (Slice #36
not yet installed), take no lock (there is none for a Bot PR anyway), print
`afk-pickup: skip — Bot PR #<RECON_PR> needs /auto-agent:deps-land (not installed)`
and `exit 0`. Otherwise spawn the **`/auto-agent:deps-land`** skill via the
`Agent` tool (`subagent_type: general-purpose`, `run_in_background: false` —
blocking, same rule as §6a.1; never pass `model:`, the Fire's model policy
carries through) with the prompt:

`"Invoke the /auto-agent:deps-land skill with --pr <RECON_PR> --branch <RECON_BRANCH> --sha <RECON_SHA> --reason <dependabot|conflict> [--security <true|false> --major <true|false>] [--agent-commits <true|false>]."`

**The two bot reasons carry different verdict fields, so build the arguments per
reason** — `pr_triage_pick` attaches `sha`/`security`/`major` to reason
`dependabot` only, and `agentCommits` to reason `conflict` only. Reading the
missing ones anyway would dispatch `--sha null`, and the lane's liveness check
would end every conflict Fire `superseded` before it ever posted
`@dependabot rebase`:

```bash
if [ "$RECON_REASON" = "dependabot" ]; then
    RECON_SHA=$(printf '%s' "$TRIAGE" | jq -r '.reconcile.sha')
    DEPS_ARGS="--sha $RECON_SHA --security $(printf '%s' "$TRIAGE" | jq -r '.reconcile.security')"
    DEPS_ARGS="$DEPS_ARGS --major $(printf '%s' "$TRIAGE" | jq -r '.reconcile.major')"
else
    # reason "conflict" on a dependabot/ branch: the verdict carries no sha and
    # no flags. Read the sha here (one call) so the lane's liveness rule works;
    # security/major are NOT passed — the Fire ends in the lane's §1, before the
    # retitle and the gate, which are the only steps that read them.
    RECON_SHA=$(gh pr view "$RECON_PR" --repo "$REPO" --json headRefOid -q .headRefOid)
    DEPS_ARGS="--sha $RECON_SHA --agent-commits $(printf '%s' "$TRIAGE" | jq -r '.reconcile.agentCommits')"
fi
```

There is **no issue lock** on this path: `.reconcile.issue` is `null` for a Bot
PR, so skip the `gh issue edit` pair entirely (the pick line still prints, with
`issue #null`).

The lane runs the whole recipe — re-read state, retitle, inject the Bot-PR
checklist (the config's `bot-pr-checklist.md` sibling), Tier A
(`/auto-agent:pr-watch --bot`), Tier B (`/auto-agent:verify-pr --force-tour`),
the bounded fix loop, then `lib/deps-gate.sh` — and returns one terminal
`deps-land:` line plus the `deps:` report line. It **never merges**.

**The merge happens here, at the same call site as `docs-merge` above** — the
one place in the harness that can land a commit on the default branch. The lane
ran in its own agent, so its stdout is the only channel: on
`deps-land: APPROVED …` it prints exactly one `merge-cmd: <gate .mergeCmd verbatim>`
line, and that line is where the command comes from. Extract it, never retype
it (it carries `--squash --admin --match-head-commit`, so a branch that moved
between gate and merge fails the merge instead of landing unverified code), then
rewrite the report line's `outcome=` to `merged <sha>`:

```bash
DEPS_OUT=$(...)                # the /auto-agent:deps-land agent's full terminal output
MERGE_CMD=$(printf '%s\n' "$DEPS_OUT" | sed -n 's/^merge-cmd: //p' | tail -1)

# The command is scraped from a subagent's free-form stdout and is about to be
# eval'd with the machine user's admin credentials against the default branch,
# so validate it against the ONE shape the harness merge recipe emits before
# running it. Unlike the docs-merge site above — which evals `.mergeCmd`
# straight out of the gate's own JSON — this text passed through a lane that
# echoes pr-watch lines, manual-verify lines and a `Last failure: <verbatim>`
# string, all of it PR- and page-derived. Without this check a crafted or
# hallucinated `merge-cmd:` line anywhere in that stream is arbitrary shell.
# The PR number is pinned to the one we dispatched and the sha to hex, and the
# anchors leave no room for a shell metacharacter.
MERGE_RE="^gh pr merge ${RECON_PR} --repo [A-Za-z0-9._-]+/[A-Za-z0-9._-]+ --squash --admin --match-head-commit [0-9a-f]{7,40}$"

if ! printf '%s' "$DEPS_OUT" | grep -q '^deps-land: APPROVED'; then
    :   # any other terminal line merges nothing
elif [ -z "$MERGE_CMD" ]; then
    :   # outcome=refused:merge-cmd-missing
elif ! printf '%s\n' "$MERGE_CMD" | grep -Eq "$MERGE_RE"; then
    :   # outcome=refused:merge-cmd-malformed — run NOTHING
else
    eval "$MERGE_CMD"
fi
```

An `APPROVED` line with no `merge-cmd:` line is a lane bug, not a merge: report
`outcome=refused:merge-cmd-missing` and merge nothing. A `merge-cmd:` line that
fails `MERGE_RE` is worse than a bug — it is a malformed gate or text that
reached the lane's stdout from the PR — so report
`outcome=refused:merge-cmd-malformed`, log the offending line verbatim in the §7
report, and run nothing. Never edit the line to make it match: the only
sanctioned merge command is the gate's own `.mergeCmd`, byte-for-byte.

Any other terminal line merges nothing. The lane owns its own labels (`HITL` +
the hand-off comment on a major, draft + `AFK:deps-failed` on exhaustion,
nothing at all on `superseded`); this section only reports.

The gate itself — `lib/deps-gate.sh` — is the sibling of the docs-only gate
above, with the same contract: it decides, never mutates, and prints one JSON
verdict carrying the exact `.mergeCmd`. It needs no local clone (it reads the PR
through `gh` only):
`"$AA" deps-gate --pr <N> --head <sha> [--major true|false] [--security true|false]`,
exit 0 approved / exit 1 refused. It approves only an open, non-draft PR
authored by the Dependabot app on a `dependabot/` branch whose comments carry
`tierA=green` **and** `tierB=PASS` markers for that exact sha, with every check
green and every check named in the config's `required_checks` actually run and
passed (a `skipping` bucket vouches for nothing), a deps title, no human review
requesting changes at any bump size, and an `APPROVED` review if the bump is
major. Its refusals are what the lane's `outcome=` reports:

| `.reason`                     | means                                                                     | lane response                                              |
| ----------------------------- | ------------------------------------------------------------------------- | ---------------------------------------------------------- |
| _(absent, exit 0)_            | approved                                                                  | this section runs `.mergeCmd` verbatim                     |
| `not-dependabot`              | author or head branch is not the Dependabot app's                         | not a Bot PR — leave it alone                              |
| `draft-or-closed`             | the PR is closed, merged or a draft a human parked                        | outcome `superseded` — leave it alone                      |
| `markers-stale`               | tier A/B markers absent, for another sha, or the PR moved                 | re-run the tiers on the new head                           |
| `checks-not-green`            | a check is failing or still pending                                       | fix loop, or wait for CI                                   |
| `checks-missing`              | the check list is EMPTY, or a required check did not run and pass         | wait for CI, then re-gate                                  |
| `title-not-deps`              | not a `fix(deps):`/`chore(deps):` title                                   | retitle, or leave to a human                               |
| `changes-requested`           | a human reviewed the bump and requested changes                           | leave it alone — the rejection stands                      |
| `major-unapproved`            | a major bump with no `APPROVED` review                                    | `HITL` + the hand-off comment; never merged                |
| `checks-unreadable` / `usage` | PR state unreadable, or bad args — **the gate could not run**             | report as a harness error, never as a verdict about the PR |

The last row says nothing about the PR at all, exactly like the docs gate's
`ERROR —` row above: a recurring `checks-unreadable` or `usage` is a bug to
file, not a bump to triage.

Report per the deps block in §7, and exit — a deps Fire never falls through to
§1.5/§2.

Then (every other reconcile reason) spawn the **`/auto-agent:pr-reconcile`**
skill via the `Agent` tool — `subagent_type: general-purpose`,
`run_in_background: false` (blocking, same rule as §6a.1: never proceed or emit
output while it is in flight; never pass `model:`) — with the prompt:

`"Invoke the /auto-agent:pr-reconcile skill with --pr <RECON_PR> --branch <RECON_BRANCH> --issue <RECON_N> --reason <RECON_REASON>."`

Record its terminal `pr-reconcile:` line verbatim as `RECONCILE_LINE`. Then
restore the lock and exit — a reconcile Fire never falls through to §1.5/§2 (one
Fire = one unit of work):

```bash
if [ "$RECON_N" != "null" ]; then
    gh issue edit "$RECON_N" --repo "$REPO" --remove-label AFK:in-progress 2>/dev/null || true
    [ "$HAD_DONE" = "true" ] && gh issue edit "$RECON_N" --repo "$REPO" --add-label AFK:done 2>/dev/null || true
fi
```

Report per the reconcile output block in §7 and exit 0 (exit non-zero only if
the pr-reconcile agent itself crashed with no terminal line).

### 1.5. Resume paused work before any new pick

A `AFK:paused` issue is work that a prior window started and the Fire wrapper
froze mid-run (a `wip:` commit, branch kept) when Claude usage ran out.
In-flight work is always finished before new work is started, so a paused issue
is resumed **before** the §2 pick — and its partial branch is preserved, never
reset.

The probe and the resume-vs-fail decision (including the resume-count cap, the
config's `rounds.pause_resume`) already ran inside §0's triage call, which
delegates the cap logic to the **Pause/Resume state logic** deep module
(`lib/pause-resume.sh`). Do not re-derive either inline.

On verdict **`resume`**:

```bash
# RESUME_MODE tells §4 to preserve the branch and §5 to dispatch in resume
# mode. §2's fresh pick is skipped entirely.
RESUME_MODE=1
N=$(printf '%s' "$TRIAGE" | jq -r '.paused.issue')
```

On verdict **`resume-cap`** (paused too many times — a too-big issue; hand to a
human). `--dry-run`: print `afk-pickup: would-fail #<N> <title>` and `exit 0`
(no label, no comment). Otherwise:

```bash
PAUSED_N=$(printf '%s' "$TRIAGE" | jq -r '.paused.issue')
PAUSE_COUNT=$(printf '%s' "$TRIAGE" | jq -r '.paused.pauseCount')
gh issue edit "$PAUSED_N" --repo "$REPO" --remove-label AFK:paused --add-label AFK:failed
gh issue comment "$PAUSED_N" --repo "$REPO" --body "afk-pickup: paused $PAUSE_COUNT times (resume cap reached) — marking AFK:failed for human triage at $(date -Iseconds)."
echo "afk-pickup: #$PAUSED_N hit the resume cap — AFK:failed, stopping"
exit 0
```

If `RESUME_MODE` is set, skip §2 and §3 and go straight to §4 (§3's dry-run
short-circuit still applies — print `would-resume #N` and exit). Otherwise
(`pick-new`, or no paused issue) continue to §2.

### 2. Pick next eligible issue (the config's pick signal)

The pick signal is the Harness config's `pick` block, and the triage script
already applied it. Under a **Project** pick, the set is: open issues with the
`AFK` label, present in the configured GitHub Project, not carrying
`AFK:in-progress`, `AFK:done`, `AFK:failed` or `AFK:paused`; the configured
priority field ranks them in the configured order (a missing or unknown value
ranks last), then oldest `createdAt`. Issues that carry `AFK` but are **not in
the project** are skipped silently — project membership is the explicit triage
signal. Under a **label-only** pick, the set is the same minus the Project
membership, ordered oldest first.

An issue is eligible only when every GitHub **native** `blockedBy` dependency is
closed (body text like `Blocked by #123` is prose and is ignored) and it carries
no assignee other than the machine user's own login — so unassigned, or assigned
to the machine user alone, is eligible; a ticket a human has claimed is never
picked, even if the machine user is a co-assignee. If the first `blockedBy` page
is full (`pageInfo.hasNextPage`), the candidate fails safe and is treated as
blocked. The GraphQL query, the sort, and those blocker/assignee checks all
already ran inside §0's triage call. On verdict **`pick`**, the winner is in the
verdict:

```bash
N=$(printf '%s' "$TRIAGE" | jq -r '.pick.issue')
TITLE=$(printf '%s' "$TRIAGE" | jq -r '.pick.title')
```

Verdict `idle` means no candidate survived — print
`afk-pickup: no eligible issue` and `exit 0`. Do not notify.

> On verdict `pick-mcp` (or `no-gh`), gh can't run the query — invoke the
> GitHub MCP GraphQL tool with the same query and apply the same
> filters/sort/blocker rules by hand. The exact query string and jq shape live
> in the header of `$AUTO_AGENT_ROOT/lib/pickup-triage.sh` — read them from
> there rather than reconstructing from memory.
>
> **The wayfinder routing is one of those rules, not an extra.** The query
> selects `labels`; the winner's `wayfinder:*` label decides where it goes,
> exactly as the script's own branch does:
>
> - no `wayfinder:*` label → a Slice: continue to §3/§4 as verdict `pick`.
> - exactly `wayfinder:research` / `wayfinder:task` → a Decision ticket: go to
>   §2b with `TYPE` = `research` / `task`, never to the implementer.
> - any other `wayfinder:*` type (`grilling`, `prototype`, …), or **more than
>   one** `wayfinder:*` label → mislabelled HITL/ambiguous: skip that candidate
>   with a note on stderr and consider the next one.
>
> Skipping this by hand is how a Decision ticket ends up with an implementer
> writing code for a question — the failure §2b exists to prevent.

### 2b. Wayfinder Decision ticket → `/auto-agent:afk-resolve`

Verdict `pick-wayfinder` means the winner of §2 is a **Decision ticket** off a
wayfinder **Map**, not a Slice: its resolution is a decision or a fact, so it
takes no branch, no implementer and no code review. It is worked by
`/auto-agent:afk-resolve` instead, which owns the whole protocol (claim →
research → docs PR → docs-only merge → resolution comment → close → Map append →
fog graduation). This section is only the routing.

```bash
N=$(printf '%s' "$TRIAGE" | jq -r '.pick.issue')
TYPE=$(printf '%s' "$TRIAGE" | jq -r '.pick.type')    # research | task
TITLE=$(printf '%s' "$TRIAGE" | jq -r '.pick.title')
SLUG=$(printf '%s' "$TITLE" | tr 'A-Z' 'a-z' | sed -E 's/[^a-z0-9]+/-/g; s/^-+|-+$//g' | cut -c1-60)
```

This `SLUG` is the **only** one: it is passed to `/auto-agent:afk-resolve`
below as `--slug`, and the skill branches, writes and reports on exactly the
string it is handed rather than re-deriving one from the title. Two independent
slug pipelines would drift on some title and leave the Fire wrapper's
out-of-gas cleanup deleting a `research/*` branch that never existed while the
real one stayed pushed.

`--dry-run`: print `afk-pickup: would-resolve #<N> <title>` and `exit 0` — no
label, no branch, no agent.

If `/auto-agent:afk-resolve` is not among this session's skills (Slice #29 not
yet installed), take **no** lock and print
`afk-pickup: skip — Decision ticket #<N> needs /auto-agent:afk-resolve (not installed)`,
then `exit 0`. The ticket stays in the queue for a Harness install that carries
the resolve lane; the Daemon reads `afk-pickup: skip` as no work.

Otherwise emit **both** report lines before doing anything else, then take the
lock. The `resolve:` line is the machine marker the Fire wrapper scrapes to tell
a resolve Fire from a Slice Fire on a crash or an out-of-gas exit (it drops the
lock and deletes `research/<slug>` instead of pausing), so it must be printed
**before** the resolve can be interrupted, and its slug must be the one
`/auto-agent:afk-resolve` branches on:

```
picked:   #<N> <title>
resolve: #<N> <type> <slug>
```

```bash
gh issue edit "$N" --repo "$REPO" --add-label AFK:in-progress
```

`AFK:in-progress` is the same single-flight lock as every other Fire: while
`/auto-agent:afk-resolve` runs, §1 makes every other Fire skip. Then invoke the
skill **in-process** via the `Skill` tool (like `/auto-agent:afk-dispatch`, not
as a spawned `Agent` — the resolve's own subagents are its business):

```
/auto-agent:afk-resolve --issue <N> --type <TYPE> --slug <SLUG>
```

Record its terminal line verbatim as `RESOLVE_RESULT`. `/auto-agent:afk-resolve`
ends on exactly one of four lines, and each leaves the ticket in a settled state
this wrapper must not second-guess:

| terminal line                                       | ticket end state                                                      |
| --------------------------------------------------- | --------------------------------------------------------------------- |
| `resolve: DONE — #<N> closed, PR #<P> merged <sha>` | closed; `AFK:in-progress` → `AFK:done`                                |
| `resolve: DONE — #<N> closed (task)`                | closed; `AFK:in-progress` → `AFK:done`                                |
| `resolve: DONE — #<N> relabelled HITL (needs code)` | open; `AFK` and `AFK:in-progress` removed, `HITL` added, un-projected |
| `resolve: FAILED — #<N> <reason>`                   | open; `AFK:in-progress` → `AFK:failed`, explaining comment posted     |

The skill has already released the lock and applied every label in that column,
including the `AFK:failed` + comment on `FAILED` — this wrapper adds nothing to
any of them. The `relabelled HITL` row is a normal, successful outcome, not a
failure: a Decision ticket that turns out to need product code belongs to a
human to re-route, and dropping `AFK` plus the Project membership is what stops
the picker offering it again.

Then run §6c token accounting for `$N`, emit the resolve output block from §7,
and `exit 0` — a resolve Fire never falls through to §4 (one Fire = one unit of
work). Exit non-zero only if the skill crashed with no terminal line at all.

### 3. Dry-run short-circuit

If `--dry-run` was passed:

```
afk-pickup: would-pick #<N> <title>        # normal pick
afk-pickup: would-resume #<N> <title>      # RESUME_MODE from §1.5
afk-pickup: would-resolve #<N> <title>     # pick-wayfinder from §2b
afk-pickup: would-fail #<N> <title>        # resume-cap from §1.5 (would apply AFK:failed)
```

…and `exit 0`. No git or GitHub mutations. (The reconcile shape,
`afk-pickup: would-reconcile PR #<P> (issue #<N|null>)`, was printed in §1.2.)
Nothing else is printed after a `would-` line: the Fire wrapper reads it as the
Fire's verdict.

**Output discipline (dry-run and real).** The Fire wrapper reads ONLY the text
you write in your own assistant messages (and the final result), never the
stdout of a Bash tool call. A `would-` line, `afk-pickup: no eligible issue`,
`afk-pickup: skip …`, and the `picked:` / `resolve:` lines therefore have to
appear verbatim in YOUR reply, on their own line, not merely inside a
script's output you ran. In a dry-run your final message is exactly the one
`would-` line (optionally preceded by the `picked:` line); do not paraphrase
it into prose, do not add a summary or a "what would happen next" list. A
dry-run whose reply carries no such line is reported by the wrapper as
`work=unknown` and fails.

### 4. Branch + apply lock

**Normal pick** — fresh branch from the tip of the default branch.
`checkout -B` resets if a stale branch from a prior failed Fire exists.

```bash
# N and TITLE already set from §2's verdict — no extra gh read here.
git fetch origin "$BASE"
git checkout -B "feat/issue-$N" "origin/$BASE"
gh issue edit "$N" --repo "$REPO" --add-label AFK:in-progress
```

Emit the pick line now (this exact shape — the Fire wrapper's crash cleanup
scrapes it, and §7 repeats it in the block):

```
picked:   #<N> <title>
```

**Resume** (`RESUME_MODE=1` from §1.5) — the paused issue already has a
`feat/issue-$N` branch carrying its partial work (a `wip:` freeze commit and any
green tests from the prior window). Check it out **as-is** — do **not**
`checkout -B` from `origin/$BASE`, which would wipe exactly the work we are
resuming. Move the lock `AFK:paused → AFK:in-progress`.

```bash
N="$PAUSED_N"
TITLE=$(gh issue view "$N" --repo "$REPO" --json title --jq .title)
git fetch origin                              # refresh refs; do NOT reset the branch
git checkout "feat/issue-$N"                  # existing branch, partial work intact
gh issue edit "$N" --repo "$REPO" --remove-label AFK:paused --add-label AFK:in-progress
```

### 5. Delegate to /auto-agent:afk-dispatch

Invoke the dispatch skill **in-process** via the `Skill` tool, in single-issue
mode. On a resume, add the `--resume` flag so dispatch takes its resume entry
path (read the partial branch, run the tests to discover what remains, continue
TDD) instead of starting the issue from scratch:

```
/auto-agent:afk-dispatch --issue <N>            # normal pick
/auto-agent:afk-dispatch --issue <N> --resume   # RESUME_MODE from §1.5
```

Capture its terminal `dispatch:` line and the final commit on `feat/issue-<N>`.
Dispatch is the single-agent implementer: it drives TDD itself, spawns the
reviewer and verifier subagents, lands the commit with the `smoke:` trailer,
applies `AFK:done` and closes the issue. It never opens the PR — that is §6a.

### 6a. Success path → open PR

Verify the HEAD commit message contains a passing or skipped smoke trailer.
Treat `smoke: FAIL` and a missing trailer as failures (route to §6b).

```bash
TRAILER=$(git log -1 --format=%B | grep -E '^smoke:' | head -1)
case "$TRAILER" in
  "smoke: PASS"*|"smoke: SKIPPED"*) ;;  # ok
  "")            FAIL_REASON="missing smoke trailer on HEAD"; goto §6b ;;
  "smoke: FAIL"*) FAIL_REASON="$TRAILER"; goto §6b ;;
esac

git push -u origin "feat/issue-$N"
```

Build PR body. Extract the issue's Acceptance Criteria block — everything
between a heading matching `^## *Acceptance [Cc]riteria` and the next `^## `
heading (or end of body). If absent, substitute the placeholder
`_(no Acceptance Criteria found in issue body)_`.

Use this template:

```markdown
## Summary

<first paragraph of HEAD commit body>

## Closes

Closes #<N> — <issue title>

## Smoke

`<TRAILER>`

## Manual verification

<AC block — verbatim — or placeholder>

---

Generated by `/auto-agent:afk-pickup` at <ISO-8601 timestamp>
```

Build the PR title as a **conventional-commit subject** — never
`Closes #N: ...`. The harness squash-merges, so this title becomes the commit
subject on the default branch and any release tooling in the Target Project
derives its changelog from it. The issue reference lives in the body's
`## Closes` section, which is what auto-closes the issue on merge; a `#N` in the
title would only pollute the changelog.

```
<type>(<scope>): <description>
```

- `<type>` — inferred from the issue's nature: `feat` for a feature Slice, `fix`
  for a bugfix, `docs` for docs-only, `ci` for workflow-only, `chore`,
  `refactor`, `test`, `perf`, `build`, `style`, `revert`. When in doubt on a
  Slice, use `feat`. Breaking change → append `!` (e.g. `feat(core)!:`).
- `<scope>` — optional, lowercase, no spaces: one of the Harness config's
  `commit_scopes` (`$COMMIT_SCOPES`), the area most affected. Never invent a
  scope outside that list.
- `<description>` — imperative, no trailing period, no issue number. Derive it
  from `$TITLE` by stripping any leading `Closes #N:` and any redundant
  `<area> slice N:` bookkeeping so it reads as a changelog line.

Validate the title inline **before** calling `gh pr create` — there is no
validator script in the harness; the rule is this regex plus the scope list:

```bash
title_ok() {
  printf '%s' "$1" | grep -Eq '^(feat|fix|docs|ci|chore|refactor|test|perf|build|style|revert)(\([a-z0-9-]+\))?!?: [^ ].*$' || return 1
  local scope; scope=$(printf '%s' "$1" | sed -nE 's/^[a-z]+\(([a-z0-9-]+)\)!?:.*/\1/p')
  [ -z "$scope" ] && return 0
  jq -e --arg s "$scope" 'index($s) != null' <<<"$COMMIT_SCOPES" >/dev/null
}

PR_TITLE="feat(core): conventional PR titles for the pickup skill"  # example

ATTEMPT=1
until title_ok "$PR_TITLE"; do
  # Rewrite PR_TITLE to `<type>(<scope>): <desc>`, correcting exactly what
  # failed (unknown/uppercase type, a scope outside commit_scopes, missing
  # space after the colon, empty subject, leading `Closes #N`), then loop.
  ATTEMPT=$((ATTEMPT + 1))
  if [ "$ATTEMPT" -gt 3 ]; then
    goto §6b with FAIL_REASON="PR title failed the conventional-commit rule after 3 attempts: <last title>"
  fi
done
```

A green, pushed Slice must not be handed to a human over a typo you made one
line earlier — but never open the PR with a title that fails, and never relax
the rule.

Then:

```bash
PR_URL=$(gh pr create --repo "$REPO" --base "$BASE" --head "feat/issue-$N" \
  --title "$PR_TITLE" \
  --body "$PR_BODY")
PR_NUM=$(echo "$PR_URL" | grep -oE '[0-9]+$')

# dispatch likely already applied AFK:done; this is idempotent insurance.
gh issue edit "$N" --repo "$REPO" --remove-label AFK:in-progress 2>/dev/null || true
gh issue edit "$N" --repo "$REPO" --add-label AFK:done           2>/dev/null || true
```

### 6a.1. Hand off to `/auto-agent:pr-watch` (BLOCKING)

After the PR is open, immediately spawn the `/auto-agent:pr-watch` skill as an
agent using the `Agent` tool. This wrapper **blocks** until pr-watch returns —
the autonomous Fire is not complete until CI passes (or the fix-loop is
exhausted and the PR is marked draft). One Host = one unit of work in flight at
a time; the `AFK:in-progress` lock and the blocking pr-watch are the two halves
of that constraint.

Invoke pr-watch via the `Agent` tool with:

- `subagent_type: general-purpose`
- `run_in_background: false` ← blocking
- no `model:` — the Fire's model policy carries through
- `prompt`:
  `"Invoke the /auto-agent:pr-watch skill with --pr <PR_NUM> --branch feat/issue-<N> --issue <N>. Return the skill's terminal pr-watch: line verbatim."`

Wait for the agent to return. Record its final message verbatim as
`PR_WATCH_LINE`. It will be one of:

- `pr-watch: PASS — all checks green at attempt <K>`
- `pr-watch: DRAFT — exhausted <PR_WATCH_ROUNDS_MAX> rounds, marked draft, AFK:checks-failed`
- `pr-watch: ERROR — <reason>`

**pr-watch is a blocking checkpoint, not a background task.** While the pr-watch
agent is in flight you MUST NOT proceed to §6a.2, §6a.3, §7, or any other step,
and MUST NOT emit the output block or any summary. If the `Agent` tool reports
the spawn as still running / "in flight", keep waiting (poll for the agent
result) until a terminal `pr-watch:` line exists. A run that emits its output
block while pr-watch is still in flight is an **invalid run** —
`pr-watch: (in flight)` is never a legal value for the §7 block.

§6a.1 may execute **more than once per Fire**: every `fix(manual)` push from
§6a.3 re-enters this step, because new commits re-run CI and invalidate the
previous PASS. Each entry is a fresh pr-watch invocation with its own
`rounds.pr_watch` budget.

### 6a.1b. Autonomous code review (delegates to `/auto-agent:pr-review`, BLOCKING, once per PR ever)

Run only when the **most recent** §6a.1 invocation returned `pr-watch: PASS`.
The review happens exactly once in a PR's life; the gate is the done-marker
comment `/auto-agent:pr-review` posts on completion, read through the Review
Poster deep module:

```bash
. "$AUTO_AGENT_ROOT/lib/review-poster.sh"
if rp_done_marker_present "$PR_NUM"; then
  REVIEW_LINE="pr-review: SKIPPED — already reviewed"
  # → proceed to §6a.2
fi
```

When the marker is absent, spawn `/auto-agent:pr-review` via the `Agent` tool —
`subagent_type: general-purpose`, `run_in_background: false` (blocking, same
rule as §6a.1: never proceed or emit output while it is in flight; no `model:`)
— with the prompt:

> Invoke the /auto-agent:pr-review skill with --pr \<PR_NUM> --branch
> feat/issue-\<N> --issue \<N>. Return the skill's terminal `pr-review:` line
> verbatim.

Record its terminal `pr-review:` line verbatim as `REVIEW_LINE`. Routing:

- `pr-review: DONE — <N> findings posted, AFK:revise applied` → **end the Fire's
  success path here** (emit the §7 block with this `review:` line and no
  `verify:` line). The label hands the PR to the NEXT Fire's §1.2 triage →
  `/auto-agent:pr-reconcile`, whose comment loop fixes the 🤖 threads and then
  re-runs the full pr-watch + manual-verification tail — running §6a.2 now would
  verify code the reconcile is about to rewrite.
- `pr-review: PASS — 0 findings` → proceed to §6a.2.
- `pr-review: SKIPPED — …` → proceed to §6a.2.
- `pr-review: ERROR — …` → record the line and proceed to §6a.2. The review is
  **best-effort**: it never drafts the PR and never blocks the tail (the
  done-marker is absent on ERROR, so a later Fire's reconcile tail gets one more
  chance).

`/auto-agent:pr-review` never fixes, replies, or resolves anything itself — its
🤖 threads are ordinary unresolved review threads that ride the `AFK:revise` →
`/auto-agent:pr-reconcile` machinery, exactly like a human hand-back.

### 6a.2. Manual verification round (delegates to `/auto-agent:verify-pr`, BLOCKING)

Run only when the **most recent** §6a.1 invocation returned `pr-watch: PASS`
(the code is final — a pr-watch fix loop may rewrite it, so verifying earlier
would produce stale evidence) **and §6a.1b has completed or skipped**. Skip when
§6a.1 returned DRAFT or ERROR. This step is **round `M` of the manual
verification loop** (`M` starts at 1, cap `MANUAL_ROUNDS_MAX` from the config's
`rounds.manual_verify` — see §6a.3).

**Bootstrap state first.** When `$HERMETIC` is `null` the Target Project has no
Environment provider yet (Spec: Bootstrap state): there is nothing to boot and
no round to run. Do not park, do not loop. Label the PR for a human verifier and
report it:

```bash
if [ "$HERMETIC" = "null" ]; then
  gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:verify-human
  MANUAL_LINE="verify:   SKIPPED — Bootstrap state, AFK:verify-human applied"
  # → §7; the loop is done (no FAILs, no spec-demands)
fi
```

That line is a legal `verify:` value **only** in Bootstrap state; a Target
Project with a hermetic tier never earns it.

Otherwise delegate the whole round to the **`/auto-agent:verify-pr`** skill.
This wrapper carries no inline verifier prompt: the skill parses the PR's
checklist, boots the per-PR environment through the Target Project's
Environment provider, drives every **unchecked** item on the declared Surfaces,
ticks the boxes that passed (previously ticked boxes stay ticked; deferred and
failed boxes stay `- [ ]`), posts exactly one round evidence comment, tears the
environment down on every exit path, and emits the terminal `manual-verify:`
line this wrapper consumes. The verifier runbook it pastes is the config's
`verifier-runbook.md` sibling.

Spawn `/auto-agent:verify-pr` via the `Agent` tool — `subagent_type:
general-purpose`, `run_in_background: false` (blocking, same rule as §6a.1:
never proceed or emit output while it is in flight; no `model:`) — with a
prompt that names the PR, issue, and the current round so the skill heads its
single evidence comment with the round marker:

> Invoke the /auto-agent:verify-pr skill for PR #\<PR_NUM>, issue #\<N>. This
> is round \<M> of \<MANUAL_ROUNDS_MAX> of the manual verification loop; head
> this round's single evidence comment
> `### Manual verification — round <M>/<MANUAL_ROUNDS_MAX>`. Return the skill's
> terminal `manual-verify:` line verbatim.

On a PR whose diff touches a `browser` or `electron` Surface, the skill also
captures a screenshot tour and posts it into the **PR description** (its
`## Screenshots` section, refreshed each round). That is why the round can be
worth running even for a PR with no unchecked checklist items. Record the
skill's `screenshots:` line, when present, as `SHOTS_LINE` for the §7 block; it
is advisory and never routes the fix loop.

Wait for the agent to return and record its terminal
`manual-verify: <pass>/<total> PASS, <deferred> deferred, <fail> FAIL` line as
`MANUAL_LINE`. This spawn is blocking under the same rule as §6a.1 — do not
proceed or emit output while it is in flight; `manual-verify: (in flight)` is
never a legal value. If `/auto-agent:verify-pr` aborts on a prerequisite or
returns `manual-verify: infra-error …` (the environment never booted — a
non-verdict, zero items acted on), record it verbatim, do **not** enter the fix
loop (parse FAIL as 0), and report it in §7 like a pr-watch ERROR.

**The round is NOT skippable.** "The skill would not run for me" is a real
failure, not a reason to move on. Exactly two outcomes let the Fire continue:

1. a real `manual-verify: <pass>/<total> PASS, <deferred> deferred, <fail> FAIL`
   line, or
2. an explicit recorded non-verdict, `manual-verify: infra-error …` — recorded
   verbatim and reported like a pr-watch ERROR (above), never as PASS.

Anything else — the spawned agent returned no `manual-verify:` line at all, the
spawn failed, the skill refused, `/auto-agent:verify-pr` is not among this
session's skills (a Harness install without Slice #33), or you were tempted to
skip the step because the PR "looks fine" — is a **missing round**. Never infer
a verdict from absence: a missing round can never be reported as
`result: PASS`, and never as a silent skip. Park it instead — on the **PR**,
exactly like the §6a.3 exhaustion escalation below:

```bash
gh pr comment "$PR_NUM" --repo "$REPO" --body "Manual verification round did not run: <reason>.
Parking for a human — no verification evidence exists for this PR."
gh pr ready "$PR_NUM" --repo "$REPO" --undo
gh pr edit  "$PR_NUM" --repo "$REPO" --add-label AFK:checks-failed
```

The draft flip is the load-bearing half. PR Triage (`lib/pr-triage.sh`) keys on
PR state and PR labels only — it never reads the backing issue, and the park
comment deliberately does not match its `Manual verification — .*round` marker —
so **drafting is what takes the PR out of the `incomplete` class**. A label
with no draft leaves the PR open, non-draft and still incomplete, and the very
next Fire re-picks it in an unbounded loop.

Then set `MANUAL_LINE` to `verify: MISSING — <reason>` (for a missing skill:
`verify: MISSING — /auto-agent:verify-pr is not installed in this Harness install`),
emit that verbatim as the §7 `verify:` line, and end the Fire parked rather than
passing. The §7 hard validity rule treats a missing `verify:` line as an invalid
run for exactly this reason — the only legal ways past this step are a verdict,
a recorded `infra-error`, the Bootstrap-state SKIPPED line, or a park.

**Split the deferrals — spec-demanding route like FAILs.** The
`manual-verify:` line lumps every DEFER into one `deferred` count, but the
verifier classifies them two ways and they route differently:

- **hardware DEFER** (named human blocker), and **deployed-env DEFER whose
  demanded post-deploy spec is already present in the PR body** — justified;
  they do not loop, they stay unticked for the human.
- **deployed-env DEFER demanding a tagged post-deploy spec the PR does not yet
  carry** — an _outstanding spec-demand_. It routes to §6a.3 exactly like a
  FAIL, where the implementer's fix is to add the demanded
  `<!-- post-deploy: … -->`-tagged spec item. The next round's re-verify then
  checks that the spec is present (and runs it where the deployed tier can).

The concrete artifact a spec fix produces is a `<!-- post-deploy: … -->` tag in
the PR body, so an outstanding demand is one this round's evidence comment asked
for that the body does not yet carry:

```bash
ROUND_COMMENT=$(gh pr view "$PR_NUM" --repo "$REPO" --json comments \
  --jq '[.comments[] | select(.body | test("Manual verification — .*round"))] | last | .body')
SPEC_DEMANDS=$(printf '%s' "$ROUND_COMMENT"        | grep -ci 'spec-demanded' || true)
SPECS_TAGGED=$(gh pr view "$PR_NUM" --repo "$REPO" --json body --jq .body | grep -c '<!-- post-deploy:' || true)
OUTSTANDING_SPECS=$(( SPEC_DEMANDS > SPECS_TAGGED ? SPEC_DEMANDS - SPECS_TAGGED : 0 ))
```

Parse `<fail>` from `MANUAL_LINE`. If **FAIL = 0 and `OUTSTANDING_SPECS` = 0**,
the loop is done — go to §7 (every item either passed or is a justified
deferral). Otherwise (FAIL > 0 **or** an outstanding spec-demand), go to §6a.3.

### 6a.3. Manual fix round (this session commits, mirrors pr-watch)

When §6a.2 reports one or more ❌ FAIL items — **or an outstanding deployed-env
spec-demand** — do NOT leave them for the human: loop the implementer to fix the
shipped behavior (or add the demanded spec), then re-verify. Because a fix push
re-runs CI and stales the prior `pr-watch: PASS`, each fix re-enters §6a.1
before re-running §6a.2. The whole success path is this bounded outer loop:

```
M=1; MANUAL_ROUNDS_MAX from the config
while true:
  §6a.1 pr-watch (blocking) → PR_WATCH_LINE
  if PR_WATCH_LINE != PASS: break            # DRAFT/ERROR — report, no verify line
  §6a.1b pr-review (blocking, marker-gated once-ever) → REVIEW_LINE
  if REVIEW_LINE says AFK:revise applied: break   # fixes belong to the next Fire's reconcile
  §6a.2 /auto-agent:verify-pr round M (blocking) → MANUAL_LINE, OUTSTANDING_SPECS
  if Bootstrap state: break                  # AFK:verify-human applied, no round owed
  if FAIL == 0 and OUTSTANDING_SPECS == 0: break   # all pass / justified deferrals
  if M == MANUAL_ROUNDS_MAX: exhaust; break
  spawn implementer with the ❌ evidence + spec-demands → stages fix (never commits)
  git commit -m "fix(manual): round $M — <failed items / spec-demands summary>"; git push
  M=M+1                                       # → re-enter §6a.1 (CI re-runs)
```

**Implementer spawn** — via the `Agent` tool: `subagent_type:
auto-agent:implementer` (the plugin's implementer agent), `run_in_background:
false` (blocking), no `model:`. The prompt embeds the issue title + body, the PR
diff (`git diff "origin/$BASE...HEAD"`, capped at 2000 lines as in pr-watch §3),
the round's ❌ FAIL evidence lines and any outstanding deployed-env spec-demands
verbatim (from `ROUND_COMMENT`), plus these instructions verbatim:

> Fix the shipped behavior so each failed manual-verification item passes when
> exercised live. For each outstanding deployed-env spec-demand, add the
> demanded post-deploy verification as a `<!-- post-deploy: … -->`-tagged item
> under the PR's `## Manual verification` (or `## Human verification required`)
> checklist — and, where the check can be scripted, commit the spec it
> references so the re-verify round can run it. Stage the fix (`git add`). Do
> NOT commit and do NOT push — the wrapper handles that. Reply with a short
> summary when staged. If you believe the acceptance criterion itself is wrong
> (not the code), reply `manual-verify-dispute: <one-line reason>` and stage
> nothing.

On `manual-verify-dispute` (or if the implementer staged nothing), treat it as
exhaustion immediately — this loop never edits acceptance criteria. Otherwise
this session commits the staged fix as `fix(manual): round <M> — <summary>` and
pushes with a plain `git push` (never `--force`), then re-enters §6a.1.

**Exhaustion** (cap reached, or a dispute) — mirror pr-watch's escalation; do
NOT route to §6b (a PR exists, and §6b is the no-PR path that must not run after
a push):

```bash
gh pr ready "$PR_NUM" --repo "$REPO" --undo
gh pr edit  "$PR_NUM" --repo "$REPO" --add-label AFK:checks-failed
gh issue comment "$N" --repo "$REPO" --body "manual verification exhausted $MANUAL_ROUNDS_MAX fix rounds on PR #$PR_NUM — <f> item(s) still FAIL. Marked draft + AFK:checks-failed. Human triage required."
```

Report the last `MANUAL_LINE` in the §7 block, suffixed `— EXHAUSTED`.

### 6b. Failure path

On any of: dispatch non-zero exit or a `dispatch: FAIL` line, missing smoke
trailer, `smoke: FAIL`, or unresolved reviewer change-request:

```bash
gh issue edit "$N" --repo "$REPO" --remove-label AFK:in-progress
gh issue edit "$N" --repo "$REPO" --add-label AFK:failed
gh issue comment "$N" --repo "$REPO" --body "afk-pickup FAILED at $(date -Iseconds): ${FAIL_REASON:-afk-dispatch returned non-zero}"
```

Do NOT open a PR. Do NOT push the branch. Exit non-zero so the Fire record
captures the failure.

### 6c. Token accounting (every Fire that worked an issue — one call)

Before emitting the output block — on the success path, the failure path, a
reconcile Fire, AND a resolve Fire (§2b) — post the issue's cumulative token
spend as a single create-or-update comment (marker `<!-- token-usage -->`,
PATCHed in place on re-runs, so resumes and reconciles keep one comment current
instead of stacking new ones):

```bash
"$AA" token-usage post --issue "$N" || true
```

It sums every `feat/issue-$N` transcript line (main session + subagents) on this
Host. Advisory: `|| true` — accounting never fails a Fire. Skip only when the
Fire picked nothing (`skip` / `no eligible issue`).

## Output format

One block per Fire, written to stdout:

```
=== /auto-agent:afk-pickup <ISO-8601> ===
picked:   #<N> <title>            (or: skip — N in flight, or: no eligible)
dispatch: PASS | FAIL — <reason>
pr:       <url>                    (success only)
pr-watch: PASS | DRAFT | ERROR — <detail>   (success only)
review:   <verbatim pr-review terminal line>   (pr-watch PASS only)
verify:   <pass>/<total> PASS, <n> deferred, <n> FAIL — round <M>/<MANUAL_ROUNDS_MAX> [— EXHAUSTED]   (pr-watch PASS only)
          | SKIPPED — Bootstrap state, AFK:verify-human applied   (no hermetic tier in the config)
          | MISSING — <reason>            (§6a.2 park: round never ran)
shots:    <verbatim screenshots: line from the last /auto-agent:verify-pr round>   (when present)
tokens:   <verbatim token-usage: line from §6c>   (when an issue was worked)
```

A **reconcile Fire** (§1.2 picked a PR instead of an issue) emits this block
instead:

```
=== /auto-agent:afk-pickup <ISO-8601> ===
picked:   reconcile PR #<P> (issue #<N>)
reconcile: <verbatim terminal pr-reconcile: line from the §1.2 agent>
```

A **docs-merge Fire** (§1.2 ran the docs-only gate on a docs-only PR; no agent
is spawned when it merges) emits this block instead:

```
=== /auto-agent:afk-pickup <ISO-8601> ===
picked:   reconcile PR #<P> (issue #<N>)
docs-merge: PR #<P> squash-merged <sha> at <ISO ts>
```

…or, when the gate refused or could not run, the matching `REFUSED — <reason>` /
`ERROR — gate could not run: <reason>` line from §1.2's table, followed by the
usual `reconcile:` line from the fall-through:

```
docs-merge: REFUSED — checks-not-green
reconcile: <verbatim terminal pr-reconcile: line>
```

A **deps Fire** (§1.2 picked a Dependabot PR and ran `/auto-agent:deps-land`)
emits this block instead — the `deps:` line comes verbatim from the lane, except
that on the merge path this section rewrites its `outcome=` to the sha it just
merged:

```
=== /auto-agent:afk-pickup <ISO-8601> ===
picked:   reconcile PR #<P> (issue #null)
deps: PR #<N> "<title>" — security=<y/n> major=<y/n> tierA=<green|fixed(k)|failed> tierB=<k/n|skipped> outcome=<merged sha|HITL|deps-failed|superseded>
result:   <verbatim terminal deps-land: line>
```

A gate refusal that is none of the four terminal outcomes is reported as
`outcome=refused:<reason>` from the table in §1.2 — never dropped: a gate that
could not run is a harness bug and must not vanish into a silent skip.

A **resolve Fire** (§2b picked a wayfinder Decision ticket) emits this block
instead — the `resolve:` marker line, then `/auto-agent:afk-resolve`'s terminal
line and, for a research ticket that merged, the `docs-merge:` line the skill
emitted. Echo that line **verbatim** from `/auto-agent:afk-resolve` — it is the
resolve-side marker shape (`docs-merge: PR #<p> <sha>`), which is deliberately
terser than §1.2's reconcile `docs-merge:` line; never reformat one into the
other:

```
=== /auto-agent:afk-pickup <ISO-8601> ===
picked:   #<N> <title>
resolve: #<N> <research|task> <slug>
docs-merge: PR #<P> <sha>                            (research only, on merge)
result:   <verbatim terminal resolve: DONE|FAILED line from /auto-agent:afk-resolve>
tokens:   <verbatim token-usage: line from §6c>
```

`pr-watch` line mirrors verbatim the final message returned by the spawned
pr-watch agent in §6a.1; `verify:` mirrors the §6a.2 `/auto-agent:verify-pr`
round's final `manual-verify:` line (from the last round), suffixed
`— round <M>/<MANUAL_ROUNDS_MAX>` and, on §6a.3 exhaustion, `— EXHAUSTED`.
`shots:` mirrors that same round's `screenshots:` line when the skill emitted
one — it reports whether the PR description got its UI screenshot tour, and it
is **advisory**: it never gates the Fire. If §6a failed (no PR opened), omit the
`pr:`, `pr-watch:`, and `verify:` lines; if pr-watch returned DRAFT/ERROR, omit
`verify:`.

**Machine lines (for the Fire wrapper).** When the Fire does not pick — a
concurrency skip (§1), a lane this Harness install does not carry (§1.2, §2b),
or an empty queue (§2) — emit, in addition to the human `picked:` line, one of
these exact standalone lines so the wrapper can tell the Daemon to sleep out the
window instead of hot-looping into the lock:

- `afk-pickup: skip — <n> in flight` — an `AFK:in-progress` lock is already held
  (§1).
- `afk-pickup: skip — <reason>` — any other reason nothing was worked (a lane
  skill not installed); always starts `afk-pickup: skip`.
- `afk-pickup: no eligible issue` — nothing eligible in the queue (§2).

The `picked:   #<N> <title>`, `picked:   reconcile PR #<P> (issue #<N|null>)`
and `resolve: #<N> <type> <slug>` lines, and the four `afk-pickup: would-…`
dry-run lines, are the other stable lines; `lib/fire-record.sh` scrapes all of
them into the Fire record and `lib/fire.sh` cleans a leaked lock from them.

**Hard validity rule.** On the §6a success path the output block MUST contain a
`pr-watch:` line copied verbatim from the last pr-watch agent's terminal
message, and — whenever that line says PASS — a `review:` line copied from
§6a.1b's terminal `pr-review:` message (SKIPPED counts) and a `verify:` line
copied from the last `/auto-agent:verify-pr` round's `manual-verify:` message
(or the Bootstrap-state SKIPPED line). Exception: when the `review:` line says
`AFK:revise applied`, the Fire legally ends without a `verify:` line (the
reconcile Fire re-verifies after the fixes). If any required line is missing,
the run is **invalid**: do not emit the report; go back and wait for the
in-flight agent. `pr-watch: (in flight)` and `pr-review: (in flight)` are never
legal values.

Whenever §6a.2 was reached, the `verify:` line therefore has exactly four legal
shapes: a real `manual-verify:` verdict, a recorded
`manual-verify: infra-error …` non-verdict, the Bootstrap-state
`SKIPPED — Bootstrap state, AFK:verify-human applied` line, or
`verify: MISSING — <reason>` from the §6a.2 park — and only the first and the
third may accompany a passing Fire. The `AFK:revise applied` exception above is
the **only** way past §6a.1b without one: that Fire ends before a round is ever
owed, so it legally carries no `verify:` line, takes no park, and applies no
`AFK:checks-failed` (the next Fire's reconcile owns the round — double-labelling
it here would muddy that Fire's triage reason). Otherwise a reviewed PR must
never reach a passing report on an absent round: when the round was owed and did
not run, the Fire ends parked (draft + `AFK:checks-failed` + the explanatory
comment on the PR), not green.

## Failure modes

- **Stale `AFK:in-progress` from a crashed prior Fire** — the Fire wrapper
  clears the lock its own Fire took (from the `picked:` / `resolve:` line it
  scraped); a lock nothing scraped blocks all future Fires until manually
  cleared. Fix: `gh issue edit <N> --remove-label AFK:in-progress`.
- **Branch `feat/issue-<N>` already exists from a prior failed Fire** — §4's
  `checkout -B` resets it from `origin/$BASE`, discarding stale work.
  Recoverable via `git reflog`. This reset applies to the **normal-pick** path
  only; the **resume** path (§1.5 → §4) deliberately checks the branch out as-is
  so the paused window's partial work survives.
- **Paused issue loops across too many windows** — §1.5 caps resumes via the
  Pause/Resume state logic (the config's `rounds.pause_resume`); on the cap it
  applies `AFK:failed` and stops, so a too-big issue is handed to a human
  instead of bouncing forever.
- **Acceptance Criteria section missing from issue body** — PR body uses a
  placeholder line. PR still opens; the reviewer will notice and request the
  amendment.
- **`smoke:` trailer present but says FAIL** — §6a treats as failure, routes to
  §6b. SKIPPED is acceptable (a Target Project without a smoke hook, or in
  Bootstrap state, has none).
- **§6a.2 `/auto-agent:verify-pr` round reports ❌ (or an outstanding
  spec-demand)** — §6a.3 spawns the implementer with the failure evidence (and
  any demanded post-deploy specs), this session commits `fix(manual): round <M> …`
  and pushes, and the loop re-enters §6a.1 (CI must re-green before
  re-verification). Cap `rounds.manual_verify`; on exhaustion (or an implementer
  `manual-verify-dispute`) the PR converts to draft + `AFK:checks-failed` with an
  issue comment — the same escalation as pr-watch exhaustion. A **justified
  DEFER** item (real hardware / human observation, or a deployed-env item whose
  demanded `<!-- post-deploy: … -->` spec is already present) is not a failure
  and does not loop; it stays unticked for the human.
- **Output emitted while pr-watch or the `/auto-agent:verify-pr` round is in
  flight** — an invalid run per the §7 hard validity rule. The missing terminal
  `pr-watch:` / `verify:` line is the detection signal; the fix is to wait for
  the blocking agent before emitting anything.
- **Crash or out-of-gas mid-resolve (§2b)** — the `resolve: #<N> <type> <slug>`
  line is what the Fire wrapper scrapes to tell a resolve Fire apart: a crash
  flips the ticket `AFK:in-progress → AFK:failed` with a comment, and an
  out-of-gas exit drops the lock, deletes the pushed `research/<slug>` branch
  and applies **no** label, so the next Fire restarts the resolve from scratch.
  A resolve is never paused — there is no partial implementation worth
  resuming. Once the terminal `resolve: DONE …` line has gone out, neither
  cleanup applies: the ticket is closed and answered, so a crash in the
  best-effort tail only restores `AFK:done`, and a cutoff there only drops the
  lock — the branch is the merged PR's head and is left alone.
- **A `wayfinder:grilling` / `wayfinder:prototype` ticket carries `AFK`** — a
  mislabelled HITL ticket. The triage skips the candidate with a stderr note and
  considers the next one; fix the labels (drop `AFK`, add `HITL`, remove it
  from the Project) rather than letting an agent stand in for the human.
- **Network/auth flake mid-dispatch** — §6b cleans GitHub state; subagents the
  dispatch spawned end with the session.
- **Crash mid-reconcile (§1.2)** — the issue is left `AFK:in-progress` with no
  live Fire. The Fire wrapper scrapes the
  `picked:   reconcile PR #<P> (issue #<N>)` line and restores the lock
  (`AFK:in-progress` removed, `AFK:done` re-added) instead of applying
  `AFK:failed` — the issue's work was already done; only the PR needs care.
- **Reconcile pick loops on the same PR** — cannot happen while parked:
  `/auto-agent:pr-reconcile` always exits having either removed the attention
  signal (rebased ⇒ no longer CONFLICTING; threads done ⇒ `AFK:revise` dropped)
  or applied a parked label (`AFK:rebase-failed` / `AFK:revise-failed`) that the
  PR Triage skips. An `incomplete` pick self-clears the same way: every
  reconcile tail run either posts the review marker / a verification round,
  applies `AFK:revise` (re-picked as `revise` next Fire), or parks the PR — the
  PR exits the incomplete class every Fire.
- **A lane skill is not installed** (`/auto-agent:afk-resolve`,
  `/auto-agent:deps-land`) — the Fire takes no lock and ends on an
  `afk-pickup: skip — …` line; the ticket or Bot PR waits for a Harness install
  that carries the lane. A recurring skip of this kind on a Host that should
  have the lane is a Setup problem, not a queue problem.

## Boundaries

- Never picks more than one unit of work per Fire — a reconcile (§1.2), a
  resume (§1.5), a fresh Slice (§2), or a wayfinder Decision ticket resolved via
  `/auto-agent:afk-resolve` (§2b), in that priority order, never two.
- Never modifies the parent Spec issue. Only operates on `AFK`-labeled child
  issues. (Specs themselves must NOT carry the `AFK` label.)
- Never opens a PR if smoke did not pass or skip.
- **Never merges an Agent PR.** The only two merge sites in this skill are the
  docs-only gate's `.mergeCmd` (a research PR) and the deps gate's `.mergeCmd`
  (a Dependabot PR), each run byte-for-byte from a tested gate; a
  `feat/issue-<N>` PR reaches the default branch only through a human's
  approval and merge.
- Never exits before the §6a.1 pr-watch agent (and, on pr-watch PASS, the
  §6a.1b `/auto-agent:pr-review` review and — unless the review applied
  `AFK:revise` or the Target Project is in Bootstrap state — the §6a.2
  `/auto-agent:verify-pr` round, re-entered once per manual fix round) returns.
  One Fire = one issue picked, implemented, PR opened, CI watched to verdict,
  code-reviewed once, and either manual verification executed and recorded on
  the PR or the review's `AFK:revise` hand-off queued for the next Fire's
  reconcile.
- The §6a.2 **`/auto-agent:verify-pr` round** mutates nothing beyond the PR
  body (boxes it proved) and one evidence comment per round, and tears its
  environment down on every exit path; it does spend Claude usage. The §6a.3
  **fix loop** also burns usage (one implementer spawn per manual round, capped
  by `rounds.manual_verify`) and pushes `fix(manual):` commits to the PR branch
  only — never to the default branch, never force-pushed. The §6a.1b
  **`/auto-agent:pr-review` review** burns usage once per PR ever and never
  pushes — its only writes are inline review comments, at most one `AFK:revise`
  label add, and one done-marker comment; the fixes it queues are pushed later
  by `/auto-agent:pr-reconcile` under that skill's own rules.
- Never passes `model:` to a spawned agent: the Fire's `--model` (the Host's
  model policy) is the only model choice.
