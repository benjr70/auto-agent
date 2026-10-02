# Roles

The plugin ships four subagents in [`plugin/agents/`](../../plugin/agents/).
Each file has frontmatter (`name`, `description`, `tools`, `effort`) and a
body that becomes the subagent's instructions. Because the plugin is
namespaced, a skill spawns one as `auto-agent:<name>` through the `Agent`
tool. There are no agent teams, no shared task list and no long-lived
teammates: a subagent is spawned for one job, blocks its caller, replies once
and ends.

None of the four pins a model. Each inherits the model of the Fire that
spawned it, so the Host env's Model policy carries through, and no skill
passes `model:` when spawning. All four declare `effort: medium`. Separation
between roles comes from the tool allowlist and the hooks, not from model
choice.

| Subagent | Tools | Spawned by | Writes |
| --- | --- | --- | --- |
| `auto-agent:implementer` | Read, Edit, Write, Bash, Glob, Grep | `pr-watch`, `pr-reconcile`, `afk-pickup` (manual fix round) | The working tree and the index. No commit, no push. |
| `auto-agent:reviewer` | Read, Grep, Glob, Bash | `afk-dispatch` | Nothing. |
| `auto-agent:verifier` | Read, Bash | `afk-dispatch` | One commit, with the `smoke:` trailer. |
| `auto-agent:manual-verifier` | Read, Grep, Glob, Bash | `verify-pr`, `verify-deploy` | Evidence files in the round's artifact directory. |

## The implementer in dispatch is not a subagent

In [`/auto-agent:afk-dispatch`](dispatch.md) the implementer is the Fire's own
session: it reads the ticket, drives the TDD loop, stages the diff and writes
the commit message. The `auto-agent:implementer` subagent below is the same
role handed a narrower brief by the skills that run after the first commit.

## implementer

**File**: [`plugin/agents/implementer.md`](../../plugin/agents/implementer.md)

Given an issue, the branch's diff and a concrete fix brief, it changes the
shipped code, stages the result and reports. The brief is one of: failing CI
job logs (from `/auto-agent:pr-watch`), review threads or a rebase conflict
(from `/auto-agent:pr-reconcile`), or failed manual-verification items (from
`/auto-agent:afk-pickup`). The prompt embeds everything it needs, including
the Target Project's test and lint commands, so it makes no `gh` calls.

It may: edit files, run the test and lint commands, and `git add` the paths
it changed.

It may not: commit, push or amend history; edit labels, comments, the PR body
or the issue; run the Environment provider; run `git rebase --continue` in a
conflict brief (the caller drives the rebase); weaken a test or edit an
acceptance criterion to make a failure go away.

When it believes no code change is warranted it stages nothing and replies
with the dispute line the caller named: `pr-watch-flake: <reason>`,
`<threadId>: revise-dispute — <reason>` or `manual-verify-dispute: <reason>`.

## reviewer

**File**: [`plugin/agents/reviewer.md`](../../plugin/agents/reviewer.md)

Reads the implementer's staged diff and the issue before the commit exists,
and replies with exactly one of `approved` or `change-request:` followed by
one specific ask per line. In a plan round it receives a plan instead of a
diff and replies `plan-approved` or `plan-rejected: <reason>`.

Its checklist: tests cover the behaviours the issue lists; tests go through
public interfaces; no mocks of internal collaborators; no files unrelated to
the issue; no change to a `plan_gated_paths` path without an approved plan
round; the commit message has the configured shape and scope; naming follows
the Target Project's glossary and no ADR is contradicted.

It may: read, search, and run the test and lint commands to confirm the diff
is green.

It may not: edit or write files, `git add`, or commit. Edit and Write are not
in its allowlist, and it is told not to route around that with Bash. It
describes the problem and never supplies the fix, so a second reader stays a
second reader.

## verifier

**File**: [`plugin/agents/verifier.md`](../../plugin/agents/verifier.md)

Runs after the reviewer approves. It drives the Target Project's Environment
provider through the contract (`down`, `up --pr <N>`, `smoke`, `down`),
decides the `smoke: PASS|FAIL|SKIPPED — <detail>` trailer, and on PASS or
SKIPPED lands the staged work as one commit with the implementer's message
verbatim plus the trailer as the last line. On FAIL it commits nothing and
replies `smoke FAIL: <detail>`.

`PASS` requires a real exit 0 from `smoke`. Anything it could not execute (no
provider in the Bootstrap state, smoke disabled in the Harness config, a
failed boot, a smoke that could not run) is `SKIPPED` with the real reason.

It may: read files, run the provider, and make that one commit.

It may not: edit source files; rewrite the subject or the `Closes #<N>` line;
amend earlier commits; touch labels or the issue; push; install anything. It
always runs the final `down`, and never calls the provider with a PR number
other than the one it was given.

The `smoke-trailer.sh` hook re-checks HEAD when the verifier stops, so a
commit without a trailer is caught independently. See
[Dispatch: hooks](dispatch.md#hooks).

## manual-verifier

**File**: [`plugin/agents/manual-verifier.md`](../../plugin/agents/manual-verifier.md)

The subagent behind a Verification round. `/auto-agent:verify-pr` hands it an
environment that is already running, the unchecked checklist items, the
declared Surfaces and the round's artifact directory. For each item it
returns one verdict with concrete evidence:

- **PASS** or **FAIL** for anything that can be exercised on a declared
  Surface. It must exercise it; deferring a locally runnable item is a FAIL.
- **DEFER (deployed-env)** for an item only a real deployment can prove,
  together with the exact post-deploy check to add. A deferral without that
  demanded spec is a FAIL.
- **DEFER (hardware)** for an item that needs physical hardware, with the
  named blocker and the check a human should perform.

How it drives a Surface is decided by the Surface's kind, not by the
subagent: a `browser` or `electron` Surface through the MCP server the
harness registered for it, a `cli` or `api` Surface through its own shell and
HTTP calls against the URLs the provider's block carried. When the round asks
for a Screenshot tour it captures one viewport-clipped screenshot per touched
screen, named `<surface>-NN-<slug>.png`.

In a Deployed round (`/auto-agent:verify-deploy`) the same subagent works a
merged PR's deferred items against a live environment, read-only.

It may: read, search, and run commands scoped to the environment the round
booted; write evidence files under the artifact directory.

It may not: tick a box, edit the PR, write to the repo or touch git; install
anything; start, stop or restart an app or the environment (the skill owns the
lifecycle and the teardown); reach anything outside this PR's environment. In
a Deployed round it may not write to the live environment or run the
provider's `up` or `down`.

Evidence must be an observation, not a conclusion: a request line and status
code, an on-screen value, a stored row, a log excerpt, a screenshot filename.

## How they are chosen

No role is chosen at run time by judgement. Each skill names the subagent it
spawns:

- `/auto-agent:afk-dispatch` always spawns the reviewer for the diff, the
  reviewer again for a plan round only when a planned path matches
  `commands.plan_gated_paths`, and the verifier once the reviewer approves.
- `/auto-agent:pr-watch` spawns the implementer once per red CI round.
- `/auto-agent:pr-reconcile` spawns the implementer for each rebase conflict
  stop and once per review-fix round.
- `/auto-agent:afk-pickup` spawns the implementer once per manual fix round,
  when a Verification round reported a FAIL or an outstanding spec demand.
- `/auto-agent:verify-pr` and `/auto-agent:verify-deploy` spawn the
  manual-verifier once per round.

The skills that run as their own agents (`pr-watch`, `pr-review`,
`pr-reconcile`, `verify-pr`, `deps-land`) are spawned by the pickup skill as
`general-purpose` agents told to invoke the skill. They are skills, not
roles, and are described in [Autonomous loop](autonomous-loop.md).

## Related

- [Dispatch](dispatch.md): the playbook the reviewer and verifier serve.
- [README: The verification round](../../README.md#the-verification-round):
  what the harness owns around the manual-verifier.
