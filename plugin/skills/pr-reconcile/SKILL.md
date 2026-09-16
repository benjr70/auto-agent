---
name: pr-reconcile
description:
  Bring one open Agent PR back to a mergeable, review-clean state: rebase it
  over the default branch when it conflicts, fix the review comments a human
  (or `/auto-agent:pr-review`) handed back via the `AFK:revise` label
  (replying in-thread with what changed and resolving each thread), then
  re-run the full CI + manual verification tail. Invoked (blocking) by
  `/auto-agent:afk-pickup` §1.2 when its PR triage picks a PR needing
  attention. Takes the PR number + branch + issue number + reason.
---

# PR Reconcile — Autonomous PR Feedback + Conflict Fixer

You are the **reconciler** spawned by `/auto-agent:afk-pickup` when an
already-open Agent PR needs attention: the default branch moved under it (merge
conflict), a human reviewed it and handed it back with the `AFK:revise` label,
and/or its bot tail never finished (`incomplete` — a prior Fire died mid-tail).
One Fire = one PR brought back to green — rebased, comments addressed with
in-thread replies, CI re-watched, manual verification re-run — or escalated
with a parked label.

Every run is **fresh and stateless**: context is reconstructed from the issue
body, the PR diff, and the review threads. No session is ever resumed.

This skill assumes:

- The PR was opened by `/auto-agent:afk-pickup` on `feat/issue-<N>` against the
  Target Project's default branch.
- `lib/rebase-driver.sh`, `lib/thread-reconciler.sh` and `lib/review-poster.sh`
  exist in the Harness install (sourceable deep modules — do not hand-roll
  their git/GraphQL/REST).
- The caller (`/auto-agent:afk-pickup` §1.2) already flipped the backing issue
  `AFK:done → AFK:in-progress` as the single-flight lock and will restore it;
  this skill never touches that lock itself.

## Harness context

Every repo fact comes from the Harness config the Fire wrapper resolved and
exported; nothing below names a repo, a branch or a round cap as a literal.
Read it once, first:

```bash
AA="${AUTO_AGENT_ROOT:?the Fire wrapper exports AUTO_AGENT_ROOT}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")              # the Target Project, from its origin remote
BASE=$(jq -r .repo.default_branch <<<"$CFG")    # detected from GitHub, never declared
REVISE_ROUNDS_MAX=$(jq -r .rounds.revise <<<"$CFG")        # replaces the literal 3
MANUAL_ROUNDS_MAX=$(jq -r .rounds.manual_verify <<<"$CFG") # replaces the literal 3 in the tail
HERMETIC=$(jq -c .verification.hermetic <<<"$CFG")          # null = Bootstrap state
```

Every `gh` call below carries `--repo "$REPO"`. The libs read the same config
(`HARNESS_CONFIG_JSON`) on their own; they take no repo argument.

## Invocation

```
/auto-agent:pr-reconcile --pr <PR_NUM> --branch <BRANCH> --issue <ISSUE_N> --reason <revise|conflict|both|incomplete>
```

All four arguments required, supplied verbatim from `/auto-agent:afk-pickup`'s
triage verdict (`reason` is the triage pick reason; when the PR both conflicts
and carries `AFK:revise`, the caller passes `both`). Reason `incomplete` means
the PR carries no attention label and no conflict, but its bot tail never
finished: the one-time review marker (`<!-- pr-review-done -->`) and/or any
manual verification round comment is missing — a prior Fire died mid-tail. §1
and §2 are then natural no-ops; §3 is the whole job.

## Process

### 0. Pre-flight

```bash
gh auth status >/dev/null || { echo "pr-reconcile: ERROR — gh not authenticated"; exit 1; }
gh pr view "$PR_NUM" --repo "$REPO" --json state,isDraft,headRefName,mergeable,labels \
  | jq -e --arg br "$BRANCH" '.state == "OPEN" and (.isDraft | not) and .headRefName == $br' >/dev/null \
  || { echo "pr-reconcile: ERROR — PR #$PR_NUM not open on $BRANCH (or draft)"; exit 1; }
```

Record from that view: `MERGEABLE` (the current mergeable state — re-read it
here, triage's snapshot may be stale) and whether `AFK:revise` is present.
`mergeable == UNKNOWN` at this point: poll `gh pr view --repo "$REPO" --json
mergeable` every 20s up to 3 minutes for GitHub to finish computing; still
UNKNOWN → treat as not conflicting (the comment phase can still run).

Check out the branch fresh:

```bash
git fetch origin
git checkout "$BRANCH"
git reset --hard "origin/$BRANCH"   # local state = exactly what the PR shows
```

### 1. Rebase phase (runs first, only when CONFLICTING)

Ordering is deliberate: land on a clean, mergeable base **before** touching
review comments, so comment fixes are written against post-rebase code and the
final CI run covers everything.

Source the Rebase Driver and attempt the rebase — **cap: 1 rebase attempt per
Fire**. The driver rebases onto `origin/<default branch>` from the Harness
config; never pass a base name:

```bash
. "$AUTO_AGENT_ROOT/lib/rebase-driver.sh"
VERDICT=$(rebase_onto "$BRANCH")          # {"status":"CLEAN"|"CONFLICT","files":[...]}
```

- **CLEAN** → push and continue to §2:

  ```bash
  rebase_push "$BRANCH"                    # --force-with-lease, the ONLY sanctioned force site
  ```

- **CONFLICT** → spawn one **implementer** (blocking,
  `subagent_type: auto-agent:implementer`) to resolve **in place**. Prompt
  embeds: the issue title + body, the conflicted file list from the verdict, and
  these instructions verbatim:

  > A rebase of `<BRANCH>` onto `origin/<BASE>` (the default branch) stopped
  > on conflicts in the files listed. Resolve each conflict so the branch's
  > intent AND the default branch's changes both survive. Edit the files to
  > remove all conflict markers, then `git add` each resolved file. Do NOT run
  > `git rebase --continue`, do NOT commit, do NOT push — the wrapper drives
  > the rebase. Reply with a short summary per file when everything is staged.

  Then drive the rebase to completion — a multi-commit rebase may stop more than
  once; each stop gets the same implementer treatment:

  ```bash
  VERDICT=$(rebase_continue)               # repeat resolve→continue per CONFLICT stop
  ```

  When CLEAN → `rebase_push "$BRANCH"`.

- **Escalation** — on ANY of: `rebase_onto`/`rebase_continue` returns ERROR, the
  implementer cannot produce a resolution, or `rebase_push` returns REJECTED
  (the lease refused — someone pushed to the branch after our fetch; never retry
  harder):

  ```bash
  rebase_abort                             # leave the branch exactly as the PR shows
  gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:rebase-failed
  gh pr comment "$PR_NUM" --repo "$REPO" --body "pr-reconcile: automatic rebase onto $BASE failed at $(date -Iseconds) — <reason: conflicts unresolvable | lease rejected (branch moved) | rebase error>. Human rebase required."
  ```

  Report `pr-reconcile: REBASE-FAILED — <reason>` and stop (skip §2–§3; a
  conflicted PR cannot land anyway). The caller restores the issue lock.

If the PR is not CONFLICTING, skip this phase entirely.

### 2. Comment phase (only when `AFK:revise` is present)

The PR was explicitly handed back — by a human review, or by
`/auto-agent:pr-review` (`/auto-agent:afk-pickup` §6a.1b), which posts its
findings as inline threads marked `<!-- pr-review-bot -->` / 🤖 and applies
this same label. Both kinds of thread are worked identically: the reconciler
only cares that a thread is unresolved, not who authored it. Work every
unresolved review thread; **cap: `REVISE_ROUNDS_MAX` implementer rounds per
Fire** (the Harness config's `rounds.revise`).

```bash
. "$AUTO_AGENT_ROOT/lib/thread-reconciler.sh"
THREADS=$(tr_unresolved_threads "$PR_NUM")
# [ {threadId, path, line, commentDatabaseId, body}, ... ]
```

No unresolved threads → the label was applied without open threads; treat the PR
body / review summary comments as the feedback source only if they contain
explicit change requests, otherwise just drop the label (§2-exit) and continue.

Round loop (`R` starts at 1, cap `REVISE_ROUNDS_MAX`):

1. **Spawn one implementer per round** (blocking,
   `subagent_type: auto-agent:implementer`) covering ALL currently-unresolved
   threads. Prompt embeds: the issue title + body, the current PR diff
   (`git diff "origin/$BASE...HEAD"`, capped at 2000 lines as in
   `/auto-agent:pr-watch` §3), and every thread verbatim — `threadId`,
   `path:line`, and comment body — plus these instructions verbatim:

   > Address each review comment by changing the shipped code accordingly. Stage
   > the changes (`git add`). Do NOT commit and do NOT push — the wrapper
   > handles that. Reply with one line per thread:
   > `<threadId>: <what you changed>` — or, if you believe a comment is wrong or
   > must not be applied, `<threadId>: revise-dispute — <one-line reason>` and
   > stage nothing for it.

2. **Commit + push** (append-only; the rebase already happened, so plain push):

   ```bash
   git commit -m "fix(review): round $R — address review comments on PR #$PR_NUM"
   git push origin "$BRANCH"                # plain push — never force here
   SHA=$(git rev-parse --short HEAD)
   ```

3. **Reply + resolve per addressed thread** — the reply lands in-thread on the
   reviewer's comment and states concretely what changed:

   ```bash
   tr_reply "$PR_NUM" "<commentDatabaseId>" "fixed in $SHA: <implementer's one-line summary for this thread>"
   tr_resolve "<threadId>"
   ```

   Disputed / unaddressed threads are NOT replied to or resolved this round —
   they carry to the next round (a dispute counts as unaddressed).

4. Re-enumerate. All threads resolved → **§2-exit**. Threads remain and
   `R == REVISE_ROUNDS_MAX` (or every remaining thread is disputed) →
   **escalate**:

   ```bash
   # One reply per still-open thread, then park the PR for a human:
   tr_reply "$PR_NUM" "<commentDatabaseId>" "pr-reconcile: could not auto-resolve after $REVISE_ROUNDS_MAX attempts — human triage."
   gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:revise-failed --remove-label AFK:revise
   gh pr comment "$PR_NUM" --repo "$REPO" --body "pr-reconcile: <k> review thread(s) could not be auto-resolved after $REVISE_ROUNDS_MAX round(s) at $(date -Iseconds). Labeled AFK:revise-failed for human triage."
   ```

   Report `pr-reconcile: REVISE-FAILED — <k> thread(s) unresolved` and stop
   (skip §3 — the PR is parked; verification runs after the human weighs in).

**§2-exit** (all threads addressed):

```bash
gh pr edit "$PR_NUM" --repo "$REPO" --remove-label AFK:revise
```

The label drop is what stops the Daemon re-picking this PR next Fire; the human
re-applies `AFK:revise` (and re-opens threads) if a fix missed.

### 3. Verification tail (when §1/§2 pushed anything — or `--reason incomplete`)

Any push (rebase or comment fix) re-ran CI and staled ALL previous evidence —
per the locked design, **all verification re-runs**. Execute
`/auto-agent:afk-pickup`'s success tail against this PR, with one modification:

- **§6a.1 pr-watch** (blocking, fresh `rounds.pr_watch` budget) — spawn
  `/auto-agent:pr-watch` via the `Agent` tool (`subagent_type: general-purpose`,
  `run_in_background: false`) exactly as `/auto-agent:afk-pickup` §6a.1
  specifies, with this PR's number/branch/issue. Record its terminal
  `pr-watch:` line verbatim.
- **§6a.1b pr-review** — marker-gated exactly as `/auto-agent:afk-pickup`
  specifies. Source `. "$AUTO_AGENT_ROOT/lib/review-poster.sh"` and check
  `rp_done_marker_present "$PR_NUM"`: a reconciled PR was normally reviewed
  when it first landed, so the `<!-- pr-review-done -->` marker makes this a
  SKIP. If the marker is absent (the PR predates `/auto-agent:pr-review`, or a
  prior attempt ended `pr-review: ERROR`), the one-time review runs here
  (spawn `/auto-agent:pr-review` via `subagent_type: general-purpose`,
  blocking); on findings it applies `AFK:revise` and the tail ends — the next
  Fire reconciles.
- **§6a.2 manual verification** (blocking) — delegate to
  `/auto-agent:verify-pr` exactly as `/auto-agent:afk-pickup` §6a.2 specifies
  (a blocking `/auto-agent:verify-pr` round per PR, spawned via
  `subagent_type: general-purpose`, consuming its terminal `manual-verify:`
  line and splitting spec-demanding deferrals into the fix loop). Because the
  rebase/fixes may have changed anything, existing ticks are stale: instruct
  the round to **re-verify every item, including ones already ticked
  `- [x]`**, and head its evidence comment
  `### Manual verification — post-reconcile round <M>/$MANUAL_ROUNDS_MAX`.
- **§6a.3 manual fix loop** — identical semantics, cap `MANUAL_ROUNDS_MAX`
  (the Harness config's `rounds.manual_verify`), exhaustion → draft +
  `AFK:checks-failed` + issue comment.

The same blocking rules apply verbatim: never emit output while pr-watch or the
`/auto-agent:verify-pr` round is in flight; `pr-watch: (in flight)` is never a
legal value.

**Bootstrap state (the one legal skip).** When `$HERMETIC` is `null`, the
Target Project has no Environment provider yet, so no round can run and none is
owed. Do not park. Label the PR for the human verifier and record the skip:

```bash
gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:verify-human
```

The block's `verify:` line is then
`verify:    SKIPPED — Bootstrap state, AFK:verify-human applied`, and that line
is legal for `result: PASS` in Bootstrap state only. Nowhere else is a skip
legal.

**The round is NOT skippable outside Bootstrap state.** `/auto-agent:verify-pr`
is agent-invocable: "the harness would not run for me" is a real failure, not a
reason to move on (three consecutive reconcile Fires once each reported a clean
result with zero verification behind it). When the tail runs and `$HERMETIC`
is present, the round runs: the **only** outcome that lets this Fire report
`result: PASS` is a real `manual-verify:` verdict line. An explicitly recorded
`manual-verify: infra-error …` non-verdict (the provider never brought the
environment up — zero items acted on) is recorded verbatim as the `verify:`
line and reported like a pr-watch ERROR, exactly as `/auto-agent:afk-pickup`
§6a.2 requires — never as PASS. If the round produced neither — no
`manual-verify:` line came back, the spawn failed, the harness refused, or
`/auto-agent:verify-pr` is not among this session's skills at all (this
Harness install predates Slice #33) — that is a **missing round**, and it must
never be reported as `result: PASS` or as an ordinary skip. Park it on the
**PR**, exactly as `/auto-agent:afk-pickup` §6a.2 specifies:

```bash
gh pr comment "$PR_NUM" --repo "$REPO" --body "Manual verification round did not run: <reason>.
Parking for a human — no verification evidence exists for this PR."
gh pr ready "$PR_NUM" --repo "$REPO" --undo
gh pr edit  "$PR_NUM" --repo "$REPO" --add-label AFK:checks-failed
```

The draft flip is the load-bearing half. PR Triage (`lib/pr-triage.sh`) keys
on PR state and PR labels only — it never reads the backing issue, and the park
comment deliberately does not match its `Manual verification — .*round` marker
— so **drafting is what takes the PR out of the `incomplete` class**. A label
with no draft leaves the PR open, non-draft and still incomplete, and the very
next Fire re-picks it in an unbounded loop. (Park on the PR only: `$ISSUE_N`
names the backing issue here, but triage cannot see issue labels at all.)

Then emit `verify: MISSING — <reason>` as the block's `verify:` line (for the
uninstalled-skill case the reason is exactly
`/auto-agent:verify-pr is not installed in this Harness install`) and
`result: ERROR — manual verification round did not run` as the verdict.

If neither §1 nor §2 pushed a commit (e.g. `AFK:revise` with zero actionable
threads) **and `--reason` is not `incomplete`**, skip the tail — nothing
changed, existing evidence stands.

When `--reason incomplete`, ALWAYS run the tail even with no push: the tail
itself is the missing work (pr-watch to green, the marker-gated one-time review,
a verification round). On a no-push `incomplete` run existing ticks are NOT
stale — the `/auto-agent:verify-pr` round uses `/auto-agent:afk-pickup` §6a.2's
standard semantics (unchecked items only), headed
`### Manual verification — round <M>/$MANUAL_ROUNDS_MAX` with `M` = 1 + the
count of existing round comments (respecting `MANUAL_ROUNDS_MAX`; at cap with
FAILs it exhausts into draft as usual). Convergence: if the review marker is
absent, §3's pr-review may apply `AFK:revise` and end the tail — the next Fire
re-picks the PR as `revise`; either way the PR exits the `incomplete` class
every Fire (marker/round posted, `AFK:revise` applied, `AFK:verify-human`
applied in Bootstrap state, or parked), so the pick can never loop.

## Output format

One block per Fire, written to stdout:

```
=== /auto-agent:pr-reconcile PR #<PR_NUM> <ISO-8601> ===
reason:    revise | conflict | both | incomplete
rebase:    CLEAN — pushed | SKIPPED | FAILED — <detail>
comments:  <k> thread(s) addressed in <R> round(s) | SKIPPED | FAILED — <n> unresolved
pr-watch:  <verbatim terminal line>            (when §3 ran)
verify:    <verbatim manual-verify line> — post-reconcile   (when §3 ran and pr-watch PASS)
           | SKIPPED — Bootstrap state, AFK:verify-human applied   (no hermetic tier)
           | MISSING — <reason>                             (§3 park: round never ran)
result:    PASS | REBASE-FAILED | REVISE-FAILED | DRAFT | ERROR — <detail>
```

The final `result:` line doubles as the terminal verdict the caller parses:

- `pr-reconcile: PASS — rebased and/or <k> comment(s) addressed, checks green, manual verify clean`
- `pr-reconcile: REBASE-FAILED — <reason>`
- `pr-reconcile: REVISE-FAILED — <k> thread(s) unresolved`
- `pr-reconcile: DRAFT — verification tail exhausted, marked draft, AFK:checks-failed`
- `pr-reconcile: ERROR — <reason>`

The hard validity rule from `/auto-agent:afk-pickup` §7 applies: when §3 ran,
the block MUST carry the verbatim `pr-watch:` terminal line (and `verify:` on
PASS) before the result is emitted. **When §3 ran**, `result: PASS` requires a
`verify:` line holding a real `manual-verify:` verdict, or — in Bootstrap state
only — the `SKIPPED — Bootstrap state` line; `verify: MISSING — <reason>`, a
recorded `manual-verify: infra-error …`, and an absent line are all
disqualifying, and a `pr-reconcile: PASS` emitted without a round that actually
ran is an invalid Fire. The rule is scoped to the tail actually running: on the
§3 skip path (neither §1 nor §2 pushed and `--reason` is not `incomplete`) no
round was owed, so the block legally carries no `pr-watch:`/`verify:` lines at
all and `result: PASS` stands on the PR's existing evidence — that Fire must not
park a healthy PR.

## Failure modes

- **Lease push rejected** — someone (human) pushed to the PR branch between our
  fetch and push. Never force through it: abort, `AFK:rebase-failed`, park.
  Their work is untouched — that is the point of the lease.
- **Implementer disputes a review comment** — the loop never argues with a
  human's review by force; the thread stays open, and if disputes are all that
  remain, the PR parks as `AFK:revise-failed` with in-thread explanations.
- **`AFK:revise` applied but no unresolved threads** — drop the label; there is
  nothing machine-actionable. The human should leave inline review comments (not
  just a top-level comment) to hand work back.
- **PR turns draft / closes mid-reconcile** — stop at the next step boundary,
  report `pr-reconcile: ERROR — pr no longer open`, touch nothing further.
- **Verification tail exhausts** — same escalation as `/auto-agent:afk-pickup`:
  draft + `AFK:checks-failed`; report DRAFT. The reconcile's own labels are NOT
  applied (the tail failing is a checks problem, not a revise/rebase problem).
- **Verification round never ran** — the harness refused, the spawn failed, or
  the agent returned no `manual-verify:` line. Do NOT treat it as a skip and do
  NOT pass: draft (`gh pr ready --undo`) + `AFK:checks-failed` on the **PR**, an
  explanatory comment on the PR, and `verify: MISSING — <reason>` with
  `result: ERROR`. The draft is what stops PR Triage re-picking it. Before
  parking, check that `/auto-agent:verify-pr` is listed among this session's
  skills — an install without it is the known cause, and the reason line names
  it. The one exception is Bootstrap state (`$HERMETIC` null), where no round
  is owed and the PR is labelled `AFK:verify-human` instead.
- **Crash mid-Fire** — the caller's
  `picked:   reconcile PR #<PR_NUM> (issue #<N>)` log line lets the Fire
  wrapper's crash cleanup (`lib/fire.sh`) restore the issue lock
  (`AFK:in-progress` cleared, `AFK:done` restored).

## Boundaries

- `git push --force-with-lease` is permitted **only** in §1 to publish a
  conflict rebase — never in §2, never in the verification tail, never plain
  `--force` anywhere. Everything else is append-only plain push.
- Never merges the PR. Green + resolved threads is the verdict; merge stays
  human-gated.
- Never edits the PR's acceptance criteria, the issue body, or a reviewer's
  comments. Replies are additive.
- Never resolves a thread it did not just address with a pushed commit.
- Never operates on a PR whose head is not `feat/issue-<N>` (§0 enforces).
- Never touches the `AFK:in-progress`/`AFK:done` lock — the caller owns it.
- Never names a repo, a branch or a cap as a literal — every one comes from
  the Harness config read in "Harness context".
- One PR per Fire; the Daemon's budget gate paces successive Fires.
