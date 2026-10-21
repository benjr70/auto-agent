#!/usr/bin/env bash
# pick.sh: `auto-agent pick` for the bash Daemon, until it is cut over.
#
# Why this exists: the planning skills hand a ticket to the Daemon with one
# call, `"$AA" pick add|remove`, the shape of the Go binary's `pick` command
# (internal/pick). Until a Target Project is cut over (ADR 0014) `$AA` is this
# bash engine, so the same call lands here and runs the bash libs it replaces
# in the order the Go command uses: labels ensured, the Project item and its
# Priority, then the AFK label last, so a ticket whose board write failed is
# never picked. remove takes AFK off first, then the item off the board.
#
# Usage:
#   lib/pick.sh add <issue> [--priority <name>]
#   lib/pick.sh remove <issue>
#   lib/pick.sh labels
#
# Under a Project pick, add's Priority defaults to the FIRST entry of the
# configured order (pick-publish's own default is the last; this passes the
# first explicitly), and a name outside the order is refused before any write.
# Under a label-only pick --priority is ignored. labels only ensures the
# harness labels: what a skill runs before it creates a ticket labelled with
# one that never goes on the pick signal (a Spec, a Map, a HITL ticket).
#
# Output (stdout): the line the Go command prints on success:
#   pick: #<N> is on the pick signal: AFK[, on project <n> with <field> <P>]
#   pick: #<N> is off the pick signal: no AFK[, taken off|not on project <n>]
#   pick: labels created <n>, present <m>
#
# Exit codes:
#   0  landed
#   1  it did not land: the step that failed is named on stderr, with
#      pick-publish's reason when that is the step
#   2  usage, no Harness config, or an unknown priority name
#
# Env:
#   GH_BIN   gh CLI (default: gh), injectable for tests; passed through to
#            labels-ensure.sh and pick-publish.sh

set -uo pipefail

_pick_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_pick_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_pick_lib_dir}/host-env.sh"

_pick_err() { echo "auto-agent: pick: $*" >&2; }

_pick_usage() {
    sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
}

# _pick_publish <verb> <issue> [<priority>]: pick-publish's verdict on
# stdout, its exit code returned.
_pick_publish() {
    local args=("$1" --issue "$2")
    [ -n "${3:-}" ] && args+=(--priority "$3")
    bash "${_pick_lib_dir}/pick-publish.sh" "${args[@]}"
}

# _pick_labels: labels-ensure.sh, its lines in the Go command's words.
_pick_labels() {
    local ensured rc
    ensured="$(bash "${_pick_lib_dir}/labels-ensure.sh" 2>/dev/null)"; rc=$?
    printf '%s\n' "${ensured}" | sed -n 's/^labels-ensure: created \([^0-9].*\)$/pick: created the label \1/p'
    [ "${rc}" -eq 0 ] || return "${rc}"
    printf '%s\n' "${ensured}" | sed -n 's/^labels-ensure: \(created [0-9]*, present [0-9]*\)$/\1/p'
}

pick_labels() {
    local summary rc
    harness_config_resolve >/dev/null || return 2
    summary="$(_pick_labels)"; rc=$?
    printf '%s\n' "${summary}" | sed 's/^\(created [0-9]*, present [0-9]*\)$/pick: labels \1/'
    if [ "${rc}" -ne 0 ]; then
        _pick_err "labels: the harness labels could not be ensured"
        return 1
    fi
}

pick_add() {
    local issue="$1" priority="$2" cfg shape slug number field verdict rc
    cfg="$(harness_config_resolve)" || return 2
    export HARNESS_CONFIG_JSON="${cfg}"
    shape="$(jq -r '.pick.shape' <<<"${cfg}")"
    slug="$(jq -r '.repo.slug' <<<"${cfg}")"
    if [ "${shape}" = "project" ]; then
        number="$(jq -r '.pick.project.number' <<<"${cfg}")"
        field="$(jq -r '.pick.project.priority_field' <<<"${cfg}")"
        [ -n "${priority}" ] || priority="$(jq -r '.pick.project.order[0] // empty' <<<"${cfg}")"
        if ! jq -e --arg p "${priority}" '.pick.project.order | index($p) != null' <<<"${cfg}" >/dev/null; then
            _pick_err "add #${issue}: unknown-priority: priority '${priority}' is not in the configured order $(jq -c '.pick.project.order' <<<"${cfg}")"
            return 2
        fi
    fi
    local ensured
    ensured="$(_pick_labels)"; rc=$?
    printf '%s\n' "${ensured}" | grep '^pick: '
    if [ "${rc}" -ne 0 ]; then
        _pick_err "add #${issue}: labels-failed: the harness labels could not be ensured"
        return 1
    fi
    if [ "${shape}" = "project" ]; then
        verdict="$(_pick_publish publish "${issue}" "${priority}" 2>/dev/null)"; rc=$?
        if [ "${rc}" -ne 0 ]; then
            _pick_err "add #${issue}: $(jq -r '.reason // "pick-publish failed"' <<<"${verdict}" 2>/dev/null || echo "pick-publish failed")"
            return 1
        fi
    fi
    if ! "${GH_BIN:-gh}" issue edit "${issue}" --repo "${slug}" --add-label AFK >/dev/null 2>&1; then
        _pick_err "add #${issue}: label-failed: AFK could not be put on #${issue}"
        return 1
    fi
    if [ "${shape}" = "project" ]; then
        echo "pick: #${issue} is on the pick signal: AFK, on project ${number} with ${field} ${priority}"
    else
        echo "pick: #${issue} is on the pick signal: AFK"
    fi
}

pick_remove() {
    local issue="$1" cfg shape slug number verdict rc
    cfg="$(harness_config_resolve)" || return 2
    export HARNESS_CONFIG_JSON="${cfg}"
    shape="$(jq -r '.pick.shape' <<<"${cfg}")"
    slug="$(jq -r '.repo.slug' <<<"${cfg}")"
    if ! "${GH_BIN:-gh}" issue edit "${issue}" --repo "${slug}" --remove-label AFK >/dev/null 2>&1; then
        _pick_err "remove #${issue}: label-failed: AFK could not be taken off #${issue}"
        return 1
    fi
    if [ "${shape}" != "project" ]; then
        echo "pick: #${issue} is off the pick signal: no AFK"
        return 0
    fi
    number="$(jq -r '.pick.project.number' <<<"${cfg}")"
    verdict="$(_pick_publish unpublish "${issue}" 2>/dev/null)"; rc=$?
    if [ "${rc}" -ne 0 ]; then
        _pick_err "remove #${issue}: $(jq -r '.reason // "pick-publish failed"' <<<"${verdict}" 2>/dev/null || echo "pick-publish failed")"
        return 1
    fi
    if [ "$(jq -r '.removed' <<<"${verdict}")" = "true" ]; then
        echo "pick: #${issue} is off the pick signal: no AFK, taken off project ${number}"
    else
        echo "pick: #${issue} is off the pick signal: no AFK, not on project ${number}"
    fi
}

_pick_main() {
    local verb="${1:-}" issue="" priority=""
    shift || true
    while [ $# -gt 0 ]; do
        case "$1" in
            --priority) priority="${2:-}"; shift ;;
            -h|--help) _pick_usage; return 2 ;;
            -*) _pick_err "unknown argument '$1'"; return 2 ;;
            *) if [ -z "${issue}" ]; then issue="$1"; else _pick_err "unexpected argument '$1'"; return 2; fi ;;
        esac
        shift
    done
    case "${verb}" in
        add|remove) ;;
        labels)
            [ -z "${issue}${priority}" ] || { _pick_err "labels takes no issue and no --priority"; return 2; }
            host_env_load
            pick_labels; return ;;
        *) _pick_err "add, remove or labels required"; _pick_usage; return 2 ;;
    esac
    if ! printf '%s' "${issue}" | grep -Eq '^[1-9][0-9]*$'; then
        _pick_err "an issue number is required"; return 2
    fi
    if [ "${verb}" = "remove" ] && [ -n "${priority}" ]; then
        _pick_err "remove takes no --priority"; return 2
    fi
    host_env_load
    case "${verb}" in
        add) pick_add "${issue}" "${priority}" ;;
        remove) pick_remove "${issue}" ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    _pick_main "$@"
fi
