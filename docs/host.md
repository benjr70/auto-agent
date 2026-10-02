# Host

A Host is the machine one Daemon runs on. There is one Host per Target
Project, and each has its own Target Project checkout, its own Claude login
and its own GitHub identity. The reference shape is a VM running Ubuntu 24.04
LTS, wherever it lives ([ADR 0004](adr/0004-host-is-a-vm-with-three-setup-entry-points.md)).
A VM is the reference because the Target Project's Verification Harness may
need a real display for a browser or an Electron app, and Docker for per-PR
environments.

A Host is produced by Setup, not built by hand. This page says what Setup
does to a Host, what it leaves to you, where things end up, and how to run a
Host afterwards. The commands and their flags are documented in
[README: Setup](../README.md#setup); this page does not repeat them.

## How a Host is made

Setup has three entry points that converge on one Ansible configure step:

- **Inside the VM**: clone this repo in the VM and run `bin/auto-agent setup`.
  That clone is the Harness install.
- **Bring your own VM**: `bin/auto-agent setup --host <user@vm>` from the
  Operator machine, over SSH.
- **Proxmox**: `bin/auto-agent setup --provision proxmox --name <name>`,
  which creates the VM with terraform and cloud-init, then runs the remote
  entry point against it.

The front for all three is the `/auto-agent:setup` skill, which interviews
you and drafts the Harness config. The skill never writes to a Host; the
engine does ([ADR 0009](adr/0009-setup-is-a-skill-over-a-cli-engine.md)).
Every stage prints one `setup: <stage>: ok|changed|skipped|FAIL — <detail>`
line, and a re-run converges.

## What Setup installs and writes

The configure step ([`infra/ansible/roles/host`](../infra/ansible/roles/host/))
installs base needs derived from the Harness config, and nothing the config
does not call for:

| Need | When | What |
| --- | --- | --- |
| Base | always | `git`, `jq`, `curl`, `ca-certificates`, `python3`, `gh` |
| Display | a `browser` or `electron` Surface | Xvfb as `auto-agent-xvfb.service` on a fixed `DISPLAY` (`:99`), the reference fonts |
| Browser | a `browser` Surface | Node, and Google Chrome on x86_64 |
| Electron | an `electron` Surface | Node, the Electron runtime libraries, and an AppArmor profile at `/etc/apparmor.d/auto-agent-electron` granting user namespaces to the launcher and the Electron binary |
| Docker | `host.docker` is true | Docker and Compose, started at boot, the Host user in the `docker` group |

A Host that already resolves a working `docker compose`, or `node` and `npx`,
on the units' PATH (`AUTO_AGENT_UNIT_PATH` in the Host env) keeps its own:
Setup installs neither over it. That is what lets Setup adopt a Host built by
hand, with Docker from its vendor's repository or Node from a version manager.

Google ships no Chrome build for arm64, so on that architecture a `browser`
Surface needs the Host extension to install a browser.

It then writes:

- the **Host env**, mode 0600, with the secrets and machine facts;
- the **State dir**, mode 0700;
- the **Daemon and Dashboard units**, rendered from the templates in
  `infra/systemd/` by `bin/auto-agent unit-render`.

Other stages write the rest. The config stage sets the Target Project
checkout's local git identity to the Machine user
(`<login>@users.noreply.github.com`, with `gh` as the credential helper) and,
when the checkout has no Harness config, opens a PR proposing one from the
branch `auto-agent/harness-config`. The extension stage runs the Target
Project's `.auto-agent/host-extension` when it has one. The enable stage
enables and starts both units, only after the verify stage has passed. The
remote entry points also install the engine's prerequisites and Claude Code
on the Host, check the Harness install out at its ref, and write a Host
inventory entry on the Operator machine.

## What stays manual

Setup does not do these. Some it checks and refuses without.

- **The VM itself**, for the in-VM and bring-your-own entry points: the
  reference Ubuntu LTS release, x86_64 or arm64, systemd, outbound internet,
  and a Host user with passwordless sudo. Setup asserts this baseline, the
  distribution first, and stops if it is not met.
  For bring-your-own, your public key must already be in the VM's
  `~/.ssh/authorized_keys`.
- **The Machine user and its PAT.** Create the dedicated GitHub account, make
  it an admin collaborator on the Target Project (and a Write collaborator on
  the Project board, when the pick signal is a Project), and mint a classic
  PAT with `repo`, `project` and `workflow` in a browser. Setup verifies the
  login, the scopes and admin; it cannot create any of them. The Daemon never
  acts as your own account
  ([ADR 0005](adr/0005-daemon-identity-machine-user-and-subscription-login.md)).
- **The Claude login.** In the `login` auth mode, run `claude auth login` on
  the Host as the Host user. For the in-VM entry point this is a
  precondition; an attended remote run offers it over `ssh -t`. In the
  `setup-token` mode, run `claude setup-token` yourself and give Setup the
  token file.
- **Prerequisites on the machine you run Setup from.** The doctor and
  operator stages name each missing command with its install line and never
  install one. In the VM: `git gh jq curl python3 claude ansible-playbook`.
  On the Operator machine: `ssh ansible-playbook jq git`, plus `terraform`
  for Proxmox.
- **Tailscale**, unless you pass `--tailscale`, which installs it, joins the
  tailnet and binds the Dashboard to all interfaces.
- **A firewall.** Setup configures none. The Dashboard binds loopback by
  default for that reason; opening it is your decision
  ([ADR 0006](adr/0006-dashboard-per-host-reading-fire-records.md)).
- **Merging the Harness config PR.** The Daemon is enabled anyway, and its
  preflight fails closed until the config is on the default branch.
- **Anything project-specific.** Language toolchains, databases, application
  secrets and extra packages belong in the Target Project's Host extension,
  which must be idempotent.

Secrets are never typed into the skill's conversation or passed on a command
line. You save each one to a 0600 file and give Setup the path.

## Where things live

| Thing | Where |
| --- | --- |
| Harness install | `~/auto-agent` for the remote entry points (`--install-dir`); for the in-VM entry point, wherever you cloned this repo |
| Target Project checkout | The `<target-dir>` given to Setup; `AUTO_AGENT_TARGET_DIR` in the Host env |
| Host env | `~/.config/auto-agent/env`, mode 0600 |
| State dir | `AUTO_AGENT_STATE_DIR`, default `~/.local/state/auto-agent` |
| Fire records | `<state>/fires/<fire-id>.json` |
| Fire transcripts | `<state>/logs/<fire-id>.stream.jsonl` and `.stderr.log` |
| Daemon state | `<state>/daemon-state.json`, `<state>/gate-verdict.json`, `<state>/parked.json` while parked |
| Setup's Ansible log | `<state>/setup/configure-<time>.log` |
| Units | `/etc/systemd/system/auto-agent-daemon.service`, `auto-agent-dashboard.service`, and `auto-agent-xvfb.service` when a display is needed |
| Daemon log | `journalctl -u auto-agent-daemon` |
| Dashboard log | `journalctl -u auto-agent-dashboard` |
| Host inventory | On the Operator machine: `~/.config/auto-agent/hosts/<name>.env`. Never a secret. |
| Claude `/login` credential | Where Claude Code manages it, not in the Host env |

The Host env holds `GH_TOKEN`, `DAEMON_GH_LOGIN`, `CLAUDE_AUTH_MODE`,
`CLAUDE_CODE_OAUTH_TOKEN` in the setup-token mode, `AUTO_AGENT_TARGET_DIR`,
`AUTO_AGENT_STATE_DIR`, `AUTO_AGENT_HOST_USER`, `DISPLAY` when a display is
needed, and the Dashboard's keys. It is the `EnvironmentFile` of both units.
Setup rewrites the keys it manages in place and keeps every other line, so
keys you add survive a re-run. Nothing under the State dir is in the Target
Project checkout, and the Daemon writes nothing into the checkout.

The rendered units are overwritten on every Setup run. Edit the templates in
`infra/systemd/`, or set `AUTO_AGENT_MEMORY_MAX` (default 8G),
`AUTO_AGENT_DASHBOARD_MEMORY_MAX` (512M) or `AUTO_AGENT_UNIT_PATH` in the
Host env.

## Day-two operations

Run these from the Harness install on the Host, or by inventory name from the
Operator machine where noted.

**Check a Host.** `bin/auto-agent check` runs the verify stage alone and
writes nothing: the Host env is 0600 and complete, the Machine user's login
and admin, `claude auth status` in the declared mode, the Harness config
against the schema, the Provider check when a Hermetic tier is declared, and
one dry-run Fire. From the Operator machine, `bin/auto-agent check <name>`
first confirms the install is at the inventory's ref.

**Rotate a secret.** Mint the new token, save it to a 0600 file, and re-run
Setup with the file. A token file always wins over the value the Host env
holds; `--rotate` makes Setup ask for the secrets again instead.

```sh
bin/auto-agent setup --gh-token-file <new-pat-file> <target-dir>
bin/auto-agent setup --host <name> --gh-token-file <new-pat-file>   # from the Operator machine
bin/auto-agent check
```

Each Host has its own PAT, so rotating one Host does not disturb another.

**Change a Host env key.** `bin/auto-agent setup --set KEY=VALUE` writes it
and restarts the units when anything changed.

**Upgrade the harness.** From the Operator machine,
`bin/auto-agent upgrade <name> --ref <tag-or-sha>` moves the Harness install,
re-runs configure and the Host extension (with `upgrade` as its argument),
restarts both units and records the ref. A tag or SHA pins; a branch floats
to its tip on each upgrade. Inside the VM, move the checkout yourself and
then run `bin/auto-agent upgrade`. The install changes only through an
upgrade, so a Fire never straddles two harness versions
([ADR 0001](adr/0001-harness-install-loaded-as-plugin.md)).

**Stop and start the Daemon.** `sudo systemctl stop auto-agent-daemon` and
`sudo systemctl start auto-agent-daemon`.

### Park and un-park after credential death

When the Claude credential dies (the login expired or was revoked), the
Daemon parks: it writes `parked.json`, opens or reuses one `AFK:needs-human`
issue in the Target Project, and runs no Fire. A Fire that was in flight is
paused like an exhausted one. The Daemon then re-probes `claude auth status`
every hour and un-parks itself, closing the issue, when the probe passes.

The fix in the `login` mode is to log in again as the Host user:

```sh
ssh -t <user@host> claude auth login
bin/auto-agent park status     # on the Host: parked.json, or {"parked":false}
bin/auto-agent park reprobe    # un-park now instead of waiting for the hourly probe
```

In the `setup-token` mode, mint a fresh token with `claude setup-token` and
re-run Setup with `--claude-token-file`.

### Reading the Dashboard

Each Host serves a read-only page for its own Daemon on
`AUTO_AGENT_DASHBOARD_PORT` (8090), bound to `AUTO_AGENT_DASHBOARD_BIND`
(loopback unless you opt in). It shows the budget, what the Daemon is doing
now, Fire history and the Fire in flight, the queue, open Agent PRs and Maps.
It has no route that changes anything. `/api/status` returns the same as
JSON:

```sh
curl -s localhost:8090/api/status | jq '.daemon.state, .budget.message'
```

`daemon.state` is the state `daemon-state.json` records: for example
`firing`, `queue_empty`, `budget_low`, `exhausted`, `fire_failed`, `parked`.
A Target Project in the Bootstrap state shows a warning until an Environment
provider is merged. The full shape is in
[`dashboard/README.md`](../dashboard/README.md).

## Troubleshooting

Setup exits with the failing stage's code:

| Exit | Stage | Usual cause |
| --- | --- | --- |
| 3 | baseline | Not the reference Ubuntu release, no systemd, sudo asks for a password, no internet, or the Host already serves another Target Project |
| 4 | doctor, operator | A missing command; run the install line Setup printed |
| 5 | github | The PAT is not the Machine user's, is fine-grained, lacks a scope, or the Machine user is not admin |
| 6 | claude | Not logged in, or the login disagrees with the declared auth mode |
| 7 | config | The Harness config does not validate, or the push or PR failed |
| 8 | configure | An Ansible task failed; read the log Setup names |
| 9 | extension | The Host extension exited non-zero |
| 10 | verify | A `check:` line failed |
| 11 | enable | A unit did not start |
| 12 | bootstrap | The bootstrap issue could not be opened |
| 13, 14, 15 | ssh, install or inventory, provision | Remote entry points only |

Other symptoms:

- **The Daemon unit is down and does not restart.** It does not restart on
  exit 2 (no Target Project in the Host env), 6 (the `api-key` auth mode,
  which is not implemented) or 7 (another Daemon holds the State dir). Read
  `journalctl -u auto-agent-daemon`.
- **No Fires, state `mismatch`.** `CLAUDE_AUTH_MODE` contradicts the
  credential actually present. Fix the Host env or the login; the Daemon
  re-gates hourly.
- **No Fires, state `parked`.** See above.
- **Every Fire fails at preflight.** The Harness config is missing or invalid
  on the default branch. Merge the config PR, or run
  `bin/auto-agent check-config <target-dir>`.
- **Every Fire skips.** An issue still holds `AFK:in-progress`. See
  [Autonomous loop: operational notes](afk/autonomous-loop.md#operational-notes).
- **A round reports no display.** `DISPLAY` in the Host env must name the
  running `auto-agent-xvfb.service`. There is no headless fallback.
- **A round says `sandbox: DEGRADED`.** No AppArmor profile grants the
  Electron binary user namespaces. `AUTO_AGENT_ELECTRON_BINARY` overrides the
  default path the profile is written for (see the configure stage in
  [README: Setup](../README.md#setup)); re-run Setup. The round still runs.
- **A rebuilt Proxmox VM.** Re-running Setup recreates it, but the GitHub and
  Claude secrets lived on the old VM only; pass them again. With tailscale,
  remove the old node in the admin console.

## Related

- [AFK overview](afk/index.md) and [Autonomous loop](afk/autonomous-loop.md):
  what the Daemon does once the Host is up.
- [Proxmox end-to-end runbook](runbooks/e2e-proxmox.md): proving Setup on a
  throwaway VM.
