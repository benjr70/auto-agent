#!/usr/bin/env bash
# Tests for lib/pr-checks.sh
#
# Run: bash lib/pr-checks.test.sh
#
# Strategy: the verdict is pure (a check list on stdin, the required names as
# an argument), so each test feeds one list and asserts the reason line and
# exit code the two merge gates act on.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=pr-checks.sh
. "${SCRIPT_DIR}/pr-checks.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# expect <name> <required-json> <checks> <want-reason|''>
expect() {
    local t="$1" req="$2" checks="$3" want="$4" out rc reason
    out="$(printf '%s' "${checks}" | pr_checks_verdict "${req}")"; rc=$?
    reason="${out%%$'\t'*}"
    if [ -z "${want}" ]; then
        if [ $rc -eq 0 ] && [ -z "${out}" ]; then pass "$t"; else fail "$t" "rc=$rc out=${out}"; fi
    else
        if [ $rc -eq 1 ] && [ "${reason}" = "${want}" ]; then pass "$t"; else fail "$t" "rc=$rc out=${out}"; fi
    fi
}

GREEN='[{"name":"test","bucket":"pass"},{"name":"lint","bucket":"skipping"}]'

expect "all green (pass and skipping) with nothing required vouches" '[]' "${GREEN}" ''
expect "an unreadable list is checks-unreadable" '[]' 'not json' checks-unreadable
expect "an empty list is checks-missing" '[]' '[]' checks-missing
expect "a failing check is checks-not-green" '[]' '[{"name":"test","bucket":"fail"}]' checks-not-green
expect "a pending check is checks-not-green" '[]' '[{"name":"test","bucket":"pending"}]' checks-not-green
expect "a required check that passed vouches" '["test"]' "${GREEN}" ''
expect "a required check that is absent is checks-missing" '["title"]' "${GREEN}" checks-missing
expect "a required check that was skipped is checks-missing" '["lint"]' "${GREEN}" checks-missing
expect "not-green is reported before a missing required check" '["title"]' '[{"name":"test","bucket":"fail"}]' checks-not-green

t="the detail names the missing required check"
out="$(printf '%s' "${GREEN}" | pr_checks_verdict '["title"]')"
if [ "${out#*$'\t'}" = "a required check did not run and pass: title" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
