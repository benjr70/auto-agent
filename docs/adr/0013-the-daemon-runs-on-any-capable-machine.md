---
status: accepted
---

# The Daemon is one binary that runs on any capable machine and installs nothing

ADR 0004 made the reference Host an Ubuntu VM that Setup prepared with
Ansible, optionally provisioned on Proxmox, and ADR 0009 made Setup a skill
over a bash engine run from a clone of this repo (the Harness install of ADR
0001). That put a VM, three entry points and an installer between a person
with a repo and a running Daemon. We decided the Daemon is one Go binary that
assumes its Host can already run the Target Project and its Verification
Harness. It installs and provisions nothing. `init` scaffolds a Harness
config, `doctor` reports what is missing and how to fix it, `run` starts the
Daemon with its Dashboard, and `service install` writes a systemd user unit.
It works in its own clone, keeps one non-secret Host config with every key
defaulted, stores no credential, and acts as whoever its token belongs to; a
Machine user is recommended, not required.

## Considered options

- **Keep the Provisioner as an optional extra**: a second product to
  maintain and test end to end on real hardware. Rejected.
- **Ship a container image as the reference Host**: headed browsers and
  nested Docker need privileged mode; it moves the machine opinion, it does
  not remove it. Rejected.

## Consequences

- Preparing a display, Chromium, Docker or the Electron sandbox is the
  operator's job; `doctor` must be precise about each.
- A Daemon on the operator's own account loses second-party review and
  notifications; `doctor` says so.
- Supersedes ADRs 0004 and 0009, and the Harness install of ADR 0001.
