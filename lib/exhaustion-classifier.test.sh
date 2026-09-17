#!/usr/bin/env bash
# Tests for lib/exhaustion-classifier.sh
#
# Run: bash lib/exhaustion-classifier.test.sh
#
# Strategy: exhaustion_classify is a pure function of (exit code, captured
# output, the Fire's last rate-limit record). Each test feeds a fixture and
# asserts the outcome. The pause-vs-fail distinction and the new
# credential-death branch are the safety properties, so they come first.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=exhaustion-classifier.sh
. "${SCRIPT_DIR}/exhaustion-classifier.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# 2026-09-15T00:00:00Z, the clock every scrape is pinned to.
export EC_NOW=1789430400

REJECTED='{"observedAt":"2026-09-15T00:00:00Z","fireId":"f1","model":"claude-fable-5-1","status":"rejected","rateLimitType":"five_hour","resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z","utilization":1,"source":"windows","windows":{"five_hour":{"usedPct":100,"resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z"}}}'
ALLOWED='{"observedAt":"2026-09-15T00:00:00Z","fireId":"f1","model":null,"status":"allowed","rateLimitType":"five_hour","resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z","windows":{}}'

check() { # <name> <got-json> <want-json-subset via jq filter> <want>
    local name="$1" got="$2" filter="$3" want="$4" have
    have="$(printf '%s' "${got}" | jq -c "${filter}")"
    if [ "${have}" = "${want}" ]; then pass "${name}"; else fail "${name}" "got ${have}, want ${want}"; fi
}

test_exit_zero_is_ok() {
    echo "TEST: exit 0 is OK whatever the transcript says"
    local out
    out="$(printf 'reconcile: PASS\ndocker manifest inspect fail for rate-limit/network reason\n' | exhaustion_classify 0)"
    check "prose mentioning rate-limit on exit 0: OK, no source" "${out}" '[.status, .resetAt, .source, .limitType]' '["OK","",null,null]'
    out="$(printf 'done\n' | exhaustion_classify 0 "${REJECTED}")"
    check "a rejected record cannot override a completed Fire" "${out}" '.status' '"OK"'
    if printf '%s' "${out}" | jq -e '.observedAt | test("^[0-9]{4}-")' >/dev/null; then pass "observedAt is stamped"; else fail "observedAt is stamped" "${out}"; fi
}

test_rejected_record_wins() {
    echo "TEST: a rejected rate-limit record is the authoritative exhaustion signal (stream-events)"
    local out
    out="$(printf 'some unrelated stack trace\n' | exhaustion_classify 1 "${REJECTED}")"
    check "EXHAUSTED from the record with its reset and window" "${out}" '[.status, .resetAt, .source, .limitType]' '["EXHAUSTED","2026-09-15T05:10:00Z","stream-events","five_hour"]'
    out="$(printf "You've hit your session limit · resets 10:50pm (America/New_York)\n" | exhaustion_classify 1 "${REJECTED}")"
    check "the record beats the string's scraped reset" "${out}" '[.resetAt, .source]' '["2026-09-15T05:10:00Z","stream-events"]'
    out="$(printf 'boom\n' | exhaustion_classify 1 "${ALLOWED}")"
    check "an allowed record is not exhaustion: FAILED" "${out}" '.status' '"FAILED"'
    out="$(printf 'boom\n' | exhaustion_classify 1 'not json')"
    check "a malformed record is ignored" "${out}" '.status' '"FAILED"'
}

test_limit_strings_fallback() {
    echo "TEST: the documented limit strings stay as the text-mode fallback (limit-strings)"
    local out
    out="$(printf "You've hit your session limit · resets 10:50pm (America/New_York)\n" | exhaustion_classify 1)"
    # 10:50pm New York on 2026-09-14 (EDT, UTC-4) is 02:50Z on the 15th, still ahead of 00:00Z.
    check "session limit with a wall-clock reset" "${out}" '[.status, .resetAt, .source, .limitType]' '["EXHAUSTED","2026-09-15T02:50:00.000Z","limit-strings","session"]'
    out="$(printf "You've hit your weekly limit · resets Sep 18, 7pm (America/New_York)\n" | exhaustion_classify 1)"
    check "weekly limit names the weekly scope" "${out}" '[.status, .limitType]' '["EXHAUSTED","weekly"]'
    out="$(printf "You've hit your Fable limit · resets 2026-09-18T19:00:00Z\n" | exhaustion_classify 1)"
    check "a per-model limit string names the model and scrapes the ISO reset" "${out}" '[.status, .resetAt, .limitType]' '["EXHAUSTED","2026-09-18T19:00:00.000Z","fable"]'
    out="$(printf 'Claude AI usage limit reached|1789449000\n' | exhaustion_classify 1)"
    check "the pipe-delimited epoch form" "${out}" '[.status, .resetAt, .limitType]' '["EXHAUSTED","2026-09-15T05:10:00.000Z",null]'
    out="$(printf 'API Error: 429 too many requests\n' | exhaustion_classify 1)"
    check "429 without a reset: EXHAUSTED, empty resetAt" "${out}" '[.status, .resetAt]' '["EXHAUSTED",""]'
}

test_auth_dead() {
    echo "TEST: credential death is AUTH_DEAD, never exhaustion, never a ticket failure"
    local out
    out="$(printf 'Failed to authenticate: OAuth session expired and could not be refreshed\n' | exhaustion_classify 1)"
    check "the documented login-expiry result" "${out}" '[.status, .resetAt, .source]' '["AUTH_DEAD","",null]'
    out="$(printf '{"type":"system","subtype":"api_retry","error":"authentication_failed"}\n' | exhaustion_classify 1)"
    check "authentication_failed in a system event" "${out}" '.status' '"AUTH_DEAD"'
    out="$(printf 'OAuth token has expired · Please run /login\n' | exhaustion_classify 1)"
    check "a mid-Fire 401" "${out}" '.status' '"AUTH_DEAD"'
    out="$(printf 'Failed to authenticate\nrate limit\n' | exhaustion_classify 1)"
    check "auth death outranks a limit phrase in the same output" "${out}" '.status' '"AUTH_DEAD"'
    out="$(printf 'Failed to authenticate\n' | exhaustion_classify 1 "${REJECTED}")"
    check "a rejected record still outranks the text" "${out}" '.status' '"EXHAUSTED"'
}

test_failed() {
    echo "TEST: a genuine failure is FAILED"
    local out
    out="$(printf 'Error: ENOENT no such file\nTraceback\n' | exhaustion_classify 1)"
    check "no signature: FAILED" "${out}" '[.status, .resetAt, .source, .limitType]' '["FAILED","",null,null]'
    out="$(printf 'the design limits the number of retries\n' | exhaustion_classify 2)"
    check "the bare word limit is not a signature" "${out}" '.status' '"FAILED"'
    out="$(printf '' | exhaustion_classify 137)"
    check "empty output: FAILED" "${out}" '.status' '"FAILED"'
}

test_exit_zero_is_ok
test_rejected_record_wins
test_limit_strings_fallback
test_auth_dead
test_failed

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
