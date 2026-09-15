#!/usr/bin/env bash
# Tests for lib/deps-lane.sh (the marker vocabulary and fix budget)
#
# Run: bash lib/deps-lane.test.sh
#
# Strategy: the marker functions are pure text (args and stdin in, stdout
# out), so every test calls the sourced functions directly with no stubs and
# no network. The fix cap arrives as the Harness config's rounds.deps_fix
# through HARNESS_CONFIG_JSON. What the lane risks is a parser that reads a
# stale sha's verdict as vouching for the current head, or a budget that never
# trips, so the assertions are about exact-sha matching and the cap.
#
# The marker cases are Smart-Smoker-V2's own; the cap cases read the config.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/deps-lane.sh"
# shellcheck source=deps-lane.sh
. "${LIB}"

# The resolved config every test runs under: a fix cap of 3.
cfg_with_cap() { printf '{"repo":{"slug":"acme/widgets"},"pick":{"shape":"labels"},"rounds":{"deps_fix":%s}}' "$1"; }
export HARNESS_CONFIG_JSON
HARNESS_CONFIG_JSON="$(cfg_with_cap 3)"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

#-------------------------------------------------------------------------------
# Test 6: emit prints exactly one hidden HTML comment per state, carrying the
#         sha, and refuses an unknown state (behavior 3; AC 3). One line, always
#         a comment: the marker is the lane's memory, and it has to be invisible
#         in the rendered PR while staying greppable in the raw body.
#-------------------------------------------------------------------------------
test_marker_emit_per_state() {
    echo "TEST: marker emit prints one hidden comment per state"

    local sha='deadbeefcafe1234567890abcdef0123456789ab'
    local out rc

    out="$(deps_lane_marker_emit tierA "${sha}")"
    if [ "${out}" != "<!-- deps-lane tierA=green sha=${sha} -->" ]; then
        fail "tierA marker shape" "got: ${out}"
        return
    fi
    if [ "$(printf '%s\n' "${out}" | wc -l)" != "1" ]; then
        fail "tierA marker must be one line" "got: ${out}"
        return
    fi

    out="$(deps_lane_marker_emit tierB "${sha}")"
    if [ "${out}" != "<!-- deps-lane tierB=PASS sha=${sha} -->" ]; then
        fail "tierB marker shape" "got: ${out}"
        return
    fi

    out="$(deps_lane_marker_emit fix-attempt "${sha}" 2)"
    if [ "${out}" != "<!-- deps-lane fix-attempt=2 sha=${sha} -->" ]; then
        fail "fix-attempt marker shape" "got: ${out}"
        return
    fi

    # An unknown state is a caller bug; it must fail loudly rather than emit a
    # marker the parser will never read.
    out="$(deps_lane_marker_emit tierC "${sha}" 2>/dev/null)"; rc=$?
    if [ "${rc}" -eq 0 ]; then
        fail "an unknown state must exit non-zero" "got rc=${rc} out=${out}"
        return
    fi
    if [ -n "${out}" ]; then
        fail "an unknown state must print no marker on stdout" "got: ${out}"
        return
    fi

    # fix-attempt without N is equally a caller bug: an attempt marker with no
    # number cannot be counted.
    out="$(deps_lane_marker_emit fix-attempt "${sha}" 2>/dev/null)"; rc=$?
    if [ "${rc}" -eq 0 ]; then
        fail "fix-attempt without N must exit non-zero" "got rc=${rc} out=${out}"
        return
    fi

    pass "marker emit prints one hidden comment per state"
}

#-------------------------------------------------------------------------------
# Test 7: parse round-trips every emitted state for the sha it was emitted for
#         (behavior 3; AC 3). Emit and parse are one contract; if they drift the
#         lane forgets what it did and re-runs tier B (or re-fires a fix) on a
#         PR it already handled.
#-------------------------------------------------------------------------------
test_marker_parse_round_trips() {
    echo "TEST: parse round-trips each emitted marker state"

    local sha='deadbeefcafe1234567890abcdef0123456789ab'
    local out

    out="$( { deps_lane_marker_emit tierA "${sha}"; deps_lane_marker_emit tierB "${sha}"; } \
        | deps_lane_marker_parse "${sha}")"

    if [ "$(printf '%s' "${out}" | jq -r '.sha')" != "${sha}" ]; then
        fail "parse must echo the sha it was asked about" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.tierA')" != "true" ]; then
        fail "an emitted tierA marker must parse as true" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.tierB')" != "true" ]; then
        fail "an emitted tierB marker must parse as true" "got: ${out}"
        return
    fi

    # A marker buried inside a real comment body — prose above and below — is
    # still the same marker.
    out="$(printf 'Tier A is green for this bump.\n\n%s\n\nRe-run with /afk-pickup.\n' \
        "$(deps_lane_marker_emit tierA "${sha}")" | deps_lane_marker_parse "${sha}")"
    if [ "$(printf '%s' "${out}" | jq -r '.tierA')" != "true" ]; then
        fail "a marker inside a prose comment must still be found" "got: ${out}"
        return
    fi

    pass "parse round-trips each emitted marker state"
}

#-------------------------------------------------------------------------------
# Test 8: with no markers at all, every verdict is false and the attempt count
#         is zero (behavior 3; AC 3). This is the first-touch state of every PR
#         and the fail-safe direction: absent evidence never vouches for a bump.
#-------------------------------------------------------------------------------
test_marker_parse_without_markers() {
    echo "TEST: parse with no markers reports nothing green"

    local out
    out="$(printf 'Just a human comment, no markers here.\n' \
        | deps_lane_marker_parse 'deadbeefcafe1234567890abcdef0123456789ab')"

    if [ "$(printf '%s' "${out}" | jq -r '.tierA')" != "false" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.tierB')" != "false" ]; then
        fail "no markers must mean no green tiers" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.fixAttempts')" != "0" ]; then
        fail "no markers must mean zero fix attempts" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.capReached')" != "false" ]; then
        fail "zero attempts must not reach the cap" "got: ${out}"
        return
    fi

    pass "parse with no markers reports nothing green"
}

#-------------------------------------------------------------------------------
# Test 9: markers from another sha are ignored entirely (behavior 3; AC 3).
#         This is the whole reason markers are sha-keyed: an old push's tier B
#         PASS and its fix attempts must not vouch for — or count against — the
#         code that would actually merge now.
#-------------------------------------------------------------------------------
test_marker_parse_ignores_other_shas() {
    echo "TEST: parse ignores markers from another sha"

    local sha='1111111111111111111111111111111111111111'
    local old='2222222222222222222222222222222222222222'
    local comments out

    comments="$( {
        deps_lane_marker_emit tierA "${old}"
        deps_lane_marker_emit tierB "${old}"
        deps_lane_marker_emit fix-attempt "${old}" 1
        deps_lane_marker_emit fix-attempt "${old}" 2
        deps_lane_marker_emit tierA "${sha}"
    } )"

    out="$(printf '%s\n' "${comments}" | deps_lane_marker_parse "${sha}")"

    if [ "$(printf '%s' "${out}" | jq -r '.tierA')" != "true" ]; then
        fail "this sha's tierA marker must be seen" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.tierB')" != "false" ]; then
        fail "another sha's tierB must not vouch for this sha" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.fixAttempts')" != "0" ]; then
        fail "another sha's fix attempts must not be counted" "got: ${out}"
        return
    fi

    pass "parse ignores markers from another sha"
}

#-------------------------------------------------------------------------------
# Test 10: fix attempts are counted for this sha and the cap is reported
#          (behavior 3; AC 3). The cap is what stops the lane grinding forever
#          on a bump it cannot fix: at 3 attempts it must hand over, at 2 it may
#          still try. The cap is the config's rounds.deps_fix.
#-------------------------------------------------------------------------------
test_marker_parse_counts_attempts_and_cap() {
    echo "TEST: parse counts fix attempts and reports the cap"

    local sha='1111111111111111111111111111111111111111'
    local two three out

    two="$( { deps_lane_marker_emit fix-attempt "${sha}" 1
              deps_lane_marker_emit fix-attempt "${sha}" 2; } )"
    three="$( printf '%s\n' "${two}"; deps_lane_marker_emit fix-attempt "${sha}" 3 )"

    out="$(printf '%s\n' "${two}" | deps_lane_marker_parse "${sha}")"
    if [ "$(printf '%s' "${out}" | jq -r '.fixAttempts')" != "2" ]; then
        fail "two attempt markers must count 2" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.capReached')" != "false" ]; then
        fail "two attempts must not reach the default cap of 3" "got: ${out}"
        return
    fi

    out="$(printf '%s\n' "${three}" | deps_lane_marker_parse "${sha}")"
    if [ "$(printf '%s' "${out}" | jq -r '.fixAttempts')" != "3" ]; then
        fail "three attempt markers must count 3" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.capReached')" != "true" ]; then
        fail "three attempts must reach the default cap" "got: ${out}"
        return
    fi

    pass "parse counts fix attempts and reports the cap"
}

#-------------------------------------------------------------------------------
#-------------------------------------------------------------------------------
# Test 13: sha matching is exact, never a prefix, in both directions (behavior 3;
#          AC 3). Short shas are everywhere in this repo's tooling (`git rev-parse
#          --short`, PR titles, log lines), so a substring match would let a
#          7-character sha collect the verdicts of every long sha it happens to
#          prefix — exactly the stale-verdict merge the sha key exists to stop.
#-------------------------------------------------------------------------------
test_marker_parse_requires_exact_sha() {
    echo "TEST: parse matches the sha exactly, not by prefix"

    local short='abc123' long='abc1234567'
    local out

    # Markers written for the LONG sha must not vouch for the SHORT one.
    out="$( { deps_lane_marker_emit tierA "${long}"
              deps_lane_marker_emit tierB "${long}"
              deps_lane_marker_emit fix-attempt "${long}" 1; } \
        | deps_lane_marker_parse "${short}")"
    if [ "$(printf '%s' "${out}" | jq -r '.tierA')" != "false" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.tierB')" != "false" ]; then
        fail "a longer sha's markers must not vouch for its prefix" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.fixAttempts')" != "0" ]; then
        fail "a longer sha's attempts must not be counted for its prefix" "got: ${out}"
        return
    fi

    # And the reverse: markers written for the SHORT sha must not vouch for the
    # LONG one either.
    out="$( { deps_lane_marker_emit tierA "${short}"
              deps_lane_marker_emit tierB "${short}"
              deps_lane_marker_emit fix-attempt "${short}" 1; } \
        | deps_lane_marker_parse "${long}")"
    if [ "$(printf '%s' "${out}" | jq -r '.tierA')" != "false" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.tierB')" != "false" ]; then
        fail "a prefix sha's markers must not vouch for the longer sha" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.fixAttempts')" != "0" ]; then
        fail "a prefix sha's attempts must not be counted for the longer sha" "got: ${out}"
        return
    fi

    pass "parse matches the sha exactly, not by prefix"
}

#-------------------------------------------------------------------------------
# Test 16: parse refuses an empty sha instead of answering with an all-false
#          verdict (behavior 3; AC 3). A caller whose head-sha lookup came back
#          empty would otherwise read capReached=false on every fire — markers
#          nobody looked for — so the cap would never trip and the fix
#          loop would retry the same bot PR forever. Emit already refuses the
#          same input; parse must agree.
#-------------------------------------------------------------------------------
test_marker_parse_rejects_empty_sha() {
    echo "TEST: parse refuses an empty sha"

    local out rc
    out="$(deps_lane_marker_parse '' < /dev/null 2>/dev/null)"; rc=$?
    if [ "${rc}" -eq 0 ]; then
        fail "an empty sha must exit non-zero" "rc=${rc} out=${out}"
        return
    fi
    if [ -n "${out}" ]; then
        fail "an empty sha must print no verdict on stdout" "got: ${out}"
        return
    fi

    pass "parse refuses an empty sha"
}

#-------------------------------------------------------------------------------
# Test 17: a failing (or missing) jq is propagated, not masked (behavior 3;
#          AC 3). The documented contract is a well-formed verdict; empty stdout
#          with rc=0 is not one. Downstream, `jq -r '.capReached'` on empty input
#          prints nothing and exits 0, so every field would read as not-true and
#          the lane would silently re-run tiers and blow past the fix cap on a
#          runner that merely lacks jq.
#-------------------------------------------------------------------------------
test_marker_parse_propagates_jq_failure() {
    echo "TEST: parse propagates a jq failure instead of printing nothing at rc=0"

    local out rc
    out="$(bash -c '
        . "$1"
        jq() { echo "jq: command not found" >&2; return 127; }
        printf "" | deps_lane_marker_parse "$2"
    ' _ "${LIB}" '1111111111111111111111111111111111111111' 2>/dev/null)"; rc=$?

    if [ "${rc}" -eq 0 ]; then
        fail "a failing jq must make parse exit non-zero" "rc=${rc} out=${out}"
        return
    fi
    if [ -n "${out}" ]; then
        fail "a failing jq must not leave a half-verdict on stdout" "got: ${out}"
        return
    fi

    pass "parse propagates a jq failure instead of printing nothing at rc=0"
}

#-------------------------------------------------------------------------------
# Test 18: rounds-left turns the recorded attempt count into the fix budget this
#          fire may still spend (behavior 2; AC 1). pr-watch --bot asks for it
#          once, before it polls anything: at the cap the answer must be exactly
#          0 so the lane goes straight to the draft + AFK:deps-failed path
#          instead of burning a CI wait it has already decided to abandon. It
#          never goes negative — a PR that somehow carries more markers than the
#          cap (a re-fire that raced, a hand-pasted marker) must read as "no
#          budget", not as a negative that a `[ "$N" -gt 0 ]` caller would still
#          treat as false but a `for` loop would count down from.
#-------------------------------------------------------------------------------
test_rounds_left() {
    echo "TEST: rounds-left is the cap minus the recorded attempts, floored at 0"

    local out rc
    local -a cases=("0 3" "1 2" "2 1" "3 0" "4 0" "9 0")
    local case_line recorded want
    for case_line in "${cases[@]}"; do
        recorded="${case_line%% *}"
        want="${case_line##* }"
        out="$(deps_lane_rounds_left "${recorded}")"; rc=$?
        if [ "${rc}" -ne 0 ] || [ "${out}" != "${want}" ]; then
            fail "rounds-left ${recorded} must be ${want}" "rc=${rc} got: ${out}"
            return
        fi
    done

    pass "rounds-left is the cap minus the recorded attempts, floored at 0"
}

#-------------------------------------------------------------------------------
# Test 19: a non-numeric (or missing) attempt count is refused, loudly, with
#          nothing on stdout (behavior 2; AC 1). The caller substitutes this
#          straight into `MAX_ROUNDS=$(… rounds-left "$fixAttempts")`, and
#          fixAttempts arrives from a jq read that prints `null` — or nothing at
#          all — when the marker parse failed. Answering that with the full cap
#          would hand a PR whose history could not be read the largest possible
#          fix budget, which is precisely backwards; answering with an empty
#          string would make the caller's `-eq 0` test a syntax error mid-lane.
#-------------------------------------------------------------------------------
test_rounds_left_rejects_non_numeric() {
    echo "TEST: rounds-left refuses a non-numeric attempt count"

    local out rc bad
    for bad in 'null' '' 'two' '-1' '2.5' '3 '; do
        out="$(deps_lane_rounds_left "${bad}" 2>/dev/null)"; rc=$?
        if [ "${rc}" -eq 0 ]; then
            fail "rounds-left must refuse '${bad}'" "rc=${rc} out=${out}"
            return
        fi
        if [ -n "${out}" ]; then
            fail "a refused count must print no budget" "'${bad}' gave: ${out}"
            return
        fi
    done

    pass "rounds-left refuses a non-numeric attempt count"
}


#-------------------------------------------------------------------------------
# Test: the cap is the config's rounds.deps_fix, the only source. With no
#       resolvable config the two cap readers refuse (rc 2, no stdout) rather
#       than guess a budget.
#-------------------------------------------------------------------------------
test_cap_comes_from_config() {
    echo "TEST: the fix cap is the Harness config's rounds.deps_fix"

    local sha='1111111111111111111111111111111111111111' out rc
    local two
    two="$( { deps_lane_marker_emit fix-attempt "${sha}" 1
              deps_lane_marker_emit fix-attempt "${sha}" 2; } )"

    out="$(printf '%s\n' "${two}" | HARNESS_CONFIG_JSON="$(cfg_with_cap 2)" deps_lane_marker_parse "${sha}")"
    if [ "$(printf '%s' "${out}" | jq -r '.capReached')" != "true" ]; then
        fail "a config cap of 2 must trip at two attempts" "got: ${out}"
        return
    fi
    out="$(HARNESS_CONFIG_JSON="$(cfg_with_cap 5)" deps_lane_rounds_left 2)"
    if [ "${out}" != "3" ]; then
        fail "rounds-left reads the same config cap" "got: ${out}"
        return
    fi

    # An env override is NOT a source: the config is the only one (ADR 0002).
    out="$(printf '%s\n' "${two}" | DEPS_LANE_FIX_CAP=2 deps_lane_marker_parse "${sha}")"
    if [ "$(printf '%s' "${out}" | jq -r '.capReached')" != "false" ]; then
        fail "DEPS_LANE_FIX_CAP must not override the config" "got: ${out}"
        return
    fi

    out="$(printf '%s\n' "${two}" | HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= deps_lane_marker_parse "${sha}" 2>/dev/null)"; rc=$?
    if [ "${rc}" -ne 2 ] || [ -n "${out}" ]; then
        fail "parse without a config must refuse with rc 2 and no verdict" "rc=${rc} out=${out}"
        return
    fi
    out="$(HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= deps_lane_rounds_left 1 2>/dev/null)"; rc=$?
    if [ "${rc}" -ne 2 ] || [ -n "${out}" ]; then
        fail "rounds-left without a config must refuse with rc 2 and no budget" "rc=${rc} out=${out}"
        return
    fi

    pass "the fix cap is the Harness config's rounds.deps_fix"
}

#-------------------------------------------------------------------------------
# Run suite
#-------------------------------------------------------------------------------
test_marker_emit_per_state
test_marker_parse_round_trips
test_marker_parse_without_markers
test_marker_parse_ignores_other_shas
test_marker_parse_counts_attempts_and_cap
test_marker_parse_requires_exact_sha
test_marker_parse_rejects_empty_sha
test_marker_parse_propagates_jq_failure
test_rounds_left
test_rounds_left_rejects_non_numeric
test_cap_comes_from_config

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
