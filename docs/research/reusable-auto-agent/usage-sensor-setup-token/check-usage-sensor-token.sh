#!/usr/bin/env bash
# check-usage-sensor-token.sh — does /api/oauth/usage accept a `claude
# setup-token` bearer? (auto-agent map #1, ticket #20)
#
# Run on any machine with curl + jq. Never prints a token.
#
#   claude setup-token            # on a browser machine; copy the token
#   CLAUDE_CODE_OAUTH_TOKEN=<token> bash check-usage-sensor-token.sh
#
# Optional comparison against the /login credential on this machine:
#   COMPARE_LOGIN=1 CLAUDE_CODE_OAUTH_TOKEN=<token> bash check-usage-sensor-token.sh
#
# Output: one block per token kind — HTTP status, the top-level keys of the
# body, and whether lib/usage-sensor.sh's usage_gate would compute a verdict
# from it. Ends with a single VERDICT= line.
set -u

URL="${USAGE_API_URL:-https://api.anthropic.com/api/oauth/usage}"
BETA="anthropic-beta: oauth-2025-04-20"
SENSOR="${SENSOR_SH:-}"   # path to Smart-Smoker-V2 scripts/claude-agent/lib/usage-sensor.sh (optional)

probe() {   # $1 label, token on stdin
    local label="$1" token status body keys gate
    token="$(cat)"
    [ -n "${token}" ] || { echo "[${label}] no token"; return 2; }
    body="$(curl -s -m 15 -o /dev/stderr -w '%{http_code}' "${URL}" \
             -H "Authorization: Bearer ${token}" -H "${BETA}" 2>"/tmp/usage-body.$$")"
    status="${body}"
    body="$(cat "/tmp/usage-body.$$")"; rm -f "/tmp/usage-body.$$"
    keys="$(printf '%s' "${body}" | jq -r 'if type=="object" then (keys|join(",")) else "<non-object>" end' 2>/dev/null || echo '<non-json>')"
    echo "[${label}] http=${status} keys=${keys}"
    if [ "${status}" != "200" ]; then
        # error shape, redacted to type/message
        printf '[%s] error=%s\n' "${label}" "$(printf '%s' "${body}" | jq -c '{type: .error.type?, message: .error.message?}' 2>/dev/null || echo '<non-json>')"
    fi
    if [ -n "${SENSOR}" ] && [ -f "${SENSOR}" ]; then
        # shellcheck disable=SC1090
        . "${SENSOR}"
        gate="$(printf '%s' "${body}" | usage_gate 2>/dev/null)"; rc=$?
        echo "[${label}] usage_gate rc=${rc} verdict=${gate}"
    fi
    [ "${status}" = "200" ]
}

setup_ok=1
printf '%s' "${CLAUDE_CODE_OAUTH_TOKEN:-}" | probe setup-token; setup_ok=$?

if [ "${COMPARE_LOGIN:-0}" = "1" ]; then
    creds="${USAGE_CREDS_FILE:-${CLAUDE_CONFIG_DIR:-${HOME}/.claude}/.credentials.json}"
    if [ -f "${creds}" ]; then
        jq -r '.claudeAiOauth.accessToken // empty' "${creds}" | probe login-token || true
        echo "[login-token] scopes=$(jq -c '.claudeAiOauth.scopes // []' "${creds}")"
    else
        echo "[login-token] no ${creds}"
    fi
fi

case "${setup_ok}" in
    0) echo "VERDICT=accepted   # sensor can read CLAUDE_CODE_OAUTH_TOKEN directly" ;;
    2) echo "VERDICT=no-token   # set CLAUDE_CODE_OAUTH_TOKEN" ;;
    *) echo "VERDICT=rejected   # sensor needs the documented-error fallback on setup-token Hosts" ;;
esac
