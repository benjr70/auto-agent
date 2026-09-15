# auto-agent

A reusable autonomous agent harness: a Daemon that picks up `AFK` issues in a
Target Project and drives each one to a merged PR. Extracted from
Smart-Smoker-V2. Vocabulary is in `CONTEXT.md`; decisions are in `docs/adr/`.

## Layout

- `bin/auto-agent`: the CLI engine Setup drives (ADR 0009).
  `check-config <target-dir>` validates a Target Project's Harness config;
  `show-config <target-dir>` prints the resolved JSON every lib reads.
- `lib/`: the bash libs the Daemon runs from, each with a `*.test.sh` suite.
  `harness-config.sh` is the only reader of `.auto-agent/harness.json`.
- `plugin/`: the Claude Code plugin a Fire loads with `--plugin-dir` (ADR 0001).
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
