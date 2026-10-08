# Autonomous loop

How the Daemon takes an AFK ticket to an Agent PR that is ready to merge, and
keeps that PR ready after a human reviews it. The Daemon runs on a
[Host](../host.md) as a systemd service, paces itself by Claude budget rather
than by clock, and covers the whole life of a PR: pick, implement, open, CI
green, one review, a Verification round, and afterwards review fixes and
merge conflicts.

```
systemd (auto-agent-daemon.service, Restart=always)
  └─ bin/auto-agent daemon      Budget gate → Fire → Sleep Planner
       └─ bin/auto-agent fire   reset to the default branch, claude -p "/auto-agent:afk-pickup"
            └─ /auto-agent:afk-pickup    ONE unit of work, in strict order:
                 1. reconcile   an open PR needs attention  → /auto-agent:pr-reconcile
                                (a docs-only PR → the docs-only gate; a Bot PR → /auto-agent:deps-land)
                 2. resume      an AFK:paused issue
                 3. pick        the next eligible AFK ticket
                      Slice           → /auto-agent:afk-dispatch, then the PR and its tail:
                                        /auto-agent:pr-watch → /auto-agent:pr-review → /auto-agent:verify-pr
                      Decision ticket → /auto-agent:afk-resolve
                 4. deployed    a merged PR's deferred items → /auto-agent:verify-deploy
```

## The Daemon

`bin/auto-agent daemon` ([`lib/daemon.sh`](../../lib/daemon.sh)) runs one cycle
after another against one Target Project:

1. **Gate.** `bin/auto-agent usage-sensor` prints the Gate verdict. The Daemon
   fires when `shouldFire` is true. How the verdict is reached depends on the
   auth mode; see [README: The Budget gate](../../README.md#the-budget-gate)
   and [ADR 0008](../adr/0008-budget-gate-per-auth-mode.md). The fire
   threshold is `AUTO_AGENT_GATE_MIN_PCT` (default 25).
2. **Fire.** `bin/auto-agent fire` does one unit of work and writes one Fire
   record. After a clean Fire the Daemon goes straight back to the gate, so a
   backlog drains within one budget window.
3. **Sleep.** When the gate says wait, the Sleep Planner
   ([`lib/sleep-planner.sh`](../../lib/sleep-planner.sh)) sleeps to the
   verdict's reset, then polls the gate (every 300 s, up to 12 times) because
   a reset estimate can be early. An unknown reset sleeps 18000 s.

The Fire's stable stdout lines steer the exceptions:

| Line | Meaning | Daemon reaction |
| --- | --- | --- |
| `AGENT_RUN_NO_WORK=1` | Empty queue, or the lock is held | Sleep to the reset in Work Probe chunks; wake early if work appears |
| `AGENT_RUN_RESET_AT=<iso>` | Budget ran out mid-Fire; the work was paused | Sleep to that reset |
| `AGENT_RUN_MODEL_LIMIT=<scope>` | A per-model limit | Re-gate at once so the Model policy switches model; a second in a row sleeps like exhaustion |
| `AGENT_RUN_AUTH_DEAD=1` | The Claude credential died | The Daemon is Parked |
| non-zero exit, no line | A failed Fire | Probe-sleep; after `AUTO_AGENT_DAEMON_FAIL_CAP` (3) failures in a row, sleep deaf to the reset |

**The Work Probe** ([`lib/work-probe.sh`](../../lib/work-probe.sh)) lets a
no-work sleep notice work that arrives on human time. Every
`AUTO_AGENT_WORK_PROBE_INTERVAL` seconds (default 300) it sweeps the Target
Project with `gh` alone, at zero Claude cost, and wakes the Daemon when:

- a PR needs reconciling (the same triage the Fire runs) or an issue is
  `AFK:paused`: always;
- an open PR that was in the baseline has gone (merged or closed), which may
  have unblocked the queue;
- the set of pick candidates differs from the baseline taken when the Fire
  reported no work, so a ticket the Fire already declined cannot wake-loop.

It never wakes while `AFK:in-progress` is held, and a `gh` error on the lock
read counts as held.

**Parked.** A dead credential is never treated as exhaustion. The Daemon
writes `parked.json`, opens (or reuses) one `AFK:needs-human` issue, and from
then on only re-probes `claude auth status` every
`AUTO_AGENT_PARK_REPROBE_SECS` (3600). See
[Host: after credential death](../host.md#park-and-un-park-after-credential-death).

## One Fire is one unit of work

The Fire wrapper ([`lib/fire.sh`](../../lib/fire.sh)) validates the Harness
config and fails closed, resets the checkout to the tip of the detected
default branch (anything worth keeping is already on its own branch), and
runs `claude -p` with the plugin, the settings baseline and
`--permission-mode bypassPermissions`. Details are in
[README: A Fire](../../README.md#a-fire).

`/auto-agent:afk-pickup` makes one read-only triage call
(`bin/auto-agent pickup-triage`) and then does exactly one of these, first
match wins:

1. **Reconcile** an open PR that needs attention. A human waiting on their
   own review outranks everything. See [PR reconcile](#pr-reconcile).
2. **Resume** an `AFK:paused` issue. Its branch is checked out as it is,
   never reset. After `rounds.pause_resume` pauses (default 3) the issue goes
   `AFK:failed` instead.
3. **Pick** the next eligible AFK ticket: open, labelled `AFK`, carrying none
   of `AFK:in-progress`, `AFK:done`, `AFK:failed`, `AFK:paused`. Under a
   Project pick it must be an item of the configured Project, ranked by the
   configured priority field then oldest; under a label-only pick, oldest
   first. Every native GitHub `blockedBy` dependency must be closed (body
   prose such as "Blocked by #12" is ignored by the picker), and it must have
   no assignee other than the Machine user. A Slice goes to
   [dispatch](dispatch.md); a ticket labelled `wayfinder:research` or
   `wayfinder:task` goes to [the resolve lane](#the-resolve-lane).
4. **Deployed round**, only when nothing else is owed and the Deployed tier
   is on. See [The Deployed tier](#the-deployed-tier).

Triage refuses to act when `gh` is logged in as anyone but the Machine user.
`AFK:in-progress` is the Single-flight lock: while any open issue holds it,
every other Fire skips.

## The verification tail

After a PR opens, and again after any later push to it, the same tail runs
before the Fire may end:

- **`/auto-agent:pr-watch`** waits for CI (60 s polls, 45 minutes per round).
  On red it spawns the implementer and pushes a `fix(ci):` commit, up to
  `rounds.pr_watch` rounds (default 10). On exhaustion the PR becomes a draft
  with `AFK:checks-failed`.
- **`/auto-agent:pr-review`** runs once in a PR's life, gated by a
  `<!-- pr-review-done -->` marker comment. It reviews on two axes
  (correctness, and the diff against the issue's acceptance criteria and its
  parent Spec) and posts findings as inline review threads. It never fixes
  them. With findings it applies `AFK:revise` and the Fire ends; the next
  Fire reconciles. A PR that edits the Harness config directory also gets
  `AFK:verify-human`.
- **`/auto-agent:verify-pr`** is one Verification round: the PR's unchecked
  checklist items exercised live on the declared Surfaces in an environment
  the Environment provider booted for this PR, the passing boxes ticked, one
  evidence comment. See
  [README: The verification round](../../README.md#the-verification-round).
  A FAIL, or a deferral demanding a `<!-- post-deploy: … -->` item the PR
  does not carry yet, loops the implementer (`fix(manual):` commits, cap
  `rounds.manual_verify`, default 3) and re-enters `pr-watch`. A justified
  deferral stays unticked for the human. Exhaustion drafts the PR with
  `AFK:checks-failed`.

In the Bootstrap state there is no provider to boot, so no round runs: the PR
is labelled `AFK:verify-human` and a human verifies it. A round that was owed
and did not run is never reported as a pass; the PR is drafted with
`AFK:checks-failed` and a comment.

## PR reconcile

The triage ([`lib/pr-triage.sh`](../../lib/pr-triage.sh)) costs no Claude
usage when nothing needs attention. It considers only open, non-draft PRs on
a branch shape the harness creates (`feat/issue-<N>` or `research/<slug>`)
authored by the Machine user. One is picked, in this order, oldest first
within a reason:

1. `revise`: it carries `AFK:revise`.
2. `ruling`: it carries `AFK:ruling` and your reply to its Ruling request
   parses as a Ruling (`1A 2B`, or a partial `1A`). The reply is the whole
   trigger: nothing is relabelled, and the Work Probe wakes a sleeping Daemon
   on it through this same triage.
3. `conflict`: its mergeable state is `CONFLICTING`. No label is needed.
4. `docs-merge`: every file it changes is under `docs_research_prefix`. It is
   not reconciled; the docs-only gate squash-merges it when CI is green.
5. `incomplete`: the review marker or a Verification round comment is
   missing, because an earlier Fire died mid-tail.

Bot PRs rank below all of these. PRs carrying `AFK:revise-failed`,
`AFK:rebase-failed` or `AFK:deps-failed` are skipped. While any Agent PR
still needs machine work, no new ticket is picked; a PR that is only waiting
for a human merge blocks nothing, and neither does one waiting on a Ruling:
`AFK:ruling` with no reply (or a reply that is not a Ruling) earns no pick by
itself and hides the PR from none of the reasons above.

`/auto-agent:pr-reconcile` rebuilds its context from the issue, the diff and
the review threads, then:

1. **Rebase** (only when conflicting, one attempt): rebase onto the default
   branch, the implementer resolving conflict stops, published with
   `git push --force-with-lease`, the only force push the harness makes. A
   refused lease or an unresolvable conflict parks the PR `AFK:rebase-failed`.
2. **Comments** (only with `AFK:revise`, cap `rounds.revise`, default 3): the
   implementer addresses every unresolved thread, the session commits
   `fix(review): round <R> …` and pushes, replies in each thread
   `fixed in <sha>: <what changed>` and resolves it. When all are resolved
   `AFK:revise` is dropped. A fix still failing at the cap parks the PR
   `AFK:revise-failed`. What only the human can decide — a product
   ambiguity, a dispute on a thread the human wrote — does not park: it is
   collected as a decision, and after the tail one consolidated **Ruling
   request** (`lib/ruling.sh`) is posted with the tail's evidence and
   `AFK:ruling` applied. The human answers in one line (`1A 2B`); the next
   reconcile applies exactly those letters, resolves the threads with marked
   replies, posts a `Ruling applied` comment, re-runs the tail if code
   changed, and drops `AFK:ruling`. A partial answer applies what it names
   and re-asks the rest; free text gets one nudge and changes nothing.
3. **Tail**: any push staled the evidence, so the whole verification tail
   re-runs and re-verifies every checklist item, ticked ones included. It
   runs on the fixed head even when a Ruling request is about to go out, so
   the all-recommended answer can leave the PR ready to merge.

During a reconcile the backing issue swaps `AFK:done` for `AFK:in-progress`
and gets `AFK:done` back on exit, also after a crash.

### Human workflow

1. Review the Agent PR. To hand it back, leave **inline** review comments and
   apply `AFK:revise`. A top-level comment alone gives the machine no thread
   to work.
2. Do nothing about conflicts: the next Fire rebases.
3. If a fix missed the point, re-open the thread and re-apply `AFK:revise`.
4. A PR labelled `AFK:verify-human` needs you to verify it by hand.
4b. A PR labelled `AFK:ruling` needs one line from you: reply to its Ruling
   request comment with one letter per decision (`1A 2B`). The recommended
   letters are marked; nothing else in the reply is read. The Dashboard
   shows the wait as a `Ruling · N decisions` badge linking that comment.
5. A parked PR (`AFK:revise-failed`, `AFK:rebase-failed`, or a draft with
   `AFK:checks-failed`) is yours. It is not picked again while the label is
   present or, for `AFK:checks-failed`, while it is a draft.
6. When satisfied, approve and merge. PR titles are written as
   conventional-commit subjects so a squash merge reads well.

## Label taxonomy

The names are constants in
[`lib/harness-config.sh`](../../lib/harness-config.sh);
`bin/auto-agent labels-ensure` creates any that are missing.

| Label | On | Set by | Cleared by |
| --- | --- | --- | --- |
| `AFK` | issue | A human or the planning skills | Stays; the resolve lane removes it when it relabels `HITL` |
| `AFK:in-progress` | issue | Pickup: a pick, a resume, a resolve, a reconcile | Dispatch on completion or failure; pickup after a reconcile; the Fire wrapper after a crash or a pause |
| `AFK:paused` | issue | The Fire wrapper, when budget or the credential ran out mid-pick (`wip:` commit, branch kept) | The next resume, or the resume cap (to `AFK:failed`) |
| `AFK:done` | issue | Dispatch or the resolve lane, on success | Swapped out for the length of a reconcile |
| `AFK:failed` | issue | Dispatch, pickup, the resolve lane, the resume cap, or the wrapper after a crash | A human; removing it requeues an open ticket |
| `AFK:revise` | PR | A human review, or `pr-review` with findings | `pr-reconcile`, when every thread is resolved |
| `AFK:revise-failed` | PR | `pr-reconcile`, a fix still failing at the round cap (never a decision awaiting the human); the PR comment says which. A dispute alone never parks: the Arbiter rules it | A human |
| `AFK:ruling` | PR | `pr-reconcile` or pickup, with a Ruling request the human must answer (`1A 2B`) | `pr-reconcile`, when the Ruling is applied; re-applied for the rest of a partial answer |
| `AFK:rebase-failed` | PR | `pr-reconcile`, rebase failed or lease refused | A human |
| `AFK:checks-failed` | PR | `pr-watch`, the manual fix loop, or a round that never ran; the PR is drafted | A human |
| `AFK:deps-failed` | PR | The deps-land lane, fix budget spent; the PR is drafted | A human |
| `AFK:verify-human` | PR | The tail in the Bootstrap state; `pr-review` on a Harness config change | A human, after verifying |
| `AFK:needs-human` | issue | The Daemon when it parks | The issue is closed on un-park |
| `HITL` | issue or PR | A human or the planning skills; the resolve lane; the deps-land lane on a major bump | A human |

`spec`, `wayfinder:map` and the `wayfinder:<type>` labels are planning
vocabulary created by the same command.

## The deps-land lane

On only when the Harness config declares a `dependabot` block.
`/auto-agent:deps-land` drives one Bot PR per Fire: retitle, inject the
Target Project's `bot-pr-checklist.md`, CI green (`pr-watch --bot`), one
Verification round that must pass with no deferral, then the deps gate, whose
merge command the pickup skill runs. Fixes are capped at `rounds.deps_fix` in
total (default 3). A major bump gets `HITL` and waits for an approving
review. See [README: The deps-land lane](../../README.md#the-deps-land-lane).

## The resolve lane

A picked ticket labelled `wayfinder:research` or `wayfinder:task` is a
Decision ticket, and goes to `/auto-agent:afk-resolve` instead of dispatch.
For research it writes its findings under `docs_research_prefix` on a
`research/<slug>` branch, opens a `docs(research):` PR, greens it, merges it
through the docs-only gate, comments the answer on the ticket, closes it,
appends the Map's Decisions so far and graduates at most three new tickets
out of the fog. A ticket that turns out to need product code is relabelled
`HITL`. A resolve is never paused: running out of budget drops the lock and
the pushed branch, and the next Fire restarts it. A crash marks it
`AFK:failed`.

## The Deployed tier

Optional, on when the config declares `verification.deployed` and `enabled`
is not false. It fills only an otherwise idle Fire: `/auto-agent:verify-deploy`
runs a merged Agent PR's `post-deploy` items read-only against the live
environment the deployed command's `status` reports, and never calls `up` or
`down`. See [README: The Deployed tier](../../README.md#the-deployed-tier).

## Testing

`bash run-tests.sh` runs every `*.test.sh` and `*.test.py` suite. The libs
are sourceable bash with `gh`, `git` and `claude` injected through `GH_BIN`,
`GIT_BIN` and `CLAUDE_BIN`; the Rebase Driver's suite builds real throwaway
repositories instead, because rebase and lease behaviour is what it tests.
`bin/auto-agent runbook-check` fails when a skill loses a load-bearing rule
or gains a Target Project literal. Three Fire kinds exercise the loop without
writing to GitHub:

```sh
bin/auto-agent fire --noop                   # the plugin loads
bin/auto-agent fire --dry-run <target-dir>   # the pick verdict, no write
bin/auto-agent pickup-triage <target-dir>    # the read-only triage JSON
```

The whole path, from a new VM to a green Fire, is the
[Proxmox end-to-end runbook](../runbooks/e2e-proxmox.md).

## Operational notes

- **What is it doing?** The Dashboard, `daemon-state.json` in the State dir,
  or `journalctl -u auto-agent-daemon`.
- **Deploying a change.** A Harness config change takes effect when it is
  merged to the default branch, since every Fire resets to it and validates
  the config first. A harness change reaches a Host through
  `bin/auto-agent upgrade`, which restarts both units.
- **Tuning.** Host env keys, with defaults: `AUTO_AGENT_GATE_MIN_PCT` (25),
  `AUTO_AGENT_DAEMON_FAIL_CAP` (3), `AUTO_AGENT_WORK_PROBE_INTERVAL` (300),
  `AUTO_AGENT_PARK_REPROBE_SECS` (3600). Every Fire runs on the latest Opus
  at medium effort; `AUTO_AGENT_FIRE_MODEL` pins a different model and
  `AUTO_AGENT_FIRE_EFFORT` a different effort. The round caps are in the
  Harness config's `rounds` block.
- **Stuck lock.** The wrapper clears the lock its own Fire took. If one is
  left behind: `gh issue edit <N> --remove-label AFK:in-progress`.
- **Stop the Daemon.** `sudo systemctl stop auto-agent-daemon`, and
  `start` to resume. `AFK:paused` is a per-issue state, not a switch.
