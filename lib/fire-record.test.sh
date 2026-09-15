#!/usr/bin/env bash
# Tests for lib/fire-record.sh
#
# Run: bash lib/fire-record.test.sh
#
# Strategy: drive the three public functions with canned streams and assert
# the JSON a Dashboard would read.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CANNED="${SCRIPT_DIR}/testdata/dry-run.stream.jsonl"
# shellcheck source=fire-record.sh
. "${SCRIPT_DIR}/fire-record.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

test_write_and_path() {
    echo "TEST: fire_record_write lands at fires/<id>.json, atomically, and rejects bad JSON"
    local state; state="$(mktemp -d)"
    if [ "$(fire_record_path "${state}" abc)" = "${state}/fires/abc.json" ]; then pass "path"; else fail "path"; fi
    fire_record_write "${state}" abc '{"fireId":"abc","exit":0}'
    if [ "$(jq -r .fireId "${state}/fires/abc.json")" = "abc" ] && [ ! -e "${state}/fires/abc.json.tmp" ]; then pass "written, no tmp left"
    else fail "written, no tmp left" "$(ls "${state}/fires")"; fi
    local rc; fire_record_write "${state}" bad '{not json' 2>/dev/null; rc=$?
    if [ "${rc}" -ne 0 ] && [ ! -e "${state}/fires/bad.json" ]; then pass "invalid JSON is refused"; else fail "invalid JSON is refused" "rc=${rc}"; fi
    rm -rf "${state}"
}

test_summarize_full_stream() {
    echo "TEST: fire_record_summarize_stream reads init, result and the last rate-limit event"
    local got; got="$(fire_record_summarize_stream "${CANNED}" auto-agent auto-agent:dry-run | jq -c 'del(.result.text)')"
    local want='{"plugin":{"name":"auto-agent","loaded":true,"skill":"auto-agent:dry-run","skillListed":true},"result":{"subtype":"success","isError":false,"totalCostUsd":0.0204001,"numTurns":1,"sessionId":"687d1fd6-67d1-441b-9ed3-aead38412809","model":"claude-haiku-4-5-20251001"},"rateLimit":{"status":"allowed","rateLimitType":"five_hour","resetsAt":1789491600},"work":{"kind":null,"issue":null,"pr":null,"slug":null,"line":null,"settled":null},"issue":null}'
    if [ "${got}" = "${want}" ]; then pass "summary"; else fail "summary" "${got}"; fi
    got="$(fire_record_summarize_stream "${CANNED}" auto-agent auto-agent:afk-pickup | jq -c '.plugin | {loaded, skillListed}')"
    if [ "${got}" = '{"loaded":true,"skillListed":false}' ]; then pass "an unlisted skill is reported as not listed"; else fail "an unlisted skill is reported as not listed" "${got}"; fi
    got="$(fire_record_summarize_stream "${CANNED}" other-plugin "" | jq -c '.plugin')"
    if [ "${got}" = '{"name":"other-plugin","loaded":false,"skill":null,"skillListed":false}' ]; then pass "another plugin name is not loaded; empty skill is null"
    else fail "another plugin name is not loaded; empty skill is null" "${got}"; fi
}

test_summarize_tolerates_truncated_missing_and_picked() {
    echo "TEST: a truncated, malformed or missing stream still summarises; picked: #N sets issue"
    local f; f="$(mktemp)"
    head -1 "${CANNED}" > "${f}"; printf '{"type":"result","sub' >> "${f}"
    local got; got="$(fire_record_summarize_stream "${f}" auto-agent x | jq -c '{loaded: .plugin.loaded, subtype: .result.subtype, isError: .result.isError, rl: .rateLimit, issue}')"
    if [ "${got}" = '{"loaded":true,"subtype":null,"isError":null,"rl":null,"issue":null}' ]; then pass "truncated last line ignored, nulls elsewhere"
    else fail "truncated last line ignored, nulls elsewhere" "${got}"; fi
    got="$(fire_record_summarize_stream "${f}.missing" auto-agent x | jq -c '{loaded: .plugin.loaded, model: .result.model}')"
    if [ "${got}" = '{"loaded":false,"model":null}' ]; then pass "missing file yields an all-null summary"; else fail "missing file yields an all-null summary" "${got}"; fi
    jq -c 'if .type == "assistant" then .message.content[0].text = "thinking\n  picked:   #42 something\npicked: #43" else . end' "${CANNED}" > "${f}"
    got="$(fire_record_summarize_stream "${f}" auto-agent x | jq -r .issue)"
    if [ "${got}" = "42" ]; then pass "first picked line wins"; else fail "first picked line wins" "${got}"; fi
    rm -f "${f}"
}

test_noop_ok() {
    echo "TEST: fire_record_noop_ok needs exit 0, plugin, skill and the ok line"
    local f; f="$(mktemp)"
    local base='{"exit":0,"plugin":{"loaded":true,"skillListed":true},"result":{"text":"a\ndry-run: ok"}}'
    echo "${base}" > "${f}"
    if fire_record_noop_ok "${f}" "dry-run: ok"; then pass "all four present"; else fail "all four present"; fi
    for mutation in '.exit = 1' '.plugin.loaded = false' '.plugin.skillListed = false' '.result.text = "dry-run: okay"' '.result.text = null'; do
        echo "${base}" | jq "${mutation}" > "${f}"
        if fire_record_noop_ok "${f}" "dry-run: ok"; then fail "fails when ${mutation}"; else pass "fails when ${mutation}"; fi
    done
    rm -f "${f}"
}

test_work_and_dry_run_ok() {
    echo "TEST: the work block classifies the pickup skill's stable lines; fire_record_dry_run_ok needs a dry-run verdict"
    local f; f="$(mktemp)"
    local t got want
    for case in \
        'picked:   #30 Budget gate|pick|30|null' \
        'picked:   reconcile PR #47 (issue #27)|reconcile|27|47' \
        'picked:   reconcile PR #47 (issue #null)|reconcile|null|47' \
        'picked:   #12 x\nresolve: #12 research the-slug|resolve|12|null' \
        'afk-pickup: would-pick #30 Budget gate|dry-run|30|null' \
        'picked:   #30 x\nafk-pickup: would-resume #30 x|dry-run|30|null' \
        'afk-pickup: would-reconcile PR #47 (issue #27)|dry-run|27|47' \
        'afk-pickup: would-fail #9 Too big (resume cap)|dry-run|9|null' \
        'afk-pickup: skip — 1 in flight|none|null|null' \
        'afk-pickup: no eligible issue|none|null|null' \
        'nothing decisive|null|null|null'; do
        IFS='|' read -r text kind issue pr <<<"${case}"
        jq -c --arg t "$(printf '%b' "${text}")" 'if .type == "assistant" then .message.content[0].text = $t else . end' "${CANNED}" > "${f}"
        got="$(fire_record_summarize_stream "${f}" auto-agent x | jq -c '[.work.kind, .work.issue, .work.pr]')"
        t="'${text}' -> ${kind} ${issue} ${pr}"
        want="$(jq -cn --arg k "${kind}" --argjson i "${issue}" --argjson p "${pr}" '[(if $k == "null" then null else $k end), $i, $p]')"
        if [ "${got}" = "${want}" ]; then pass "$t"; else fail "$t" "${got}"; fi
    done
    for case in 'resolve: DONE — #12 closed|done' 'resolve: DONE — #12 relabelled HITL (needs code)|hitl' 'resolve: FAILED — #12 reason|failed'; do
        IFS='|' read -r text settled <<<"${case}"
        jq -c --arg t "resolve: #12 task s"$'\n'"${text}" 'if .type == "assistant" then .message.content[0].text = $t else . end' "${CANNED}" > "${f}"
        got="$(fire_record_summarize_stream "${f}" auto-agent x | jq -r '.work.settled')"
        if [ "${got}" = "${settled}" ]; then pass "settled ${settled}"; else fail "settled ${settled}" "${got}"; fi
    done
    local base='{"exit":0,"plugin":{"loaded":true,"skillListed":true},"work":{"kind":"none"}}'
    echo "${base}" > "${f}"
    if fire_record_dry_run_ok "${f}"; then pass "dry-run ok on work none"; else fail "dry-run ok on work none"; fi
    echo "${base}" | jq '.work.kind = "dry-run"' > "${f}"
    if fire_record_dry_run_ok "${f}"; then pass "dry-run ok on work dry-run"; else fail "dry-run ok on work dry-run"; fi
    for mutation in '.exit = 1' '.plugin.loaded = false' '.plugin.skillListed = false' '.work.kind = "pick"' '.work.kind = null'; do
        echo "${base}" | jq "${mutation}" > "${f}"
        if fire_record_dry_run_ok "${f}"; then fail "dry-run fails when ${mutation}"; else pass "dry-run fails when ${mutation}"; fi
    done
    rm -f "${f}"
}

test_write_and_path
test_summarize_full_stream
test_summarize_tolerates_truncated_missing_and_picked
test_noop_ok
test_work_and_dry_run_ok

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
