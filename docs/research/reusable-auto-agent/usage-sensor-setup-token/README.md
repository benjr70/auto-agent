# Task: does the usage sensor work with a `claude setup-token` bearer?

Map #1, ticket #20. Status: **resolved 2026-09-12**; result below.

## Result (2026-09-12, Max subscription, Claude Code 2.1.269)

```
[setup-token] http=403 keys=error,request_id,type
[setup-token] error={"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}
[setup-token] usage_gate rc=3 verdict={"remainPct":0,"resetAt":"","shouldFire":false}
[login-token] http=200 keys=...,five_hour,limits,seven_day,seven_day_opus,seven_day_sonnet,...
[login-token] usage_gate rc=0 verdict={"remainPct":85.00,"resetAt":"2026-09-12T15:50:00+00:00","shouldFire":true}
[login-token] scopes=["user:file_upload","user:inference","user:mcp_servers","user:profile","user:sessions:claude_code"]
VERDICT=rejected
```

- **The usage endpoint rejects a `claude setup-token` bearer**: HTTP 403,
  `permission_error`, "OAuth token does not meet scope requirement
  `user:profile`". The setup-token lacks `user:profile`; the `/login`
  credential carries it. This is a scope decision on Anthropic's side, not a
  transient, so no retry or header change helps.
- The same setup-token **is** a valid login for the CLI: with an empty
  `CLAUDE_CONFIG_DIR`, `claude auth status` exits 0 with
  `"loggedIn": true, "authMethod": "oauth_token"`. So `claude auth status`
  is the documented, zero-cost probe that tells "token rejected by the
  usage endpoint" apart from "token dead": `authMethod` names the mode and
  a dead token exits 1.
- A 403 `permission_error` is distinguishable from a 401
  `authentication_error` ("OAuth access token is invalid."), which the bogus-
  token run produced. The sensor should treat 403 as "this credential can
  never read usage" (switch to the fallback gate permanently for the
  process) and 401 as "credential dead" (park, needs-human).

## Why it matters

`lib/usage-sensor.sh` (and the Dashboard's `fetch_usage`) read
`.claudeAiOauth.accessToken` out of `~/.claude/.credentials.json` and call
`https://api.anthropic.com/api/oauth/usage` with `anthropic-beta:
oauth-2025-04-20`. On a Host authenticated with `CLAUDE_CODE_OAUTH_TOKEN`
(ADR 0004 entry points, auth-modes research #13) that file does not exist, so
today's sensor returns non-zero and the daemon falls back to the ccusage
time proxy before it has asked the endpoint anything.

## What the docs say (2026-09-12, code.claude.com)

- `claude setup-token` "opens the same browser authorization flow as
  `/login`", prints a one-year token, "does not save the token anywhere".
  The command has no flags (`claude setup-token --help`, 2.1.269), so it
  cannot be driven headlessly and there is no scope option.
- The token "authenticates with your Claude subscription ... It can only make
  model requests, so it can't establish Remote Control sessions or fetch
  claude.ai connectors." That is a documented *capability* restriction, which
  is what an OAuth scope reduction looks like from the outside. The `/login`
  credential on this box carries `user:inference`, `user:profile`,
  `user:file_upload`, `user:mcp_servers` and one more scope; a token that
  "can only make model requests" plausibly carries `user:inference` alone.
- `/api/oauth/usage` is still undocumented (re-confirmed on the auth, costs,
  statusline and errors pages). The only documented usage projections are
  the `/usage` screen (interactive only) and the status-line `rate_limits`
  object, "only present for Claude.ai Pro and Max subscribers ... and only
  after the first API response".
- Expiry/revocation of the token surfaces as `OAuth token has expired` /
  `OAuth token revoked` (401); a `/login` credential that cannot refresh
  surfaces as `Failed to authenticate: OAuth session expired and could not
  be refreshed` / `Login expired · Please run /login`.

## Expectation before the run

**Most likely rejected** (401/403 on the usage endpoint) because the usage
endpoint is a profile-class read and the token is documented as
inference-only. If it is accepted, the sensor change is trivial (read the
token from the env before the file). Either way the fallback below is
needed, because the endpoint is unsupported and a Host on an API key or
cloud provider has no such token at all.

## How to get the fact (human, ~2 minutes)

1. On a machine with a browser, logged into the **same** subscription the
   Host will use: `claude setup-token`, approve, copy the token.
2. Anywhere with `curl` + `jq`:

   ```bash
   CLAUDE_CODE_OAUTH_TOKEN=<token> \
   SENSOR_SH=/path/to/Smart-Smoker-V2/scripts/claude-agent/lib/usage-sensor.sh \
   COMPARE_LOGIN=1 \
   bash docs/research/reusable-auto-agent/usage-sensor-setup-token/check-usage-sensor-token.sh
   ```

   `COMPARE_LOGIN=1` also probes the machine's `/login` credential so the
   two shapes sit side by side. The script never prints a token.
3. Paste the output (status lines and the `VERDICT=` line) on ticket #20.
   Revoke the token afterwards if it was minted only for this check
   (claude.ai → Settings → Privacy / connected apps).

Verified without a real token: a bogus bearer gets HTTP 401 with a JSON
error body (`{"type":"authentication_error"...}`), so the script's error
branch and `usage_gate rc=3` degrade path render correctly.

## The sensor's documented-error fallback (needed regardless)

Token source, in order: `CLAUDE_CODE_OAUTH_TOKEN` (if the answer is
"accepted"), then `.credentials.json`, then none. Then:

| Sensor outcome | Meaning | Daemon action |
|---|---|---|
| 200 + parseable limits | authoritative | `usage_gate` verdict (as today) |
| 401/403, or `authentication_error` | token rejected/expired/revoked, or setup-token can't read usage | **not** exhaustion: if `claude auth status` also fails → park in needs-human (HITL signal); if it succeeds (setup-token accepted for inference but not usage) → mark sensor `unavailable` and use the error-parsing gate below |
| 429 / 5xx / network | endpoint rate-limited or down | keep last verdict ≤ 60 min old (mirrors `/usage`'s own "last-known" rule), else error-parsing gate |
| no token at all (API key / cloud Host) | no window exists | spend pacing (`--max-budget-usd`, `total_cost_usd`), never the usage endpoint |

**Error-parsing gate**: after each Fire, read the result for the documented
limit strings and sleep to the named reset:
`You've hit your session limit`, `You've hit your weekly limit` (sleep to
the reset the message names), `You've hit your Opus/Sonnet/Fable limit`
(switch model, as `FABLE_FALLBACK_*` already does), `OAuth token has
expired` / `OAuth token revoked` / `Login expired` / `authentication_failed`
(park, needs-human). The ccusage time proxy remains the last resort.

**Status-line hook** (`rate_limits.{five_hour,seven_day}`): the documented
projection of the same numbers, but only "after the first API response" and
only for Pro/Max, so it can confirm a Fire's burn after the fact but cannot
gate *before* the first Fire of a window. Worth a follow-up prototype only if
the endpoint is rejected for setup-token.
