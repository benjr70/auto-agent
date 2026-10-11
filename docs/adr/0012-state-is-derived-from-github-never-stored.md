---
status: accepted
---

# The Daemon derives its state from GitHub on every pass and stores none

With Go owning the flow, the obvious design is a state field per ticket in a
local store. We decided against it: on every pass the Daemon reads labels,
hidden markers, the draft flag, mergeability, checks and branches, and
computes where each ticket and PR stands as a pure function of those facts.
The local journal records what happened and is never the source of a
decision; the one value read back from it is a Claude session id, used to
resume a Step cut off by the usage limit, with a fallback when it is gone.
To make this hold, every Step that changes code ends with Go pushing to the
ticket's branch, so no work exists only on the Host.

## Considered options

- **A local state store reconciled against GitHub**: fewer API reads, but two
  sources of truth, a migration on every move of machine, and a human editing
  a label becomes a conflict. Rejected.

## Consequences

- A restart, a new machine or a hand-edited label cannot desynchronise it.
- The label and marker vocabulary is the state encoding and cannot be changed
  casually.
- What GitHub cannot show is not state: progress through the pre-commit review
  is lost on a restart and that review runs again.
- Polling cost matters; conditional requests with ETags keep it low.
