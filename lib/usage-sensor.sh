#!/usr/bin/env bash
# Usage sensor: the Budget gate's one sensor, chosen per auth mode, never
# guessing (ADR 0008 and addendum). It reads CLAUDE_AUTH_MODE from the Host
# env, cross-checks it against the secrets present and `claude auth status`,
# and emits the Gate verdict the Daemon fires on, the Fire record embeds and
# the Dashboard shells to. The clock-based time proxy is gone: every mode has
# a better fallback and a wrong number is worse than none.
#
# Source this file, then:
#
#   usage_verdict
#       Prints the Gate verdict JSON on stdout, always, and returns:
#         0  a verdict was computed (branch on .shouldFire)
#         4  auth-dead: the credential is dead; the Daemon runs
#            `bin/auto-agent park enter` (lib/daemon-park.sh) and re-probes
#            with `park reprobe` hourly
#         5  mode mismatch: CLAUDE_AUTH_MODE contradicts the secrets or
#            `claude auth status`; the Daemon refuses to fire until fixed
#         6  api-key mode refuses to start until spend pacing exists
#
#   usage_endpoint_verdict <payload-json> [<now-epoch>]
#   usage_event_verdict <rate-limits-record-json> [<now-epoch>]
#   usage_outcome_verdict <outcome-json> [<now-epoch>]
#       The pure verdict builders behind usage_verdict, one per sensor, for
#       tests and the Dashboard: from the usage endpoint's payload, from the
#       last tapped rate_limit_event (lib/rate-limits-tap.sh's record) and
#       from the last Fire's outcome (lib/exhaustion-classifier.sh's verdict).
#
#   usage_model_family <model-id>
#       "claude-fable-5-1" -> "fable", "opus" -> "opus": the scope name a
#       per-model limit carries in limits[].
#
# The Gate verdict (one object for the sensor, the Fire record and the
# Dashboard):
#
#   {
#     "authMode":   "login" | "setup-token" | "api-key" | null,
#     "sensor":     "usage-endpoint" | "stream-events" | "limit-strings" | "spend" | "none",
#     "state":      "ok" | "stale" | "unavailable" | "auth-dead",
#     "remainPct":  <0..100> | null           (null when no sensor has spoken)
#     "resetAt":    "<ISO>" | null            (when the binding limit frees)
#     "shouldFire": <bool>                    (always a boolean)
#     "observedAt": "<ISO>"                   (when the sensor's number was read)
#     "limits":     [ { "scope", "utilization", "resetsAt" } ]
#                   scope is "session" (5-hour), "weekly" (7-day) or a model
#                   family ("fable"); utilization is 0..100
#     "warnings":   [ "<text>" ]              (the 3-day login expiry notice the
#                                              last Fire printed lands here)
#     "fireModel":  "<model>" | null          (the model policy's switch)
#     "fireModelUntil": "<ISO>" | null        (the switch lasts until this reset)
#   }
#
# Per mode:
#
#   login        the usage endpoint is the pre-Fire sensor (`usage-endpoint`,
#                `ok`). 429/5xx or a network failure keeps the last good verdict
#                for at most AUTO_AGENT_GATE_STALE_MAX_SECS (3600) as `stale`;
#                beyond that the Host behaves like a setup-token Host. 403 marks
#                the endpoint unavailable for the Daemon process (the mark is
#                keyed to AUTO_AGENT_DAEMON_ID; with no id it holds for that
#                call only) and falls through the same way. 401 is auth-dead.
#   setup-token  fires optimistically: the last tapped rate_limit_event seeds
#                the verdict (`stream-events`, `stale`, observedAt from that
#                Fire, per-model window keyed to the model that fired); a
#                `rejected` event on an account window sets resetAt and holds
#                the Fire until then; the last Fire's EXHAUSTED outcome
#                (`limit-strings`) does the same when it is newer than the
#                event. With no un-expired limit verdict at all: `none`,
#                `unavailable`, remainPct null, shouldFire true.
#   api-key      schema-only: `spend`, `unavailable`, shouldFire false, rc 6.
#
# Per-model limits never gate. Under the model policy a per-model limit on
# AUTO_AGENT_MODEL_PRIMARY at or above AUTO_AGENT_MODEL_SWITCH_PCT, or a
# rejected event / limit string naming it, sets fireModel to
# AUTO_AGENT_MODEL_FALLBACK until that limit's reset.
#
# Writes <state-dir>/usage-sensor.json: the last good endpoint verdict (the
# stale hold) and the 403 mark.
#
# Environment (Host env unless noted):
#   CLAUDE_AUTH_MODE               login | setup-token | api-key (required)
#   CLAUDE_CODE_OAUTH_TOKEN        present only in setup-token mode
#   ANTHROPIC_API_KEY              present only in api-key mode
#   AUTO_AGENT_GATE_MIN_PCT        remainPct at or above which a Fire starts (25)
#   AUTO_AGENT_GATE_STALE_MAX_SECS the endpoint's stale hold (3600)
#   AUTO_AGENT_MODEL_PRIMARY       the model family the Fires run on (fable)
#   AUTO_AGENT_MODEL_FALLBACK      the family a spent primary switches to (opus;
#                                  set empty to never switch)
#   AUTO_AGENT_MODEL_SWITCH_PCT    per-model utilization that switches (95)
#   AUTO_AGENT_DAEMON_ID           the Daemon process's id, scopes the 403 mark
#   USAGE_API_URL                  the endpoint (https://api.anthropic.com/api/oauth/usage)
#   USAGE_CREDS_FILE               the login credential (~/.claude/.credentials.json,
#                                  or under CLAUDE_CONFIG_DIR)
#   CLAUDE_BIN, CURL_BIN           injected for tests (claude, curl)
#   USAGE_SENSOR_NOW               epoch "now" for tests

_usage_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=host-env.sh
. "${_usage_lib_dir}/host-env.sh"
# shellcheck source=fire-record.sh
. "${_usage_lib_dir}/fire-record.sh"

USAGE_SENSOR_STATE_FILE="usage-sensor.json"
USAGE_SENSOR_MODES="login setup-token api-key"
USAGE_SENSOR_ACCOUNT_SCOPES='["session","weekly"]'

_usage_now() { printf '%s' "${USAGE_SENSOR_NOW:-$(date -u +%s)}"; }
_usage_iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
_usage_err() { echo "usage-sensor: $*" >&2; }

# usage_model_family <model-id>
usage_model_family() {
    printf '%s' "${1:-}" | jq -R -r 'ascii_downcase | sub("^claude-"; "") | split("-") | map(select(test("^[a-z]+$"))) | first // ""'
}

# The jq prelude every verdict builder shares: the policy knobs as $vars and
# the shape function. Exported as a string so the builders stay one jq call.
_usage_jq_defs='
    def family: ascii_downcase | sub("^claude-"; "") | split("-") | map(select(test("^[a-z]+$"))) | first // "";
    def scope_of($window; $model):
        if $window == "five_hour" then "session"
        elif $window == "seven_day" then "weekly"
        elif ($window | test("^seven_day_") and . != "seven_day_overage_included") then ($window | sub("^seven_day_"; "") | family)
        elif $window == "seven_day_overage_included" and ($model // "") != "" then ($model | family)
        else $window end;
    def unexpired: select(.resetsEpoch == null or .resetsEpoch > $now);
    def account: [ .[] | select(.scope == "session" or .scope == "weekly") ];
    def per_model: [ .[] | select(.scope != "session" and .scope != "weekly") ];
    def binding: [ .[] | select(.utilization != null) | unexpired ] | max_by(.utilization);
    def switch($limits; $rejected_scope):
        if $fallback == "" then null
        elif ($rejected_scope // "") == $primary then $fallback
        elif ([ $limits | per_model[] | select(.scope == $primary) | unexpired | select(.utilization != null and .utilization >= $switch_pct) ] | length) > 0 then $fallback
        else null end;
    def tile: { scope, utilization, resetsAt };
    def verdict($sensor; $state; $limits; $rejected; $observed; $warnings):
        ($limits | account | binding) as $b
        | (if $b == null then null else ((100 - $b.utilization) | if . < 0 then 0 elif . > 100 then 100 else . end) end) as $remain
        | ($rejected != null and (($rejected.scope == "session") or ($rejected.scope == "weekly"))) as $held
        | switch($limits; (if $rejected == null then null else $rejected.scope end)) as $model
        | {
            authMode: (if $mode == "" then null else $mode end),
            sensor: $sensor, state: $state,
            remainPct: $remain,
            resetAt: (if $held then $rejected.resetsAt elif $b == null then null else $b.resetsAt end),
            shouldFire: (if $held then false elif $remain == null then true else ($remain >= $min_pct) end),
            observedAt: $observed,
            limits: ($limits | map(tile)),
            warnings: ($warnings
                       + (if $model == null then [] else ["model policy: \($primary) limit spent, Fires run on \($model)"] end)
                       + (if $held then ["\($rejected.scope) limit rejected the last Fire, holding until \($rejected.resetsAt)"] else [] end)),
            fireModel: $model,
            fireModelUntil: (if $model == null then null
                             elif ($rejected != null and $rejected.scope == $primary) then $rejected.resetsAt
                             else ([ $limits | per_model[] | select(.scope == $primary) | .resetsAt ] | map(select(. != null)) | max) end)
          };
'

# _usage_jq <filter> [jq args...] : jq with the shared defs and policy $vars bound.
_usage_jq() {
    local filter="$1"; shift
    jq -c \
        --arg mode "${CLAUDE_AUTH_MODE:-}" \
        --argjson now "${_USAGE_NOW:-$(_usage_now)}" \
        --argjson min_pct "${AUTO_AGENT_GATE_MIN_PCT:-25}" \
        --arg primary "${AUTO_AGENT_MODEL_PRIMARY:-fable}" \
        --arg fallback "${AUTO_AGENT_MODEL_FALLBACK-opus}" \
        --argjson switch_pct "${AUTO_AGENT_MODEL_SWITCH_PCT:-95}" \
        "$@" "${_usage_jq_defs} ${filter}"
}

# _usage_plain <sensor> <state> <shouldFire> <warning...> : a verdict with no limits.
_usage_plain() {
    local sensor="$1" state="$2" fire="$3"; shift 3
    local warnings; warnings="$(printf '%s\n' "$@" | jq -R -s -c 'split("\n") | map(select(length > 0))')"
    printf 'null' | _usage_jq 'verdict($sensor; $state; []; null; ($now | todate); $warnings) | .shouldFire = $fire' \
        --arg sensor "${sensor}" --arg state "${state}" --argjson fire "${fire}" --argjson warnings "${warnings}"
}

# usage_endpoint_verdict <payload-json> [<now-epoch>]
usage_endpoint_verdict() {
    local payload="${1:?payload required}" _USAGE_NOW="${2:-$(_usage_now)}"
    printf '%s' "${payload}" | _usage_jq '
        def epoch: if . == null then null else (sub("\\+00:00$"; "Z") | try fromdate catch null) end;
        def named($k; $scope): .[$k] | select(type == "object" and (.utilization | type) == "number")
            | { scope: $scope, utilization: .utilization, resetsAt: (.resets_at // null), resetsEpoch: (.resets_at | epoch) };
        ([ named("five_hour"; "session"), named("seven_day"; "weekly") ]
         + [ to_entries[] | select(.key | test("^seven_day_") and . != "seven_day_overage_included")
             | (.key | sub("^seven_day_"; "") | family) as $m | select($m != "")
             | .value | select(type == "object" and (.utilization | type) == "number")
             | { scope: $m, utilization: .utilization, resetsAt: (.resets_at // null), resetsEpoch: (.resets_at | epoch) } ]) as $named
        | ([ .limits[]? | select(type == "object" and (.percent | type) == "number" and (.scope | type) == "object")
             | ((.scope.model.display_name // "") | family) as $m | select($m != "")
             | { scope: $m, utilization: .percent, resetsAt: (.resets_at // null), resetsEpoch: (.resets_at | epoch) } ]) as $scoped
        | ($named + $scoped | map(.resetsEpoch = null)
           | reduce .[] as $l ([]; if any(.[]; .scope == $l.scope) then . else . + [$l] end)) as $limits
        | if ($limits | account | length) == 0 then
            verdict("usage-endpoint"; "unavailable"; $limits; null; ($now | todate); ["usage endpoint payload carries no account window"]) | .shouldFire = false
          else verdict("usage-endpoint"; "ok"; $limits; null; ($now | todate); []) end'
}

# usage_event_verdict <rate-limits-record-json> [<now-epoch>]
usage_event_verdict() {
    local record="${1:?record required}" _USAGE_NOW="${2:-$(_usage_now)}"
    printf '%s' "${record}" | _usage_jq '
        .model as $model
        | ([ (.windows // {}) | to_entries[] | select(.value | type == "object")
             | { scope: scope_of(.key; $model), utilization: .value.usedPct, resetsAt: .value.resetsAtIso, resetsEpoch: .value.resetsAt } ]) as $limits
        | (if .status == "rejected" and .resetsAt != null and .resetsAt > $now
           then { scope: scope_of(.rateLimitType // "unknown"; $model), resetsAt: .resetsAtIso } else null end) as $rejected
        | ([ $limits | account[] | unexpired ] | length) as $live
        | if $live == 0 and $rejected == null then
            verdict("none"; "unavailable"; []; null; ($now | todate); ["the last rate-limit event (Fire \(.fireId)) has reset its windows; firing optimistically"])
          else
            verdict("stream-events"; "stale"; ($limits | map(unexpired)); $rejected; .observedAt; ["seeded from the rate-limit event of Fire \(.fireId)"])
          end'
}

# usage_outcome_verdict <outcome-json> [<now-epoch>]
# The last Fire's EXHAUSTED outcome (limit strings) as a verdict: no number,
# a reset to hold until, a model switch when the string named the primary.
usage_outcome_verdict() {
    local outcome="${1:?outcome required}" _USAGE_NOW="${2:-$(_usage_now)}"
    printf '%s' "${outcome}" | _usage_jq '
        (.resetAt // "" | if . == "" then null else (sub("\\.[0-9]+Z$"; "Z") | try fromdate catch null) end) as $reset_epoch
        | (.limitType // "" | ascii_downcase) as $t
        | (if $t == "session" or $t == "weekly" then $t elif $t == "" then "session" else ($t | family) end) as $scope
        | if .status != "EXHAUSTED" or $reset_epoch == null or $reset_epoch <= $now then
            verdict("none"; "unavailable"; []; null; ($now | todate); ["no un-expired limit verdict; firing optimistically"])
          else
            verdict("limit-strings"; "stale"; []; { scope: $scope, resetsAt: ($reset_epoch | todate) }; (.observedAt // ($now | todate)); ["seeded from the limit string of the last Fire (\($scope))"])
          end'
}

# _usage_state_read <state-dir> -> the sensor state JSON ({} when absent)
_usage_state_read() {
    local f="$1/${USAGE_SENSOR_STATE_FILE}"
    if [ -f "${f}" ] && jq -e 'type == "object"' "${f}" >/dev/null 2>&1; then jq -c . "${f}"; else echo '{}'; fi
}

# _usage_state_write <state-dir> <json>
_usage_state_write() {
    mkdir -p "$1" || return 1
    printf '%s\n' "$2" | jq . > "$1/${USAGE_SENSOR_STATE_FILE}.tmp" && mv "$1/${USAGE_SENSOR_STATE_FILE}.tmp" "$1/${USAGE_SENSOR_STATE_FILE}"
}

# _usage_auth_status -> prints `claude auth status --json`; returns its exit code.
_usage_auth_status() {
    "${CLAUDE_BIN:-claude}" auth status --json 2>/dev/null
}

# _usage_mode_check <auth-status-json> <auth-rc> -> "" when consistent, else the reason.
# The declared mode must agree with the secrets present and with what
# `claude auth status` reports; inference from a secret alone is only ever
# the cross-check, never the source (ADR 0008).
_usage_mode_check() {
    local status="$1" rc="$2" mode="${CLAUDE_AUTH_MODE:-}"
    local method key_source
    case " ${USAGE_SENSOR_MODES} " in
        *" ${mode} "*) ;;
        *) printf 'CLAUDE_AUTH_MODE must be one of: %s (got "%s")' "${USAGE_SENSOR_MODES// /, }" "${mode}"; return 0 ;;
    esac
    case "${mode}" in
        login)
            [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] || { printf 'CLAUDE_AUTH_MODE=login but CLAUDE_CODE_OAUTH_TOKEN is set'; return 0; }
            [ -z "${ANTHROPIC_API_KEY:-}" ] || { printf 'CLAUDE_AUTH_MODE=login but ANTHROPIC_API_KEY is set'; return 0; } ;;
        setup-token)
            [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] || { printf 'CLAUDE_AUTH_MODE=setup-token but CLAUDE_CODE_OAUTH_TOKEN is not set'; return 0; }
            [ -z "${ANTHROPIC_API_KEY:-}" ] || { printf 'CLAUDE_AUTH_MODE=setup-token but ANTHROPIC_API_KEY is set'; return 0; } ;;
        api-key)
            [ -n "${ANTHROPIC_API_KEY:-}" ] || { printf 'CLAUDE_AUTH_MODE=api-key but ANTHROPIC_API_KEY is not set'; return 0; } ;;
    esac
    # A dead credential is not a mismatch; the caller parks on it.
    [ "${rc}" -eq 0 ] || return 0
    method="$(printf '%s' "${status}" | jq -r '.authMethod // ""' 2>/dev/null)"
    key_source="$(printf '%s' "${status}" | jq -r '.apiKeySource // ""' 2>/dev/null)"
    case "${mode}" in
        login)
            [ -z "${key_source}" ] || { printf 'CLAUDE_AUTH_MODE=login but claude auth status reports an API key (%s)' "${key_source}"; return 0; }
            [ "${method}" = "claude.ai" ] || { printf 'CLAUDE_AUTH_MODE=login but claude auth status reports authMethod "%s"' "${method}"; return 0; } ;;
        setup-token)
            [ "${method}" = "oauth_token" ] || { printf 'CLAUDE_AUTH_MODE=setup-token but claude auth status reports authMethod "%s"' "${method}"; return 0; } ;;
        api-key)
            [ -n "${key_source}" ] || { printf 'CLAUDE_AUTH_MODE=api-key but claude auth status reports no API key'; return 0; } ;;
    esac
    printf ''
}

# _usage_fetch <body-file> -> prints the HTTP status ("000" on a transport failure)
_usage_fetch() {
    local out="$1"
    local url="${USAGE_API_URL:-https://api.anthropic.com/api/oauth/usage}"
    local creds="${USAGE_CREDS_FILE:-${CLAUDE_CONFIG_DIR:-${HOME}/.claude}/.credentials.json}"
    local token code
    [ -f "${creds}" ] || { echo "nocreds"; return 0; }
    token="$(jq -r '.claudeAiOauth.accessToken // empty' "${creds}" 2>/dev/null)"
    [ -n "${token}" ] || { echo "nocreds"; return 0; }
    code="$("${CURL_BIN:-curl}" -s -m 15 -o "${out}" -w '%{http_code}' "${url}" \
        -H "Authorization: Bearer ${token}" \
        -H "anthropic-beta: oauth-2025-04-20" 2>/dev/null)" || code="000"
    printf '%s' "${code:-000}"
}

# _usage_last_outcome <state-dir> -> { outcome, endedAt } of the newest Fire
# record that carries an outcome (a preflight failure or a noop carries none),
# by endedAt, or "". The seed compares endedAt with the tapped event's
# observedAt to pick the newer of the two.
_usage_last_outcome() {
    local dir="$1/${FIRE_RECORD_DIRNAME}"
    [ -d "${dir}" ] || return 0
    find "${dir}" -maxdepth 1 -name '*.json' -type f -print0 2>/dev/null \
    | xargs -0 -r cat 2>/dev/null \
    | jq -s -c '[ .[] | select(type == "object" and .outcome != null and .endedAt != null) ]
                | sort_by(.endedAt) | last | if . == null then empty else { outcome: .outcome, endedAt: .endedAt } end' 2>/dev/null || true
}

# _usage_outcome_warnings <state-dir> -> the last outcome's warnings (the
# login-expiry notice claude printed during the last Fire), one per line.
_usage_outcome_warnings() {
    _usage_last_outcome "$1" | jq -r '.outcome.warnings[]? // empty' 2>/dev/null
}

# _usage_seed_verdict <state-dir> <warning...> : the setup-token path, also
# the login path once the endpoint is unavailable. Prefers the tapped event;
# the last Fire's limit-string outcome wins only when it is newer.
_usage_seed_verdict() {
    local state="$1"; shift
    local now; now="$(_usage_now)"
    local event="" event_at="" outcome="" outcome_at="" verdict
    if [ -f "${state}/${RATE_LIMITS_JSON:-rate-limits.json}" ]; then
        event="$(jq -c . "${state}/${RATE_LIMITS_JSON:-rate-limits.json}" 2>/dev/null || true)"
        event_at="$(printf '%s' "${event}" | jq -r '.observedAt // ""' 2>/dev/null)"
    fi
    outcome="$(_usage_last_outcome "${state}")"
    [ -z "${outcome}" ] || outcome_at="$(printf '%s' "${outcome}" | jq -r '.endedAt // ""')"
    if [ -n "${outcome}" ] && [ "$(printf '%s' "${outcome}" | jq -r '.outcome.status')" = "EXHAUSTED" ] \
       && { [ -z "${event}" ] || [ "${outcome_at}" \> "${event_at}" ]; }; then
        verdict="$(usage_outcome_verdict "$(printf '%s' "${outcome}" | jq -c .outcome)" "${now}")"
        # An outcome whose reset has passed says nothing; fall back to the event.
        if [ "$(printf '%s' "${verdict}" | jq -r .sensor)" != "none" ] || [ -z "${event}" ]; then
            printf '%s' "${verdict}" | _usage_append_warnings "$@"; return 0
        fi
    fi
    if [ -n "${event}" ]; then
        usage_event_verdict "${event}" "${now}" | _usage_append_warnings "$@"; return 0
    fi
    _usage_plain none unavailable true "no sensor has spoken yet; firing optimistically" "$@"
}

# _usage_append_warnings <warning...> : stdin verdict + warnings on stdout
_usage_append_warnings() {
    local warnings; warnings="$(printf '%s\n' "$@" | jq -R -s -c 'split("\n") | map(select(length > 0))')"
    jq -c --argjson w "${warnings}" '.warnings = ($w + .warnings)'
}

# _usage_with_model_hold <state-dir> <verdict-json>
# The model policy's switch outlives the Fire that observed it: a Fire on the
# fallback model never refreshes the primary's window (its per-model window is
# keyed to the model that fired), so the switch is remembered in the sensor
# state until the reset it named, and re-applied to every verdict until then.
_usage_with_model_hold() {
    local state="$1" verdict="$2" sensor_state hold_until hold_model now
    now="$(_usage_now)"
    sensor_state="$(_usage_state_read "${state}")"
    if [ "$(printf '%s' "${verdict}" | jq -r '.fireModel // ""')" != "" ] \
       && [ "$(printf '%s' "${verdict}" | jq -r '.fireModelUntil // ""')" != "" ]; then
        _usage_state_write "${state}" "$(printf '%s' "${sensor_state}" | jq -c --argjson v "${verdict}" '
            .modelHold = { model: $v.fireModel, until: $v.fireModelUntil }')"
        printf '%s\n' "${verdict}"
        return 0
    fi
    hold_until="$(printf '%s' "${sensor_state}" | jq -r '.modelHold.until // ""')"
    hold_model="$(printf '%s' "${sensor_state}" | jq -r '.modelHold.model // ""')"
    if [ -n "${hold_until}" ] && [ -n "${hold_model}" ] \
       && [ "$(date -u -d "${hold_until}" +%s 2>/dev/null || echo 0)" -gt "${now}" ] \
       && [ "$(printf '%s' "${verdict}" | jq -r '.fireModel // ""')" = "" ]; then
        printf '%s' "${verdict}" | jq -c --arg m "${hold_model}" --arg u "${hold_until}" --arg p "${AUTO_AGENT_MODEL_PRIMARY:-fable}" '
            .fireModel = $m | .fireModelUntil = $u
            | .warnings += ["model policy: \($p) limit spent until \($u), Fires run on \($m)"]'
        return 0
    fi
    printf '%s\n' "${verdict}"
}

# usage_verdict
usage_verdict() {
    local state; state="$(host_env_state_dir)"
    local verdict rc
    verdict="$(_usage_verdict_inner "${state}")"; rc=$?
    # The last Fire's login-expiry notice (claude's 3-day warning on stderr)
    # rides on every verdict until a Fire runs without it.
    local expiry; expiry="$(_usage_outcome_warnings "${state}")"
    if [ -n "${expiry}" ]; then
        verdict="$(printf '%s' "${verdict}" | _usage_append_warnings "${expiry}")"
    fi
    if [ "${rc}" -eq 0 ]; then
        _usage_with_model_hold "${state}" "${verdict}"
    else
        printf '%s\n' "${verdict}"
    fi
    return "${rc}"
}

# _usage_verdict_inner <state-dir> : the per-mode decision; prints the verdict.
_usage_verdict_inner() {
    local state="$1"
    local now; now="$(_usage_now)"
    local mode="${CLAUDE_AUTH_MODE:-}" status auth_rc reason
    local verdict

    if [ -z "${mode}" ]; then
        _usage_plain none unavailable false "CLAUDE_AUTH_MODE is not declared in the Host env (login | setup-token | api-key)"
        return 5
    fi
    status="$(_usage_auth_status)"; auth_rc=$?
    reason="$(_usage_mode_check "${status}" "${auth_rc}")"
    if [ -n "${reason}" ]; then
        _usage_err "mode mismatch: ${reason}"
        _usage_plain none unavailable false "mode mismatch: ${reason}"
        return 5
    fi
    if [ "${mode}" = "api-key" ]; then
        _usage_plain spend unavailable false "api-key mode refuses to start until spend pacing is implemented (ADR 0008)"
        return 6
    fi
    if [ "${auth_rc}" -ne 0 ] || [ "$(printf '%s' "${status}" | jq -r '.loggedIn // false' 2>/dev/null)" != "true" ]; then
        _usage_plain none auth-dead false "claude auth status: not logged in (exit ${auth_rc}); the credential is dead"
        return 4
    fi

    if [ "${mode}" = "setup-token" ]; then
        _usage_seed_verdict "${state}"
        return 0
    fi

    # login: the endpoint, with its stale hold and 403 mark.
    local sensor_state forbidden_daemon forbidden_at daemon="${AUTO_AGENT_DAEMON_ID:-}"
    sensor_state="$(_usage_state_read "${state}")"
    forbidden_daemon="$(printf '%s' "${sensor_state}" | jq -r '.endpoint.forbiddenDaemon // ""')"
    forbidden_at="$(printf '%s' "${sensor_state}" | jq -r '.endpoint.forbiddenAt // ""')"
    # The mark is per Daemon process: without an id to key it to, a 403 is
    # honoured for this call only, so a by-hand run never poisons the next.
    if [ -n "${forbidden_at}" ] && [ -n "${daemon}" ] && [ "${forbidden_daemon}" = "${daemon}" ]; then
        _usage_seed_verdict "${state}" "usage endpoint refused this Daemon with 403 at ${forbidden_at}; running as a setup-token Host"
        return 0
    fi

    local body code
    body="$(mktemp)"
    code="$(_usage_fetch "${body}")"
    case "${code}" in
        200)
            verdict="$(usage_endpoint_verdict "$(cat "${body}")" "${now}")"
            rm -f "${body}"
            if [ "$(printf '%s' "${verdict}" | jq -r .state)" = "ok" ]; then
                _usage_state_write "${state}" "$(printf '%s' "${sensor_state}" | jq -c --argjson v "${verdict}" --arg at "$(_usage_iso "${now}")" '
                    .endpoint = ((.endpoint // {}) + { lastVerdict: $v, lastGoodAt: $at })')"
            fi
            printf '%s\n' "${verdict}"
            return 0 ;;
        401)
            rm -f "${body}"
            _usage_plain none auth-dead false "usage endpoint answered 401; the login credential is dead"
            return 4 ;;
        403)
            rm -f "${body}"
            _usage_state_write "${state}" "$(printf '%s' "${sensor_state}" | jq -c --arg at "$(_usage_iso "${now}")" --arg d "${daemon}" '
                .endpoint = ((.endpoint // {}) + { forbiddenAt: $at, forbiddenDaemon: $d })')"
            _usage_seed_verdict "${state}" "usage endpoint answered 403 (the credential lacks user:profile); unavailable for this Daemon, running as a setup-token Host"
            return 0 ;;
        nocreds)
            rm -f "${body}"
            _usage_plain none auth-dead false "no login credential in ${USAGE_CREDS_FILE:-${CLAUDE_CONFIG_DIR:-${HOME}/.claude}/.credentials.json}"
            return 4 ;;
        *)
            rm -f "${body}"
            local last_at last_epoch hold="${AUTO_AGENT_GATE_STALE_MAX_SECS:-3600}"
            last_at="$(printf '%s' "${sensor_state}" | jq -r '.endpoint.lastGoodAt // ""')"
            last_epoch="$(date -u -d "${last_at}" +%s 2>/dev/null || echo 0)"
            if [ -n "${last_at}" ] && [ $((now - last_epoch)) -le "${hold}" ]; then
                printf '%s' "${sensor_state}" | jq -c --arg code "${code}" --arg at "${last_at}" '
                    .endpoint.lastVerdict | .state = "stale"
                    | .warnings += ["usage endpoint answered \($code); holding the verdict observed at \($at)"]'
                return 0
            fi
            _usage_seed_verdict "${state}" "usage endpoint answered ${code} and the last good verdict is older than ${hold}s; running as a setup-token Host"
            return 0 ;;
    esac
}

_usage_main() {
    case "${1:-}" in
        ''|verdict) ;;
        -h|--help|help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; return 0 ;;
        *) echo "usage: usage-sensor.sh [verdict]" >&2; return 2 ;;
    esac
    host_env_load
    usage_verdict
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    set -uo pipefail
    _usage_main "$@"
    exit $?
fi
