#!/usr/bin/env bash
# Structural tests for plugin/skills/pr-reconcile/SKILL.md §2 (issue #77, the
# critical behaviours 1-3 as the runbook orders them).
#
# Run: bash lib/pr-reconcile-runbook.test.sh
#
# Strategy: lib/runbook-check.sh pins phrases; this suite pins STRUCTURE —
# which step comes before which, and which commands a step may contain — so
# an edit that keeps every sentence but moves the commit before the dismissal,
# or drops the library calls that make a dispute un-redisputable, is loud. The
# executable halves of the same behaviours are in lib/arbiter-verdicts.test.sh
# (replies, rounds) and lib/thread-reconciler.test.sh (the dismissal is a reply
# and a resolve, no git). No network, no gh, no writes.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
SKILL="${ROOT_DIR}/plugin/skills/pr-reconcile/SKILL.md"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# line_of <regex> -> the first line number matching, or empty
line_of() { grep -n -E -m1 -- "$1" "${SKILL}" | cut -d: -f1; }
# between <from-line> <to-line> -> the text of that line range
between() { sed -n "${1},${2}p" "${SKILL}"; }

t="the runbook exists"
if [ -f "${SKILL}" ]; then pass "$t"; else fail "$t" "${SKILL}"; exit 1; fi

sec2="$(line_of '^### 2\. Comment phase')"
step1="$(line_of '^1\. \*\*Spawn one implementer per round\*\*')"
step2="$(line_of '^2\. \*\*Commit \+ push\*\*')"
step3="$(line_of '^3\. \*\*Reply \+ resolve per addressed thread\*\*')"
step3b="$(line_of '^3b\. \*\*The Arbiter step\*\*')"
step4="$(line_of '^4\. Re-enumerate')"
exit2="$(line_of '^\*\*§2-exit\*\*')"
sec3="$(line_of '^### 3\. Verification tail')"

echo "TEST: §2 keeps its round-loop shape"
t="steps 1, 2, 3, 3b, 4 and §2-exit appear once each, in that order, inside §2"
if [ -n "${sec2}" ] && [ -n "${step1}" ] && [ -n "${step2}" ] && [ -n "${step3}" ] && [ -n "${step3b}" ] && [ -n "${step4}" ] && [ -n "${exit2}" ] && [ -n "${sec3}" ] \
   && [ "${sec2}" -lt "${step1}" ] && [ "${step1}" -lt "${step2}" ] && [ "${step2}" -lt "${step3}" ] && [ "${step3}" -lt "${step3b}" ] && [ "${step3b}" -lt "${step4}" ] && [ "${step4}" -lt "${exit2}" ] && [ "${exit2}" -lt "${sec3}" ] \
   && [ "$(grep -c -E '^3b\. \*\*The Arbiter step\*\*' "${SKILL}")" -eq 1 ]; then pass "$t"; else fail "$t" "sec2=${sec2} 1=${step1} 2=${step2} 3=${step3} 3b=${step3b} 4=${step4} exit=${exit2} sec3=${sec3}"; fi

echo "TEST: behaviour 1 — a dismissal resolves the thread with no commit (AC 2)"
arbiter="$(between "${step3b}" "$((step4 - 1))")"
dismiss_from="$(printf '%s\n' "${arbiter}" | grep -n -m1 -- '`<threadId>: dismiss — <reason>`' | cut -d: -f1)"
dismiss_to="$(printf '%s\n' "${arbiter}" | grep -n -m1 -- '`<threadId>: ambiguity — <decision JSON>`' | cut -d: -f1)"
t="the dismiss verdict bullet exists and precedes the ambiguity bullet"
if [ -n "${dismiss_from}" ] && [ -n "${dismiss_to}" ] && [ "${dismiss_from}" -lt "${dismiss_to}" ]; then pass "$t"; else fail "$t"; exit 1; fi
dismiss="$(printf '%s\n' "${arbiter}" | sed -n "${dismiss_from},${dismiss_to}p")"
t="the dismiss bullet closes the thread with tr_resolve_with_reply under the Arbiter marker and the exact reply shape"
if printf '%s' "${dismiss}" | grep -q 'tr_resolve_with_reply "\$PR_NUM" "<commentDatabaseId>" "<threadId>" "\$TR_MARKER_ARBITER" "arbiter: dismissed — <reason>"'; then pass "$t"; else fail "$t"; fi
t="the dismiss bullet contains no git commit, git push, or implementer spawn"
if ! printf '%s' "${dismiss}" | grep -Eq 'git commit|git push|auto-agent:implementer'; then pass "$t"; else fail "$t"; fi
t="the only git commit in §2 is step 2, which precedes the Arbiter step; nothing commits between the Arbiter step and §2-exit"
commits="$(between "${sec2}" "${sec3}" | grep -n 'git commit' | cut -d: -f1)"
ok=1
for c in ${commits}; do
    abs=$((sec2 + c - 1))
    if [ "${abs}" -lt "${step2}" ] || [ "${abs}" -ge "${step3}" ]; then ok=0; fi
done
if [ "${ok}" -eq 1 ] && [ -n "${commits}" ]; then pass "$t"; else fail "$t" "commit lines: ${commits}"; fi
t="step 3b names the dismissal as consuming no round"
if printf '%s' "${arbiter}" | grep -qi 'dismissal consumes no round'; then pass "$t"; else fail "$t"; fi

echo "TEST: behaviour 2 — a ruled fix cannot be re-disputed (AC 3)"
step1_text="$(between "${step1}" "$((step2 - 1))")"
t="the implementer's verbatim instructions forbid disputing a ruled thread and name the cannot reply"
if printf '%s' "${step1_text}" | grep -q '> ruled: apply the instruction as given. You may NOT dispute it' && printf '%s' "${step1_text}" | grep -q '> cannot apply it, reply `<threadId>: cannot — <one-line reason>`'; then pass "$t"; else fail "$t"; fi
t="step 1 reads the reply through av_parse_replies with the round number, so the refusal is the library's, not the session's"
if printf '%s' "${step1_text}" | grep -q 'av_parse_replies "\$ROUND_THREADS" "\$IMPLEMENTER_REPLY" "\$R"'; then pass "$t"; else fail "$t"; fi
t="step 1 says a round-2+ dispute on any thread is read as cannot and becomes an ambiguity decision"
if printf '%s' "${step1_text}" | tr '\n' ' ' | tr -s ' ' | grep -qi 'from round 2 on, a `revise-dispute` line on any thread, ruled or not, is refused and read as `cannot`' \
   && printf '%s' "${step1_text}" | tr '\n' ' ' | tr -s ' ' | grep -qi 'becomes an \*\*ambiguity decision\*\*'; then pass "$t"; else fail "$t"; fi
t="step 3b reads the Arbiter's reply through av_parse_verdicts and names fix as binding and not disputable"
if printf '%s' "${arbiter}" | grep -q 'av_parse_verdicts "\$DISPUTED_THREADS" "\$ARBITER_REPLY"' && printf '%s' "${arbiter}" | tr '\n' ' ' | tr -s ' ' | grep -q 'It \*\*may not be disputed\*\*'; then pass "$t"; else fail "$t"; fi

echo "TEST: behaviour 3 — the cap counts implementer rounds only and the ruled round is guaranteed (AC 5)"
t="the next round comes from av_next_round with the cap and the fix count, inside step 3b"
if printf '%s' "${arbiter}" | grep -q 'av_next_round "\$R" "\$REVISE_ROUNDS_MAX" "<count of fix verdicts>"'; then pass "$t"; else fail "$t"; fi
t="step 4 branches on NEXT, not on R < REVISE_ROUNDS_MAX"
step4_text="$(between "${step4}" "$((exit2 - 1))")"
if printf '%s' "${step4_text}" | grep -q '`NEXT` is a round' && ! printf '%s' "${step4_text}" | grep -Eq 'R *< *REVISE_ROUNDS_MAX|R == REVISE_ROUNDS_MAX'; then pass "$t"; else fail "$t"; fi
t="the §2 preamble says the cap counts implementer rounds only and one implementer round plus one Arbiter run is one round"
pre="$(between "${sec2}" "$((step1 - 1))" | tr '\n' ' ' | tr -s ' ')"
if printf '%s' "${pre}" | grep -qi 'counts implementer rounds only' && printf '%s' "${pre}" | grep -qi 'one implementer round and one Arbiter run has used one round'; then pass "$t"; else fail "$t"; fi
t="step 3b says a cap of 1 still runs the ruled round"
if printf '%s' "${arbiter}" | tr '\n' ' ' | tr -s ' ' | grep -q '`NEXT` is 2 even under `REVISE_ROUNDS_MAX=1`'; then pass "$t"; else fail "$t"; fi

echo "TEST: the park never asks for triage on a dispute, and the issue body is never a remedy (AC 6)"
# Issue #78 replaced the "awaiting a product decision" park with the Ruling
# request exit: the only AFK:revise-failed outcome left is fixes still failing
# at the cap; a decision that awaits the human carries AFK:ruling instead.
t="no escalate reply in §2 says human triage; AFK:revise-failed is only the cap outcome, and a pending decision exits as a Ruling request under AFK:ruling"
sec2_text="$(between "${sec2}" "${sec3}" | tr '\n' ' ' | tr -s ' ')"
if ! printf '%s' "${sec2_text}" | grep -qi 'human triage' && printf '%s' "${sec2_text}" | grep -q 'AFK:revise-failed: fixes still failing at the round cap' && ! printf '%s' "${sec2_text}" | grep -q 'AFK:revise-failed: awaiting a product decision' && printf '%s' "${sec2_text}" | grep -q 'never apply `AFK:revise-failed` on this exit' && printf '%s' "${sec2_text}" | grep -q 'add-label AFK:ruling'; then pass "$t"; else fail "$t"; fi
t="the PR body is named a remedy and the issue body / AC are not, and no gh issue edit --body appears anywhere in the runbook"
if printf '%s' "${sec2_text}" | grep -q 'may edit the PR body' && printf '%s' "${sec2_text}" | grep -q 'issue body and the Acceptance Criteria are not' && ! grep -Eq 'gh issue edit [^`]{0,80}--body' "${SKILL}"; then pass "$t"; else fail "$t"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
