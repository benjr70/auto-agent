---
name: afk-dispatch
description:
  Implement one `AFK` issue on its `feat/issue-<N>` branch as a single-agent
  implementer: read the ticket, drive a TDD loop, get the staged diff approved
  by the reviewer subagent, have the verifier subagent run the Environment
  provider's smoke and land the commit with the `smoke:` trailer, then flip the
  issue to `AFK:done`. Invoked in-process by `/auto-agent:afk-pickup` with
  `--issue <N> [--resume] [--dry-run]`; never opens a PR.
---

# AFK Dispatch — Single-Agent Implementer with Reviewer and Verifier Subagents

You are the **implementer**. One issue, one vertical slice, TDD discipline. You
own the working tree and the staged diff; two plain subagents own what you must
not judge yourself: the **reviewer** (`auto-agent:reviewer`) approves or
requests changes on your staged diff, and the **verifier**
(`auto-agent:verifier`) runs the Environment provider's smoke and lands the
commit with the `smoke:` trailer. One Fire costs one context: there is no
second session, no shared queue, no relay. Everything below runs in this
session except the two blocking `Agent` spawns.

## Invocation

```
/auto-agent:afk-dispatch --issue <N> [--resume] [--dry-run]
```

- `--issue <N>` — the `AFK` issue to implement. The caller
  (`/auto-agent:afk-pickup` §4/§5) has already checked out `feat/issue-<N>`
  and applied the `AFK:in-progress` lock.
- `--resume` — the issue was paused mid-Fire in a prior window; its
  `feat/issue-<N>` branch already carries partial work. Take the resume entry
  path (§1.1) instead of starting from scratch.
- `--dry-run` — print the plan (§2) and stop. No edits, no spawns, no labels.

## Harness context

```bash
AA="${AUTO_AGENT_ROOT:?the Fire wrapper exports AUTO_AGENT_ROOT}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")              # the Target Project, from its origin remote
BASE=$(jq -r .repo.default_branch <<<"$CFG")    # detected from GitHub, never declared

INSTALL_CMD=$(jq -r .commands.install <<<"$CFG")
TEST_CMD=$(jq -r .commands.test <<<"$CFG")
LINT_CMD=$(jq -r '.commands.lint // empty' <<<"$CFG")
SCOPES=$(jq -r '.commit_scopes | join(", ")' <<<"$CFG")
PLAN_GATED=$(jq -r '.commands.plan_gated_paths[]' <<<"$CFG")   # globs, may be empty
HERMETIC=$(jq -c '.verification.hermetic' <<<"$CFG")          # null in Bootstrap state
RUNBOOK=$(jq -r '.prose.verifier_runbook // empty' <<<"$CFG")  # markdown path or empty
REVIEW_STATE="$(git rev-parse --git-dir)/auto-agent/review-state.json"
```

Every repo fact comes from `$CFG`. The cwd is the Target Project root; every
command below runs from there.

## Process

### 0. Pre-flight (idempotent, runs every dispatch)

```bash
gh auth status >/dev/null || { echo "afk-dispatch: gh not authenticated"; exit 1; }
command -v jq >/dev/null  || { echo "afk-dispatch: jq missing"; exit 1; }
```

**Open the review-state file** (not in `--dry-run`). Its presence for this
branch is what tells the plugin's `smoke-trailer.sh` hook that a dispatch is
in flight and HEAD must carry a trailer; without it the hook ignores HEAD, so a
Fire that never dispatched is never blocked on an unrelated commit:

```bash
mkdir -p "$(dirname "$REVIEW_STATE")"
jq -n --arg b "feat/issue-$N" --argjson n "$N" \
      '{branch: $b, issue: $n, round: 0, verdict: "pending", asks: []}' > "$REVIEW_STATE"
```

**GitHub labels (create-if-missing):**

```bash
"$AA" labels-ensure    # every harness label, created only when absent
```

`labels-ensure` (`lib/labels-ensure.sh`) is idempotent and non-destructive: it
creates only what is missing and never runs `gh label create --force`, which
would rewrite the curated colour and description of an existing label on every
run. The label table lives in the lib, not here.

Never sweep stale `AFK:in-progress` here: `/auto-agent:afk-pickup` applied the
lock before invoking this skill and its wrapper clears it on a crash. Sweeping
would clobber the lock and break single-flight.

### 1. Read the issue

```bash
gh issue view "$N" --repo "$REPO" --json number,title,body,labels
```

Read the whole body: acceptance criteria, interface changes, behaviors to test,
blocked-by. If a `Blocked by` section names an open issue
(`gh issue view <blocker> --repo "$REPO" --json state`), stop: this issue is
not yours to implement. Report `afk-dispatch: BLOCKED #<blocker>` and take the
failure path (§6) with that reason.

Before writing anything, read the Target Project's `CONTEXT.md` and any
`docs/adr/` entries covering the area you are touching, when they exist. Name
modules, interfaces and tests in the glossary's vocabulary, and do not
contradict an ADR without saying so in the commit body.

Run `$INSTALL_CMD` once if the working tree has never been installed (a fresh
checkout, a missing dependency dir); skip it when a prior Fire already did.

### 1.1. Resume entry path (`--resume` only)

`/auto-agent:afk-pickup` §4 already checked out the existing `feat/issue-<N>`
branch **without** resetting it, so its partial work is present: green tests
from the prior window and, at HEAD, a
`wip: freeze partial work on #<N> (usage exhausted)` commit the wrapper made
when usage ran out. The goal is to continue TDD from that state to green, not
to re-implement what already passes.

```bash
git rev-parse --abbrev-ref HEAD                 # expect feat/issue-<N>
git log --oneline "origin/$BASE..HEAD"          # the partial work; may end in a wip: commit
if git log -1 --format=%s | grep -q '^wip: freeze partial work'; then
  git reset --soft HEAD~1                       # un-freeze; edits back in the index/worktree
fi
```

Then prime yourself for resume rather than a cold start:

> **RESUME — this issue was paused mid-Fire; the branch already has partial
> work.** Do NOT restart from scratch. First run `$TEST_CMD` to discover what
> already passes and what remains. Any un-frozen `wip:` edits are already in
> your working tree — reconcile them into proper TDD red-green commits. Then
> continue the red-green-refactor loop only for the acceptance criteria not
> yet satisfied, and finish as usual (stage, commit message, review round).

Continue to §2.

### 2. Plan (and the dry-run short-circuit)

Write a short plan, in this order: which acceptance criteria, which files,
which tests (name the behavior and the public seam it is tested through, not
"add tests"). Decide whether any file you intend to touch matches a
`plan_gated_paths` glob:

```bash
GATED_HIT=""
for g in $PLAN_GATED; do
  # A `[[ == ]]` pattern match: `*` and `**` both cross `/` here, so a glob
  # like `src/**/*.service.ts` matches any depth. Over-gating is the safe side.
  for p in $PLANNED_PATHS; do [[ "$p" == $g ]] && GATED_HIT="$GATED_HIT $p"; done
done
```

If `--dry-run` was passed, print and stop:

```
=== /auto-agent:afk-dispatch --issue <N>: dry run ===
issue:    #<N> <title>
branch:   feat/issue-<N>
gated:    <paths matching plan_gated_paths, or none>
roles:    implementer (this session) · reviewer (auto-agent:reviewer) · verifier (auto-agent:verifier)
```

Do NOT edit, spawn or label.

**Plan round (only when `GATED_HIT` is non-empty).** A path under
`plan_gated_paths` is one the Target Project wants planned before it is
edited. Spawn `auto-agent:reviewer` via the `Agent` tool (blocking,
`run_in_background: false`; never pass `model:`) with the issue title, body and
acceptance criteria, the plan, and the gated paths, asking for a **plan
round**. It replies exactly `plan-approved` or `plan-rejected: <reason>`. On
rejection, revise the plan once against the reason and re-spawn; a second
rejection is an unfixable blocker: take the failure path (§6) with the reason.
When `plan_gated_paths` is empty or nothing planned matches, there is no plan
round.

### 3. TDD loop (you are the implementer)

Drive red-green-refactor **one test at a time**: write the failing test, run
`$TEST_CMD`, implement, run again, next. Do not write all tests first.
Vertical slices only. Tests verify behavior through public interfaces; mock
only at system boundaries (external APIs, hardware, databases), never an
internal collaborator. Read `$RUNBOOK` when set: it is the maintainer's prose
on what "works" means for this project.

When every acceptance criterion is green:

1. Run `$LINT_CMD` when it is set, and fix what it flags.
2. Stage **only** the files you changed. Do NOT `git add .` or `-A`.
3. Write the commit message (do not commit):

   ```
   <type>(<scope>): <short description>

   Closes #<N>
   ```

   `<type>` is `feat` for a feature slice, `fix` for a bugfix, else the
   conventional type that fits. `<scope>` is one of the configured commit
   scopes (`$SCOPES`): the area most affected. The verifier appends the
   `smoke:` trailer as the last line.

### 4. Review round (blocking subagent, cap 10)

Spawn `auto-agent:reviewer` via the `Agent` tool (`run_in_background: false`,
no `model:`) with: the issue title, body and acceptance criteria; the staged
diff (`git diff --staged`, capped at 2000 lines, `... [truncated]` marker if
longer); the commit message; `$TEST_CMD` and `$LINT_CMD`; the gated paths and
whether a plan round approved them. It replies **exactly** one of:

- `approved`
- `change-request: <specific asks, one per line>`

Record the verdict into the review-state file **before** acting on it:

```bash
mkdir -p "$(dirname "$REVIEW_STATE")"
jq -n --arg b "feat/issue-$N" --argjson n "$N" --argjson r "$ROUND" \
      --arg v "$VERDICT" --argjson a "$ASKS_JSON" \
      '{branch: $b, issue: $n, round: $r, verdict: $v, asks: $a}' > "$REVIEW_STATE"
```

(`VERDICT` is `approved` or `change-request`; `ASKS_JSON` is the asks as a JSON
array of strings, `[]` on approval.) The plugin's `Stop` hook
(`review-gate.sh`) reads this file and blocks the session from ending while it
says `change-request` for the current branch, so an unaddressed ask can never
be abandoned by a session that simply stops.

On `change-request`: address **every** ask by changing the shipped code (or
its tests), re-run `$TEST_CMD` and `$LINT_CMD`, re-stage, and re-spawn the
reviewer with the new diff. `ROUND` counts every review round and every
smoke-fail round together; the cap is **10** (a fixed harness constant). If
the 10th round still ends in `change-request` or `smoke: FAIL`, take the
failure path (§6) with the last asks as the reason.

On `approved`: continue to §5.

### 5. Verify round (blocking subagent) and the commit

Spawn `auto-agent:verifier` via the `Agent` tool (`run_in_background: false`,
no `model:`) with: the commit message from §3; `$HERMETIC` (the hermetic block
as JSON, or `null`); `$RUNBOOK`; `$N`; `$TEST_CMD`. The verifier owns the
Environment provider protocol (ADR 0003):

- provider present (`$HERMETIC` not null, `command` set): `down --pr <N>`,
  then `up --pr <N>` capturing its `KEY=value` block into the environment,
  then `smoke` only when `hermetic.smoke` is `true`, then always
  `down --pr <N>`;
- exit codes: `up` 0 healthy / 3 prerequisite missing / 4 boot failed;
  `smoke` 0 pass / 1 fail / 2 could not run.

It decides the trailer and never guesses PASS:

| outcome                                                                          | trailer                          |
| -------------------------------------------------------------------------------- | -------------------------------- |
| `smoke` exit 0                                                                   | `smoke: PASS — <detail>`         |
| `smoke` exit 1                                                                   | `smoke: FAIL — <detail>`         |
| smoke off in config, no provider (Bootstrap state), `up` 3/4, or `smoke` 2       | `smoke: SKIPPED — <reason>`      |

On PASS or SKIPPED it **commits** the staged work with your message plus the
trailer as the last line, and replies with the trailer. The plugin's
`Stop`/`SubagentStop` hook (`smoke-trailer.sh`) blocks the verifier from
finishing if HEAD is a dispatch commit (conventional subject plus `Closes #N`)
without a `smoke:` trailer.

On `smoke: FAIL` it does **not** commit and replies with the failure detail.
Fix the shipped behavior, re-run `$TEST_CMD`, re-stage, and go back to §4 (a
changed diff needs a fresh review). This counts as a round against the shared
cap of 10.

### 6. Completion, or the failure path

**Success** (HEAD carries the commit with a `smoke: PASS` or `smoke: SKIPPED`
trailer):

```bash
gh issue edit "$N" --repo "$REPO" --remove-label AFK:in-progress --add-label AFK:done
gh issue close "$N" --repo "$REPO"
rm -f "$REVIEW_STATE"
```

Report and exit 0. `/auto-agent:afk-pickup` §6a pushes the branch and opens
the PR; this skill never pushes and never opens a PR.

**Failure** (cap exhausted, a second `plan-rejected`, an open blocker, or an
error you cannot fix):

```bash
gh issue edit "$N" --repo "$REPO" --remove-label AFK:in-progress --add-label AFK:failed
gh issue comment "$N" --repo "$REPO" --body "afk-dispatch FAILED at $(date -Iseconds): <reason>"
rm -f "$REVIEW_STATE"
```

Do not commit half-work, do not push. Exit non-zero so the caller takes its
§6b path.

## Output format

A structured log in this session's output, one block per issue:

```
[#<N>] <issue title>
  plan:        approved | not gated
  implementer: tests green, staged
  reviewer:    approved after <r> round(s)
  verifier:    smoke: PASS | SKIPPED — <detail>
  closed #<N>
```

## Failure modes

- **Reviewer keeps requesting changes** — cap 10 rounds shared with smoke
  fails; on exhaustion `AFK:failed` + comment naming the last asks.
- **`smoke: FAIL`** — never committed; fix, re-review, re-verify; counts toward
  the cap.
- **Provider missing (Bootstrap state)** — the verifier commits with
  `smoke: SKIPPED — no Environment provider`; the PR the caller opens is
  labelled `AFK:verify-human`.
- **`smoke-trailer.sh` blocks the verifier** — HEAD is a dispatch commit with
  no trailer; the verifier amends the message with the correct trailer.
- **`review-gate.sh` blocks a stop** — the review-state file still says
  `change-request`; address the asks, re-review, and only then end.
- **Blocked by an open issue** — report `BLOCKED #<blocker>`, failure path.

## Boundaries

- Never opens a PR, never pushes. The caller does both.
- Never approves your own diff (the reviewer does), never writes the `smoke:`
  trailer yourself (the verifier does).
- Never runs the Environment provider yourself; the verifier owns `up`,
  `smoke`, `down`.
- Never touches labels other than the completion/failure flips above; the
  single-flight lock is the caller's.
- Never spawns more than the two subagents named here.
