---
name: verify-pr
description:
  Run one manual-verification round against an open Agent PR — parse its
  verification checklist, boot the per-PR environment through the Target
  Project's Environment provider, spawn the manual-verifier subagent to
  exercise each unchecked item on the declared Surfaces, capture a screenshot
  tour of every touched UI Surface, tick the boxes that passed, post one
  evidence comment, emit the terminal `manual-verify:` line, and tear
  everything down. Use when anyone says "verify PR <n>", "run manual
  verification", or invokes /auto-agent:verify-pr — a human, an agent calling
  the Skill tool, or the subagent `/auto-agent:afk-pickup` §6a.2 and
  `/auto-agent:pr-reconcile` §3 delegate the round to.
argument-hint: '--pr <PR_NUM> [--issue <N>] [--round <M>/<MAX>] [--force-tour]'
---

# Verify PR — One Manual-Verification Round

You are the orchestrator of a single **manual-verification round** for one open
Agent PR. You wire the Target Project's Environment provider, the launchers its
Surfaces need and the `auto-agent:manual-verifier` subagent together, then
reconcile the result onto the PR. **You do not verify anything yourself** — the
subagent does the testing; you own setup, mutation and teardown.

Three properties are load-bearing. The round is **honest** (it never fabricates
a verdict), **self-cleaning** (teardown on every exit path, including your own
error), and **idempotent on the PR** (it only ever ticks boxes that passed,
never un-ticks, and never re-verifies a box a human already signed off).

## Harness context

Every repo fact comes from the Harness config (ADR 0002). Read it once, first
thing, and never spell one as a literal:

```bash
AA="${AUTO_AGENT_ROOT:?the Fire wrapper exports AUTO_AGENT_ROOT}/bin/auto-agent"
CFG="${HARNESS_CONFIG_JSON:-$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")}"
REPO=$(jq -r .repo.slug <<<"$CFG")
HERMETIC=$(jq -c '.verification.hermetic' <<<"$CFG")   # null == Bootstrap state
RUNBOOK=$(jq -r '.prose.verifier_runbook // empty' <<<"$CFG")
```

**The config this round obeys is the one on the PR head** (ADR 0007), and
`$HARNESS_CONFIG_JSON` is not it: the Fire resolved that once, from the default
branch, before this PR was ever checked out. So §3 re-reads the config from the
checkout, and every round command below carries **`--head`**, which reads the
checkout rather than the inherited one. That is what makes the PR that ADDS an
Environment provider verifiable by the provider it adds — its first green round
is the evidence that closes the Bootstrap state.

## Invocation

```
/auto-agent:verify-pr --pr <PR_NUM> [--issue <N>] [--round <M>/<MAX>] [--force-tour]
```

`--pr` is required; if it is missing or not an open PR, stop with a clear
message — never guess. `--round <M>/<MAX>` is what the caller's loop is on, and
it heads this round's single evidence comment. `--force-tour` says **capture
the tour whatever the touched-path detection says**: it exists for the bot-PR
lane, where a lockfile-only diff touches no Surface path but the whole risk is
broken pixels. It can only ADD shots, never suppress them, so a caller passing
it can never weaken a round.

### Callers — any caller is legitimate

A human typing the command, an agent calling the Skill tool, and a
`general-purpose` subagent told to invoke this skill are all ordinary entry
points, and none needs a human in the loop. Do not refuse, defer or downgrade a
round because the caller is a model rather than a person: an unrun round is the
pipeline's last quality gate going missing. If the round genuinely cannot run,
say so with `manual-verify: infra-error …`; never hand it back as a silent skip.

## The round, step by step

### 0. Bootstrap state first

```bash
if [ "$HERMETIC" = "null" ]; then
  gh pr edit "$PR_NUM" --repo "$REPO" --add-label AFK:verify-human
  # one comment saying why, then:
  echo "manual-verify: SKIPPED — Bootstrap state, AFK:verify-human applied"
  exit 0
fi
```

There is no environment to boot and no round to run. Do not park, do not loop.
A Target Project that HAS a hermetic tier never earns that line.

### 1. Fetch the PR and parse its checklist

```bash
gh pr view "$PR_NUM" --repo "$REPO" --json number,headRefName,body,state
gh pr view "$PR_NUM" --repo "$REPO" --json body -q .body | "$AA" checklist parse
```

Stop unless `state` is `OPEN`. `checklist parse` prints the **unchecked** items
of the two verification sections only, tagged `manual` or `human`; a checkbox
anywhere else in the body is not a verification item. Parse it, never eyeball
the body yourself.

No items is not automatically a no-op: check §1b first — a PR that changes a UI
Surface still earns a tour. With no items **and** no tour Surfaces, post a short
comment saying so, emit

```
manual-verify: 0/0 PASS, 0 deferred, 0 FAIL
screenshots: none (no UI Surface touched)
```

and stop; nothing needs booting.

### 1b. Which Surfaces this PR touched, and which earn a tour

Touched-path detection is a tested judgement over the Surface declarations, not
a call you make per round:

```bash
TOUCHED=$("$AA" surfaces touched --pr "$PR_NUM" --head)   # every kind
TOUR=$("$AA" surfaces tour --pr "$PR_NUM" --head)         # browser + electron only
```

`browser` and `electron` Surfaces **always earn a tour when touched**; `cli` and
`api` Surfaces are evidence-only (ADR 0003). A project cannot opt out of
screenshots by declaration, and you never add a Surface the config does not
declare. With `--force-tour`, set `TOUR` to every `browser`/`electron` Surface
in the config without consulting the detection; `screenshots: none` is then
never a legal outcome.

Each Surface's capture shape comes from its declaration, never from the
verifier:

```bash
"$AA" surfaces viewport <surface> --head   # the declared viewport, else the default
```

### 2. The round's evidence directory

```bash
ARTIFACT_DIR=$("$AA" evidence dir --pr "$PR_NUM" --round "$M")
```

One directory per round, in the State dir — outside the checkout, which the
Daemon resets. Its path is cited in the evidence comment.

### 3. Check the PR out, then re-read the config from the head

```bash
gh pr checkout "$PR_NUM"
CFG=$("$AA" show-config "${AUTO_AGENT_TARGET_DIR:-.}")   # the PR head's config
export HARNESS_CONFIG_JSON="$CFG"                        # what every lib now reads
HERMETIC=$(jq -c '.verification.hermetic' <<<"$CFG")
```

The environment is booted from **this** checkout, so what runs is the PR's
code, and the config the round obeys is the PR head's (ADR 0007). Re-read
`$HERMETIC` and every Surface answer after this step; a Surface the PR added
exists only here.

### 4. Boot the environment and the apps it needs (one call, one retry inside)

```bash
BLOCK=$("$AA" verify-boot up --pr "$PR_NUM" --head $(printf -- '--surface %s ' $TOUCHED))
while IFS='=' read -r k v; do [ -n "$k" ] && export "$k=$v"; done <<<"$BLOCK"
```

`verify-boot` runs `down` before the first `up`, retries a failed boot exactly
once with a `down` between, checks the block grammar and every declared
Surface's `url_key`, and launches the app of each `electron` Surface you named.
Its stdout is only the block; progress is on stderr. Branch on its exit code:

- **0** — healthy. The block is exported; `AUTO_AGENT_SANDBOX` says `OK` or
  `DEGRADED` when an app was launched. Proceed.
- **2** — the config or the provider is unusable; nothing booted. Report it as
  an infra-error (below).
- **3** — Bootstrap state (see §0).
- **4** — the environment did not boot (a prerequisite is missing, or the boot
  failed twice; teardown already ran). **An infrastructure error, not a
  verdict**: do NOT spawn the subagent, do NOT invent item verdicts. Post the
  infrastructure-error comment with the stderr tail, emit
  `manual-verify: infra-error — the environment did not boot (0 items verified)`
  and stop.
- **5** — the environment is healthy but an app launch failed; the environment
  is left up. Retry that one launch with
  `"$AA" surface-launch start <surface> --pr "$PR_NUM" --head`; if it fails again,
  note it in the evidence comment and continue with the Surfaces you have.

**A DEGRADED sandbox is reported, never silent.** When `AUTO_AGENT_SANDBOX` is
`DEGRADED`, the app's own sandbox is off because this Host has no profile
granting it user namespaces: carry `AUTO_AGENT_SANDBOX_DETAIL` into the
evidence comment and suffix the result line with ` (sandbox DEGRADED)`. It
never turns an item into a FAIL and is never a reason to skip the round.

### 5. The Surfaces the verifier drives

Each Surface's kind selects its tooling, and the harness owns that choice:

- a `browser` Surface is driven through its MCP server — headful on the Host's
  display, a fresh profile per run, at the declared viewport. The Fire already
  registered it: `surface-launch mcp-config` renders one MCP entry per UI
  Surface from the config, each running `surface-launch mcp <surface>`, and the
  Fire wrapper passes that registry to the session;
- an `electron` Surface is the app §4 launched, attached over the Chrome
  DevTools Protocol by the same command. **The server registers before the app
  exists, by design**: it dials lazily, so a tool call made before §4 launched
  the app fails with a connection error. That is a sequencing mistake, not a
  missing tool — launch first, then call;
- `cli` and `api` Surfaces have no launcher: the verifier drives them with its
  own shell and HTTP calls, from the URLs the block carried.

Never `npm start` an app, never launch a browser by hand, never hand-roll a
debugging-protocol client: the launchers are the only route to a Surface.

**Display truth comes only from the Host env's `DISPLAY`** (Xvfb on the
reference Host), resolved by the display lib every launcher shares. An unset
`DISPLAY` in your own shell is never evidence of a headless Host — that shell
simply does not inherit it. The lib failing (exit 3) is the one legitimate "no
display" signal, and it is an infrastructure finding, not an item verdict.

### 6. Spawn the manual-verifier subagent

Spawn `auto-agent:manual-verifier` (blocking) with, in the prompt:

- every parsed item, with its `manual`/`human` tag;
- the exported block, naming each Surface's URL key, and the Surface list with
  kinds;
- `ARTIFACT_DIR`;
- `$RUNBOOK`'s contents when the config names one — the maintainer's
  description of what a healthy environment looks like;
- the tour instruction below when `$TOUR` is non-empty.

The subagent classifies each item (local → execute it now on the real Surface;
deployed-env → defer and demand a post-deploy spec; hardware → defer to a human
with a named blocker), gathers concrete evidence, and returns a per-item verdict
block plus a `verifier-tally:` line. You do **not** re-test; you consume its
report.

> This PR touches these UI Surfaces: \<TOUR>. In addition to your per-item
> evidence, capture a screenshot tour of them on the real Surface: each screen
> the diff touches, in the state a reviewer would want to see (populated, not an
> empty first-run screen), one file per screen, **viewport-clipped, never
> full-page** — fixed and sticky chrome is painted once at its viewport anchor,
> so a full-page capture of a longer document strands that chrome mid-image and
> a reviewer reads it as a layout bug that does not exist. When what a reviewer
> needs is below the fold, scroll to it and capture the viewport there. **Set
> the viewport before you capture, per Surface**, to the shape given in this
> prompt — it comes from the Harness config; never choose your own. Name each
> file `<surface>-NN-<slug>.png` in ARTIFACT_DIR, numbered in the order a
> reviewer should read them, and list them at the end of your report as
> `ui-shot: <filename> — <what it shows>` lines. Capture the tour even for
> screens whose items you deferred: the tour documents the change, it does not
> verify it.

The tour is **evidence for humans, not a verdict**: a screen you could not
reach is absent from the tour and mentioned in the report; it never becomes a
FAIL by itself.

### 7. Reconcile the result onto the PR

1. **Tick the boxes that passed** — passing items only; deferred and failed
   items keep their empty box, and a ticked box is never un-ticked:

   ```bash
   gh pr view "$PR_NUM" --repo "$REPO" --json body -q .body > "$ARTIFACT_DIR/body.md"
   printf '%s\n' "$PASSED_ITEMS" | "$AA" checklist tick "$ARTIFACT_DIR/body.md" > "$ARTIFACT_DIR/body.new.md"
   gh pr edit "$PR_NUM" --repo "$REPO" --body-file "$ARTIFACT_DIR/body.new.md"
   ```

2. **Put the tour in the PR description** (only when §1b found tour Surfaces
   and the subagent returned `ui-shot:` lines). This runs AFTER the box-ticking
   push and re-reads the body from GitHub, so the two edits cannot clobber each
   other. The section is rewritten in place, so a re-verify round refreshes the
   tour instead of stacking a new copy under the old one:

   ```bash
   gh pr view "$PR_NUM" --repo "$REPO" --json body -q .body > "$ARTIFACT_DIR/body.shots.md"
   "$AA" evidence shots "$ARTIFACT_DIR" \
     | "$AA" evidence inject "$ARTIFACT_DIR/body.shots.md" > "$ARTIFACT_DIR/body.shots.new.md"
   gh pr edit "$PR_NUM" --repo "$REPO" --body-file "$ARTIFACT_DIR/body.shots.new.md"
   ```

   **A screenshot failure is never a verification failure.** It changes the
   `screenshots:` line and nothing else.

3. **Post exactly one evidence comment for the round** — never one per item.
   Head it `### Manual verification — round <M>/<MAX>` when the caller gave a
   round. It lists, per item: the verdict, the classification, the concrete
   evidence (status codes, request lines, log excerpts, screenshot filenames),
   and for a deferral the demanded post-deploy spec or the named hardware
   blocker. It cites `ARTIFACT_DIR` and, when the sandbox was DEGRADED, says so.

   ```bash
   gh pr comment "$PR_NUM" --repo "$REPO" --body-file "$ARTIFACT_DIR/comment.md"
   ```

4. **Emit the summary lines** as your final output — the machine-readable
   contract the caller parses. Write them in your own assistant message; never
   `echo` them from Bash and call that the answer:

   ```
   manual-verify: <pass>/<total> PASS, <deferred> deferred, <fail> FAIL
   screenshots: <n> posted | PARTIAL — <n>/<total> | SKIPPED — <reason> | none (no UI Surface touched)
   ```

   `<total>` is the number of items this round acted on. An unjustified deferral
   counts as a **FAIL** — the subagent already classified it that way. The
   `screenshots:` line is informational and never changes the verdict; the
   `manual-verify:` line stays first and unchanged in shape.

### 8. Teardown — UNCONDITIONALLY, on pass, fail or error

```bash
"$AA" verify-boot down --pr "$PR_NUM" --head   # every launched app, then the environment
git checkout -                            # never strand the checkout on the PR branch
```

Teardown runs however the round ended — success, any FAIL, a boot abort, or a
mid-round crash. Structure the round so this always executes (a `trap` on EXIT).
`verify-boot down` is idempotent, so it is safe even when nothing was ever
booted. Leave the evidence directory in place: its path was cited in the
comment.

## Hard rules

- **Never fabricate a verdict.** If the environment never booted there are zero
  item verdicts — say so (`manual-verify: infra-error …`); never invent
  PASS/FAIL for items nobody ran.
- **Only tick what passed.** Deferred and failed items keep their empty box.
  Never un-tick a box. A re-run re-verifies only the still-unchecked items.
- **One comment per round.** Not one per item, not one per re-run of an item.
- **Teardown is not optional.** No exit path may leave the environment, an app
  or a browser profile behind.
- **Report DEGRADED, never hide it.** A sandbox fallback, a Surface you could
  not reach, a failed launch: each belongs in the evidence comment and the
  result lines.
- **Install nothing.** A missing prerequisite is an infrastructure finding to
  report, not something to `apt-get` your way out of.
- **You do not verify.** The subagent tests; you orchestrate and reconcile. Do
  not substitute your own judgement for a missing verdict.
- **Never merge the PR, and never push code.** This round edits the PR body,
  posts one comment and nothing else; merge stays human-gated.

## Demo

Against the fixture Target Project, without a PR: boot it, drive its browser
Surface, and see the pieces this round is made of.

```bash
"$AA" surfaces list plugin/fixtures/target-project
printf 'app/server.py\n' | "$AA" surfaces tour plugin/fixtures/target-project
"$AA" verify-boot up --pr 0 plugin/fixtures/target-project
"$AA" verify-boot down --pr 0 plugin/fixtures/target-project
```
