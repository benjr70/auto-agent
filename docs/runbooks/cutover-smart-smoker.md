# Runbook: Smart Smoker cut-over onto a Harness install

The live Smart Smoker Daemon on `claude-agent-1` runs from files committed in
its own checkout (`~/Smart-Smoker-V2/scripts/claude-agent/`). This runbook moves
it onto a Harness install of this repo in **one switch with a rollback window**
(ticket #17, Slice #43). It is not a hot hand-off: the old units stop, Setup
runs, the new units start. Between those two moments the queue is nobody's, so
everything that can be proven beforehand is proven first.

The order is fixed:

1. the machine user exists and can act on the repo;
2. the parity gate passes from the Harness install, with the old daemon still
   running;
3. the old units stop (their files stay);
4. the Host is cleaned of what the old harness left behind;
5. Setup runs inside the Host, and the new units start;
6. the new Daemon soaks on one AFK ticket and one resolve Fire;
7. the deletion PR on Smart-Smoker-V2 merges. **That ends the rollback
   window.**

Rollback is three commands at any point before step 7; they are at the end.

## What is different about this Host

`claude-agent-1` was built by hand, before Setup existed. Four facts follow, and
each one has a line below:

| Fact | What it means here |
|---|---|
| It runs Ubuntu 26.04 LTS, not the reference 24.04. | Setup's baseline accepts both. |
| Docker is Docker's own `docker-ce`, and Node is nvm's. | Setup keeps a Docker and a Node the units' PATH already resolves and installs neither over them. The nvm directory is not on the default units' PATH, so the Setup command below passes `AUTO_AGENT_UNIT_PATH`. |
| The Host user has sudo, but not passwordless sudo. | Setup's configure step needs it. Step 3 grants it for the run and step 5 takes it away again. |
| The Daemon acts as the maintainer's own GitHub account. | The harness refuses that (ADR 0005). Step 1 creates the machine user. |

Two things are accepted, not fixed, by the cut-over:

- **Fire history starts empty.** The new Dashboard reads Fire records the old
  daemon never wrote. The old logs are archived in step 4, not imported.
- **The Electron sandbox reports DEGRADED.** The list of loaded AppArmor
  profiles is readable by root only on this Host, so a round cannot see the
  grant Setup writes and starts the smoker shell with
  `ELECTRON_DISABLE_SANDBOX=1`, which is what the old harness's shim did. Every
  round says so in its evidence comment.

## Before you start

- [ ] This repo's PR for #43 is merged to `main`. The Harness install floats on
      `main` and needs the baseline and configure changes in it.
- [ ] Smart-Smoker-V2 PR "chore(agent): adopt the auto-agent harness" is merged
      to its default branch. It adds `.auto-agent/` and `scripts/verify/` and
      changes nothing the old daemon reads.
- [ ] No Agent PR the old daemon opened is still open. The harness reconciles
      only Agent PRs **its machine user** opened, so one opened under the
      maintainer's account would be nobody's after the switch. Merge or close
      them first:
      `gh pr list --repo benjr70/Smart-Smoker-V2 --json number,headRefName,author --jq '.[] | select(.headRefName | startswith("feat/issue-") or startswith("research/"))'`.
      Open Dependabot PRs are fine: they are matched by the Dependabot app,
      not by the Daemon's login.
- [ ] Smart-Smoker-V2's deletion PR ("chore(agent): remove the repo-owned
      harness") is open as a **draft** and stays unmerged until step 7.

Every command below runs on `claude-agent-1`, as `claude-agent-1`, in a real
terminal (SSH or the console). Several of them prompt for a sudo password.

## 1. The machine user

A human does this in a browser. Nothing here can be scripted.

1. Create a GitHub account for the Daemon, for example `benjr70-agent`. One
   machine user serves every Target Project the owner has (ADR 0005).
2. As the repository owner: Smart-Smoker-V2 → Settings → Collaborators → add
   the machine user with the **Admin** role.
3. As the owner: the Smart Smoker V2 Project (number 1) → ⋯ → Settings →
   Manage access → invite the machine user with **Write**.
4. As the machine user: accept both invitations.
5. As the machine user: Settings → Developer settings → Personal access tokens
   → Tokens (classic) → Generate new token, with the scopes `repo`, `project`
   and `workflow`. A fine-grained token will not do: the harness needs the
   classic scopes header.
6. Save the token on the Host, one line, mode 0600:

   ```sh
   mkdir -p ~/.config/auto-agent-cutover && chmod 700 ~/.config/auto-agent-cutover
   umask 077 && cat > ~/.config/auto-agent-cutover/gh-pat    # paste, Enter, Ctrl-D
   ```

Prove the token before going on. Each command prints `true`, and the first
also proves the login:

```sh
export GH_TOKEN="$(cat ~/.config/auto-agent-cutover/gh-pat)"
gh api user --jq '.login == "<machine-user>"'
gh api repos/benjr70/Smart-Smoker-V2 --jq .permissions.admin
gh api graphql -f query='{ user(login: "benjr70") { projectV2(number: 1) { viewerCanUpdate } } }' --jq .data.user.projectV2.viewerCanUpdate
unset GH_TOKEN
```

Setup's `github` stage checks the login, the scopes and admin on the repo
again. It does not check the Project, so the third line is the only proof
that the Daemon will be able to read and write the pick signal.

Ticket #17 allows proving the new identity against the old daemon first, by
adding `GH_TOKEN=<the PAT>` to `~/.config/agent-daemon/env` and restarting
`agent-daemon`. It is optional, and this runbook folds the identity into the
one switch instead.

From the switch on, the maintainer's approval of an Agent PR is a real
second-party review: the PR's author is the machine user.

## 2. The Harness install and the parity gate

The Harness install is its own clone, never the checkout anyone develops in:

```sh
git clone https://github.com/benjr70/auto-agent.git ~/auto-agent-install
```

Run the gate while the old daemon is **between Fires** (it refuses to run
while an issue holds `AFK:in-progress`), from a shell that has nvm's Node on
its PATH:

```sh
git -C ~/Smart-Smoker-V2 log -1 --format=%h -- .auto-agent   # prints a commit: the config is in the checkout
~/auto-agent-install/infra/cutover/parity-gate.sh ~/Smart-Smoker-V2
```

If the first command prints nothing, the checkout has not reached the merged
adoption PR yet. The old daemon resets it to the default branch at the start
of every Fire; wait for the next one.

The gate writes nothing to GitHub, to git or to the Host. It prints one line
per step and ends on `parity: PASS — the old daemon can be stopped`:

| Step | What it proves |
|---|---|
| `install` | the install is a clean checkout, and at which commit |
| `config` | Smart Smoker's `.auto-agent/harness.json` matches the schema |
| `lock` | no Fire is in flight |
| `suites` | `bash run-tests.sh` is green from the install (about eight minutes) |
| `queue` | the old daemon's triage and the harness's read the same verdict and name the same issue or PR |
| `provider` | `scripts/verify/provider` conforms: a real per-PR stack boots, smokes and tears down |
| `fire` | one dry-run Fire loads the plugin, reads the Harness config and reaches the pick the triage named |

The `provider` step builds the stack's images and takes several minutes. The
`fire` step spends a little budget (under a dollar on Haiku with
`AUTO_AGENT_FIRE_MODEL=haiku` in front of the command). `--skip-suites`,
`--skip-provider` and `--skip-fire` exist for a re-run after one step failed;
a gate with a skipped step has not passed.

Do not go on until the gate passes. A failed step names what to fix, and the
old daemon is still working the queue while you fix it.

## 3. Stop the old daemon

Grant passwordless sudo for the Setup run:

```sh
echo 'claude-agent-1 ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/90-auto-agent-setup
sudo chmod 440 /etc/sudoers.d/90-auto-agent-setup && sudo visudo -c
```

Stop and disable the old units. Their unit files and `~/.config/agent-daemon/env`
stay exactly where they are: they are the rollback.

```sh
sudo systemctl disable --now agent-daemon.service agent-dashboard.service
```

The old daemon may be stopped at any time. If it was mid-Fire, the issue it
was working still holds the single-flight lock and no Fire will run until it
is cleared:

```sh
gh issue list --repo benjr70/Smart-Smoker-V2 --label AFK:in-progress
gh issue edit <N> --repo benjr70/Smart-Smoker-V2 --remove-label AFK:in-progress   # only if the list was not empty
```

## 4. Host cleanup

The old harness left three things on the Host, and they go now: the old
daemon is stopped, so nothing is using them, and the new one has not started.
The first is a correctness item, not tidiness: local-scope MCP entries for the
checkout would shadow the per-Surface servers a Fire registers. Removing them
does not hurt a rollback, because the old harness's own copies of both names
are in the checkout's `.mcp.json`.

```sh
cd ~/Smart-Smoker-V2
claude mcp remove playwright-chrome -s local
claude mcp remove playwright-electron -s local
claude mcp list                                  # neither name is listed any more

git worktree list                                # what is still registered under .claude/worktrees
git worktree remove --force .claude/worktrees/<name>   # once per stale worktree of an old Fire
git worktree prune

tar -czf ~/claude-agent-logs-$(date +%Y%m%d).tar.gz -C ~ claude-agent/logs && rm -rf ~/claude-agent/logs
```

Look at a worktree before removing it: one that holds work nobody pushed is
not stale. Disk pressure and orphaned MCP processes stay ongoing hygiene; this is the
one-time part.

## 5. Setup

Claude is already logged in on this Host (`claude auth status` says
`claude.ai`), which is the in-VM entry point's precondition. From a shell with
nvm's Node on its PATH:

```sh
cd ~/auto-agent-install
NODE_BIN="$(dirname "$(command -v node)")"
bin/auto-agent setup \
    --gh-login <machine-user> \
    --gh-token-file ~/.config/auto-agent-cutover/gh-pat \
    --set "AUTO_AGENT_UNIT_PATH=$HOME/.local/bin:$NODE_BIN:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    --set AUTO_AGENT_DASHBOARD_BIND=0.0.0.0 \
    --set AUTO_AGENT_GATE_MIN_PCT=20 \
    ~/Smart-Smoker-V2
```

- `AUTO_AGENT_UNIT_PATH` puts nvm's Node on the units' PATH. It names the Node
  version's directory, so it has to be set again (re-run this command) when
  the Node version on the Host changes.
- `AUTO_AGENT_DASHBOARD_BIND=0.0.0.0` keeps the Dashboard reachable over the
  tailnet on port 8090, as today. The Host's ufw rule (LAN denied, `tailscale0`
  allowed) stays as it is; Setup configures no firewall.
- `AUTO_AGENT_GATE_MIN_PCT=20` carries the old `BUDGET_GATE_MIN_PCT=20` over.

What each stage should say on this Host:

| Stage | Expect |
|---|---|
| `baseline` | `ok — Ubuntu 26.04 x86_64, systemd, passwordless sudo, outbound internet` |
| `doctor` | `ok` |
| `github` | `changed — <machine-user> verified (classic PAT, admin on benjr70/Smart-Smoker-V2)` |
| `claude` | `changed — CLAUDE_AUTH_MODE=login verified by claude auth status` |
| `config` | `changed — the checkout commits as <machine-user>; .auto-agent/harness.json present` |
| `configure` | `changed — … base needs: browser, electron, docker, display; kept this Host's own: docker, node` |
| `extension` | `ok — host-extension ran (exit 0)`. The first run installs the smoke runner's Chromium and rebuilds the smoker shell bundle. |
| `verify` | six `check:` lines, all `ok`. The Provider check boots a real stack again, and one dry-run Fire runs. |
| `enable` | `changed — started auto-agent-daemon.service, started auto-agent-dashboard.service` |
| `bootstrap` | `ok — no issue owed` (Smart Smoker declares a hermetic tier) |

Setup stops at the first stage that fails, and a re-run converges, so fix
what the line names and run the same command again. Until `enable` has run,
nothing is working the queue: if a stage cannot be fixed quickly, roll back
(below) and fix it with the old daemon running.

Then check the result:

```sh
bin/auto-agent check
systemctl status auto-agent-daemon auto-agent-dashboard --no-pager
journalctl -u auto-agent-daemon -n 30 --no-pager
curl -s http://127.0.0.1:8090/api/status | jq '{daemon: .daemon.state, fires: (.fires.items | length)}'
```

Take the sudo grant away again, whether Setup passed or you are about to roll
back. The Daemon needs none at run time, and a Fire's deny list refuses `sudo`
in any case:

```sh
sudo rm /etc/sudoers.d/90-auto-agent-setup
```

## 6. Soak

The soak is two Fires on the Daemon's own schedule:

1. **One AFK ticket, end to end**: the Daemon picks it, opens the Agent PR as
   the machine user, drives CI, reviews it once, runs the hermetic
   verification round, and stops. A human approves and merges.
2. **One resolve Fire**: the Daemon picks a `wayfinder:research` Decision
   ticket, researches it, lands the `docs(research):` PR through the docs-only
   gate and closes the ticket.

If the queue has neither, put one of each on it. Then ask the Host where it
stands, as often as you like; the command only reads:

```sh
~/auto-agent-install/infra/cutover/soak-check.sh
```

It prints one line per acceptance criterion and ends on one of:

- `soak: NOT YET — <items>`: the named Fires or the merge have not happened;
- `soak: FAIL — <items>`: something is wrong (the Daemon is not the machine
  user, an old unit is enabled again, the Dashboard reads another State dir);
- `soak: PASS — the deletion PR may merge; that ends the rollback window`.

**Any fix that lands in this repo during the soak is recorded as a ticket
here**, so a rollback knows what the frozen copy in Smart-Smoker-V2 is
missing. Pick it up on the Host with an upgrade (grant sudo as in step 3 for its
run, and remove the grant afterwards):

```sh
git -C ~/auto-agent-install pull --ff-only && ~/auto-agent-install/bin/auto-agent upgrade
```

That is also how the install floats on `main` from now on.

## 7. End the rollback window

When `soak-check.sh` says PASS:

1. Mark Smart-Smoker-V2's deletion PR ready and merge it. The next Fire resets
   the checkout to the default branch, and the repo-owned harness is gone from
   the Host.
2. Remove what was kept for the rollback:

   ```sh
   sudo rm /etc/systemd/system/agent-daemon.service /etc/systemd/system/agent-dashboard.service
   sudo systemctl daemon-reload
   rm -r ~/.config/agent-daemon ~/.config/auto-agent-cutover
   rm -f ~/.local/bin/electron      # the old harness's launcher shim
   ```

3. Close #43.

## Rollback (any time before step 7)

```sh
sudo rm -f /etc/sudoers.d/90-auto-agent-setup      # if step 3's grant is still there
sudo systemctl disable --now auto-agent-daemon.service auto-agent-dashboard.service
sudo systemctl enable --now agent-daemon.service agent-dashboard.service
```

The old daemon runs the copy frozen in Smart-Smoker-V2 when the extraction
branch was cut; it knowingly lacks every fix made in this repo since.

Setup made the checkout commit and push as the machine user. The old daemon
works either way; to put the checkout back as it was:

```sh
cd ~/Smart-Smoker-V2
git config --local --unset user.name; git config --local --unset user.email
git config --local --unset-all credential.https://github.com.helper
```

The Host env, the State dir, the Xvfb unit and the AppArmor profile Setup
wrote are inert once the harness units are disabled. Leave them: a second
attempt converges on them. Setup refuses to run a second Daemon for another
Target Project on this Host, and the single-flight lock keeps two daemons on
one repo from working at once, but do not leave both pairs of units enabled.
