---
name: pr-review
description:
  Run the one-time autonomous code review of a freshly green Agent PR — a
  correctness pass (the plugin's own `/auto-agent:correctness-review`, the
  bug hunt with the posting bar written in) plus a spec pass (diff vs. the
  issue's Acceptance Criteria and parent Spec). Only a defect opens a thread:
  defects are posted as marked inline review comments and the `AFK:revise`
  label routes the next Fire's `/auto-agent:pr-reconcile` loop to fix them;
  Review notes are listed once in the done-marker comment and acted on by
  nobody; product ambiguities ride in the done-marker as structured data for
  the Ruling request. Invoked (blocking) by `/auto-agent:afk-pickup` §6a.1b
  after pr-watch PASS. Takes the PR number + branch + issue number as
  arguments; the repo comes from the Harness config.
---

# PR Review — Autonomous Two-Axis Reviewer, Fixes via the Reconcile Loop

You are the **post-PR reviewer** spawned by `/auto-agent:afk-pickup` after CI
first goes green. One fire = one PR = **once in that PR's life**. You review the
whole diff on two axes, sort every Finding by the posting bar, post the
defects as marked inline review threads, apply `AFK:revise` when at least one
thread was opened so the Daemon's next Fire routes the PR into
`/auto-agent:pr-reconcile`'s proven fix-reply-resolve loop, and return a single
terminal verdict line that the caller pastes into its output block.

**The posting bar** (Spec #74; ADR 0010): a Finding is one of three kinds and
**only a defect opens a thread**. A defect is a concrete failure with the
inputs that trigger it, or a written requirement contradicted and quoted.
Severity is not the bar. A **Review note** (a Standards finding — duplication,
naming, test structure, speculative generality — a test-coverage gap the issue
did not ask for in words, anything "defensible" or "no runtime failure") is
listed once, collapsed, in the done-marker comment; the reconciler ignores it
and the implementer never acts on it. A **product ambiguity is never a
thread**: the implementer can neither fix nor dispute one, so it is carried in
the done-marker as structured data for the Ruling request. A notes-only review
applies no `AFK:revise`; the PR goes straight on to verification. There is no
thread cap: every defect gets a thread, and a defect GitHub refuses to anchor
is posted on its file's first changed line with the intended location named.

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
per line, between sentinels, then a terminal count line. `kind` is the route
(defect → thread; product-ambiguity → the done-marker's structured block;
review-note → the done-marker's collapsed list); `quoted_requirement` is the
sentence a defect contradicts (empty when the defect is a concrete failure):

```
PR_REVIEW_FINDINGS_BEGIN
{"kind":"<defect|product-ambiguity|review-note>","axis":"<correctness|spec>","category":"<slug>","path":"<repo-relative path>","line":<int>,"severity":"high|medium|low","summary":"<one line>","failure_scenario":"<given X, the code does Y, and Z is seen>","quoted_requirement":"<verbatim sentence, or empty>"}
PR_REVIEW_FINDINGS_END
<axis>-review: <k> finding(s)
```

**Correctness axis** — the plugin's own bug hunt, invoked by its namespaced
name so no Host or personal skill can shadow it (ADR 0010). Prompt:

> You are the CORRECTNESS-AXIS reviewer for PR #\<PR_NUM> (branch \<BRANCH>,
> repo \<REPO>). Invoke the `/auto-agent:correctness-review` skill with
> `--pr <PR_NUM> --branch <BRANCH>` via the Skill tool. Below the arguments,
> paste this context so it can quote a requirement: the originating issue
> #\<N> title and body, the extracted Acceptance Criteria, and the parent Spec
> body (or "(none)"). Return the skill's `PR_REVIEW_FINDINGS_BEGIN` …
> `PR_REVIEW_FINDINGS_END` block and its terminal line
> `correctness-review: <k> finding(s)` verbatim — nothing added, nothing
> dropped, nothing re-sorted.

The bar lives in that skill's prompt; the wrapper's only job is faithful
relay. `scope-creep is not a category` on either axis.

**Spec axis** — the orchestrator embeds everything from §1 (the subagent makes
no `gh` calls). Prompt:

> You are the SPEC-AXIS reviewer for PR #\<PR_NUM> (branch feat/issue-\<N>).
> Your only question: does this diff faithfully implement what was asked —
> nothing missing, nothing that contradicts the spec?
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
> Check two things, and ONLY these two:
>
> 1. MISSING REQUIREMENT — an Acceptance Criterion (or an explicit Spec
>    requirement this issue's slice owns) with no corresponding implementation
>    in the diff. Anchor the finding to the changed file + diff line where the
>    implementation should live (the closest hunk in the most relevant file).
>    Quote the criterion in `quoted_requirement`.
> 2. SPEC MISMATCH — code that implements a requirement wrongly (wrong
>    threshold, wrong event name, inverted condition, wrong default — anything
>    that contradicts the written spec). Anchor to the offending line. Quote
>    the contradicted sentence in `quoted_requirement`.
>
> Both are `"kind":"defect"` only when you can quote the requirement. Unasked-for
> code is NOT a finding category any more: unrequested code that changes
> user-visible behaviour is a spec mismatch (quote what it contradicts) or,
> when the Spec is silent on the point and the readings differ for a user, a
> `"kind":"product-ambiguity"` (`category` `ambiguity`; the question in
> `summary`, the readings in `failure_scenario`, "silent" or the clashing
> sentences in `quoted_requirement`); the rest is a `"kind":"review-note"`
> (`category` `note`). A test-coverage gap is a `review-note` unless the issue
> or Spec asks for that test in words — then it is a missing requirement.
>
> Do NOT report style, bugs unrelated to the spec, or Standards opinions —
> the correctness axis owns bugs and nobody runs a Standards pass. Report only
> findings you are confident about; this is a medium-depth review, not a
> fishing trip.
>
> Anchoring rule: every path+line you cite MUST be a line visible in the diff
> above (an added or context line, right-hand side). Never invent line numbers.
>
> Output the sentinel block in the JSON contract with `"axis":"spec"` and
> `category` one of `missing-requirement|spec-mismatch|ambiguity|note`, then
> the terminal line `spec-review: <k> finding(s)`. Zero findings → an empty
> block and `spec-review: 0 findings`.

### 3. Merge, bar, dedupe, post the defects

1. Parse each reply: only lines between `PR_REVIEW_FINDINGS_BEGIN` and
   `PR_REVIEW_FINDINGS_END`; a malformed line is dropped with a logged warning,
   never a crash. A missing `kind` is read as `defect` (the bar below decides).
2. Merge both arrays into one JSON array and **apply the bar**, then split
   into the three routes. The reviewer's sorting is trusted; the orchestrator
   enforces only the mechanical part: a `defect` whose `failure_scenario` is
   empty, or says nothing fails, and that quotes no requirement is demoted to
   a Review note (`demoted: true`), and a Standards category or `scope-creep`
   is a Review note whatever the reviewer called it. Never re-sort by hand:

```bash
. "$AUTO_AGENT_ROOT/lib/review-poster.sh"
SPLIT=$(printf '%s' "$ALL_FINDINGS_JSON" | rp_apply_bar | rp_split_findings)
DEFECTS=$(jq -c '.defects'     <<<"$SPLIT")
NOTES=$(jq -c '.notes'         <<<"$SPLIT")
AMBIGUITIES=$(jq -c '.ambiguities' <<<"$SPLIT")
N_FINDINGS=$(jq 'length' <<<"$ALL_FINDINGS_JSON")
```

3. **Dedupe the defects only**: exact `path:line` collision → one comment
   whose body carries both findings (correctness first); near-dup (same
   `path`, lines within 3, substantially the same summary) → keep the
   higher-severity one. There is **no cap**: every defect gets a thread; a
   defect is never folded into the summary.
4. **No defects** (zero Findings, or a notes-only / ambiguities-only review)
   → go to §5 with an empty thread count. A notes-only review applies no
   `AFK:revise`: the PR goes straight on to the caller's verification tail.
5. **Duplicate guard** (a retried review after a partial post must not re-post):
   enumerate any already-open agent threads and drop defects that already have
   one at the same `path` within 3 lines:

```bash
. "$AUTO_AGENT_ROOT/lib/thread-reconciler.sh"
EXISTING=$(tr_unresolved_threads "$PR_NUM" | rp_filter_agent_threads)
# skip a defect when EXISTING holds a thread with the same .path and |line diff| <= 3
```

6. Post each surviving defect. `rp_render_finding` renders only kind
   `defect` (it refuses the other two kinds with exit 2 — the bar is enforced
   at the one function every thread body passes through):

```bash
BODY=$(rp_render_finding defect "$axis" "$category" "$severity" "$summary" "$failure_scenario")
# a defect that contradicts a requirement: append the quote to the body
[ -n "$quoted_requirement" ] && BODY="$BODY"$'\n\n'"**Contradicts:** > $quoted_requirement"
if rp_post_inline "$PR_NUM" "$REVIEWED_SHA" "$path" "$line" "$BODY" \
   || rp_post_inline_fallback "$PR_NUM" "$REVIEWED_SHA" "$path" "$line" "$BODY"; then
  N_THREADS=$((N_THREADS + 1))   # a thread was opened
fi
```

On a 422 (line not commentable despite the anchoring rule), the fallback
posts the same body on the **file's first changed line** with the intended
location named in the body (`rp_post_inline_fallback` reads the first hunk
of `origin/$BASE...$REVIEWED_SHA -- $path`). If the fallback itself fails
(the file has no changed line, or a second API failure), the defect is not
lost: append it to the §5 done-marker under a `Could not anchor:` list — but
this is a harness error to log, not a routine outcome.

### 4. Hand the fixes to the reconcile loop (`AFK:revise`)

The skill does NOT fix its own findings. Posting the defects created
unresolved review threads; the proven fixer for unresolved threads is
`/auto-agent:pr-reconcile` §2 (the same machinery that handles a human
hand-back). Route the PR into it — **only when at least one thread was
opened** (`N_THREADS` > 0). Review notes and product ambiguities never earn
the label on their own:

```bash
if [ "$N_THREADS" -gt 0 ]; then
  gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:revise
fi
```

The Daemon's next Fire (its Work Probe wakes early on a reconcile candidate)
picks the PR via afk-pickup §1.2 → `/auto-agent:pr-reconcile`, whose comment
loop spawns the implementer, commits `fix(review):` rounds, replies in-thread
`fixed in <sha>: …`, resolves each addressed thread, drops the label, and
re-runs the full CI + manual verification tail. Disputes and exhaustion follow
pr-reconcile's existing escalation.

### 5. Done-marker + terminal verdict

```bash
rp_post_done_marker "$PR_NUM" "$N_FINDINGS" 0 "$REVIEWED_SHA" none "$NOTES" "$AMBIGUITIES" "$EXTRA_MD"
```

`N_FINDINGS` counts every Finding both axes produced (all three kinds); the
fixed-count is always 0 here — fixes happen later in the reconcile loop. The
lib renders the tally (threads, notes and ambiguities counted apart), the one
collapsed **Review notes, no action taken** list, and the product ambiguities
both as a human list and as the structured
`<!-- pr-review-ambiguities … -->` block the Ruling request lane reads back
with `rp_parse_ambiguities`. `EXTRA_MD` is whatever else this review owes the
human, as markdown: §1b's **`Harness config changed — human review
required`** section, a `Could not anchor:` list from §3, and a note when an
axis returned no sentinel block. Empty when there is nothing. The marker must
be posted AFTER the label so a crash between the two leaves the review
retryable, not half-done.

When §1b found paths, print this line first — the caller copies it into its
output block beside the `review:` line, so a human reading the Fire's report
sees the flag without opening the PR:

```
config-change: <n> path(s) under the Harness config dir — AFK:verify-human applied
```

Then print exactly one terminal line:

- `pr-review: PASS — 0 findings` (no Finding of any kind)
- `pr-review: PASS — 0 defects, <n> review note(s), <a> product ambiguity(ies)` (notes and/or ambiguities only — no thread, no label)
- `pr-review: DONE — <N> findings posted, AFK:revise applied` (`<N>` = threads opened; the done-marker carries the notes and ambiguities)
- `pr-review: SKIPPED — already reviewed (done-marker present)`
- `pr-review: ERROR — <reason>`

The afk-pickup caller parses this line verbatim into its output block and routes
on it: `AFK:revise applied` means the fixes (and the re-verification they stale)
belong to the NEXT Fire's reconcile — the current Fire skips manual verification
and exits. Either `PASS` line sends the current Fire straight on to
verification.

## Failure modes

- **PR closed/merged mid-review** — stop, `pr-review: ERROR — pr not open`.
- **Inline post 422** — `rp_post_inline_fallback` posts the defect on the
  file's first changed line with the intended location named (§3 step 6); only
  a second failure lands it in the done-marker's `Could not anchor:` list,
  which is a harness error to log, never a routine outcome.
- **A defect with no scenario and no quote** — demoted to a Review note by
  `rp_apply_bar`, listed in the done-marker with `demoted`, no thread. A
  review that produced only notes posts the done-marker and applies no
  `AFK:revise`.
- **A product ambiguity** — never a thread; it rides in the done-marker's
  structured block for the Ruling request lane. Alone it earns no label.
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
  inline review comments (defects only), the §1b `AFK:verify-human` flag, at
  most one `AFK:revise` label add (only when a thread was opened), and one
  done-marker comment. All fixing belongs to `/auto-agent:pr-reconcile`.
- Never opens a thread for a Review note or a product ambiguity, never caps
  the defect threads, and never folds a defect into the summary while its
  file has a changed line to anchor to.
- Never runs a Standards pass and never calls a review skill by a bare name:
  the correctness axis is `/auto-agent:correctness-review` (ADR 0010).
- Never merges the PR. Merge is human-gated.
- Never replies to or resolves ANY thread (not even its own) — thread
  reply/resolution is `/auto-agent:pr-reconcile` §2's job.
- Never applies any label other than `AFK:revise` and the `AFK:verify-human`
  flag of §1b, never removes a label, never drafts the PR.
- Posts exactly one done-marker comment ever per PR — it is the once-per-PR
  idempotency gate for every future §6a.1b entry.
- Never operates on a PR not on `feat/issue-<N>` (only afk-pickup output is
  supported).
