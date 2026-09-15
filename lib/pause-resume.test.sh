#!/usr/bin/env bash
# Tests for lib/pause-resume.sh
#
# Run: bash lib/pause-resume.test.sh
#
# Strategy: pause_resume_action is a pure function of the label/count state and
# the cap. The resume-vs-fail cap is the core safety property (a too-big issue
# must not loop forever), so it is covered most thoroughly; the cap arrives as
# the third argument from the Harness config's rounds.pause_resume.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=pause-resume.sh
. "${SCRIPT_DIR}/pause-resume.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# expect <name> <issue> <count> <cap-or-empty> <want-json>
expect() {
    local name="$1" got
    if [ -n "$4" ]; then got="$(pause_resume_action "$2" "$3" "$4")"; else got="$(pause_resume_action "$2" "$3")"; fi
    if [ "${got}" = "$5" ]; then pass "${name}"; else fail "${name}" "got ${got} want $5"; fi
}

echo "pause-resume.sh tests:"
expect "a paused issue below the cap resumes" 290 1 '' '{"action":"resume","issue":290,"pauseCount":1}'
expect "no paused issue picks new" '' '' '' '{"action":"pick-new","issue":null,"pauseCount":0}'
expect "reaching the cap fails the issue (default cap 3)" 290 3 '' '{"action":"fail","issue":290,"pauseCount":3}'
expect "just below the cap still resumes" 290 2 '' '{"action":"resume","issue":290,"pauseCount":2}'
expect "the cap from the config (3rd arg) lowers the limit" 290 2 2 '{"action":"fail","issue":290,"pauseCount":2}'
expect "the cap from the config (3rd arg) raises the limit" 290 3 5 '{"action":"resume","issue":290,"pauseCount":3}'
expect "an unreadable cap falls back to the default" 290 3 'lots' '{"action":"fail","issue":290,"pauseCount":3}'
expect "an empty pause count resumes fail-safe" 290 '' '' '{"action":"resume","issue":290,"pauseCount":1}'
expect "a non-numeric pause count resumes fail-safe" 290 'not-a-number' '' '{"action":"resume","issue":290,"pauseCount":1}'
expect "a non-numeric issue picks new" 'abc' 2 '' '{"action":"pick-new","issue":null,"pauseCount":0}'
got="$(PAUSE_RESUME_CAP=2 pause_resume_action 290 2)"
if [ "${got}" = '{"action":"fail","issue":290,"pauseCount":2}' ]; then pass "PAUSE_RESUME_CAP still overrides the default when no cap is passed"
else fail "PAUSE_RESUME_CAP still overrides the default when no cap is passed" "${got}"; fi
got="$(PAUSE_RESUME_CAP=2 pause_resume_action 290 2 4)"
if [ "${got}" = '{"action":"resume","issue":290,"pauseCount":2}' ]; then pass "the config cap wins over PAUSE_RESUME_CAP"
else fail "the config cap wins over PAUSE_RESUME_CAP" "${got}"; fi

echo ""
echo "${TESTS_RUN} tests, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  failed: %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
