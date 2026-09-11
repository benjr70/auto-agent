---
status: accepted
---

# The reference Host is an Ubuntu VM anywhere, reached by Setup through three entry points

The Daemon itself (bash, `claude -p`, `gh`, Node) runs in any non-root Linux
userland with a persistent config dir. What constrains the Host is the Target
Project's Verification Harness: Smart Smoker's needs a real display for headed
Chrome and Electron plus nested Docker for per-PR stacks, and a full VM is the
only runtime where every one of those needs is met by documented behaviour
(LXC has no documented headed-browser path, Docker and Kubernetes need
privileged mode plus Xvfb and add nothing for a single-replica process). We
decided the reference Host is a **VM running Ubuntu 24.04 LTS, wherever it
lives**: Proxmox is the first Provisioner, not the runtime. Setup reaches the
Host through three entry points that converge on one Ansible configure step:
provision a new VM on Proxmox (terraform, cloud-init, then configure over
SSH); bring your own VM (SSH key in, configure); or clone this repo inside the
VM and run Setup locally, in which case that clone *is* the Harness install.
The image is an Ubuntu Server cloud image plus Xvfb on a fixed `DISPLAY`,
not a logged-in desktop session. Base Host needs derive from the Harness
config (a `browser` Surface implies display plus Playwright's Chromium, an
`electron` Surface implies display plus the Electron runtime libraries, and
`host.docker` is explicit); everything project-specific is installed by the
Target Project's **Host extension**, an executable in `.auto-agent/` that
Setup runs after the base role, at Setup and upgrade time only, never per
Fire. The provisioning code lives in this repo as part of the Harness install.

## Considered options

- **LXC as the default, VM for GUI projects**: cheaper, but no documented
  headed-browser path, `nesting` exposes host procfs/sysfs, and the first
  Target Project needs a display. Rejected as reference; kept as a future
  second Host shape for GUI-free projects.
- **Docker image of the Daemon**: right for headless, Docker-free projects,
  wrong for this one (privileged DinD, sandbox off). Out of scope; the Daemon
  stays container-friendly by reading its config dir from the environment.
- **Kubernetes**: every Docker constraint plus a control plane the Daemon
  cannot use. Rejected.
- **A scripted Ubuntu Desktop install**: keeps today's XWayland display but a
  desktop ISO is not a cloud image, so cloud-init provisioning is lost.
  Rejected, gated on a prototype proving the screenshot tour is equivalent
  under Xvfb.
- **Declarative extra packages in `harness.json`** or **a Target Project
  Ansible playbook** instead of the Host extension executable: the former
  runs out of expressiveness, the latter forces a tool on every Target
  Project. Rejected in favour of the same executable-with-a-contract shape
  the Environment provider uses.

## Consequences

- One baseline assertion (Ubuntu 24.04, x86_64 or arm64, systemd, passwordless
  sudo, outbound internet) sits at the top of the configure step and every
  entry point passes through it. Other distributions are a foreseen
  enhancement, so the assertion is a single replaceable check, not scattered
  assumptions.
- The Proxmox Provisioner is a seam: a second Provisioner is a new terraform
  environment behind the same configure step. None ships in v1.
- Setup defaults a VM to 12 GB RAM and 80 GB disk (Daemon peak 6.4 GB RSS
  plus swap, plus per-PR images) and caps the Daemon's cgroup with a systemd
  `MemoryMax`; both overridable.
- Setup can no longer assume an interactive `/login`, so the usage sensor's
  coupling to `~/.claude/.credentials.json` must be verified against a
  `claude setup-token` bearer before Smart Smoker cuts over.
- The Host extension must be idempotent; a non-zero exit fails Setup before
  the Daemon starts.
