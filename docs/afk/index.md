# AFK

AFK ("away from keyboard") is the part of the harness that works without a
human present: the Daemon, the Fires it runs, the skills and subagents a Fire
loads, and the `AFK` label family that carries state between Fires. It turns
an AFK ticket in a Target Project into an Agent PR that is green, reviewed
once, verified live, and waiting for a human to merge.

The harness was extracted from Smart-Smoker-V2. Nothing in these pages is
specific to that project: every repo fact comes from the Target Project's
Harness config (`.auto-agent/harness.json`) and every machine fact from the
Host env (`~/.config/auto-agent/env`). The vocabulary is in
[`CONTEXT.md`](../../CONTEXT.md).

## How a ticket becomes a merged PR

You plan with the planning skills (`/auto-agent:wayfinder`,
`/auto-agent:to-spec`, `/auto-agent:to-tickets`), which publish Slices labelled
`AFK` on the pick signal the Harness config declares. On the Host, the Daemon
asks the Budget gate whether to fire. When it may, it runs one Fire: the
checkout is reset to the tip of the default branch and `claude -p` is prompted
with `/auto-agent:afk-pickup`. The pickup skill does exactly one unit of work.
For a fresh pick it takes the `AFK:in-progress` lock, creates
`feat/issue-<N>`, and invokes `/auto-agent:afk-dispatch`, in which the session
itself implements the ticket test-first, the reviewer subagent approves the
staged diff, and the verifier subagent runs the Environment provider's smoke
and lands the commit with a `smoke:` trailer. The pickup skill then pushes,
opens the PR, and drives it through `/auto-agent:pr-watch` (CI green),
`/auto-agent:pr-review` (one code review, once per PR) and
`/auto-agent:verify-pr` (a Verification round on the declared Surfaces). You
review the PR. If you want changes you leave inline comments and apply
`AFK:revise`, and a later Fire fixes them. When you are satisfied you approve
and merge. The machine never merges an Agent PR.

## The pieces

| Piece | What it is | Read |
| --- | --- | --- |
| Daemon | Always on and budget-paced: Budget gate, one Fire, Sleep Planner, Work Probe. Run by `auto-agent-daemon.service`. | [Autonomous loop](autonomous-loop.md), [README: The Daemon](../../README.md#the-daemon) |
| Fire | One Daemon pass, one unit of work, one Fire record. | [Autonomous loop](autonomous-loop.md#one-fire-is-one-unit-of-work), [README: A Fire](../../README.md#a-fire) |
| Budget gate | The fire-or-wait decision, chosen per auth mode. | [README: The Budget gate](../../README.md#the-budget-gate), [ADR 0008](../adr/0008-budget-gate-per-auth-mode.md) |
| Dispatch | `/auto-agent:afk-dispatch`: the single-agent implementer with a review round and a verify round. | [Dispatch](dispatch.md) |
| Subagents | `auto-agent:implementer`, `auto-agent:reviewer`, `auto-agent:verifier`, `auto-agent:manual-verifier`. | [Roles](roles.md) |
| Hooks | `smoke-trailer.sh`, `review-gate.sh` and `caveman.sh`, shipped in the plugin. | [Dispatch: hooks](dispatch.md#hooks) |
| Verification tail | CI watch, the one-time review, the Verification round. | [Autonomous loop](autonomous-loop.md#the-verification-tail), [README: The verification round](../../README.md#the-verification-round) |
| PR reconcile | Rebase on conflict, fix review threads, re-run the tail. | [Autonomous loop](autonomous-loop.md#pr-reconcile) |
| Labels | The `AFK:*` state labels and who sets and clears each. | [Autonomous loop](autonomous-loop.md#label-taxonomy) |
| Optional lanes | The deps-land lane and the Deployed tier, on only when declared. | [Autonomous loop](autonomous-loop.md#the-deps-land-lane), [README: The deps-land lane](../../README.md#the-deps-land-lane), [README: The Deployed tier](../../README.md#the-deployed-tier) |
| Resolve lane | `/auto-agent:afk-resolve`: a Decision ticket answered with research, not code. | [Autonomous loop](autonomous-loop.md#the-resolve-lane) |
| Environment provider | The Target Project's `up`, `down` and `smoke` behind the Hermetic tier. | [README: The Environment provider](../../README.md#the-environment-provider), [`CONTRACT.md`](../../plugin/providers/CONTRACT.md), [ADR 0003](../adr/0003-environment-provider-contract.md) |
| Harness config | What a Target Project commits under `.auto-agent/`. | [README: Harness config](../../README.md#harness-config), [ADR 0002](../adr/0002-harness-config-json-directory.md) |
| Host | The machine a Daemon runs on, and how Setup produces it. | [Host](../host.md), [README: Setup](../../README.md#setup) |
| Dashboard | The read-only status page each Host serves. | [`dashboard/README.md`](../../dashboard/README.md), [ADR 0006](../adr/0006-dashboard-per-host-reading-fire-records.md) |

## What is fixed and what a Target Project declares

The harness owns a fixed vocabulary that no Target Project configures: the
labels, the branch shapes (`feat/issue-<N>` for a Slice, `research/<slug>` for
a resolve), the commit and PR shapes, and the order of work inside a Fire.
These live as constants in [`lib/harness-config.sh`](../../lib/harness-config.sh).

A Target Project declares everything else in its Harness config: the pick
signal (a GitHub Project with a priority field, or labels only), the commit
scopes, the install, test and lint commands, the paths that need a plan round,
the five round caps, the Surfaces, the Hermetic tier's provider command, and
the optional lanes. The default branch is detected from GitHub and is never
declared.

## Why one agent with two subagents

The implementer is the Fire's own session, so one Fire costs one context. The
two things it must not judge itself are handed to subagents with narrower
tools: the reviewer cannot edit, and the verifier can only read and run
commands. The separation is enforced by each subagent's tool allowlist and by
the two gating hooks, not by a prompt asking for restraint. See [Roles](roles.md).
