---
status: accepted
---

# The hermetic tier is one project-owned executable with `up`, `down` and `smoke`

Smart-Smoker-V2 proves a change works by booting a per-PR compose stack, waiting
on health endpoints, running a smoke probe and driving a headful browser and an
Electron app through a checklist round. Every one of those pieces is wired to
Smart Smoker's service names, ports and health paths. For the harness to be
reusable, the Daemon must never know how an environment comes up. We decided
the Target Project owns one executable, the Environment provider, with three
subcommands: `up --pr N` prints a flat `KEY=value` block on stdout and returns
only when the environment is healthy, `down --pr N` is idempotent, and
`smoke` prints a `smoke: PASS|FAIL` line. Exit codes are fixed (0 healthy,
3 prerequisite missing, 4 boot failed; smoke 0/1/2). The harness keeps the
one-retry rule, the checklist protocol, the verifier core, the evidence sink
and the result lines. Surfaces, declared in the Harness config, map provider
stdout keys to what the verifier drives, and the screenshot tour runs for
every touched UI surface.

## Considered options

- **Declarative health targets in config with the harness doing the wait**:
  pushes readiness semantics (status bodies, retry budgets) into a schema.
  Rejected; readiness is the provider's business.
- **Three separate command strings** for up, down and smoke: three things to
  keep consistent and test. Rejected in favour of one deep module.
- **Making the hermetic tier schema-required**: cleaner, but a project without
  a provider could not use the Daemon to build one. Rejected; absence is a
  visible bootstrap state (`AFK:verify-human` plus a Dashboard warning), not a
  supported mode.
- **A smoke tier of its own**: only meaningful against a booted environment.
  Rejected; smoke is a sub-hook of hermetic.

## Consequences

- The deployed tier reuses the same shape minus `up`/`down`: a `status`
  subcommand prints the `KEY=value` block, so one verifier serves both tiers.
- The `smoke:` trailer and its hook apply only when `hermetic.smoke` is true.
- The harness exports the `up` (or `status`) block into the environment of
  `smoke`, every launcher and the verifier; keys are uppercase shell
  identifiers, values run to end of line, and a missing `url_key` is an
  infra-error. The harness calls `down` between retries and after every
  `up`; before the first `up` it calls `down --all` (see the amendment
  below). (Sharpened by the fixture prototype.)
- `cli` and `api` surfaces are evidence-only; `browser` and `electron` surfaces
  always earn a tour when touched, so a project cannot opt out of screenshots.
- Smart Smoker's stack-runner, preflight-boot and Electron launcher become its
  provider and launcher implementations behind this contract at cut-over.

## Amendment: the provider owns every environment it booted

A Daemon killed mid-round (`kill -9`, a reboot) left the round's environment
running, and nothing on the Host knew which PR it belonged to: the next Fire
called `down --pr N` only for the PR it was about to boot. The Footprint grew
until a human cleaned up. We tightened the contract and added a verb, rather
than have the Daemon keep a record of live rounds:

- **`down --pr N`** must leave nothing on the Host that `up --pr N` created for
  that PR: its containers, networks, volumes and the images it built. Shared
  things (pulled base images, the build cache, package caches) are explicitly
  left. It stays idempotent (nothing to stop exits 0), and exits non-zero
  when anything of the PR's it owns remains.
- **`down --all`** tears down every environment the provider owns for its
  Target Project, whatever PR booted it and whichever Fire. It is idempotent
  and exits 0 only when none remain, a clean Host included.
- **The provider is the source of what is up.** The Daemon keeps no record of
  live rounds. The Sweep calls `down --all` at the start of every Fire (so at
  the Daemon's start, before its first pick) and the harness calls it before
  every `up`, in place of the per-PR `down --pr N` it called there before.
- A `down --all` that exits non-zero at a Fire's start is a warning on that
  Fire's record, which the Dashboard shows; the next Fire's Sweep runs it
  again. Before an `up` it is a contract violation, and nothing is booted.
- The **Provider check** drives `down --all` before anything else and again
  on the Host its run left clean, beside `down --pr N` with nothing up, so a
  provider without the verb fails and the verdict names it.

Considered: a local record of which PRs had an environment up, swept by the
Daemon. Rejected: a `kill -9` between `up` and the write loses it, and only
the provider knows what its environments are made of.
