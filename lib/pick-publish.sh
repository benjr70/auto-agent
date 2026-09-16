#!/usr/bin/env bash
# pick-publish.sh: put an AFK ticket on the Target Project's pick signal, or
# take it off, whatever shape the Harness config declares (ADR 0002).
#
# Why this exists: the pick signal is either a Project board with a Priority
# field or labels only, and three skills have to honour it when they create or
# re-route tickets: to-tickets (Slices), wayfinder (Decision tickets at chart
# time), afk-resolve (fog graduation, and un-projecting a ticket it relabels
# HITL). Under a Project pick an AFK ticket that is not on the board is
# silently never picked, and an item whose Priority edit failed is silently
# read as the lowest value, so "add it to the project" is four ids from three
# gh commands plus an exit-status check. That recipe lives here once; a skill
# calls it and reads one verdict, and under a label-only pick the same call is
# a no-op, so the skill's prose never branches on the pick shape (issue #29
# AC 2).
#
# Usage:
#   lib/pick-publish.sh publish   --issue <N> [--priority <P>]
#   lib/pick-publish.sh unpublish --issue <N>
#
# `publish` (project pick): `gh project item-add`, then `item-edit` setting the
# configured priority field to <P>, which must be one of the configured order
# (default: its last value, the lowest priority). The field name and the order
# arrive already defaulted in the resolved config; nothing here re-spells them. Label-only pick: nothing to
# do, the AFK label already is the signal.
#
# `unpublish` (project pick): finds the issue's item on the board and deletes
# it. What afk-resolve runs when a Decision ticket turns out to need product
# code and is relabelled HITL: project membership is the Daemon's pick signal,
# so a projected HITL ticket would be picked again. Absent item: a no-op.
#
# Output (stdout): one compact JSON verdict, nothing else:
#   publish   { "shape": "project"|"labels", "issue": N, "projected": <bool>,
#               "priority": "<P>"|null, "itemId": "<id>"|null, "reason": "<why>" }
#   unpublish { "shape": "project"|"labels", "issue": N, "removed": <bool>,
#               "itemId": "<id>"|null, "reason": "<why>" }
# `reason` is present only on a failure.
#
# Exit codes:
#   0  done (or nothing to do under a label-only pick)
#   1  gh failed: `reason` is one of
#        issue-unreadable       the issue url could not be read
#        project-unreadable     the project id or field list could not be read
#        field-missing          the configured priority field is not on the board
#        option-missing         the board's field has no option for <P>
#        item-add-failed        `gh project item-add` failed
#        priority-edit-failed   `gh project item-edit` failed: the item IS on the
#                               board but with no Priority; the caller must
#                               report it, never assume the priority landed
#        item-list-unreadable   (unpublish) the board's items could not be read
#        item-list-truncated    (unpublish) the board has more items than one
#                               listing returns, so absence cannot be proven
#        item-delete-failed     (unpublish) `gh project item-delete` failed
#   2  usage: bad/missing arguments, no Harness config, or
#        unknown-priority       <P> is not in the configured order
#
# Every Target Project fact comes from the resolved Harness config through
# harness_config_resolve (HARNESS_CONFIG_JSON, or AUTO_AGENT_TARGET_DIR from
# the Host env): the repo slug and owner, the pick shape, the project number,
# priority field and order.
#
# Env:
#   GH_BIN   gh CLI (default: gh), injectable for tests

set -uo pipefail

_pick_publish_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_pick_publish_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_pick_publish_lib_dir}/host-env.sh"

_pp_err() { echo "pick-publish: $*" >&2; }

_pp_usage() {
    sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
}

# The pick context every verb reads, filled by _pp_load: shape and, under a
# Project pick, slug, owner, project number, priority field and order.
_pp_shape=""; _pp_slug=""; _pp_owner=""; _pp_number=""; _pp_field=""; _pp_order=""

# _pp_load : resolve the config once into the variables above; 2 when none
_pp_load() {
    local cfg
    cfg="$(harness_config_resolve)" || return 2
    _pp_shape="$(printf '%s' "${cfg}" | jq -r '.pick.shape')"
    _pp_slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
    _pp_owner="$(printf '%s' "${cfg}" | jq -r '.repo.owner')"
    _pp_number="$(printf '%s' "${cfg}" | jq -r '.pick.project.number // empty')"
    _pp_field="$(printf '%s' "${cfg}" | jq -r '.pick.project.priority_field // empty')"
    _pp_order="$(printf '%s' "${cfg}" | jq -c '.pick.project.order // empty')"
}

# _pp_verdict <verb> <issue> <flag-value> [<priority>] [<item-id>] [<reason>]
# The one verdict shape: publish carries `projected` and `priority`, unpublish
# carries `removed`; itemId and reason as documented in the header.
_pp_verdict() {
    local verb="$1" issue="$2" val="$3" prio="${4:-}" item="${5:-}" reason="${6:-}"
    local flag="projected"; [ "${verb}" = "unpublish" ] && flag="removed"
    jq -n -c --arg shape "${_pp_shape}" --argjson issue "${issue}" --arg flag "${flag}" --argjson val "${val}" \
        --arg verb "${verb}" --arg prio "${prio}" --arg item "${item}" --arg reason "${reason}" '
        { shape: $shape, issue: $issue }
        + { ($flag): $val }
        + (if $verb == "publish" then { priority: (if $prio == "" then null else $prio end) } else {} end)
        + { itemId: (if $item == "" then null else $item end) }
        + (if $reason == "" then {} else { reason: $reason } end)'
}

# _pp_fail <verb> <issue> <reason> [<priority>] [<item-id>] [<exit>]
# A refused verb: the verdict with its reason, then the exit code (default 1).
# `projected` is true only for priority-edit-failed (the item IS on the board).
_pp_fail() {
    local val=false
    [ "$3" = "priority-edit-failed" ] && val=true
    _pp_verdict "$1" "$2" "${val}" "${4:-}" "${5:-}" "$3"
    return "${6:-1}"
}

# _pp_field_ids <option-name>
# Prints "<project-id> <field-id> <option-id>" for the configured project and
# priority field. Returns 1 project-unreadable, 3 field-missing, 4 option-missing.
_pp_field_ids() {
    local gh="${GH_BIN:-gh}" option="$1" pid fields fid oid
    pid="$("${gh}" project view "${_pp_number}" --owner "${_pp_owner}" --format json 2>/dev/null | jq -r '.id // empty')" || pid=""
    [ -n "${pid}" ] || return 1
    fields="$("${gh}" project field-list "${_pp_number}" --owner "${_pp_owner}" --format json 2>/dev/null)" || return 1
    fid="$(printf '%s' "${fields}" | jq -r --arg f "${_pp_field}" '.fields[]? | select(.name == $f) | .id' 2>/dev/null | head -1)"
    [ -n "${fid}" ] || return 3
    oid="$(printf '%s' "${fields}" | jq -r --arg f "${_pp_field}" --arg o "${option}" '.fields[]? | select(.name == $f) | .options[]? | select(.name == $o) | .id' 2>/dev/null | head -1)"
    [ -n "${oid}" ] || return 4
    printf '%s %s %s\n' "${pid}" "${fid}" "${oid}"
}

# pick_publish <issue> [<priority>]
pick_publish() {
    local issue="$1" priority="${2:-}"
    _pp_load || return 2
    if [ "${_pp_shape}" != "project" ]; then
        _pp_verdict publish "${issue}" false
        return 0
    fi
    local gh="${GH_BIN:-gh}" url ids pid fid oid item
    [ -n "${priority}" ] || priority="$(printf '%s' "${_pp_order}" | jq -r 'last')"
    if ! printf '%s' "${_pp_order}" | jq -e --arg p "${priority}" 'index($p) != null' >/dev/null; then
        _pp_err "priority '${priority}' is not in the configured order ${_pp_order}"
        _pp_fail publish "${issue}" unknown-priority "${priority}" "" 2; return $?
    fi
    url="$("${gh}" issue view "${issue}" --repo "${_pp_slug}" --json url 2>/dev/null | jq -r '.url // empty')" || url=""
    [ -n "${url}" ] || { _pp_fail publish "${issue}" issue-unreadable "${priority}"; return $?; }
    ids="$(_pp_field_ids "${priority}")"
    case $? in
        0) ;;
        3) _pp_fail publish "${issue}" field-missing "${priority}"; return $? ;;
        4) _pp_fail publish "${issue}" option-missing "${priority}"; return $? ;;
        *) _pp_fail publish "${issue}" project-unreadable "${priority}"; return $? ;;
    esac
    read -r pid fid oid <<<"${ids}"
    item="$("${gh}" project item-add "${_pp_number}" --owner "${_pp_owner}" --url "${url}" --format json 2>/dev/null | jq -r '.id // empty')" || item=""
    [ -n "${item}" ] || { _pp_fail publish "${issue}" item-add-failed "${priority}"; return $?; }
    if ! "${gh}" project item-edit --project-id "${pid}" --id "${item}" --field-id "${fid}" --single-select-option-id "${oid}" >/dev/null 2>&1; then
        _pp_err "issue #${issue} is on project ${_pp_number} but its ${_pp_field} could not be set to ${priority}"
        _pp_fail publish "${issue}" priority-edit-failed "${priority}" "${item}"; return $?
    fi
    _pp_verdict publish "${issue}" true "${priority}" "${item}"
}

# One listing must hold the whole board for "absent" to mean absent.
_PP_ITEM_LIST_LIMIT=5000

# pick_unpublish <issue>
pick_unpublish() {
    local issue="$1"
    _pp_load || return 2
    if [ "${_pp_shape}" != "project" ]; then
        _pp_verdict unpublish "${issue}" false
        return 0
    fi
    local gh="${GH_BIN:-gh}" items item count
    items="$("${gh}" project item-list "${_pp_number}" --owner "${_pp_owner}" --format json --limit "${_PP_ITEM_LIST_LIMIT}" 2>/dev/null)" \
        || { _pp_fail unpublish "${issue}" item-list-unreadable; return $?; }
    count="$(printf '%s' "${items}" | jq -r '.items | length' 2>/dev/null)" || count=0
    if [ "${count}" -ge "${_PP_ITEM_LIST_LIMIT}" ]; then
        _pp_fail unpublish "${issue}" item-list-truncated; return $?
    fi
    item="$(printf '%s' "${items}" | jq -r --argjson n "${issue}" --arg slug "${_pp_slug}" '
        .items[]? | select(.content.number == $n and ((.content.repository // $slug) == $slug)) | .id' 2>/dev/null | head -1)"
    if [ -z "${item}" ]; then
        _pp_verdict unpublish "${issue}" false
        return 0
    fi
    "${gh}" project item-delete "${_pp_number}" --owner "${_pp_owner}" --id "${item}" >/dev/null 2>&1 \
        || { _pp_fail unpublish "${issue}" item-delete-failed "" "${item}"; return $?; }
    _pp_verdict unpublish "${issue}" true "" "${item}"
}

_pp_main() {
    local cmd="${1:-}" issue="" priority=""
    shift || true
    while [ $# -gt 0 ]; do
        case "$1" in
            --issue) issue="${2:-}"; shift ;;
            --priority) priority="${2:-}"; shift ;;
            -h|--help) _pp_usage; return 2 ;;
            *) _pp_err "unknown argument '$1'"; _pp_usage; return 2 ;;
        esac
        shift
    done
    case "${cmd}" in
        publish|unpublish) ;;
        *) _pp_err "subcommand publish|unpublish required"; _pp_usage; return 2 ;;
    esac
    if ! printf '%s' "${issue}" | grep -Eq '^[0-9]+$'; then
        _pp_err "--issue <N> required"; return 2
    fi
    host_env_load
    case "${cmd}" in
        publish) pick_publish "${issue}" "${priority}" ;;
        unpublish) pick_unpublish "${issue}" ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    _pp_main "$@"
fi
