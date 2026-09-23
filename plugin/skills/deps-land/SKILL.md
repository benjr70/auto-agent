---
name: deps-land
description:
  Take one open Dependabot PR to a terminal state — the deps-land lane.
  Retitle it, inject the Target Project's Bot-PR checklist, prove it with Tier
  A (`/auto-agent:pr-watch --bot`) and Tier B (`/auto-agent:verify-pr
  --force-tour`), fix it in a loop bounded by the config's `rounds.deps_fix`
  when either tier is red, then run the deps gate and hand its approving
  verdict back to the caller to merge, park a major bump with `HITL`, or park
  an exhausted one with `AFK:deps-failed`. Config-driven: the lane is on only
  when the Harness config declares a `dependabot` block and `enabled` is not
  false. Invoked (blocking) by `/auto-agent:afk-pickup` when its PR triage
  returns reason `dependabot` (or reason `conflict` on a `dependabot/…`
  branch); a human may invoke it the same way.
argument-hint: '--pr <PR_NUM> --branch <BRANCH> --sha <HEAD_SHA> --reason <dependabot|conflict> [--security <true|false> --major <true|false>] [--agent-commits <true|false>]'
---

# Deps Land — the Dependabot Gate-and-Merge Lane

You are the **deps lane**, spawned by `/auto-agent:afk-pickup` when its PR
triage picks a **Dependabot PR**. One Fire = one Bot PR driven to exactly one
terminal outcome: `merged`, `HITL`, `deps-failed` or `superseded`. There is no
backing issue, so there is no issue lock, no ticket comment and no code review:
a bump is judged by evidence (CI plus one real-app round), never by reading its
diff.

Every Fire is **fresh and stateless**. The lane's memory is the sha-keyed marker
comments `deps-lane marker-emit` writes on the PR, so a Fire that crashes
mid-Tier-B resumes at the next step instead of redoing the round, and a
force-push (a Dependabot `rebase`/`recreate`, or our own fix commit)
invalidates every marker, because a verdict earned on an older sha says nothing
about the code that would actually merge.

**Step order**, with the PR state re-read before every step: (0) pre-flight and
the lane gate; (1) the conflict path, when that is the reason; (2) retitle;
(3) inject the Bot-PR checklist; (4) Tier A; (5) Tier B; (6) the gate, then the
caller's merge, the `HITL` hand-off or the `AFK:deps-failed` park. Any step
whose sha-keyed marker matches the current head is skipped.

**This skill never merges anything itself.** The gate decides, the caller
merges, at the one call site in the harness that can land a commit on the
default branch.

## Harness context

Every repo fact comes from the Harness config (ADR 0002). Read it once, first
thing, and never spell one as a literal:

```bash
AA="${AUTO_AGENT_ROOT:?the Fire wrapper exports AUTO_AGENT_ROOT}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")
CAP=$(jq -r .rounds.deps_fix <<<"$CFG")                                # the fix budget, total
LOCKFILE_CMD=$(jq -r '.commands.lockfile_refresh // empty' <<<"$CFG")  # the lockfile recipe
```

`$CAP` is the documented `rounds.deps_fix` round cap: the number of fix
attempts a bump gets **in total**, across Tier A and Tier B and across every
Fire. The lockfile recipe is the config's `commands.lockfile_refresh`; the
Bot-PR checklist is the config's `bot-pr-checklist.md` sibling, which
`deps-lane inject-checklist` reads itself. None of them is ever retyped here.

**Output discipline.** The caller reads ONLY the text you write in your own
assistant messages. Every machine line below (`deps-land: …`, `merge-cmd: …`,
`deps: …`) must be written in your assistant message, never echoed from Bash.

## Invocation

```
/auto-agent:deps-land --pr <PR_NUM> --branch <BRANCH> --sha <HEAD_SHA> --reason <dependabot|conflict> \
                      [--security <true|false>] [--major <true|false>] [--agent-commits <true|false>]
```

**The arguments differ by reason, because PR triage emits two different
verdict shapes** (`pr_triage_pick` in `lib/pr-triage.sh`), and a lane that
assumed one shape for both would be dispatched `--sha null` on every conflict:

| reason       | verdict carries                                   | dispatched with                                                                     |
| ------------ | ------------------------------------------------- | ----------------------------------------------------------------------------------- |
| `dependabot` | `pr`, `branch`, `sha`, `security`, `major`, tiers | `--pr --branch --sha --security --major --reason dependabot`                        |
| `conflict`   | `pr`, `branch`, `agentCommits`, no sha or flags   | `--pr --branch --sha <read by the caller> --reason conflict --agent-commits <bool>` |

`--pr`, `--branch`, `--sha` and `--reason` are always present. `--security` and
`--major` are required for reason `dependabot` and absent for reason
`conflict`: a conflict Fire ends in §1, before the retitle (the only reader of
`security`) and the gate (the only reader of `major`). When they are absent the
report line prints `security=n/a major=n/a`, never `null` and never a guess.
`--agent-commits` is required for reason `conflict` and ignored otherwise.

## Process

### 0. Pre-flight, and the lane is on or there is no Fire

```bash
gh auth status >/dev/null || { echo "deps-land: ERROR — gh not authenticated"; exit 1; }
LANE=$("$AA" deps-lane lane); LANE_RC=$?
```

The lane is on only when the Harness config declares a `dependabot` block and
its `enabled` is not false. Off (exit 3), write

```
deps-land: SKIPPED — <the deps-lane: off … line>
```

and stop, mutating nothing. A Fire never gets here with the lane off (triage
asks the same resolved `lanes.deps_land` and never hands a Fire a Bot PR the
lane would refuse), but a human can, and the answer is the same.

**Never operate on a PR whose head branch is not `dependabot/…`.** The gate's
`not-dependabot` refusal is the backstop; refuse before spending anything.

**Bootstrap state.** Tier B needs the hermetic tier. When
`"$AA" bootstrap state | jq -r .bootstrap` is `true` the lane cannot prove a
bump, so it parks it for a human instead of re-picking it every Fire: draft it
(`gh pr ready "$PR" --repo "$REPO" --undo`, which is what stops triage),
add `AFK:verify-human`, post one comment saying the Target Project declares no
hermetic tier so Tier B cannot run and that marking the PR ready for review
once one is declared hands it back to the lane (drafts are invisible to
triage, so the un-draft is the whole re-entry), then write
`deps-land: SKIPPED — Bootstrap state, AFK:verify-human applied` and stop.

**Re-read the PR state before every step.** A Dependabot PR is a moving target:
the bot force-pushes it, supersedes it with a newer bump and closes this one,
or a human closes it to reject the upgrade. One cheap read stands between the
lane and fixing, verifying or merging a dead branch:

```bash
STATE=$(gh pr view "$PR" --repo "$REPO" --json state,isDraft,headRefOid,title,body,labels,reviewDecision,mergeable)
```

If `state != OPEN`, the PR is a draft, or `headRefOid` no longer equals the
`--sha` this Fire was dispatched with, the PR was **closed or superseded**
under us: stop with outcome `superseded`. No label, no comment, nothing mutated
beyond the report. Dependabot closes its own PRs constantly; a superseded PR is
not a failure and must never be labelled like one. The next Fire picks the PR
up on its new sha with a clean slate.

Read the fix budget once, from the markers, and keep it for §4 and §5:

```bash
# Read the comments into a variable FIRST and check that read's own status.
# Never one pipeline into marker-parse: a pipeline reports only its last
# command's status, and marker-parse given a sha and empty stdin answers
# `{"fixAttempts":0,…}`, so a failed read would refund the whole budget.
COMMENTS=$(gh api "repos/$REPO/issues/$PR/comments" --paginate --jq '.[].body') \
  || COMMENTS="__unreadable__"
if [ "$COMMENTS" != "__unreadable__" ]; then
  MARKERS=$("$AA" deps-lane marker-parse "$SHA" <<<"$COMMENTS") || MARKERS=""   # {"sha","tierA","tierB","fixAttempts","capReached"}
  ATTEMPTS=$(jq -r '.fixAttempts // empty' <<<"$MARKERS")
fi
```

When the comment read failed, `marker-parse` failed or `ATTEMPTS` is empty,
write `deps-land: ERROR — bot marker history unreadable` and stop: a PR whose past
attempts cannot be counted is exactly the PR that must not be handed the full
budget. The count comes only from the markers, never from a variable in this
Fire, so a crash cannot refund an attempt.

The two steps that touch git (§1's rebase, §5's fix) run on a fresh checkout
of the branch, so local state is exactly what the PR shows:

```bash
git fetch origin
git checkout "$BRANCH"
git reset --hard "origin/$BRANCH"
```

**How a sha-keyed marker binds across Fires**: every fix push moves the head,
and `marker-parse` deliberately ignores markers for any other sha. So **after
every fix push, re-stamp the whole accumulated history onto the NEW head
sha**: one `fix-attempt` marker per attempt spent so far, earlier Fires
included, in a single comment (§5; `/auto-agent:pr-watch` does the same). The
count then resets only when the head moves without our markers following it (a
Dependabot rebase or a new version push), which is correct: that is a
different bump.

### 1. Conflict path (`--reason conflict` only)

A conflicting Bot PR cannot be verified: whatever the tiers prove is about a
merge base that no longer exists. Fix the branch, then end the Fire; the next
Fire re-picks the PR on its new sha and runs the tiers.

- **`--agent-commits false`** (only Dependabot's own commits): post
  `@dependabot rebase` and stop. Dependabot owns its branch; nudging it is
  cheaper and safer than rebasing it ourselves.

  ```bash
  gh pr comment "$PR" --repo "$REPO" --body "@dependabot rebase"
  ```

- **`--agent-commits true`** (this lane has pushed fix commits): Dependabot
  would refuse the nudge, so drive the rebase with the same Rebase Driver
  `/auto-agent:pr-reconcile` uses:

  ```bash
  . "$AUTO_AGENT_ROOT/lib/rebase-driver.sh"
  VERDICT=$(rebase_onto "$BRANCH")      # {"status":"CLEAN"|"CONFLICT"|"ERROR",...}
  ```

  `CLEAN` → `rebase_push "$BRANCH"` (the only sanctioned force site).
  `CONFLICT` on a dependency bump is a lockfile conflict: when `$LOCKFILE_CMD`
  is set, spawn one `auto-agent:implementer` to regenerate the lockfile with
  exactly that command, staged but not committed, then `rebase_continue` and
  `rebase_push`, as pr-reconcile does. `ERROR` or a rejected lease (the branch
  moved) is transient: `rebase_abort` and end the Fire `ERROR`; the next Fire
  retries on whatever head it finds. A conflict only a human can resolve (an
  empty `$LOCKFILE_CMD`, or an implementer that cannot produce a resolution) →
  `rebase_abort`, then park the bump per §6 naming it as the last failure.

Either way the Fire ends here with `tierA=skipped tierB=skipped`.

### 2. Retitle (once, idempotent)

Runs first so a PR-title check re-runs and is green by the time the gate reads
it. A security bump must land as `fix(deps):` so release notes say a
vulnerability was fixed:

```bash
TITLE=$(jq -r .title <<<"$STATE")
NEW_TITLE=$("$AA" deps-lane retitle "$TITLE" "$SECURITY"); RETITLE_RC=$?
if [ "$RETITLE_RC" -ne 0 ] || [ -z "$NEW_TITLE" ]; then
    :   # deps-land: ERROR — retitle refused the security flag; title left untouched
elif [ "$NEW_TITLE" != "$TITLE" ]; then
    gh pr edit "$PR" --repo "$REPO" --title "$NEW_TITLE"
fi
```

**Check the exit status before editing anything.** `retitle` returns 2 and
prints NOTHING whenever the flag is not exactly `true`/`false`, which is what a
missing `security` field yields. An unchecked empty title differs from
`$TITLE`, and `gh pr edit --title ""` destroys the only record of which
dependency moved. A refused flag ends the Fire `ERROR` with nothing mutated.
The lib owns the rule (only the exact `chore(deps):` prefix is promoted); never
retype the transform.

### 3. Inject the Bot-PR checklist

Tier B verifies the boxes in the PR body, so the body must carry them. The
checklist is the Target Project's `bot-pr-checklist.md` beside `harness.json`,
pasted verbatim inside the harness's markers:

```bash
BODY=$(jq -r .body <<<"$STATE")
NEW_BODY=$(printf '%s' "$BODY" | "$AA" deps-lane inject-checklist); INJECT_RC=$?
if [ "$INJECT_RC" -ne 0 ] || [ -z "$NEW_BODY" ]; then
    :   # rc 2: no checklist, or one with no item → deps-land: ERROR — <its stderr line>
        # rc 3: the body could not be buffered → WARN, body left untouched, next Fire retries
elif [ "$NEW_BODY" != "$BODY" ]; then
    printf '%s' "$NEW_BODY" | gh pr edit "$PR" --repo "$REPO" --body-file -
fi
```

**Check the exit status, and pipe into `--body-file -`.** Every refusal prints
NO stdout; an unchecked empty body differs from `$BODY`, and
`gh pr edit --body ""` wipes Dependabot's release notes, changelog and commit
list, unrecoverably. Exit 2 is a Target Project problem (no
`bot-pr-checklist.md`, or one with no unchecked `- [ ]` item under a
`## Manual verification` heading): end the Fire `ERROR` naming it, because a
Tier B round over zero items would pass a bump nobody looked at. Exit 3 is
transient: skip the inject and let the next Fire retry.

Injection is a **no-op when the `<!-- bot-pr-checklist v1 -->` marker is
already present**, so ticked boxes survive a re-round. When Dependabot
regenerates the body the section disappears and is re-appended unticked, which
is right: a regenerated body means a new push, so everything is re-verified.

### 4. Tier A — CI green (`/auto-agent:pr-watch --bot`)

Re-read the PR (§0). If the markers carry `tierA=green` for this sha, **skip
this step**: a prior Fire proved it and nothing has moved since.

Otherwise spawn `/auto-agent:pr-watch` via the `Agent` tool
(`subagent_type: general-purpose`, `run_in_background: false`, blocking; never
pass `model:`, the Fire's model policy carries through) with the prompt:

`"Invoke the /auto-agent:pr-watch skill with --pr <PR> --branch <BRANCH> --issue none --bot."`

Bot mode reads the same markers, so its fix budget is the shared `$CAP`, its
fix commits carry the `[dependabot skip]` trailer, and its exhaustion park is
the same `deps-lane park`. Consume its terminal line verbatim:

- `pr-watch: PASS — all checks green at attempt <K> (bot)` → **re-read the head
  sha before recording anything**. pr-watch may have pushed fix commits, and a
  marker keyed to the dispatched sha would vouch for code that no longer exists:

  ```bash
  GREEN_SHA=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid)
  gh pr comment "$PR" --repo "$REPO" --body "$("$AA" deps-lane marker-emit tierA "$GREEN_SHA")"
  ```

  `tierA` is `green` when `K` was the first attempt and `fixed(k)` when
  pr-watch pushed `k` fixes. When `GREEN_SHA` differs from `$SHA` the PR moved
  under this Fire: end it here with `tierB=skipped`; the next Fire re-dispatches
  on the new sha, skips Tier A on the marker just written and runs Tier B. When
  it is unchanged, continue to Tier B in this Fire.
- `pr-watch: DRAFT — exhausted <CAP> attempts, marked draft, AFK:deps-failed` →
  the budget is gone and pr-watch has parked the PR. Go to §6's exhausted
  branch with `tierA=failed`.
- `pr-watch: ERROR — <reason>` → end the Fire `ERROR`, touch nothing.

### 5. Tier B — one real-app round (`/auto-agent:verify-pr --force-tour`)

Re-read the PR (§0). If the markers carry `tierB=PASS` for this sha, skip this
step.

Otherwise spawn one blocking `/auto-agent:verify-pr` round the same way:

`"Invoke the /auto-agent:verify-pr skill with --pr <PR> --force-tour."`

`--force-tour` is mandatory. A dependency bump is usually a lockfile-only diff,
which the Surface detector reads as "no UI touched", and a bump whose whole risk
is that it breaks pixels or a native dependency would then land with no pixels
captured. Forced, the round tours every `browser`/`electron` Surface on every
Bot PR, so a human can diff the shots by eye from one bump to the next.

Read the round's terminal `manual-verify:` line. **A Bot PR round passes only
at `<n>/<n> PASS, 0 deferred, 0 FAIL` with `n` > 0**: any FAIL, and equally
`deferred > 0`, is a Tier B failure. A deferral is how a round says "I could not
check this", and on a bump nobody is going to check it later.

- PASS → record the marker and go to §6:

  ```bash
  gh pr comment "$PR" --repo "$REPO" --body "$("$AA" deps-lane marker-emit tierB "$SHA")"
  ```

- FAIL / deferrals → the **same fix loop and the same counter as Tier A**.
  Check the budget first: `"$AA" deps-lane rounds-left "$ATTEMPTS"`; at 0, go to
  §6's exhausted branch. Otherwise spawn one `auto-agent:implementer`
  (blocking) with the failing and deferred items' text verbatim as the brief
  (lead with: this is a Dependabot DEPENDENCY BUMP; most failures are a stale
  lockfile or an API change the bump brought in; when `$LOCKFILE_CMD` is set,
  name it as the only way to regenerate the lockfile, and never name an empty
  one). It stages, never commits. Then commit through the trailer helper, push,
  and record the attempt **against the sha the push just created**,
  re-stamping the whole accumulated history:

  ```bash
  MSG=$(printf 'fix(deps): deps-land tier B fix attempt %s\n\n%s\n' "$((ATTEMPTS + 1))" "<implementer summary>" \
        | "$AA" deps-lane commit-trailer)
  git commit -m "$MSG"
  git push origin "$BRANCH"           # plain push, never --force
  N=$((ATTEMPTS + 1))
  NEW_SHA=$(git rev-parse HEAD)
  BODY_MARKERS="deps-land tier B fix attempt $N of $CAP on this bump."$'\n'
  for i in $(seq 1 "$N"); do
      BODY_MARKERS="$BODY_MARKERS$("$AA" deps-lane marker-emit fix-attempt "$NEW_SHA" "$i")"$'\n'
  done
  gh pr comment "$PR" --repo "$REPO" --body "$BODY_MARKERS"
  ```

  Every detail is load-bearing. Without the `[dependabot skip]` trailer,
  Dependabot treats the branch as human-owned and stops rebasing it. `N` must
  be a real number: `marker-emit` prints nothing without one, the comment
  fails, and the attempt goes unrecorded. The markers must name `$NEW_SHA`, not
  `$SHA`: an attempt stamped on the pre-push sha is invisible to the next Fire,
  so the bump would be fixed and re-fired forever and never reach
  `AFK:deps-failed`. The push moves the head, so the Fire ends there: the next
  Fire re-verifies both tiers on the new sha and reads the re-stamped count.

- `manual-verify: infra-error …` (the environment never booted; zero items
  acted on) is **not** a Tier B failure and must not consume an attempt: end
  the Fire `ERROR`, and the next Fire retries the round.

### 6. Gate, then merge / hand off / park

Re-read the PR (§0) one last time, then run the gate. It decides and never
mutates; it reads the repo, the required checks and the fix cap from the
Harness config, so it takes no `--repo`:

```bash
GATE_JSON=$("$AA" deps-gate --pr "$PR" --head "$SHA" --major "$MAJOR" --security "$SECURITY"); GATE_RC=$?
```

**Approved (exit 0).** The lane does **not** merge. It prints the gate's merge
command and the caller (`/auto-agent:afk-pickup`) runs it at the same call site
the docs-merge gate uses. The command has an exact, parseable shape: **one
line, starting `merge-cmd: `, carrying `.mergeCmd` byte-for-byte**, written
directly after the `APPROVED` terminal line. `.mergeCmd` is the harness's one
admin-squash recipe (`--squash --admin --match-head-commit <sha>`). Never
retype, reformat or line-wrap it, and never write a `merge-cmd:` line on any
other verdict: that line is the merge trigger, and the caller refuses anything
that does not match the recipe's exact shape.

**Major bump (`.reason == major-unapproved`).** After Tier A green and Tier B
PASS a major is never merged on machine evidence; hand it to the maintainer:

```bash
gh pr edit "$PR" --repo "$REPO" --add-label HITL
gh pr comment "$PR" --repo "$REPO" --body "$HANDOFF"
```

`$HANDOFF` carries, in order: the bump summary (dependency, from → to, read
from Dependabot's first commit message), both tier verdicts, a pointer to the
screenshot tour in the PR description, and this sentence verbatim:

> Approve this PR (GitHub review) to let the daemon land it; close to reject.

That sentence is the entire re-entry protocol. A later Fire's triage sees `HITL`
with `reviewDecision == APPROVED`, re-enters this lane, re-gates the **same
sha** and merges. `Request changes` and a close both leave the PR alone.

**Exhausted.** When either tier ran out of the `$CAP` attempts (or §1's rebase
could not be finished) the PR is parked for good: drafted, labelled
`AFK:deps-failed`, and commented once. The park is one idempotent helper,
whichever tier ran out:

```bash
"$AA" deps-lane park "$PR" "$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid)" "<last failure, verbatim>"
```

It reads the PR and does only the moves still owed, so after pr-watch's own
park on Tier A it posts nothing twice and never errors on an already-draft PR.
The draft is the load-bearing half: PR triage skips drafts, so a labelled but
non-draft PR would be re-picked every Fire forever. A park that exits non-zero
did not happen: write `deps-land: ERROR — park failed: <its stderr line>`.
Otherwise the Fire ends `deps-land: DEPS-FAILED — <last failure>`.

**Any other refusal** (`checks-not-green`, `checks-missing`, `markers-stale`,
`title-not-deps`, `changes-requested`, `draft-or-closed`, `not-dependabot`)
ends the Fire with no mutation and `outcome=refused:<reason>`.
`checks-unreadable` and `usage` say nothing about the PR at all: the gate could
not run, and a recurring one is a harness bug to file, never a bump to triage.

## Output format

One block per Fire, written in your assistant message:

```
=== /auto-agent:deps-land PR #<PR_NUM> <ISO-8601> ===
deps: PR #<N> "<title>" — security=<y/n|n/a> major=<y/n|n/a> tierA=<green|fixed(k)|failed|skipped> tierB=<n/n|k/n|skipped> outcome=<merged sha|HITL|deps-failed|superseded|refused:<reason>>
pr-watch: <verbatim terminal line>          (when §4 ran)
verify:   <verbatim manual-verify line>     (when §5 ran)
gate:     approved | REFUSED — <reason> | ERROR — <reason>
result:   <terminal line, below>
```

The `deps:` line is the one the caller copies into its report. `tierA` and
`tierB` are `skipped` when a marker for this sha made the step a no-op or the
Fire ended before it. On the merge path the caller rewrites `outcome=` to
`merged <sha>` after its merge command succeeds (the lane cannot know: it does
not merge).

Terminal lines, parsed verbatim by the caller:

- `deps-land: APPROVED — PR #<N> ready to merge at <sha>`, immediately followed
  by one `merge-cmd: <gate .mergeCmd verbatim>` line, which the caller runs
- `deps-land: HITL — major bump handed to the maintainer at <sha>`
- `deps-land: DEPS-FAILED — <last failure>`
- `deps-land: SUPERSEDED — PR #<N> is closed, drafted or has moved past <sha>`
- `deps-land: REBASE-NUDGED — @dependabot rebase posted` /
  `deps-land: REBASED — pushed <sha>`
- `deps-land: REFUSED — <gate reason>`
- `deps-land: SKIPPED — <deps-lane: off … line | Bootstrap state, AFK:verify-human applied>`
- `deps-land: ERROR — <reason>`

A Fire that ran Tier A MUST carry the verbatim `pr-watch:` line, and one that
ran Tier B the verbatim `verify:` line, before any result is written;
`(in flight)` is never a legal value for either.

## Failure modes

- **PR closed or superseded mid-Fire**: every step re-reads state, so the lane
  stops at the next boundary with `SUPERSEDED`. No label, no comment:
  Dependabot supersedes its own PRs routinely.
- **Head sha moved**: same treatment. Markers are sha-keyed; the next Fire
  re-verifies from scratch on the new head.
- **Fix loop exhausted**: draft + `AFK:deps-failed` + one comment naming the
  last failure, through `deps-lane park`. Triage skips drafts and that label, so
  the PR is never re-picked.
- **Major bump approved, then force-pushed**: the re-entry Fire re-gates the
  new sha, finds the markers stale, and re-runs both tiers before merging. An
  approval never vouches for code the maintainer did not see.
- **Dependabot refuses to update the branch**: expected once this lane has
  pushed a fix commit; that is why fix commits carry `[dependabot skip]` and
  why the conflict path switches to the Rebase Driver when `agentCommits` is
  true.
- **Gate approves but the merge fails**: the caller's `--match-head-commit`
  refused because the branch moved between gate and merge. Nothing unverified
  lands, and the next Fire re-verifies the new sha.

## Boundaries

- **Never merges.** The gate decides, the caller merges. No code path here runs
  `gh pr merge`.
- Never merges, or asks the caller to merge, a major bump without an
  `APPROVED` review; the gate refuses it and §6 hands it off instead.
- Never applies `AFK:checks-failed` (the agent lane's label) to a Bot PR, and
  never applies `AFK:deps-failed` to anything else.
- Never reviews the bump's diff or opens threads on a Bot PR: evidence only.
- Never extends the `rounds.deps_fix` cap, and never counts attempts from
  anywhere but the markers.
- `git push --force-with-lease` only through the Rebase Driver in §1;
  everywhere else pushes are append-only.
- One Bot PR per Fire; the Daemon's budget gate paces successive Fires, and
  triage ranks Bot PRs below every Agent PR, so Dependabot never starves
  feature work.
