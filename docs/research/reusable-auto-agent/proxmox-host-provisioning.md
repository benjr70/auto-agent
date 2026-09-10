# Auto-provisioning a Host on Proxmox with the existing terraform and Ansible

- **Ticket**: #9 (wayfinder:research) — part of map #1
- **Date**: 2026-09-10
- **Question**: How can a Host (one Daemon instance's machine, per `CONTEXT.md`) be
  auto-provisioned on the Proxmox server this box runs on, reusing what
  Smart-Smoker-V2 already has under `infra/proxmox/`?
- **Method**: read-only survey of the Smart-Smoker-V2 checkout at
  `/home/claude-agent-1/Smart-Smoker-V2` (paths below are relative to that repo
  unless absolute), then primary docs for every external claim. No terraform or
  ansible command was run.

## Sources

Local (Smart-Smoker-V2):

- `infra/README.md`, `infra/proxmox/README.md`, `infra/proxmox/ansible/README.md`
- `infra/proxmox/terraform/{shared/versions.tf,shared/providers.tf,shared/backend.tf,.terraform.lock.hcl,main.tf,variables.tf,outputs.tf,terraform.tfvars.example}`
- `infra/proxmox/terraform/modules/{lxc-container,arm64-vm,networking}/*.tf`
- `infra/proxmox/terraform/environments/{github-runner,virtual-smoker}/main.tf`
- `infra/proxmox/scripts/create-cloud-init-template.sh`
- `infra/proxmox/ansible/{ansible.cfg,inventory/hosts.yml,inventory/group_vars/all.yml,inventory/group_vars/runners.yml,inventory/host_vars/github-runner.yml}`
- `infra/proxmox/ansible/playbooks/setup-github-runner.yml`
- `infra/proxmox/ansible/roles/{tailscale,docker,nodejs,common,github-runner}/`
- `.github/workflows/{infra-provision-vm.yml,ansible-provision.yml}`
- `docs/Infrastructure/guides/terraform-remote-state.md`, `docs/CI-CD/claude-agent-vm.md`
- `infra/systemd/agent-daemon.service`, `scripts/claude-agent/lib/usage-sensor.sh`
- Installed provider changelog: `infra/proxmox/terraform/.terraform/providers/registry.terraform.io/bpg/proxmox/0.57.1/linux_amd64/CHANGELOG.md`

Primary docs (fetched 2026-09-10):

- [bpg/proxmox provider index (auth, `ssh` block, env vars)](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/index.md)
- [bpg `proxmox_virtual_environment_vm`](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/resources/virtual_environment_vm.md)
- [bpg `proxmox_virtual_environment_container`](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/resources/virtual_environment_container.md)
- [bpg `proxmox_virtual_environment_file`](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/resources/virtual_environment_file.md)
- [bpg `proxmox_virtual_environment_download_file`](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/resources/virtual_environment_download_file.md)
- [bpg latest release (GitHub API)](https://api.github.com/repos/bpg/terraform-provider-proxmox/releases/latest)
- [Proxmox VE wiki: Cloud-Init Support](https://pve.proxmox.com/wiki/Cloud-Init_Support)
- [Proxmox VE wiki: Linux Container](https://pve.proxmox.com/wiki/Linux_Container)
- [Proxmox `pct(1)`](https://pve.proxmox.com/pve-docs/pct.1.html), [`pct.conf(5)`](https://pve.proxmox.com/pve-docs/pct.conf.5.html)
- [cloud-init module reference](https://docs.cloud-init.io/en/latest/reference/modules.html), [CLI reference](https://docs.cloud-init.io/en/latest/reference/cli.html), [instance-data](https://docs.cloud-init.io/en/latest/explanation/instancedata.html)
- [Tailscale auth keys](https://tailscale.com/kb/1085/auth-keys), [Tailscale tags](https://tailscale.com/kb/1068/tags)
- [Claude Code: Authentication](https://code.claude.com/docs/en/iam)
- [Terraform: sensitive data in state](https://developer.hashicorp.com/terraform/language/state/sensitive-data)

---

## 1. What the user already runs (Smart-Smoker-V2)

### 1.1 Provider and versions

- Provider is **`bpg/proxmox`**, constraint `~> 0.57.0`, locked at **0.57.1**
  (`infra/proxmox/terraform/shared/versions.tf:5-7`, `.terraform.lock.hcl:4-6`).
  The `moved {}` blocks in `main.tf:123-141` show a migration from the telmate
  resources (`proxmox_lxc`, `proxmox_vm_qemu`) to bpg's
  `proxmox_virtual_environment_container` / `_vm`; telmate is no longer used.
- Upstream latest is **v0.112.0** (published 2026-09-04) per the GitHub
  releases API, so the pin is ~55 minor versions behind. Everything this doc
  relies on (`initialization.user_data_file_id`, snippets upload over SSH,
  `features.keyctl`) already existed in 0.57.x: the installed CHANGELOG lists
  "snippets upload using SSH input stream (#1085)" and "use `sudo` for snippets
  upload (#1004)" well before 0.57.1 (`CHANGELOG.md:333-439`).
- Terraform `>= 1.5.0` required (`versions.tf:2`); CI installs **1.14.2**
  (`.github/workflows/infra-provision-vm.yml`, "Ensure Terraform installed").
- Auth: API token `terraform@pve!iac` preferred, password fallback
  (`shared/providers.tf`, `terraform.tfvars.example:7-13`). **No `ssh {}`
  provider block is configured** — nothing today uploads snippets.
- State: Terraform Cloud in *Local* execution mode, state+locking only; plans
  and applies run on the self-hosted `github-runner` LXC because the provider
  needs tailnet reach to the hypervisor (`shared/backend.tf:1-13`,
  `infra/README.md` "Terraform Cloud").
- Secret-bearing tfvars are **never in the checkout**: they live at
  `/opt/iac/terraform.tfvars` (root-owned, 0600) on the runner and are passed
  with `-var-file` (`docs/Infrastructure/guides/terraform-remote-state.md:44-51`,
  `infra-provision-vm.yml` env `TFVARS_FILE`).

### 1.2 Two patterns, both already modelled

| Pattern | Module | Used by | Key facts |
|---|---|---|---|
| **LXC from a vztmpl** | `modules/lxc-container` (`proxmox_virtual_environment_container`) | `github-runner` (CT 106), `dev-cloud` (CT 108), `prod-cloud` (CT 104) | `unprivileged = true` default, `features { nesting, fuse, keyctl, mount }`, `operating_system.type = "ubuntu"`, static IP on the `10.20.0.0/24` NAT bridge, `initialization.user_account { password, keys }` = **root** account (`modules/lxc-container/main.tf:19-88`, `variables.tf`). Template: `local:vztmpl/ubuntu-22.04-standard_22.04-1_amd64.tar.zst` (`terraform.tfvars.example:40`). `ignore_changes` on `user_account.keys` and `disk.size` because Ansible manages keys post-provision (`main.tf:90-105`). |
| **VM cloned from a cloud-init template** | `modules/arm64-vm` (`proxmox_virtual_environment_vm`) | `virtual-smoker` (VMID from template 9000) | `clone { vm_id, full, datastore_id }`, `agent { enabled }`, OVMF/q35, `initialization { ip_config, dns, user_account { username, password, keys } }`, serial console (`modules/arm64-vm/main.tf:19-90`). The template is built once by hand on the PVE host with `infra/proxmox/scripts/create-cloud-init-template.sh` (`qm create … --ide2 $STORAGE:cloudinit … qm template`). |

The **Host this Daemon runs on today (`claude-agent-1`) is neither** — it is an
Ubuntu Desktop VM installed by hand through the Proxmox UI, with Tailscale joined
interactively (`docs/CI-CD/claude-agent-vm.md` §2, §4a, decision table rows 13
and 24). The map's charting comment confirms "No IaC provisions this host".

### 1.3 How the github-runner LXC was actually provisioned

1. Terraform (root `main.tf` → `environments/github-runner` → `modules/lxc-container`)
   creates the CT with `nesting = true`, root password + SSH keys from tfvars,
   static IP `10.20.0.10/24` (`terraform.tfvars.example:35-59`).
2. Ansible `playbooks/setup-github-runner.yml` runs roles
   `common, docker, terraform, nodejs, github-runner, tailscale` over SSH as
   root (`inventory/hosts.yml:6-14`, `ansible.cfg` `become=true`).
3. Secrets arrive as `--extra-vars` from GitHub Actions secrets on the
   self-hosted runner: `github_runner_pat=${{ secrets.RUNNER_PAT }}`,
   `tailscale_auth_key=${{ secrets.TAILSCALE_AUTH_KEY }}`
   (`.github/workflows/ansible-provision.yml:135-140`). The roles use `no_log: true`
   on every task that touches them (`roles/tailscale/tasks/main.yml:55-77`,
   `roles/github-runner/tasks/main.yml:121-144, 399-407`), and the PAT is
   persisted on the box at `/etc/github-runner/pat` mode 0600 for the
   self-healing timer (`infra/proxmox/README.md:132`).
4. The tailscale role builds `tailscale up --authkey=… --hostname=… --advertise-tags=…`
   and only runs it when `BackendState != Running`, so re-runs are idempotent
   (`roles/tailscale/tasks/main.yml:39-77`). It then asserts the tailnet admin has
   global nameservers set, because `chattr +i /etc/resolv.conf` is blocked in
   unprivileged LXCs (`tasks/main.yml:131-143`, ansible README "Tailscale Admin DNS").

### 1.4 Secret handling today (names only)

| Secret | Where it lives | Reaches the box how | In terraform state? |
|---|---|---|---|
| Proxmox API token (`terraform@pve!iac`) | `/opt/iac/terraform.tfvars` on runner; `TF_TOKEN_app_terraform_io` in GH secrets | provider config only | No (provider config is not a resource attribute) |
| CT root password / VM cloud-init password | same tfvars (`initial_password`, `cloud_init_password`) | `initialization.user_account.password` | **Yes** — resource attribute; `sensitive = true` only redacts CLI output ([Terraform sensitive-data](https://developer.hashicorp.com/terraform/language/state/sensitive-data): "Terraform stores values with the `sensitive` argument in both state and plan files") |
| SSH public keys | committed in `group_vars/all.yml:18-24` (public, safe) | tfvars + Ansible `common` role | Yes (public) |
| `TAILSCALE_AUTH_KEY` | GitHub Actions secret | Ansible `--extra-vars`, `no_log` | No |
| `RUNNER_PAT` | GitHub Actions secret | Ansible `--extra-vars`, written to `/etc/github-runner/pat` 0600 | No |
| `SSH_PRIVATE_KEY` (Ansible control key) | GitHub Actions secret | written to `~/.ssh/id_ed25519` on runner for the job, deleted after | No |

The one gap: `--extra-vars` on the command line is visible in the runner's
process table for the duration of the play; a file-based `@vars.json` or an
env-var lookup would close it. Not a blocker.

### 1.5 Reusable as-is for a Host

- `modules/lxc-container` and `modules/arm64-vm` — both generic, both accept
  SSH keys, static IP, tags, pool. The VM module is named "arm64" but its
  `cpu_architecture` defaults to `x86_64` (`modules/arm64-vm/variables.tf:63-67`).
- `modules/networking` (bridge) — not needed; a Host sits on `vmbr0` like the
  runner.
- Ansible roles `common` (apt baseline, sshd hardening, ufw, fail2ban),
  `docker` (Docker CE + crun workaround for unprivileged LXC,
  `roles/docker/tasks/main.yml:76-102`), `nodejs` (NodeSource; version is a var),
  `tailscale` (install + `tailscale up` + Serve/Funnel + DNS assert). None of
  them is Smart-Smoker-specific except defaults (`tailscale_domain`,
  `github_repository`, DNS servers in `group_vars/all.yml`).
- The workflow skeleton in `infra-provision-vm.yml`: plan with `-target`,
  assert non-destructive (`terraform show -json` + python), apply, wait for
  SSH, `cloud-init status --wait`, then Ansible.

What does **not** exist yet: a role that installs `gh`, the Claude Code CLI, a
dedicated agent user, the checkout, and the `agent-daemon`/`agent-dashboard`
systemd units. Those steps are only prose in `docs/CI-CD/claude-agent-vm.md`.

---

## 2. What the primary docs establish

### 2.1 bpg/proxmox: VM from a cloud-init template with a first-boot script

- `initialization` on `proxmox_virtual_environment_vm` accepts either
  `user_account { username, password, keys }` **or** `user_data_file_id`
  ("conflicts with `user_account`"), plus `vendor_data_file_id`,
  `meta_data_file_id`, `network_data_file_id` ("conflicts with `ip_config`"),
  `datastore_id` (cloud-init disk, default `local-lvm`), `dns`, `ip_config`,
  `upgrade` (apt upgrade on first boot, default `true`)
  ([vm docs](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/resources/virtual_environment_vm.md)).
- `clone { vm_id (required), full (default true), datastore_id, node_name, retries }`
  — same shape the `arm64-vm` module already uses.
- The custom user-data is a `proxmox_virtual_environment_file` with
  `content_type = "snippets"` and `source_raw { data, file_name }`. Two hard
  requirements ([file docs](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/resources/virtual_environment_file.md)):
  1. "Snippets are not enabled by default in new Proxmox installations. You
     need to enable them in the 'Datacenter>Storage' section" — a one-time
     click on the PVE host (the `local` storage).
  2. Snippet upload "uses SSH access to the node", i.e. the provider needs an
     `ssh { username, agent|private_key, node { name, address } }` block; with
     an API token, `ssh.username` is required, and a non-root SSH user needs the
     documented `/etc/sudoers.d/terraform` entries (`pvesm`, `qm`,
     `tee /var/lib/vz/snippets/…`) ([index](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/index.md)).
     "SSH is NOT required for: Creating, modifying, or deleting VMs and
     Containers" — which is why the existing setup never needed it.
- Proxmox side: `cicustom "user=local:snippets/userconfig.yaml"`; "The custom
  config files have to be on a storage that supports snippets"; when `user=` is
  given the auto-generated user config (ciuser/sshkeys) is replaced, and
  `qm cloudinit dump <vmid> user` shows what Proxmox would have generated, a
  good starting point ([PVE Cloud-Init Support](https://pve.proxmox.com/wiki/Cloud-Init_Support)).
- The template itself can be made fully declarative:
  `proxmox_virtual_environment_download_file` with `content_type = "import"`
  pulls `https://cloud-images.ubuntu.com/.../jammy-server-cloudimg-amd64.img`
  through the PVE download-url API (no SSH), and the VM docs show
  `disk { import_from = proxmox_virtual_environment_download_file.x.id }`.
  This would replace `create-cloud-init-template.sh`, but the existing
  template-9000 + `clone {}` path also works and is what `arm64-vm` does.
- cloud-init ([modules](https://docs.cloud-init.io/en/latest/reference/modules.html)):
  `users` (name, ssh_authorized_keys, sudo, groups, shell, lock_passwd),
  `packages` / `package_update` / `package_upgrade`, `write_files` (path,
  content, owner, permissions, defer), `runcmd` (frequency **once-per-instance**,
  output to `/var/log/cloud-init-output.log`, "written to a script and run by
  cc_scripts_user"). `bootcmd` "should only be for things that could not be done
  later". `cloud-init status --wait` blocks until done, exit 0/1/2
  ([CLI](https://docs.cloud-init.io/en/latest/reference/cli.html)) — the
  existing workflow already calls it (`infra-provision-vm.yml`, "Wait for VM").
- Sensitivity: user-data is kept root-only on the instance ("Non-root users
  referencing `userdata` or `vendordata` keys will see only redacted values",
  [instance-data](https://docs.cloud-init.io/en/latest/explanation/instancedata.html)),
  **but** the whole `source_raw.data` string is a resource attribute of
  `proxmox_virtual_environment_file`, so anything in it lands in terraform
  state and in `/var/lib/vz/snippets/` on the hypervisor. See §4.

### 2.2 bpg/proxmox + Proxmox: LXC with `nesting=1` and Docker

- `proxmox_virtual_environment_container` has **no cloud-init / user-data
  mechanism** — the docs list none ([container docs](https://raw.githubusercontent.com/bpg/terraform-provider-proxmox/main/docs/resources/virtual_environment_container.md)).
  `initialization.user_account` is root-only ("`keys` and `password` for the
  root account"; no username). The only script hook is `hook_script_file_id`
  = Proxmox `hookscript`, which runs **on the PVE host** "during various steps in
  the containers lifetime" ([pct.conf(5)](https://pve.proxmox.com/pve-docs/pct.conf.5.html)),
  not inside the guest. First-boot configuration for an LXC is therefore
  Ansible over SSH (the existing pattern) or `pct exec`/`pct push` from the host
  ("Launch a command inside the specified container", "Copy a local file to the
  container", with `--user/--group/--perms`, [pct(1)](https://pve.proxmox.com/pve-docs/pct.1.html)).
- Features ([pct.conf(5)](https://pve.proxmox.com/pve-docs/pct.conf.5.html)):
  `nesting` — "Allow nesting. Best used with unprivileged containers with
  additional id mapping"; `keyctl` — "Allow the use of the keyctl() system
  call. This is required to use docker inside a container"; `fuse`, `mount`,
  `mknod`. The bpg docs add: "Changing flags (except nesting) is only allowed
  for `root@pam` authenticated user" — so setting `keyctl = true` at create
  time is fine with the API token, but flipping it later on a live CT needs the
  password auth path that `shared/providers.tf` already supports.
- `unprivileged` "defaults to `false`" in the provider, but the module sets
  `true` (`modules/lxc-container/variables.tf:34-38`) and Proxmox's own default
  for creation is 1; the LXC team calls unprivileged containers "safe by design"
  and privileged ones fit only "trusted environments" ([Linux Container wiki](https://pve.proxmox.com/wiki/Linux_Container)).
- `operating_system.type` must match the template ("Container start fails if
  the configured ostype differs from the auto detected type") — `ubuntu` is the
  module default and the README explains the `veth0`-stays-down failure when
  it is unset (`infra/proxmox/README.md:60-72`).
- Docker in an **unprivileged** LXC is already proven on `dev-cloud`/`prod-cloud`
  with the `docker` role's crun swap (runc fails on
  `net.ipv4.ip_unprivileged_port_start`; distro crun 0.17 is too old; upstream
  static crun 1.21 is fetched — `roles/docker/tasks/main.yml:76-102`,
  `roles/docker/defaults/main.yml`). Note the example tfvars only sets
  `nesting = true` for those CTs; `keyctl` is what the Proxmox docs call
  required for Docker — pass `features = { nesting = true, keyctl = true }`
  for a new Host so it never drifts.

### 2.3 Tailscale keys

([auth keys](https://tailscale.com/kb/1085/auth-keys), [tags](https://tailscale.com/kb/1068/tags))

- Key flavours: one-off ("can only be used to connect a device or server one
  time") vs reusable ("Be very careful with reusable keys! … best kept in a key
  vault"); **ephemeral** ("automatically remove … after the device goes
  offline", meant for "containers or Lambda functions" — wrong for a
  long-lived Host); **pre-approved**; **tagged** ("automatically tag devices that
  use the auth key").
- Auth-key expiry is 1–90 days. A tagged device's node-key expiry is "disabled
  by default", and "applying a tag to a device removes any user-based
  authentication" — so a Host joined with a `tag:agent-host` key does not
  inherit the maintainer's login and never needs re-auth. The key creator must
  be a `tagOwners` owner of the tag. With a tagged key the tags apply
  automatically; `--advertise-tags` (which the existing role passes,
  `roles/tailscale/tasks/main.yml:61`) must then agree with the key's tags.
- `tailscale up --auth-key=<key>` bypasses interactive login.

**Recommendation for a Host**: one-off, pre-approved, tagged (`tag:agent-host`),
non-ephemeral key minted per Host in the admin console; used once by the
`tailscale` role (already `no_log`) and then worthless. The existing docs
recommend reusable 90-day keys (ansible README "Prerequisites"); one-off is
strictly better for a machine provisioned once.

### 2.4 Claude Code login on a headless machine

([Authentication](https://code.claude.com/docs/en/iam))

Three documented options, none of which are "paste a browser session":

1. **Interactive OAuth over SSH** — run `claude`, press `c` to copy the login
   URL, open it on a laptop; "If your browser shows a login code instead of
   redirecting back … paste it into the terminal at the `Paste code here if
   prompted` prompt. This happens when the browser can't reach Claude Code's
   local callback server, which is common in WSL2, SSH sessions, and
   containers." Credentials land in `~/.claude/.credentials.json` mode 0600
   on Linux and auto-refresh. This is exactly how `claude-agent-1` was set up
   (`docs/CI-CD/claude-agent-vm.md` §14a).
2. **`claude setup-token`** — run on any machine with a browser; prints a
   **one-year OAuth token** that "does not save … anywhere"; export as
   `CLAUDE_CODE_OAUTH_TOKEN`. "It can only make model requests" (no Remote
   Control, no claude.ai connectors; local MCP still works), requires a
   Pro/Max/Team/Enterprise plan, and is **not read in bare mode** (`--bare`).
   Precedence: below `ANTHROPIC_API_KEY`/`apiKeyHelper`, above `/login`.
3. **`apiKeyHelper`** — a script that returns an API key (Console billing, not
   the subscription), re-run every 5 min by default.

Consequences for the Daemon:

- Option 2 is the only one a Setup wizard can complete **without an SSH
  session on the Host**: the human runs `claude setup-token` locally, the
  wizard drops the token into the Host's env file. The existing unit already
  reads `EnvironmentFile=-~/.config/agent-daemon/env`
  (`infra/systemd/agent-daemon.service`), so no unit change is needed.
- **But** the budget pacer reads `.claudeAiOauth.accessToken` from
  `~/.claude/.credentials.json` (`scripts/claude-agent/lib/usage-sensor.sh:14,58-67`)
  and that file does not exist under option 2. Whether the usage endpoint
  accepts a `setup-token` bearer is **not documented** — treat it as an open
  verification item for #10 (see §6). The renewal warning ("Your login expires
  in 3 days") applies only to `/login` credentials, not to the env-var token,
  so a one-year token on a Host needs a calendar reminder, not a sensor.
- Option 1 remains the fallback: Setup ends with "ssh in and run `claude`
  once" — a HITL step, but a short one.

---

## 3. Minimal shapes

Both shapes reuse the root layout in `infra/proxmox/terraform` (shared
`providers.tf`/`backend.tf`/`versions.tf` symlinks, one `environments/<name>`
wrapper, tfvars on the runner). The Host-specific work is one new environment
directory plus one new Ansible role; nothing in the existing modules has to
change for the LXC shape, and the VM shape needs two optional inputs added to
`modules/arm64-vm`.

### 3.1 (a) VM from a cloud-init template

Terraform (`environments/agent-host-vm/main.tf`, sketch):

```hcl
# Snippet upload needs SSH to the node: add to shared/providers.tf
#   ssh { agent = true  username = "terraform"  node { name = var.target_node  address = var.node_ssh_address } }
# and enable "Snippets" on the `local` storage once in Datacenter > Storage.

resource "proxmox_virtual_environment_file" "user_data" {
  node_name    = var.target_node
  datastore_id = "local"
  content_type = "snippets"
  source_raw {
    file_name = "${var.vm_name}-user-data.yaml"
    data      = templatefile("${path.module}/user-data.yaml.tftpl", {
      hostname = var.vm_name
      user     = var.agent_user          # e.g. "agent"
      ssh_keys = var.ssh_public_keys     # public — fine in state
    })
  }
}

module "vm" {
  source          = "../../modules/arm64-vm"   # generic despite the name; cpu_architecture defaults to x86_64
  target_node     = var.target_node
  vm_name         = var.vm_name
  clone_template  = var.clone_template         # 9000 from create-cloud-init-template.sh
  cpu_cores       = 6
  memory_mb       = 12288
  storage         = var.storage
  ipv4_cidr       = var.ipv4_cidr              # 10.20.0.5x/24 on vmbr0, like the runner
  gateway         = var.gateway
  ssh_public_keys = var.ssh_public_keys
  # NEW module inputs (add alongside cloud_init_user/password, mutually exclusive):
  user_data_file_id = proxmox_virtual_environment_file.user_data.id
  tags            = ["agent-host", var.target_project]
}
```

The module change is small: in `modules/arm64-vm/main.tf` make the
`user_account {}` block `dynamic` on `var.user_data_file_id == null` and emit
`user_data_file_id = var.user_data_file_id` otherwise (the two conflict per the
provider docs). Keep `ip_config`/`dns` — they do not conflict with user data.

`user-data.yaml.tftpl` (no secrets, ever — see §4):

```yaml
#cloud-config
hostname: ${hostname}
package_update: true
packages: [git, curl, jq, ca-certificates, gnupg, ufw, qemu-guest-agent, python3]
users:
  - name: ${user}
    groups: [sudo, docker]
    shell: /bin/bash
    lock_passwd: true
    sudo: ALL=(ALL) NOPASSWD:ALL        # narrow this later; Ansible needs it on first run
    ssh_authorized_keys:
%{ for k in ssh_keys ~}
      - ${k}
%{ endfor ~}
runcmd:
  - systemctl enable --now qemu-guest-agent
  - ufw default deny incoming && ufw default allow outgoing && ufw --force enable
```

Then the existing workflow shape: wait for SSH → `cloud-init status --wait` →
`ansible-playbook playbooks/setup-agent-host.yml` with roles
`common, docker, nodejs, tailscale, agent-host` (§3.3), `ansible_user` =
`${user}` with `become`. Ansible does the tool install (`gh`, `claude`, Node
24.x via the `nodejs` role's `nodejs_version`), creates the checkout, installs
the units, and is the only thing that ever sees a secret.

Why not put everything in `runcmd`? Because `runcmd` is once-per-instance
([cloud-init modules](https://docs.cloud-init.io/en/latest/reference/modules.html)),
so day-2 changes would need a rebuild, and because Ansible is where the
`no_log` secret path already lives. Keep cloud-init to "make the box
SSH-reachable as a non-root user with Python"; that is also all the
`virtual-smoker` VM gets from Proxmox's auto-generated config today.

### 3.2 (b) LXC Host

Terraform (`environments/agent-host-lxc/main.tf`, sketch — identical in shape
to `environments/github-runner/main.tf`):

```hcl
module "container" {
  source           = "../../modules/lxc-container"
  target_node      = var.target_node
  hostname         = var.hostname            # e.g. agent-host-<project>
  template         = "local:vztmpl/ubuntu-22.04-standard_22.04-1_amd64.tar.zst"
  storage          = var.storage
  disk_size        = 60                     # the live Host sits at 94% of 50 GB (map #1 §6.1)
  cpu_cores        = 4
  memory_mb        = 8192
  swap_mb          = 2048
  network_bridge   = "vmbr0"
  ipv4_cidr        = var.ipv4_cidr
  gateway          = var.gateway
  ssh_public_keys  = var.ssh_public_keys
  initial_password = var.initial_password   # lands in state — see §4; rotate via Ansible
  features         = { nesting = true, keyctl = true }   # keyctl: "required to use docker inside a container"
  tags             = ["agent-host", var.target_project]
}
```

No provider `ssh {}` block, no snippets, no template build: the vztmpl is
already on `local` and container CRUD is pure API. First boot = root over SSH
with the tfvars keys, exactly like CT 106; then
`ansible-playbook playbooks/setup-agent-host.yml` as `root` with the same
role list. The `agent-host` role creates the agent user (Ansible, not
cloud-init, since containers have no user-data).

Caveats that are LXC-only and already documented locally:

- Unprivileged + Docker needs the crun swap the `docker` role already does.
- DNS: Tailscale admin global nameservers are mandatory
  (`roles/tailscale/tasks/main.yml:131-143`).
- `apt` sandbox must be disabled (`roles/common/tasks/main.yml:7`).
- No display: the current `/verify-pr` harness launches headful Chrome and a
  real Electron window (`docs/CI-CD/claude-agent-vm.md` rows 11-12; map #1
  §3.4 "DISPLAY :0"). An LXC Host can run a headless Verification Harness
  only.

### 3.3 The one new Ansible role: `agent-host`

Independent of (a)/(b). Inputs: `agent_user` (default `agent`),
`agent_repo` (Target Project `owner/name`), `agent_node_version`,
`agent_claude_version`, `agent_github_token`, `agent_claude_oauth_token`
(both optional, `no_log`). Tasks:

1. user + `docker` group (`docker_users` already exists in the docker role).
2. `gh` from `cli.github.com/packages` (the apt recipe in
   `docs/CI-CD/claude-agent-vm.md` §"GitHub CLI").
3. Node via the existing `nodejs` role (set `nodejs_version: "24"`), then
   `npm install -g @anthropic-ai/claude-code`.
4. `~/.config/agent-daemon/env` mode 0600 from a template, containing only
   `GH_TOKEN=` and `CLAUDE_CODE_OAUTH_TOKEN=` when provided (`no_log`).
5. `gh auth login --with-token` from a 0600 file and `gh auth setup-git`
   (`claude-agent-vm.md` §"GitHub CLI"), git identity for the bot.
6. `git clone` the Target Project into `~/<repo>`.
7. Install `agent-daemon.service` / `agent-dashboard.service` with
   `User=`, `WorkingDirectory=`, `PATH=` rendered from vars — the committed
   units hard-code `claude-agent-1` paths (`infra/systemd/agent-daemon.service`;
   map #1 §3.1).
8. Print what is still HITL: "no `CLAUDE_CODE_OAUTH_TOKEN` given — ssh in and
   run `claude` once" (option 1 in §2.4).

---

## 4. Getting each secret onto the box without landing in terraform state

Ground truth: "Terraform stores your state in a plaintext file, which includes
any secret values you defined in your configuration" and `sensitive` "redacts
those values from CLI output" only ([Terraform](https://developer.hashicorp.com/terraform/language/state/sensitive-data)).
Ephemeral variables (TF ≥ 1.10) and write-only arguments (≥ 1.11) exist, but
the bpg resources used here do not document write-only attributes, so the
practical rule is: **terraform never sees a runtime secret**.

| Secret | Path | Never do |
|---|---|---|
| Proxmox API token | tfvars on the runner, provider config (not state) — unchanged | commit tfvars |
| Node SSH creds for snippet upload (VM shape only) | `ssh { agent = true }` with the runner's agent, or `PROXMOX_VE_SSH_PRIVATE_KEY` env — provider config, not state | `ssh.password` in tfvars |
| CT root / cloud-init password | tfvars → state (unavoidable with `user_account.password`). Mitigate: generate with `random_password` (still in state, but never reused elsewhere), have the `common` role rotate/lock it on first run, and rely on keys (`ssh_permit_root_login: prohibit-password`, `group_vars/all.yml:6`). VM shape avoids it entirely: `users[].lock_passwd: true` in user-data, no `user_account` | reuse a human password |
| Tailscale auth key | GH secret → Ansible `tailscale_auth_key` (`no_log`), one-off tagged key, consumed by `tailscale up` and then dead | put it in cloud-init `runcmd` (would be in `proxmox_virtual_environment_file` state **and** in `/var/lib/vz/snippets/` on the hypervisor **and** in `/var/lib/cloud/instance/user-data.txt`) |
| GitHub token for the Daemon | GH secret → Ansible `agent_github_token` (`no_log`) → `~/.config/agent-daemon/env` 0600, mirroring `/etc/github-runner/pat` | echo it in a workflow step |
| Claude OAuth token (`setup-token`) | human runs `claude setup-token` on a laptop → pasted into the Setup wizard → same env file; or skipped and done by `claude` over SSH | cloud-init; tfvars |
| Ansible control key | GH secret written to the runner's `~/.ssh` for the job, deleted in `always` (`ansible-provision.yml:96,171`) — unchanged | — |

If Ansible is driven locally instead of from Actions, pass secrets with
`--extra-vars @secrets.json` (0600, gitignored) rather than inline, so they
are not in `ps`.

Cloud-init can carry a secret safely only in the narrow sense that the
instance keeps user-data root-readable ([instance-data](https://docs.cloud-init.io/en/latest/explanation/instancedata.html));
the terraform-state and hypervisor copies are the problem, not the guest.

---

## 5. Which one? Facts that bear on the choice (decided in #10)

| | VM (cloud-init clone) | LXC |
|---|---|---|
| Existing module | `arm64-vm` (+2 inputs) | `lxc-container` unchanged |
| Terraform-side prereqs | provider `ssh {}` + sudoers on the PVE node, Snippets enabled, template 9000 | none beyond today |
| First-boot user/keys/packages | cloud-init user-data (declarative, no root SSH) | root SSH + Ansible (root password in state) |
| Docker | native | works unprivileged with crun swap; needs `nesting`+`keyctl` |
| GUI / headful verification | possible (this is what `claude-agent-1` does) | no |
| Resource cost | full kernel, 12 GB RAM today | shared kernel, cheaper |
| Snapshots / rebuild | `qm` clone from template is fast; whole-disk snapshot | `pct` snapshot; rebuild is `terraform apply` + play |
| Tailscale DNS gotcha | no | yes (admin global nameservers) |

Everything except the display column is a wash; the display requirement of the
current Verification Harness is the deciding fact, and it belongs to the
Target Project (bring-your-own harness, `CONTEXT.md` "Verification Harness"),
not to the Host.

---

## 6. Implications for the Host runtime decision (#10) and Setup (#16)

**For #10 (Host runtime):**

1. Both shapes are provisionable from the existing `infra/proxmox` layout with
   one new `environments/agent-host-*` directory and one new Ansible role; the
   modules are reusable as-is (LXC) or with two added inputs (VM). Neither
   requires upgrading the provider, though `~> 0.57.0` is 55 minors behind
   v0.112.0 and a bump should be planned separately.
2. The VM shape is the only one with a real first-boot mechanism
   (`user_data_file_id` → cloud-init). LXC has none; Proxmox `hookscript` runs
   on the hypervisor, not in the guest. If #10 wants "terraform apply and the
   box comes up ready", that is VM + cloud-init; if Ansible is always in the
   loop anyway (it must be, for secrets), LXC is cheaper and needs no
   hypervisor-side SSH/snippets setup.
3. Choose LXC only if the Daemon's default Verification Harness is headless.
   The Smart-Smoker harness needs a display; a reusable Daemon should not
   assume one, which argues for LXC-by-default with VM as the documented
   alternative for GUI Target Projects.
4. Tailscale: tagged one-off key, non-ephemeral, hostname = Host name; the
   `tailscale` role handles it and is idempotent. The Host-name/tag pair should
   be part of the Harness config so the Dashboard can find the Host.
5. Claude auth: the pacer's dependency on `~/.claude/.credentials.json`
   (`usage-sensor.sh:58-67`) is a hidden coupling to option 1 (interactive
   `/login`). Before #10 commits to `setup-token`, verify whether the OAuth
   usage endpoint accepts a `CLAUDE_CODE_OAUTH_TOKEN` bearer; if not, the
   usage sensor needs a second source or Setup must keep the SSH `/login` step.

**For #16 (Setup):**

1. Inputs the wizard must collect, in order: Proxmox target node/storage/IP;
   Target Project `owner/repo`; SSH public key(s); a one-off tagged Tailscale
   key; a GitHub token for the bot identity; optionally a `claude setup-token`
   output. Only the last three are secrets and none of them goes through
   terraform.
2. Sequence = the existing `infra-provision-vm.yml` skeleton: `terraform plan
   -target=module.agent_host` → non-destructive assert → apply → wait for SSH
   (→ `cloud-init status --wait` on the VM shape) → `ansible-playbook
   setup-agent-host.yml` → verify (`tailscale status`, `gh auth status`,
   `claude --version`, `systemctl is-active agent-daemon`).
3. Two steps stay human no matter what: enabling Snippets on the PVE storage
   (VM shape only, once per hypervisor) and completing the Claude OAuth grant
   in a browser (either `setup-token` locally or `/login` over SSH). The
   `wizard` skill's model — steps only a human can perform — fits both.
4. The Setup should write the Host's identity (name, tailnet name, agent user,
   checkout path) into the Harness config so the systemd units, dashboard and
   token-usage accounting stop hard-coding `claude-agent-1` (map #1 §3.1,
   §A.6).
5. Re-running Setup must be safe: the `lxc-container` module already ignores
   `user_account.keys` and `disk.size` drift for this reason (`main.tf:90-105`);
   the new environment should copy that lifecycle block, and the play must
   remain idempotent (the tailscale role's `BackendState` check is the model).
