# Runbook: the Proxmox end-to-end test

`infra/e2e/proxmox-e2e.sh` proves that Setup works end to end (Spec #23, Testing
Decisions: the Setup seam). One command does the whole cycle:

1. provisions a throwaway VM on Proxmox,
2. runs unattended `setup` against the fixture Target Project,
3. waits for the Daemon's first green Fire (a no-work Fire: the fixture repo
   has nothing queued, so it proves the Host, the auth and the Daemon's
   pickup path, not an implementation),
4. checks the Dashboard's `/api/status`,
5. destroys the VM.

The script exits 0 only when the Fire is green and the status route is
healthy. It destroys the VM whether the run passes or fails, and on Ctrl-C.
The logs are kept either way. A human with Proxmox credentials runs it on
demand, before a release or after a change to Setup, the Ansible roles, the
terraform or the Daemon. It never runs per PR.

A run takes about 20 to 40 minutes. Most of that is cloud-init's first boot
and the Ansible configure step. The Claude spend is two short Fires (Setup's
dry-run Fire and the Daemon's Fire), both on Haiku by default.

## What you need, and where it comes from

Run it from a clone of this repo on your Operator machine, meaning your
laptop or any box that can reach the Proxmox API and the VM's address over
SSH.

| What | Where it comes from | How the script gets it |
|---|---|---|
| **Proxmox API token** | In the Proxmox UI: Datacenter → Permissions → API Tokens → Add. The user needs a role that can create and destroy VMs and download a cloud image to the image datastore. The bpg/proxmox provider's usual set is `Datastore.AllocateSpace Datastore.AllocateTemplate Datastore.Audit Sys.Audit Sys.Console Sys.Modify SDN.Use VM.Allocate VM.Audit VM.Clone VM.Config.* VM.PowerMgmt`, on `/`. Or, for a lab box, a `root@pam` token with privilege separation off. The value is `user@realm!tokenid=secret`. | `--proxmox-token-file <f>`: one line, mode 0600. It is used for the provision and for the destroy, and never written anywhere else. |
| **Proxmox facts** | Your Proxmox: the API URL (`https://<pve>:8006/`), the node name, a free static IPv4 in CIDR form, the gateway, and the datastore and bridge if they are not `local-lvm`, `local` and `vmbr0`. | `--proxmox-endpoint --node --ipv4 --gateway` (plus `--proxmox-insecure` for a self-signed certificate). They are remembered in `~/.config/auto-agent/hosts/<name>.proxmox.json` after the first run. |
| **GitHub machine user and classic PAT** | The account the Daemon acts as (ADR 0005), with admin on the fixture repo. Create the classic PAT at github.com → Settings → Developer settings → Personal access tokens (classic), with the scopes `repo`, `project` and `workflow`. | `--gh-login <login> --gh-token-file <f>`. The fixture push uses the same token. |
| **Claude `setup-token` token** | Run `claude setup-token` on any machine logged in to the Claude subscription the test should bill. The run is unattended, so the Host uses setup-token auth mode (an interactive `/login` cannot happen). | `--claude-token-file <f>` |
| **Fixture repo** | A GitHub repo kept for this test, e.g. `<you>/auto-agent-e2e-fixture`. Create it once, empty and private: `gh repo create <owner>/auto-agent-e2e-fixture --private`. Each run commits `plugin/fixtures/target-project` onto its default branch. The repo must have **no open issue or PR**, so the Fire finds no work and ends quickly. | `--fixture-repo <owner/name>` |
| **SSH key** | Your key pair. cloud-init installs its public key on the VM. | `~/.ssh/id_ed25519.pub` by default, or `--ssh-identity <private-key>` (with its `.pub` beside it). |
| **Tools on the Operator machine** | `terraform`, `ansible-playbook` (`pipx install ansible-core`), `ssh`, `git`, `gh` and `jq`. | Setup's operator stage names any that are missing. |

The Harness install on the VM is this clone's `HEAD`, which must already be
pushed, because the VM clones it from `origin`. To test another ref, pass
`--ref <tag|sha|branch>`.

## Run it

The first run passes every Proxmox fact:

```sh
infra/e2e/proxmox-e2e.sh \
  --fixture-repo <owner>/auto-agent-e2e-fixture \
  --gh-login <machine-user> --gh-token-file ~/secrets/gh-pat \
  --claude-token-file ~/secrets/claude-setup-token \
  --proxmox-token-file ~/secrets/pve-token \
  --proxmox-endpoint https://pve.lan:8006/ --proxmox-insecure \
  --node pve --ipv4 192.168.1.60/24 --gateway 192.168.1.1
```

Later runs need only the secrets and the fixture repo:

```sh
infra/e2e/proxmox-e2e.sh --fixture-repo <owner>/auto-agent-e2e-fixture \
  --gh-login <machine-user> --gh-token-file ~/secrets/gh-pat \
  --claude-token-file ~/secrets/claude-setup-token --proxmox-token-file ~/secrets/pve-token
```

The script has these options of its own:

- `--name` sets the VM and inventory name (default `auto-agent-e2e`).
- `--model` sets the Fire model (default `haiku`).
- `--fire-timeout` sets how long, in seconds, to wait for the first finished
  Fire (default 1800).
- `--setup-timeout` sets how long Setup may run before the run counts it as
  hung (default 5400).
- `--log-dir` sets where the logs go.
- `--keep-vm` skips the destroy (see "Debugging on the live VM" below).

Any other option goes on to `setup --provision proxmox`. See `--help`.

Each stage prints one line, `e2e: <stage>: ok|FAIL|skipped — <detail>`.
Setup's own `setup: ...` lines stream in between. The run ends with one of:

- `e2e: PASS — ...; logs in <dir>`
- `e2e: FAIL — <stage> (exit <n>); logs in <dir>`

## Logs

The default log directory is
`~/.local/state/auto-agent-e2e/<name>-<utc>/`. It holds:

- `summary.txt`: every `e2e:` line and the verdict.
- `fixture.log`: the fixture clone and push.
- `setup.log`: all of Setup's output.
- `status.json`: the last `/api/status` read while waiting.
- `status.final.json`: the status read just before the destroy.
- `journal.log`: both units' journal.
- `state.tgz`: the Host's whole State dir, including the Fire records
  (`fires/`) and the raw Fire transcripts (`logs/`).
- `destroy.log`: the destroy's output.

## What a failure at each stage means

| Stage | Exit | What failed | Look at / do |
|---|---|---|---|
| `preflight` | 2 / 20 | 2 is a missing or bad option. 20 means one of: a secret file is unreadable, a tool is missing, this clone's `HEAD` is on no remote branch, or a VM from an earlier run under the same name is still in the inventory. | For an unpushed `HEAD`, push it or pass `--ref`. For a leftover VM, run the `--teardown` command the line prints. No VM exists yet. |
| `fixture` | 21 | The PAT cannot read the fixture repo, the clone or push failed, or the repo has open issues or PRs. | `fixture.log`. Close whatever the line names (a Fire with work to do would run long and cost real money). No VM exists yet. |
| `setup` | 22 | `setup --provision proxmox` exited non-zero, or hung past `--setup-timeout`. The line quotes Setup's first `FAIL` stage. Setup's own exit codes are in `bin/auto-agent help`: 15 means terraform, or a VM that never answered SSH; 13 means SSH; 14 means the install play; 3 to 12 are the in-VM stages. | `setup.log`, then `journal.log`. For 15, open the VM's console on the node: cloud-init output, the static address, your key. For a terraform 401 or 403, check the token and the role. |
| `fire` | 23 | Either the Daemon's first pickup Fire finished but was not green (the line gives its exit code, outcome and summary), or no Fire finished within `--fire-timeout`, or the Dashboard never answered. | `state.tgz` → `fires/<id>.json` and `logs/<id>.stream.jsonl` for the Fire itself. `journal.log` for the Daemon: whether it was gated, parked or crashed. An `EXHAUSTED` or `AUTH_DEAD` outcome points at the Claude token and its usage. |
| `status` | 24 | `/api/status` answered but is unhealthy. The line names each problem: the Daemon unit is not active, the Daemon is parked, the Fire history is stale, the repo is wrong, or there is a bootstrap warning. | `status.final.json` and `journal.log`. See `dashboard/README.md` for the shape. |
| `collect` | none | Nothing could be read back over SSH. The verdict is unaffected. | The VM was probably already gone, or SSH broke. The other logs still stand. |
| `destroy` | 25 | `terraform destroy` failed. **A VM may still be running on Proxmox.** This code wins over every other, a pass included. | `destroy.log`. Fix the cause (usually the token or the Proxmox API), then run the `--teardown` command the line prints. As a last resort, delete the VM in the Proxmox UI and then delete `~/.config/auto-agent/hosts/<name>.{env,proxmox.tfstate}`. |

Exit 130 means the run was interrupted (Ctrl-C, or the terminal closed).
Collect and destroy still ran. A `kill -9` cannot be caught: after one, run
the `--teardown` command. If a run was interrupted during `terraform apply`,
before terraform wrote any state, the teardown finds nothing to destroy;
check the Proxmox UI for the VM and delete it there.

## Teardown

```sh
infra/e2e/proxmox-e2e.sh --teardown --name auto-agent-e2e --proxmox-token-file ~/secrets/pve-token
```

This collects the logs and destroys the VM. The VM's provision settings are
kept, so the next run needs no Proxmox flags. The inventory entry and the
terraform state are removed.

## Debugging on the live VM

Pass `--keep-vm` to skip the destroy. The run still gives its verdict, and
the destroy line prints the `--teardown` command. Reach the VM with
`bin/auto-agent check auto-agent-e2e`, or with `ssh auto-agent@<ip>`, then
`journalctl -u auto-agent-daemon -f`. Always finish with `--teardown`.
