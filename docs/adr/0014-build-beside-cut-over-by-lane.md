---
status: accepted
---

# The Go Daemon is built beside the bash and takes over one Lane at a time

The first plan was to replace the bash one module at a time behind the
existing subcommands, with the bash suites as the parity oracle. Two later
decisions removed its footing: Go calls the GitHub API directly, so suites
that fake the `gh` executable cannot test it, and the flow of the pickup
skill is replaced by the state machine as a whole. We decided to build the
Go Daemon in its own directories as thin end-to-end slices while the bash
stays untouched and keeps running the live Daemon, which builds its
replacement. A Target Project is cut over when the Go version covers the
Lanes it uses; the bash is deleted after the last cut-over has run clean.

## Consequences

- Nothing ever runs half-migrated, and the bash is the rollback.
- The bash suites are a reference, not an oracle: each Slice ports the cases
  of the behaviour it takes over to Go table tests and lists any it drops.
- Two implementations exist for the length of the migration; a behaviour
  change in that window has to be made twice or deferred.
- Live runs go to a fixture repo before any real Target Project.
