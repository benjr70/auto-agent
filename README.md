# auto-agent

A reusable autonomous agent harness: a Daemon that picks up `AFK` issues in a
Target Project and drives each one to a merged PR. Extracted from
Smart-Smoker-V2. Vocabulary is in `CONTEXT.md`; decisions are in `docs/adr/`.

## Layout

- `bin/auto-agent`: the CLI engine Setup drives (ADR 0009).
  `check-config <target-dir>` validates a Target Project's Harness config;
  `show-config <target-dir>` prints the resolved JSON every lib reads;
  `fire [--dry-run | --resolve-dry-run <N>] [<target-dir>]` runs one Fire;
  `pick-publish`, `labels-ensure` and `vendored-skills` are the libs the
  planning skills and Setup call.
- `lib/`: the bash libs the Daemon runs from, each with a `*.test.sh` suite.
  `harness-config.sh` is the only reader of `.auto-agent/harness.json`;
  `host-env.sh` reads the Host env; `fire.sh` is the Fire wrapper;
  `rate-limits-tap.sh` records the stream's rate-limit events;
  `fire-record.sh` writes the Fire record; `runbook-check.sh` asserts the
  plugin's skills still carry their load-bearing rules and no Target Project
  literal; `pick-publish.sh` puts an AFK ticket on (or takes it off) whatever
  pick signal the Harness config declares; `labels-ensure.sh` creates the
  harness labels create-if-missing; `vendored-skills.sh` checks and syncs
  the vendored upstream skills against their pinned commit. `testdata/`
  holds canned streams.
- `plugin/`: the Claude Code plugin a Fire loads with `--plugin-dir` (ADR 0001).
  `.claude-plugin/plugin.json` is the manifest; `skills/` the namespaced
  `/auto-agent:<name>` skills (the core lane: `afk-pickup`, `afk-dispatch`,
  `pr-watch`, `pr-review`, `pr-reconcile`; the resolve lane: `afk-resolve`;
  the planning skills: `wayfinder`, `to-spec`, `to-tickets`; the vendored
  upstream skills `research`, `grilling` and `domain-modeling`, copied from
  mattpocock/skills at the commit `vendored-skills.json` pins; plus the no-op
  `dry-run`);
  `agents/` the `auto-agent:implementer`, `auto-agent:reviewer` and
  `auto-agent:verifier` subagents; `hooks/` the `smoke-trailer` and
  `review-gate` Stop hooks; `settings/baseline.json` the `--settings`
  baseline (env, permissions, deny list) every Fire carries;
  `schema/` holds the Harness config JSON schema and its jq validator;
  `fixtures/target-project/` is the fixture Target Project the harness tests
  itself against.
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
  loaded, result, last rate-limit event, Gate verdict). The Dashboard's input.
- `rate-limits.json` / `rate-limits.jsonl`: the last and every
  `rate_limit_event` the tap saw (ADR 0008 addendum).
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
window, and, when claude exits non-zero, clears the single-flight lock the
Fire took (a pick goes `AFK:in-progress` to `AFK:failed` with a comment; a
reconcile restores `AFK:done`). The pause-on-exhaustion path arrives with the
budget-gate Slice.

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
```
