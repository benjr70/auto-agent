#!/usr/bin/env bash
# Tests for the Arbiter's escalation test against the dispute fixture
# (issue #77 AC 4, behaviour 4).
#
# Run: bash lib/arbiter-fixture.test.sh
#
# Strategy: `plugin/agents/arbiter.md` is the Arbiter's program; its
# escalation test says a disputed Finding is a product ambiguity iff BOTH
# (a) the issue and Spec are silent or contradict each other on the point and
# (b) the options differ in behaviour a user of the Target Project would see;
# otherwise, and when unsure, the Arbiter rules. `lib/testdata/
# arbiter-disputes.json` holds the five technical disputes from the first
# Target Project's PRs (720, 721 and 723 on that project) that parked a PR
# for a human under the old shape, each scored against (a) and (b) from the
# thread text. The test applies the written rule to each score and asserts
# every one is ruled, not escalated — so a future edit to the rule in the
# agent prompt (both conditions, the unsure default) or to the fixture is
# loud. No LLM runs here; the prompt is checked for the rule's words, the
# fixture for the rule's outcome. No network, no gh, no writes.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
AGENT="${ROOT_DIR}/plugin/agents/arbiter.md"
FIXTURE="${SCRIPT_DIR}/testdata/arbiter-disputes.json"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# escalates <spec_silent_or_contradicting> <user_visible_difference>
# -> prints "ambiguity" iff both are exactly true, else "ruled". This is the
#    escalation test as the agent prompt states it: both conditions must
#    hold; anything else (including an "unsure") is ruled.
escalates() {
    if [ "$1" = "true" ] && [ "$2" = "true" ]; then echo ambiguity; else echo ruled; fi
}

echo "TEST: the fixture is well formed"
t="fixture exists and parses"
if [ -f "${FIXTURE}" ] && jq -e . "${FIXTURE}" >/dev/null 2>&1; then pass "$t"; else fail "$t" "${FIXTURE}"; exit 1; fi
n="$(jq '.disputes | length' "${FIXTURE}")"
t="fixture carries the five technical disputes"
if [ "${n}" -eq 5 ]; then pass "$t"; else fail "$t" "got ${n}"; fi
t="every dispute carries the Finding, the dispute line, the escalation scores and the expected verdict"
bad="$(jq -r '.disputes[] | select(
    (.id | type) != "string" or (.finding | type) != "string" or (.dispute | type) != "string"
    or (.requirement_quoted | type) != "string"
    or (.spec_silent_or_contradicting | type) != "boolean"
    or (.user_visible_difference | type) != "boolean"
    or ((.expected.outcome == "ruled" or .expected.outcome == "ambiguity") | not)
    or ((.expected.verdict == "fix" or .expected.verdict == "dismiss" or .expected.verdict == "ambiguity") | not)
  ) | .id // "<no id>"' "${FIXTURE}")"
if [ -z "${bad}" ]; then pass "$t"; else fail "$t" "malformed: ${bad}"; fi
t="dispute ids are unique"
dups="$(jq -r '.disputes[].id' "${FIXTURE}" | sort | uniq -d)"
if [ -z "${dups}" ]; then pass "$t"; else fail "$t" "${dups}"; fi

echo "TEST: the escalation test as written rules every one of the five (AC 4: ruled, not escalated)"
while IFS=$'\t' read -r id silent visible outcome verdict; do
    got="$(escalates "${silent}" "${visible}")"
    t="${id}: escalation test -> ${got}"
    if [ "${got}" = "ruled" ] && [ "${outcome}" = "ruled" ]; then pass "$t"; else fail "$t" "expected ruled (fixture says ${outcome}); silent=${silent} visible=${visible}"; fi
    t="${id}: a ruled dispute's verdict is fix or dismiss, never ambiguity"
    if [ "${verdict}" != "ambiguity" ]; then pass "$t"; else fail "$t"; fi
done < <(jq -r '.disputes[] | [.id, (.spec_silent_or_contradicting|tostring), (.user_visible_difference|tostring), .expected.outcome, .expected.verdict] | @tsv' "${FIXTURE}")

echo "TEST: the escalation test is a real test — a dispute meeting both conditions escalates, one meeting one does not"
t="both conditions true -> ambiguity"
if [ "$(escalates true true)" = "ambiguity" ]; then pass "$t"; else fail "$t"; fi
t="silent-or-contradicting alone -> ruled"
if [ "$(escalates true false)" = "ruled" ]; then pass "$t"; else fail "$t"; fi
t="user-visible difference alone -> ruled"
if [ "$(escalates false true)" = "ruled" ]; then pass "$t"; else fail "$t"; fi
t="unsure (neither known true) -> ruled"
if [ "$(escalates unsure unsure)" = "ruled" ]; then pass "$t"; else fail "$t"; fi

echo "TEST: the agent prompt states the rule the fixture is scored against"
t="arbiter.md exists"
if [ -f "${AGENT}" ]; then pass "$t"; else fail "$t" "${AGENT}"; fi
text="$(tr '\n' ' ' < "${AGENT}" 2>/dev/null | tr -s '[:space:]' ' ')"
t="names condition (a): the issue and Spec silent or contradicting"
if printf '%s' "${text}" | grep -Eqi 'silent or contradict'; then pass "$t"; else fail "$t"; fi
t="names condition (b): a difference a user of the Target Project would see"
if printf '%s' "${text}" | grep -Eqi 'a user of the Target Project would see'; then pass "$t"; else fail "$t"; fi
t="requires both conditions"
if printf '%s' "${text}" | grep -Eqi 'both .{0,20}hold'; then pass "$t"; else fail "$t"; fi
t="names the unsure default: the Arbiter rules"
if printf '%s' "${text}" | grep -Eqi 'when unsure, (you )?rule'; then pass "$t"; else fail "$t"; fi
t="the prompt names the five-dispute fixture so the rule and its evidence stay linked"
if printf '%s' "${text}" | grep -Eq 'arbiter-disputes\.json'; then pass "$t"; else fail "$t"; fi

echo ""
echo "${TESTS_RUN} tests, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for name in "${FAILED_NAMES[@]}"; do echo "  - ${name}"; done
    exit 1
fi
exit 0
