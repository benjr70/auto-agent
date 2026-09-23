# auto-agent

A reusable autonomous agent harness: a Daemon that picks up `AFK` issues in a
Target Project and drives each one to a merged PR. Extracted from
Smart-Smoker-V2. Vocabulary is in `CONTEXT.md`; decisions are in `docs/adr/`.

## Layout

- `bin/auto-agent`: the CLI engine Setup drives (ADR 0009).
  `check-config <target-dir>` validates a Target Project's Harness config;
  `show-config <target-dir>` prints the resolved JSON every lib reads;
  `fire [--dry-run | --resolve-dry-run <N>] [<target-dir>]` runs one Fire;
  `usage-sensor` prints the Gate verdict for the declared auth mode and
  `park` drives the parked state behind a dead credential;
  `daemon [<target-dir>]` is the Daemon the Daemon unit runs and
  `unit-render daemon|dashboard` renders its systemd units from the Host env;
  `dashboard` serves this Host's read-only status page and `/api/status`;
  `provider-check [--pr <N>] [<target-dir>]` drives a Target Project's
  Environment provider through its contract and prints one verdict;
  `surfaces`, `checklist`, `evidence`, `verify-boot` and `surface-launch` are
  the verification round's own commands, and `deployed` the Deployed tier's;
  `pick-publish`, `labels-ensure` and `vendored-skills` are the libs the
  planning skills and Setup call.
- `lib/`: the bash libs the Daemon runs from, each with a `*.test.sh` suite.
  `harness-config.sh` is the only reader of `.auto-agent/harness.json`;
  `host-env.sh` reads the Host env; `fire.sh` is the Fire wrapper;
  `rate-limits-tap.sh` records the stream's rate-limit events;
  `fire-record.sh` writes the Fire record; `usage-sensor.sh` is the Budget
  gate's one sensor, chosen per auth mode; `exhaustion-classifier.sh` reads
  how a Fire ended; `daemon-park.sh` parks and un-parks the Daemon on
  credential death; `daemon.sh` is the Daemon: the gate, the Fire and the
  Sleep Planner, cycle after cycle (`sleep-planner.sh`); `unit-render.sh`
  renders the systemd units; `runbook-check.sh` asserts the plugin's skills
  still carry their load-bearing rules and no Target Project literal;
  `pick-publish.sh` puts an AFK ticket on (or takes it off) whatever pick
  signal the Harness config declares; `labels-ensure.sh` creates the harness
  labels create-if-missing; `vendored-skills.sh` checks and syncs the vendored
  upstream skills against their pinned commit; `provider-check.sh` is the
  Provider check, the conformance run behind the Environment provider
  contract, over `provider-contract.sh`, the one implementation of ADR 0003's
  driving rules that the check and a round's boot share; the verification
  round's libs are `surfaces.sh` (which Surfaces a
  diff touched, which earn a tour, at what viewport), `checklist.sh` (the PR
  body's items, and the tick of the ones that passed), `evidence.sh` (the
  round's evidence sink), `display-env.sh` (display truth and the Electron
  sandbox mode), `surface-launch.sh` (the launcher a Surface's kind selects)
  and `verify-boot.sh` (the environment and its apps, up and down);
  `deployed-tier.sh` is the Deployed tier (its lane gate, the deferred items,
  the merged PR it works next, and the live `status` block).
  `testdata/` holds canned streams.
- `plugin/`: the Claude Code plugin a Fire loads with `--plugin-dir` (ADR 0001).
  `.claude-plugin/plugin.json` is the manifest; `skills/` the namespaced
  `/auto-agent:<name>` skills (the core lane: `afk-pickup`, `afk-dispatch`,
  `pr-watch`, `pr-review`, `pr-reconcile`; the resolve lane: `afk-resolve`;
  the verification round: `verify-pr`; the Deployed tier: `verify-deploy`;
  the planning skills: `wayfinder`, `to-spec`, `to-tickets`; the vendored
  upstream skills `research`, `grilling` and `domain-modeling`, copied from
  mattpocock/skills at the commit `vendored-skills.json` pins; plus the no-op
  `dry-run`);
  `agents/` the `auto-agent:implementer`, `auto-agent:reviewer`,
  `auto-agent:verifier` and `auto-agent:manual-verifier` subagents; `hooks/`
  the `smoke-trailer` and `review-gate` Stop hooks;
  `settings/baseline.json` the `--settings` baseline (env, permissions, deny
  list) every Fire carries;
  `schema/` holds the Harness config JSON schema and its jq validator;
  `fixtures/target-project/` is the fixture Target Project the harness tests
  itself against; `providers/` is what a maintainer writing an Environment
  provider gets: `CONTRACT.md`, the sourceable `provider-lib.sh` and the
  compose reference provider (the fixture's `verify/provider` is the
  single-process one).
- `dashboard/`: the Dashboard (`server.py`, stdlib Python, and `index.html`);
  its README documents the `/api/status` shape and its Host env keys.
- `infra/systemd/`: the Daemon and Dashboard unit templates Setup installs.
- `run-tests.sh`: runs every `*.test.sh` and `*.test.py` suite; the one entry
  point CI calls.

## Harness config

A Target Project commits one `.auto-agent/` directory (ADR 0002):
`harness.json` for the machine, validated against
`plugin/schema/harness.schema.json` at Setup and before every Fire, and
fixed-name markdown siblings (`verifier-runbook.md`, `bot-pr-checklist.md`,
`deployed-checks.md`) for prose. An optional executable `host-extension` adds
project-specific Host needs at Setup time. The default branch is detected from
GitHub, never declared. See `plugin/fixtures/target-project/.auto-agent/` for
a complete example.

```sh
bin/auto-agent check-config plugin/fixtures/target-project
bash run-tests.sh
```

## A Fire

`bin/auto-agent fire` runs one Daemon pass against a Target Project: it
validates the Harness config (failing closed), resolves it once and exports it
as `HARNESS_CONFIG_JSON` for every lib and skill inside the Fire, resets the
checkout to the tip of the detected default branch, runs `claude -p` with
`--plugin-dir plugin`, `--settings plugin/settings/baseline.json` and
`--output-format stream-json`, pipes the stream through the rate-limit tap,
and writes a Fire record when the Fire ends, failed ones included. The
baseline carries the env, permissions and deny list a plugin cannot; the
Target Project's own `.claude/settings.json` merges on top of it, and the
deny list binds even though a Fire runs with permissions bypassed. Everything
lands in the State dir, `AUTO_AGENT_STATE_DIR` from the Host env
(`~/.config/auto-agent/env`), defaulting to `~/.local/state/auto-agent`:

- `fires/<fire-id>.json`: the Fire record (start, kind, issue, exit, plugin
  loaded, result, last rate-limit event, outcome, Gate verdict). Written once
  before claude runs (`endedAt` and `exit` null: the Fire in flight) and again
  when it exits. The Dashboard's input.
- `daemon-state.json`: what the Daemon is doing now (state, the log line, the
  reset it sleeps to, the fail count), rewritten at every step so the
  Dashboard never parses the journal.
- `rate-limits.json` / `rate-limits.jsonl`: the last and every
  `rate_limit_event` the tap saw (ADR 0008 addendum).
- `usage-sensor.json`: the usage sensor's memory (the last good endpoint
  verdict for the stale hold, the 403 mark, the model switch and its reset).
- `parked.json`: present while the Daemon is parked on a dead credential.
- `logs/<fire-id>.stream.jsonl` and `.stderr.log`: the raw transcript.

The prompt is `/auto-agent:afk-pickup`, the core lane's entry skill: one unit
of work per Fire, in strict order (reconcile an Agent PR that needs attention,
resume paused work, pick the next `AFK` ticket, route a Decision ticket), then
`/auto-agent:afk-dispatch` implements the pick as a single-agent implementer
with a TDD loop and the reviewer and verifier subagents, and the pickup skill
opens the PR and drives it through `/auto-agent:pr-watch` (CI), the one-time
`/auto-agent:pr-review` and the verification round. An Agent PR is never
merged by the machine. Every skill reads the repo, the default branch, the
commit scopes, the commands and the round caps from the Harness config;
`bin/auto-agent runbook-check` fails the suite if a skill regains a repo slug,
a `master`, a port or an app name, or loses a load-bearing rule.

The wrapper scrapes the skill's stable lines (`picked:   #N`, `picked:
reconcile PR #P (issue #N)`, `resolve: #N`, `afk-pickup: no eligible issue`,
`afk-pickup: skip …`) into the record's `work` block, prints
`AGENT_RUN_NO_WORK=1` when the queue was empty so the Daemon sleeps out the
window, and classifies every non-zero exit through the exhaustion classifier
(`lib/exhaustion-classifier.sh`), whose verdict the record carries as
`outcome`. A `rejected` rate-limit event tapped during the Fire is the
authoritative signal; the documented limit strings (`You've hit your
session/weekly/<model> limit … resets …`) stay as the text-mode fallback;
`authentication_failed` is credential death. An EXHAUSTED Fire is paused,
not failed: a pick freezes partial work in a `wip:` commit and moves its lock
`AFK:in-progress` to `AFK:paused` (the branch stays for resume), a reconcile
restores `AFK:done`, a resolve drops its lock and research branch so the next
Fire restarts it; the wrapper prints `AGENT_RUN_RESET_AT=<iso>` and exits 0.
When the limit was per-model it prints `AGENT_RUN_MODEL_LIMIT=<scope>` and an
empty reset instead, so the Daemon re-gates at once and the next verdict
switches the model rather than sleeping out a week.
An AUTH_DEAD Fire is paused the same way and parks the Daemon (below), with
`AGENT_RUN_AUTH_DEAD=1`. A FAILED Fire clears the lock it took (a pick goes
`AFK:in-progress` to `AFK:failed` with a comment; a reconcile restores
`AFK:done`).

## The Budget gate

`bin/auto-agent usage-sensor` is the Budget gate's one sensor, chosen per
auth mode and never guessing (ADR 0008 and addendum; the clock-based time
proxy is gone). It reads `CLAUDE_AUTH_MODE` from the Host env, cross-checks
it against the secrets present and `claude auth status` (failing loud with
exit 5 on a mismatch), and prints one Gate verdict: `authMode`, `sensor`
(`usage-endpoint | stream-events | limit-strings | spend | none`), `state`
(`ok | stale | unavailable | auth-dead`), `remainPct`, `resetAt`,
`shouldFire` (always a boolean), `observedAt`, `limits[]` (`scope` is
`session`, `weekly` or a model family, `utilization`, `resetsAt`),
`warnings[]`, plus `fireModel` and `fireModelUntil` from the model policy.
The Daemon fires on `shouldFire` and hands the verdict to the Fire through
`AUTO_AGENT_GATE_VERDICT_FILE`, so the same object lands in the Fire record's
`gate` block; the Dashboard shells to the sensor rather than re-implementing
it.

- **`login`**: the usage endpoint is the pre-Fire sensor. A 429, 5xx or
  network failure keeps the last good verdict as `stale` for 60 minutes
  (`AUTO_AGENT_GATE_STALE_MAX_SECS`); beyond that, and after a 403 (which
  marks the endpoint unavailable for the Daemon process,
  `AUTO_AGENT_DAEMON_ID`), the Host behaves like a setup-token Host.
- **`setup-token`**: fires optimistically. The last tapped `rate_limit_event`
  seeds the verdict (`stream-events`, `stale`, `observedAt` from that Fire,
  the unnamed per-model window keyed to the model that fired); a `rejected`
  event on an account window sets `resetAt` and holds the Fire until then; the
  last Fire's limit-string outcome does the same when it is newer. With no
  un-expired limit verdict the sensor is `none` and `shouldFire` is true.
- **`api-key`**: accepted by the schema, refuses to start (exit 6, sensor
  `spend`) until spend pacing exists.

Per-model limits never gate. Under the model policy (`AUTO_AGENT_MODEL_PRIMARY`,
default `fable`; `AUTO_AGENT_MODEL_FALLBACK`, default `opus`, empty to never
switch; `AUTO_AGENT_MODEL_SWITCH_PCT`, default 95) a spent primary sets
`fireModel`, the wrapper passes it as `--model` unless `AUTO_AGENT_FIRE_MODEL`
pins one, and the switch is remembered in `usage-sensor.json` until the
limit's reset. `AUTO_AGENT_GATE_MIN_PCT` (default 25) is the fire threshold.

**Credential death** (401 from the endpoint, `claude auth status` exiting 1,
or `authentication_failed` in a Fire) is never exhaustion: the sensor exits 4
with `state: auth-dead`, and the Daemon **parks** (`bin/auto-agent park`):
`parked.json` in the State dir, one reused `AFK:needs-human` issue open in the
Target Project (matched by its body marker), `park reprobe` re-probing
`claude auth status` hourly and un-parking (closing the issue) when it
passes, so re-running `/login` over SSH is the whole fix. A Fire that dies
on its credential is paused like an exhausted one and parks itself.

```sh
CLAUDE_AUTH_MODE=login bin/auto-agent usage-sensor | jq .
bin/auto-agent park status
bin/auto-agent park reprobe
```

`fire --dry-run` prompts `/auto-agent:afk-pickup --dry-run`, against the
fixture Target Project when no target is given: the skill reaches its pick
verdict and prints `afk-pickup: would-pick #N …` (or `no eligible issue`)
without a GitHub or git write, and the wrapper exits 0 only when the stream's
init event lists the plugin and the skill and such a line was printed. It is
the Fire seam Setup's verify stage and the harness's own tests demo on.
`fire --noop` runs the no-op `/auto-agent:dry-run` skill instead, which proves
the plugin loads without touching GitHub at all.

`fire --resolve-dry-run <N>` prompts `/auto-agent:afk-resolve --issue <N>
--dry-run`, the resolve lane's dry run: it reads Decision ticket `<N>` and its
Map, runs the vendored `research` skill and writes the findings file under the
config's research prefix in the checkout, with no GitHub or git write (no
claim, branch, PR, comment or close), and ends on `afk-resolve: would-open PR
research/<slug> (<path>)`. Any `wayfinder:research` ticket with a Map parent
will do, closed ones included. A real resolve Fire is what `/auto-agent:afk-pickup`
runs when the pick is a Decision ticket: research, a `research/<slug>` branch,
a `docs(research): …` PR driven green by `/auto-agent:pr-watch`, the docs-only
gate's own merge command, the resolution comment, the close, the Map append
and fog graduation. A resolve is never paused: the wrapper restarts it.

The planning skills (`/auto-agent:wayfinder`, `/auto-agent:to-spec`,
`/auto-agent:to-tickets`) run interactively against any Target Project and
publish through the same Harness config: `bin/auto-agent labels-ensure`
creates the harness labels, and `bin/auto-agent pick-publish` puts an AFK
ticket on the pick signal, Project plus Priority when the `pick` block names a
Project, nothing (the `AFK` label already is the signal) when it is
label-only.

```sh
AUTO_AGENT_STATE_DIR=/tmp/aa-state bin/auto-agent fire --dry-run
AUTO_AGENT_STATE_DIR=/tmp/aa-state bin/auto-agent fire --resolve-dry-run 3
AUTO_AGENT_STATE_DIR=/tmp/aa-state bin/auto-agent fire --noop
bin/auto-agent runbook-check
bin/auto-agent vendored-skills check --upstream
bin/auto-agent provider-check plugin/fixtures/target-project
```

## The Daemon

`bin/auto-agent daemon` is the Daemon: one per Target Project per Host, run by
systemd from the Harness install, knowing the Target Project only through the
Host env (`AUTO_AGENT_TARGET_DIR`) and the Harness config each Fire loads.
Each cycle reads the Gate verdict from `usage-sensor`, keeps it as
`gate-verdict.json` in the State dir and hands it to the Fire
(`AUTO_AGENT_GATE_VERDICT_FILE`), then:

- fires (`bin/auto-agent fire`, one Fire record per Fire) when the verdict
  says so, and goes straight back to the gate after a clean Fire;
- sleeps to the verdict's reset when it does not, then polls the gate (the
  Sleep Planner, carried over unchanged);
- after an empty queue (`AGENT_RUN_NO_WORK=1`) sleeps in Work Probe chunks
  and wakes early when work appears; after an exhausted Fire sleeps to the
  Fire's reset; after a per-model limit re-gates at once so the model policy
  switches; after a failed Fire probe-sleeps, going deaf to the reset after
  `AUTO_AGENT_DAEMON_FAIL_CAP` (3) failures in a row;
- on a dead credential parks, then only re-probes hourly until `/login` is
  re-run; on a mode mismatch holds off and re-gates hourly; in api-key mode
  exits 6 and stays down.

Within the Target Project the `AFK:in-progress` label is the single-flight
lock; a `flock` on `daemon.lock` in the State dir stops a second Daemon on the
same State dir (exit 7). The Daemon writes nothing into the checkout.

`bin/auto-agent unit-render daemon|dashboard [--out <dir>]` renders
`infra/systemd/auto-agent-<name>.service.in` from the Host env: `User` from
`AUTO_AGENT_HOST_USER`, `EnvironmentFile` the Host env itself, `ExecStart`
from this install, `PATH` from `AUTO_AGENT_UNIT_PATH`, `MemoryMax` from
`AUTO_AGENT_MEMORY_MAX` (8G) and `AUTO_AGENT_DASHBOARD_MEMORY_MAX` (512M).
Setup's configure step installs the result; `systemd-analyze verify` passes on
both. `AUTO_AGENT_FIRE_MODEL` in the Host env pins every Fire's model and so
overrides the model policy's switch; leave it unset to let the gate switch.
The Dashboard unit runs `bin/auto-agent dashboard` (see
[`dashboard/README.md`](dashboard/README.md)): it binds
`AUTO_AGENT_DASHBOARD_BIND` (loopback by default) on
`AUTO_AGENT_DASHBOARD_PORT` (8090), and `AUTO_AGENT_DASHBOARD_SUMMARY=off`
turns off the Haiku summary of the Fire in flight.

```sh
AUTO_AGENT_STATE_DIR=/tmp/aa-state AUTO_AGENT_DAEMON_FIRE_ARGS=--dry-run \
    DAEMON_MAX_CYCLES=1 bin/auto-agent daemon plugin/fixtures/target-project
bin/auto-agent unit-render daemon --out /tmp/aa-units
systemd-analyze verify /tmp/aa-units/auto-agent-daemon.service
```

## The Environment provider

A Target Project brings its own environment behind one executable with
`up --pr N`, `down --pr N` and `smoke` (ADR 0003); the harness owns the round.
The whole contract, its exit codes and every verdict the check can print are
in [`plugin/providers/CONTRACT.md`](plugin/providers/CONTRACT.md), beside the
two reference providers and `provider-lib.sh`.

`bin/auto-agent provider-check [<target-dir>]` is the conformance run: it drives
`down`, `up --pr N`, the key block, the declared Surfaces' `url_key`s, `smoke`
and `down` again, and prints one verdict. It answers "does my provider
conform" with no checklist round, no PR and no Claude, which is what Setup's
verify stage asks and what a maintainer writing a provider iterates against.
A Target Project with no hermetic block at all is in the Bootstrap state
(exit 3): the Daemon still works its tickets, and the provider is the first
thing it is asked to write.

## The verification round

`/auto-agent:verify-pr` is one manual-verification round against an open Agent
PR: the PR's checklist parsed, the environment booted through the Environment
provider, every unchecked item exercised on the declared Surfaces by the
`auto-agent:manual-verifier` subagent, a screenshot tour of every touched UI
Surface posted into the PR description, the boxes that passed ticked, one
evidence comment, and the terminal line the caller reads:

```
manual-verify: <pass>/<total> PASS, <deferred> deferred, <fail> FAIL
screenshots: <n> posted | PARTIAL — <n>/<total> | SKIPPED — <reason> | none (no UI Surface touched)
```

The round reads `.auto-agent/harness.json` from the **PR head** (ADR 0007), so
a PR that adds an Environment provider is verified by the provider it adds:
every round command takes `--head`, which reads the checkout the round is
standing in rather than the config the Fire resolved from the default branch
and exported. The ADR 0003 driving rules themselves (the `down` before the
first `up`, the one retry, the block grammar, the `url_key`s) live in
`lib/provider-contract.sh`, which the Provider check and the round's boot both
call, so the check and the round cannot drift apart about what conformance is.

What the harness owns, and the Target Project only declares:

- **which Surfaces a diff touched** — `bin/auto-agent surfaces touched --pr <N>`
  matches the changed paths against each Surface's `paths` globs;
- **which of them earn a screenshot tour** — `surfaces tour` narrows that to the
  `browser` and `electron` kinds, which ALWAYS earn one when touched; `cli` and
  `api` Surfaces are evidence-only (ADR 0003), and no declaration can opt a UI
  Surface out;
- **the capture shape** — `surfaces viewport <name>`, from the Surface's
  `viewport` (a project whose users hold a phone or watch a fixed panel declares
  that shape; the default only keeps a tour from having no shape at all);
- **the checklist protocol** — `checklist parse` reads the unchecked items of
  the two verification sections, `checklist tick` flips only the ones that
  passed, never un-ticks, and never touches a checkbox elsewhere in the body;
- **the evidence sink** — `evidence dir --pr <N> --round <M>` is the round's
  directory in the State dir, `evidence name`/`shots` the screenshot naming, and
  `evidence inject` rewrites the PR body's `## Screenshots` section in place, so
  a re-verify round refreshes the tour instead of stacking a new copy;
- **the boot** — `verify-boot up --pr <N> [--surface <name>]... --head` runs `down`
  before the first `up`, retries a failed boot exactly once, checks the block
  and every declared `url_key`, launches the app of each `electron` Surface, and
  prints the block the round exports. `verify-boot down --pr <N>` is the
  teardown that runs on every exit path;
- **the launchers** — `surface-launch mcp <surface>` is what an MCP entry
  points at, and the Surface's kind chooses what it becomes: a headful browser
  on the Host display with a fresh profile per run, or an attach to the app's
  debugging endpoint. `surface-launch mcp-config` renders that registry from
  the config — Surface names are per Target Project, so it cannot be a static
  file — and the Fire wrapper passes it to every session as `--mcp-config`.

Display truth is a plain `DISPLAY` in the Host env — Xvfb on the reference Host
(ticket #19), no session globbing, and never a headless fallback: no display is
an infra finding the round reports. On a Host that restricts unprivileged user
namespaces, an `electron` Surface's app needs an AppArmor profile granting them
to its binary (Setup writes it). Without one the app starts with
`ELECTRON_DISABLE_SANDBOX=1` and the round says `sandbox: DEGRADED — …`, in the
block, the evidence comment and the result line: degraded is reported, never
silent, and never a reason to skip the round.

```sh
printf 'app/server.py\n' | bin/auto-agent surfaces tour plugin/fixtures/target-project
bin/auto-agent verify-boot up --pr 0 plugin/fixtures/target-project
bin/auto-agent verify-boot down --pr 0 plugin/fixtures/target-project
```

## The Deployed tier

A hermetic round defers what only a real deployment can prove, and demands a
`<!-- post-deploy: … -->`-tagged checklist item for it. The Deployed tier runs
those items after the PR merges, read-only, against a live environment, when
the Target Project declares it:

```json
"verification": {
  "hermetic": { "command": "verify/provider", "smoke": true },
  "deployed": { "command": "verify/provider", "enabled": true }
}
```

The lane is on only when the block exists and `enabled` is not false (an
omitted `enabled` is on). Declared with `enabled: false`, every Fire record
carries the note `deployed-lane: off — verification.deployed.enabled is false`
in its `notes`; not declared, the lane is never asked and no merged PR is ever
listed. It is optional, so it only fills a Fire that would otherwise be idle:
the pickup triage's `deployed` verdict names the oldest merged Agent PR with
unchecked post-deploy items, a round left under `rounds.manual_verify`, and
30 minutes (`DEPLOYED_TIER_WAIT_MINS`) since it merged and since its last
round — time for its deploy to land, and no spending every round inside one
Daemon cycle.

`/auto-agent:verify-deploy` is the round: the same checklist protocol, verifier
core and evidence sink as `verify-pr`, over the deferred items only. The
deployed command resolves its own targets and answers `status` with the same
`KEY=value` block (exit 0 healthy, 1 unhealthy, 3 prerequisite missing); the
tier calls `status` and **never `up` or `down`**. One comment per round, headed
`### Deployed verification — round <M>/<MAX>`, and one terminal line:

```
deployed-verify: <pass>/<total> PASS, <deferred> deferred, <fail> FAIL — round <M>/<MAX> [— EXHAUSTED]
```

```sh
bin/auto-agent deployed lane plugin/fixtures/target-project     # off: the fixture ships enabled false
bin/auto-agent deployed items body.md                           # the deferred items of a PR body
bin/auto-agent deployed pick|status <target-dir>                # the next merged PR; the live block
```

## The deps-land lane

Dependabot PRs land behind their gate, with no human, when the Target Project
declares a `dependabot` block:

```json
"dependabot": { "enabled": true }
```

The lane is on only when the block exists and `enabled` is not false. Without
it, the pickup triage never hands a Fire a Dependabot PR and the reconcile
order skips them. With it, a Bot PR ranks below every Agent PR, and
`/auto-agent:deps-land` drives one per Fire to a terminal state:

1. retitle a security bump `fix(deps):` (`deps-lane retitle`);
2. append the Target Project's `bot-pr-checklist.md` sibling to the PR body,
   verbatim, inside the harness's `<!-- bot-pr-checklist v1 -->` markers. The
   file must carry at least one `- [ ]` item under a `## Manual verification`
   heading, or the lane refuses it;
3. Tier A: CI green through `/auto-agent:pr-watch --bot`;
4. Tier B: one `/auto-agent:verify-pr --force-tour` round, passing only at
   `n/n PASS, 0 deferred, 0 FAIL`;
5. the deps gate (`bin/auto-agent deps-gate`), whose approving verdict carries
   the shared admin-squash merge command the pickup skill runs, checked
   against the config's `required_checks`.

Either tier's failure spends one fix attempt from a budget of
`rounds.deps_fix` in total, counted from sha-keyed marker comments on the PR so
a crash never refunds one; a fix commit carries `[dependabot skip]`, and a
conflicting bump is either nudged (`@dependabot rebase`) or rebased by the
Rebase Driver with the config's `commands.lockfile_refresh`. When the budget is
spent the PR is parked (drafted, labelled `AFK:deps-failed`, commented once)
by `deps-lane park`, whichever tier ran out. A major bump is never merged on
machine evidence: it gets `HITL` and waits for an approving review.

```sh
bin/auto-agent deps-lane lane plugin/fixtures/target-project    # off: the fixture declares no block
bin/auto-agent deps-lane inject-checklist [<checklist>] < body  # the body with the checklist unit
bin/auto-agent deps-lane park <pr> <sha> "<last failure>"       # the idempotent exhaustion park
```
