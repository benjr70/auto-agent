---
status: accepted
---

# Setup is a Claude Code skill driving a converging bash CLI, run from a clone of this repo

Setup has to turn any Target Project plus a Host into a running Daemon
through three entry points (ADR 0004), mint and verify two identities (ADR
0005), scaffold the Surfaces into a Harness config (bootstrap decision), and
be safe to re-run and to script for the harness's own end-to-end test. The
operator always has Claude Code and a Claude account, so we decided Setup is
a **skill over an engine**: `/auto-agent:setup` is the front the human uses,
and `bin/auto-agent` in the Harness install is the engine it drives stage by
stage. The skill owns the conversation (the Surfaces interview, explaining
each stage, diagnosing a failed one, writing the Harness config draft and its
PR body); the CLI owns everything mechanical and is the only thing that
writes to a Host, so the skill can never do what the unattended path cannot.
The operator **clones this repo and starts Claude Code in it**; the repo's
own plugin exposes the skill and that clone is the operator-side install the
CLI runs from; the Target Project is an argument, never the working
directory. A "doctor" stage checks operator prerequisites (Ansible, and
terraform for the Proxmox entry point) and prints install commands, never
installing. The stages are fixed and shared from the baseline assertion on:
doctor and entry point; provision (Proxmox only); baseline assertion; GitHub
machine user and PAT; Claude auth mode and login (over SSH for the remote
entry points; a documented precondition for the in-VM entry point, where the
stage becomes a verify); Target Project and Harness config, scaffolded as a
**pull request opened by the machine user** when missing; Ansible configure;
Host extension; verify (machine login and admin, `claude auth status` in the
declared mode, schema-valid config, Provider check when a hermetic block
exists, one dry-run Fire); enable both units, open the bootstrap issue when
the hermetic block is missing, print the summary. The Daemon is enabled even
while the config PR is open: its preflight fails closed on the default-branch
copy and the Dashboard shows "waiting for Harness config". `setup`
**converges** on re-run (Ansible idempotent, secrets prompted only when
absent from the Host env unless a rotate flag forces, scaffold skipped when
the config exists, bootstrap issue idempotent by marker); `upgrade` moves the
ref, re-runs configure and the Host extension and restarts; `check` runs the
verify stage alone. Every prompt, wizard stages included, has a flag or
environment override so the whole run is unattended for the end-to-end test.
Setup remembers how to reach a Host in a non-secret **Host inventory** on
the operator machine (`~/.config/auto-agent/hosts/<name>.env`) and reads
everything else back from the Host env over SSH; secrets are never persisted
on the operator machine.

## Considered options

- **Bash CLI plus wizard stages, no skill**: the same engine with a scripted
  front; rejected because the operator is already in Claude Code and the
  Surfaces interview is a conversation, not a form.
- **Skill as the whole of Setup**: no unattended path, no `check` after a
  rotation, and re-run behaviour left to a prompt. Rejected.
- **Marketplace plugin run inside the Target Project**: nothing is published
  yet; deferred with ADR 0001's marketplace patch.
- **Config written locally for the human to commit, or pushed to the
  default branch directly**: a machine-user PR is the review surface the
  harness already uses and survives branch protection.
- **Setup installs Ansible and terraform on the laptop**: two one-line
  installs are not worth surprising a developer's machine.
- **Stop before enabling until the config PR merges**: forces a second run
  for a merge the human controls.
