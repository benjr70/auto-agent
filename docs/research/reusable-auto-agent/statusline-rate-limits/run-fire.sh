#!/usr/bin/env bash
# PROTOTYPE (ticket #22): one "Fire" = a tiny `claude -p` run with
# `--output-format stream-json` piped through rate-limits-tap.sh, then a
# side-by-side of the tapped rate_limit_event against the /login usage
# endpoint (via the live usage-sensor lib) on the same account.
# Usage: SENSOR_SH=<path to usage-sensor.sh> bash run-fire.sh [label]
# Env: FIRE_MODEL (haiku), FIRE_PROMPT, AUTO_AGENT_STATE_DIR
set -u
here="$(cd "$(dirname "$0")" && pwd)"
label="${1:-fire}"
export AUTO_AGENT_STATE_DIR="${AUTO_AGENT_STATE_DIR:-${here}/state.PROTOTYPE-wipe-me}"
mkdir -p "${AUTO_AGENT_STATE_DIR}"
before="$(wc -l < "${AUTO_AGENT_STATE_DIR}/rate-limits.jsonl" 2>/dev/null || echo 0)"

t0="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
( cd "${AUTO_AGENT_STATE_DIR}" && claude -p "${FIRE_PROMPT:-Reply with the single word ok.}" \
    --model "${FIRE_MODEL:-haiku}" --output-format stream-json --verbose 2>"${AUTO_AGENT_STATE_DIR}/${label}.stderr" ) \
  | FIRE_ID="${label}" bash "${here}/rate-limits-tap.sh" > "${AUTO_AGENT_STATE_DIR}/${label}.stream.jsonl"
rc=$?
sleep 1  # let the tee'd jq drain
t1="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
after="$(wc -l < "${AUTO_AGENT_STATE_DIR}/rate-limits.jsonl" 2>/dev/null || echo 0)"

echo "[${label}] rc=${rc} start=${t0} end=${t1} events_tapped=$((after-before))"
jq -c 'select(.type=="result") | {subtype,is_error,total_cost_usd,num_turns}' "${AUTO_AGENT_STATE_DIR}/${label}.stream.jsonl"
echo "[${label}] last tapped event:"
jq -c . "${AUTO_AGENT_STATE_DIR}/rate-limits.json"

if [ -n "${SENSOR_SH:-}" ] && [ -f "${SENSOR_SH}" ]; then
  # shellcheck disable=SC1090
  . "${SENSOR_SH}"
  payload="$(usage_sensor_fetch)" && {
    echo "[${label}] endpoint (/login credential, same account) at $(date -u +%H:%M:%SZ):"
    printf '%s' "${payload}" | jq -c '{five_hour: {utilization: .five_hour.utilization, resets_at: .five_hour.resets_at},
      seven_day: {utilization: .seven_day.utilization, resets_at: .seven_day.resets_at},
      scoped: [ .limits[]? | select(.scope.model.display_name != null) | {model: .scope.model.display_name, percent, is_active, resets_at} ]}'
  } || echo "[${label}] endpoint fetch failed"
fi
