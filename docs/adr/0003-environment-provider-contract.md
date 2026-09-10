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
  infra-error. The harness calls `down` before the first `up` as well as
  between retries. (Sharpened by the fixture prototype.)
- `cli` and `api` surfaces are evidence-only; `browser` and `electron` surfaces
  always earn a tour when touched, so a project cannot opt out of screenshots.
- Smart Smoker's stack-runner, preflight-boot and Electron launcher become its
  provider and launcher implementations behind this contract at cut-over.
