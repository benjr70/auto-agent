# Dashboard

The read-only status page one Host serves for its own Daemon (ADR 0006).
`bin/auto-agent dashboard` runs `server.py` (Python stdlib, no dependencies);
the Dashboard unit (`infra/systemd/auto-agent-dashboard.service.in`) runs the
same command under systemd with the Host env as its `EnvironmentFile`.

It re-implements nothing the Daemon owns:

- **Fire history and the current Fire** come from the Fire records in
  `<state>/fires/` (`lib/fire-record.sh`). A record whose `endedAt` is null is
  the Fire in flight.
- **The budget** is `bin/auto-agent usage-sensor`, shelled to and shown
  verbatim. There is no Python copy of the gate. A setup-token Host has no
  pre-Fire sensor, so the tile says "no usage sensor in this auth mode" and
  shows the last Fire record's verdict beside it; every other auth mode gets its
  own line too (see `sensor_message` in `server.py`).
- **What the Daemon is doing** is `<state>/daemon-state.json`, which the Daemon
  rewrites at every step (`lib/daemon.sh`), plus `<state>/parked.json` and
  `systemctl` for the unit. The journal is read only for the live tail and is
  never parsed.
- **The queue, open Agent PRs and Maps** come from `bin/auto-agent work-probe`,
  `bin/auto-agent show-config` (the repo slug) and `gh`. Labels and branch
  prefixes are the harness vocabulary from `lib/harness-config.sh`.
- **The bootstrap warning** comes from the newest finished Fire record's
  `bootstrap` field, which the Fire derives from the default-branch Harness
  config (ADR 0007). Its `notes` (optional lanes that are declared but switched
  off) are shown beside it.

## Host env

| Key | Default | Meaning |
|---|---|---|
| `AUTO_AGENT_DASHBOARD_BIND` | `127.0.0.1` | Address to bind. Loopback by default; set `0.0.0.0` to opt into all interfaces (the Proxmox entry point does, behind tailscale). |
| `AUTO_AGENT_DASHBOARD_PORT` | `8090` | Port to bind. |
| `AUTO_AGENT_DASHBOARD_SUMMARY` | `1` | The Haiku summary of the in-flight Fire's transcript; `0`, `false`, `no` or `off` turns it off and makes no `claude` call. |
| `AUTO_AGENT_DASHBOARD_SUMMARY_MODEL` | `haiku` | The model that writes that summary. |
| `AUTO_AGENT_TARGET_DIR`, `AUTO_AGENT_STATE_DIR`, `CLAUDE_AUTH_MODE` | as for the Daemon | Read, never re-defined. The gate knobs are the sensor's alone. |

`CLAUDE_BIN`, `GH_BIN`, `SYSTEMCTL_BIN`, `JOURNALCTL_BIN` and `AUTO_AGENT_BIN`
are test seams. There are no POST routes, so the page cannot change anything.

## `/api/status`

`/api/status` is the JSON seam a later aggregator reads across Hosts. It
never returns 500: every section refreshes on its own and, when a refresh
fails, keeps its last good value with `stale: true` and the `error` string.
Sections that come from a command also carry `asOf`. `null` in the block below
means "any value, or null"; otherwise a value may be the type shown or null.
`server.test.py` checks a live response against this block, so edit both
together.

```json
{
  "generatedAt": "2026-09-23T13:31:00+00:00",
  "host": {
    "bind": "127.0.0.1",
    "port": 8090,
    "stateDir": "/home/agent/.local/state/auto-agent",
    "target": "/home/agent/project",
    "repo": "owner/name",
    "summaryEnabled": true
  },
  "budget": {
    "rc": 0,
    "verdict": null,
    "authMode": "login",
    "message": "no usage sensor in this auth mode",
    "tiles": [
      { "key": "session", "label": "Session", "percent": 38, "resetsAt": "2026-09-23T15:00:00Z" }
    ],
    "lastFire": { "fireId": "20260923T130000Z-104", "endedAt": "2026-09-23T13:30:00Z", "gate": null },
    "asOf": "2026-09-23T13:31:00+00:00",
    "stale": false,
    "error": "usage-sensor exit 1 printed no verdict"
  },
  "daemon": {
    "unit": { "active": "active", "mainPid": 1234, "since": "Wed 2026-09-23 09:00:00 UTC" },
    "state": "queue_empty",
    "stateDetail": "queue empty, sleeping until the reset (with work probe)",
    "stateAt": "2026-09-23T11:01:31Z",
    "resetAt": "2026-09-23T15:00:00Z",
    "fail": { "count": 1, "cap": 3 },
    "parked": null,
    "tail": ["[daemon 2026-09-23T11:01:31Z] queue empty, sleeping until the reset (with work probe)"],
    "tailError": "journalctl exit 1: no journal",
    "asOf": "2026-09-23T13:31:00+00:00",
    "stale": false,
    "error": null
  },
  "fires": {
    "items": [
      {
        "id": "20260923T130000Z-104",
        "kind": "pickup",
        "badge": "research",
        "startedAt": "2026-09-23T13:00:00Z",
        "endedAt": "2026-09-23T13:30:00Z",
        "inFlight": false,
        "exit": 0,
        "phase": "claude",
        "summary": "resolve #3 research gh-deps",
        "issue": 3,
        "pr": 12,
        "noWork": false,
        "outcome": "OK",
        "resetAt": "2026-09-23T15:00:00Z",
        "costUsd": 0.42,
        "model": "claude-fable-5-1",
        "line": "resolve: #3 research gh-deps",
        "notes": ["deployed-lane: off — verification.deployed.enabled is false"]
      }
    ],
    "current": null,
    "unreadable": ["20260923T090000Z-99.json"],
    "asOf": "2026-09-23T13:31:00+00:00",
    "stale": false,
    "error": null
  },
  "bootstrap": {
    "warning": true,
    "fireId": "20260923T130000Z-104",
    "notes": ["deployed-lane: off — verification.deployed.enabled is false"]
  },
  "pipeline": { "scan": null, "asOf": "2026-09-23T13:31:00+00:00", "stale": false, "error": null },
  "openPrs": {
    "items": [
      {
        "number": 56, "title": "feat(deps): the deps-land lane (#36)", "url": "https://github.com/owner/name/pull/56",
        "branch": "feat/issue-36", "labels": ["AFK:verify-human"], "mergeable": "MERGEABLE",
        "isDraft": false, "docsOnly": false
      }
    ],
    "asOf": "2026-09-23T13:31:00+00:00",
    "stale": false,
    "error": null
  },
  "maps": {
    "items": [
      {
        "number": 1, "title": "Wayfinder: reusable harness", "url": "https://github.com/owner/name/issues/1",
        "destination": "A spec ready for slicing.",
        "frontier": [
          { "number": 5, "title": "Research: X", "url": "https://github.com/owner/name/issues/5", "type": "research", "badge": "AFK" }
        ],
        "partial": false
      }
    ],
    "total": 1,
    "truncated": false,
    "asOf": "2026-09-23T13:31:00+00:00",
    "stale": false,
    "error": null
  },
  "wayfinder": {
    "maps": 1, "frontier": 1, "afk": 2, "unknown": false, "truncated": false,
    "queueSlices": 2, "queueWayfinder": 1
  },
  "fireSummary": {
    "enabled": true,
    "forFire": "20260923T140000Z-105",
    "title": "Implementing #38: writing tests",
    "description": "The implementer is writing the first failing test.",
    "issue": "issue #38: Setup engine",
    "asOf": "2026-09-23T13:31:00+00:00",
    "stale": false,
    "error": null
  }
}
```

`daemon.state` is one of the states `lib/daemon.sh` documents for
`daemon-state.json`, or `unknown` before the Daemon has written one.
`fires.items[].badge` is `slice`, `reconcile`, `deployed`, `research`, `task`,
`resolve`, `dry-run`, `noop` or null (a Fire that found no work). `budget.verdict`
is the Gate verdict exactly as `usage-sensor` printed it (ADR 0008), and
`budget.lastFire.gate` is the one the newest finished Fire record embeds.

## Run it

```sh
bin/auto-agent dashboard                 # Host env from ~/.config/auto-agent/env
AUTO_AGENT_DASHBOARD_PORT=8091 AUTO_AGENT_STATE_DIR=/tmp/aa-state bin/auto-agent dashboard
curl -s localhost:8091/api/status | jq .budget
python3 dashboard/server.test.py
```
