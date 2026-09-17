#!/usr/bin/env bash
# Tests for lib/usage-sensor.sh
#
# Run: bash lib/usage-sensor.test.sh
#
# Strategy: drive `bin/auto-agent usage-sensor` with a CLAUDE_BIN stub whose
# `auth status` answer is a file and a CURL_BIN stub that writes a canned body
# and prints a canned HTTP code, against an isolated State dir with the
# clock pinned. Assert the Gate verdict JSON, the exit code, what landed in
# usage-sensor.json and whether the endpoint was called (issue #30 AC 1-3, 5;
# behaviours 1 and 3).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# 2026-09-15T00:00:00Z
NOW=1789430400
LOGIN_STATUS='{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}'
TOKEN_STATUS='{"loggedIn":true,"authMethod":"oauth_token"}'
APIKEY_STATUS='{"loggedIn":true,"authMethod":"claude.ai","apiKeySource":"ANTHROPIC_API_KEY"}'
DEAD_STATUS='{"loggedIn":false,"authMethod":"none"}'
# The endpoint's live shape (the 2026-07-10 numbers: 19% session, 18% weekly, Fable at 97%).
ENDPOINT='{"five_hour":{"utilization":19,"resets_at":"2026-09-15T05:10:00+00:00"},"seven_day":{"utilization":18,"resets_at":"2026-09-18T19:00:00+00:00"},"seven_day_opus":{"utilization":40,"resets_at":"2026-09-18T19:00:00+00:00"},"limits":[{"kind":"session","percent":19,"is_active":true,"resets_at":"2026-09-15T05:10:00+00:00"},{"kind":"weekly_model","percent":97,"is_active":true,"resets_at":"2026-09-18T19:00:00+00:00","scope":{"model":{"display_name":"Fable"}}}]}'
# The tap's record of an allowed event from a Fable Fire, and a rejected one.
EVENT_ALLOWED='{"observedAt":"2026-09-14T23:00:00Z","fireId":"f1","sessionId":"s1","model":"claude-fable-5-1","status":"allowed","rateLimitType":"five_hour","resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z","utilization":null,"isUsingOverage":false,"overageStatus":"rejected","source":"windows","windows":{"five_hour":{"usedPct":24,"resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z"},"seven_day":{"usedPct":6,"resetsAt":1789758000,"resetsAtIso":"2026-09-18T19:00:00Z"},"seven_day_overage_included":{"usedPct":11,"resetsAt":1789758000,"resetsAtIso":"2026-09-18T19:00:00Z"}}}'
EVENT_REJECTED='{"observedAt":"2026-09-14T23:30:00Z","fireId":"f2","sessionId":"s2","model":"claude-fable-5-1","status":"rejected","rateLimitType":"five_hour","resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z","utilization":1,"isUsingOverage":false,"overageStatus":"rejected","source":"binding","windows":{"five_hour":{"usedPct":100,"resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z"}}}'

make_env() {
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/state" "${dir}/home"
    cat > "${dir}/claude-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/claude.log"
cat "${dir}/auth.out"; exit \$(cat "${dir}/auth.code")
STUB
    cat > "${dir}/curl-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/curl.log"
out=""
while [ \$# -gt 0 ]; do case "\$1" in -o) out="\$2"; shift ;; esac; shift; done
[ -f "${dir}/curl.fail" ] && exit 7
[ -n "\$out" ] && cp "${dir}/body.out" "\$out"
cat "${dir}/http.code"
STUB
    chmod +x "${dir}/claude-stub" "${dir}/curl-stub"
    printf '%s' "${LOGIN_STATUS}" > "${dir}/auth.out"; echo 0 > "${dir}/auth.code"
    printf '%s' "${ENDPOINT}" > "${dir}/body.out"; printf '200' > "${dir}/http.code"
    echo '{"claudeAiOauth":{"accessToken":"tok-1"}}' > "${dir}/creds.json"
    echo "${dir}"
}

# run_sensor <dir> <mode> [VAR=value ...] : the CLI with the stubs, the clock pinned
# and the secrets scrubbed, then the given env applied on top.
run_sensor() {
    local dir="$1" mode="$2"; shift 2
    env -u CLAUDE_CODE_OAUTH_TOKEN -u ANTHROPIC_API_KEY -u AUTO_AGENT_DAEMON_ID -u AUTO_AGENT_FIRE_MODEL \
        HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/missing.env" AUTO_AGENT_STATE_DIR="${dir}/state" \
        CLAUDE_BIN="${dir}/claude-stub" CURL_BIN="${dir}/curl-stub" USAGE_CREDS_FILE="${dir}/creds.json" \
        USAGE_SENSOR_NOW="${NOW}" CLAUDE_AUTH_MODE="${mode}" "$@" \
        bash "${CLI}" usage-sensor
}

check() { # <name> <json> <jq filter> <want>
    local have; have="$(printf '%s' "$2" | jq -c "$3" 2>/dev/null)"
    if [ "${have}" = "$4" ]; then pass "$1"; else fail "$1" "got ${have}, want $4"; fi
}

shape_ok() { # <name> <json> : AC 1, the verdict's fixed keys and a boolean shouldFire
    if printf '%s' "$2" | jq -e '
        (keys | index("authMode") and index("sensor") and index("state") and index("remainPct") and index("resetAt")
               and index("shouldFire") and index("observedAt") and index("limits") and index("warnings"))
        and (.shouldFire | type) == "boolean" and (.limits | type) == "array" and (.warnings | type) == "array"
        and (.sensor | IN("usage-endpoint","stream-events","limit-strings","spend","none"))
        and (.state | IN("ok","stale","unavailable","auth-dead"))
        and (.observedAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T"))' >/dev/null 2>&1; then pass "$1: verdict shape (AC 1)"
    else fail "$1: verdict shape (AC 1)" "$2"; fi
}

test_login_endpoint_ok() {
    echo "TEST: login: the usage endpoint is the pre-Fire sensor (AC 1, story 18)"
    local dir; dir="$(make_env)"
    local out rc; out="$(run_sensor "${dir}" login)"; rc=$?
    shape_ok "endpoint ok" "${out}"
    if [ "${rc}" -eq 0 ]; then pass "rc 0"; else fail "rc 0" "rc=${rc} ${out}"; fi
    check "usage-endpoint, ok, remainPct 81 (100 minus the binding session), session reset, fires" "${out}" \
        '[.authMode, .sensor, .state, .remainPct, .resetAt, .shouldFire, .observedAt]' \
        '["login","usage-endpoint","ok",81,"2026-09-15T05:10:00+00:00",true,"2026-09-15T00:00:00Z"]'
    check "limits[] carries session, weekly and the per-model scopes without endpoint field names" "${out}" \
        '.limits | map([.scope, .utilization])' '[["session",19],["weekly",18],["opus",40],["fable",97]]'
    check "the per-model Fable limit does not gate, it switches the model (behaviour 3)" "${out}" \
        '[.shouldFire, .fireModel, .fireModelUntil]' '[true,"opus","2026-09-18T19:00:00+00:00"]'
    if grep -q 'Authorization: Bearer tok-1' "${dir}/curl.log" && grep -q 'api/oauth/usage' "${dir}/curl.log"; then pass "the endpoint was read with the login token"
    else fail "the endpoint was read with the login token" "$(cat "${dir}/curl.log")"; fi
    check "usage-sensor.json remembers the good verdict and the model hold" "$(cat "${dir}/state/usage-sensor.json")" \
        '[.endpoint.lastGoodAt, .endpoint.lastVerdict.remainPct, .modelHold]' '["2026-09-15T00:00:00Z",81,{"model":"opus","until":"2026-09-18T19:00:00+00:00"}]'
    # Below the threshold: the verdict says wait, resetAt is the binding limit's.
    printf '%s' "${ENDPOINT}" | jq -c '.five_hour.utilization = 80 | .limits[0].percent = 80' > "${dir}/body.out"
    out="$(run_sensor "${dir}" login)"
    check "80% session used: remainPct 20, shouldFire false" "${out}" '[.remainPct, .shouldFire, .resetAt]' '[20,false,"2026-09-15T05:10:00+00:00"]'
    out="$(run_sensor "${dir}" login AUTO_AGENT_GATE_MIN_PCT=10)"
    check "AUTO_AGENT_GATE_MIN_PCT moves the threshold" "${out}" '.shouldFire' 'true'
    rm -rf "${dir}"
}

test_login_stale_hold_and_fallthrough() {
    echo "TEST: login: 429/5xx keeps the last verdict for 60 minutes, then the Host behaves like setup-token (story 18)"
    local dir; dir="$(make_env)"
    run_sensor "${dir}" login >/dev/null
    printf '429' > "${dir}/http.code"
    local out; out="$(run_sensor "${dir}" login USAGE_SENSOR_NOW=$((NOW + 1800)))"
    check "30 min later, 429: the held verdict, state stale, numbers kept" "${out}" \
        '[.sensor, .state, .remainPct, .shouldFire, .observedAt, (.warnings | any(test("429")))]' \
        '["usage-endpoint","stale",81,true,"2026-09-15T00:00:00Z",true]'
    out="$(run_sensor "${dir}" login USAGE_SENSOR_NOW=$((NOW + 3601)))"
    check "61 min later, still 429, no event: optimistic like a setup-token Host" "${out}" \
        '[.sensor, .state, .remainPct, .shouldFire, (.warnings | any(test("older than 3600s")))]' '["none","unavailable",null,true,true]'
    printf '%s' "${EVENT_ALLOWED}" > "${dir}/state/rate-limits.json"
    out="$(run_sensor "${dir}" login USAGE_SENSOR_NOW=$((NOW + 3601)))"
    check "61 min later with a tapped event: the event seeds the verdict" "${out}" '[.sensor, .state, .remainPct]' '["stream-events","stale",76]'
    touch "${dir}/curl.fail"
    out="$(run_sensor "${dir}" login USAGE_SENSOR_NOW=$((NOW + 600)))"
    check "a transport failure is held like a 5xx" "${out}" '[.state, .remainPct]' '["stale",81]'
    rm -rf "${dir}"
}

test_login_403_marks_the_endpoint_unavailable() {
    echo "TEST: login: 403 marks the endpoint unavailable for the Daemon process (AC 2)"
    local dir; dir="$(make_env)"
    printf '403' > "${dir}/http.code"
    local out; out="$(run_sensor "${dir}" login AUTO_AGENT_DAEMON_ID=d1)"
    check "403: setup-token behaviour with a warning" "${out}" '[.sensor, .state, .shouldFire, (.warnings | any(test("403")))]' '["none","unavailable",true,true]'
    check "the mark is written with the Daemon id" "$(cat "${dir}/state/usage-sensor.json")" '[.endpoint.forbiddenAt, .endpoint.forbiddenDaemon]' '["2026-09-15T00:00:00Z","d1"]'
    printf '200' > "${dir}/http.code"
    out="$(run_sensor "${dir}" login AUTO_AGENT_DAEMON_ID=d1)"
    if [ "$(wc -l < "${dir}/curl.log")" -eq 1 ] && [ "$(printf '%s' "${out}" | jq -r .sensor)" = "none" ]; then pass "the same Daemon never asks the endpoint again"
    else fail "the same Daemon never asks the endpoint again" "curl calls=$(wc -l < "${dir}/curl.log") $(printf '%s' "${out}" | jq -c .)"; fi
    out="$(run_sensor "${dir}" login AUTO_AGENT_DAEMON_ID=d2)"
    if [ "$(wc -l < "${dir}/curl.log")" -eq 2 ] && [ "$(printf '%s' "${out}" | jq -r .sensor)" = "usage-endpoint" ]; then pass "a new Daemon process tries the endpoint again"
    else fail "a new Daemon process tries the endpoint again" "curl calls=$(wc -l < "${dir}/curl.log") $(printf '%s' "${out}" | jq -c .)"; fi
    rm -rf "${dir}"
}

test_auth_dead_parks() {
    echo "TEST: 401, auth status exit 1 and a missing credential are auth-dead, rc 4 (AC 2, story 21)"
    local dir; dir="$(make_env)"
    local out rc
    printf '401' > "${dir}/http.code"
    out="$(run_sensor "${dir}" login)"; rc=$?
    shape_ok "401" "${out}"
    if [ "${rc}" -eq 4 ]; then pass "401: rc 4"; else fail "401: rc 4" "rc=${rc}"; fi
    check "401: state auth-dead, never fires, warning names it" "${out}" '[.sensor, .state, .shouldFire, .remainPct, (.warnings | any(test("401")))]' '["none","auth-dead",false,null,true]'
    printf '%s' "${DEAD_STATUS}" > "${dir}/auth.out"; echo 1 > "${dir}/auth.code"; printf '200' > "${dir}/http.code"
    rm -f "${dir}/curl.log"
    out="$(run_sensor "${dir}" login)"; rc=$?
    if [ "${rc}" -eq 4 ] && [ "$(printf '%s' "${out}" | jq -r .state)" = "auth-dead" ] && [ ! -s "${dir}/curl.log" ]; then pass "auth status exit 1: rc 4 before the endpoint is asked"
    else fail "auth status exit 1: rc 4 before the endpoint is asked" "rc=${rc} $(printf '%s' "${out}" | jq -c .)"; fi
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t)"; rc=$?
    if [ "${rc}" -eq 4 ] && [ "$(printf '%s' "${out}" | jq -r .state)" = "auth-dead" ]; then pass "setup-token with a dead token: rc 4"
    else fail "setup-token with a dead token: rc 4" "rc=${rc} $(printf '%s' "${out}" | jq -c .)"; fi
    printf '%s' "${LOGIN_STATUS}" > "${dir}/auth.out"; echo 0 > "${dir}/auth.code"
    rm -f "${dir}/creds.json"
    out="$(run_sensor "${dir}" login)"; rc=$?
    if [ "${rc}" -eq 4 ] && [ "$(printf '%s' "${out}" | jq -r .state)" = "auth-dead" ]; then pass "no credentials file: rc 4"
    else fail "no credentials file: rc 4" "rc=${rc} $(printf '%s' "${out}" | jq -c .)"; fi
    rm -rf "${dir}"
}

test_mode_mismatch_fails_loud() {
    echo "TEST: a mode mismatch fails loud, rc 5, never fires (AC 2)"
    local dir; dir="$(make_env)"
    local out rc err
    try() { # <name> <mode> <status-json> [env...]
        local name="$1" mode="$2" status="$3"; shift 3
        printf '%s' "${status}" > "${dir}/auth.out"
        err="$(run_sensor "${dir}" "${mode}" "$@" 2>&1 >"${dir}/out.json")"; rc=$?
        out="$(cat "${dir}/out.json")"
        if [ "${rc}" -eq 5 ] && [ "$(printf '%s' "${out}" | jq -c '[.shouldFire, .state]')" = '[false,"unavailable"]' ] \
           && printf '%s' "${out}" | jq -e '.warnings | any(test("mismatch|CLAUDE_AUTH_MODE"))' >/dev/null \
           && printf '%s' "${err}" | grep -q 'usage-sensor: mode mismatch\|CLAUDE_AUTH_MODE'; then pass "${name}"
        else fail "${name}" "rc=${rc} out=${out} err=${err}"; fi
    }
    try "login with CLAUDE_CODE_OAUTH_TOKEN set" login "${LOGIN_STATUS}" CLAUDE_CODE_OAUTH_TOKEN=t
    try "login with ANTHROPIC_API_KEY set" login "${LOGIN_STATUS}" ANTHROPIC_API_KEY=k
    try "login but auth status reports oauth_token" login "${TOKEN_STATUS}"
    try "login but auth status reports an API key" login "${APIKEY_STATUS}"
    try "setup-token without CLAUDE_CODE_OAUTH_TOKEN" setup-token "${TOKEN_STATUS}"
    try "setup-token but auth status reports claude.ai" setup-token "${LOGIN_STATUS}" CLAUDE_CODE_OAUTH_TOKEN=t
    try "api-key without ANTHROPIC_API_KEY" api-key "${APIKEY_STATUS}"
    try "an unknown mode" bogus "${LOGIN_STATUS}"
    printf '%s' "${LOGIN_STATUS}" > "${dir}/auth.out"
    out="$(run_sensor "${dir}" "" 2>/dev/null)"; rc=$?
    if [ "${rc}" -eq 5 ] && printf '%s' "${out}" | jq -e '.authMode == null and .shouldFire == false' >/dev/null; then pass "no CLAUDE_AUTH_MODE at all: rc 5, authMode null"
    else fail "no CLAUDE_AUTH_MODE at all: rc 5, authMode null" "rc=${rc} ${out}"; fi
    if [ ! -s "${dir}/curl.log" ]; then pass "a mismatch never reaches the endpoint"; else fail "a mismatch never reaches the endpoint"; fi
    rm -rf "${dir}"
}

test_api_key_refuses_to_start() {
    echo "TEST: api-key is schema-only: rc 6, sensor spend, never fires (AC 1, story 27)"
    local dir; dir="$(make_env)"
    printf '%s' "${APIKEY_STATUS}" > "${dir}/auth.out"
    local out rc; out="$(run_sensor "${dir}" api-key ANTHROPIC_API_KEY=k)"; rc=$?
    shape_ok "api-key" "${out}"
    if [ "${rc}" -eq 6 ]; then pass "rc 6"; else fail "rc 6" "rc=${rc}"; fi
    check "spend, unavailable, shouldFire false, the warning names spend pacing" "${out}" \
        '[.authMode, .sensor, .state, .shouldFire, .remainPct, (.warnings | any(test("spend pacing")))]' '["api-key","spend","unavailable",false,null,true]'
    rm -rf "${dir}"
}

test_setup_token_seeds_from_the_event() {
    echo "TEST: setup-token seeds from the last tapped rate-limit event (AC 3, story 19)"
    local dir; dir="$(make_env)"
    printf '%s' "${TOKEN_STATUS}" > "${dir}/auth.out"
    local out rc
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t)"; rc=$?
    shape_ok "first Fire" "${out}"
    check "no event yet: none, unavailable, remainPct null, fires optimistically" "${out}" \
        '[.authMode, .sensor, .state, .remainPct, .resetAt, .shouldFire]' '["setup-token","none","unavailable",null,null,true]'
    if [ ! -s "${dir}/curl.log" ]; then pass "the endpoint is never asked in setup-token mode"; else fail "the endpoint is never asked in setup-token mode"; fi
    printf '%s' "${EVENT_ALLOWED}" > "${dir}/state/rate-limits.json"
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t)"; rc=$?
    if [ "${rc}" -eq 0 ]; then pass "rc 0"; else fail "rc 0" "rc=${rc}"; fi
    check "stream-events, stale, observedAt from that Fire, remainPct 76 from the session window" "${out}" \
        '[.sensor, .state, .observedAt, .remainPct, .resetAt, .shouldFire]' '["stream-events","stale","2026-09-14T23:00:00Z",76,"2026-09-15T05:10:00Z",true]'
    check "the unnamed per-model window is keyed to the model that fired" "${out}" \
        '.limits | map([.scope, .utilization])' '[["session",24],["weekly",6],["fable",11]]'
    printf '%s' "${EVENT_REJECTED}" > "${dir}/state/rate-limits.json"
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t)"
    check "a rejected event sets resetAt and holds the Fire (AC 3)" "${out}" \
        '[.sensor, .state, .remainPct, .resetAt, .shouldFire, (.warnings | any(test("session limit rejected")))]' \
        '["stream-events","stale",0,"2026-09-15T05:10:00Z",false,true]'
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t USAGE_SENSOR_NOW=1789449001)"
    check "once the rejected window has reset: optimistic again" "${out}" '[.sensor, .shouldFire, .resetAt]' '["none",true,null]'
    rm -rf "${dir}"
}

test_setup_token_limit_string_outcome() {
    echo "TEST: setup-token: the last Fire's limit-string outcome seeds the verdict when newer than the event"
    local dir; dir="$(make_env)"
    printf '%s' "${TOKEN_STATUS}" > "${dir}/auth.out"
    mkdir -p "${dir}/state/fires"
    printf '%s' "${EVENT_ALLOWED}" > "${dir}/state/rate-limits.json"
    echo '{"fireId":"20260914T233000Z-1","endedAt":"2026-09-14T23:45:00Z","outcome":{"status":"EXHAUSTED","resetAt":"2026-09-15T02:50:00.000Z","source":"limit-strings","limitType":"session","observedAt":"2026-09-14T23:45:00Z"}}' > "${dir}/state/fires/20260914T233000Z-1.json"
    local out; out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t)"
    check "limit-strings, stale, held until the scraped reset" "${out}" \
        '[.sensor, .state, .remainPct, .resetAt, .shouldFire, .observedAt]' '["limit-strings","stale",null,"2026-09-15T02:50:00Z",false,"2026-09-14T23:45:00Z"]'
    echo '{"fireId":"20260914T220000Z-1","endedAt":"2026-09-14T22:00:00Z","outcome":{"status":"EXHAUSTED","resetAt":"2026-09-15T02:50:00.000Z","source":"limit-strings","limitType":"session","observedAt":"2026-09-14T22:00:00Z"}}' > "${dir}/state/fires/20260914T233000Z-1.json"
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t)"
    check "an outcome older than the event loses to the event" "${out}" '[.sensor, .shouldFire]' '["stream-events",true]'
    echo '{"fireId":"x","endedAt":"2026-09-14T23:50:00Z","outcome":{"status":"EXHAUSTED","resetAt":"2026-09-20T00:00:00.000Z","source":"limit-strings","limitType":"Fable","observedAt":"2026-09-14T23:50:00Z"}}' > "${dir}/state/fires/20260914T233000Z-1.json"
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t)"
    check "a per-model limit string switches the model and does not hold the Fire (behaviour 3)" "${out}" \
        '[.sensor, .shouldFire, .resetAt, .fireModel, .fireModelUntil]' '["limit-strings",true,null,"opus","2026-09-20T00:00:00Z"]'
    rm -rf "${dir}"
}

test_model_policy_hold() {
    echo "TEST: the model switch is remembered until its reset, whatever the next Fire's event shows (behaviour 3)"
    local dir; dir="$(make_env)"
    printf '%s' "${TOKEN_STATUS}" > "${dir}/auth.out"
    printf '%s' "${EVENT_REJECTED}" | jq -c '.rateLimitType = "seven_day_overage_included" | .resetsAt = 1789758000 | .resetsAtIso = "2026-09-18T19:00:00Z" | .windows = {"five_hour":{"usedPct":30,"resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z"},"seven_day_overage_included":{"usedPct":100,"resetsAt":1789758000,"resetsAtIso":"2026-09-18T19:00:00Z"}}' > "${dir}/state/rate-limits.json"
    local out; out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t)"
    check "a rejected per-model window: fires on the fallback, not held" "${out}" '[.shouldFire, .fireModel, .fireModelUntil, .remainPct]' '[true,"opus","2026-09-18T19:00:00Z",70]'
    # The next Fire ran on opus: its event carries no fable window at all.
    printf '%s' "${EVENT_ALLOWED}" | jq -c '.model = "claude-opus-5" | .observedAt = "2026-09-15T01:00:00Z" | del(.windows.seven_day_overage_included)' > "${dir}/state/rate-limits.json"
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t USAGE_SENSOR_NOW=$((NOW + 7200)))"
    check "the hold carries the switch across a Fire that could not see the primary's window" "${out}" '[.fireModel, .fireModelUntil, (.warnings | any(test("until 2026-09-18T19:00:00Z")))]' '["opus","2026-09-18T19:00:00Z",true]'
    out="$(run_sensor "${dir}" setup-token CLAUDE_CODE_OAUTH_TOKEN=t USAGE_SENSOR_NOW=1789758001)"
    check "after the reset the switch is gone" "${out}" '.fireModel' 'null'
    # Policy knobs.
    printf '%s' "${LOGIN_STATUS}" > "${dir}/auth.out"; rm -f "${dir}/state/usage-sensor.json"
    out="$(run_sensor "${dir}" login AUTO_AGENT_MODEL_FALLBACK=)"
    check "an empty AUTO_AGENT_MODEL_FALLBACK never switches" "${out}" '[.fireModel, .shouldFire]' '[null,true]'
    out="$(run_sensor "${dir}" login AUTO_AGENT_MODEL_PRIMARY=opus AUTO_AGENT_MODEL_FALLBACK=sonnet AUTO_AGENT_MODEL_SWITCH_PCT=30)"
    check "primary, fallback and switch percent come from the Host env" "${out}" '[.fireModel, .fireModelUntil]' '["sonnet","2026-09-18T19:00:00+00:00"]'
    rm -rf "${dir}"
}

test_no_ccusage_remains() {
    echo "TEST: no reference to ccusage remains in the harness (AC 5, story 22)"
    local hits
    hits="$(grep -ril 'ccusage' "${ROOT_DIR}/lib" "${ROOT_DIR}/bin" "${ROOT_DIR}/plugin" "${ROOT_DIR}/README.md" "${ROOT_DIR}/CONTEXT.md" 2>/dev/null | grep -v '/usage-sensor.test.sh$' || true)"
    if [ -z "${hits}" ]; then pass "lib, bin, plugin, README and CONTEXT are free of it"; else fail "lib, bin, plugin, README and CONTEXT are free of it" "${hits}"; fi
    if ! grep -q 'npx' "${ROOT_DIR}/lib/usage-sensor.sh"; then pass "the sensor shells to nothing but curl and claude"; else fail "the sensor shells to nothing but curl and claude"; fi
}

test_login_endpoint_ok
test_login_stale_hold_and_fallthrough
test_login_403_marks_the_endpoint_unavailable
test_auth_dead_parks
test_mode_mismatch_fails_loud
test_api_key_refuses_to_start
test_setup_token_seeds_from_the_event
test_setup_token_limit_string_outcome
test_model_policy_hold
test_no_ccusage_remains

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
