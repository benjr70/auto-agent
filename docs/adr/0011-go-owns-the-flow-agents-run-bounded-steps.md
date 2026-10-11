---
status: accepted
---

# Go owns the flow; the agent runs bounded Steps and never writes to GitHub

A Fire used to be one long Claude session following a 1,497-line skill: the
model read the triage verdict, flipped labels, opened the PR, chained review,
CI and verification, and merged, calling 32 bash subcommands on the way. The
flow was only as reliable as the model's reading of prose, it could not be
tested without a model, and one path evalled a merge command scraped from
agent output. We decided the Daemon's flow is compiled Go: an explicit state
machine that calls Claude Code for one **Step** at a time, hands it a typed
input, and receives a schema-checked result that Go re-checks and then acts
on. A Step reads, edits the checkout and drives Surfaces; it holds no GitHub
credential, and every push, comment, label change and merge is made by Go.
Step prompts are embedded in the binary; the repo's Claude Code plugin keeps
only the human-driven planning skills.

## Considered options

- **Keep the flow in skills, harden the prose** (the `runbook-check` path):
  guards rules with greps, cannot prove behaviour. Rejected.
- **Agent with write access, Go only as scheduler**: keeps the eval path and
  the untestable flow. Rejected.
- **The Agent SDK instead of the CLI**: no official Go SDK exists; the
  Python and TypeScript ones spawn the same binary. The CLI with
  `--json-schema` is the supported route, behind one interface.

## Consequences

- The flow is testable in CI against a fake GitHub server and a fake `claude`.
- A Step that is denied a tool, or loses one to a wrong flag, still reports
  success, so the runner must compare the tools a Step got with the tools it
  needs, and must exclude the account's MCP connectors.
- Supersedes the "harness as a plugin the Fire loads" half of ADR 0001.
