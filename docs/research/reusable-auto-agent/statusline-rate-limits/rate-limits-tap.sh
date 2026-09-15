#!/usr/bin/env bash
# PROTOTYPE (ticket #22): read a Fire's `--output-format stream-json` stream on
# stdin, pass it through unchanged on stdout, and record every
# `rate_limit_event` into the State dir. The LAST event of the Fire becomes
# rate-limits.json, the seed for the next Gate verdict on a setup-token Host.
set -u
state="${AUTO_AGENT_STATE_DIR:-${HOME}/.local/state/auto-agent}"
mkdir -p "${state}"
fire="${FIRE_ID:-unknown}"
tee >(jq -c --arg fire "${fire}" '
    select(.type == "rate_limit_event")
    | .rate_limit_info as $i
    | { observedAt: (now | todate), fireId: $fire, sessionId: .session_id,
        status: $i.status, rateLimitType: ($i.rateLimitType // null),
        resetsAt: ($i.resetsAt // null),
        isUsingOverage: ($i.isUsingOverage // false),
        overageStatus: ($i.overageStatus // null),
        windows: ( ($i.unifiedWindows // {}) | with_entries(.value |= {
                     usedPct: ((.utilization // 0) * 100 | round),
                     resetsAt: .resetsAt,
                     resetsAtIso: (.resetsAt | todate) }) ) }' \
    | while IFS= read -r rec; do
        printf '%s\n' "${rec}" >> "${state}/rate-limits.jsonl"
        printf '%s\n' "${rec}" > "${state}/rate-limits.json"
      done)
