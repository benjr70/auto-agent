#!/usr/bin/env bash
# deps-lane.sh: the deps-land lane's gate, text transforms, marker vocabulary,
# fix budget and exhaustion park.
#
# Sourceable library, and a CLI (`bin/auto-agent deps-lane <sub>`). Every
# mutation the lane makes to a Dependabot PR (its title, its body, the hidden
# markers it leaves in comments, the park) is defined here and tested, instead
# of being prose in a skill that an agent re-improvises each Fire. The text
# transforms take args and stdin and print to stdout; only `park` calls gh.
#
# The lane's memory of a bot PR lives in hidden HTML comments keyed to the head
# sha, so every verdict expires the moment the PR moves. Two other readers
# depend on the marker format: lib/pr-triage.sh (which classifies a bot PR by
# its markers) and lib/deps-gate.sh (which refuses to merge without both tier
# markers), so the emit/parse pair lives here rather than in a skill.
#
# Carried over from the Smart Smoker harness's deps-lane.sh; the lane gate,
# the config-read checklist and the park are this harness's.
#
#   deps_lane_lane [<target-dir>]
#       Is the lane on? Prints `deps-lane: on`, or `deps-lane: off — <why>`
#       and returns 3. On only when the Harness config declares a `dependabot`
#       block and its `enabled` is not false (the resolved lanes.deps_land).
#       No config at all returns 2 with no stdout.
#
#   deps_lane_retitle <title> true|false | deps_lane_retitle true|false < title
#       A security bump's `chore(deps):` prefix becomes `fix(deps):`; every
#       other title comes back unchanged. The flag must be exactly true|false:
#       anything else returns 2 with no stdout.
#
#   deps_lane_inject_checklist [<checklist-path>] < body > new-body
#       Appends the Bot-PR checklist unit to a body that does not already carry
#       the `<!-- bot-pr-checklist v1 -->` marker; a byte-level no-op on one
#       that does. The path defaults to the Harness config's
#       prose.bot_pr_checklist (the Target Project's `bot-pr-checklist.md`
#       sibling). Returns 2 with no stdout when there is no checklist or it
#       holds no unchecked item under a verification heading, 3 with no stdout
#       when the body cannot be buffered.
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
#   deps_lane_commit_trailer [message] | deps_lane_commit_trailer < message
#       The message with ` [dependabot skip]` on its last line, exactly once.
#       An empty message returns 2.
#
#   deps_lane_park <pr> <sha> <last-failure>
#       Parks an exhausted bump: draft (what stops triage re-picking it), label
#       AFK:deps-failed, one hand-off comment carrying
#       `<!-- deps-lane parked sha=<sha> -->`. Idempotent: it reads the PR once
#       and does only the moves still owed, so whichever tier ran out may call
#       it. Prints `deps-lane: parked PR #<pr> — draft, AFK:deps-failed`.
#       Returns 2 on a missing pr or sha (no gh call), 4 when gh could not read
#       the PR or a move failed.
#
# The cap is the Harness config's documented `rounds.deps_fix` (ADR 0002),
# read through harness_config_resolve, so the Target Project decides how many
# fix rounds a bot PR gets; there is no env override. When no config can be
# resolved the functions that read it return 2: a lane without a config
# cannot run at all, and a guessed cap would silently disable the budget.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        or AUTO_AGENT_TARGET_DIR): lanes.deps_land,
#                        rounds.deps_fix, prose.bot_pr_checklist, repo.slug
#   GH_BIN (default: gh) park only; injected for tests

_deps_lane_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_deps_lane_lib_dir}/harness-config.sh"

DEPS_LANE_CHECKLIST_MARKER='<!-- bot-pr-checklist v1 -->'
DEPS_LANE_CHECKLIST_END='<!-- /bot-pr-checklist -->'
DEPS_LANE_SKIP_TRAILER='[dependabot skip]'

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

# _deps_lane_cfg -> the resolved Harness config, or return 2 (the resolver's
# stderr line says why).
_deps_lane_cfg() {
    harness_config_resolve "${1:-}" || return 2
}

#-------------------------------------------------------------------------------
# lane
#-------------------------------------------------------------------------------
# deps_lane_lane [<target-dir>] : the lane line; 0 on, 3 off, 2 no config
#
# The lane is optional (Spec #23, Scope of lanes): a Target Project pays for it
# only by declaring a `dependabot` block, and `enabled: false` is the quick
# switch that keeps the block. lib/pr-triage.sh's
# pr_triage_bot_verdict_unworkable asks the same resolved lanes.deps_land, so
# triage never hands a Fire a bot PR the lane would refuse. A config with no
# lanes key at all reads as off, never as on.
deps_lane_lane() {
    local cfg
    cfg="$(_deps_lane_cfg "${1:-}")" || return 2
    if [ "$(printf '%s' "${cfg}" | jq -r '.lanes.deps_land.present // false')" != "true" ]; then
        echo "deps-lane: off — the Harness config declares no dependabot block"
        return 3
    fi
    if [ "$(printf '%s' "${cfg}" | jq -r '.lanes.deps_land.enabled // false')" != "true" ]; then
        echo "deps-lane: off — dependabot.enabled is false"
        return 3
    fi
    echo "deps-lane: on"
    return 0
}


#-------------------------------------------------------------------------------
# retitle
#-------------------------------------------------------------------------------
# deps_lane_retitle <title> <security> | deps_lane_retitle <security> < title
#
# A security bump must land as `fix(deps):` so a release tool cuts a patch
# release and the user-visible changelog says a vulnerability was fixed; a
# routine bump stays `chore(deps):` and stays out of the notes. Only the prefix
# is rewritten — Dependabot's remaining text (dependency names, version ranges,
# multi-dep groups) is carried through byte-for-byte, because that text is the
# only record of what actually moved.
#
# Unchanged, always: non-security titles, titles already starting `fix(deps):`
# (so the transform is idempotent), and any foreign prefix such as
# `chore(deps-dev):` or `build:` — the lane only ever promotes the exact
# `chore(deps):` prefix it knows Dependabot emits for production dependencies.
#
# The security flag must be exactly `true` or `false`; anything else returns 2
# and prints nothing. That is what makes the one-argument misuse
# `deps_lane_retitle "<title>"` fail loudly instead of treating the title as a
# flag and then reading a title from a stdin nobody is piping — which would
# either rename the PR to an empty string or block forever on a tty. A merely
# unrecognised flag is refused too (`True`, `1`, `yes`): silently reading it as
# "not a security update" would leave a vulnerability fix titled `chore`.
deps_lane_retitle() {
    local title security
    if [ "$#" -ge 2 ]; then
        title="$1"
        security="$2"
    else
        security="${1:-}"
    fi

    if [ "${security}" != "true" ] && [ "${security}" != "false" ]; then
        echo "deps-lane: retitle needs a security flag of exactly true|false," \
            "got: ${security:-<none>}" >&2
        return 2
    fi

    # Only now is it safe to block on stdin: the flag is known-good, so this can
    # only be the documented `deps_lane_retitle <flag> < title` form.
    if [ "$#" -lt 2 ]; then
        title="$(cat)"
    fi

    if [ "${security}" != "true" ]; then
        printf '%s\n' "${title}"
        return 0
    fi

    case "${title}" in
        'chore(deps):'*) printf 'fix(deps):%s\n' "${title#chore(deps):}" ;;
        *)               printf '%s\n' "${title}" ;;
    esac
    return 0
}


#-------------------------------------------------------------------------------
# inject checklist
#-------------------------------------------------------------------------------
# deps_lane_inject_checklist [<checklist-path>] < body > new-body
#
# Appends the Bot-PR checklist's *injected unit* to a PR body that does not
# already carry the `<!-- bot-pr-checklist v1 -->` marker, and is a byte-level
# no-op on a body that does. The marker is the idempotence key: the lane may
# touch the same PR many times (rebase, re-fire), and a body that grew a second
# copy of the checklist would also grow a second set of empty checkboxes,
# silently un-ticking the verification a round already did.
#
# The checklist is the Target Project's prose (the `bot-pr-checklist.md`
# sibling of harness.json, ADR 0002), pasted verbatim; the markers are the
# harness's (ADR 0003: the checklist protocol is harness-owned). So the unit is
#   - a file whose first line IS the opening marker: that line through the
#     closing marker, inclusive (what follows the closing marker is notes about
#     the file, never injected; no closing marker injects the whole file);
#   - any other file: the opening marker, a blank line, the file verbatim, a
#     blank line and the closing marker.
# The unit must hold at least one unchecked item under a verification heading
# (lib/checklist.sh's parse): Tier B passes only when every item passes, and a
# round over zero items would pass a bump nobody looked at. Refused with 2.
#
# The no-op path streams the body straight back, so a body with no trailing
# newline, CRLFs or trailing blank lines survives byte-for-byte. On the append
# path the body is separated from the unit by one blank line, and an empty body
# yields the unit alone.
#
# If the body cannot be buffered (mktemp fails, or the write to the buffer
# fails) the function prints NOTHING on stdout and returns 3. The caller pipes
# this straight into `gh pr edit --body-file -`, so a buffer failure that still
# printed the checklist would overwrite Dependabot's release notes, changelog
# and commit list with the checklist alone, unrecoverably. Every refusal is
# loud and empty for the same reason.
deps_lane_inject_checklist() {
    local checklist="${1:-}"

    if [ -z "${checklist}" ]; then
        local cfg
        cfg="$(_deps_lane_cfg)" || return 2
        checklist="$(printf '%s' "${cfg}" | jq -r '.prose.bot_pr_checklist // empty')"
        if [ -z "${checklist}" ]; then
            echo "deps-lane: no ${HARNESS_PROSE_BOT_PR_CHECKLIST} beside harness.json;" \
                "the lane has no checklist to inject" >&2
            return 2
        fi
    fi
    if [ ! -f "${checklist}" ]; then
        echo "deps-lane: checklist file not found: ${checklist}" >&2
        return 2
    fi

    local unit
    unit="$(_deps_lane_checklist_unit "${checklist}")"
    if [ -z "$(printf '%s\n' "${unit}" | bash "${_deps_lane_lib_dir}/checklist.sh" parse)" ]; then
        echo "deps-lane: ${checklist} holds no unchecked \`- [ ]\` item under a" \
            "\`## Manual verification\` heading; Tier B would have nothing to verify" >&2
        return 2
    fi

    # Buffer the body in a file rather than a variable: `$(cat)` strips trailing
    # newlines, and the marker-present path must be byte-identical. The buffer is
    # removed explicitly on every exit path rather than by a RETURN trap: a trap
    # set here outlives the call in the *caller's* shell (it is only
    # function-scoped under `set -o functrace`), and this lib is sourced into
    # other people's shells.
    local body_file
    if ! body_file="$(mktemp)" || [ -z "${body_file}" ]; then
        echo "deps-lane: could not create a buffer for the PR body;" \
            "refusing to inject rather than risk discarding it" >&2
        return 3
    fi

    # Consume stdin whatever happens, then check the write landed: a partial or
    # failed write means the body we would echo back is not the body we were
    # given, and echoing a truncated body is the same data loss as echoing none.
    if ! cat > "${body_file}"; then
        echo "deps-lane: could not buffer the PR body (write to ${body_file}" \
            "failed); refusing to inject rather than risk discarding it" >&2
        rm -f "${body_file}"
        return 3
    fi

    if grep -qF "${DEPS_LANE_CHECKLIST_MARKER}" "${body_file}"; then
        if ! cat "${body_file}"; then
            echo "deps-lane: could not read back the buffered PR body" >&2
            rm -f "${body_file}"
            return 3
        fi
        rm -f "${body_file}"
        return 0
    fi

    if [ -s "${body_file}" ]; then
        if ! cat "${body_file}"; then
            echo "deps-lane: could not read back the buffered PR body" >&2
            rm -f "${body_file}"
            return 3
        fi
        # Exactly one blank line between the body and the unit, whether or not
        # the body ended with a newline.
        if [ "$(tail -c 1 "${body_file}" | wc -l)" -eq 0 ]; then
            printf '\n'
        fi
        printf '\n'
    fi
    rm -f "${body_file}"

    printf '%s\n' "${unit}"
    return 0
}

# _deps_lane_checklist_unit <file> : the injected unit (see above), without its
# trailing newline.
_deps_lane_checklist_unit() {
    local file="$1"
    if [ "$(head -n 1 "${file}")" = "${DEPS_LANE_CHECKLIST_MARKER}" ]; then
        awk -v end="${DEPS_LANE_CHECKLIST_END}" '{ print } index($0, end) { exit }' "${file}"
        return 0
    fi
    printf '%s\n\n%s\n\n%s' "${DEPS_LANE_CHECKLIST_MARKER}" "$(cat "${file}")" "${DEPS_LANE_CHECKLIST_END}"
}

#-------------------------------------------------------------------------------
# commit trailer
#-------------------------------------------------------------------------------
# deps_lane_commit_trailer [message] | deps_lane_commit_trailer < message
#
# Prints the commit message with ` [dependabot skip]` ending it, exactly once.
#
# Every commit the lane pushes onto a Dependabot branch must carry this marker,
# because Dependabot stops managing a branch it believes a human has taken over:
# no more rebases onto master, no more version bumps onto the same PR. A fix
# round that lands without the marker therefore strands the very PR it was
# trying to rescue — behind master, unrebasable, with only a human able to move
# it. That is why this is a text transform with a test rather than a sentence in
# a skill an agent re-types each fire.
#
# The marker goes at the END of the message (appended to the last non-empty
# line), not on a line of its own: Dependabot scans the whole message, but
# ending the message is the shape the acceptance criteria name and the shape a
# `tail -1` check in review can see. Trailing blank lines are dropped and the
# result ends with exactly one newline, so the output is safe to hand to
# `git commit -F -`, which reproduces its input byte-for-byte including any
# accidental blank tail.
#
# Idempotent: a message whose last non-empty line ALREADY CONTAINS the marker —
# at the end, followed by trailing spaces, or anywhere in the line — comes back
# with the same text (trailing whitespace trimmed) and no second marker. The
# caller pipes every message through unconditionally — across rounds it
# re-commits amended messages — and a doubled
# `[dependabot skip] [dependabot skip]` in a squash subject would fail the
# repo's conventional-commit title lint. "Ends with the marker" is too narrow a
# test for that job: a message round-tripped through an editor or a `gh` body
# picks up a trailing space, and the strict check would then append a second
# marker to a line that already had one.
#
# An empty (or whitespace-only) message is refused with return 2 and nothing on
# stdout: a commit whose entire subject is `[dependabot skip]` records nothing
# about the fix, and the caller would not notice it had lost the summary.
deps_lane_commit_trailer() {
    local msg
    if [ "$#" -ge 1 ]; then
        msg="$1"
    else
        msg="$(cat)"
    fi

    # `$(cat)` and `$1` both keep interior newlines; only the trailing ones need
    # normalizing, and awk below rebuilds the message line by line anyway.
    if [ -z "${msg//[[:space:]]/}" ]; then
        echo "deps-lane: commit-trailer needs a non-empty commit message" >&2
        return 2
    fi

    printf '%s\n' "${msg}" | awk -v marker="${DEPS_LANE_SKIP_TRAILER}" '
        { lines[NR] = $0 }
        END {
            last = 0
            for (i = 1; i <= NR; i++) if (lines[i] ~ /[^[:space:]]/) last = i
            # Rebuild through the last non-empty line, dropping the blank tail.
            for (i = 1; i < last; i++) print lines[i]
            tail = lines[last]
            # Trailing whitespace is not content: trim it before deciding, so a
            # message ending `[dependabot skip] ` is recognized as already
            # marked instead of collecting a second marker.
            sub(/[[:space:]]+$/, "", tail)
            # Presence ANYWHERE in the last line is enough. Requiring the marker
            # to sit at the very end re-marks a line that already carries one
            # (twice, or mid-line), which is the doubling this guard exists to
            # prevent; a message that mentions the marker mid-line is already
            # skippable by Dependabot, which scans the whole message.
            if (index(tail, marker) > 0)
                print tail
            else
                print tail " " marker
        }'
    return 0
}

#-------------------------------------------------------------------------------
# park
#-------------------------------------------------------------------------------
# deps_lane_park <pr> <sha> <last-failure>
#
# The one place an exhausted bump is parked, whichever tier ran out of the
# `rounds.deps_fix` budget: `/auto-agent:pr-watch --bot` on Tier A, the deps-land
# skill on Tier B. Three moves, in this order:
#   1. draft: the load-bearing half. PR triage skips drafts, so a labelled but
#      non-draft PR would be re-picked every Fire forever;
#   2. label AFK:deps-failed, never the agent lane's AFK:checks-failed, which
#      the deps lane never looks for;
#   3. one hand-off comment naming the cap and the last failure, carrying
#      `<!-- deps-lane parked sha=<sha> -->` so a second park of the same head
#      posts nothing.
# It reads the PR once (isDraft, labels, comments) and does only the moves
# still owed, so two callers, or a re-fire after a crash between moves, never
# error on `gh pr ready --undo` against a draft or post a second comment.
deps_lane_park() {
    local pr="${1:-}" sha="${2:-}" failure="${3:-}"
    if ! [[ "${pr}" =~ ^[0-9]+$ ]] || [ -z "${sha}" ]; then
        echo "deps-lane: park needs <pr> <sha> <last-failure>" >&2
        return 2
    fi

    local cfg slug cap
    cfg="$(_deps_lane_cfg)" || return 2
    slug="$(harness_config_slug deps-lane)" || return 2
    cap="$(_deps_lane_fix_cap)" || return 2

    local gh="${GH_BIN:-gh}" view marker
    marker="<!-- deps-lane parked sha=${sha} -->"
    view="$("${gh}" pr view "${pr}" --repo "${slug}" --json isDraft,labels,comments 2>/dev/null)"
    if ! printf '%s' "${view}" | jq -e 'type == "object"' >/dev/null 2>&1; then
        echo "deps-lane: could not read PR #${pr} to park it" >&2
        return 4
    fi

    if [ "$(printf '%s' "${view}" | jq -r '.isDraft')" != "true" ]; then
        "${gh}" pr ready "${pr}" --repo "${slug}" --undo >/dev/null || {
            echo "deps-lane: could not draft PR #${pr}" >&2
            return 4
        }
    fi
    if ! printf '%s' "${view}" | jq -e --arg l "${HARNESS_LABEL_DEPS_FAILED}" \
        'any(.labels[]?; .name == $l)' >/dev/null; then
        "${gh}" pr edit "${pr}" --repo "${slug}" --add-label "${HARNESS_LABEL_DEPS_FAILED}" >/dev/null || {
            echo "deps-lane: could not label PR #${pr} ${HARNESS_LABEL_DEPS_FAILED}" >&2
            return 4
        }
    fi
    if ! printf '%s' "${view}" | jq -e --arg m "${marker}" \
        'any(.comments[]?; (.body // "") | contains($m))' >/dev/null; then
        local body
        body="$(printf 'deps-land: the fix budget of %s fix attempts on this bump is spent. Marked draft + labelled %s. Last failure: %s. Human triage required.\n\n%s' \
            "${cap}" "${HARNESS_LABEL_DEPS_FAILED}" "${failure:-<not given>}" "${marker}")"
        "${gh}" pr comment "${pr}" --repo "${slug}" --body "${body}" >/dev/null || {
            echo "deps-lane: could not comment on PR #${pr}" >&2
            return 4
        }
    fi

    echo "deps-lane: parked PR #${pr} — draft, ${HARNESS_LABEL_DEPS_FAILED}"
    return 0
}

#-------------------------------------------------------------------------------
# CLI
#-------------------------------------------------------------------------------
# What `bin/auto-agent deps-lane <sub>` execs, so a skill step is one shell line
# rather than a source plus a call. Subcommands take the same arguments, in the
# same order, as the functions; the function's return code is the exit code.
_deps_lane_usage() {
    echo "usage: deps-lane.sh {lane|retitle|inject-checklist|marker-emit|marker-parse|rounds-left|commit-trailer|park} …"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    set -uo pipefail
    # shellcheck source=host-env.sh
    . "${_deps_lane_lib_dir}/host-env.sh"
    host_env_load
    _cmd="${1:-}"
    shift || true
    case "${_cmd}" in
        lane)             deps_lane_lane "$@" ;;
        retitle)          deps_lane_retitle "$@" ;;
        inject-checklist) deps_lane_inject_checklist "$@" ;;
        marker-emit)      deps_lane_marker_emit "$@" ;;
        marker-parse)     deps_lane_marker_parse "$@" ;;
        rounds-left)      deps_lane_rounds_left "$@" ;;
        commit-trailer)   deps_lane_commit_trailer "$@" ;;
        park)             deps_lane_park "$@" ;;
        -h|--help|help)   _deps_lane_usage; exit 0 ;;
        *)                _deps_lane_usage >&2; exit 2 ;;
    esac
    exit $?
fi
