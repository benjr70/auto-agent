#!/usr/bin/env bash
# Tests for lib/rate-limits-tap.sh
#
# Run: bash lib/rate-limits-tap.test.sh
#
# Strategy: feed canned stream-json through rate_limits_tap and assert what a
# reader sees: the passthrough is byte-identical, rate-limits.json holds the
# last event, rate-limits.jsonl every event, and the record degrades to the
# binding window when the internal `unifiedWindows` object is absent.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CANNED="${SCRIPT_DIR}/testdata/dry-run.stream.jsonl"
# shellcheck source=rate-limits-tap.sh
. "${SCRIPT_DIR}/rate-limits-tap.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

EVENT_FULL='{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789449000,"rateLimitType":"five_hour","overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.24,"resetsAt":1789449000},"seven_day":{"utilization":0.06,"resetsAt":1789758000},"seven_day_overage_included":{"utilization":0.11,"resetsAt":1789758000}}},"uuid":"u1","session_id":"s1"}'
EVENT_BARE='{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789449000,"rateLimitType":"seven_day","utilization":0.97},"uuid":"u2","session_id":"s1"}'
INIT='{"type":"system","subtype":"init","session_id":"s1","model":"claude-fable-5-1","plugins":[],"slash_commands":[]}'

test_passthrough_is_byte_identical() {
    echo "TEST: the tap forwards the stream unchanged"
    local state; state="$(mktemp -d)"
    local out; out="$(rate_limits_tap "${state}" f1 < "${CANNED}")"
    if [ "${out}" = "$(cat "${CANNED}")" ]; then pass "stdout equals stdin"
    else fail "stdout equals stdin"; fi
    # A last line without a trailing newline must not be dropped.
    out="$(printf '%s\n%s' "${INIT}" "${EVENT_FULL}" | rate_limits_tap "${state}" f1 | wc -l)"
    if [ "${out}" -eq 2 ]; then pass "a final unterminated line is forwarded"
    else fail "a final unterminated line is forwarded" "lines=${out}"; fi
    rm -rf "${state}"
}

test_records_last_event_and_appends_all() {
    echo "TEST: rate-limits.json is the last event, rate-limits.jsonl every event"
    local state; state="$(mktemp -d)"
    printf '%s\n%s\n%s\n' "${INIT}" "${EVENT_FULL}" "${EVENT_BARE}" | rate_limits_tap "${state}" fire-9 >/dev/null
    local n; n="$(wc -l < "${state}/rate-limits.jsonl")"
    if [ "${n}" -eq 2 ]; then pass "jsonl has one line per event"
    else fail "jsonl has one line per event" "lines=${n}"; fi
    local last; last="$(jq -r '.status + " " + .fireId + " " + .sessionId + " " + .model' "${state}/rate-limits.json")"
    if [ "${last}" = "rejected fire-9 s1 claude-fable-5-1" ]; then pass "json is the last event, tagged with fire id, session and the init event's model"
    else fail "json is the last event, tagged with fire id, session and the init event's model" "${last}"; fi
    local first; first="$(head -1 "${state}/rate-limits.jsonl" | jq -c '{source, w: (.windows | keys), pct: .windows.seven_day_overage_included.usedPct, iso: .windows.five_hour.resetsAtIso}')"
    if [ "${first}" = '{"source":"windows","w":["five_hour","seven_day","seven_day_overage_included"],"pct":11,"iso":"2026-09-15T05:10:00Z"}' ]; then
        pass "a full event records every window as usedPct with ISO resets"
    else fail "a full event records every window as usedPct with ISO resets" "${first}"; fi
    # Appending across Fires: a second tap run must not truncate the jsonl.
    printf '%s\n' "${EVENT_FULL}" | rate_limits_tap "${state}" fire-10 >/dev/null
    n="$(wc -l < "${state}/rate-limits.jsonl")"
    if [ "${n}" -eq 3 ] && [ "$(jq -r .fireId "${state}/rate-limits.json")" = "fire-10" ]; then pass "a later Fire appends and replaces the last event"
    else fail "a later Fire appends and replaces the last event" "lines=${n}"; fi
    rm -rf "${state}"
}

test_degrades_to_binding_window() {
    echo "TEST: an event without unifiedWindows degrades to the binding window"
    local rec; rec="$(rate_limits_record "${EVENT_BARE}" f1 "")"
    local got; got="$(printf '%s' "${rec}" | jq -c '{source, status, rateLimitType, resetsAt, utilization, model, windows}')"
    local want='{"source":"binding","status":"rejected","rateLimitType":"seven_day","resetsAt":1789449000,"utilization":0.97,"model":null,"windows":{"seven_day":{"usedPct":97,"resetsAt":1789449000,"resetsAtIso":"2026-09-15T05:10:00Z"}}}'
    if [ "${got}" = "${want}" ]; then pass "binding window rebuilt from status, rateLimitType, resetsAt, utilization"
    else fail "binding window rebuilt from status, rateLimitType, resetsAt, utilization" "${got}"; fi
    rec="$(rate_limits_record '{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"session_id":"s"}' f1 "")"
    got="$(printf '%s' "${rec}" | jq -c '{source, windows}')"
    if [ "${got}" = '{"source":"binding","windows":{"unknown":{"usedPct":null,"resetsAt":null,"resetsAtIso":null}}}' ]; then
        pass "an event with only a status still yields a record"
    else fail "an event with only a status still yields a record" "${got}"; fi
}

test_no_event_writes_nothing() {
    echo "TEST: a stream without events leaves the State dir untouched"
    local state; state="$(mktemp -d)"
    printf '%s\n{"type":"result","subtype":"success"}\n' "${INIT}" | rate_limits_tap "${state}/sub" f1 >/dev/null
    if [ ! -e "${state}/sub" ]; then pass "no files created"
    else fail "no files created" "$(ls "${state}/sub")"; fi
    rm -rf "${state}"
}

test_malformed_event_is_forwarded_not_fatal() {
    echo "TEST: a malformed event line is forwarded and skipped"
    local state; state="$(mktemp -d)"
    local out; out="$(printf '%s\n{"type":"rate_limit_event" broken\n%s\n' "${INIT}" "${EVENT_FULL}" | rate_limits_tap "${state}" f1 | wc -l)"
    local n; n="$(wc -l < "${state}/rate-limits.jsonl")"
    if [ "${out}" -eq 3 ] && [ "${n}" -eq 1 ]; then pass "3 lines forwarded, 1 event recorded"
    else fail "3 lines forwarded, 1 event recorded" "out=${out} recorded=${n}"; fi
    rm -rf "${state}"
}

test_passthrough_is_byte_identical
test_records_last_event_and_appends_all
test_degrades_to_binding_window
test_no_event_writes_nothing
test_malformed_event_is_forwarded_not_fatal

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
