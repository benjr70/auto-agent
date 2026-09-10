---
status: accepted
---

# Harness config is a committed `.auto-agent/` directory: JSON for the machine, markdown for prose

Smart-Smoker-V2's daemon knows its project through ~40 hard-coded values
spread over bash libs, a Python dashboard and every skill prompt. Once the
harness runs from a separate Harness install (ADR 0001) those values must come
from the Target Project. We decided the Target Project commits a `.auto-agent/`
directory holding `harness.json`, the one file the libs and Dashboard read, and
fixed-name markdown siblings (verifier runbook, bot-PR checklist, deployed
checks) that skills paste into prompts verbatim. Account and machine facts
(tokens, budget gate, model policy, harness ref, paths) never enter that file;
they stay in the Host env.

## Considered options

- **YAML via `yq`**: friendlier to write, but a new binary on every Host for a
  file that is edited rarely and read by machines. Rejected.
- **A sourced bash env file**: zero parsing, but flat, so the optional lane
  blocks the Scope decision gates on presence have nowhere to live. Rejected.
- **Prose only (CLAUDE.md)**: fine for the implementer, useless for bash libs
  that need commands and caps as strings. Rejected; prose stays for humans and
  the implementer, `harness.json` is what the machine runs.
- **Configurable label and branch vocabulary**: multiplies the surface across
  five libs, the Dashboard and every skill for no demand. Rejected; labels,
  branch shapes and the admin-squash merge recipe are fixed harness vocabulary.

## Consequences

- `jq` is the only parser; the plugin ships a JSON schema and both Setup and
  every Fire's preflight validate against it, failing closed before any GitHub
  write.
- The default branch is detected from GitHub each Fire, not declared.
- Only five round caps are documented keys; every other numeric constant stays
  an undocumented env override.
- The `pick` block has two shapes, Project+Priority or label-only, and the
  forked planning skills read the same block, so a Target Project without a
  Project board can run the harness.
- Model policy lives in the Host env because it follows the Claude account,
  which several Hosts may share.
