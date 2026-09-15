# PROTOTYPE: screenshot tour under Xvfb (throwaway)

Answers the map ticket "Prototype: screenshot tour under Xvfb on an Ubuntu Server cloud image":
does the harness screenshot tour (ADR 0003) capture the same thing on the ADR 0004 Host image
(Ubuntu 24.04 Server + Xvfb, fixed `DISPLAY`) as on today's Host (GNOME + XWayland on `:0`)?
And what happens to the Electron sandbox on that image?

## What it is

- `app/server.py`: fixture page (cards, sticky bottom nav, emoji, tall body) served on one port.
- `electron-app/`: a frameless 800x480 Electron shell that loads the fixture and exposes CDP.
- `tour.js`: the tour without the MCP layer: headed Chrome via `playwright-core` at 427x952,
  viewport-clipped shots, then a raw-CDP `Page.captureScreenshot` of the Electron window.
- `run-host.sh`: runs the tour on the live Host (GNOME/XWayland, the checkout's Electron shim).
- `Dockerfile` + `run-in-container.sh` + `run-xvfb.sh`: an Ubuntu 24.04 container as the
  stand-in Host image (Xvfb `:99`, Chrome stable, the Host's Electron 24.8.8 dist mounted in,
  `fonts-noto-core`), runs the tour and the sandbox probes. Two runs: elevated
  (`seccomp=unconfined`, `SYS_ADMIN`) and default confinement.
- `compare.py`: size check and pixel diff between `out/host` and `out/xvfb`.

Run: `./run-host.sh && ./run-xvfb.sh && ./compare.py` (no sudo needed; Docker needed).

## Findings (2026-09-14, Host claude-agent-1, kernel shared by both runs)

**Captures are pixel-identical.** Browser 427x952 and Electron 800x480, DPR 1 on both sides,
0.00% differing pixels at threshold 24/255 for all three shots, even with different Chrome builds
(Host 148, container 153). The one thing that made them differ before it was fixed: **fonts**.
The bare cloud image resolves `sans-serif` to Liberation Mono; with `fonts-noto-core` it resolves
to Noto Sans like the Desktop Host. The Host image must ship the same font packages as the
reference (`fonts-noto-core`, `fonts-noto-color-emoji`, `fonts-liberation`).

**Electron sandbox on Ubuntu 24.04** (`apparmor_restrict_unprivileged_userns=1` on both):

| Mode | Live Host (real VM) | Container, default confinement | Container, elevated |
|---|---|---|---|
| helper 0755, no flag | FATAL abort (`setuid_sandbox_host.cc`) | FATAL abort | starts (userns via SYS_ADMIN, confounded) |
| helper root 4755 | not testable (no sudo) | fails: Docker seccomp blocks PID/net namespaces | starts (confounded) |
| `ELECTRON_DISABLE_SANDBOX=1` | starts (what the shim does today) | starts | starts |
| `--no-sandbox` | starts | not run | not run |

**Chrome** with its sandbox on is "adequately sandboxed" on the live Host: Ubuntu 24.04 ships
`/etc/apparmor.d/chrome`, an `unconfined` profile that grants `userns` to
`/opt/google/chrome/chrome`. Electron has no such profile, hence the abort.

So on the Host image: today's degraded mode reproduces exactly; the SUID-4755 path could not be
proved here (needs root on a VM, and a container cannot stand in for it); the AppArmor `userns`
grant is the mechanism the OS itself uses for Chrome and is path-based, so it survives
`npm install` recreating `node_modules/electron`, but it is untested for Electron.
