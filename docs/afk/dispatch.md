# Dispatch

`/auto-agent:afk-dispatch` implements one AFK ticket on its `feat/issue-<N>`
branch. The session that runs it is the implementer. It drives a TDD loop,
gets its staged diff approved by the reviewer subagent, has the verifier
subagent run the Environment provider's smoke and land the commit, and flips
the issue to `AFK:done`. It never pushes and never opens a PR: the caller,
`/auto-agent:afk-pickup`, does both.

The authoritative playbook is
[`plugin/skills/afk-dispatch/SKILL.md`](../../plugin/skills/afk-dispatch/SKILL.md).
This page is the summary.

## Invocation

```
/auto-agent:afk-dispatch --issue <N> [--resume] [--dry-run]
```

- `--issue <N>` is the ticket. The caller has already checked out
  `feat/issue-<N>` and applied the `AFK:in-progress` lock.
- `--resume` says the branch carries partial work from a Fire that was paused.
  Dispatch un-freezes the `wip:` commit, runs the tests to find what already
  passes, and continues from there instead of starting over.
- `--dry-run` prints the plan and stops, with no edit, spawn or label:

```
=== /auto-agent:afk-dispatch --issue <N>: dry run ===
issue:    #<N> <title>
branch:   feat/issue-<N>
gated:    <paths matching plan_gated_paths, or none>
roles:    implementer (this session) · reviewer (auto-agent:reviewer) · verifier (auto-agent:verifier)
```

The pickup skill invokes dispatch in-process during a Fire. Dispatch expects
the environment the Fire wrapper exports, `AUTO_AGENT_ROOT` in particular, and
stops without it.

Dispatch reads every repo fact from the resolved Harness config
(`HARNESS_CONFIG_JSON`, which the Fire wrapper exports, or
`bin/auto-agent show-config`): the repo slug, the default branch,
`commands.install`, `commands.test`, `commands.lint`, `commit_scopes`,
`commands.plan_gated_paths`, the Hermetic tier and the verifier runbook.

## The playbook

0. **Pre-flight.** Check that `gh` is authenticated and `jq` is installed.
   Open the review-state file for this branch (see [Hooks](#hooks)). Run
   `bin/auto-agent labels-ensure`, which creates any missing harness label and
   never rewrites an existing one. Dispatch never sweeps a stale
   `AFK:in-progress`: the lock belongs to the caller.
1. **Read the issue**, whole. If a `Blocked by` section names an open issue,
   stop with `afk-dispatch: BLOCKED #<blocker>` and take the failure path.
   Read the Target Project's `CONTEXT.md` and relevant `docs/adr/` entries
   when they exist. Run the install command once on a fresh checkout.
2. **Plan.** Name the acceptance criteria, the files, and the tests (which
   behaviour, through which public seam). If a planned path matches a
   `plan_gated_paths` glob, spawn the reviewer for a **plan round**; it
   replies `plan-approved` or `plan-rejected: <reason>`. One revision is
   allowed. A second rejection takes the failure path.
3. **TDD loop.** One failing test at a time, then the code that passes it.
   Tests go through public interfaces and mock only at system boundaries.
   When every criterion is green, run the lint command, stage only the files
   changed (never `git add .` or `-A`), and write the commit message without
   committing:

   ```
   <type>(<scope>): <short description>

   Closes #<N>
   ```

   `<scope>` is one of the configured `commit_scopes`.
4. **Review round.** Spawn `auto-agent:reviewer`, blocking, with the issue,
   the staged diff (capped at 2000 lines) and the commit message. It replies
   `approved` or `change-request:` with one ask per line. Dispatch records the
   verdict in the review-state file before acting on it, addresses every ask,
   re-stages and re-spawns.
5. **Verify round.** Spawn `auto-agent:verifier`, blocking. It drives the
   provider (`down --pr <N>`, `up --pr <N>`, `smoke` when the config enables
   it, `down --pr <N>`), decides the trailer, and on PASS or SKIPPED commits
   the staged work with the trailer as the last line. On `smoke: FAIL` nothing
   is committed; dispatch fixes the behaviour and goes back to step 4.
6. **Completion or failure.** See [Label flow](#label-flow).

Review rounds and smoke-fail rounds share one cap of **10**, a fixed harness
constant. Dispatch spawns no subagent other than the reviewer and the
verifier, and never passes a `model:` to either, so the Fire's model applies
to both.

The trailer the verifier writes:

| Outcome | Trailer |
| --- | --- |
| `smoke` exits 0 | `smoke: PASS — <detail>` |
| `smoke` exits 1 | `smoke: FAIL — <detail>` (no commit) |
| Smoke off in the config, no provider (Bootstrap state), `up` exits 3 or 4, or `smoke` exits 2 | `smoke: SKIPPED — <reason>` |

The provider's exit codes are defined in
[`plugin/providers/CONTRACT.md`](../../plugin/providers/CONTRACT.md).

## Label flow

```
AFK                 the caller picks the ticket and adds AFK:in-progress
AFK:in-progress     dispatch runs: TDD, review round, verify round
  success           AFK:in-progress removed, AFK:done added, issue closed
  failure           AFK:in-progress removed, AFK:failed added, comment posted
```

On failure dispatch commits nothing, pushes nothing, and exits non-zero so the
pickup skill takes its own failure path. Failure means the round cap was
exhausted, a plan was rejected twice, a blocker is open, or an error dispatch
could not fix. Dispatch touches no label other than these. The full label
table is in [Autonomous loop](autonomous-loop.md#label-taxonomy).

## Hooks

The plugin ships three hooks, registered in
[`plugin/hooks/hooks.json`](../../plugin/hooks/hooks.json). The two that gate
a dispatch key on the **review-state file**, which dispatch creates at pre-flight and removes when
the issue ends:

```
<git-dir>/auto-agent/review-state.json
{ "branch": "feat/issue-<N>", "issue": <N>, "round": <r>,
  "verdict": "pending" | "change-request" | "approved", "asks": ["..."] }
```

A Fire that never dispatched (a dry run, an idle Fire, a reconcile) has no
such file for its branch, so neither of those hooks blocks it.

### `smoke-trailer.sh` (Stop and SubagentStop)

Checks that HEAD carries a `smoke: PASS|FAIL|SKIPPED` trailer. It runs when
the verifier subagent stops and when the dispatch session stops. It only
checks while the review-state file names the checked-out branch, and only a
dispatch commit: a conventional-commit subject plus a `Closes #<N>` line. Any
other HEAD (a merge, a `wip:` freeze, a `fix(ci):` round) passes. When the
trailer is missing it exits 2 and tells the agent to amend the commit.

### `review-gate.sh` (Stop)

Blocks the session from ending while the review-state file says
`change-request` for the checked-out branch. It exits 2 and feeds the recorded
asks back. A file for another branch is stale debris and is ignored.

Both hooks read `stop_hook_active` from the hook input and let a second stop
through, so an ask or a commit the agent cannot fix never pins the session in
a loop. Both fall back to exit 0 when `git` fails or `jq` is missing.

### `caveman.sh` (UserPromptSubmit)

Asks a Fire for terse prose: nobody reads a Fire's sentences as they are
written, and every word is output the budget pays for. It adds its context
only when the Fire wrapper's `AUTO_AGENT_FIRE=1` is in the environment, so a
session a human is talking to (the Setup conversation, a planning skill)
keeps its full sentences. The context exempts what must stay verbatim: the
lines a skill tells the session to emit exactly, commit messages, PR and
issue text, and code. `AUTO_AGENT_CAVEMAN=off` in the Host env turns it off.
It never blocks a prompt.

All three hooks are tested in `plugin/hooks/hooks.test.sh`.

## Troubleshooting

- **The reviewer keeps requesting changes.** After the 10th round dispatch
  applies `AFK:failed` and comments with the last asks. Read the comment, fix
  the ticket or the code by hand, then remove `AFK:failed` to requeue it.
- **`smoke: FAIL`.** Nothing was committed. Dispatch fixes, re-reviews and
  re-verifies, and each attempt counts toward the cap.
- **`smoke: SKIPPED — no Environment provider`.** The Target Project is in the
  Bootstrap state. The commit lands, and the PR the caller opens is labelled
  `AFK:verify-human`.
- **`smoke-trailer.sh` blocks the verifier.** HEAD is a dispatch commit with
  no trailer. The verifier amends that one commit's message.
- **`review-gate.sh` blocks a stop.** The review-state file still says
  `change-request`. Address the asks and re-run the review round.
- **`afk-dispatch: BLOCKED #<n>`.** The issue body names an open blocker.
  Close the blocker, or correct the body, then remove `AFK:failed`.
- **A stale review-state file after a crash.** It names a branch. The hooks
  ignore it on any other branch, and the next dispatch on that branch
  overwrites it at pre-flight.
- **A stuck `AFK:in-progress`.** Not dispatch's to clear. See
  [Operational notes](autonomous-loop.md#operational-notes).

## Related

- [Roles](roles.md): the subagents dispatch spawns.
- [Autonomous loop](autonomous-loop.md): what calls dispatch and what happens
  after it.
- [ADR 0003](../adr/0003-environment-provider-contract.md): the provider
  contract the verifier drives.
