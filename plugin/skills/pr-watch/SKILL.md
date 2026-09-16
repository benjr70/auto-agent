---
name: pr-watch
description:
  Watch a freshly opened PR's CI checks, auto-fix failures by spawning the
  implementer in a bounded loop, and either land green or mark the PR draft on
  exhaustion. Invoked (blocking) by `/auto-agent:afk-pickup` §6a.1 immediately
  after PR creation and by `/auto-agent:pr-reconcile`'s verification tail.
  Takes the PR number + branch + issue number as arguments; the repo and every
  cap come from the Harness config. `--bot` switches it to the Dependabot
  lane's budget, label, fix brief and commit trailer.
---

# PR Watch — Autonomous CI Babysitter + Fix Loop

You are the **CI watcher** spawned by `/auto-agent:afk-pickup` after a PR
opens. One fire = one PR. You poll checks, dispatch fixes when checks fail, and
return a single terminal verdict line that the caller pastes into its output
block.

afk-pickup may invoke you **more than once on the same PR** — once per manual
verification round (§6a.3), after each `fix(manual)` push re-runs CI. Your
default-mode fix cap is **per invocation**; each call starts a fresh budget and
just watches the PR's current head to green.

## Harness context

Every repo fact and every cap comes from the Harness config (ADR 0002). Read it
once, first thing, and never spell a repo, a branch name or a round count as a
literal:

```bash
AA="${AUTO_AGENT_ROOT:?the Fire wrapper exports AUTO_AGENT_ROOT}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")              # the Target Project, from its origin remote
BASE=$(jq -r .repo.default_branch <<<"$CFG")    # detected from GitHub, never declared

MAX_ROUNDS=$(jq -r .rounds.pr_watch <<<"$CFG")                     # default-mode fix cap, per invocation
DEPS_CAP=$(jq -r .rounds.deps_fix <<<"$CFG")                       # bot-mode cap, shared across fires
LOCKFILE_CMD=$(jq -r '.commands.lockfile_refresh // empty' <<<"$CFG")  # bot mode: the lockfile recipe
```

`$MAX_ROUNDS` is the number the default-mode DRAFT verdict names; `$DEPS_CAP`
is the number the bot-mode DRAFT verdict names. Neither is ever hard-coded
here.

## Two modes

|                  | default (agent PR)                | `--bot` (Dependabot PR)                          |
| ---------------- | --------------------------------- | ------------------------------------------------ |
| caller           | `/auto-agent:afk-pickup` §6a.1    | the `/auto-agent:deps-land` lane (Slice #36)     |
| branch           | `feat/issue-<N>`                  | `dependabot/…`                                   |
| backing issue    | required                          | none — `--issue` may be `none`                   |
| fix budget       | `rounds.pr_watch`, fresh per fire | `rounds.deps_fix` **total**, accumulated via markers |
| exhaustion label | `AFK:checks-failed`               | `AFK:deps-failed`                                |
| exhaustion note  | comment on the issue              | comment on the PR                                |
| fix commits      | plain `fix(ci):` message          | message ends `[dependabot skip]`                 |
| per-round record | none                              | one `fix-attempt` marker comment                 |

**The default path is unchanged by bot mode.** Without `--bot`, every step,
every label and every verdict line below is exactly what it has always been; bot
behaviour only ever appears in an explicitly marked _(bot mode)_ branch.

This skill assumes:

- The PR is already open on `feat/issue-<N>` — or, in bot mode, on
  `dependabot/…` — against the default branch (`$BASE`).
- The plugin's implementer agent is callable via the `Agent` tool with
  `subagent_type: auto-agent:implementer`.
- The repo's `AFK:checks-failed` label is created by `/auto-agent:afk-dispatch`
  §0, and its `AFK:deps-failed` label by the deps-land lane.
- `lib/deps-lane.sh` in the Harness install is sourceable (bot mode only — it
  owns the cap arithmetic and the markers).

## Invocation

```
/auto-agent:pr-watch --pr <PR_NUM> --branch <BRANCH> --issue <ISSUE_N> [--bot]
```

The first three arguments are required. No defaults — the caller supplies them
verbatim from the PR-create step. There is no `--repo` argument: the repo is
`$REPO` from the Harness config.

`--bot` (optional) says: **this is a Dependabot PR**. The caller is the
deps-land lane, the branch is `dependabot/…`, and there is no backing issue —
`--issue` may be omitted or passed as `none`, and the exhaustion comment goes on
the PR instead of on an issue.

## Process

### 0. Pre-flight

```bash
gh auth status >/dev/null || { echo "pr-watch: ERROR — gh not authenticated"; exit 1; }
gh pr view "$PR_NUM" --repo "$REPO" --json number,headRefName,state \
  | jq -e --arg br "$BRANCH" '.headRefName == $br and .state == "OPEN"' >/dev/null \
  || { echo "pr-watch: ERROR — PR #$PR_NUM not open on $BRANCH"; exit 1; }
```

If the PR is already closed/merged, exit `pr-watch: ERROR — pr not open`.

_(bot mode)_ The branch guard accepts `dependabot/…` as well as
`feat/issue-<N>`; nothing else, in either mode. The guard exists to stop a
hand-crafted PR being driven by this loop, and Dependabot's branch prefix is the
second — and only other — shape the autonomous system creates.

### 1. Round loop (max `$MAX_ROUNDS`)

```
ROUND=0
# MAX_ROUNDS already read from rounds.pr_watch in the Harness context
```

_(bot mode)_ The budget is not per-fire but **`$DEPS_CAP` in total across every
fire on this bump**, so read what previous fires already spent before doing
anything else:

```bash
. "$AUTO_AGENT_ROOT/lib/deps-lane.sh"
HEAD_SHA=$(gh pr view "$PR_NUM" --repo "$REPO" --json headRefOid -q .headRefOid)

# Read the comments into a variable FIRST and check that read's own status.
# Never `gh api … | deps_lane_marker_parse …` as one pipeline: a pipeline
# reports only its LAST command's status, and marker-parse given a valid sha
# and empty stdin exits 0 printing `{"fixAttempts":0,…}`. A rate-limited,
# unauthenticated or offline `gh api` would then read as "zero attempts spent"
# and hand the least trustworthy PR a fresh full budget — the exact
# grind-forever case this section exists to prevent.
COMMENTS=$(gh api "repos/$REPO/issues/$PR_NUM/comments" --paginate --jq '.[].body') \
  || COMMENTS="__unreadable__"

if [ -z "$HEAD_SHA" ] || [ "$COMMENTS" = "__unreadable__" ]; then
  MAX_ROUNDS=""
else
  MARKERS=$(deps_lane_marker_parse "$HEAD_SHA" <<<"$COMMENTS") || MARKERS=""
  ATTEMPTS=$(printf '%s' "$MARKERS" | jq -r '.fixAttempts // empty')
  MAX_ROUNDS=$(deps_lane_rounds_left "$ATTEMPTS") || MAX_ROUNDS=""
fi
```

**An unreadable history is an ERROR, never a full budget.** If the head sha or
the comment read failed, or `deps_lane_marker_parse` exits non-zero (no sha,
missing `jq`, no Harness config), or `deps_lane_rounds_left` prints nothing —
`MAX_ROUNDS` empty — stop immediately:

```bash
if [ -z "$MAX_ROUNDS" ]; then
  echo "pr-watch: ERROR — bot marker history unreadable"
  exit 1
fi
```

Do **not** fall back to `$DEPS_CAP`, and do not treat the empty read as 0
attempts spent. A PR whose past attempts cannot be counted is exactly the PR
that must not be handed the largest possible fix budget: that is how a bump the
lane already failed `$DEPS_CAP` times gets ground on forever. Erroring out
costs one fire and leaves the PR untouched for the next one; guessing costs the
cap.

If `MAX_ROUNDS` is 0, go **straight to the exhaustion path (§6) without polling
CI at all** and return the bot DRAFT verdict. The cap's attempts are already on
the record; paying a full CI wait to re-learn a decision already made is pure
budget burn.

**Marker keying.** Markers are keyed to the **PR head sha**, the one marker
vocabulary the whole lane shares: `deps_lane_marker_emit` keys to "this exact
head sha", and `lib/pr-triage.sh`'s Dependabot enrichment parses markers
against the current `headRefOid`. Read the count against the head at
invocation start; write each round's marker against the head the push just
created (§5), so the next fire — which sees that push as the head — reads the
accumulated count back. Tier B (`/auto-agent:verify-pr`) markers are written
against the same key, which is what makes the cap "`$DEPS_CAP` attempts total
across both tiers and across fires" rather than `$DEPS_CAP` per tier.

Our own pushes move the head, so each round re-stamps the accumulated count onto
the new head (§5); the count therefore resets only when the head moves without
our markers following it — a Dependabot rebase or a new version push replaces
the branch tip, and the fresh bump correctly starts with a full budget. Keying
instead to the newest _bot-authored_ commit would survive our pushes without
re-stamping, but it would key on a sha no other lane component parses: triage
would report `fixAttempts: 0` and Tier B markers would never combine with this
loop's count.

Each round:

1. **Poll CI** (§2)
2. If green → return `pr-watch: PASS — all checks green at attempt $ROUND` and
   exit 0.
3. If red → **gather failure context** (§3), **spawn implementer** (§4),
   **commit + push** (§5), increment `ROUND`, loop.
4. If `ROUND == MAX_ROUNDS` and still red → **draft-on-exhaust** (§6) and return
   `pr-watch: DRAFT — exhausted $MAX_ROUNDS rounds, marked draft, AFK:checks-failed`.

_(bot mode)_ Same loop, with `MAX_ROUNDS` from the cap read above; the green
verdict is `pr-watch: PASS — all checks green at attempt <K> (bot)` and the
exhausted verdict is
`pr-watch: DRAFT — exhausted $DEPS_CAP attempts, marked draft, AFK:deps-failed`
— the count named is the cap, not this fire's rounds, because the budget is
shared across fires.

### 2. Wait for CI to settle (zero turns while it runs)

Run the consolidated waiter **in the background** — one Bash call with
`run_in_background: true`. It polls on its own clock (60s interval, 45-min cap),
and the harness re-invokes you exactly once when it exits. Do **not** poll
`gh pr checks` yourself between rounds, and do not run the waiter in the
foreground (the Bash tool's 10-min ceiling would force re-invocations — the
exact turn burn this script eliminates). The repo comes from the Harness config;
there is no `--repo` flag.

```bash
# run_in_background: true
"$AA" ci-wait --pr "$PR_NUM"
```

When it completes, read its output. **Line 1 is the JSON verdict**; on a red
settle the §3 failure-log bundle follows it in the same output:

- exit 0 / `"result":"green"` → success branch in §1
  (`pr-watch: round $ROUND — all green`)
- exit 1 / `"result":"fail"` → fix branch in §1
  (`pr-watch: round $ROUND — <failed count> failed check(s), proceeding to fix`)
- exit 2 / `"result":"timeout"` →
  `pr-watch: ERROR — polling timeout (45min) at round $ROUND`, exit 1
- exit 3 / `"result":"error"` → checks unreadable 3 polls straight, or no
  Harness config — re-check `gh auth status` and the PR state before deciding
  anything

`bucket == "skipping"` is benign and already ignored by the script. Only `fail`
counts as red.

### 3. Gather failure context

For the fix-loop, the implementer needs:

1. **Issue body** — `gh issue view $ISSUE_N --repo $REPO --json title,body` —
   _(bot mode)_ there is no issue; use the PR's own title and body instead
   (`gh pr view $PR_NUM --repo $REPO --json title,body`), which carry
   Dependabot's release notes, changelog and commit list.
2. **PR diff** — `gh pr diff $PR_NUM --repo $REPO` (capped at 2000 lines; if
   longer, truncate with a `... [truncated]` marker)
3. **Failed job logs** — already in hand: `ci-wait` printed the last 200 lines
   of each failed job as `=== <job name> ===` sections right after its JSON
   verdict line. Do **not** re-fetch logs with `gh run view` — reuse that bundle
   verbatim.

Fetch 1 and 2 in a single Bash call, then bundle all three into a single context
blob the implementer prompt embeds verbatim.

### 4. Spawn implementer

Use the `Agent` tool. Subagent is the plugin's `implementer` definition
(allowlist Edit/Write/Bash/Read/Grep/Glob). Do not pass a `model`: the Fire's
model policy carries through.

- `subagent_type: auto-agent:implementer`
- `run_in_background: false` ← blocking; we need the fix before next poll
- `prompt`:

  ```
  You are fixing failing CI checks on PR #<PR_NUM> (branch <BRANCH>) for
  issue #<ISSUE_N> in <REPO>.

  ## Original issue
  <issue title + body>

  ## Current PR diff
  <pr diff or truncated tail>

  ## Failing job logs (tail 200 lines per job)
  <log bundle>

  Fix the failures. Stage the fix. Do NOT commit and do NOT push — the
  wrapper handles that. Reply only when staged changes are ready, with a
  short summary of what you changed. If the failure looks like flake/infra
  (no code change warranted), reply with: `pr-watch-flake: <one-line reason>`
  and stage nothing.
  ```

_(bot mode)_ The prompt **leads** with the dependency-bump context, before the
diff and the logs, because it changes what a correct fix looks like. When
`$LOCKFILE_CMD` is non-empty:

```
This branch is a Dependabot DEPENDENCY BUMP on PR #<PR_NUM> in <REPO>.
There is no backing issue.

Before any other fix, regenerate the lockfile with this project's configured
lockfile refresh command, exactly as written:

    <LOCKFILE_CMD>

It is the Target Project's own recipe (its Harness config's
`commands.lockfile_refresh`); a lockfile regenerated any other way will fail CI
differently. Most bot-PR CI failures are a stale or partially-resolved lockfile
and need nothing else.

Only if the lockfile is already correct should you touch application code,
and then minimally: adapt our code to the new dependency version. Do NOT
change the dependency's version range to dodge the failure — that reverts
the bump this PR exists to make.

## PR title + body (Dependabot's release notes)
<pr title + body>
```

When `$LOCKFILE_CMD` is empty, replace the lockfile paragraph with: "No lockfile
refresh command is configured for this project; fix the code minimally — adapt
our code to the new dependency version and nothing else." The rule against
changing the version range stands either way.

The usual PR diff and failing-job-log sections follow verbatim, and the same
closing instructions apply (stage the fix, do not commit, do not push, or reply
`pr-watch-flake: <reason>`).

### 5. Commit + push (append, no force)

After the implementer returns:

```bash
if git diff --staged --quiet; then
  if echo "$IMPL_REPLY" | grep -q '^pr-watch-flake:'; then
    echo "pr-watch: round $ROUND — implementer flagged flake, re-polling without commit"
    # Loop back to §2 without bumping ROUND-as-fix; still counts toward cap.
  else
    echo "pr-watch: ERROR — implementer staged nothing and did not flag flake"
    exit 1
  fi
else
  git commit -m "fix(ci): pr-watch round $ROUND — auto-fix failing checks

$(echo "$IMPL_REPLY" | head -20)
"
  git push origin "$BRANCH"   # plain push, never --force
fi
```

_(bot mode)_ The message ends with the `[dependabot skip]` trailer line, and
the round is recorded as a marker after the push. (The deps-land Slice moves the
trailer into `lib/deps-lane.sh` as a helper; until then append the literal line
yourself, last, on its own line.)

```bash
MSG=$(printf 'fix(ci): pr-watch round %s — auto-fix failing checks\n\n%s\n\n[dependabot skip]\n' \
        "$ROUND" "$(echo "$IMPL_REPLY" | head -20)")
git commit -m "$MSG"
git push origin "$BRANCH"   # plain push, never --force

# One marker comment per fix round, keyed to the head sha the push just created
# — the same key triage and Tier B use (§1 "Marker keying"). marker-parse
# COUNTS markers for exactly that sha, and our push moved the sha, so the
# comment re-stamps the whole history onto the new head: one marker per attempt
# spent so far, earlier fires included. Anything less and the count silently
# restarts at 1 after every push.
HEAD_SHA=$(git rev-parse HEAD)
BODY="pr-watch bot fix attempt $((ATTEMPTS + ROUND)) of $DEPS_CAP on this bump."$'\n'
for i in $(seq 1 $((ATTEMPTS + ROUND))); do
  BODY="$BODY$(deps_lane_marker_emit fix-attempt "$HEAD_SHA" "$i")"$'\n'
done
gh pr comment "$PR_NUM" --repo "$REPO" --body "$BODY"
```

Both halves are load-bearing. Without the trailer, Dependabot treats the branch
as human-owned and stops rebasing it — the PR then rots behind the default
branch with no bot able to update it. Without exactly **one marker comment per
fix round**, carrying one marker per attempt spent, the next fire re-reads the
wrong budget: too few markers and the cap never trips, too many and a bump is
abandoned a round early.

Plain `git push` (no `--force`, no `--force-with-lease`). If push is rejected
because someone pushed concurrently to the branch, return
`pr-watch: ERROR — branch diverged, manual triage required` — one Daemon per
Target Project means this should never happen; if it does, abort.

### 6. Draft on exhaust

After `$MAX_ROUNDS` rounds without green:

```bash
gh pr ready "$PR_NUM" --repo "$REPO" --undo                # convert to draft
gh pr edit  "$PR_NUM" --repo "$REPO" --add-label AFK:checks-failed
gh issue comment "$ISSUE_N" --repo "$REPO" --body \
  "pr-watch exhausted $MAX_ROUNDS fix rounds on PR #$PR_NUM. Marked draft + labeled AFK:checks-failed. Human triage required."
```

Return: `pr-watch: DRAFT — exhausted $MAX_ROUNDS rounds, marked draft, AFK:checks-failed`

_(bot mode)_ Same three moves, against the PR — there is no issue to comment on:

```bash
# `|| true`: a re-fire may find the PR already drafted by an earlier
# exhaustion, and `gh pr ready --undo` fails on a PR that is already a draft.
# Failing there would abort before the label, the comment and the DRAFT verdict
# line the lane parses — so the already-drafted case must be a no-op, not a stop.
gh pr ready "$PR_NUM" --repo "$REPO" --undo || true         # convert to draft
gh pr edit  "$PR_NUM" --repo "$REPO" --add-label AFK:deps-failed
gh pr comment "$PR_NUM" --repo "$REPO" --body \
  "pr-watch exhausted $DEPS_CAP fix attempts on this bump. Marked draft + labeled AFK:deps-failed. Human triage required."
```

**Rule: never apply `AFK:checks-failed` to a Dependabot PR.** The deps-land lane
only ever looks for `AFK:deps-failed`; a bot PR wearing the agent-lane label is
invisible to both lanes and sits drafted and unowned until a human happens to
notice it.

Return: `pr-watch: DRAFT — exhausted $DEPS_CAP attempts, marked draft, AFK:deps-failed`

## Terminal verdict

Exactly one of these is the final line printed before exit:

Default mode:

- `pr-watch: PASS — all checks green at attempt <K>`
- `pr-watch: DRAFT — exhausted $MAX_ROUNDS rounds, marked draft, AFK:checks-failed`
- `pr-watch: ERROR — <reason>`

Bot mode (`--bot`) — the PASS and DRAFT lines name the mode and the shared cap;
the ERROR line is identical in both modes:

- `pr-watch: PASS — all checks green at attempt <K> (bot)`
- `pr-watch: DRAFT — exhausted $DEPS_CAP attempts, marked draft, AFK:deps-failed`
- `pr-watch: ERROR — <reason>`

`$MAX_ROUNDS` and `$DEPS_CAP` are printed as their numbers (e.g. `exhausted 10
rounds`, `exhausted 3 attempts`); the caller parses the line by its prefix and
label, never by the count. The afk-pickup caller parses this line verbatim into
its §7 output block; the deps-land lane parses the bot lines the same way.

These exact strings — both labels and all five verdict shapes — are asserted by
`lib/runbook-check.sh` in the Harness install, which runs in the harness test
suite. Editing a line here without editing that check is the failure it exists
to catch.

## Failure modes

- **PR closed/merged mid-watch** — exit `pr-watch: ERROR — pr not open` on the
  next poll. Do not attempt to push.
- **Branch diverged** (concurrent push) — see §5; should not happen with one
  Daemon per Target Project.
- **Implementer returns flake flag** — round still counts toward the cap.
  Re-poll without a new commit; if checks were truly transient they may green on
  retry.
- **All rounds pass implementer but checks stay red** — §6 fires; PR drafts.
- **Bot PR already at the cap** — §1 finds `MAX_ROUNDS` 0 and goes straight to
  §6 without polling CI; no implementer is spawned.
- **Bot marker history unreadable** — the head sha or the comment read failed;
  §1 exits `pr-watch: ERROR — bot marker history unreadable` before polling CI
  or spawning anyone. There is no fallback budget: a PR whose attempts cannot be
  counted never gets a fresh `$DEPS_CAP`.
- **`gh run view` rate-limited** — the log bundle comes from `ci-wait`, so this
  no longer applies to polling; if a re-fetch is ever needed, skip the bundle
  for that round — the implementer still gets issue + diff.

## Boundaries

- Never force-pushes. Never rewrites history. Append-only fix commits. (The sole
  sanctioned force-push in the whole autonomous system is
  `/auto-agent:pr-reconcile`'s rebase phase, and even that is
  `--force-with-lease` only — pr-watch itself has no exception.)
- Never merges the PR. Green CI is the verdict; merge is human-gated.
- Never operates on a PR not on `feat/issue-<N>` — or, with `--bot`, not on
  `dependabot/…` (defense against the caller passing a hand-crafted PR — only
  afk-pickup and deps-land output is supported).
- Never applies `AFK:checks-failed` to a Dependabot PR, and never
  `AFK:deps-failed` to an agent PR. One lane, one label.
- Never spawns reviewer/verifier. The fix-loop is implementer-only; the
  pre-commit review happens during `/auto-agent:afk-dispatch` and the one-time
  post-PR review is `/auto-agent:pr-review` (afk-pickup §6a.1b) — pr-watch
  itself never reviews.
- Never extends the `rounds.pr_watch` cap — nor, in bot mode, the
  `rounds.deps_fix` cap it reads from the markers. Exhaustion is the signal to
  escalate to a human, not to retry harder.
- Never merges a Dependabot PR either. Bot mode gets the bump to green; the
  gate-and-merge decision belongs to the deps-land lane.
