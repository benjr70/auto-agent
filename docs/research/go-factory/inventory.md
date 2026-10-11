# Inventory: every module and skill sorted into Go, agent step, or dropped

Resolves [issue #94](https://github.com/benjr70/auto-agent/issues/94) for the
map [auto-agent as a Go software factory](https://github.com/benjr70/auto-agent/issues/90).
Read from `origin/main` at `8ef7d6c` on 2026-10-11 (after the Ruling
request Slices merged). Line counts are of the module, then its test suite.

## How to read the fate column

- **Go**: deterministic code in the binary. No model call.
- **Agent step**: a bounded call to Claude Code with a typed input and a
  schema-checked result. The prose that remains is the step's prompt.
- **Go + agent**: Go owns the flow and the GitHub plumbing; an agent is called
  for the one part that needs judgement.
- **Owned skill**: prose a human drives interactively (planning). Stays a
  skill, owned and edited in this repo.
- **Dropped**: goes with the Proxmox, remote Setup and machine-setup removal.
- **Retired**: exists only to guard something the redesign removes.

## Totals

| Area | Files | Lines (code / tests) | Fate in one line |
|---|---|---|---|
| `lib/*.sh` | 46 modules, 45 suites | 13.0k / 15.0k | 38 to Go, 3 Go + agent, 3 dropped, 2 retired |
| `plugin/skills/` | 18 skills | 6.7k | 10 lane runbooks dissolve into Go plus step prompts, 6 planning skills stay, 2 go |
| `plugin/agents/` | 5 agents | 571 | all become step prompts |
| `plugin/hooks/` | 3 hooks | 157 / 149 | 2 become Go checks, 1 becomes a prompt line |
| `dashboard/` | server + page | 843 + 409 / 701 | Go, same JSON route |
| `infra/` | terraform, Ansible, e2e, cutover, systemd | about 2.4k | dropped, except the two unit templates |

## 1. The Daemon loop and one Fire

| Module | Lines | What it does | Fate | Depends on |
|---|---|---|---|---|
| `daemon.sh` | 357 / 510 | The always-on loop: gate, fire, sleep, wake on work, fail cap. | Go | fire, usage-sensor, sleep-planner, daemon-park, single-flight-lock, host-env |
| `fire.sh` | 587 / 853 | One Fire: loads config, runs `claude -p` in stream-json through the tap, classifies the Outcome, clears the lock on a crash, writes the Fire record. | Go. Becomes the one agent-step runner; today it prompts a single skill that does everything. | fire-record, rate-limits-tap, exhaustion-classifier, usage-sensor, daemon-park, surface-launch, host-env |
| `fire-record.sh` | 187 / 133 | Writes the Fire record JSON into the State dir. | Go (the local journal) | none |
| `sleep-planner.sh` | 71 / 173 | Pure function: how long to sleep and how to poll after. | Go | none |
| `work-probe.sh` | 250 / 271 | "Did work appear?" using only `gh`, between sleep chunks. | Go. The ETag polling from the webhook research replaces its fetch. | pr-triage |
| `single-flight-lock.sh` | 28 / none | Reads the `AFK:in-progress` label. | Go | none |
| `pause-resume.sh` | 66 / 52 | Pure function: resume, give up, or pick fresh, from the pause count. | Go | none |
| `daemon-park.sh` | 198 / 158 | Parked state: one `AFK:needs-human` issue, hourly `claude auth status` probe, un-park. | Go | host-env |

## 2. Budget gate

| Module | Lines | What it does | Fate | Depends on |
|---|---|---|---|---|
| `usage-sensor.sh` | 505 / 360 | The one sensor per auth mode; emits the Gate verdict. | Go | rate-limits-tap, exhaustion-classifier, daemon-park |
| `rate-limits-tap.sh` | 101 / 121 | Passes the stream through and records each `rate_limit_event`. | Go (part of the step runner's stream reader). Reads four fields the headless research found undocumented. | none |
| `exhaustion-classifier.sh` | 157 / 129 | Pure function: OK, EXHAUSTED, AUTH_DEAD or FAILED from exit code, output and the tapped event. | Go | none |
| `token-usage.sh` | 271 / 328 | Per-ticket token totals from session transcripts, keyed on branch name. | Go. With one call per step, usage can come from each step's result instead of transcript scraping. | none |

## 3. Config

| Module | Lines | What it does | Fate | Depends on |
|---|---|---|---|---|
| `harness-config.sh` | 437 / 336 | The only reader of `.auto-agent/harness.json`; validates and exports. | Go, reading the same file unchanged. `plugin/schema/harness.schema.json` (141) and `validate.jq` (127) fold into it. | none |
| `host-env.sh` | 81 / 94 | Reads the Host env file for by-hand runs. | Go, shape to be redesigned (map decision). | none |
| `labels-ensure.sh` | 107 / 83 | Creates the fixed label vocabulary, create-if-missing. | Go | none |
| `pick-publish.sh` | 225 / 183 | Puts a ticket on, or takes it off, the pick signal (board or labels). | Go, kept as a subcommand: the planning skills call it. | none |

## 4. Picking and routing

| Module | Lines | What it does | Fate | Depends on |
|---|---|---|---|---|
| `pickup-triage.sh` | 405 / 571 | The whole read-only pre-pick decision tree in one call, one JSON verdict. | Go. This verdict is the state machine's entry transition. | pr-triage, deployed-tier |
| `pr-triage.sh` | 877 / 2191 | Which open Agent PR needs reconciling and why (conflict, revise, incomplete, ruling, docs-merge, dependabot, both). | Go. The reason vocabulary is the best existing sketch of the PR states. | review-poster, ruling, deps-lane, docs-only-gate, docs-research-paths, harness-config |
| `bootstrap-state.sh` | 318 / 263 | Detects the Bootstrap state and opens or reuses the Bootstrap issue. | Go | none |

## 5. The Agent PR loop

| Module | Lines | What it does | Fate | Depends on |
|---|---|---|---|---|
| `ci-wait.sh` | 149 / 228 | Blocks until a PR's checks settle, with no agent turns. | Go | none |
| `pr-checks.sh` | 64 / 60 | One reading of a PR's check buckets, shared by the merge gates. | Go | none |
| `rebase-driver.sh` | 138 / 327 | Rebase onto the default branch and push with `--force-with-lease`; the only force-push site. | Go + agent: Go attempts the rebase and owns the push; an agent resolves conflicts. | none |
| `review-poster.sh` | 385 / 813 | Renders, posts and tracks the automated review and its two markers. | Go | none |
| `thread-reconciler.sh` | 235 / 301 | Enumerates, replies to and resolves review threads (GraphQL). | Go + agent: Go fetches, posts and resolves; an agent reads the threads and decides each reply or fix. | review-poster |
| `arbiter-verdicts.sh` | 124 / 118 | Parses the Arbiter's verdicts and the implementer's replies; round arithmetic. | Go. Becomes the Arbiter step's result schema. | none |
| `ruling.sh` | 486 / 298 | Composes, posts, parses and applies the Ruling request and the Ruling. | Go + agent: Go owns the grammar, posting and parsing; an agent writes each decision's text. | work-probe |
| `docs-only-gate.sh` | 183 / 658 | Decides whether a PR may merge as docs-only, against the real diff. | Go | docs-research-paths, pr-checks, harness-config |
| `docs-research-paths.sh` | 42 / 58 | The one definition of "research docs only". | Go | none |

## 6. Verification

| Module | Lines | What it does | Fate | Depends on |
|---|---|---|---|---|
| `checklist.sh` | 131 / 115 | Parses the PR checklist and makes the one allowed tick mutation. | Go | none |
| `evidence.sh` | 233 / 130 | Evidence sink: round directory, screenshot names, the PR body section. | Go | none |
| `surfaces.sh` | 269 / 135 | Which Surfaces a PR touched, which earn a tour, at what viewport. | Go | none |
| `provider-contract.sh` | 129 / none | The Environment provider driving rules (down before up, one retry, block grammar). | Go | none |
| `provider-check.sh` | 254 / 299 | The Provider check conformance run. | Go | provider-contract |
| `verify-boot.sh` | 241 / 231 | Preflight and boot for one round: provider plus launchers, one call. | Go | provider-contract |
| `deployed-tier.sh` | 229 / 284 | The Deployed tier: which merged PRs have deferred items, and the `status` read. | Go | none |
| `surface-launch.sh` | 365 / 182 | Launchers and MCP wrappers per Surface kind (headful browser, Electron over CDP). | Go, but **open**: it assumes a display the harness set up. See the VM assumptions below. | display-env |
| `display-env.sh` | 137 / 83 | Finds the display and decides the Electron sandbox flag. | Go, but **open**, same reason. | none |

## 7. Bot PRs

| Module | Lines | What it does | Fate | Depends on |
|---|---|---|---|---|
| `deps-gate.sh` | 262 / 652 | Decides whether a Dependabot PR may be squash-merged. | Go | deps-lane, pr-checks, harness-config |
| `deps-lane.sh` | 597 / 1387 | The lane's text transforms, hidden markers, fix budget and park. | Go | checklist, deps-gate, pr-triage |

## 8. Setup, install and guards

| Module | Lines | What it does | Fate |
|---|---|---|---|
| `setup.sh` | 1128 / 440 | The in-VM Setup engine and its stages. | Mostly dropped. What survives becomes `init` and `doctor`: the Harness config scaffold, the label bootstrap, the identity and Claude login checks. |
| `setup-remote.sh` | 572 / 289 | Setup over SSH from the Operator machine; `upgrade` and `check`. | Dropped |
| `setup-provision.sh` | 405 / 413 | The Proxmox entry point. | Dropped |
| `unit-render.sh` | 122 / 155 | Renders the two systemd units from templates. | Go (`service install`), one unit once the Dashboard is in the binary. |
| `vendored-skills.sh` | 208 / 146 | Pins three upstream skills to one commit and checks drift. | Retired: the repo now owns its skills outright. |
| `runbook-check.sh` | 585 / 152 | Asserts the skills still carry their load-bearing rules and no Target Project literal. | Retired in stages: each rule it guards disappears as that rule moves from prose into Go. A small check for the remaining prompts may stay. |
| `arbiter-fixture.sh` | 96 / 142 | Runs five recorded disputes through the Arbiter (a live model call). | Go: the first golden-input test for an agent step. |

`bin/auto-agent` (315 lines) is a dispatcher over these modules with 32
subcommands. The Go binary replaces it; it is the natural seam for the
migration, because skills and tests already reach most modules through it.

## 9. Skills

The ten lane runbooks are where control flow lives in prose today. Each row
names the flow that must move into Go and the judgement that stays.

| Skill | Lines | Flow that moves into Go | What stays as agent work |
|---|---|---|---|
| `afk-pickup` | 1497 | The whole Fire: triage branch, lock apply and restore, routing by PR reason, resume, pick, branch creation, opening the PR with its body sections, chaining review then watch then verify, validating and running the merge command. | Nothing. This skill disappears. |
| `afk-dispatch` | 358 | Pre-flight, the plan / TDD / review / verify sequence, the review round cap, the commit and the failure path. | **Implement** (plan and TDD), **pre-commit review** of the staged diff. |
| `pr-watch` | 474 | The round loop and cap, waiting on CI, gathering failure logs, commit and push, the fix-attempt markers, draft on exhaust. | **Fix a CI failure** (the implementer with a brief). |
| `pr-review` | 437 | The idempotency gate, context gathering, merging the two axes, dedupe against existing threads, posting, the done-marker, the hand-off label. | **Review** on two axes (correctness and Spec). |
| `correctness-review` | 174 | None; it is already a bounded prompt. | The correctness axis of **Review**. |
| `pr-reconcile` | 872 | Pre-flight and the parked-PR refusal, the rebase phase, the per-round thread loop and round cap, when the Arbiter runs, applying its verdicts, composing the Ruling request, the verification tail. | **Resolve a rebase conflict**, **answer review threads** (fix or Dispute), **arbitrate**, **write Ruling request decisions**. |
| `verify-pr` | 353 | Bootstrap check, checklist parse, touched Surfaces, evidence directory, checkout and config re-read from the PR head, boot, ticking boxes, the evidence comment, teardown on every exit. | **Verify**: the checklist round on the Surfaces. |
| `verify-deploy` | 229 | Lane check, deferred-item parse, the read-only `status` read, reconciling the result. | **Verify**, the deployed variant. |
| `deps-land` | 498 | Pre-flight, the conflict path, retitle, checklist injection, the two tiers, gate, merge or park. | Reuses **Fix a CI failure** and **Verify**. |
| `afk-resolve` | 536 | Claim and lock, branch, commit, PR, green and merge through the docs gate, closing the ticket, pick-signal updates. | **Research** a Decision ticket and write the findings; **graduate fog** (proposing new tickets). |

| Skill | Lines | Fate |
|---|---|---|
| `wayfinder` | 384 | Owned skill, human-driven. Calls `pick-publish` and `labels-ensure`. |
| `to-spec` | 144 | Owned skill, human-driven. |
| `to-tickets` | 267 | Owned skill, human-driven. |
| `grilling` | 28 | Owned skill (was vendored). |
| `domain-modeling` | 74 | Owned skill (was vendored). |
| `research` | 12 | Owned skill (was vendored); also the prompt behind the Research step. |
| `setup` | 323 | Dropped with remote Setup. A short `init` interview for Surfaces may replace it. |
| `dry-run` | 24 | Retired: Go can confirm the plugin loaded from the stream's init message. |

## 10. Agents and hooks

| File | Lines | Fate |
|---|---|---|
| `agents/implementer.md` | 64 | Prompt for the Implement, Fix and conflict steps. |
| `agents/reviewer.md` | 77 | Prompt for the pre-commit review step. |
| `agents/verifier.md` | 102 | Mostly Go: it drives the provider (`down`, `up`, `smoke`, `down`) and writes a trailer. Only "is this smoke result honest" needs judgement, and probably not even that. |
| `agents/manual-verifier.md` | 200 | Prompt for the Verify step. |
| `agents/arbiter.md` | 128 | Prompt for the Arbitrate step. |
| `hooks/smoke-trailer.sh` | 66 | Go check after the implement step: the commit carries its `smoke:` trailer. |
| `hooks/review-gate.sh` | 55 | Go: a change-request is answered because Go runs the next round, not because a Stop hook blocks. |
| `hooks/caveman.sh` | 36 | One line in each step prompt. |
| `settings/baseline.json` | 34 | Go passes the permission settings per step. |

## 11. The rest

| Path | Fate |
|---|---|
| `dashboard/server.py`, `index.html` | Go, same `/api/status` shape. It shells to five `auto-agent` subcommands today; in one binary those become function calls. |
| `plugin/providers/` (contract, compose reference, `provider-lib.sh`) | Kept as is. These are bash a Target Project copies; they are the bring-your-own side and need not be Go. |
| `verify/provider`, `.auto-agent/` | Kept: this repo as its own Target Project. `commands.test` and `lint` change to the Go toolchain as modules move. |
| `infra/terraform/`, `infra/e2e/` | Dropped |
| `infra/ansible/` | Dropped. It installs Xvfb, an Electron AppArmor profile, Docker and Playwright's Chromium: the machine setup the map rules out. |
| `infra/cutover/` | Dropped once Smart Smoker's cut-over is finished; `soak-check.sh` is the only place that requires the token not to own the repo. |
| `infra/systemd/*.in` | Kept as templates behind `service install`. |
| `run-tests.sh`, `.github/workflows/tests.yml` | Kept through the migration, then replaced by `go test`. |
| `docs/host.md`, `docs/runbooks/e2e-proxmox.md` | Dropped. `docs/afk/` is rewritten around the state machine. |

## The agent steps this implies

Twelve candidate steps, for the state machine ticket to confirm or merge:

1. Implement an issue (plan and TDD)
2. Pre-commit review of a staged diff
3. Fix a CI failure
4. Review an Agent PR, correctness axis
5. Review an Agent PR, Spec axis
6. Resolve a rebase conflict
7. Answer review threads (fix or Dispute each)
8. Arbitrate Disputes
9. Write Ruling request decisions
10. Verify (hermetic or deployed checklist round)
11. Research a Decision ticket
12. Graduate fog into proposed tickets

Steps 1, 3, 6 and 7 all use the implementer prompt with a different brief
today, so they may be one step with four input shapes.

## What assumes the VM

| Where | Assumption | After the redesign |
|---|---|---|
| `display-env.sh`, `surface-launch.sh` | A display exists at a fixed `DISPLAY` because Ansible started Xvfb. | Detecting a display stays useful; starting one does not. `doctor` reports a missing display for a declared `browser` or `electron` Surface. |
| `surface-launch.sh` | Playwright's Chromium and the Electron runtime libraries are installed. | `doctor` checks; nobody installs. |
| `infra/ansible` AppArmor template, `display-env.sh` sandbox decision | Ubuntu's AppArmor blocks Electron's sandbox unless a profile is installed. | Open: either the Target Project's problem, or `doctor` explains it. |
| `harness-config.sh` (`host.docker`), `bootstrap-state.sh` | Setup installs Docker when the config asks. | The key becomes a `doctor` check only. |
| `setup.sh`, `unit-render.sh` | systemd system units, a dedicated Linux user, a fixed State dir path. | A user unit, or none; paths from the redesigned Host env. |
| Host extension (`.auto-agent/` executable run by Setup) | Setup runs it as root on the Host. | Nothing runs it. The term retires; the Target Project documents its own needs. |

## Facts later tickets depend on

- **The tests are of two kinds.** About 20 of the 45 suites source the module and call
  its functions directly; the rest run it as a command with stub `gh`, `git`
  and `claude` executables on `PATH`. Only the second kind can be pointed at a
  Go subcommand unchanged. The first kind needs porting to `go test` or a thin
  bash shim. This is from a grep of each suite, not a full read.
- **Two modules have no suite of their own:** `single-flight-lock.sh` and
  `provider-contract.sh` (covered through its two callers). One suite has no
  module: `pr-reconcile-runbook.test.sh` tests skill prose.
- **`pr-triage.sh` is the centre of gravity:** 877 lines, a 2,191-line suite,
  six dependencies, and the reason vocabulary every lane routes on.
- **The lock and its crash cleanup span three places:** `afk-pickup` applies
  it in prose, `fire.sh` clears it when claude exits non-zero, `daemon.sh`
  reads it. One Go owner removes the split.
- **`afk-pickup` evals a merge command scraped from a subagent's output**,
  guarded by a shape check. With Go owning the merge, that path goes away.
- **Dependency leaves, safe to port first** (pure functions, no `gh`):
  `sleep-planner`, `pause-resume`, `exhaustion-classifier`, `checklist`,
  `evidence`, `arbiter-verdicts`, `docs-research-paths`, `pr-checks`,
  `fire-record`, `host-env`.
