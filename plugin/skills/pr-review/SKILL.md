---
name: pr-review
description:
  Run the one-time autonomous code review of a freshly green Agent PR — a
  correctness pass (built-in /code-review at medium effort) plus a spec pass
  (diff vs. the issue's Acceptance Criteria and parent Spec), findings posted as
  marked inline review comments, then the `AFK:revise` label applied so the next
  Fire's `/auto-agent:pr-reconcile` loop fixes the threads, plus a done-marker
  comment. Invoked (blocking) by `/auto-agent:afk-pickup` §6a.1b after pr-watch
  PASS. Takes the PR number + branch + issue number as arguments; the repo comes
  from the Harness config.
---

# PR Review — Autonomous Two-Axis Reviewer, Fixes via the Reconcile Loop

You are the **post-PR reviewer** spawned by `/auto-agent:afk-pickup` after CI
first goes green. One fire = one PR = **once in that PR's life**. You review the
whole diff on two axes, post findings as marked inline review threads, apply
`AFK:revise` so the Daemon's next Fire routes the PR into
`/auto-agent:pr-reconcile`'s proven fix-reply-resolve loop, and return a single
terminal verdict line that the caller pastes into its output block.

The reviewer **never fixes its own findings** — separation is structural: this
skill only writes review comments and one label; `/auto-agent:pr-reconcile` §2
owns every fix, in-thread reply, and thread resolution. The review is
best-effort: it never drafts the PR, and merge remains human-gated regardless of
the outcome.

## Harness context

The repo and the default branch come from the Harness config (ADR 0002). Read
them once, first thing, and never spell either as a literal:

```bash
AA="${AUTO_AGENT_ROOT:?the Fire wrapper exports AUTO_AGENT_ROOT}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")              # the Target Project, from its origin remote
BASE=$(jq -r .repo.default_branch <<<"$CFG")    # detected from GitHub, never declared
```

This skill assumes:

- The PR is already open on `feat/issue-<N>` against the default branch
  (`$BASE`) and CI is green (the caller runs it only after `pr-watch: PASS`).
- `lib/thread-reconciler.sh` and `lib/review-poster.sh` exist in the Harness
  install — sourceable deep modules that read the repo from the Harness config.
  Never hand-roll their GraphQL/REST calls or marker strings.
- The `AFK:revise` label exists (created by `/auto-agent:afk-dispatch` §0).
- The caller checks the done-marker before spawning; §0 re-checks anyway
  (defense in depth).

## Invocation

```
/auto-agent:pr-review --pr <PR_NUM> --branch <BRANCH> --issue <ISSUE_N>
```

All three arguments required. There is no `--repo` argument. The caller
(afk-pickup §6a.1b) supplies the arguments verbatim from its PR-create step;
`BRANCH` must be `feat/issue-<ISSUE_N>`.

## Process

### 0. Pre-flight + idempotency gate

```bash
gh auth status >/dev/null || { echo "pr-review: ERROR — gh not authenticated"; exit 1; }
gh pr view "$PR_NUM" --repo "$REPO" --json number,headRefName,state \
  | jq -e --arg br "$BRANCH" '.headRefName == $br and .state == "OPEN"' >/dev/null \
  || { echo "pr-review: ERROR — PR #$PR_NUM not open on $BRANCH"; exit 1; }

. "$AUTO_AGENT_ROOT/lib/review-poster.sh"
if rp_done_marker_present "$PR_NUM"; then
  echo "pr-review: SKIPPED — already reviewed (done-marker present)"
  exit 0
fi

git fetch origin
git checkout "$BRANCH"
git reset --hard "origin/$BRANCH"
REVIEWED_SHA=$(git rev-parse HEAD)
```

### 1. Gather context

1. **Issue** — `gh issue view "$ISSUE_N" --repo "$REPO" --json title,body`.
2. **Acceptance Criteria block** — everything between a heading matching
   `^## *Acceptance [Cc]riteria` and the next `^## ` heading (or end of body);
   the same extraction afk-pickup §6a uses. Absent → note "(none found)".
3. **Parent Spec** — the first `#<digits>` reference inside the issue body's
   `## Parent` section (the `/auto-agent:to-tickets` convention). Issues created before the
   rename use the legacy heading `## Parent PRD` (a literal to match, not a
   term this harness uses: the glossary says Spec); accept either (match
   `^## *Parent( PRD)?\b`, preferring `## Parent`). If found,
   `gh issue view <SPEC_N> --repo "$REPO" --json title,body`. Neither section →
   proceed AC-only and say so in the spec-axis prompt.
4. **Diff** — `git diff "origin/$BASE...HEAD"`, capped at 2000 lines; if
   longer, truncate with a `... [truncated]` marker (pr-watch §3 convention).

### 1b. Flag a PR that changes the Harness config (ADR 0007)

The verification round reads `.auto-agent/harness.json` **from the PR head**,
so a PR can change its own verification: drop a Surface, turn `smoke` off,
point `hermetic.command` somewhere harmless. That is not something a reviewing
agent can sign off, because the agent verifying it is the thing being
configured. Every Agent PR that touches the Harness config directory is
therefore flagged for a human, whatever the two axes find:

```bash
CFG_TOUCHED=$("$AA" bootstrap config-touched --pr "$PR_NUM" --head); CFG_RC=$?
if [ -n "$CFG_TOUCHED" ] || [ "$CFG_RC" -ne 0 ]; then
  gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:verify-human
fi
```

`--head` reads the config from the checkout you are standing in (§0 put you on
the PR branch), not from the one the Fire resolved on the default branch — the
PR that ADDS `.auto-agent/` to a project has no config on the default branch to
read. A non-zero exit **also** flags: a config change must never go unflagged
because a `gh` call failed.

The label is the flag; it is never removed here, and it never replaces the
review. Carry the paths into the §5 done-marker comment under a
**`Harness config changed — human review required`** heading, one path per
line, saying what a human must check: that the change does not weaken this
PR's own verification (a removed Surface, `smoke` turned off, a redirected
`hermetic.command`), and that the screenshot tour of any touched UI Surface is
still mandatory (ADR 0003) whatever the config now says.

This runs on **every** Agent PR, in the Bootstrap state or out of it. The PR
that adds the first Environment provider touches `.auto-agent/` by definition,
so it is flagged too — expected, not a defect.

### 2. Round 1 — dual-axis review (parallel subagents)

Spawn **both** wrappers in a single message so they run concurrently. Each:
`subagent_type: general-purpose`, `run_in_background: false` (blocking — wait
for both before §3). Do not pass a `model`: the Fire's model policy carries
through.

Both must return findings in the same contract — a fenced block, one JSON object
per line, between sentinels, then a terminal count line:

```
PR_REVIEW_FINDINGS_BEGIN
{"axis":"<correctness|spec>","category":"<slug>","path":"<repo-relative path>","line":<int>,"severity":"high|medium|low","summary":"<one line>","failure_scenario":"<what concretely goes wrong if shipped as-is>"}
PR_REVIEW_FINDINGS_END
<axis>-review: <k> finding(s)
```

**Correctness axis** — prompt:

> You are the CORRECTNESS-AXIS reviewer for PR #\<PR_NUM> (branch \<BRANCH>,
> repo \<REPO>). Check out the branch, then invoke the built-in `/code-review`
> skill at **medium** effort against the branch diff vs the default branch
> (`origin/<BASE>`). Do NOT pass `--comment` and do NOT pass `--fix`. When it
> completes, restate every finding it produced — nothing added, nothing dropped
> — in the JSON contract below, with `"axis":"correctness"` and `category` one
> of `bug|logic-error|data-loss|race|error-handling|security`. Cite only
> path+line pairs that appear in the diff (right-hand side). Then the sentinel
> block and the terminal line `correctness-review: <k> finding(s)`.

Treat `/code-review`'s native output as opaque; the wrapper's only job beyond
invoking it is faithful translation into the contract.

**Spec axis** — the orchestrator embeds everything from §1 (the subagent makes
no `gh` calls). Prompt:

> You are the SPEC-AXIS reviewer for PR #\<PR_NUM> (branch feat/issue-\<N>).
> Your only question: does this diff faithfully implement what was asked —
> nothing missing, nothing extra, nothing that contradicts the spec?
>
> ## Originating issue #\<N>: \<title>
>
> \<issue body>
>
> ## Acceptance Criteria (extracted)
>
> \<AC block, or "(none found — judge against the issue body and Spec)">
>
> ## Parent Spec issue #\<P>: \<title>
>
> \<Spec body, or "(no Parent section in the issue — judge against the issue
> alone)">
>
> ## The diff under review (origin/\<BASE>...HEAD, the default branch, capped
> 2000 lines)
>
> \<diff>
>
> Check three things, and ONLY these three:
>
> 1. MISSING REQUIREMENT — an Acceptance Criterion (or an explicit Spec
>    requirement this issue's slice owns) with no corresponding implementation
>    in the diff. Anchor the finding to the changed file + diff line where the
>    implementation should live (the closest hunk in the most relevant file).
> 2. SCOPE CREEP — a substantive change not traceable to the issue, its AC, or
>    the Spec (drive-by refactors, new endpoints/config/deps nobody asked for).
>    Anchor to the offending added line.
> 3. SPEC MISMATCH — code that implements a requirement wrongly (wrong
>    threshold, wrong event name, inverted condition, wrong default — anything
>    that contradicts the written spec). Anchor to the offending line.
>
> Do NOT report style, bugs unrelated to the spec, or test-coverage opinions —
> the correctness axis owns those. Report only findings you are confident about;
> this is a medium-depth review, not a fishing trip.
>
> Anchoring rule: every path+line you cite MUST be a line visible in the diff
> above (an added or context line, right-hand side). Never invent line numbers.
>
> Output the sentinel block in the JSON contract with `"axis":"spec"` and
> `category` one of `missing-requirement|scope-creep|spec-mismatch`, then the
> terminal line `spec-review: <k> finding(s)`. Zero findings → an empty block
> and `spec-review: 0 findings`.

### 3. Merge, dedupe, post inline

1. Parse each reply: only lines between `PR_REVIEW_FINDINGS_BEGIN` and
   `PR_REVIEW_FINDINGS_END`; a malformed line is dropped with a logged warning,
   never a crash.
2. Merge both arrays. **Dedupe**: exact `path:line` collision → one comment
   whose body carries both findings (correctness first); near-dup (same `path`,
   lines within 3, substantially the same summary) → keep the higher-severity
   one. Bias toward fewer comments — this is a medium-depth review. Cap at 10
   posted comments; if more survive, post the 10 highest-severity and list the
   rest in the §5 done-marker comment (never silently drop).
3. **Zero findings** →
   `rp_post_done_marker "$PR_NUM" 0 0 "$REVIEWED_SHA" none`, print
   `pr-review: PASS — 0 findings`, exit 0.
4. **Duplicate guard** (a retried review after a partial post must not re-post):
   enumerate any already-open agent threads and drop findings that already have
   one at the same `path` within 3 lines:

```bash
. "$AUTO_AGENT_ROOT/lib/thread-reconciler.sh"
EXISTING=$(tr_unresolved_threads "$PR_NUM" | rp_filter_agent_threads)
# skip a finding when EXISTING holds a thread with the same .path and |line diff| <= 3
```

5. Post each surviving finding:

```bash
BODY=$(rp_render_finding "$axis" "$category" "$severity" "$summary" "$failure_scenario")
rp_post_inline "$PR_NUM" "$REVIEWED_SHA" "$path" "$line" "$BODY"
```

On a 422 (line not commentable despite the anchoring rule): retry once at the
first added line of that file's first hunk; if that also fails, fold the finding
into the §5 done-marker comment under a `Could not anchor:` list — it is
reported but produces no thread.

### 4. Hand the fixes to the reconcile loop (`AFK:revise`)

The skill does NOT fix its own findings. Posting them created unresolved review
threads; the proven fixer for unresolved threads is `/auto-agent:pr-reconcile`
§2 (the same machinery that handles a human hand-back). Route the PR into it:

```bash
gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:revise
```

The Daemon's next Fire (its Work Probe wakes early on a reconcile candidate)
picks the PR via afk-pickup §1.2 → `/auto-agent:pr-reconcile`, whose comment
loop spawns the implementer, commits `fix(review):` rounds, replies in-thread
`fixed in <sha>: …`, resolves each addressed thread, drops the label, and
re-runs the full CI + manual verification tail. Disputes and exhaustion follow
pr-reconcile's existing escalation (`AFK:revise-failed`, parked for a human).

### 5. Done-marker + terminal verdict

```bash
rp_post_done_marker "$PR_NUM" "$N_FINDINGS" 0 "$REVIEWED_SHA" none
```

(`N_FINDINGS` = findings posted in §3; the fixed-count is always 0 here — fixes
happen later in the reconcile loop. Append any `Could not anchor:` / over-cap
findings from §3 to this comment's body. The marker must be posted AFTER the
label so a crash between the two leaves the review retryable, not half-done.)

When §1b found paths, print this line first — the caller copies it into its
output block beside the `review:` line, so a human reading the Fire's report
sees the flag without opening the PR:

```
config-change: <n> path(s) under the Harness config dir — AFK:verify-human applied
```

Then print exactly one terminal line:

- `pr-review: PASS — 0 findings`
- `pr-review: DONE — <N> findings posted, AFK:revise applied`
- `pr-review: SKIPPED — already reviewed (done-marker present)`
- `pr-review: ERROR — <reason>`

The afk-pickup caller parses this line verbatim into its output block and routes
on it: `AFK:revise applied` means the fixes (and the re-verification they stale)
belong to the NEXT Fire's reconcile — the current Fire skips manual verification
and exits.

## Failure modes

- **PR closed/merged mid-review** — stop, `pr-review: ERROR — pr not open`.
- **Inline post 422** — two-stage fallback per §3 step 5; the finding still
  surfaces in the done-marker comment.
- **Crash after posting comments but before the label/marker** — the done-marker
  is absent, so a later Fire's tail retries the whole review; §3's duplicate
  guard keeps the retry from re-posting the same threads.
- **gh rate limit on posting / label edit** — `pr-review: ERROR`; same retry
  property (marker absent → next tail run tries again).
- **Sentinel block missing/garbled from an axis subagent** — treat that axis as
  0 findings and note it in the done-marker comment; never crash the round.
- **No Harness config resolvable** — the poster and reconciler libs return 2
  and make no call; stop with `pr-review: ERROR — no Harness config`.
- **`bootstrap config-touched` cannot read the diff** (§1b) — treat it as
  "flag it": apply `AFK:verify-human` and say in the done-marker comment that
  the config diff could not be read. A config change must never go unflagged
  because a `gh` call failed.

## Boundaries

- Never pushes, commits, or edits code — the skill's entire write surface is
  inline review comments, the §1b `AFK:verify-human` flag, one `AFK:revise`
  label add, and one done-marker comment. All fixing belongs to
  `/auto-agent:pr-reconcile`.
- Never merges the PR. Merge is human-gated.
- Never replies to or resolves ANY thread (not even its own) — thread
  reply/resolution is `/auto-agent:pr-reconcile` §2's job.
- Never applies any label other than `AFK:revise` and the `AFK:verify-human`
  flag of §1b, never removes a label, never drafts the PR.
- Posts exactly one done-marker comment ever per PR — it is the once-per-PR
  idempotency gate for every future §6a.1b entry.
- Never operates on a PR not on `feat/issue-<N>` (only afk-pickup output is
  supported).
