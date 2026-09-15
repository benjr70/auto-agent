#!/usr/bin/env bash
# Rate-limit tap: reads a Fire's `--output-format stream-json` stream on stdin,
# passes every line through unchanged on stdout, and records every
# `rate_limit_event` into the State dir (ADR 0008 addendum). The last event of
# a Fire is the seed for the next Gate verdict on a setup-token Host.
#
# Source this file, then:
#
#   rate_limits_tap <state-dir> <fire-id>
#       stdin -> stdout unchanged. Appends one record per event to
#       <state-dir>/rate-limits.jsonl and overwrites <state-dir>/rate-limits.json
#       with the last one. Writes nothing when the stream carries no event.
#
#   rate_limits_record <event-line> <fire-id> <model>
#       Prints the record for one event (see "Record shape"). Used by the tap;
#       public so the Gate can re-derive a record from a logged stream.
#
# Record shape:
#
#   {
#     "observedAt": "<ISO instant the tap saw the event>",
#     "fireId": "<fire-id>",
#     "sessionId": "<claude session id>",
#     "model": "<the Fire's model, from the stream's init event>" | null,
#     "status": "allowed" | "allowed_warning" | "rejected",
#     "rateLimitType": "<binding window name>" | null,
#     "resetsAt": <epoch seconds> | null,
#     "resetsAtIso": "<ISO>" | null,
#     "utilization": <0..1> | null,        (top-level field, when present)
#     "isUsingOverage": <bool>,
#     "overageStatus": "<string>" | null,
#     "source": "windows" | "binding",
#     "windows": { "<name>": { "usedPct": <0..100> | null, "resetsAt", "resetsAtIso" } }
#   }
#
# `source` says where `windows` came from: the event's `unifiedWindows` object
# (five_hour, seven_day and an unnamed per-model weekly when the Fire's model has
# one, hence `model` on the record), or, when that internal object is absent,
# the single binding window rebuilt from the stable top-level fields.

RATE_LIMITS_JSON="rate-limits.json"
RATE_LIMITS_JSONL="rate-limits.jsonl"

_rl_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# rate_limits_record <event-line> <fire-id> <model>
rate_limits_record() {
    local line="${1:?rate_limits_record: event line required}"
    local fire="${2:-unknown}" model="${3:-}"
    printf '%s\n' "${line}" | jq -c --arg fire "${fire}" --arg model "${model}" --arg now "$(_rl_now)" '
        .rate_limit_info as $i
        | def window: { usedPct: (if .utilization == null then null else ((.utilization * 100) | round) end),
                        resetsAt: (.resetsAt // null),
                        resetsAtIso: (if .resetsAt == null then null else (.resetsAt | todate) end) };
          ($i.unifiedWindows // {}) as $w
        | {
            observedAt: $now,
            fireId: $fire,
            sessionId: (.session_id // null),
            model: (if $model == "" then null else $model end),
            status: ($i.status // null),
            rateLimitType: ($i.rateLimitType // null),
            resetsAt: ($i.resetsAt // null),
            resetsAtIso: (if $i.resetsAt == null then null else ($i.resetsAt | todate) end),
            utilization: ($i.utilization // null),
            isUsingOverage: ($i.isUsingOverage // false),
            overageStatus: ($i.overageStatus // null),
            source: (if ($w | length) > 0 then "windows" else "binding" end),
            windows: (if ($w | length) > 0
                      then ($w | with_entries(.value |= window))
                      else { ($i.rateLimitType // "unknown"): ({ utilization: $i.utilization, resetsAt: $i.resetsAt } | window) }
                      end)
          }'
}

# rate_limits_tap <state-dir> <fire-id>
rate_limits_tap() {
    local state="${1:?rate_limits_tap: state dir required}"
    local fire="${2:?rate_limits_tap: fire id required}"
    local line model="" rec
    while IFS= read -r line || [ -n "${line}" ]; do
        printf '%s\n' "${line}"
        case "${line}" in
            *'"rate_limit_event"'*)
                rec="$(rate_limits_record "${line}" "${fire}" "${model}" 2>/dev/null)" || continue
                [ -n "${rec}" ] || continue
                mkdir -p "${state}"
                printf '%s\n' "${rec}" >> "${state}/${RATE_LIMITS_JSONL}"
                printf '%s\n' "${rec}" > "${state}/${RATE_LIMITS_JSON}.tmp" && mv "${state}/${RATE_LIMITS_JSON}.tmp" "${state}/${RATE_LIMITS_JSON}"
                ;;
            *'"subtype":"init"'*)
                model="$(printf '%s\n' "${line}" | jq -r 'select(.type == "system") | .model // empty' 2>/dev/null)" || model=""
                ;;
        esac
    done
    return 0
}
