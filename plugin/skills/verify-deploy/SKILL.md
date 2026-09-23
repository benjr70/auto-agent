---
name: verify-deploy
description:
  Run one deployed-verification round against a merged Agent PR — the
  Deployed tier of the Verification Harness. Parse the PR's deferred
  (`<!-- post-deploy: … -->`-tagged) checklist items, read the live
  environment's `KEY=value` block from the Target Project's deployed command
  (`status`, never `up` or `down`), spawn the manual-verifier subagent to
  exercise each item read-only against it, tick the boxes that passed, post one
  evidence comment and emit the terminal `deployed-verify:` line. Config-driven:
  the lane is on only when the Harness config declares `verification.deployed`
  and `enabled` is not false. Use when anyone says "verify the deploy of PR
  <n>", "run the post-deploy checks", or invokes /auto-agent:verify-deploy — a
  human, or `/auto-agent:afk-pickup` §2c on a `deployed` triage verdict.
argument-hint: '--pr <PR_NUM> [--round <M>/<MAX>]'
---

# Verify Deploy — One Deployed-Verification Round

A hermetic round (`/auto-agent:verify-pr`) proves what a per-PR environment
can prove and **defers** what only a real deployment can: it demands a
`<!-- post-deploy: … -->`-tagged item for each, and the item stays unticked
when the PR merges. This skill is where those items get run. You orchestrate
one round over one merged Agent PR against the **live** environment the
Target Project's own deployed command resolves; the
`auto-agent:manual-verifier` subagent does the testing, you own setup and the
PR mutation.

It is the same round as verify-pr in every way that matters — the same
checklist protocol, the same verifier core, the same evidence rules, one
comment — with two differences that are load-bearing:

- **Read-only.** The environment is live and shared. You call the deployed
  command's `status` and nothing else: **never `up`, never `down`**, never
  `verify-boot`, never a deploy, a restart or a write to the live system.
  There is no teardown, because nothing was booted.
- **The targets are the command's.** The deployed command resolves its own
  hosts, tunnels and URLs (ADR 0003); the Harness config carries no hostname,
  and neither does this round. The `status` block is the only address book.

## Harness context

Every repo fact comes from the Harness config (ADR 0002). Read it once, first
thing, and never spell one as a literal:

```bash
AA="${AUTO_AGENT_ROOT:?the Fire wrapper exports AUTO_AGENT_ROOT}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")
MAX=$(jq -r .rounds.manual_verify <<<"$CFG")                 # the round cap
CHECKS=$(jq -r '.prose.deployed_checks // empty' <<<"$CFG")  # the maintainer's deployed checks
RUNBOOK=$(jq -r '.prose.verifier_runbook // empty' <<<"$CFG")
```

The config is the **default branch's**, the one the Fire resolved: the PR has
merged, so its config is the default branch's too. There is no `--head` here
and nothing to check out.

## Invocation

```
/auto-agent:verify-deploy --pr <PR_NUM> [--round <M>/<MAX>]
```

`--pr` is required; if it is missing or not a merged PR, stop with a clear
message — never guess. `--round` is what the caller's triage counted (the
`deployed` verdict's `.round` and `.max`); without it, count the PR's
`### Deployed verification — round` comments and add one. Do not run a round
past the cap: at `M > MAX` say so and stop.

## The round, step by step

### 0. The lane is on, or there is no round

```bash
LANE=$("$AA" deployed lane); LANE_RC=$?
```

The lane is on only when the Harness config declares `verification.deployed`
and `enabled` is not false (an omitted `enabled` is on). Off (exit 3):

```
deployed-verify: SKIPPED — <the deployed-lane: off … line>
```

and stop. A Fire never gets here with the lane off — triage never asks — but a
human can, and the answer is the same.

### 1. Fetch the PR and parse its deferred items

```bash
gh pr view "$PR_NUM" --repo "$REPO" --json number,state,headRefName,body
gh pr view "$PR_NUM" --repo "$REPO" --json body -q .body | "$AA" deployed items
```

Stop unless `state` is `MERGED`. `deployed items` prints the **unchecked**
items of the two verification sections that carry the `<!-- post-deploy:`
tag — the checklist parser's answer, filtered to the deferrals. Every other
item belongs to the hermetic round or to a human, and you never touch it.
Parse it, never eyeball the body yourself.

With no items there is nothing to run: emit
`deployed-verify: 0/0 PASS, 0 deferred, 0 FAIL` and stop, with no comment.

### 2. The round's evidence directory

```bash
ARTIFACT_DIR=$("$AA" evidence dir --pr "$PR_NUM" --round "$M")
```

The same sink as the hermetic round: one directory per round in the State
dir. Its path is cited in the evidence comment.

### 3. Read the live environment's block — `status` only

```bash
BLOCK=$("$AA" deployed status); STATUS_RC=$?
while IFS='=' read -r k v; do [ -n "$k" ] && export "$k=$v"; done <<<"$BLOCK"
```

`deployed status` runs the deployed command's `status` from the Target
Project root and prints only its `KEY=value` block; the command's own
progress and the tier's warnings are on stderr. It never calls `up` or
`down`. Branch on its exit code:

- **0** — the live environment answered healthy, with every declared
  Surface's `url_key` in the block. The block is exported.
- **4** — the live environment is not reachable (`status` said unhealthy, or
  a prerequisite — a tunnel, a key, a login — is missing on this Host).
  **An infrastructure error, not a verdict**: do NOT spawn the subagent, do
  NOT invent item verdicts. Post the round's one comment with the stderr tail,
  emit `deployed-verify: infra-error — the deployed environment is not reachable (0 items verified)`
  and stop. The round still counts toward the cap.
- **2** — the deployed command broke its contract (not executable, an exit
  outside 0/1/3, progress on stdout, a declared Surface's `url_key` missing).
  The same infra-error path, with
  `deployed-verify: infra-error — the deployed command broke its contract (0 items verified)`.
- **3** — the lane is off: §0's SKIPPED line.

### 4. Spawn the manual-verifier subagent — deployed round

Spawn `auto-agent:manual-verifier` (blocking) with, in the prompt:

- the line `round: deployed` — it switches the subagent to its deployed-round
  rules (read-only, and a deployed-env item is exercised, not deferred);
- every deferred item, verbatim, with its `manual`/`human` tag and the check
  its `post-deploy:` tag spells out;
- the exported block, naming each Surface's URL key, and the Surface list
  with kinds;
- `ARTIFACT_DIR`;
- `$CHECKS`' contents when the config names one — the maintainer's
  description of what the live environment should answer — and `$RUNBOOK`'s.

Say in the prompt, verbatim:

> This is a deployed round against a LIVE, shared environment. Exercise each
> item read-only: requests that read, screens you navigate without
> submitting. Never create, change or delete anything in it, never restart or
> redeploy anything, never run the Environment provider's `up` or `down`. An
> item that can only be proven by a write is a DEFER to a human, naming the
> write it needs.

The subagent returns a per-item verdict block plus a `verifier-tally:` line.
You do **not** re-test; you consume its report. No screenshot tour is owed:
the tour documents a PR's change and was captured by its hermetic rounds; a
screenshot the subagent takes as evidence is cited like any other file.

### 5. Reconcile the result onto the PR

1. **Tick the boxes that passed** — passing items only; deferred and failed
   items keep their empty box, and a ticked box is never un-ticked. The one
   ticker, on the merged PR's body:

   ```bash
   gh pr view "$PR_NUM" --repo "$REPO" --json body -q .body > "$ARTIFACT_DIR/body.md"
   printf '%s\n' "$PASSED_ITEMS" | "$AA" checklist tick "$ARTIFACT_DIR/body.md" > "$ARTIFACT_DIR/body.new.md"
   gh pr edit "$PR_NUM" --repo "$REPO" --body-file "$ARTIFACT_DIR/body.new.md"
   ```

2. **Post exactly one evidence comment for the round** — never one per item.
   Head it `### Deployed verification — round <M>/<MAX>`: that header is what
   the lane counts rounds by, so never reword it. It lists, per item: the
   verdict, the concrete evidence (request lines, status codes, the value
   read), and for a deferral the named blocker. It cites `ARTIFACT_DIR`. On
   the last round (`M == MAX`) with anything still unticked, say so: those
   items are a human's now, and the lane will not pick this PR again.

   ```bash
   gh pr comment "$PR_NUM" --repo "$REPO" --body-file "$ARTIFACT_DIR/comment.md"
   ```

3. **Emit the result line** as your final output — the machine-readable
   contract the caller parses. Write it in your own assistant message; never
   `echo` it from Bash and call that the answer:

   ```
   deployed-verify: <pass>/<total> PASS, <deferred> deferred, <fail> FAIL — round <M>/<MAX> [— EXHAUSTED]
   ```

   The tally has the hermetic round's shape. `— EXHAUSTED` is appended when
   `M == MAX` and anything is left unticked. An unjustified deferral counts as
   a **FAIL** — the subagent already classified it that way.

## Hard rules

- **Read-only, always.** `status` is the only subcommand of the deployed
  command this round runs. Never `up`, never `down`, never `verify-boot`, never
  a deploy, a restart, or a write to the live environment.
- **Never fabricate a verdict.** If `status` did not answer healthy there are
  zero item verdicts — say so (`deployed-verify: infra-error …`).
- **Only tick what passed.** Deferred and failed items keep their empty box.
  Never un-tick a box; never touch an item without the `post-deploy:` tag.
- **One comment per round**, headed exactly `### Deployed verification — round <M>/<MAX>`.
- **Install nothing.** A missing tunnel, key or tool is an infrastructure
  finding for the Host's operator, not something to fix from here.
- **You do not verify.** The subagent tests; you orchestrate and reconcile.
- **Never reopen, revert or push.** The PR is merged; this round edits its body
  and posts one comment, nothing else.

## Demo

Against the fixture Target Project, without a PR: the lane gate, the deferred
items of a body, and a `status` read of a "live" environment (the fixture's
own service standing in for one).

```bash
"$AA" deployed lane plugin/fixtures/target-project      # off: the fixture ships enabled false
printf '## Manual verification\n\n- [ ] live <!-- post-deploy: GET /api/health -->\n' | "$AA" deployed items
```
