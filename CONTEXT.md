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

**Single-flight lock**: The `AFK:in-progress` label: while any issue in the
Target Project holds it, every other Fire skips. _Avoid_: mutex, busy flag

### Reuse

**Target Project**: The repository the Daemon works on. It is never this
repo; it is the project that installs the harness. _Avoid_: client repo,
consumer, downstream

**Host**: The machine or container a Daemon instance runs on, one per Target
Project, with its own checkout, Claude login and GitHub identity.
_Avoid_: VM, box, runner (as general terms)

**Harness config**: The per-Target-Project declaration the Daemon reads
instead of hard-coded knowledge: repo, pick signal, branch shapes, commands,
and the Verification Harness hook. _Avoid_: settings, profile

**Verification Harness**: The Target Project's own way of proving a change
works live (bring-your-own): a command the Daemon calls with a fixed contract
and reads a verdict from. _Avoid_: smoke suite, e2e, test harness (as a
synonym for unit tests)

**Dashboard**: The read-only status page for one or more Daemon instances:
Fire history, queue, open Agent PRs. _Avoid_: monitor, console

**Setup**: The guided process that turns a Target Project plus a Host into a
running Daemon instance. _Avoid_: install, onboarding, bootstrap

**Harness install**: The pinned checkout of this repo on a Host that the
Daemon runs from and that an upgrade moves; the plugin is one directory inside
it. _Avoid_: clone, vendor, plugin (for the whole thing)
