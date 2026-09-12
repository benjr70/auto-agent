---
status: accepted
---

# The Daemon acts as a dedicated GitHub machine user with a classic PAT and a Claude subscription login

The Daemon needs a GitHub identity that can label, sub-issue, open and merge
PRs and move cards on the Target Project's board, and a Claude identity the
budget gate can read. On a user-owned repo with a user-owned Projects v2
board (Smart Smoker's shape) a GitHub App cannot reach the board (the App
`Projects` permission is organization-only) and a fine-grained PAT cannot
contribute as a collaborator, so the only identity that covers every harness
operation is a **classic PAT (`repo, project, workflow`) on a dedicated
machine user** that is an admin collaborator on the repo and a Write
collaborator on the board. We decided that machine user is **required**, not
the operator's own account: the human's approval then counts as a real
second-party review and `gh api user` returning the machine login is what
human-vs-agent checks key on. One machine user per GitHub owner serves every
Target Project; each Host gets its own PAT so rotation is independent; the
git author is derived from the login (`<login>@users.noreply.github.com`).
For Claude, the Host env declares the auth mode and v1 implements two:
**subscription `/login`** (via `claude auth login`, the Setup default because
it keeps the authoritative usage sensor) and **`claude setup-token`** (for
Hosts where the login expiry or code-paste flow is unacceptable, at the cost
of the usage sensor, which refuses that token for lack of `user:profile`).
API key is accepted by the schema but unimplemented until a Target Project
needs it; cloud providers are out. Every secret lives in one file, the Host
env at `~/.config/auto-agent/env` (mode 0600, the systemd `EnvironmentFile`
for Daemon and Dashboard): `GH_TOKEN`, `DAEMON_GH_LOGIN`,
`CLAUDE_CODE_OAUTH_TOKEN` in setup-token mode, and the rest of ADR 0002's
account and machine facts. The `/login` credential stays where Claude Code
manages it. Secrets are minted by the operator in a browser, handed to Setup
on the operator machine (Ansible `no_log`, never terraform state) or typed at
the prompt when Setup runs inside the VM, and verified before the Daemon is
enabled: the expected machine login with admin on the repo, and
`claude auth status` exiting 0 in the declared mode.

## Considered options

- **GitHub App**: 1-hour installation tokens re-minted from a JWT per Fire;
  covers everything except user-owned Projects. Kept as a documented seam
  (a second token source in the Host env) for an org-owned Target Project.
- **Operator's personal PAT** (what the live Host does today): works, but
  makes the Daemon its own reviewer and indistinguishable from the human.
- **API key / cloud provider**: no session window, spend-paced; unneeded.
