---
status: accepted
---

# The Dashboard is per-Host, reads Fire records and the sensor's JSON, and never re-implements the Daemon

The Smart Smoker Dashboard is a stdlib Python server that scrapes the Daemon's
journal lines with regexes, reconstructs Fire history from log filenames, and
carries its own Python copy of the budget gate, with the checkout path, repo
slug, log dir, node PATH and transcript dir hard-coded. Tenancy fixed one
Daemon per Host with no coordinator, so we decided the Dashboard is **one per
Host**, always installed, showing only its own Daemon, with `/api/status`
kept as the stable JSON seam a later aggregator reads; a central page is fog
until a second Host exists. The Dashboard **re-implements nothing**: the
Daemon writes a **Fire record** (JSON: start, kind, issue, exit, gate verdict)
into the State dir for every Fire and the Dashboard reads those for history
and current state, keeping `journalctl` only for the live tail; the bash usage
sensor gains a JSON output the Dashboard shells to, so verdict and per-auth-mode
degrade path are defined once (a setup-token Host shows "no usage sensor in
this auth mode" plus the last Fire record's verdict, never a hidden tile).
Every constant becomes the split ADR 0002 made for the Daemon: repo facts from
`harness.json`, machine facts (paths, port, bind address, model policy,
tokens) from the Host env, and the Dashboard reads no value the Daemon does
not. Setup's Ansible configure step writes its system unit beside the
Daemon's, ExecStart from the Harness install, same `EnvironmentFile`. It binds
**loopback by default**; the Host env opts into all interfaces, which the
Proxmox entry point does when it also installs tailscale, because a
bring-your-own VM cannot be assumed to have the ufw-plus-tailnet shape the
live box relies on. All tiles carry over: Maps is harness vocabulary, the
Haiku summary of the in-flight transcript stays behind a Host env toggle
(default on) as the only view into a Fire while `claude --print` buffers, and
the one-off `verify-deploy.sh` moves to harness self-testing.

## Considered options

- **Central Dashboard polling N Hosts**: nothing it shows is cross-Host today;
  deferred behind the JSON seam.
- **Keep journal scraping**: harness owns both ends, but every log wording
  change breaks history and a `team-pickup-` compatibility shim already exists.
- **Keep the Python gate copy**: two implementations of a per-auth-mode gate
  would drift on the first mode added.
- **Container**: ruled out with the Host runtime decision.
