# Prototype: status-line `rate_limits` as a post-Fire sensor for setup-token Hosts

Map #1, ticket #22. Status: **resolved 2026-09-15**. Claude Code 2.1.271, Max
subscription (`default_claude_max_5x`), `/login` credential on the live
Smart Smoker box. Verdict below; the asset is this directory.

## Verdict

1. **The status line never runs under `claude -p`.** A `statusLine`
   command passed via `--settings` wrote nothing across five Fires with
   `--output-format json` and `stream-json`, while `SessionStart`, `Stop`
   and `SessionEnd` hooks in the *same* settings file all fired (so the file
   loaded). Hook payloads carry no rate limits either. The status line is a
   TUI renderer; the hook idea in the ticket is dead as written.
2. **The same numbers reach a Fire anyway, on stdout.** With
   `--output-format stream-json` (`--verbose` required) the CLI emits a
   `rate_limit_event` line after each API response whose limit headers
   changed. The event is built from the `anthropic-ratelimit-unified-*`
   response headers, so it is account-real, not a clock estimate:

   ```json
   {"type":"rate_limit_event","rate_limit_info":{"status":"allowed",
     "resetsAt":1789449000,"rateLimitType":"five_hour",
     "overageStatus":"rejected","overageDisabledReason":"org_level_disabled",
     "isUsingOverage":false,
     "unifiedWindows":{"five_hour":{"utilization":0.24,"resetsAt":1789449000},
                       "seven_day":{"utilization":0.06,"resetsAt":1789758000},
                       "seven_day_overage_included":{"utilization":0.11,"resetsAt":1789758000}}},
    "uuid":"…","session_id":"…"}
   ```
3. **Fresh and complete enough to set `remainPct` on the next Gate verdict.**
   Side by side with the usage endpoint on the same account, read within
   three seconds of the event (the live Daemon was burning concurrently, so
   the endpoint reads slightly later):

   | Fire | event 5h / 7d / model | endpoint 5h / 7d / Fable | reset match |
   |---|---|---|---|
   | 3 (haiku) | 23 / 6 / – | 22 / 6 / 1 | 05:10Z, 09-18 19:00Z: exact |
   | 4 (haiku) | 23 / 6 / – | 24 / 7 / 11 | exact |
   | 5 (fable) | 24 / 6 / 11 | 24 / 7 / 12 | exact |

   Both windows are always present; `resetsAt` is epoch seconds and equals
   the endpoint's `resets_at` to the minute; utilization is a 0–1 fraction
   at 1% granularity (same as the endpoint's integer percent). The
   per-model weekly appears as a third window, `seven_day_overage_included`,
   **only when the Fire's model has one** (Fable here) and it is not named by
   model, so the tap keys it to the Fire's model. A Fire that ran on the
   fallback model therefore does not refresh the preferred model's scope;
   the last observation of each scope is kept separately.
4. **Rejection is machine-readable.** The public part of the schema is
   `status` (`allowed | allowed_warning | rejected`), `resetsAt`,
   `rateLimitType` (`five_hour | seven_day | seven_day_opus |
   seven_day_sonnet | seven_day_overage_included | overage`) and
   `utilization`. A `rejected` event with a `rateLimitType` and `resetsAt`
   is the structured form of the "You've hit your … limit · resets …"
   string ADR 0008 parses, so the exhaustion classifier can prefer the event
   and keep the string regex as the fallback for text-mode output.
5. **Caveats.** `unifiedWindows` is marked `@internal` in the CLI's own
   schema ("as read from the anthropic-ratelimit-unified-* response
   headers"), and neither the headless page nor the Agent SDK reference
   documents `rate_limit_event` today; the top-level `status`,
   `rateLimitType`, `resetsAt` and `utilization` are the stable contract, so
   the tap must degrade to "binding window only" if the windows object goes
   away. It is a **post-Fire** number by construction: the first Fire of a
   Host, or after a long idle, still fires optimistically as ADR 0008 says.
   Not tested with a `claude setup-token` bearer (none can be minted
   headlessly); the headers come from the inference API the token is
   documented to call, and Setup's verify stage (dry-run Fire) should assert
   the event appears before enabling a setup-token Host.

## What changes for the harness

- The Fire wrapper (today `agent-run` runs `claude --print` in text mode and
  tees the log) switches to `--output-format stream-json --verbose` piped
  through a tap that (a) forwards the stream to the log and (b) records every
  `rate_limit_event` into the State dir; the last one of the Fire becomes
  `rate-limits.json`.
- ADR 0008's Gate verdict on a setup-token Host: `sensor: "stream-events"`
  (replacing `limit-strings` as the seed), `state: stale` with the Fire's
  `observedAt`, `remainPct` = 100 − max over the recorded windows (the
  preferred model's scope when known), `resetAt` from the binding window,
  `limits[]` = the windows; a `rejected` event sets the reset directly. The
  limit-string regex stays as the text-mode fallback.
- `/login` Hosts keep the endpoint as the primary sensor (pre-Fire, plus
  per-model scopes by name); the tap runs there too and becomes the
  60-minute stale fallback ADR 0008 already describes.

## Files

- `rate-limits-tap.sh`: stdin stream-json → stdout unchanged; records
  `rate_limit_event`s into `$AUTO_AGENT_STATE_DIR/rate-limits.{jsonl,json}`.
- `run-fire.sh`: one tiny Fire through the tap, then the endpoint read via the
  live `usage-sensor.sh` (`SENSOR_SH=…`). `FIRE_MODEL` picks the model.
- `state.PROTOTYPE-wipe-me/`: the five recorded Fires (streams, hook dumps,
  `rate-limits.jsonl`). Fires 1–3 used the abandoned statusLine hook plus the
  Stop/SessionStart/SessionEnd dump hooks (`hooks/*.json`).

## Reproduce

```bash
SENSOR_SH=../Smart-Smoker-V2/scripts/claude-agent/lib/usage-sensor.sh \
  bash docs/research/reusable-auto-agent/statusline-rate-limits/run-fire.sh fire-N
FIRE_MODEL=fable … run-fire.sh fire-N-fable   # to see the per-model window
```
