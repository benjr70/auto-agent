#!/usr/bin/env bash
# Tests for lib/deps-lane.sh (the deps-land lane's gate, text transforms,
# marker vocabulary, fix budget and exhaustion park)
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
# The retitle, inject, marker and trailer cases are Smart-Smoker-V2's own; the
# cap, lane, checklist-from-config and park cases read the config. Park is the
# one function that calls gh, through a GH_BIN stub that logs every call.

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
# Test 1: a security PR's `chore(deps):` prefix becomes `fix(deps):` and nothing
#         else in Dependabot's title changes (behavior 1; AC 1). The prefix is
#         the whole point: a release tool reads it, so a security bump titled
#         chore never reaches a user-visible release note.
#-------------------------------------------------------------------------------
test_security_chore_becomes_fix() {
    echo "TEST: a security chore(deps) title is retitled fix(deps)"

    local out
    out="$(deps_lane_retitle 'chore(deps): bump axios from 1.6.0 to 1.6.8' true)"

    if [ "${out}" != 'fix(deps): bump axios from 1.6.0 to 1.6.8' ]; then
        fail "security chore(deps) must become fix(deps)" "got: ${out}"
        return
    fi

    pass "a security chore(deps) title is retitled fix(deps)"
}

#-------------------------------------------------------------------------------
# Test 2: everything that is not an exact security `chore(deps):` passes through
#         untouched (behavior 1; AC 1). Retitling is a mutation on someone
#         else's PR, so the transform must be conservative: a routine bump, an
#         already-promoted title, a dev-dependency prefix and a non-conventional
#         prefix are all returned byte-for-byte.
#-------------------------------------------------------------------------------
test_non_matching_titles_pass_through() {
    echo "TEST: non-security and foreign-prefix titles pass through unchanged"

    local title out
    title='chore(deps): bump axios from 1.6.0 to 1.6.8'
    out="$(deps_lane_retitle "${title}" false)"
    if [ "${out}" != "${title}" ]; then
        fail "a non-security title must pass through" "got: ${out}"
        return
    fi

    title='fix(deps): bump axios from 1.6.0 to 1.6.8'
    out="$(deps_lane_retitle "${title}" true)"
    if [ "${out}" != "${title}" ]; then
        fail "an already-promoted title must pass through" "got: ${out}"
        return
    fi

    title='chore(deps-dev): bump jest from 29.0.0 to 29.7.0'
    out="$(deps_lane_retitle "${title}" true)"
    if [ "${out}" != "${title}" ]; then
        fail "a dev-dependency prefix must pass through" "got: ${out}"
        return
    fi

    title='build: bump the docker base image'
    out="$(deps_lane_retitle "${title}" true)"
    if [ "${out}" != "${title}" ]; then
        fail "a non-deps prefix must pass through" "got: ${out}"
        return
    fi

    pass "non-security and foreign-prefix titles pass through unchanged"
}

#-------------------------------------------------------------------------------
# Test 3: retitling is idempotent, carries multi-dependency group text through,
#         and reads the title from stdin when only the security flag is given
#         (behavior 1; AC 1). Idempotence matters because the lane may re-run on
#         the same PR after a rebase or a re-fire: applying twice must equal
#         applying once, never `fix(deps): fix(deps): …`.
#-------------------------------------------------------------------------------
test_retitle_is_idempotent_and_reads_stdin() {
    echo "TEST: retitle is idempotent, group-safe and stdin-capable"

    local once twice group out
    once="$(deps_lane_retitle 'chore(deps): bump axios from 1.6.0 to 1.6.8' true)"
    twice="$(deps_lane_retitle "${once}" true)"
    if [ "${twice}" != "${once}" ]; then
        fail "applying retitle twice must equal applying it once" \
            "once=${once} twice=${twice}"
        return
    fi

    # Dependabot's grouped updates carry no versions at all — the remaining text
    # must survive verbatim, prefix aside.
    group='chore(deps): bump the npm-production group across 4 directories with 7 updates'
    out="$(deps_lane_retitle "${group}" true)"
    if [ "${out}" != "fix(deps): bump the npm-production group across 4 directories with 7 updates" ]; then
        fail "a grouped title must keep its text" "got: ${out}"
        return
    fi

    out="$(printf '%s' 'chore(deps): bump axios from 1.6.0 to 1.6.8' | deps_lane_retitle true)"
    if [ "${out}" != 'fix(deps): bump axios from 1.6.0 to 1.6.8' ]; then
        fail "retitle must accept the title on stdin" "got: ${out}"
        return
    fi

    pass "retitle is idempotent, group-safe and stdin-capable"
}

#-------------------------------------------------------------------------------
# make_checklist: write a fixture checklist into a fresh temp dir and echo its
# path. Shaped like a Target Project `bot-pr-checklist.md` that carries its own markers: opening
# marker, the manual-verification items, a closing marker, and — critically —
# maintainer notes AFTER the closing marker that must never reach a PR body.
#-------------------------------------------------------------------------------
make_checklist() {
    local dir; dir="$(mktemp -d)"
    cat > "${dir}/bot-pr-checklist.md" <<'CHECKLIST'
<!-- bot-pr-checklist v1 -->

## Manual verification

Tier B could not vouch for this PR on its own; confirm the items below by hand.

- [ ] Stack healthy: backend /health + /ready, device /health, frontend all 200
- [ ] Electron launches at 800x480, renders the Smoke screen, no console errors
- [ ] Live temps stream from the device service to the chart
- [ ] A smoke can be started, stopped and saved to history
- [ ] Settings round-trip through the backend
- [ ] No new errors in any container log

<!-- /bot-pr-checklist -->

Maintainer notes: keep the items above in sync with the smoke suite. These
notes are for humans editing this file and must never be injected into a PR.
CHECKLIST
    echo "${dir}/bot-pr-checklist.md"
}

#-------------------------------------------------------------------------------
# Test 4: injecting into a body without the marker appends the injected unit —
#         opening marker through closing marker — and stops there (behavior 2;
#         AC 2). An empty body yields just the unit; a body with Dependabot's
#         own sections keeps them and gains the checklist below. The maintainer
#         notes living after the closing marker must be absent both times:
#         they are instructions for whoever edits the checklist, and pasting
#         them onto every bot PR would be noise a reviewer has to read past.
#-------------------------------------------------------------------------------
test_inject_appends_unit_once() {
    echo "TEST: inject appends the checklist unit to a body lacking the marker"

    local checklist out
    checklist="$(make_checklist)"

    out="$(printf '' | deps_lane_inject_checklist "${checklist}")"
    if ! printf '%s' "${out}" | grep -qF '<!-- bot-pr-checklist v1 -->'; then
        fail "empty body must gain the opening marker" "got: ${out}"
        return
    fi
    if ! printf '%s' "${out}" | grep -qF '<!-- /bot-pr-checklist -->'; then
        fail "empty body must gain the closing marker" "got: ${out}"
        return
    fi
    if printf '%s' "${out}" | grep -qF 'Maintainer notes'; then
        fail "maintainer notes after the closing marker must not be injected" \
            "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | head -1)" != '<!-- bot-pr-checklist v1 -->' ]; then
        fail "an empty body must yield just the unit" "got first line: $(printf '%s' "${out}" | head -1)"
        return
    fi

    local body
    body='Bumps [axios](https://github.com/axios/axios) from 1.6.0 to 1.6.8.

## Release notes

Sourced from axios releases.'
    out="$(printf '%s\n' "${body}" | deps_lane_inject_checklist "${checklist}")"

    if ! printf '%s' "${out}" | grep -qF '## Release notes'; then
        fail "inject must keep the existing body sections" "got: ${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | grep -cF '<!-- bot-pr-checklist v1 -->')" != "1" ]; then
        fail "the opening marker must appear exactly once" "got: ${out}"
        return
    fi
    if printf '%s' "${out}" | grep -qF 'Maintainer notes'; then
        fail "maintainer notes must not be injected into a real body" "got: ${out}"
        return
    fi
    if ! printf '%s' "${out}" | grep -qF -e '- [ ] Stack healthy'; then
        fail "the checklist items must be injected" "got: ${out}"
        return
    fi

    rm -rf "$(dirname "${checklist}")"
    pass "inject appends the checklist unit to a body lacking the marker"
}

#-------------------------------------------------------------------------------
# Test 5: a body that already carries the marker comes back byte-identical, and
#         injecting twice equals injecting once (behavior 2; AC 2). The ticked
#         boxes are the point: they are a maintainer's record of manual
#         verification, and the second inject must not reset them or reflow a
#         single byte of the body. Compared with `cmp`, so trailing-newline
#         mangling fails the test rather than passing a lenient match.
#-------------------------------------------------------------------------------
test_inject_is_byte_level_noop_when_marker_present() {
    echo "TEST: inject is a byte-level no-op on a body carrying the marker"

    local checklist dir
    checklist="$(make_checklist)"
    dir="$(mktemp -d)"

    # A body already injected and then hand-ticked by a maintainer. No trailing
    # newline, on purpose: GitHub bodies often arrive without one.
    printf '%s' 'Bumps axios from 1.6.0 to 1.6.8.

<!-- bot-pr-checklist v1 -->

## Manual verification

- [x] Stack healthy: backend /health + /ready, device /health, frontend all 200
- [x] Electron launches at 800x480, renders the Smoke screen, no console errors

<!-- /bot-pr-checklist -->' > "${dir}/body"

    deps_lane_inject_checklist "${checklist}" < "${dir}/body" > "${dir}/once"

    if ! cmp -s "${dir}/body" "${dir}/once"; then
        fail "a body carrying the marker must be returned byte-identical" \
            "$(diff "${dir}/body" "${dir}/once" | head -5)"
        rm -rf "${dir}" "$(dirname "${checklist}")"
        return
    fi

    # And the fresh-inject path is itself idempotent: inject, then inject again.
    printf '%s\n' 'Bumps axios from 1.6.0 to 1.6.8.' > "${dir}/plain"
    deps_lane_inject_checklist "${checklist}" < "${dir}/plain" > "${dir}/first"
    deps_lane_inject_checklist "${checklist}" < "${dir}/first" > "${dir}/second"

    if ! cmp -s "${dir}/first" "${dir}/second"; then
        fail "injecting twice must equal injecting once" \
            "$(diff "${dir}/first" "${dir}/second" | head -5)"
        rm -rf "${dir}" "$(dirname "${checklist}")"
        return
    fi

    if [ "$(grep -cF '<!-- bot-pr-checklist v1 -->' "${dir}/second")" != "1" ]; then
        fail "a twice-injected body must carry exactly one checklist" \
            "$(cat "${dir}/second")"
        rm -rf "${dir}" "$(dirname "${checklist}")"
        return
    fi

    rm -rf "${dir}" "$(dirname "${checklist}")"
    pass "inject is a byte-level no-op on a body carrying the marker"
}

#-------------------------------------------------------------------------------
# Test 6: emit prints exactly one hidden HTML comment per state, carrying the
#         sha, and refuses an unknown state (behavior 3; AC 3). One line, always
#         a comment: the marker is the lane's memory, and it has to be invisible
#         in the rendered PR while staying greppable in the raw body.
#-------------------------------------------------------------------------------
# Test 12: the security flag must be exactly `true` or `false`; anything else is
#          refused, loudly, with nothing on stdout (behavior 1; AC 1). The trap
#          this guards is the one-arg misuse `retitle "<title>"`, where the title
#          lands in the flag position: without the check the function would read
#          a title from stdin that nobody is piping — printing an empty line (so
#          the caller cheerfully renames the PR to nothing) or blocking forever
#          on a tty. A rejected flag must never produce a title.
#-------------------------------------------------------------------------------
test_retitle_rejects_bad_security_flag() {
    echo "TEST: retitle refuses a security flag that is not true/false"

    local out rc bad
    for bad in 'chore(deps): bump axios from 1.6.0 to 1.6.8' 'True' 'TRUE' '1' 'yes' ''; do
        out="$(deps_lane_retitle "${bad}" </dev/null 2>/dev/null)"; rc=$?
        if [ "${rc}" -eq 0 ]; then
            fail "flag '${bad}' must exit non-zero" "rc=${rc} out=${out}"
            return
        fi
        if [ -n "${out}" ]; then
            fail "flag '${bad}' must print nothing on stdout" "got: ${out}"
            return
        fi
    done

    # Same guard in the two-argument form: a typo'd flag must not silently mean
    # "not a security update" and pass the title through unpromoted.
    out="$(deps_lane_retitle 'chore(deps): bump axios' 'True' 2>/dev/null)"; rc=$?
    if [ "${rc}" -eq 0 ] || [ -n "${out}" ]; then
        fail "a typo'd flag in the 2-arg form must be refused" "rc=${rc} out=${out}"
        return
    fi

    # And through the CLI, which is where the misuse actually happens.
    out="$(bash "${LIB}" retitle 'chore(deps): bump axios' </dev/null 2>/dev/null)"; rc=$?
    if [ "${rc}" -eq 0 ] || [ -n "${out}" ]; then
        fail "the CLI must refuse a missing security flag" "rc=${rc} out=${out}"
        return
    fi

    # The valid flags still work in both arities.
    out="$(deps_lane_retitle 'chore(deps): bump axios' false)"
    if [ "${out}" != 'chore(deps): bump axios' ]; then
        fail "false must still pass the title through" "got: ${out}"
        return
    fi
    out="$(printf '%s' 'chore(deps): bump axios' | deps_lane_retitle true)"
    if [ "${out}" != 'fix(deps): bump axios' ]; then
        fail "true must still promote from stdin" "got: ${out}"
        return
    fi

    pass "retitle refuses a security flag that is not true/false"
}

#-------------------------------------------------------------------------------
# Test 13: sha matching is exact, never a prefix, in both directions (behavior 3;
#          AC 3). Short shas are everywhere in this repo's tooling (`git rev-parse
#          --short`, PR titles, log lines), so a substring match would let a
#          7-character sha collect the verdicts of every long sha it happens to
#          prefix — exactly the stale-verdict merge the sha key exists to stop.
#-------------------------------------------------------------------------------
# Test 14: inject leaves nothing behind in the shell that sourced it, and no
#          temp files on disk (behavior 2; AC 4). A sourceable lib runs inside
#          someone else's shell: a stray RETURN trap (the obvious way to clean
#          up the body buffer) is NOT function-scoped without `set -o functrace`
#          and would keep firing on every later function return in the caller.
#-------------------------------------------------------------------------------
test_inject_leaves_no_trace() {
    echo "TEST: inject leaks neither a trap nor a temp file"

    local checklist dir tmphome leaked
    checklist="$(make_checklist)"
    dir="$(mktemp -d)"
    printf 'Bumps axios.\n' > "${dir}/body"

    # Point inject's mktemp at a private TMPDIR and count what is left in THERE.
    # Counting the shared /tmp instead would make this assertion a race: any
    # other process on the box (another agent team, a CI step, an editor)
    # creating a `tmp.*` file mid-test would report a leak inject did not cause.
    tmphome="$(mktemp -d)"
    TMPDIR="${tmphome}" deps_lane_inject_checklist "${checklist}" < "${dir}/body" > /dev/null
    TMPDIR="${tmphome}" deps_lane_inject_checklist "${checklist}" < "${dir}/body" > /dev/null
    leaked="$(find "${tmphome}" -mindepth 1 2>/dev/null | wc -l)"

    if [ "${leaked}" -ne 0 ]; then
        fail "inject must not leave temp files behind" \
            "left in TMPDIR: $(find "${tmphome}" -mindepth 1 | tr '\n' ' ')"
        rm -rf "${dir}" "${tmphome}" "$(dirname "${checklist}")"
        return
    fi

    if [ -n "$(trap -p RETURN)" ]; then
        fail "inject must not leave a RETURN trap in the caller's shell" \
            "got: $(trap -p RETURN)"
        rm -rf "${dir}" "${tmphome}" "$(dirname "${checklist}")"
        return
    fi

    rm -rf "${dir}" "${tmphome}" "$(dirname "${checklist}")"
    pass "inject leaks neither a trap nor a temp file"
}

#-------------------------------------------------------------------------------
# Test 15: when the body buffer cannot be created or written, inject prints
#          NOTHING and fails (behavior 2; AC 2). This is the data-loss path: the
#          lane pipes inject's stdout into `gh pr edit --body-file -`, so an
#          inject that swallowed Dependabot's release notes and emitted only the
#          checklist — with rc=0 — would overwrite the PR body irrecoverably.
#          Disk pressure on this box is a recurring condition, so both halves
#          are exercised: mktemp failing outright, and mktemp handing back a
#          path that cannot be written.
#-------------------------------------------------------------------------------
test_inject_refuses_when_buffer_fails() {
    echo "TEST: inject refuses (loudly, empty) when the body buffer fails"

    local checklist out rc
    checklist="$(make_checklist)"

    # mktemp fails: a shell function shadows the external command for the
    # sourced lib, so no real disk pressure is needed to reach the path.
    out="$(bash -c '
        . "$1"
        mktemp() { return 1; }
        printf "IMPORTANT BODY\n" | deps_lane_inject_checklist "$2"
    ' _ "${LIB}" "${checklist}" 2>/dev/null)"; rc=$?

    if [ "${rc}" -eq 0 ]; then
        fail "a failed mktemp must exit non-zero" "rc=${rc} out=${out}"
        rm -rf "$(dirname "${checklist}")"
        return
    fi
    if [ -n "${out}" ]; then
        fail "a failed mktemp must print nothing (the body would be lost)" \
            "got: ${out}"
        rm -rf "$(dirname "${checklist}")"
        return
    fi

    # mktemp "succeeds" but the buffer is unwritable: the write fails, so the
    # body we could echo back is not the body we were given.
    out="$(bash -c '
        . "$1"
        mktemp() { echo "/nonexistent-dir-deps-lane/buffer"; }
        printf "IMPORTANT BODY\n" | deps_lane_inject_checklist "$2"
    ' _ "${LIB}" "${checklist}" 2>/dev/null)"; rc=$?

    if [ "${rc}" -eq 0 ]; then
        fail "an unwritable buffer must exit non-zero" "rc=${rc} out=${out}"
        rm -rf "$(dirname "${checklist}")"
        return
    fi
    if [ -n "${out}" ]; then
        fail "an unwritable buffer must print nothing" "got: ${out}"
        rm -rf "$(dirname "${checklist}")"
        return
    fi

    rm -rf "$(dirname "${checklist}")"
    pass "inject refuses (loudly, empty) when the body buffer fails"
}

#-------------------------------------------------------------------------------
# Test 20: the commit trailer appends `[dependabot skip]` once, ending the
#          message, and preserves a multi-line body (behavior 3; AC 2). Without
#          the marker, every fix commit the lane pushes makes Dependabot treat
#          the branch as human-touched and stop rebasing it — the PR then rots
#          behind master with no bot able to update it.
#-------------------------------------------------------------------------------
test_commit_trailer_appends_once() {
    echo "TEST: commit-trailer appends [dependabot skip] to the end of the message"

    local out
    out="$(printf 'fix(ci): pr-watch round 1 — auto-fix failing checks\n' \
        | deps_lane_commit_trailer)"
    if [ "${out}" != 'fix(ci): pr-watch round 1 — auto-fix failing checks [dependabot skip]' ]; then
        fail "a one-line message must end with the marker" "got: ${out}"
        return
    fi

    # The argument form is the same transform as the stdin form.
    out="$(deps_lane_commit_trailer 'fix(ci): regenerate the lockfile')"
    if [ "${out}" != 'fix(ci): regenerate the lockfile [dependabot skip]' ]; then
        fail "the argument form must append the marker too" "got: ${out}"
        return
    fi

    # A body must survive intact: the implementer's summary is the only record
    # of what the fix round actually changed.
    local msg
    msg="$(printf 'fix(ci): pr-watch round 2\n\nRegenerated package-lock.json with\n--legacy-peer-deps.\n' \
        | deps_lane_commit_trailer)"
    if [ "$(printf '%s\n' "${msg}" | head -1)" != 'fix(ci): pr-watch round 2' ]; then
        fail "the subject line must be preserved" "got: ${msg}"
        return
    fi
    if [ "$(printf '%s\n' "${msg}" | sed -n '3p')" != 'Regenerated package-lock.json with' ]; then
        fail "the body must be preserved verbatim" "got: ${msg}"
        return
    fi
    if [ "$(printf '%s\n' "${msg}" | tail -1)" != '--legacy-peer-deps. [dependabot skip]' ]; then
        fail "the marker must end a multi-line message" "got: ${msg}"
        return
    fi

    pass "commit-trailer appends [dependabot skip] to the end of the message"
}

#-------------------------------------------------------------------------------
# Test 21: the trailer is idempotent, exits with exactly one trailing newline,
#          and refuses an empty message (behavior 3; AC 2). Idempotence is what
#          lets the caller pipe every message through unconditionally — the fix
#          loop re-commits amended messages across rounds, and a second
#          `[dependabot skip] [dependabot skip]` in a subject would break the
#          repo's conventional-commit title lint on the squash.
#-------------------------------------------------------------------------------
test_commit_trailer_is_idempotent() {
    echo "TEST: commit-trailer is idempotent, newline-sane and refuses empty"

    local once twice rc
    once="$(printf 'fix(ci): round 1\n' | deps_lane_commit_trailer)"
    twice="$(printf '%s\n' "${once}" | deps_lane_commit_trailer)"
    if [ "${twice}" != "${once}" ]; then
        fail "applying the trailer twice must equal applying it once" \
            "once=${once} twice=${twice}"
        return
    fi

    # Exactly one trailing newline, whether or not the input carried one — a
    # byte-level assertion, because `git commit -F -` reproduces the file it is
    # given, so a message with three trailing blank lines commits three trailing
    # blank lines. Compared with cmp for the same reason the inject no-op test
    # is: "close enough" is not a contract.
    local dir want
    dir="$(mktemp -d)"
    printf 'fix(ci): round 1 [dependabot skip]\n' > "${dir}/want"
    printf 'fix(ci): round 1\n\n\n' | deps_lane_commit_trailer > "${dir}/got"
    if ! cmp -s "${dir}/want" "${dir}/got"; then
        fail "the output must end with exactly one newline" \
            "got: $(od -c "${dir}/got" | tr '\n' ' ')"
        rm -rf "${dir}"
        return
    fi
    printf 'fix(ci): round 1' | deps_lane_commit_trailer > "${dir}/got"
    if ! cmp -s "${dir}/want" "${dir}/got"; then
        fail "a newline-less message must still end with one newline" \
            "got: $(od -c "${dir}/got" | tr '\n' ' ')"
        rm -rf "${dir}"
        return
    fi
    rm -rf "${dir}"

    # Trailing whitespace after an existing marker must NOT earn a second one.
    # A message that has been round-tripped through an editor, a `gh` body or a
    # here-doc routinely picks up a trailing space, and the caller pipes every
    # message through this transform on every round.
    local spaced
    spaced="$(printf 'fix(ci): round 1 [dependabot skip] \n' | deps_lane_commit_trailer)"
    if [ "${spaced}" != 'fix(ci): round 1 [dependabot skip]' ]; then
        fail "trailing whitespace after the marker must not append a second" \
            "got: ${spaced}"
        return
    fi

    # Same for a marker that is not the very last token of the line: it is
    # already there, and a second copy is what breaks the squash title lint.
    local midline
    midline="$(deps_lane_commit_trailer 'fix(ci): keep [dependabot skip] in the subject')"
    if [ "${midline}" != 'fix(ci): keep [dependabot skip] in the subject' ]; then
        fail "a marker anywhere in the last line counts as present" \
            "got: ${midline}"
        return
    fi

    # And a doubled marker is never manufactured from one that repeats.
    local doubled
    doubled="$(deps_lane_commit_trailer 'fix(ci): [dependabot skip] [dependabot skip]')"
    if [ "${doubled}" != 'fix(ci): [dependabot skip] [dependabot skip]' ]; then
        fail "a repeated marker must not gain a third" "got: ${doubled}"
        return
    fi

    # An empty message is a caller bug, not a message to decorate: a commit
    # whose whole subject is `[dependabot skip]` says nothing about the fix.
    local out
    out="$(printf '' | deps_lane_commit_trailer 2>/dev/null)"; rc=$?
    if [ "${rc}" -eq 0 ] || [ -n "${out}" ]; then
        fail "an empty message must be refused with no stdout" "rc=${rc} out=${out}"
        return
    fi

    pass "commit-trailer is idempotent, newline-sane and refuses empty"
}


#-------------------------------------------------------------------------------
# make_prose_checklist: a Target Project's `bot-pr-checklist.md` as a maintainer
# writes it — plain prose, no harness markers. The harness owns the marker
# vocabulary (the checklist protocol is harness-owned, ADR 0003), so the lane
# wraps the maintainer's prose rather than asking every Target Project to spell
# the markers.
#-------------------------------------------------------------------------------
make_prose_checklist() {
    local dir; dir="$(mktemp -d)"
    cat > "${dir}/bot-pr-checklist.md" <<'CHECKLIST'
## Manual verification

Run against the hermetic environment only.

- [ ] The test suite still passes after the bump
- [ ] /api/health answers on a fresh up
CHECKLIST
    echo "${dir}/bot-pr-checklist.md"
}

#-------------------------------------------------------------------------------
# The checklist prose is the Target Project's markdown sibling, pasted verbatim
# inside the harness's markers (AC 3). A file without markers is wrapped whole;
# the unit is still idempotent by the opening marker.
#-------------------------------------------------------------------------------
test_inject_wraps_plain_prose_verbatim() {
    echo "TEST: inject wraps a marker-less checklist verbatim in the harness markers"

    local checklist dir
    checklist="$(make_prose_checklist)"
    dir="$(mktemp -d)"

    printf 'Bumps axios from 1.6.0 to 1.6.8.\n' | deps_lane_inject_checklist "${checklist}" > "${dir}/out"
    {
        printf 'Bumps axios from 1.6.0 to 1.6.8.\n\n'
        printf '<!-- bot-pr-checklist v1 -->\n\n'
        cat "${checklist}"
        printf '\n<!-- /bot-pr-checklist -->\n'
    } > "${dir}/want"
    if ! cmp -s "${dir}/want" "${dir}/out"; then
        fail "a prose checklist must be pasted verbatim between the markers" \
            "$(diff "${dir}/want" "${dir}/out" | head -8)"
        rm -rf "${dir}" "$(dirname "${checklist}")"
        return
    fi

    deps_lane_inject_checklist "${checklist}" < "${dir}/out" > "${dir}/again"
    if ! cmp -s "${dir}/out" "${dir}/again"; then
        fail "a wrapped checklist must not be injected twice" \
            "$(diff "${dir}/out" "${dir}/again" | head -5)"
        rm -rf "${dir}" "$(dirname "${checklist}")"
        return
    fi

    rm -rf "${dir}" "$(dirname "${checklist}")"
    pass "inject wraps a marker-less checklist verbatim in the harness markers"
}

#-------------------------------------------------------------------------------
# With no path argument the checklist is the Harness config's
# prose.bot_pr_checklist (AC 3). A config without the sibling refuses with rc 2
# and prints nothing: an empty stdout piped into `gh pr edit --body-file -`
# would wipe the body, so the refusal must be loud and empty.
#-------------------------------------------------------------------------------
test_inject_reads_checklist_from_config() {
    echo "TEST: inject reads the checklist path from the Harness config"

    local checklist out rc
    checklist="$(make_prose_checklist)"

    out="$(printf 'Body.\n' | HARNESS_CONFIG_JSON="$(jq -c --arg p "${checklist}" \
        '.prose = {bot_pr_checklist: $p}' <<<"${HARNESS_CONFIG_JSON}")" \
        deps_lane_inject_checklist)"; rc=$?
    if [ "${rc}" -ne 0 ] || ! printf '%s' "${out}" | grep -qF -e '- [ ] /api/health answers on a fresh up'; then
        fail "the config's checklist must be injected" "rc=${rc} out=${out}"
        rm -rf "$(dirname "${checklist}")"
        return
    fi

    out="$(printf 'Body.\n' | HARNESS_CONFIG_JSON="$(jq -c '.prose = {bot_pr_checklist: null}' \
        <<<"${HARNESS_CONFIG_JSON}")" deps_lane_inject_checklist 2>/dev/null)"; rc=$?
    if [ "${rc}" -ne 2 ] || [ -n "${out}" ]; then
        fail "no bot-pr-checklist.md sibling must refuse with rc 2 and no stdout" "rc=${rc} out=${out}"
        rm -rf "$(dirname "${checklist}")"
        return
    fi

    rm -rf "$(dirname "${checklist}")"
    pass "inject reads the checklist path from the Harness config"
}

#-------------------------------------------------------------------------------
# A checklist with no unchecked box under a verification heading is refused:
# Tier B passes only when every item passes, and a round with zero items would
# pass a bump nobody looked at.
#-------------------------------------------------------------------------------
test_inject_refuses_a_checklist_without_items() {
    echo "TEST: inject refuses a checklist Tier B could not act on"

    local dir out rc
    dir="$(mktemp -d)"
    printf '# Bot PR checklist\n\n- The tests pass.\n' > "${dir}/bot-pr-checklist.md"

    out="$(printf 'Body.\n' | deps_lane_inject_checklist "${dir}/bot-pr-checklist.md" 2>"${dir}/err")"; rc=$?
    if [ "${rc}" -ne 2 ] || [ -n "${out}" ] || ! grep -q 'Manual verification' "${dir}/err"; then
        fail "a checklist without verification items must refuse (rc 2, no stdout, says why)" \
            "rc=${rc} out=${out} err=$(cat "${dir}/err")"
        rm -rf "${dir}"
        return
    fi

    rm -rf "${dir}"
    pass "inject refuses a checklist Tier B could not act on"
}

#-------------------------------------------------------------------------------
# Lane gating (AC 1): the lane is on only when the Harness config declares a
# dependabot block and `enabled` is not false; off, the line says why and the
# exit is 3, the same shape as the Deployed tier's lane.
#-------------------------------------------------------------------------------
test_lane_gate() {
    echo "TEST: the lane is on exactly when the dependabot block is declared and enabled"

    local out rc
    out="$(HARNESS_CONFIG_JSON="$(jq -c '.lanes.deps_land = {present: true, enabled: true}' \
        <<<"${HARNESS_CONFIG_JSON}")" deps_lane_lane)"; rc=$?
    if [ "${rc}" -ne 0 ] || [ "${out}" != "deps-lane: on" ]; then
        fail "a declared, enabled block turns the lane on" "rc=${rc} out=${out}"
        return
    fi

    out="$(HARNESS_CONFIG_JSON="$(jq -c '.lanes.deps_land = {present: true, enabled: false}' \
        <<<"${HARNESS_CONFIG_JSON}")" deps_lane_lane)"; rc=$?
    if [ "${rc}" -ne 3 ] || [ "${out}" != "deps-lane: off — dependabot.enabled is false" ]; then
        fail "enabled false turns the lane off and says so" "rc=${rc} out=${out}"
        return
    fi

    out="$(HARNESS_CONFIG_JSON="$(jq -c '.lanes.deps_land = {present: false, enabled: false}' \
        <<<"${HARNESS_CONFIG_JSON}")" deps_lane_lane)"; rc=$?
    if [ "${rc}" -ne 3 ] || [ "${out}" != "deps-lane: off — the Harness config declares no dependabot block" ]; then
        fail "no block means the lane is off" "rc=${rc} out=${out}"
        return
    fi

    # A config resolved before the lanes key existed (or a hand-built one)
    # reads as off, never as on.
    out="$(HARNESS_CONFIG_JSON="$(jq -c 'del(.lanes)' <<<"${HARNESS_CONFIG_JSON}")" deps_lane_lane)"; rc=$?
    if [ "${rc}" -ne 3 ]; then
        fail "a config with no lanes key reads as off" "rc=${rc} out=${out}"
        return
    fi

    out="$(HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= deps_lane_lane 2>/dev/null)"; rc=$?
    if [ "${rc}" -ne 2 ] || [ -n "${out}" ]; then
        fail "no config at all is a usage error, not a lane verdict" "rc=${rc} out=${out}"
        return
    fi

    pass "the lane is on exactly when the dependabot block is declared and enabled"
}

#-------------------------------------------------------------------------------
# make_gh_stub <dir> <view-json> : a gh stub that logs every call and answers
# `pr view` with <view-json>. Mutations succeed unless <dir>/fail names them.
#-------------------------------------------------------------------------------
make_gh_stub() {
    local dir="$1"
    printf '%s\n' "$2" > "${dir}/view.json"
    : > "${dir}/calls"
    cat > "${dir}/gh" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/calls"
case "\$*" in
    "pr view"*) cat "${dir}/view.json" ;;
    *) if [ -f "${dir}/fail" ] && grep -qF -- "\$2" "${dir}/fail"; then exit 1; fi ;;
esac
STUB
    chmod +x "${dir}/gh"
}

#-------------------------------------------------------------------------------
# Exhaustion labelling (AC 2): a parked bump is drafted (what stops triage
# re-picking it), labelled AFK:deps-failed (never the agent lane's
# AFK:checks-failed) and commented once, naming the last failure. Every gh call
# names the configured repo.
#-------------------------------------------------------------------------------
test_park_drafts_labels_and_comments() {
    echo "TEST: park drafts the PR, labels it AFK:deps-failed and comments once"

    local dir out rc sha='abc123abc123abc123abc123abc123abc123abcd'
    dir="$(mktemp -d)"
    make_gh_stub "${dir}" '{"state":"OPEN","isDraft":false,"labels":[],"comments":[]}'

    out="$(GH_BIN="${dir}/gh" deps_lane_park 812 "${sha}" 'Tier B: settings toggle did not persist')"; rc=$?
    if [ "${rc}" -ne 0 ] || [ "${out}" != "deps-lane: parked PR #812 — draft, AFK:deps-failed" ]; then
        fail "park prints one parked line and exits 0" "rc=${rc} out=${out} calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi
    if [ "$(sed -n 2p "${dir}/calls")" != "pr ready 812 --repo acme/widgets --undo" ] \
        || [ "$(sed -n 3p "${dir}/calls")" != "pr edit 812 --repo acme/widgets --add-label AFK:deps-failed" ] \
        || ! sed -n 4p "${dir}/calls" | grep -q '^pr comment 812 --repo acme/widgets --body '; then
        fail "draft, then label, then comment — each against the configured repo" "calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi
    if ! grep -qF 'Last failure: Tier B: settings toggle did not persist' "${dir}/calls" \
        || ! grep -qF "<!-- deps-lane parked sha=${sha} -->" "${dir}/calls" \
        || ! grep -qF '3 fix attempts' "${dir}/calls"; then
        fail "the comment names the cap, the last failure and carries the sha-keyed marker" "calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi
    if grep -q 'AFK:checks-failed' "${dir}/calls"; then
        fail "a bot PR is never labelled AFK:checks-failed" "calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi

    rm -rf "${dir}"
    pass "park drafts the PR, labels it AFK:deps-failed and comments once"
}

#-------------------------------------------------------------------------------
# Park is idempotent: whichever tier ran out may call it, and a re-fire on an
# already-parked PR mutates nothing (a `gh pr ready --undo` on a draft errors,
# and a second hand-off comment is noise).
#-------------------------------------------------------------------------------
test_park_is_idempotent() {
    echo "TEST: park on an already-parked PR mutates nothing"

    local dir out rc sha='abc123abc123abc123abc123abc123abc123abcd'
    dir="$(mktemp -d)"
    make_gh_stub "${dir}" "$(jq -cn --arg sha "${sha}" '{state: "OPEN", isDraft: true,
        labels: [{name: "AFK:deps-failed"}],
        comments: [{body: ("parked\n<!-- deps-lane parked sha=" + $sha + " -->")}]}')"

    out="$(GH_BIN="${dir}/gh" deps_lane_park 812 "${sha}" 'again')"; rc=$?
    if [ "${rc}" -ne 0 ] || [ "$(wc -l < "${dir}/calls")" -ne 1 ]; then
        fail "an already-parked PR costs one read and nothing else" "rc=${rc} calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi

    # Drafted and labelled by pr-watch, but no park comment for this sha yet:
    # only the comment is owed.
    make_gh_stub "${dir}" '{"state":"OPEN","isDraft":true,"labels":[{"name":"AFK:deps-failed"}],"comments":[]}'
    GH_BIN="${dir}/gh" deps_lane_park 812 "${sha}" 'x' >/dev/null
    if [ "$(grep -c '^pr ' "${dir}/calls")" -ne 2 ] || ! sed -n 2p "${dir}/calls" | grep -q '^pr comment 812'; then
        fail "only the missing comment is posted" "calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi

    rm -rf "${dir}"
    pass "park on an already-parked PR mutates nothing"
}

#-------------------------------------------------------------------------------
# Park refuses to run on nothing and says so when gh fails: a usage error is 2
# with no gh call; a failed read or mutation is 4, so the lane reports ERROR
# instead of claiming a park that did not happen.
#-------------------------------------------------------------------------------
test_park_failures() {
    echo "TEST: park refuses bad args (2) and reports gh failure (4)"

    local dir rc sha='abc123abc123abc123abc123abc123abc123abcd'
    dir="$(mktemp -d)"
    make_gh_stub "${dir}" '{"state":"OPEN","isDraft":false,"labels":[],"comments":[]}'

    GH_BIN="${dir}/gh" deps_lane_park '' "${sha}" 'x' >/dev/null 2>&1; rc=$?
    if [ "${rc}" -ne 2 ] || [ -s "${dir}/calls" ]; then
        fail "no PR number is a usage error with no gh call" "rc=${rc} calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi
    GH_BIN="${dir}/gh" deps_lane_park 812 '' 'x' >/dev/null 2>&1; rc=$?
    if [ "${rc}" -ne 2 ]; then
        fail "no sha is a usage error" "rc=${rc}"
        rm -rf "${dir}"; return
    fi

    printf 'edit\n' > "${dir}/fail"
    GH_BIN="${dir}/gh" deps_lane_park 812 "${sha}" 'x' >/dev/null 2>&1; rc=$?
    if [ "${rc}" -ne 4 ]; then
        fail "a failed label edit is exit 4" "rc=${rc} calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi

    rm -f "${dir}/fail"
    make_gh_stub "${dir}" 'not json'
    GH_BIN="${dir}/gh" deps_lane_park 812 "${sha}" 'x' >/dev/null 2>&1; rc=$?
    if [ "${rc}" -ne 4 ] || [ "$(wc -l < "${dir}/calls")" -ne 1 ]; then
        fail "an unreadable PR is exit 4 with no mutation" "rc=${rc} calls=$(cat "${dir}/calls")"
        rm -rf "${dir}"; return
    fi

    rm -rf "${dir}"
    pass "park refuses bad args (2) and reports gh failure (4)"
}

#-------------------------------------------------------------------------------
# The lib runs as a CLI (what `bin/auto-agent deps-lane` execs), so a skill step
# is one shell line. Same argument order as the functions.
#-------------------------------------------------------------------------------
test_cli_dispatch() {
    echo "TEST: the lib runs as a CLI"

    local sha='1111111111111111111111111111111111111111'
    local checklist out rc
    checklist="$(make_prose_checklist)"

    out="$(bash "${LIB}" retitle 'chore(deps): bump axios from 1.6.0 to 1.6.8' true)"
    if [ "${out}" != 'fix(deps): bump axios from 1.6.0 to 1.6.8' ]; then
        fail "CLI retitle" "got: ${out}"; rm -rf "$(dirname "${checklist}")"; return
    fi
    out="$(printf '' | bash "${LIB}" inject-checklist "${checklist}")"
    if [ "$(printf '%s' "${out}" | head -1)" != '<!-- bot-pr-checklist v1 -->' ]; then
        fail "CLI inject-checklist" "got: ${out}"; rm -rf "$(dirname "${checklist}")"; return
    fi
    out="$(bash "${LIB}" marker-emit tierB "${sha}")"
    if [ "${out}" != "<!-- deps-lane tierB=PASS sha=${sha} -->" ]; then
        fail "CLI marker-emit" "got: ${out}"; rm -rf "$(dirname "${checklist}")"; return
    fi
    out="$(printf '%s\n' "${out}" | bash "${LIB}" marker-parse "${sha}")"
    if [ "$(printf '%s' "${out}" | jq -r '.tierB')" != "true" ]; then
        fail "CLI marker-parse" "got: ${out}"; rm -rf "$(dirname "${checklist}")"; return
    fi
    out="$(bash "${LIB}" rounds-left 2)"
    if [ "${out}" != "1" ]; then
        fail "CLI rounds-left" "got: ${out}"; rm -rf "$(dirname "${checklist}")"; return
    fi
    out="$(printf 'fix(ci): round 1\n' | bash "${LIB}" commit-trailer)"
    if [ "${out}" != 'fix(ci): round 1 [dependabot skip]' ]; then
        fail "CLI commit-trailer" "got: ${out}"; rm -rf "$(dirname "${checklist}")"; return
    fi
    out="$(HARNESS_CONFIG_JSON="$(jq -c '.lanes.deps_land = {present: false, enabled: false}' \
        <<<"${HARNESS_CONFIG_JSON}")" bash "${LIB}" lane)"; rc=$?
    if [ "${rc}" -ne 3 ]; then
        fail "CLI lane" "rc=${rc} got: ${out}"; rm -rf "$(dirname "${checklist}")"; return
    fi
    out="$(bash "${LIB}" nonsense 2>/dev/null)"; rc=$?
    if [ "${rc}" -ne 2 ]; then
        fail "an unknown subcommand exits 2" "rc=${rc} out=${out}"; rm -rf "$(dirname "${checklist}")"; return
    fi

    rm -rf "$(dirname "${checklist}")"
    pass "the lib runs as a CLI"
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
test_security_chore_becomes_fix
test_non_matching_titles_pass_through
test_retitle_is_idempotent_and_reads_stdin
test_retitle_rejects_bad_security_flag
test_inject_appends_unit_once
test_inject_is_byte_level_noop_when_marker_present
test_inject_leaves_no_trace
test_inject_refuses_when_buffer_fails
test_inject_wraps_plain_prose_verbatim
test_inject_reads_checklist_from_config
test_inject_refuses_a_checklist_without_items
test_commit_trailer_appends_once
test_commit_trailer_is_idempotent
test_lane_gate
test_park_drafts_labels_and_comments
test_park_is_idempotent
test_park_failures
test_cli_dispatch

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
