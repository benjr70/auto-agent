# Auto Agent

A reusable autonomous agent harness: a Daemon that picks up wayfinder-format
GitHub issues labelled `AFK` in a Target Project and drives them to merged PRs.
Extracted from the Smart-Smoker-V2 harness, whose agent-harness terms it
inherits.

## Language

### Agent harness

**Daemon**: The always-on, usage-paced loop that picks and works AFK tickets
without a human present. _Avoid_: bot, routine, cron job, loop

**Fire**: One Daemon pass that does exactly one unit of work (reconcile a PR,
resume paused work, pick an issue, or resolve a Decision ticket).
_Avoid_: run, tick, iteration

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

**Single-flight lock**: The `AFK:in-progress` label: while any issue in the
Target Project holds it, every other Fire skips. _Avoid_: mutex, busy flag

**Budget gate**: The Daemon's fire-or-wait decision before every Fire, chosen
per auth mode: the usage endpoint where the credential allows it, otherwise
the rate-limit event the last Fire's stream carried, with the limit strings
the last Fire produced as the text-mode fallback. _Avoid_: throttle, rate
limiter, pacer

**Usage sensor**: The one implementation that reads the account's real
limits where the auth mode allows it and emits a Gate verdict; the Dashboard
shells to it rather than re-implementing it. _Avoid_: usage API, quota check

**Gate verdict**: The JSON the Budget gate emits for one Fire (auth mode,
sensor, state, remaining percent, reset time, fire decision, limits,
warnings, model switch), written into the Fire record. _Avoid_: usage
snapshot, budget status

**Outcome**: How a Fire ended, as the exhaustion classifier read it from the
exit code, the tapped rate-limit event and the output: OK, EXHAUSTED (paused,
sleep to the reset), AUTH_DEAD (paused, the Daemon parks) or FAILED (the lock
is cleared for triage). Written into the Fire record; seeds the next Gate
verdict on a setup-token Host. _Avoid_: status, result (that is claude's
result event), classification

**Model policy**: The Host env's rule for a spent per-model limit: the
primary model family the Fires run on, the fallback they switch to, and the
utilization that switches; a per-model limit never gates a Fire, it changes
the model until its reset. _Avoid_: model fallback, downgrade

**Parked**: The Daemon state when its Claude credential is dead: no Fires,
one `AFK:needs-human` issue open in the Target Project, an hourly probe that
un-parks it when a human has logged in again. _Avoid_: halted, disabled,
exhausted (that is a budget state, not a credential state)

### Reuse

**Target Project**: The repository the Daemon works on. It is never this
repo; it is the project that installs the harness. _Avoid_: client repo,
consumer, downstream

**Host**: The machine a Daemon instance runs on, one per Target Project, with
its own checkout, Claude login and GitHub identity; the reference shape is an
Ubuntu VM, wherever it lives. _Avoid_: VM, box, runner (as general terms)

**Provisioner**: The optional Setup front that creates a Host from nothing
before the configure step; Proxmox is the first. _Avoid_: terraform, infra

**Host extension**: The Target Project's executable that Setup runs on the
Host after the base needs are installed, to add whatever its Verification
Harness needs beyond them. _Avoid_: post-install script, provision hook

**Harness config**: The per-Target-Project declaration the Daemon reads
instead of hard-coded knowledge: repo, pick signal, branch shapes, commands,
and the Verification Harness hook. Committed in the Target Project.
_Avoid_: settings, profile

**Machine user**: The dedicated GitHub account a Daemon acts as, one per
GitHub owner, an admin collaborator on every Target Project it serves; never
the operator's own account. _Avoid_: bot account, service account, the PAT
(the token is a credential, the machine user is the identity)

**Host env**: The per-Host declaration of account and machine facts the
Daemon reads alongside the Harness config: tokens, budget gate, model policy,
harness ref, paths. Never committed anywhere. _Avoid_: env file, secrets file,
daemon settings

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

**Provider check**: The harness-owned conformance run that drives an
Environment provider through its contract and prints one verdict, without a
checklist round. _Avoid_: provider test, stub, dry run

**Surface**: One thing the verifier can drive in a Target Project (a browser
UI, an Electron app, a CLI, an API), declared in the Harness config with the
paths that mark it touched. _Avoid_: app, target, frontend (as the general term)

**Hermetic tier**: The Verification Harness tier that runs the checklist round
in an environment the Environment provider booted for this PR alone.
_Avoid_: e2e, integration environment

**Deployed tier**: The optional Verification Harness tier that runs deferred
checklist items read-only against a live environment. _Avoid_: prod check,
post-deploy smoke

**State dir**: The per-Host directory outside the checkout where the Daemon
keeps Fire history, logs and worktrees; the Dashboard reads it, nothing in the
Target Project does. _Avoid_: log dir, work dir, cache

**Fire record**: The JSON the Daemon writes to the State dir for each Fire
(start, kind, issue, exit, gate verdict); the Dashboard's source for history
and current state. _Avoid_: fire log (the log is the transcript, the record is
the summary), status file

**Dashboard**: The read-only status page one Host serves for its own Daemon:
Fire history, queue, open Agent PRs, budget gate; its JSON route is the seam
any aggregation reads. _Avoid_: monitor, console

**Setup**: The guided process that turns a Target Project plus a Host into a
running Daemon instance: a conversation on the Operator machine driving
converging, re-runnable stages that alone write to the Host. _Avoid_: install,
onboarding, bootstrap

**Operator machine**: The developer's own computer where Setup is driven
from for the remote entry points; it holds the Host inventory and never a
secret at rest. _Avoid_: laptop, dev box, control node

**Host inventory**: The non-secret per-Host record on the Operator machine
that lets Setup reach a Host again for an upgrade or a check. _Avoid_:
inventory file, hosts.ini, config

**Harness install**: The pinned checkout of this repo on a Host that the
Daemon runs from and that an upgrade moves; the plugin is one directory inside
it. _Avoid_: clone, vendor, plugin (for the whole thing)
