# Auto Agent

A reusable autonomous agent harness: a Daemon that picks up wayfinder-format
GitHub issues labelled `AFK` in a Target Project and drives them to merged PRs.
Extracted from the Smart-Smoker-V2 harness, whose agent-harness terms it
inherits.

The glossary describes the Go redesign (Spec #102). Until each Target Project
is cut over, the bash Daemon under `lib/` still uses the terms that redesign
retired: Host env, Setup, Harness install, Provisioner, Operator machine,
Host inventory, Host extension.

## Language

### Agent harness

**Daemon**: The always-on, usage-paced loop that picks and works AFK tickets
without a human present. _Avoid_: bot, routine, cron job, loop

**Fire**: One Daemon pass that advances exactly one ticket or pull request as
far as it can go before it must wait: for CI, for a human, or for the budget.
_Avoid_: run, tick, iteration

**Step**: One bounded agent call inside a Fire, handed a typed input and
returning a checked result. A Step never writes to GitHub; the Daemon acts on
its result. _Avoid_: turn, stage, task, subagent

**AFK ticket**: An issue the Daemon may pick up alone: labelled `AFK` and
carrying whatever pick signal the Target Project's tracker config names.
_Avoid_: agent issue, ready-for-agent

**HITL ticket**: An issue that only resolves through live exchange with a
human; the Daemon never picks it. _Avoid_: manual issue, human issue

**Map**: The single issue that indexes one planning effort: its Destination,
Decisions so far, Fog and Out of scope. _Avoid_: epic, PRD, tracking issue

**Decision ticket**: A child of a Map whose resolution is a decision or a
fact, not a change to the product. _Avoid_: task, story

**Spec**: The issue produced at a Map's Destination that Slices are cut from
and reviewed against. _Avoid_: PRD

**Slice**: A tracer-bullet implementation ticket cut from a Spec: one narrow
end-to-end path, demoable alone. _Avoid_: task, story, sub-issue

**Agent PR**: A pull request the Daemon opened for an AFK ticket; issue-backed
and run through review and verify rounds. _Avoid_: team PR, our PR

**Lane**: One of the Daemon's work paths through a Fire, chosen by what it
picks: the core Agent PR loop, resolving a Decision ticket, landing a bot PR,
or a deployed verification. Core lanes are always on; optional lanes are on
when the Harness config declares them. _Avoid_: mode, flow, pipeline

**Bot PR**: A pull request Dependabot opened, on a `dependabot/…` branch; no
backing issue, judged by evidence (CI and one verification round) rather than
review, and landed by the deps-land lane behind the deps gate. _Avoid_:
dependency PR, bump PR

**Single-flight lock**: The `AFK:in-progress` label: while any issue in the
Target Project holds it, every other Fire skips. _Avoid_: mutex, busy flag

**Budget gate**: The Daemon's go-or-wait decision before every Step, chosen
per auth mode: the usage endpoint where the credential allows it, otherwise
the rate-limit event the last Step's stream carried. _Avoid_: throttle, rate
limiter, pacer

**Usage sensor**: The one implementation that reads the account's real
limits where the auth mode allows it and emits a Gate verdict; the Dashboard
shells to it rather than re-implementing it. _Avoid_: usage API, quota check

**Gate verdict**: What the Budget gate emits for one Step (auth mode, sensor,
state, remaining percent, reset time, go decision, limits, warnings, model
switch), written into the Fire record. _Avoid_: usage snapshot, budget status

**Outcome**: How a Step ended, as the Daemon read it from the result and the
stream: OK, EXHAUSTED (paused, sleep to the reset), AUTH_DEAD (paused, the
Daemon parks), or a failed attempt (MALFORMED, TIMED_OUT, FAILED). Written
into the Fire record. _Avoid_: status, result (that is claude's result
event), classification

**Model policy**: The Host config's rule for a spent per-model limit: the
primary model family the Steps run on, the fallback they switch to, and the
utilization that switches; a per-model limit never gates a Step, it changes
the model until its reset. _Avoid_: model fallback, downgrade

**Parked**: The Daemon state when its Claude credential is dead: no Fires,
one `AFK:needs-human` issue open in the Target Project, an hourly probe that
un-parks it when a human has logged in again. _Avoid_: halted, disabled,
exhausted (that is a budget state, not a credential state)

### Review and ruling

**Finding**: One thing the one-time review of an Agent PR noticed, sorted by
the posting bar into one of three kinds: a defect, a product ambiguity or a
Review note. Only a defect opens a thread. _Avoid_: comment, issue, nit

**Defect**: A Finding that earns a thread: a concrete failure (observable
wrong behaviour, crash, data loss or a security hole, with the inputs that
trigger it) or a written requirement contradicted and quoted. Severity is not
the bar. _Avoid_: bug (as the general term), high-severity finding

**Review note**: A Finding below the bar (a Standards point, a test-coverage
gap nobody asked for, a "defensible either way"), listed once in the review's
done-marker comment and acted on by nobody; a notes-only review applies no
`AFK:revise`. _Avoid_: nit, suggestion, minor finding

**Dispute**: The implementer's in-thread reply that a bot-authored defect
thread is wrong; what sends a thread to the Arbiter instead of a fix round.
_Avoid_: pushback, rejection, won't-fix

**Arbiter**: The read-only plugin agent that rules a Dispute on a bot-authored
thread inside the same Fire (`fix`, `dismiss`, or product ambiguity), fresh
context, never the implementer's. _Avoid_: judge, referee, second reviewer

**Product ambiguity**: A point where the issue and the Spec are silent or
contradict each other and the options differ in behaviour a user of the
Target Project would see; never a thread, always a Ruling request decision.
_Avoid_: open question, spec gap, unclear requirement

**Ruling request**: The one consolidated comment that carries everything on a
PR that needs the human (product ambiguities and disputes on human-authored
threads), each decision with lettered options and a recommendation, labelled
`AFK:ruling` while outstanding. _Avoid_: escalation, hand-off, question for
the human

**Ruling**: The human's one-line answer to a Ruling request (`1A 2B`), applied
by the Daemon exactly as written. _Avoid_: decision (that is one item in the
request), approval, answer

### Reuse

**Target Project**: The repository the Daemon works on. It is never this
repo; it is the project that installs the harness. _Avoid_: client repo,
consumer, downstream

**Host**: The machine a Daemon runs on. It must already be able to run the
Target Project and its Verification Harness; the Daemon installs nothing on
it. _Avoid_: VM, box, runner (as general terms)

**Harness config**: The per-Target-Project declaration the Daemon reads
instead of hard-coded knowledge: repo, pick signal, branch shapes, commands,
and the Verification Harness hook. Committed in the Target Project.
_Avoid_: settings, profile

**Machine user**: A dedicated GitHub account a Daemon acts as, so that its
work is a second party to the operator's. Recommended, not required: a Daemon
acts as whoever its token belongs to. _Avoid_: bot account, service account,
the PAT (the token is a credential, the machine user is the identity)

**Host config**: The non-secret settings one Host keeps for one Target
Project, read alongside the Harness config: budget gate, model policy,
Dashboard address, State dir. Every key has a default; it is never committed
and never holds a credential. _Avoid_: Host env, env file, daemon settings

**Verification Harness**: The Target Project's own way of proving a change
works live (bring-your-own): a command the Daemon calls with a fixed contract
and reads a verdict from. _Avoid_: smoke suite, e2e, test harness (as a
synonym for unit tests)

**Environment provider**: The Target Project's command behind the hermetic
tier: brings a per-PR environment up, reports where its Surfaces are, and
tears it down. _Avoid_: stack runner, compose wrapper, test harness

**Bootstrap state**: A Target Project whose Harness config has no hermetic
tier yet: the Daemon still works its tickets, every Agent PR waits for a human
verifier, and the Dashboard warns until a provider is merged. _Avoid_: degraded
mode, unverified mode

**Bootstrap issue**: The single AFK ticket a Target Project in the Bootstrap
state carries, asking the Daemon to write its Environment provider from the
reference providers; identified by a body marker, so it is opened once and
reused for ever. _Avoid_: provider ticket, setup issue

**Provider check**: The harness-owned conformance run that drives an
Environment provider through its contract and prints one verdict, without a
checklist round. _Avoid_: provider test, stub, dry run

**Surface**: One thing the verifier can drive in a Target Project (a browser
UI, an Electron app, a CLI, an API), declared in the Harness config with the
paths that mark it touched. _Avoid_: app, target, frontend (as the general term)

**Verification round**: One pass of the checklist protocol over an Agent PR:
the unchecked items exercised on the declared Surfaces in an environment booted
for that PR, the passing boxes ticked, one evidence comment, one
`manual-verify:` line. _Avoid_: manual test, QA pass, smoke (that is the
provider's own sub-hook)

**Deployed round**: A Verification round's twin in the Deployed tier: one pass
of the same checklist protocol over a merged Agent PR's deferred
(`post-deploy`) items, run read-only against the live environment the deployed
command's `status` reports, never an environment booted for the PR.
_Avoid_: post-deploy smoke, prod check

**Screenshot tour**: The ordered set of screenshots a round captures of every
touched `browser` or `electron` Surface, posted into the PR description and
refreshed by each later round. Evidence for humans, never a verdict.
_Avoid_: screenshots (as the general term), snapshots, visual test

**Evidence sink**: The harness-owned home of a round's artifacts: one directory
per round in the State dir, one screenshot filename grammar, and the one
section of the PR body the tour is written into. _Avoid_: artifacts dir,
uploads

**Hermetic tier**: The Verification Harness tier that runs the checklist round
in an environment the Environment provider booted for this PR alone.
_Avoid_: e2e, integration environment

**Deployed tier**: The optional Verification Harness tier that runs deferred
checklist items read-only against a live environment. _Avoid_: prod check,
post-deploy smoke

**State dir**: The directory on a Host, one per Target Project and outside
any human's checkout, where the Daemon keeps its own clone, Fire history and
logs; the Dashboard reads it, nothing in the Target Project does.
_Avoid_: log dir, work dir, cache

**Workspace**: The Daemon's own clone of the Target Project, inside the
State dir: the only checkout it works in, reset before each Fire (keeping
only the paths the Harness config names) and never a directory a human
uses. _Avoid_: target checkout, worktree, sandbox

**Fire record**: What the Daemon writes to the State dir for each Fire: its
ticket or PR and the Steps it ran, each with its Outcome, gate verdict and
usage. The Dashboard's source for history and current state. _Avoid_: fire
log (the log is the transcript, the record is the summary), status file

**Dashboard**: The read-only status page one Host serves for its own Daemon:
Fire history, queue, open Agent PRs, budget gate; its JSON route is the seam
any aggregation reads. _Avoid_: monitor, console
