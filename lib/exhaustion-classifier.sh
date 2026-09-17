#!/usr/bin/env bash
# Exhaustion classifier: out of gas, dead credential, or a real failure.
# Carried over from Smart-Smoker-V2; it now prefers the tapped rate-limit
# event and knows credential death (ADR 0008 and addendum).
#
# Sourceable library exposing one pure function, `exhaustion_classify`. Given
# the `claude` exit code, the Fire's captured output on stdin and, optionally,
# the last rate-limit record the tap wrote for this Fire, it decides how the
# Fire ended and emits the outcome the Fire record carries:
#
#     { "status":    "OK" | "EXHAUSTED" | "AUTH_DEAD" | "FAILED",
#       "resetAt":   "<iso8601>" | "",
#       "source":    "stream-events" | "limit-strings" | null,
#       "limitType": "session" | "weekly" | "<model>" | "<window>" | null,
#       "observedAt": "<iso8601>" }
#
# The property that matters is the pause-vs-fail distinction: a Fire that ran
# out of budget is EXHAUSTED (the wrapper pauses the issue and keeps the
# branch), a dead credential is AUTH_DEAD (the wrapper pauses and the Daemon
# parks; never exhaustion, never a failure of the ticket), and anything else
# non-zero is FAILED (surfaced for triage). A zero exit always classifies OK:
# the Fire finished, so any limit-flavoured phrase in the transcript is prose.
#
# Order of evidence for a non-zero exit:
#   1. a `rejected` rate-limit record for this Fire (source "stream-events"):
#      its resetsAt and rateLimitType are authoritative and replace the regex;
#   2. `authentication_failed` / "Failed to authenticate" in the output
#      (the documented login-expiry result): AUTH_DEAD;
#   3. the documented limit strings ("You've hit your session/weekly/<model>
#      limit … resets …", the older "usage limit reached", 429s): EXHAUSTED,
#      source "limit-strings", limitType from the string, resetAt scraped
#      from an epoch, an ISO instant or a wall-clock "resets 10:50pm (Zone)";
#   4. FAILED.
#
# Pure: reads its arguments and stdin; touches the clock only to normalize a
# scraped timestamp (EC_NOW pins it for tests).
#
# Usage:  <captured output> | exhaustion_classify <exitCode> [<rate-limit-record-json>]

# Signatures Claude emits when a Fire is cut off by usage or rate limits. Kept
# specific so an unrelated failure that merely mentions "limit" is not read as
# exhaustion. "session limit" observed live 2026-07-08: "You've hit your
# session limit · resets 10:50pm (America/New_York)", exit 1, nothing else.
_EC_EXHAUSTION_RE="hit your [a-z0-9 .-]+ limit|usage limit reached|usage limit will reset|session limit|weekly limit|rate[ -]?limit(ed)?|429 too many requests|too many requests"
# The documented login-expiry signal: `Failed to authenticate: OAuth session
# expired and could not be refreshed`, code `authentication_failed`; a mid-Fire
# `OAuth token has expired` 401 lands here too.
_EC_AUTH_DEAD_RE="authentication_failed|failed to authenticate|oauth (session|token) (has )?expired|not logged in.*(run|use) /login|please run /login"

_ec_now() { printf '%s' "${EC_NOW:-$(date -u +%s)}"; }

# Normalize a scraped reset value (bare epoch or ISO-8601) to canonical ISO;
# prints empty on anything unparseable.
_ec_to_iso() {
    local v="$1"
    [ -z "${v}" ] && return 0
    if [[ "${v}" =~ ^[0-9]+$ ]]; then
        date -u -d "@${v}" +%Y-%m-%dT%H:%M:%S.000Z 2>/dev/null || true
    else
        date -u -d "${v}" +%Y-%m-%dT%H:%M:%S.000Z 2>/dev/null || true
    fi
}

# Resolve a wall-clock reset notice ("resets 10:50pm (America/New_York)") to
# the next future instant in the named zone, as ISO UTC. Empty when the line
# carries no am/pm time.
_ec_clock_to_iso() {
    local line="$1" tod tz now_epoch day cand_epoch hh mm mer
    tod="$(printf '%s' "${line}" \
        | grep -oiE '[0-9]{1,2}(:[0-9]{2})?[ ]?(am|pm)' | head -1 | tr 'A-Z' 'a-z' | tr -d ' ')"
    [ -z "${tod}" ] && return 0
    mer="${tod: -2}"
    tod="${tod%??}"
    hh="${tod%%:*}"
    if [[ "${tod}" == *:* ]]; then mm="${tod##*:}"; else mm="00"; fi
    hh=$((10#${hh})); mm=$((10#${mm}))
    if [ "${mer}" = "pm" ] && [ "${hh}" -ne 12 ]; then hh=$((hh + 12)); fi
    if [ "${mer}" = "am" ] && [ "${hh}" -eq 12 ]; then hh=0; fi
    tod="$(printf '%02d:%02d' "${hh}" "${mm}")"
    tz="$(printf '%s' "${line}" \
        | grep -oE '\([A-Za-z_]+(/[A-Za-z_+-]+)+\)' | head -1 | tr -d '()')"
    now_epoch="$(_ec_now)"
    day="$(TZ="${tz:-UTC}" date -d "@${now_epoch}" +%Y-%m-%d 2>/dev/null)" || return 0
    cand_epoch="$(TZ="${tz:-UTC}" date -d "${day} ${tod}" +%s 2>/dev/null)" || return 0
    [ -z "${cand_epoch}" ] && return 0
    if [ "${cand_epoch}" -le "${now_epoch}" ]; then
        cand_epoch=$((cand_epoch + 86400))
    fi
    date -u -d "@${cand_epoch}" +%Y-%m-%dT%H:%M:%S.000Z 2>/dev/null || true
}

# Scrape the reset instant from an exhaustion notice: Claude's pipe-delimited
# epoch ("…reached|<epoch>"), an ISO instant on a "reset" line, a bare epoch
# on such a line, then a wall-clock time on such a line.
_ec_scrape_reset() {
    local text="$1" candidate reset_line
    candidate="$(printf '%s' "${text}" | grep -oiE 'limit reached\|[0-9]+' | head -1 | sed 's/.*|//')"
    if [ -n "${candidate}" ]; then _ec_to_iso "${candidate}"; return 0; fi
    candidate="$(printf '%s' "${text}" | grep -iE 'reset' \
        | grep -oiE '[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}(:[0-9]{2})?([.][0-9]+)?Z?' | head -1)"
    if [ -n "${candidate}" ]; then _ec_to_iso "${candidate}"; return 0; fi
    candidate="$(printf '%s' "${text}" | grep -iE 'reset' | grep -oiE '[0-9]{10,}' | head -1)"
    if [ -n "${candidate}" ]; then _ec_to_iso "${candidate}"; return 0; fi
    reset_line="$(printf '%s' "${text}" | grep -iE 'resets?[[:space:]]' | head -1)"
    [ -n "${reset_line}" ] && _ec_clock_to_iso "${reset_line}"
}

# The limit the string names: "hit your session limit" -> session, "weekly
# limit" -> weekly, "hit your Fable limit" -> fable; empty when unnamed.
_ec_scrape_limit_type() {
    local text="$1" t
    t="$(printf '%s' "${text}" | grep -oiE 'hit your [a-z0-9 .-]+ limit' | head -1 \
        | sed -E 's/^[Hh]it your (.*) limit$/\1/' | tr 'A-Z' 'a-z')"
    if [ -z "${t}" ]; then
        if printf '%s' "${text}" | grep -qiE 'weekly limit'; then t="weekly"
        elif printf '%s' "${text}" | grep -qiE 'session limit'; then t="session"; fi
    fi
    printf '%s' "${t}"
}

# Usage:  <captured output> | exhaustion_classify <exitCode> [<rate-limit-record-json>]
exhaustion_classify() {
    local exit_code="${1:-0}" record="${2:-}" text status reset="" source=null limit=null
    text="$(cat)"

    if [ "${exit_code}" -eq 0 ]; then
        status="OK"
    elif [ -n "${record}" ] && printf '%s' "${record}" | jq -e '.status == "rejected"' >/dev/null 2>&1; then
        status="EXHAUSTED"; source='"stream-events"'
        reset="$(printf '%s' "${record}" | jq -r '.resetsAtIso // (if .resetsAt == null then "" else (.resetsAt | todate) end)')"
        limit="$(printf '%s' "${record}" | jq -c '.rateLimitType // null')"
    elif printf '%s' "${text}" | grep -qiE "${_EC_AUTH_DEAD_RE}"; then
        status="AUTH_DEAD"
    elif printf '%s' "${text}" | grep -qiE "${_EC_EXHAUSTION_RE}"; then
        status="EXHAUSTED"; source='"limit-strings"'
        reset="$(_ec_scrape_reset "${text}")"
        limit="$(_ec_scrape_limit_type "${text}")"
        if [ -z "${limit}" ]; then limit=null; else limit="$(printf '%s' "${limit}" | jq -R -c .)"; fi
    else
        status="FAILED"
    fi

    jq -n -c --arg status "${status}" --arg reset "${reset}" --argjson source "${source}" \
        --argjson limit "${limit}" --arg now "$(date -u -d "@$(_ec_now)" +%Y-%m-%dT%H:%M:%SZ)" \
        '{ status: $status, resetAt: $reset, source: $source, limitType: $limit, observedAt: $now }'
}
