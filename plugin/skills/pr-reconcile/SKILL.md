---
name: pr-reconcile
description:
  Bring one open Agent PR back to a mergeable, review-clean state: rebase it
  over the default branch when it conflicts, fix the review comments a human
  (or `/auto-agent:pr-review`) handed back via the `AFK:revise` label
  (replying in-thread with what changed and resolving each thread), rule the
  implementer's disputes on bot threads through the read-only Arbiter inside
  the same Fire, carry what only the human can decide (a product ambiguity, a
  dispute on a thread the human wrote) to them as one consolidated Ruling
  request — posted after the full CI + manual verification tail has re-run on
  the fixed head — and, on a Fire re-picked for the human's one-line Ruling
  (`1A 2B`), apply exactly it. Invoked (blocking) by `/auto-agent:afk-pickup`
  §1.2 when its PR triage picks a PR needing attention. Takes the PR number +
  branch + issue number + reason.
---

# PR Reconcile — Autonomous PR Feedback + Conflict Fixer

You are the **reconciler** spawned by `/auto-agent:afk-pickup` when an
already-open Agent PR needs attention: the default branch moved under it (merge
conflict), a human reviewed it and handed it back with the `AFK:revise` label,
its bot tail never finished (`incomplete` — a prior Fire died mid-tail), or
the human answered an outstanding Ruling request (`ruling`). One Fire = one
PR brought back to green — rebased, comments addressed with in-thread replies,
CI re-watched, manual verification re-run — or, when a decision is the
human's to make, left waiting on one consolidated Ruling request with the
tail's evidence already on it; a parked label is for a fix that still fails.

Every run is **fresh and stateless**: context is reconstructed from the issue
body, the PR diff, and the review threads. No session is ever resumed.

This skill assumes:

- The PR was opened by `/auto-agent:afk-pickup` on `feat/issue-<N>` against the
  Target Project's default branch.
- `lib/rebase-driver.sh`, `lib/thread-reconciler.sh`, `lib/review-poster.sh`
  and `lib/ruling.sh` exist in the Harness install (sourceable deep modules —
  do not hand-roll their git/GraphQL/REST, and never hand-roll the Ruling
  request comment, its grammar or its labels: `"$AA" ruling …` owns them).
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
DECISIONS="$AUTO_AGENT_STATE_DIR/ruling-decisions-$PR_NUM.json"   # this Fire's collected decisions (§2)
```

Every `gh` call below carries `--repo "$REPO"`. The libs read the same config
(`HARNESS_CONFIG_JSON`) on their own; they take no repo argument.

## Invocation

```
/auto-agent:pr-reconcile --pr <PR_NUM> --branch <BRANCH> --issue <ISSUE_N> --reason <revise|conflict|both|incomplete|ruling>
```

All four arguments required, supplied verbatim from `/auto-agent:afk-pickup`'s
triage verdict (`reason` is the triage pick reason; when the PR both conflicts
and carries `AFK:revise`, the caller passes `both`). Reason `incomplete` means
the PR carries no attention label and no conflict, but its bot tail never
finished: the one-time review marker (`<!-- pr-review-done -->`) and/or any
manual verification round comment is missing — a prior Fire died mid-tail. §1
and §2 are then natural no-ops; §3 is the whole job. Reason `ruling` means the
PR carries `AFK:ruling` and a human comment newer than the Ruling request
parses as a Ruling: §2's Ruling path (step 0) is the whole job, and the tail
re-runs if it changed code. PR Triage also names `ruling` once for a reply
that is **not** a Ruling and has not been nudged yet (`reply.status`
`invalid`, `reply.nudged` false): step 0's nudge is then the whole job —
post it, report, stop — and the nudge itself is what keeps the PR from being
picked for it again. Whatever the reason, §2 always asks the lib
whether a Ruling is pending first — a human who answered the request and
re-applied `AFK:revise` by hand reaches the same path.

## Process

### 0. Pre-flight

```bash
gh auth status >/dev/null || { echo "pr-reconcile: ERROR — gh not authenticated"; exit 1; }
VIEW=$(gh pr view "$PR_NUM" --repo "$REPO" --json state,isDraft,headRefName,mergeable,labels)
# A draft is refused for every reason but `ruling`: the human's Ruling is the one thing a park never hides.
jq -e --arg br "$BRANCH" --arg reason "<reason>" \
    '.state == "OPEN" and ((.isDraft | not) or $reason == "ruling") and .headRefName == $br' <<<"$VIEW" >/dev/null \
  || { echo "pr-reconcile: ERROR — PR #$PR_NUM not open on $BRANCH (or draft)"; exit 1; }
# Parked for a human, by lib/pr-triage.sh's one definition (a draft, or a park label): empty when it is not.
PARKED=$(jq -r '[(if .isDraft then "draft" else empty end),
                 (.labels[].name | select(. == "AFK:revise-failed" or . == "AFK:rebase-failed"))]
                | join(" + ")' <<<"$VIEW")
```

**A parked PR picked for its Ruling.** A Ruling request goes out on every
exit of the tail, a DRAFT one included, so `AFK:ruling` can sit beside a park
(a draft with `AFK:checks-failed`, `AFK:revise-failed`, `AFK:rebase-failed`).
PR Triage picks such a PR for exactly one thing — reason `ruling`, once the
human's reply parses as a Ruling (or is a non-Ruling still owed its one
nudge) — and this Fire does exactly that one thing:
on a parked PR a `ruling` Fire applies the Ruling and leaves the park as it
is (or posts the nudge and leaves the park as it is). §1 is skipped (a failed rebase is the human's; never re-attempt it here),
§2 is step 0 only, §3 is skipped (the tail already failed on this PR, and
verification runs after the human repairs it), the draft state and the park
label are never touched, and the applied comment's evidence says so:
`PR still parked ($PARKED): verification not re-run`. A ruled fix is still
committed and plain-pushed; it is the human's own decision and waits on the
branch for the repair. PR Triage never hands a parked PR over for any other
reason.

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

### 1. Rebase phase (runs first, only when CONFLICTING; never on a parked PR)

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
this same label. Work every unresolved review thread; **cap:
`REVISE_ROUNDS_MAX` implementer rounds per Fire** (the Harness config's
`rounds.revise`). The cap **counts implementer rounds only**: the Arbiter step
below is not a round, and **a dismissal consumes no round** — a Fire with one
implementer round and one Arbiter run has used one round of the cap.

```bash
. "$AUTO_AGENT_ROOT/lib/thread-reconciler.sh"     # threads, marked replies, resolve
. "$AUTO_AGENT_ROOT/lib/arbiter-verdicts.sh"      # reads the two subagents' replies; round arithmetic
THREADS=$(tr_unresolved_threads "$PR_NUM")
# [ {threadId, path, line, commentDatabaseId, body,
#    authored: "bot"|"human",
#    replies: [ {databaseId, body, agent: true|false}, ... ],
#    ruling: "<latest human reply>" | null }, ... ]
```

**Thread authorship by marker.** The reconciler reads whole threads (every
comment page, not only the first) and tells its own voice from the human's by
marker, never by login (the machine user replies to its own review under the
same login). A thread is `authored: bot` iff the **first line** of its first
comment is `<!-- pr-review-bot -->`; anything else is `human` — a human
comment that quotes the marker further down is still human. Every reply this
loop posts carries exactly one of the lib's hidden markers as its first line,
and each marker means one thing — `$TR_MARKER_FIX` (`<!-- auto-agent:fix -->`)
on a reply recording a commit, `$TR_MARKER_ARBITER` on an Arbiter dismissal,
`$TR_MARKER_RULING` on a Ruling applied (with or without a commit),
`$TR_MARKER_ESCALATE` on the reply that parks a thread for a human —
and `replies[].agent` is true for exactly those (again by first line, so a
human reply quoting the loop's marker stays human). `ruling` is the latest
human reply when the human spoke last **after** the loop had replied in that
thread, and `null` when there are no replies, the loop has not replied yet,
or the loop spoke last: a human reply on a bot thread answering the loop's
own marked reply ("I agree, please resolve", "keep it as is", "do X instead")
**rules that thread**. A human follow-up on a thread the loop has never
answered is not a ruling — it is more of the thread, and the implementer
reads it whole like any other comment. Never parse a reply's visible text to
decide who wrote it — the marker is the only signal.

**Authorship decides where a dispute goes.** Every dispute goes to the
Arbiter (step 3b), which **rules** a bot thread and only **recommends** on a
human one: a dispute on a human-authored thread is collected as a decision
with the Arbiter's recommendation and reaches the human — a **human-authored
thread is never dismissed** by this loop.

No unresolved threads → the label was applied without open threads; treat the PR
body / review summary comments as the feedback source only if they contain
explicit change requests, otherwise just drop the label (§2-exit) and continue.

**Step 0 — a pending Ruling request (every reason, `ruling` above all).** Ask
the lib before any round; it reads the PR's comments once and returns the
open request (its decisions decoded) and the latest human comment after it,
parsed against the request's grammar:

```bash
rm -f "$AUTO_AGENT_STATE_DIR/ruling-open-$PR_NUM.json"   # a prior Fire's open request never merges into this one's
PENDING=$("$AA" ruling pending --pr "$PR_NUM")
# {"request": {id, head, decisions: [...], createdAt} | null,
#  "reply":   {id, body, status: "full"|"partial"|"invalid", answers: {"1":"A",…},
#              missing: [n,…], ruling: "1A 2B", reason, nudged: bool} | null}
```

- `request` null → no request outstanding; continue to the round loop.
- `reply` null → the human has not answered; the PR keeps `AFK:ruling` and
  there is nothing to apply. An outstanding request is a wait, never a
  block: continue with whatever this Fire's `--reason` owes (the round loop
  for `revise`/`both`, §3 for `incomplete`, nothing more for `conflict`),
  and only a `ruling` Fire reports
  `pr-reconcile: RULING — awaiting the human (<n> decision(s))` and stops.
  Never re-post the request and never nudge on silence. A request that was
  posted before a reconcile's own rounds (pickup §6a.4 posts on every exit
  of the tail, the `AFK:revise applied` one included) is **never orphaned**:
  keep its id and decisions aside for the Ruling exit —

  ```bash
  jq -c '.request.decisions' <<<"$PENDING" > "$AUTO_AGENT_STATE_DIR/ruling-open-$PR_NUM.json"
  OPEN_REQUEST_ID=$(jq -r '.request.id' <<<"$PENDING")
  ```

  When the rounds collect nothing new the open request stays as it is (the
  rounds' fixes do not touch it, and a Ruling applied later re-runs the
  tail on whatever head is current). When they collect a decision, the
  Ruling exit posts **one** request holding both — the open request's
  decisions first, under the numbers the human already saw, the new ones
  after — which supersedes the earlier one (`ruling pending` reads only the
  latest request, and the lib's `--supersedes` line tells the human to
  answer here). There is never a second request to answer.
- `reply.status` `invalid` → **an invalid reply changes nothing and gets one
  marked nudge** — one per request, not one per reply: when `reply.nudged` is
  false,
  `"$AA" ruling nudge --pr "$PR_NUM" --reason "<reply.reason>" <(jq -c .request.decisions <<<"$PENDING")`
  posts the one reply saying what was expected (the decisions and their
  letters); when it is true a nudge already went out on this request — post
  nothing, however many non-Rulings have followed. Labels untouched, no code
  touched. Report `pr-reconcile: RULING — invalid reply, nudged` (or
  `already nudged`) and continue as for a null reply. An un-nudged invalid
  reply is itself a `ruling` pick (PR Triage wakes the loop once for it, so
  the nudge goes out on a bot-complete PR too): on that Fire the nudge is
  the whole job. If `ruling nudge` fails to post, report
  `pr-reconcile: ERROR — ruling nudge not posted` — the next
  Fire is picked for it again, which is the retry.
- `reply.status` `full` or `partial` → **apply exactly the letters given**,
  decision by decision, nothing more and nothing less. For each `n: L` in
  `reply.answers`, the chosen option is `request.decisions[n-1].options[L]`:
  - `fix: true` → it is this Fire's work: collect it for one implementer
    round whose brief is the decision (title, scenario, **Now**, the chosen
    option's text and cost) — the instruction is the option, not the
    reviewer's original ask. The implementer stages; this session commits
    `fix(ruling): <ruling> — <option texts>` and plain-pushes. A `cannot` on
    a ruled option is not a dispute (a human ruled it): leave that decision
    unapplied, keep its thread open, and carry it into the re-posted request
    below with the implementer's reason added to the **Now** line.
  - otherwise → no code change.
  Then every applied decision that carries a `thread` gets its one-line marked
  reply through the Thread Reconciler and is resolved — the text comes from
  the lib, the marker is `$TR_MARKER_RULING` (`RULING_THREAD_MARKER`, the
  same string), never a lib-private one:

  ```bash
  TEXT=$("$AA" ruling thread-reply "$n" "$L" "<one-line summary of what the option settled>" ["$SHA"])
  tr_reply "$PR_NUM" "<thread.commentDatabaseId>" "$TR_MARKER_RULING" "$TEXT"; tr_resolve "<thread.threadId>"   # fixed
  tr_resolve_with_reply "$PR_NUM" "<thread.commentDatabaseId>" "<thread.threadId>" "$TR_MARKER_RULING" "$TEXT"   # no change
  ```

  A decision with no `thread` (an ambiguity the implementer raised, not a
  reviewer) has nothing to resolve — the applied comment records it alone.
  Then run **§3 on the new head** when a fix was pushed (a Ruling that
  changes code stales the evidence like any fix round; one that changes
  nothing leaves the request's own evidence standing, and §3 is skipped
  unless `--reason` is `incomplete`; on a parked PR — `$PARKED` non-empty,
  §0 — §3 is skipped either way and the evidence is
  `PR still parked ($PARKED): verification not re-run`), and only then post the applied
  comment with the re-run evidence — the lib removes `AFK:ruling` after the
  comment is up:

  ```bash
  jq -n '[ {n: 1, letter: "A", summary: "<what it settled>", sha: null,      thread: "<path:line>"},
           {n: 2, letter: "B", summary: "<what changed>",     sha: "<short sha>", thread: "<path:line>"} ]' > "$AUTO_AGENT_STATE_DIR/ruling-results-$PR_NUM.json"
  "$AA" ruling compose-applied "$AUTO_AGENT_STATE_DIR/ruling-results-$PR_NUM.json" --head "$(git rev-parse --short HEAD)" --ruling "<reply.ruling>" \
      --evidence "<the tail's evidence: 'CI green, manual verification <p>/<t>. Ready to merge.' — or the request's own when nothing re-ran>" \
      > "$AUTO_AGENT_STATE_DIR/ruling-applied-$PR_NUM.md"
  "$AA" ruling post-applied --pr "$PR_NUM" "$AUTO_AGENT_STATE_DIR/ruling-applied-$PR_NUM.md"   # posts, then --remove-label AFK:ruling
  ```

  On a `partial` reply the loop **re-posts the request for any decision left
  unanswered** — only those, renumbered from 1 — after the applied comment,
  on the new head and with the same evidence, so the PR is back to
  `AFK:ruling` holding exactly what is still open:

  ```bash
  "$AA" ruling remaining <(jq -c .request.decisions <<<"$PENDING") "$(jq -c .reply.answers <<<"$PENDING")" > "$DECISIONS"
  "$AA" ruling post --pr "$PR_NUM" --head "$(git rev-parse --short HEAD)" --evidence "<same evidence>" "$DECISIONS"   # re-applies AFK:ruling
  ```

  Report `pr-reconcile: RULING — applied <reply.ruling>` (full) or
  `pr-reconcile: RULING — applied <reply.ruling>, <k> decision(s) re-requested`
  (partial) and stop; the round loop below is not entered on a `ruling` Fire
  (the human's reply is the whole work).

**Decisions this Fire collects.** The round loop and the Arbiter step (3b)
produce **decisions** — the things only the human can settle: a
product ambiguity (the issue and Spec silent or contradicting each other on
the point, and the options differ in behaviour a user of the Target Project
would see) surfaced by the reviewer's findings or by the implementer, and a
dispute on a **human-authored** thread, carried with the Arbiter's (or the
implementer's) recommendation. Append each as one decision object to
`$DECISIONS` (a JSON array in `lib/ruling.sh`'s shape: `title`, a one-line
`scenario`, `now`, `wants`, `why`, the `thread` it came from when one did, and
`options` with `letter`, `text`, `cost`, `recommended` on exactly one, and
`fix: true` on any that changes code). A collected decision's thread stays
open and unreplied; it is never resolved, never disputed again, never parked.

Round loop (`R` starts at 1, cap `REVISE_ROUNDS_MAX`):

1. **Spawn one implementer per round** (blocking,
   `subagent_type: auto-agent:implementer`) covering ALL currently-unresolved
   threads. Prompt embeds: the issue title + body, the current PR diff
   (`git diff "origin/$BASE...HEAD"`, capped at 2000 lines as in
   `/auto-agent:pr-watch` §3), and every thread **whole** — `threadId`,
   `path:line`, `authored`, the first comment body, every reply in order
   (each tagged `agent` or `human` from its `agent` flag), and the `ruling`
   line (`ruling: <text>` or `ruling: none`) — plus these instructions
   verbatim:

   > Address each review comment by changing the shipped code accordingly. Stage
   > the changes (`git add`). Do NOT commit and do NOT push — the wrapper
   > handles that. Reply with one line per thread:
   > `<threadId>: <what you changed>` — or, if you believe a comment is wrong or
   > must not be applied, `<threadId>: revise-dispute — <one-line reason>` and
   > stage nothing for it. A thread whose `ruling` is set has already been
   > decided by the human in that reply: apply the ruling, or — when it asks
   > for no code change — answer `<threadId>: no-change — <what the ruling
   > settled>` and stage nothing for it. You may NOT dispute a thread with a
   > ruling. A thread carrying an Arbiter `fix` verdict has likewise been
   > ruled: apply the instruction as given. You may NOT dispute it; if you
   > cannot apply it, reply `<threadId>: cannot — <one-line reason>` and stage
   > nothing for it.

   A thread with a human `ruling` is **never carried to a dispute**: the
   implementer applies it or answers `no-change`, and the thread is resolved
   either way. A `revise-dispute` on a human-ruled thread is a malformed reply —
   treat it as `no-change` with the ruling as the recorded reason, never as a
   dispute.

   Read the reply with the library, never by eye — it is what decides what a
   line means this round:

   ```bash
   ACTIONS=$(av_parse_replies "$ROUND_THREADS" "$IMPLEMENTER_REPLY" "$R")
   # [ {threadId, action: "fixed"|"no-change"|"dispute"|"cannot"|"unaddressed", text}, ... ]
   ```

   `ROUND_THREADS` is the threads JSON this round's brief was built from; a
   thread the Arbiter ruled carries `ruled: "fix"` and its instruction, and a
   thread with a human `ruling` carries it as `tr_unresolved_threads` returned
   it (the library reads a `no-change` line, and a `revise-dispute` on a
   human-ruled thread, as `no-change`). From round 2 on, a thread in the
   brief is either one the Arbiter ruled `fix`, embedded with its verdict
   line (`arbiter: fix — <instruction>`) in place of a bare thread — the
   instruction is the brief — or one an earlier round left unaddressed. The Arbiter has already run by then, so there is nothing left
   to dispute to: **from round 2 on, a `revise-dispute` line on any thread,
   ruled or not, is refused and read as `cannot`**, never as a dispute — and
   in any round a `revise-dispute` on a ruled thread is a malformed reply,
   refused and **treated as `cannot`**. An implementer that cannot apply a
   binding fix has found a Spec contradiction by definition, and the thread
   becomes an **ambiguity decision** carrying the Arbiter's instruction as the
   recommendation (step 3b's `ambiguity` handling); a `cannot` on an unruled
   round-2+ thread is an ambiguity decision carrying the implementer's reason
   as the question. Bot-vs-bot ping-pong cannot restart, and no round-2+
   dispute is left both unruled and uncollected.

2. **Commit + push** (append-only; the rebase already happened, so plain push).
   Skip the commit when the round staged nothing (every thread answered
   `no-change`, disputed, or `cannot`); the steps below still run:

   ```bash
   git commit -m "fix(review): round $R — address review comments on PR #$PR_NUM"
   git push origin "$BRANCH"                # plain push — never force here
   SHA=$(git rev-parse --short HEAD)
   ```

3. **Reply + resolve per addressed thread** (`action: fixed`, or `no-change`
   under a human ruling) — the reply lands in-thread on the reviewer's
   comment, carries the loop's marker (the 4-arg `tr_reply` takes
   `<marker> <text>`, in that order, and puts the marker on the reply's
   first line), and states concretely what changed. A thread is resolved
   only with a reply recording a commit, an Arbiter dismissal, or a Ruling
   applied — the marker on the reply is which:

   ```bash
   # an unruled thread fixed this round: the reply records the commit
   tr_reply "$PR_NUM" "<commentDatabaseId>" "$TR_MARKER_FIX" "fixed in $SHA: <implementer's one-line summary for this thread>"
   tr_resolve "<threadId>"
   # a ruled thread the implementer applied with a commit: the reply records
   # the Ruling applied AND the commit, under the ruling marker
   tr_reply "$PR_NUM" "<commentDatabaseId>" "$TR_MARKER_RULING" "ruling applied in $SHA: <what the ruling asked and what changed>"
   tr_resolve "<threadId>"
   # a ruled thread answered no-change: the Ruling is applied with no commit
   tr_resolve_with_reply "$PR_NUM" "<commentDatabaseId>" "<threadId>" "$TR_MARKER_RULING" "ruling applied: no change — <what the ruling settled>"
   ```

   `tr_reply` takes `<marker> <text>` in that order, the same order
   `tr_resolve_with_reply` takes them; the marker is required and must be one
   of the lib's `$TR_MARKER_*` (the lib refuses anything else, so a reply can
   never go out unmarked). `tr_resolve_with_reply` posts the marked reply and
   resolves the thread in one call; it is the only way to resolve a thread
   without a commit (a `no-change` under a human ruling here; an Arbiter
   dismissal uses it with `$TR_MARKER_ARBITER`, step 3b). A thread is resolved
   only with **a reply recording a commit, an Arbiter dismissal, or a Ruling
   applied** — never silently, never on a dispute. Disputed / unaddressed
   threads are NOT replied to or resolved by this step: after round 1 they go
   to the Arbiter (3b), never to the next implementer round as-is.

3b. **The Arbiter step** — runs **after the implementer's first round**
   (`R == 1`), **at most once per Fire**, over **every** disputed thread at
   once — bot-authored threads to be ruled, human-authored ones (marked
   `authored: human`) for a recommendation only; **no dispute parks the PR**.
   Skip it when round 1 disputed nothing. Spawn one **`auto-agent:arbiter`** (blocking,
   `subagent_type: auto-agent:arbiter`, never pass `model:`). The prompt
   embeds, per disputed thread: the Finding (the thread's first comment
   verbatim, with `threadId`, `path:line` and `authored`), the implementer's
   `revise-dispute` line verbatim, and once: the issue title + body with its
   Acceptance Criteria, the parent Spec body when the issue names one, and the
   PR diff (`git diff "origin/$BASE...HEAD"`, capped as in step 1). The
   checkout is at the PR head already. The Arbiter **never sees the
   implementer's transcript** or this session's conversation — only the
   dispute line. It applies the escalation test written in its own prompt and
   returns one line per thread; read them with the library and apply each
   verdict:

   ```bash
   VERDICTS=$(av_parse_verdicts "$DISPUTED_THREADS" "$ARBITER_REPLY")
   # [ {threadId, authored, verdict: "fix"|"dismiss"|"ambiguity"|"unruled", text, reason}, ... ]
   ```

   - `<threadId>: fix — <instruction>` → **binding**. The thread is round 2's
     work: carry it into the next implementer round with the verdict line as
     its brief (step 1). It **may not be disputed**; an implementer `cannot`
     (or a refused dispute) turns it into an ambiguity decision with the
     Arbiter's instruction as the recommendation.
   - `<threadId>: dismiss — <reason>` → reply **`arbiter: dismissed — <reason>`**
     under the Arbiter's marker and resolve the thread **with no commit**:

     ```bash
     tr_resolve_with_reply "$PR_NUM" "<commentDatabaseId>" "<threadId>" "$TR_MARKER_ARBITER" "arbiter: dismissed — <reason>"
     ```

     A dismissal consumes no round of the cap.
   - `<threadId>: ambiguity — <decision JSON>` → **collected for the Ruling
     request**: append the decision object to `$DECISIONS` (the lib's shape,
     above). The thread stays open and unreplied; it reaches the human in
     the Ruling request at the end of §2 (below), never as a park.
   - a human-authored thread's dispute → the Arbiter always returns
     `ambiguity` for it (its prompt forbids `fix` or `dismiss` on one), with
     its recommendation in the decision JSON; collect it like any other
     decision. **Never** dismiss or resolve it here.

   A malformed reply (a `threadId` missing or doubled, a verdict word outside
   the three, or a `fix` / `dismiss` on a human-authored thread) leaves that
   thread **unruled** (`av_parse_verdicts` says why); an unruled thread is
   carried like an ambiguity (collected, never dismissed), and the Arbiter is
   not re-spawned this Fire.

   **The ruled round is guaranteed.** The next round number comes from the
   library, never from `R < REVISE_ROUNDS_MAX` alone:

   ```bash
   NEXT=$(av_next_round "$R" "$REVISE_ROUNDS_MAX" "<count of fix verdicts>")   # a round number, or "cap"
   ```

   After round 1 with at least one `fix` verdict, `NEXT` is 2 even under
   `REVISE_ROUNDS_MAX=1`: a cap of 1 means one unruled round, not zero ruled
   ones, and a binding instruction the Daemon never attempted is not a
   "fix still failing". That ruled round is then the Fire's last — the
   guarantee is one round, not a new cap. Only `fix` verdicts are an input;
   the Arbiter run and its dismissals are not.

   **The PR body is a remedy.** A Finding about the PR description — a
   call-out, a claim the diff no longer supports, a missing note — is the
   Daemon's own text: this loop (or the implementer, through the `fix`
   instruction) **may edit the PR body** to resolve the thread, with a
   `$TR_MARKER_FIX` reply naming the edit in place of a commit sha. The
   **issue body and the Acceptance Criteria are not** a remedy and stay
   untouchable.

4. Re-enumerate. All threads resolved → **§2-exit**. `NEXT` is a round
   number and threads remain outside `$DECISIONS` → next round. Otherwise
   (`NEXT` is `cap`, or every remaining open thread is collected in
   `$DECISIONS`) → one of two exits, told apart by **what** is left open:

   - **Fixes still failing at the cap** (a thread the implementer tried —
     Arbiter-ordered included — and the fix did not land) → **escalate**.
     `AFK:revise-failed` is applied only when fixes still fail at the round
     cap — never for a decision that awaits the human, and never for a
     bot-thread dispute the Arbiter did not rule (a malformed reply, a
     failed spawn): that dispute is not a failing fix, it is appended to
     `$DECISIONS` and carried like an ambiguity (edge cases below):

     ```bash
     # One marked reply per still-failing thread, then park the PR for a human.
     # The escalate marker records no commit and resolves nothing; it only
     # makes the human's answer to it the thread's `ruling` next round:
     tr_reply "$PR_NUM" "<commentDatabaseId>" "$TR_MARKER_ESCALATE" "pr-reconcile: fix still failing after $REVISE_ROUNDS_MAX round(s) — parked for a human."
     gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:revise-failed --remove-label AFK:revise
     gh pr comment "$PR_NUM" --repo "$REPO" --body "pr-reconcile: <k> review thread(s) still failing after $REVISE_ROUNDS_MAX round(s) at $(date -Iseconds). Labeled AFK:revise-failed: fixes still failing at the round cap; re-apply AFK:revise to retry."
     ```

     Report `pr-reconcile: REVISE-FAILED — <k> thread(s) still failing` and
     stop (skip §3 — the PR is parked; verification runs after the human
     weighs in).

   - **Only collected decisions remain** (every fix landed; what is open is
     in `$DECISIONS`) → the **Ruling request** exit. It is not a park: drop
     `AFK:revise` as at §2-exit, then the Fire **runs the verification tail
     on the fixed head anyway** (§3, on everything the rounds pushed) and
     posts the request **after** the tail, so the footer states that
     evidence and the all-recommended reply can leave the PR ready to merge
     with no further Fire:

     ```bash
     gh pr edit "$PR_NUM" --repo "$REPO" --remove-label AFK:revise
     # … §3 runs here: pr-watch, the marker-gated review, the verification round …
     SUPERSEDES=()
     if [ -s "$AUTO_AGENT_STATE_DIR/ruling-open-$PR_NUM.json" ]; then      # Step 0 found a request still open:
       jq -sc 'add' "$AUTO_AGENT_STATE_DIR/ruling-open-$PR_NUM.json" "$DECISIONS" > "$DECISIONS.merged" && mv "$DECISIONS.merged" "$DECISIONS"
       SUPERSEDES=(--supersedes "$OPEN_REQUEST_ID")                        # one request holding both; the earlier one is superseded
     fi
     "$AA" ruling post --pr "$PR_NUM" --head "$(git rev-parse --short HEAD)" \
         --evidence "CI green, manual verification <p>/<t>" "${SUPERSEDES[@]}" "$DECISIONS"     # posts, then --add-label AFK:ruling
     ```

     The evidence string is the tail's own result (`pr-watch: PASS` and the
     round's `manual-verify:` counts; on a DRAFT/ERROR tail, say so:
     `CI red (pr-watch: DRAFT), manual verification not run` — the request
     still goes out, because the decision is still the human's). The lib
     composes the comment (marker
     `<!-- auto-agent:ruling-request head=<sha> decisions=<n> -->`, one
     section per decision, the lettered option table, the footer) and
     applies `AFK:ruling` only after the comment is up; never hand-roll the
     Ruling request, and never apply `AFK:revise-failed` on this exit.
     Report `pr-reconcile: RULING — <n> decision(s) requested` with the
     tail's lines in the block and stop. The same exit applies **before**
     the cap: when a round ends with every remaining open thread collected
     in `$DECISIONS`, there is nothing left for another round.

**§2-exit** (all threads addressed, `$DECISIONS` empty):

```bash
gh pr edit "$PR_NUM" --repo "$REPO" --remove-label AFK:revise
```

The label drop is what stops the Daemon re-picking this PR next Fire; the human
re-applies `AFK:revise` (and re-opens threads) if a fix missed — or simply
replies in the thread under the loop's marked reply: the next round reads
that reply as the thread's `ruling`.

### 3. Verification tail (when §1/§2 pushed anything — or `--reason incomplete` — a Ruling request outstanding or not)

Any push (rebase, comment fix, or a Ruling applied with a fix) re-ran CI and
staled ALL previous evidence — per the locked design, **all verification
re-runs**. A Ruling request about to be posted, or already outstanding, never
skips the tail: the request is posted after the tail with the tail's
evidence, and a Fire that applies a Ruling with a fix re-runs the tail before
its applied comment. Execute `/auto-agent:afk-pickup`'s success tail against
this PR, with one modification:

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
Harness install does not ship it) — that is a **missing round**, and it must
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
reason:    revise | conflict | both | incomplete | ruling
rebase:    CLEAN — pushed | SKIPPED | FAILED — <detail>
comments:  <k> thread(s) addressed in <R> round(s) | SKIPPED | FAILED — <n> still failing
arbiter:   <d> ruled — <f> fix, <m> dismissed, <a> ambiguity | SKIPPED — no dispute   (when §2 ran)
ruling:    <n> decision(s) requested (comment <id>) | applied <ruling> [, <k> re-requested] | invalid reply, nudged | already nudged | awaiting the human | none
pr-watch:  <verbatim terminal line>            (when §3 ran)
verify:    <verbatim manual-verify line> — post-reconcile   (when §3 ran and pr-watch PASS)
           | SKIPPED — Bootstrap state, AFK:verify-human applied   (no hermetic tier)
           | MISSING — <reason>                             (§3 park: round never ran)
result:    PASS | REBASE-FAILED | REVISE-FAILED | RULING | DRAFT | ERROR — <detail>
```

The final `result:` line doubles as the terminal verdict the caller parses:

- `pr-reconcile: PASS — rebased and/or <k> comment(s) addressed, checks green, manual verify clean`
- `pr-reconcile: REBASE-FAILED — <reason>`
- `pr-reconcile: REVISE-FAILED — <k> thread(s) still failing`
- `pr-reconcile: RULING — <n> decision(s) requested` | `— applied <ruling>[, <k> decision(s) re-requested]` | `— invalid reply, nudged` | `— awaiting the human (<n> decision(s))`
- `pr-reconcile: DRAFT — verification tail exhausted, marked draft, AFK:checks-failed`
- `pr-reconcile: ERROR — <reason>`

A `RULING — <n> decision(s) requested` Fire ran the tail (its `pr-watch:` and
`verify:` lines are in the block, exactly as a PASS Fire's) and left the PR
`AFK:ruling`, not parked; a `RULING — applied …` Fire with a fix ran the tail
too. The `verify:` validity rule below applies to both whenever §3 ran.

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
- **Implementer disputes a bot-authored review comment** — the Arbiter rules
  it in the same Fire (step 3b): `fix` is round 2's binding work, `dismiss`
  resolves the thread with a marked reply and no commit, `ambiguity` is
  collected for the human. A dispute never parks the PR by itself.
- **Implementer disputes a human-authored review comment** — the loop never
  argues with a human's review by force and never dismisses their thread; the
  dispute is collected as a decision with the Arbiter's recommendation and
  reaches the human in the Ruling request, never as a park. A thread the human has already ruled in-thread cannot be disputed at all: the
  ruling is applied or answered `no-change`, and the thread is resolved.
- **Implementer disputes an Arbiter `fix`**, or disputes anything from round
  2 on — refused: the reply is treated as `cannot`, the thread becomes an
  ambiguity decision (with the Arbiter's instruction as the recommendation
  when there is one), and no further implementer round argues it.
- **`REVISE_ROUNDS_MAX` is 1 and the Arbiter rules `fix`** — the ruled round
  still runs (`av_next_round` returns 2); the Fire parks only if that round
  leaves the fix failing.
- **Arbiter reply is malformed or the spawn fails** — the disputed threads are
  left unruled and carried like ambiguities (appended to `$DECISIONS` with
  the implementer's reason as the **Now** line and no recommendation, never
  dismissed on a guess, never a reason for `AFK:revise-failed`); the
  Arbiter runs at most once per Fire, so no retry this Fire.
- **A product ambiguity surfaces mid-round** — never a fix, never a park: it
  is appended to `$DECISIONS`, the tail runs on the fixed head, and the
  request goes out with the evidence. The all-recommended reply then needs
  no further Fire when every recommended option is a no-change.
- **The human's reply is not a Ruling** (`I agree, resolve it`, `1Z`) — one
  marked nudge saying what was expected, nothing applied, nothing relabelled;
  the nudge is one per request (`reply.nudged` is true once any nudge
  followed the request), so a second non-Ruling gets no second nudge, and
  PR Triage stops picking the PR for it the moment the nudge is up. A
  reply naming only some decisions applies those and re-posts the request
  for the rest.
- **Two requests on one PR** — never left that way: `ruling pending` reads
  only the latest request not yet followed by an applied comment, so a
  request posted before this Fire's rounds (pickup §6a.4 on the
  `AFK:revise applied` exit) is kept aside at Step 0 and, when the rounds
  collect a decision of their own, folded into the one request the Ruling
  exit posts (`--supersedes <id>`; its decisions keep their numbers, the
  new ones follow). A partial reply's re-post follows the applied comment.
  A reply the human posted to the earlier request while this Fire ran lands
  before the superseding one and is not read; the superseding request says
  so and asks for one reply to itself.
- **The human answers a request over several comments** (`1A`, then `2B`) —
  `ruling pending` folds every human comment after the request into one
  answer set, a later letter replacing an earlier one for the same decision;
  the Fire applies the union and re-posts only what is still unnamed.
- **The human answered in-thread but the loop parked anyway** — cannot happen:
  `tr_unresolved_threads` reads every reply, and a human reply after the
  loop's last marked reply is that thread's `ruling`, which the next round
  applies or acknowledges with `no-change` and resolves.
- **`AFK:revise` applied but no unresolved threads** — drop the label; there is
  nothing machine-actionable. The human should leave inline review comments (not
  just a top-level comment) to hand work back.
- **PR turns draft / closes mid-reconcile** — stop at the next step boundary,
  report `pr-reconcile: ERROR — pr no longer open`, touch nothing further.
- **The human answered the Ruling request on a draft or parked PR** — PR
  Triage picks it for reason `ruling` (the reply is the whole trigger; the
  human relabels nothing) and §0 lets the draft through for that reason
  alone. Apply the Ruling, post the applied comment, and leave the park as
  it is: no rebase, no tail, no `gh pr ready`, no park label removed. The
  failure that parked the PR is still the human's to repair. A reply there
  that is not a Ruling is picked the same way, once, for its nudge alone.
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
- Never edits the issue body or the Acceptance Criteria, nor a reviewer's
  comments. Replies are additive. The PR body is the Daemon's own text and may
  be edited to resolve a thread about it.
- A thread is resolved only with a reply recording a commit, an Arbiter
  dismissal, or a Ruling applied — each reply carrying its hidden marker
  (`$TR_MARKER_FIX` / `$TR_MARKER_ARBITER` / `$TR_MARKER_RULING`) so the next
  round can tell the loop's voice from the human's. The escalation reply
  carries `$TR_MARKER_ESCALATE` and resolves nothing. Never posts an unmarked
  reply, never resolves a thread silently, never resolves a disputed thread,
  and never resolves a human-authored thread on the Arbiter's word.
- Never hand-roll the Ruling request, its applied comment, its nudge or its
  grammar: `"$AA" ruling …` (`lib/ruling.sh`) composes, posts, parses and
  labels; this skill supplies decisions and reads verdicts. Never applies
  `AFK:revise-failed` for a decision that awaits the human, never answers a
  Ruling request itself, and never applies a letter the human did not give.
- Never disputes a thread that carries a human `ruling`; never decides
  authorship by login or by parsing a reply's visible text — the marker on
  the first line is the only signal.
- Spawns the Arbiter at most once per Fire, after round 1 only, and never
  passes it the implementer's transcript; never dismisses a bot thread without
  its verdict and never decides a dispute itself.
- Counts only implementer rounds against `REVISE_ROUNDS_MAX`; an Arbiter run
  or a dismissal never consumes one, and the round applying the Arbiter's
  `fix` verdicts is never skipped for the cap.
- Never operates on a PR whose head is not `feat/issue-<N>` (§0 enforces).
- Never touches the `AFK:in-progress`/`AFK:done` lock — the caller owns it.
- Never names a repo, a branch or a cap as a literal — every one comes from
  the Harness config read in "Harness context".
- One PR per Fire; the Daemon's budget gate paces successive Fires.
