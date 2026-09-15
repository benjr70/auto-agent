---
status: accepted
---

# The verification round reads the Harness config from the PR head

A Target Project in the Bootstrap state has no Environment provider, and the
provider is meant to be written by the Daemon itself through an AFK issue.
That PR cannot be verified by a provider that does not exist on the default
branch. We decided the hermetic round reads `.auto-agent/harness.json` from
the PR head, the checkout the round already runs in, so the PR that adds a
provider is verified by the provider it adds, and its first green hermetic
round is the evidence that closes the Bootstrap state. The pickup-time
preflight keeps validating the default-branch copy, so a broken config on the
PR head fails only that PR's round.

## Considered options

- **Always the default branch**: the provider PR would land with
  `AFK:verify-human` and the human would run the Provider check by hand.
  Rejected; self-verification is stronger evidence at no cost.
- **Provider check in CI on the provider PR**: proves conformance, not a real
  checklist round. Rejected as the sole gate; the check stays a tool.

## Consequences

- A PR can change its own verification: drop a Surface, turn `smoke` off,
  point `hermetic.command` elsewhere. That is a human review item on every
  Agent PR that touches `.auto-agent/`; the screenshot tour stays mandatory for
  any touched UI surface (ADR 0003), so the strongest evidence cannot be
  configured away.
- The Dashboard's bootstrap warning derives from config presence on the
  default branch each Fire; a provider that fails after the retry shows as
  the last round's outcome, never as the warning.
