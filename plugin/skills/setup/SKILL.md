---
name: setup
description:
  Guided Setup of the auto-agent harness: turns a Target Project plus a Host
  into a running Daemon and Dashboard. Interviews the operator for the Surfaces
  and the hermetic tier, drafts the Harness config and the PR that proposes it,
  then drives the `bin/auto-agent` engine stage by stage (in the VM, over SSH
  to a VM you bring, or on a Proxmox VM it provisions), explains each stage
  and diagnoses a failed one. Also `check` and `upgrade` of a known Host. Run
  from a clone of the auto-agent repo with the Target Project as an argument.
disable-model-invocation: true
---

# Setup

Setup is a conversation in front of a converging CLI engine (ADR 0009). You own
the conversation: the Surfaces interview, the Harness config draft, the body of
the PR that proposes it, one plain sentence per engine stage, and the diagnosis
when a stage fails. The engine, `bin/auto-agent`, owns everything mechanical
and is **the only thing that writes to a Host**.

**You never write to a Host yourself.** Every Host write happens inside an
engine call (`"$AA" setup`, `"$AA" upgrade`, `"$AA" check`). You never `ssh` to
a Host, never run `ansible-playbook`, `terraform`, `systemctl`, `sudo` or a
package manager, never edit a Host env, never `git push` or `gh pr create` on
the Target Project, and never create labels or issues. The one thing you write
is the draft and its PR body, into a scratch directory on the Operator machine.
If a fix seems to need a Host write, it is either an engine re-run with
different flags or an operator action you name for them to run; never
something you do on the Host. That keeps the skill from doing what the
unattended path cannot.

Vocabulary (Target Project, Host, Operator machine, Harness install, Host env,
Host inventory, Surface, Environment provider, Bootstrap state, Bootstrap
issue, Provider check, Machine user): this repo's `CONTEXT.md`.

## Invocation

```
/auto-agent:setup <target> [--host <user@vm|name> | --provision proxmox --name <name>] [--draft-only] [--answers <file>] [-- <engine options>]
/auto-agent:setup check <name|target-dir>
/auto-agent:setup upgrade <name|target-dir> [--ref <ref>]
```

- `<target>`: the Target Project: a local checkout path, or `owner/name`
  (then read it through a read-only clone into your scratch directory:
  `gh repo clone <owner/name> "$SCRATCH/target" -- --depth 50`).
- The entry point (ADR 0004): none of `--host`/`--provision` means **in-VM**
  (this machine is the Host, and this clone is its Harness install);
  `--host` means **bring your own VM** over SSH; `--provision proxmox` means
  **Proxmox** (a new VM, then the same stages over SSH). When the operator gave
  none, ask which one before anything else.
- `--draft-only`: interview, write and validate the draft and PR body, print
  their paths, and stop. No engine call, no Host, no GitHub write. This is the
  skill's own test seam.
- `--answers <file>`: the unattended override for the interview: free text or
  JSON with the operator's answers. Use them instead of asking; for anything
  they leave out, take your recommended answer and list it under
  "Assumptions" in the PR body. Never stop to ask when `--answers` is given.
- Everything after `--` is handed to the engine untouched (for example
  `--set AUTO_AGENT_MEMORY_MAX=6G`, `--ref`, `--tailscale`, `--ssh-identity`).

## Harness context

This skill runs interactively, from a clone of this repo, outside any Fire:

```bash
# The clone you started Claude Code in is the operator-side install; the plugin
# root is not always exported to the Bash tool in an interactive session.
AA_ROOT="${AUTO_AGENT_ROOT:-${CLAUDE_PLUGIN_ROOT:+${CLAUDE_PLUGIN_ROOT%/plugin}}}"
AA="${AA_ROOT:-$(git rev-parse --show-toplevel)}/bin/auto-agent"
test -x "$AA" || echo "no bin/auto-agent here: start Claude Code in a clone of the auto-agent repo"
SCRATCH="$(mktemp -d)"; chmod 700 "$SCRATCH"   # the draft, the PR body, the engine log
"$AA" setup --help                               # the engine's options and exit codes, current
```

Read `"$AA" setup --help` once per run: its option list and exit codes are the
source of truth when they differ from this page. The schema is
`plugin/schema/harness.schema.json`; `plugin/fixtures/target-project/.auto-agent/harness.json`
is a complete worked example.

## Secrets

**Never ask the operator to paste a secret into this conversation**, and never
`cat`, `echo`, `grep` or otherwise read a secret file. The engine reads every
secret from a file or its environment and hands it to the Host itself (no_log).
When the run needs one the Host env does not already hold, ask the operator to
save it in a 0600 file **in their own terminal** and tell you only the path:

| Secret | Engine flag | When |
| --- | --- | --- |
| The Machine user's classic PAT (`repo`, `project`, `workflow`; admin on the repo) | `--gh-token-file <f>` | first run, or `--rotate` |
| The `claude setup-token` token | `--claude-token-file <f>` | `--auth-mode setup-token` only |
| The Proxmox API token (`user@realm!id=secret`) | `--proxmox-token-file <f>` | every Proxmox run |
| The Tailscale auth key | `--tailscale-authkey-file <f>` | `--tailscale`, until the Host joins |

A suggestion they can run themselves:
`install -m 600 /dev/null ~/.config/auto-agent/pat && "${EDITOR:-nano}" ~/.config/auto-agent/pat`.
After a green run, remind them the engine never needs these files again
(a re-run reads the Host env) and they may delete them.

On a re-run against a Host that already has its Host env, pass no secret at
all: the engine reuses what the Host holds.

## Process

### 1. Read the Target Project

Before asking anything, read what the repo already says: its README, its
build and test entry points (package manifests, Makefile, scripts), its
conventional-commit scopes (`git log --format=%s -200`), what it serves
(web frontends, Electron apps, CLIs, HTTP APIs) and where each lives, any
existing environment script, and whether it uses Docker.

Then check whether a Harness config already exists, on the default branch or
in an open Setup PR:

```bash
test -f "$TARGET_CHECKOUT/.auto-agent/harness.json" && "$AA" check-config "$TARGET_CHECKOUT"
gh pr list --repo <owner/name> --head auto-agent/harness-config --state open --json number,url
```

- Present and valid: **skip the interview**. The engine's config stage keeps it
  as is; tell the operator so and go to step 4.
- A Setup PR is open: skip the interview too; the engine waits for that PR.
- Otherwise: interview (step 2).

### 2. The interview

One question at a time, each with your **recommended answer** drawn from what
you read, so the operator can mostly say yes. Cover, in order:

1. **commit_scopes**: the scopes the repo's commits already use: the part in
   parentheses (`feat(app): …` gives `app`), never the type (`feat`, `fix`).
2. **commands**: `install` and `test` (required), `lint`, `lockfile_refresh`,
   `plan_gated_paths` (globs whose change needs a plan comment first).
3. **pick**: label-only (`{"labels": {}}`, AFK tickets oldest first) or a
   Project board (`{"project": {"number": <n>, "priority_field": "Priority",
   "order": ["P0","P1","P2"]}}`). Recommend label-only unless they already run
   a board.
4. **Surfaces**: every thing the verifier can drive. For each: a lowercase
   name; `kind` (`browser`, `electron`, `cli`, `api`); `url_key`, the
   uppercase key the Environment provider prints for it (`WEB_URL`, …);
   `paths`, the globs whose change marks the Surface touched; `viewport`
   (`WIDTHxHEIGHT`) for a browser or electron Surface; `launcher` for an
   electron Surface. Say why it matters: a touched browser or electron Surface
   always gets a screenshot tour, and those kinds make the Host install a
   display (and Chromium or the Electron runtime).
5. **The hermetic tier** (`verification.hermetic`): does the repo already have
   an executable that boots the environment per PR (`up --pr N` printing
   `KEY=value` lines, `down --pr N`, optional `smoke`)? If yes, its `command`
   and whether `smoke` is supported. If not, leave the block out and say what
   follows: the **Bootstrap state**. The Daemon still works tickets, every
   Agent PR gets `AFK:verify-human`, the Dashboard warns, and Setup opens one
   **Bootstrap issue** asking the Daemon to write the provider itself
   (ADR 0007). Point at `plugin/providers/CONTRACT.md` and the two reference
   providers if they would rather write it now.
6. **Optional lanes**: `verification.deployed` (a live environment the
   Deployed tier checks read-only), `dependabot` (the deps-land lane),
   `host.docker` (the Host needs Docker), `required_checks`,
   `docs_research_prefix` (where research notes land), `rounds`. Include a
   block only when the operator wants it: its presence switches the lane on.

With `--answers`, take each answer from the file; do not ask.

### 3. Draft, validate, propose

Write the draft to `$SCRATCH/harness.json` and validate it with the engine's
own schema check:

```bash
"$AA" check-config --draft "$SCRATCH/harness.json"
```

Exit 1 prints one line per schema error: fix the draft and check again until
it passes. Never hand the engine a draft that has not passed. Then show the
operator the draft and get their yes (or edits).

Write the PR body to `$SCRATCH/pr-body.md`. The engine opens the PR as the
Machine user with this body verbatim. It must carry:

- one line saying what the PR adopts (one `.auto-agent/harness.json`, ADR 0002,
  opened by the Machine user during Setup) and that the Daemon is already
  enabled but its preflight fails closed until this merges;
- **Surfaces**: each one, its kind, and why those paths;
- **Hermetic tier**: the provider, or "none yet: the Bootstrap state, and the
  Bootstrap issue follows once this merges";
- **Assumptions**: every answer you chose without the operator (always present
  under `--answers`);
- a `Before merging, check:` list of `- [ ]` items the reviewer ticks
  (commands are real, the pick signal is right, the Surfaces' paths, the
  hermetic block).

With `--draft-only`, stop here. Write these two lines in your assistant
message (never `echo` them from Bash):

```
setup-draft: ok — <absolute path of harness.json> (<n> surfaces, hermetic: <command|none>)
setup-draft: pr-body — <absolute path of pr-body.md>
```

If the draft never passed validation, write
`setup-draft: FAIL — <the first schema error>` instead.

### 4. Run the engine

Build the one command for the chosen entry point:

| Entry point | Command |
| --- | --- |
| in-VM | `"$AA" setup --unattended [--repo <owner/name>] <engine flags> <target-dir>` |
| bring your own VM | `"$AA" setup --host <user@vm\|name> --unattended [--repo <owner/name>] <engine flags> [<target-dir on the Host>]` |
| Proxmox | `"$AA" setup --provision proxmox --name <name> --unattended --proxmox-endpoint <url> --node <node> --ipv4 <cidr> --gateway <ip> --proxmox-token-file <f> [--repo <owner/name>] <engine flags> [<target-dir on the Host>]` |

The engine flags this skill adds: `--config "$SCRATCH/harness.json"
--config-pr-body "$SCRATCH/pr-body.md"` when step 3 drafted one (never when a
config already exists), `--gh-login <machine user>`, the secret-file flags from
**Secrets** the run needs, `--auth-mode login|setup-token`, and whatever
followed `--`. For `--host`/`--provision` the target dir is a path on the Host
(on a re-run the Host inventory remembers it; on a first run propose
`~/<repo name>`), and `--repo` lets the engine clone it there.

Always pass `--unattended`: the Bash tool has no terminal, so an engine prompt
cannot be answered; the engine then fails naming the flag it needed, which you
turn into a question for the operator.

**Show the operator the full command and get a yes before running it.** It
provisions or changes a machine and opens a PR as the Machine user. Then run
it with its output kept:

```bash
"$AA" setup ... > "$SCRATCH/engine.out" 2> "$SCRATCH/engine.err"; echo "exit=$?"
```

A run can take several minutes (Ansible, a Proxmox VM's first boot, a dry-run
Fire); use a long Bash timeout, or run it in the background and wait for it.

### 5. Narrate each stage

Every stage prints one machine line, `setup: <stage>: ok|changed|skipped|FAIL
— <detail>`, and the verify stage adds `check: <item>: ...` lines. Walk the
operator through them in order, one plain sentence each, quoting the detail:

| Stage | What it means |
| --- | --- |
| `operator` | this machine has what the remote entry points need (ssh, Ansible, jq, git; terraform for Proxmox) |
| `provision` | the Proxmox VM exists, answers SSH, and cloud-init finished |
| `ssh` | the Host answers over SSH |
| `tailscale` | the Host is on (or joining) your tailnet |
| `install` | the Harness install sits at the pinned ref on the Host; secrets handed over (no_log) |
| `claude-login` | the Host's Claude login, over SSH |
| `inventory` | the Host inventory entry on this machine (how to reach the Host again; no secret) |
| `baseline` | Ubuntu 24.04, x86_64/arm64, systemd, passwordless sudo, outbound internet, one Daemon per Target Project |
| `doctor` | the Host's own prerequisites are on PATH |
| `github` | the Machine user's classic PAT: the login, the scopes, admin on the repo |
| `claude` | `claude auth status` agrees with the declared auth mode |
| `config` | the checkout and its Harness config; `changed — proposed …` names the Setup PR, `waiting for Harness config` means that PR is still open |
| `configure` | the Ansible configure step: base needs from the Surfaces and `host.docker`, the display, the Host env, the units |
| `extension` | the Target Project's Host extension (`skipped` when it has none) |
| `verify` | the `check:` lines: Host env, Machine user, Claude, schema, Provider check, one dry-run Fire |
| `enable` | the Daemon and Dashboard units enabled and running |
| `bootstrap` | the Bootstrap issue, opened once when there is no hermetic tier |
| `restart` | (`upgrade`) both units restarted on the moved Harness install |

End with the engine's own summary: `setup: converged — nothing changed` or
`setup: done — <n> changed: ...`, the Dashboard address from the summary (it
is loopback on the Host unless `--tailscale` opened it), the Setup PR to review
and merge, and the Bootstrap issue if one was opened. Then the follow-ups:
`"$AA" check <name>` after a rotation, `"$AA" upgrade <name> --ref <ref>` to
move the Harness install.

### 6. When a stage fails

The engine stops at the first failing stage and exits with its code. Explain
the failure **with the engine's output**, in this order:

1. The exit code and the `FAIL` line, verbatim, in a code block, with any
   stderr lines above it that explain it (the tail of `engine.err`: an Ansible
   task, a terraform error, the dry-run Fire's last lines).
2. One or two sentences on what it means, using the table below.
3. **The next command**: the operator action that fixes it, then the exact
   `"$AA" setup ...` command to re-run, written out in full with the
   operator's flags (setup converges, so a re-run redoes only what is not
   done yet).

| Exit | Stage | Usual cause → next command |
| --- | --- | --- |
| 2 | usage | a missing target or unknown option → correct the command |
| 3 | baseline | not Ubuntu 24.04 / no systemd / sudo asks for a password / no internet / the Host already serves another Target Project → a Host that meets it (ADR 0004) |
| 4 | doctor or operator | a missing command → the install line the engine printed, on the machine it names, then re-run |
| 5 | github | the PAT is someone else's, fine-grained (no scopes header), lacks `repo`/`project`/`workflow`, or the Machine user is not admin → mint a classic PAT as the Machine user / grant admin, save it to a file, re-run with `--gh-token-file` (and `--rotate` if the Host env holds the old one) |
| 6 | claude | not logged in → `claude auth login` on the Host (for a remote Host: `ssh -t <host> claude auth login`, which the operator runs); setup-token refused → a fresh `claude setup-token`, `--claude-token-file`, `--rotate` |
| 7 | config | the existing config does not validate → fix it in the Target Project; the draft did not validate → back to step 3; push or PR creation failed → the Machine user's rights on the repo |
| 8 | configure | an Ansible task failed → the task named in the log tail (the engine prints the log path) |
| 9 | extension | the Target Project's Host extension exited non-zero → fix it (it must be idempotent), re-run |
| 10 | verify | each `check: <item>: FAIL` line names what failed; the units stay disabled until it passes; `"$AA" check <name>` re-checks without changing anything |
| 11 | enable / restart | a unit did not start → `journalctl -u <unit>` on the Host, which the operator runs |
| 12 | bootstrap | the Bootstrap issue could not be opened → the Machine user's issue rights |
| 13 | ssh | the Host does not answer → address, `--ssh-port`, `--ssh-identity` |
| 14 | install / inventory | the install play failed (its tail is above the line) or the Host inventory could not be written; `--tailscale` without an auth key |
| 15 | provision | terraform or the new VM's first boot → the terraform error above the line; the Proxmox token, node, datastore, address |

Never retry blindly: re-run only after the operator has done the fix, or with
the flag the failure asked for. Never work around a failure on the Host.

### 7. check and upgrade

`/auto-agent:setup check <name>` runs `"$AA" check <name>` (a Host inventory
name, or a target dir in-VM) and walks the `check:` lines the same way; it
writes nothing. `/auto-agent:setup upgrade <name> [--ref <ref>]` shows the
operator `"$AA" upgrade <name> [--ref <ref>]`, runs it on their yes, and
narrates `install`, `configure`, `extension` and `restart`. Both use
**When a stage fails** for a non-zero exit.
