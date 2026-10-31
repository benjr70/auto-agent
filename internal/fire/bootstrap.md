<!-- auto-agent:bootstrap -->

## What to build

This Target Project is in the **Bootstrap state**: its Harness config declares
no `verification.hermetic` block, so every Agent PR the Daemon opens is
labelled `AFK:verify-human` and waits for a human to verify it by hand. Write
the Environment provider that ends that.

Read the contract first — it is the whole specification, and the harness
ships two reference implementations beside it:

- `plugin/providers/CONTRACT.md` in the Harness install: `up --pr N`,
  `down --pr N`, `down --all` and the optional `smoke`, the `KEY=value`
  block `up` prints on stdout, and the exit codes each subcommand owes.
- `plugin/providers/compose/provider` — the reference for a project that
  boots with `docker compose`; `plugin/providers/provider-lib.sh` is the
  shared helper both references source.

The provider is one executable in THIS repository (the references are a
starting point to copy and adapt, never a file to depend on at runtime), plus
the `verification.hermetic` block in `.auto-agent/harness.json` that names
it and the `surfaces` its block must carry a URL for.

Surfaces this config declares today:

{{surfaces}}

## Acceptance criteria

- [ ] The provider is executable, committed in this repository, and named by
      `verification.hermetic.command` in `.auto-agent/harness.json`.
- [ ] `bin/auto-agent provider-check` against this checkout prints a
      conforming verdict (exit 0).
- [ ] `up` prints a `KEY=value` block carrying the `url_key` of every
      declared Surface, and `down` is idempotent.
- [ ] `down --pr N` leaves nothing `up --pr N` created for that PR, and
      `down --all` tears down every environment the provider booted, exiting
      0 only when none remain.
- [ ] Two environments for different PR numbers can be up at once without
      colliding.

## Behaviors to test

1. `up` / `down` / `up` again leaves one healthy environment (the
   contract's own sequence).
2. `provider-check` conforms.

## Notes

The PR that adds this provider is verified by the provider it adds (ADR 0007:
the verification round reads the Harness config from the PR head), so its
first green round is the evidence that closes the Bootstrap state. That PR
touches `.auto-agent/`, so the review round flags it for a human — expected,
not a defect.
