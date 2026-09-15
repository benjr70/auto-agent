#!/usr/bin/env bash
# deps-lane.sh: the Dependabot lane's marker vocabulary and fix budget.
#
# Sourceable library. The lane's memory of a bot PR lives in hidden HTML
# comments it leaves on the PR, keyed to the head sha, so every verdict
# expires the moment the PR moves. Two readers depend on the format:
# lib/pr-triage.sh (which classifies a bot PR by its markers) and
# lib/deps-gate.sh (which refuses to merge without both tier markers), so the
# emit/parse pair lives here, tested, rather than as prose in a skill.
#
# Carried over from the Smart Smoker harness's deps-lane.sh. This file holds
# the marker and budget functions the PR libs read; the lane's mutating text
# transforms (retitle, checklist inject, commit trailer) land with the
# deps-land lane port (#36) in this same file.
#
#   deps_lane_marker_emit tierA|tierB|fix-attempt <sha> [N]
#       Prints exactly one hidden comment for the state, carrying the sha.
#       An unknown state, an empty sha or a non-numeric N prints nothing and
#       returns 2, so a typo can never be recorded as a marker the parser
#       will not read.
#
#   deps_lane_marker_parse <sha> < all-comment-bodies
#       Reads every comment body on the PR (concatenated, in any order) and
#       prints one compact JSON object:
#         {"sha":"<sha>","tierA":<bool>,"tierB":<bool>,
#          "fixAttempts":<int>,"capReached":<bool>}
#       tierA / tierB are true only when a marker carrying THIS sha says so.
#       fixAttempts counts the `fix-attempt=` markers for this sha (the N each
#       carries is not read back, so one marker writing a large N cannot
#       inflate the count). capReached is fixAttempts >= the cap.
#       Given a sha, exits 0 with a well-formed verdict. An empty sha returns 2
#       with no stdout (an all-false verdict would be a lie about markers
#       nobody looked for); a failing jq propagates as 4.
#
#   deps_lane_rounds_left <recorded-attempts>
#       Prints the cap minus the recorded attempts, never below 0. A
#       non-numeric count returns 2 with no stdout: answering an unreadable
#       history with the full cap would hand the least trustworthy PR the
#       largest fix budget.
#
# The cap is the Harness config's documented `rounds.deps_fix` (ADR 0002),
# read through harness_config_resolve, so the Target Project decides how many
# fix rounds a bot PR gets; there is no env override. When no config can be
# resolved the two cap-reading functions return 2: a lane without a config
# cannot run at all, and a guessed cap would silently disable the budget.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        or AUTO_AGENT_TARGET_DIR): rounds.deps_fix

_deps_lane_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_deps_lane_lib_dir}/harness-config.sh"

# _deps_lane_fix_cap -> the configured cap, or return 2 with a stderr line.
_deps_lane_fix_cap() {
    local cfg cap
    cfg="$(harness_config_resolve 2>/dev/null)" || {
        echo "deps-lane: no Harness config to read rounds.deps_fix from" >&2
        return 2
    }
    cap="$(printf '%s' "${cfg}" | jq -r '.rounds.deps_fix // empty' 2>/dev/null)"
    if ! [[ "${cap}" =~ ^[0-9]+$ ]]; then
        echo "deps-lane: rounds.deps_fix is not a number in the Harness config" >&2
        return 2
    fi
    printf '%s\n' "${cap}"
}

# deps_lane_marker_emit tierA|tierB|fix-attempt <sha> [N]
deps_lane_marker_emit() {
    local state="${1:-}" sha="${2:-}" n="${3:-}"

    if [ -z "${sha}" ]; then
        echo "deps-lane: marker emit requires a sha" >&2
        return 2
    fi

    case "${state}" in
        tierA) printf '<!-- deps-lane tierA=green sha=%s -->\n' "${sha}" ;;
        tierB) printf '<!-- deps-lane tierB=PASS sha=%s -->\n' "${sha}" ;;
        fix-attempt)
            if ! [[ "${n}" =~ ^[0-9]+$ ]]; then
                echo "deps-lane: fix-attempt marker requires a numeric N" >&2
                return 2
            fi
            printf '<!-- deps-lane fix-attempt=%s sha=%s -->\n' "${n}" "${sha}"
            ;;
        *)
            echo "deps-lane: unknown marker state: ${state:-<none>}" >&2
            return 2
            ;;
    esac
    return 0
}

# deps_lane_marker_parse <sha> < all-comment-bodies
deps_lane_marker_parse() {
    local sha="${1:-}"

    if [ -z "${sha}" ]; then
        # Refuse before reading stdin: there is no verdict to compute, and a
        # caller in the CLI form would otherwise block on a tty for input that
        # cannot change the answer.
        echo "deps-lane: marker parse requires a sha" >&2
        return 2
    fi

    local cap
    cap="$(_deps_lane_fix_cap)" || return $?

    local bodies; bodies="$(cat)"

    local tier_a=false tier_b=false attempts=0
    if printf '%s' "${bodies}" \
        | grep -qF "<!-- deps-lane tierA=green sha=${sha} -->"; then
        tier_a=true
    fi
    if printf '%s' "${bodies}" \
        | grep -qF "<!-- deps-lane tierB=PASS sha=${sha} -->"; then
        tier_b=true
    fi
    # grep -o counts every marker, including several on one line. The sha is
    # interpolated into an ERE, so escape anything a caller's sha could carry
    # that the regex engine would otherwise read as syntax.
    local sha_re
    sha_re="$(harness_re_escape "${sha}")"
    attempts="$(printf '%s' "${bodies}" \
        | grep -oE "<!-- deps-lane fix-attempt=[0-9]+ sha=${sha_re} -->" \
        | wc -l | tr -d ' ')"
    [ -n "${attempts}" ] || attempts=0

    local cap_reached=false
    [ "${attempts}" -ge "${cap}" ] && cap_reached=true

    if ! jq -cn --arg sha "${sha}" \
        --argjson tierA "${tier_a}" --argjson tierB "${tier_b}" \
        --argjson fixAttempts "${attempts}" --argjson capReached "${cap_reached}" \
        '{sha: $sha, tierA: $tierA, tierB: $tierB,
          fixAttempts: $fixAttempts, capReached: $capReached}'; then
        echo "deps-lane: jq failed or is not installed; cannot emit a marker" \
            "verdict for ${sha}" >&2
        return 4
    fi
    return 0
}

# deps_lane_rounds_left <recorded-attempts>
deps_lane_rounds_left() {
    local recorded="${1:-}"

    if ! [[ "${recorded}" =~ ^[0-9]+$ ]]; then
        echo "deps-lane: rounds-left needs a numeric attempt count," \
            "got: ${recorded:-<none>}" >&2
        return 2
    fi

    local cap
    cap="$(_deps_lane_fix_cap)" || return $?

    local left=$((cap - recorded))
    [ "${left}" -lt 0 ] && left=0
    printf '%s\n' "${left}"
    return 0
}
