#!/usr/bin/env bash
# Tests for lib/arbiter-fixture.sh and lib/testdata/arbiter-disputes.json
# (issue #77 AC 4, behaviour 4).
#
# Run: bash lib/arbiter-fixture.test.sh
#
# Strategy: AC 4 says the five technical disputes from the first Target
# Project's PRs 720, 721 and 723 would each be ruled, not escalated, by the
# escalation test in plugin/agents/arbiter.md "when run as a fixture". The
# run itself is an agent run (a model reading the prompt), which no test here
# performs; what this suite pins is everything around it so the run is one
# command and its outcome is machine-checked:
#   - the fixture is well formed and internally consistent (each entry's
#     scores against conditions (a) and (b) agree with its expected outcome);
#   - `render` turns the fixture into the prompt shape §2 step 3b sends: the
#     Finding, the dispute line verbatim, `authored: bot`, and nothing of the
#     fixture's own rationale (the Arbiter never sees the implementer's
#     reasoning, and never the test's);
#   - `check` reads the agent's reply through the runbook's own reader and
#     fails on any escalation, unruled thread or missing id;
#   - the prompt still states the rule the fixture is scored against, using
#     the runbook-check rules for agents/arbiter.md as the single source of
#     those phrases (no second copy of the regexes here).
# No LLM, no network, no gh, no writes outside mktemp.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TOOL="${SCRIPT_DIR}/arbiter-fixture.sh"
CHECKER="${SCRIPT_DIR}/runbook-check.sh"
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

echo "TEST: the fixture is internally consistent with the rule it is scored against"
t="an entry's expected outcome is ambiguity iff both (a) and (b) are scored true, and a ruled entry's verdict is fix or dismiss"
inconsistent="$(jq -r '.disputes[] | select(
    ((.spec_silent_or_contradicting and .user_visible_difference) != (.expected.outcome == "ambiguity"))
    or (.expected.outcome == "ruled" and .expected.verdict == "ambiguity")
  ) | .id' "${FIXTURE}")"
if [ -z "${inconsistent}" ]; then pass "$t"; else fail "$t" "${inconsistent}"; fi
t="every one of the five is expected ruled (AC 4) — the agent run is what proves it; this pins the expectation"
if [ "$(jq -r '[.disputes[] | .expected.outcome] | unique | join(",")' "${FIXTURE}")" = "ruled" ]; then pass "$t"; else fail "$t"; fi

echo "TEST: render produces the prompt shape step 3b sends"
prompt="$(bash "${TOOL}" render)"; rc=$?
t="render exits 0 with a prompt"
if [ "${rc}" -eq 0 ] && [ -n "${prompt}" ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
missing=()
while IFS=$'\t' read -r id dispute; do
    printf '%s' "${prompt}" | grep -qF "Thread \`${id}\`" || missing+=("${id}: heading")
    printf '%s' "${prompt}" | grep -qF "${id}: revise-dispute — ${dispute}" || missing+=("${id}: dispute line")
done < <(jq -r '.disputes[] | [.id, .dispute] | @tsv' "${FIXTURE}")
t="every dispute appears with its threadId and its dispute line verbatim"
if [ "${#missing[@]}" -eq 0 ]; then pass "$t"; else fail "$t" "${missing[*]}"; fi
t="every thread is marked authored: bot and the Finding is embedded verbatim"
if [ "$(printf '%s' "${prompt}" | grep -c '^authored: bot$')" -eq 5 ] && printf '%s' "${prompt}" | grep -qF "$(jq -r '.disputes[2].finding' "${FIXTURE}")"; then pass "$t"; else fail "$t"; fi
t="the fixture's own rationale (why / expected) never reaches the prompt — the Arbiter sees the dispute line and nothing else of the arguing"
leak=0
while IFS= read -r why; do printf '%s' "${prompt}" | grep -qF "${why}" && leak=$((leak + 1)); done < <(jq -r '.disputes[].why' "${FIXTURE}")
if [ "${leak}" -eq 0 ] && ! printf '%s' "${prompt}" | grep -qi 'expected'; then pass "$t"; else fail "$t" "leaked ${leak}"; fi
t="the prompt asks for one verdict line per thread in the three verdict shapes"
if printf '%s' "${prompt}" | grep -q '<threadId>: fix — ' && printf '%s' "${prompt}" | grep -q '<threadId>: dismiss — ' && printf '%s' "${prompt}" | grep -q '<threadId>: ambiguity — '; then pass "$t"; else fail "$t"; fi
t="a missing fixture is a usage error"
bash "${TOOL}" render /nonexistent.json >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi

echo "TEST: check reads the agent's reply through the runbook's reader"
dir="$(mktemp -d)"
jq -r '.disputes[] | "\(.id): \(.expected.verdict) — reason for \(.id)"' "${FIXTURE}" > "${dir}/ruled.txt"
out="$(bash "${TOOL}" check "${dir}/ruled.txt" 2>&1)"; rc=$?
t="a reply ruling all five (the expected verdicts) passes and prints each verdict"
if [ "${rc}" -eq 0 ] && [ "$(printf '%s\n' "${out}" | grep -c ': dismiss — \|: fix — ')" -eq 5 ] && printf '%s' "${out}" | grep -q 'every dispute ruled'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
sed '3s/: dismiss — .*/: ambiguity — {"threadId":"x"}/' "${dir}/ruled.txt" > "${dir}/escalated.txt"
out="$(bash "${TOOL}" check "${dir}/escalated.txt" 2>&1)"; rc=$?
t="a reply escalating one of the five fails naming it"
if [ "${rc}" -eq 1 ] && printf '%s' "${out}" | grep -q 'pr721-button-after-finish: ambiguity' && printf '%s' "${out}" | grep -q '1 of 5'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
sed '1d' "${dir}/ruled.txt" > "${dir}/short.txt"
out="$(bash "${TOOL}" check "${dir}/short.txt" 2>&1)"; rc=$?
t="a reply missing a thread fails: unruled is not ruled"
if [ "${rc}" -eq 1 ] && printf '%s' "${out}" | grep -q 'pr720-reviewrows-signature: unruled — no verdict line'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
out="$(bash "${TOOL}" check - < "${dir}/ruled.txt" 2>&1)"; rc=$?
t="check reads the reply from stdin with -"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
bash "${TOOL}" check >/dev/null 2>&1; rc=$?
t="check without a reply is a usage error"
if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${dir}"

echo "TEST: the agent prompt states the rule the fixture is scored against (phrases owned by runbook-check)"
t="arbiter.md exists"
if [ -f "${AGENT}" ]; then pass "$t"; else fail "$t" "${AGENT}"; fi
text="$(sed -E 's/^[[:space:]]*>[[:space:]]?//' "${AGENT}" 2>/dev/null | tr '\n' ' ' | tr -s '[:space:]' ' ')"
rules=0; unmet=()
while IFS=$'\t' read -r kind spec pattern; do
    [ "${kind}" = "rule" ] || continue
    case "${spec}" in "agents/arbiter.md: escalation-test"|"agents/arbiter.md: unsure-default") ;; *) continue ;; esac
    rules=$((rules + 1))
    printf '%s' "${text}" | grep -Eqi -- "${pattern}" || unmet+=("${spec#*: } /${pattern}/")
done < <(bash "${CHECKER}" --list)
t="runbook-check publishes the escalation-test and unsure-default rules (both conditions, the conjunction, the unsure default) and the prompt meets each"
if [ "${rules}" -ge 4 ] && [ "${#unmet[@]}" -eq 0 ]; then pass "$t"; else fail "$t" "rules=${rules} unmet: ${unmet[*]}"; fi
t="the prompt does not point the live agent at this test fixture"
if ! grep -q 'arbiter-disputes' "${AGENT}"; then pass "$t"; else fail "$t"; fi

echo ""
echo "${TESTS_RUN} tests, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for name in "${FAILED_NAMES[@]}"; do echo "  - ${name}"; done
    exit 1
fi
exit 0
