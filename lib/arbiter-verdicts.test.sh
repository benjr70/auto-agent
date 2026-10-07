#!/usr/bin/env bash
# Tests for lib/arbiter-verdicts.sh (issue #77, the critical behaviours:
# 2 "a ruled fix cannot be re-disputed" and 3 "cap semantics"; behaviour 1
# "dismiss resolves without a commit" is in lib/thread-reconciler.test.sh and
# lib/pr-reconcile-runbook.test.sh).
#
# Run: bash lib/arbiter-verdicts.test.sh
#
# Strategy: the library is pure text-to-JSON, so each rule the runbook relies
# on is driven with a literal Arbiter reply or implementer reply and asserted
# on the parsed action. No network, no gh, no git, no writes.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/arbiter-verdicts.sh"
# shellcheck source=arbiter-verdicts.sh
. "${LIB}"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

THREADS='[{"threadId":"T1","authored":"bot"},{"threadId":"T2","authored":"bot"},{"threadId":"T3","authored":"human"},{"threadId":"T4","authored":"bot"}]'

echo "TEST: av_parse_verdicts reads one verdict per thread"
reply=$'T1: fix — move the check into useVoiceFill.tsx before the READY branch; done when the new test passes\nT2: dismiss — the Finding names a path the app never produces\nT3: ambiguity — {"threadId":"T3","recommended":"A"}\nT4: dismiss — duplicate of T2'
out="$(av_parse_verdicts "${THREADS}" "${reply}")"
t="fix, dismiss, ambiguity and a second dismiss are read with their text"
if [ "$(printf '%s' "${out}" | jq -c '[.[] | [.threadId, .verdict]]')" = '[["T1","fix"],["T2","dismiss"],["T3","ambiguity"],["T4","dismiss"]]' ] \
   && [ "$(printf '%s' "${out}" | jq -r '.[0].text')" = "move the check into useVoiceFill.tsx before the READY branch; done when the new test passes" ] \
   && [ "$(printf '%s' "${out}" | jq -r '.[2].text | fromjson | .recommended')" = "A" ]; then pass "$t"; else fail "$t" "${out}"; fi
t="the output carries authored through, so the caller can route the verdict"
if [ "$(printf '%s' "${out}" | jq -c '[.[] | .authored]')" = '["bot","bot","human","bot"]' ]; then pass "$t"; else fail "$t" "${out}"; fi

echo "TEST: a malformed reply leaves the thread unruled, never dismissed on a guess"
reply=$'T1: fix — a\nT1: dismiss — b\nT2: reject — c\nT3: ambiguity — {}\nT9: dismiss — not a thread we gave'
out="$(av_parse_verdicts "${THREADS}" "${reply}")"
t="a doubled threadId, a verdict word outside the three and a missing line are each unruled with a reason"
if [ "$(printf '%s' "${out}" | jq -c '[.[] | [.threadId, .verdict]]')" = '[["T1","unruled"],["T2","unruled"],["T3","ambiguity"],["T4","unruled"]]' ] \
   && [ "$(printf '%s' "${out}" | jq -r '.[0].reason')" = "threadId doubled" ] \
   && printf '%s' "${out}" | jq -r '.[1].reason' | grep -q 'reject' \
   && [ "$(printf '%s' "${out}" | jq -r '.[3].reason')" = "no verdict line" ]; then pass "$t"; else fail "$t" "${out}"; fi
t="a verdict for a thread the Arbiter was not given is ignored"
if [ "$(printf '%s' "${out}" | jq 'length')" -eq 4 ]; then pass "$t"; else fail "$t" "${out}"; fi

echo "TEST: a human-authored thread is never dismissed or ruled fix on the Arbiter's word"
reply=$'T1: dismiss — fine\nT2: fix — fine\nT3: dismiss — the human is wrong\nT4: ambiguity — {}'
out="$(av_parse_verdicts "${THREADS}" "${reply}")"
t="dismiss on the human thread is refused (unruled, collected); the same word on a bot thread is a verdict"
if [ "$(printf '%s' "${out}" | jq -c '[.[] | [.threadId, .verdict]]')" = '[["T1","dismiss"],["T2","fix"],["T3","unruled"],["T4","ambiguity"]]' ] \
   && printf '%s' "${out}" | jq -r '.[2].reason' | grep -q 'human-authored'; then pass "$t"; else fail "$t" "${out}"; fi
out="$(av_parse_verdicts "${THREADS}" $'T3: fix — do it')"
t="fix on the human thread is refused too"
if [ "$(printf '%s' "${out}" | jq -r '.[2].verdict')" = "unruled" ]; then pass "$t"; else fail "$t" "${out}"; fi
t="an empty reply leaves every thread unruled"
out="$(av_parse_verdicts "${THREADS}" "")"
if [ "$(printf '%s' "${out}" | jq -c '[.[] | .verdict] | unique')" = '["unruled"]' ]; then pass "$t"; else fail "$t" "${out}"; fi

echo "TEST: av_parse_replies — round 1, no ruling: a dispute is a dispute (it goes to the Arbiter)"
reply=$'T1: renamed the variable in src/a.ts\nT2: revise-dispute — the Finding names a path the app never produces\nT4: cannot — the file is generated'
out="$(av_parse_replies "${THREADS}" "${reply}" 1)"
t="fixed, dispute, unaddressed and cannot are each read"
if [ "$(printf '%s' "${out}" | jq -c '[.[] | [.threadId, .action]]')" = '[["T1","fixed"],["T2","dispute"],["T3","unaddressed"],["T4","cannot"]]' ] \
   && [ "$(printf '%s' "${out}" | jq -r '.[1].text')" = "the Finding names a path the app never produces" ]; then pass "$t"; else fail "$t" "${out}"; fi

echo "TEST: a ruled fix cannot be re-disputed (issue #77 AC 3, behaviour 2)"
RULED='[{"threadId":"T1","authored":"bot","ruled":"fix","arbiter":"move the check before the READY branch"},{"threadId":"T2","authored":"bot"}]'
reply=$'T1: revise-dispute — I still think the Arbiter is wrong\nT2: revise-dispute — and this one too'
out="$(av_parse_replies "${RULED}" "${reply}" 2)"
t="revise-dispute on a thread the Arbiter ruled fix is refused and read as cannot"
if [ "$(printf '%s' "${out}" | jq -r '.[0].action')" = "cannot" ] && [ "$(printf '%s' "${out}" | jq -r '.[0].text')" = "I still think the Arbiter is wrong" ]; then pass "$t"; else fail "$t" "${out}"; fi
t="from round 2 on a dispute on any thread is cannot: the Arbiter ran once, after round 1, and nothing remains to rule it"
if [ "$(printf '%s' "${out}" | jq -r '.[1].action')" = "cannot" ]; then pass "$t"; else fail "$t" "${out}"; fi
out="$(av_parse_replies "${RULED}" "${reply}" 1)"
t="in round 1 the ruled thread is still cannot while the unruled one is a dispute"
if [ "$(printf '%s' "${out}" | jq -c '[.[] | .action]')" = '["cannot","dispute"]' ]; then pass "$t"; else fail "$t" "${out}"; fi
out="$(av_parse_replies "${RULED}" $'T1: applied: moved the check before the READY branch, test added' 2)"
t="a ruled fix the implementer applied is fixed"
if [ "$(printf '%s' "${out}" | jq -r '.[0].action')" = "fixed" ]; then pass "$t"; else fail "$t" "${out}"; fi
t="a non-integer round is a usage error"
av_parse_replies "${RULED}" "x" one >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi

echo "TEST: a human ruling is never carried to a dispute (issue #76 ruling key)"
HUMAN_RULED='[{"threadId":"T1","authored":"bot","ruling":"keep it as is, resolve"},{"threadId":"T2","authored":"human","ruling":"do X instead"},{"threadId":"T3","authored":"bot","ruling":null}]'
reply=$'T1: revise-dispute — I disagree with the human\nT2: no-change — the ruling asked to keep the current shape\nT3: no-change — nothing to do'
out="$(av_parse_replies "${HUMAN_RULED}" "${reply}" 1)"
t="revise-dispute on a thread with a human ruling is refused and read as no-change with the ruling as the text"
if [ "$(printf '%s' "${out}" | jq -r '.[0].action')" = "no-change" ] && [ "$(printf '%s' "${out}" | jq -r '.[0].text')" = "keep it as is, resolve" ]; then pass "$t"; else fail "$t" "${out}"; fi
t="an explicit no-change line is no-change with its reason, ruling or not"
if [ "$(printf '%s' "${out}" | jq -c '[.[1:][] | [.action, .text]]')" = '[["no-change","the ruling asked to keep the current shape"],["no-change","nothing to do"]]' ]; then pass "$t"; else fail "$t" "${out}"; fi

echo "TEST: cap semantics — implementer rounds only (issue #77 AC 5, behaviour 3)"
t="cap 1, round 1, nothing ruled fix: no round remains"
if [ "$(av_next_round 1 1 0)" = "cap" ]; then pass "$t"; else fail "$t"; fi
t="cap 1, round 1, one ruled fix: the ruled round is guaranteed (round 2 runs)"
if [ "$(av_next_round 1 1 1)" = "2" ]; then pass "$t"; else fail "$t" "$(av_next_round 1 1 1)"; fi
t="cap 1, after the ruled round 2: no round remains — the guarantee is one round, not a new cap"
if [ "$(av_next_round 2 1 0)" = "cap" ]; then pass "$t"; else fail "$t"; fi
t="cap 3, round 1 -> 2 -> 3 -> cap"
if [ "$(av_next_round 1 3 0)" = "2" ] && [ "$(av_next_round 2 3 0)" = "3" ] && [ "$(av_next_round 3 3 0)" = "cap" ]; then pass "$t"; else fail "$t"; fi
t="a Fire with one implementer round and one Arbiter run has used one round: the Arbiter and its dismissals are not inputs, so the next round is 2 under a cap of 2"
if [ "$(av_next_round 1 2 0)" = "2" ]; then pass "$t"; else fail "$t"; fi
t="non-integer input is a usage error"
av_next_round 1 x 0 >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
