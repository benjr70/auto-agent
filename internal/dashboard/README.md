# Dashboard (Go)

The Dashboard the always-on Daemon serves for its own Host (ADR 0006), in
the `run` process: `auto-agent run` serves it, `auto-agent run --once` does
not. It binds the Host config's `AUTO_AGENT_DASHBOARD_BIND` and
`AUTO_AGENT_DASHBOARD_PORT` (default `127.0.0.1:8090`, loopback). When that
address is taken, `run` prints a warning and runs the Daemon without a
Dashboard: the Python Dashboard (`dashboard/`) holds 8090 on a Host until
cut-over.

Routes, and nothing else: `GET /` (a placeholder until the page lands),
`GET /api/status`, `/favicon.ico`. There is no POST route; any other method
is 405 and any other path is 404.

## `/api/status`

The shape is the Python Dashboard's (`dashboard/README.md`), kept: same keys,
same types, and each Fire item adds its `steps`. The route never returns 500.

What this build fills, all from the Fire records in `<state>/fires/`
(`internal/journal`):

- **host**: bind, port, State dir, target and repo. `summaryEnabled` is
  always false.
- **fires**: the newest 10 records, newest first, each reading as what the
  Fire did. `badge` and `line` come from the record's `kind` and `action`:
  `badge` is `slice` (a pick or a resume), `reconcile`, `resolve` (a
  Decision ticket), `noop` (any other action), or null when the Fire found
  no work. `outcome` is `OK`, `FAILED`, `EXHAUSTED` (paused on the usage
  limit, or told by the Budget gate to wait, with `resetAt` set) or `AUTH_DEAD` (Parked), null while the Fire has not ended;
  `costUsd` and `model` are the sum and the last of the Steps that ran a
  call. Each item's `steps` are its journal Steps, with the tokens of the
  Step's own `usage` (null when the call reported none); a Step the Budget
  gate stopped has a null `outcome`. `current` is the newest record when it
  has no `endedAt` and started under 3 hours ago; an older one, or one with
  a newer record after it, is a crashed Fire, history with `inFlight` false. `unreadable` lists the files that
  would not parse.
- **bootstrap**: the newest finished record that ran a Verification round:
  its `verify.bootstrap` and its `warnings` as `notes`.
- **budget.lastFire**: the newest finished record, with the Gate verdict of
  its last Step that was asked one.
- **fireSummary**: the Fire in flight and the Step it is on, as `title`. No
  model is asked, so `enabled` is false and `description` null.

Every other section (the budget's sensor and tiles, daemon, pipeline,
openPrs, rulings, maps, wayfinder) has its shape with empty values: null,
`[]`, `false`, `"unknown"`, and `wayfinder.unknown` true. Each section
carries `asOf`, `stale` and `error`; when reading the records fails,
`fires` keeps its last good value with `stale: true` and the `error`.
`null` in the block below means "any value, or null"; otherwise a value may
be the type shown or null. `dashboard_test.go` checks a live response
against this block, so edit both together.

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
        "notes": ["deployed-lane: off — verification.deployed.enabled is false"],
        "steps": [
          {
            "name": "implement",
            "model": "claude-fable-5-1",
            "outcome": "OK",
            "startedAt": "2026-09-23T13:01:00Z",
            "elapsedMs": 754000,
            "turns": 41,
            "costUsd": 0.42,
            "inputTokens": 1200,
            "outputTokens": 8400,
            "cacheReadTokens": 310000,
            "cacheCreationTokens": 22000
          }
        ]
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
    "parked": 0,
    "asOf": "2026-09-23T13:31:00+00:00",
    "stale": false,
    "error": null
  },
  "rulings": [
    { "pr": 58, "decisions": 2, "url": "https://github.com/owner/name/pull/58#issuecomment-9001" }
  ],
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
