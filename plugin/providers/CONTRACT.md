# The Environment provider contract

Your project brings its own way of booting itself; the harness brings the
verification round. The seam between them is **one executable in your
repository** — the Environment provider — with three subcommands, `down`
taking a PR or `--all`. The Daemon
never learns how your environment comes up, and you never learn how the
checklist round works.

This document is the whole contract. Every rule about `up`, `down` and
`smoke` below is one `bin/auto-agent provider-check` makes: if your provider
passes it, the harness can verify your PRs. The deployed tier's `status` is
the one part the check does not drive: `bin/auto-agent deployed status` does.

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
- Each non-empty stdout line is `KEY=value`: the key is an uppercase shell identifier
  (`[A-Z][A-Z0-9_]*`), the value runs to end of line and may contain `=`.
- The block must carry the `url_key` of **every** Surface declared in the
  Harness config. A missing one is an infra-error in a live round.
- Exit **0** healthy, **3** a prerequisite is missing and nothing was booted,
  **4** the boot failed.

The harness gives you exactly one retry: on exit 4 it runs `down --pr N` and
calls `up --pr N` once more. Exit 3 is not retried — a missing prerequisite
will still be missing a second later.

### `down --pr N`

Tear down PR `N`'s environment: **leave nothing on the Host that `up --pr N`
created for that PR** — its containers, networks, volumes and the images it
built. Shared things (pulled base images, the build cache, package caches) are
not the PR's: leave them.

**Idempotent**: nothing to stop is a success, exit **0**. Exit **non-zero**
only when something of PR `N`'s is still there afterwards, never to say there
was nothing to do. The harness calls it between the one retry and on every
exit path after an `up`.

### `down --all`

Tear down **every** environment this provider owns for your Target Project,
whatever PR booted it and whichever Fire, a killed one included. The provider
is the source of what is up: the harness keeps no record of live rounds, so
"owns" is whatever the provider can find of its own (a compose project prefix,
a pidfile directory), and nothing it did not create.

**Idempotent**: exit **0** only when none of its environments remain, a clean
Host included; non-zero when any is still there. The harness calls it before
every `up`, and at the start of every Fire before it picks anything, so a
Daemon restarted after a `kill -9` clears the dead round's environment first.
A `down --all` that fails at a Fire's start is a warning on that Fire's record,
which the Dashboard shows, and the next Fire runs it again.

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

`status` takes no `--pr`: the command resolves its own targets (hosts,
tunnels, logins), and the Harness config never carries them. Its stdout is the
block and nothing else, under the same grammar as `up`'s, and it must carry
every declared Surface's `url_key` (a missing one is an infra-error, as for
`up`). The harness drives it with:

```sh
bin/auto-agent deployed status [<target-dir>]   # 0 the block, 4 not reachable, 2 contract broken, 3 lane off
```

and runs the merged PR's `<!-- post-deploy: … -->` items against the block
through `/auto-agent:verify-deploy`, read-only.

### Temporary files

A provider that writes temporary files uses `TMPDIR`: the harness points it
at a directory of the round's own, under its State dir, and removes it right
after `down`, so nothing the provider leaves there outlives the round.

## Checking it

```sh
bin/auto-agent provider-check [--pr <N>] [<target-dir>]
```

`<target-dir>` is your checkout; leave it out on a Host, where
`AUTO_AGENT_TARGET_DIR` already names it. `--pr=<N>` works too.

It drives, in this order:

1. `down --all` — before the first `up`, as the harness calls it
2. `down --pr N` — with nothing up: `down` is idempotent
3. `up --pr N` — retried once, after another `down`, if it exits 4
4. the block grammar — every line `KEY=value` with an uppercase identifier key
5. the `url_key`s — every declared Surface's key is in the block
6. `smoke` — when `hermetic.smoke` is true, with the block exported
7. `down --pr N` — after the run
8. `down --all` — again, on the Host the run left clean

Steps 4 to 7 run in an environment that is booted, so **every exit path from a
successful `up` tears it down first** — including a failed `up`, which may
have left half a stack behind. A `down` that fails is only reported when
nothing else did: the first thing that went wrong is the useful one.

`--pr` defaults to 0 and only has to be a number nothing else is using; the
check never touches GitHub. Progress goes to stderr; the last line of stdout
is exactly one verdict:

```
provider-check: PASS — verify/provider conforms (8 checks, pr 0)
provider-check: PASS — verify/provider conforms (7 checks, pr 0)   # hermetic.smoke false
provider-check: FAIL — surface api declares url_key API_URL, which the up block does not carry
provider-check: BOOTSTRAP — the Harness config declares no hermetic tier (Bootstrap state): no Environment provider to check
```

Exit codes:

| code | meaning |
| ---- | ------- |
| 0 | the provider conforms |
| 1 | a contract violation; the verdict names it |
| 2 | usage error, or no Harness config could be resolved — the only exit with no verdict line: it is reported on stderr, because nothing was driven |
| 3 | no hermetic tier declared (Bootstrap state): nothing to check, so the verdict reads `BOOTSTRAP`, not `FAIL` |
| 4 | this machine could not boot the environment (`up` exited 3, or 4 twice, or `smoke` exited 2) — your provider may still be conformant |

The 1/4 split is the one to read carefully: **1 means fix your provider, 4
means fix the machine** (Docker not installed, a port in use, an image that
will not pull).

Every failure the check can report, and what it means:

| verdict | cause |
| ------- | ----- |
| `the hermetic command <cmd> is not an executable file under <dir>` | `hermetic.command` does not resolve, or is not `chmod +x` |
| `down --all before the first up exited <rc>, want 0 (down --all is idempotent): <stderr>` | `down --all` failed, or the provider does not answer it at all |
| `down before the first up exited <rc>, want 0 (down is idempotent): <stderr>` | `down` failed with nothing to stop |
| `down between the one retry exited <rc>, want 0 (down is idempotent): <stderr>` | the same, on the retry path |
| `down after up exited <rc>, want 0 (down is idempotent)` | `down` failed against an environment it had just booted |
| `down --all after the run exited <rc>, want 0 (nothing it owns may remain): <stderr>` | `down --all` found something of its own on a Host the run had left clean |
| `up exited 3 (prerequisite missing): <stderr>` | this machine is missing something the boot needs (check exit 4) |
| `up exited 4 (boot failed) on both attempts: <stderr>` | the boot failed twice, with a `down` between (check exit 4) |
| `up exited <rc>, want 0 healthy, 3 prerequisite missing or 4 boot failed: <stderr>` | an exit code outside the contract |
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

Both are driven by `provider-check` in `lib/provider-check.test.sh` — the
compose one against a stub compose CLI, so the suite needs no Docker; it is
run against a real `docker compose` by hand. That suite also asserts that
every verdict in the table above is a string this document and the check
share, so the two cannot drift.

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
