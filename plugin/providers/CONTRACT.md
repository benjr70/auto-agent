# The Environment provider contract

Your project brings its own way of booting itself; the harness brings the
verification round. The seam between them is **one executable in your
repository** — the Environment provider — with three subcommands. The Daemon
never learns how your environment comes up, and you never learn how the
checklist round works.

This document is the whole contract. It is enforced by
`bin/auto-agent provider-check`, and every rule below is one that check
makes: if your provider passes it, the harness can verify your PRs.

Decision: [ADR 0003](../../docs/adr/0003-environment-provider-contract.md).

## Declaring it

In your `.auto-agent/harness.json`:

```json
{
  "verification": {
    "hermetic": { "command": "verify/provider", "smoke": true }
  },
  "surfaces": {
    "web": { "kind": "browser", "url_key": "WEB_URL", "paths": ["apps/web/**"] },
    "api": { "kind": "api",     "url_key": "API_URL", "paths": ["apps/api/**"] }
  }
}
```

`command` is resolved from the root of your checkout and must be executable.
No `verification.hermetic` block at all is the **Bootstrap state**: the Daemon
keeps working your tickets, every Agent PR it opens is labelled
`AFK:verify-human` for a human to verify, and the Dashboard warns until a
provider is merged. That is a starting point, not a mode to stay in.

## The three subcommands

### `up --pr N`

Boot an environment for PR `N` and **return only when it is healthy**.
Readiness is yours: the harness has no health-target table, no retry budget
and no timeout of its own to wait on.

- **stdout is the key block and nothing else.** Progress, logs and diagnostics
  go to stderr.
- Each stdout line is `KEY=value`: the key is an uppercase shell identifier
  (`[A-Z][A-Z0-9_]*`), the value runs to end of line and may contain `=`.
- The block must carry the `url_key` of **every** Surface declared in the
  Harness config. A missing one is an infra-error in a live round.
- Exit **0** healthy, **3** a prerequisite is missing and nothing was booted,
  **4** the boot failed.

The harness gives you exactly one retry: on exit 4 it runs `down --pr N` and
calls `up --pr N` once more. Exit 3 is not retried — a missing prerequisite
will still be missing a second later.

### `down --pr N`

Tear down PR `N`'s environment. **Idempotent, and always exit 0**: the harness
calls it before the first `up` (to clear anything a killed Fire left behind),
between the one retry, and on every exit path afterwards. Nothing to stop is a
success, not an error.

### `smoke`

Only when `hermetic.smoke` is `true`. Run against the environment `up` just
booted, with **the whole key block exported into your environment**: if `up`
printed `API_URL=…`, `smoke` reads `$API_URL`. It never runs before a healthy
`up`.

- Its **last stdout line** is `smoke: PASS (n/n)` or `smoke: FAIL (<detail>)`.
- Exit **0** pass, **1** fail, **2** could not run.

### `status` (deployed tier only)

If you declare `verification.deployed`, the same executable answers `status`:
the same `KEY=value` block for a live environment, exit **0** healthy,
**1** unhealthy, **3** prerequisite missing. No `up`, no `down` — the deployed
tier is read-only.

## Checking it

```sh
bin/auto-agent provider-check [--pr <N>] <target-dir>
```

It drives, in this order:

1. `down --pr N` — before the first `up`
2. `up --pr N` — retried once, after another `down`, if it exits 4
3. the block grammar — every line `KEY=value` with an uppercase identifier key
4. the `url_key`s — every declared Surface's key is in the block
5. `smoke` — when `hermetic.smoke` is true, with the block exported
6. `down --pr N` — after the run

`--pr` defaults to 0 and only has to be a number nothing else is using; the
check never touches GitHub. Progress goes to stderr; the last line of stdout
is exactly one verdict:

```
provider-check: PASS — verify/provider conforms (6 checks, pr 0)
provider-check: FAIL — surface api declares url_key API_URL, which the up block does not carry
provider-check: BOOTSTRAP — the Harness config declares no hermetic tier (Bootstrap state): no Environment provider to check
```

Exit codes:

| code | meaning |
| ---- | ------- |
| 0 | the provider conforms |
| 1 | a contract violation; the verdict names it |
| 2 | usage error, or no Harness config could be resolved |
| 3 | no hermetic tier declared (Bootstrap state): nothing to check, so the verdict reads `BOOTSTRAP`, not `FAIL` |
| 4 | this machine could not boot the environment (`up` exited 3, or 4 twice, or `smoke` exited 2) — your provider may still be conformant |

The 1/4 split is the one to read carefully: **1 means fix your provider, 4
means fix the machine** (Docker not installed, a port in use, an image that
will not pull).

Every failure the check can report, and what it means:

| verdict | cause |
| ------- | ----- |
| `the hermetic command <cmd> is not an executable file under <dir>` | `hermetic.command` does not resolve, or is not `chmod +x` |
| `down before the first up exited <rc>, want 0 (down is idempotent)` | `down` failed with nothing to stop |
| `down between the one retry exited <rc>, want 0` | the same, on the retry path |
| `down after up exited <rc>, want 0 (down is idempotent)` | `down` failed against an environment it had just booted |
| `up exited 3 (prerequisite missing): <stderr>` | this machine is missing something the boot needs (check exit 4) |
| `up exited 4 (boot failed) on both attempts: <stderr>` | the boot failed twice, with a `down` between (check exit 4) |
| `up exited <rc>, want 0 healthy, 3 prerequisite missing or 4 boot failed` | an exit code outside the contract |
| `up printed no keys` | a healthy `up` with an empty stdout |
| `up printed a line that is not KEY=value: <line>` | progress written to stdout instead of stderr |
| `up printed a key that is not an uppercase shell identifier: <key>` | a lower-case, dashed or digit-leading key |
| `surface <name> declares url_key <KEY>, which the up block does not carry` | a Surface the round could not reach |
| `smoke exited 0 but its last stdout line is not 'smoke: PASS (…)': <line>` | the trailer line is missing or not last |
| `smoke exited 1 but its last stdout line is not 'smoke: FAIL (…)': <line>` | the same, on a failure |
| `smoke exited 1: <line>` | the smoke really failed |
| `smoke exited 2 (could not run): <stderr>` | the smoke could not be executed here (check exit 4) |
| `smoke exited <rc>, want 0 pass, 1 fail or 2 could not run` | an exit code outside the contract |

## The two reference providers

- **Single process**: [`../fixtures/target-project/verify/provider`](../fixtures/target-project/verify/provider).
  One `python3` process on a port from the PR number. The smallest thing that
  conforms, and the harness's own test fixture.
- **Compose**: [`compose/`](compose/). One compose project per PR, ports from
  the PR block, health-waited. The shape most projects start from: copy the
  directory in, point `docker-compose.yml` at your services, rename the keys.

Both are driven by `provider-check` in `lib/provider-check.test.sh`, so
neither can drift from this document.

## `provider-lib.sh`

Optional. [`provider-lib.sh`](provider-lib.sh) is sourceable helpers for a
bash provider — nothing in the harness requires it, and a provider in any
language is judged only by the rules above.

| function | what it gives you |
| -------- | ----------------- |
| `provider_need <cmd>…` | returns **3**, the contract's prerequisite code, naming what is missing |
| `provider_pr_arg "$@"` | the `--pr N` (or `--pr=N`) value |
| `provider_port_block <pr> [stride] [base]` | a port block no other PR shares (PR numbers wrap at 1000) |
| `provider_compose_project <prefix> <pr>` | `<prefix>-pr-<N>`, folded to the characters compose accepts |
| `provider_wait_healthy <url> [secs] [interval]` | a **bounded** health wait, so a bad boot fails instead of hanging a Fire |
| `provider_key <KEY> <value>` | one block line, refusing a key the harness could not export |

Source it from the Harness install, or copy it in beside your provider:

```sh
. "${AUTO_AGENT_ROOT:?}/plugin/providers/provider-lib.sh"   # from the install
. "$(dirname "$0")/provider-lib.sh"                         # copied in
```

## Writing one from nothing

If your Harness config has no hermetic block, Setup opens one `AFK` issue
asking for exactly this, and the Daemon writes the provider itself. The
verification round reads `harness.json` from the **PR head**, so the provider
PR is verified by the provider it adds: its first green hermetic round is the
evidence that closes the Bootstrap state, and the Dashboard warning clears
when it merges.
