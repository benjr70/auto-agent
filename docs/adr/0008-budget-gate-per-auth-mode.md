---
status: accepted
---

# The Budget gate is chosen per auth mode, fires optimistically without a sensor, and parks on credential death

The usage endpoint the live sensor reads is undocumented and refuses a
`claude setup-token` bearer (403, missing `user:profile`), so a Host
authenticated that way has no authoritative budget number, and an API-key
Host never had a window at all. The live Daemon treats every sensor failure
the same: it falls to the ccusage time proxy, a clock estimate that misfired
in production (refused at 9.72% remaining while the account was 81% free),
and it cannot tell a dead login from an exhausted one, sleeping five hours
on both. We decided the **Budget gate is per auth mode**, declared by an
explicit `CLAUDE_AUTH_MODE` key in the Host env that Setup writes and the
sensor cross-checks every Fire against the secrets present and
`claude auth status`'s `authMethod`, failing loud on mismatch. `/login`
stays Setup's recommendation for a subscription Host (ADR 0005 unchanged):
its sensor is the usage endpoint, a 429/5xx keeps the last verdict for at
most 60 minutes, and beyond that the Host behaves like a setup-token Host.
A **setup-token Host fires optimistically** whenever no un-expired limit
verdict exists and gates on the documented limit strings a Fire's output
carries (`You've hit your session/weekly/<model> limit … resets …`), sleeping
to the named reset; a per-model limit switches the Fire model until that
reset under the same Host env model policy the sensor path uses. The
**ccusage time proxy is deleted** in every mode: every mode now has a better
fallback and a wrong number is worse than none. An **API-key Host is
schema-only**: the verdict shape and Host env keys (per-Fire
`--max-budget-usd`, a daily ceiling summed from Fire records'
`total_cost_usd`) are defined, and the mode refuses to start until spend
pacing is implemented, never firing unpaced. **Credential death** (401 from
the endpoint, `claude auth status` exiting 1, or `authentication_failed` in
a Fire) is never exhaustion: the Daemon **parks**, writes the state into the
Fire record and Dashboard, keeps one reused `AFK:needs-human` issue open in
the Target Project, re-probes `claude auth status` hourly and un-parks
(closing the issue) when it passes, so re-running `/login` over SSH is the
whole fix. One **Gate verdict** object serves the sensor's JSON output and
the Fire record's `gate` block: `authMode`, `sensor`
(`usage-endpoint|limit-strings|spend|none`), `state`
(`ok|stale|unavailable|auth-dead`), `remainPct` (null when no sensor has
spoken), `resetAt`, `shouldFire` (always a boolean), `observedAt`, a
`limits[]` list of `{scope, utilization, resetsAt}` so per-model tiles carry
over without endpoint field names, and `warnings[]` (the 3-day login expiry
notice lands here). The Fire record also carries `outcome`, the exhaustion
classifier's result after the Fire, which seeds the next verdict on a
setup-token Host.

## Considered options

- **Recommend setup-token for subscription Hosts**: never dies from a failed
  refresh, but every subscription Host would run on the string-parsing gate;
  refresh death is now detectable, so the reason to flip is gone.
- **Infer the auth mode from which secret is present**: a stale token
  variable would silently switch the gate; kept only as the cross-check.
- **Keep the time proxy as last resort**: adds `npx ccusage` to every Host to
  produce a number known to be wrong.
- **Status-line `rate_limits` as a post-Fire sensor for setup-token Hosts**:
  the documented projection of the same numbers, only after the first API
  response; a follow-up prototype, not a gate dependency.
- **Stay parked until restart**: simpler, but turns a two-minute `/login`
  into a Host restart.

## Addendum (2026-09-15): the setup-token sensor is the stream's rate-limit event

The status-line follow-up ([ticket #22](https://github.com/benjr70/auto-agent/issues/22))
found that a `statusLine` command never runs under `--print`, but that
`--output-format stream-json --verbose` emits a `rate_limit_event` after
each API response whose `anthropic-ratelimit-unified-*` headers changed,
carrying `status` (`allowed | allowed_warning | rejected`), `rateLimitType`,
`resetsAt` and per-window utilization for the 5-hour and 7-day windows plus
an unnamed per-model weekly when the Fire's model has one. Over five Fires
it matched the usage endpoint within one point with identical reset
instants. So the Fire wrapper runs every Fire as stream-json through a tap
that forwards the stream to the log and records the events into the State
dir, and a **setup-token Host's sensor is `stream-events`**: the last event
of the previous Fire seeds the next Gate verdict (`state: stale`,
`observedAt` from that Fire, `remainPct` and `limits[]` from the recorded
windows, the per-model window keyed to the model that fired). A `rejected`
event with its `rateLimitType` and `resetsAt` replaces the limit-string
regex, which stays as the text-mode fallback. `/login` Hosts keep the
endpoint as the pre-Fire sensor and use the tap as the 60-minute stale
fallback. The event's window object is marked `@internal` and the event is
undocumented, so the top-level fields are the contract and the tap degrades
to the binding window if the object disappears; the first Fire of a Host
still goes optimistically. The `sensor` enum gains `stream-events`.
