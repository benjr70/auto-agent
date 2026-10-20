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

The live sections come from the running Daemon and GitHub. `run` hands the
Dashboard the same sources the Daemon uses; `dashboard.Server` leaves a
section empty when its source is not set:

- **budget**: the Budget gate's verdict (`budget.Sensor`), verbatim, asked
  at most once a minute (`RefreshEvery`). `rc` is the gate's answer as the
  usage sensor's exit code: 0, or 4 (credential dead), 5 (auth mode
  mismatch), 6 (api-key spend pacing). `tiles` are the verdict's limits.
  `message` is one line per auth mode, null when a login Host's endpoint
  spoke normally; with no verdict, the Host config's `CLAUDE_AUTH_MODE`
  picks it. No copy of the gate lives here.
- **daemon**: what the always-on Daemon is doing, held in memory by the
  `run` process (`daemon.Status`) and never written to disk (ADR 0012).
  `state` is `firing` during a Fire, `idle` between polls, `held` while the
  gate waits (with `resetAt`), `parked` on a dead credential (with `parked`:
  the park issue, the reason and `probeAt`), and `unknown` before the first
  Fire. `fail` counts the Fires in a row that failed; the Go Daemon has no
  cap, so `cap` is null. `tail` is the Daemon's last 30 log lines, in
  memory, so `tailError` is always null. `unit` is `systemctl --user show`
  of the unit `service install` writes, null when none is installed.
- **openPrs**: every open PR, each read alone for `mergeable` (a listing
  does not say). `docsOnly` is a research branch. `parked` is how many
  Agent PRs PR Triage's own rule (`state.Ours` and `state.Parked`) parks,
  a PR waiting on a Ruling aside; null, not 0, until the PRs could be read.
- **rulings**: each open PR labelled `AFK:ruling` with an outstanding
  request (`ruling.Read`): its `decisions` count and the request comment's
  `url`. A request that cannot be read still lists the PR, with
  `decisions` null and the PR's own `url`. They ride the `openPrs` refresh.
- **pipeline**: `scan` is what the next Fire would take: the derivation the
  Fire runs (`run --once --dry-run`'s report), with the default branch's
  Harness config read from GitHub: the Daemon's clone may be on a ticket's
  branch.
- **maps**: the open Maps (label `wayfinder:map`), newest first, each with
  its `title`, `url`, `destination` (the line under `## Destination`, or an
  inline `**Destination**:`) and its `frontier`: the native sub-issues that
  are open, unassigned and blocked by no open issue, each with its
  wayfinder `type` and an `AFK` or `HITL` `badge` (null when neither). A
  ticket is blocked by its native `blockedBy` edges (a cut-off page of them
  blocks too) and by the `Blocked by #N` lines of its body; a body blocker
  whose state the query did not see reads as open, so the card hides work
  rather than calling blocked work ready. A Map with more children than
  the query takes is `partial`, and `truncated` is set when any Map is
  partial or there are more Maps than it takes: a cut-off list is never
  silently short. `total` is GitHub's count of open Maps.
- **wayfinder**: the tile's counts: `maps` (open Maps), `frontier` (ready
  tickets across them), and the pick's queue split, the open AFK tickets not
  yet taken: `queueSlices` Slices and `queueWayfinder` Wayfinder tickets
  (`afk` is `queueSlices`, as the Python Dashboard has it). A count that
  cannot be read is null with `unknown: true`, never 0: before the first
  good read, when the last refresh failed, or (the queue's alone) when
  GitHub left the queue out. `truncated` is set when the maps are, or the
  queue has more tickets than the query takes.

The maps and the Wayfinder tile are one GraphQL query, sized under GitHub's
node limit (`maps_test.go` computes it), every five minutes: GraphQL is
outside the ETag cache. Every other GitHub read goes through the client's
ETag cache, so a refresh on a quiet repo costs mostly 304s.

Each section carries `asOf`, `stale` and `error`, and degrades on its own:
a failed refresh keeps the last good value with `stale: true` and the
`error`, and the route still answers 200. When reading the records fails,
`fires` does the same.
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
    "error": "the Budget gate gave no verdict: the usage endpoint answered 500"
  },
  "daemon": {
    "unit": { "active": "active", "mainPid": 1234, "since": "Wed 2026-09-23 09:00:00 UTC" },
    "state": "held",
    "stateDetail": "held by the Budget gate until 2026-09-23T15:00:00Z",
    "stateAt": "2026-09-23T11:01:31Z",
    "resetAt": "2026-09-23T15:00:00Z",
    "fail": { "count": 1, "cap": null },
    "parked": { "issue": 77, "reason": "authentication_failed", "probeAt": "2026-09-23T12:00:00Z" },
    "tail": ["[daemon 2026-09-23T11:01:31Z] held by the Budget gate until 2026-09-23T15:00:00Z"],
    "tailError": null,
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
