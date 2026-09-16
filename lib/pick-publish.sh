#!/usr/bin/env bash
# pick-publish.sh: put an AFK issue on the Target Project's pick signal, or
# take it off, whatever shape the Harness config declares (ADR 0002).
#
# Why this exists: the pick signal is either a Project board with a Priority
# field or labels only, and three skills have to honour it when they create or
# re-route tickets: to-tickets (Slices), wayfinder (Decision tickets at chart
# time), afk-resolve (fog graduation, and un-projecting a ticket it relabels
# HITL). Under a Project pick an AFK issue that is not on the board is
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
# (default: its last value, the lowest priority). Label-only pick: nothing to
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

# _pp_verdict <kind> <shape> <issue> <flag-name> <flag> <priority|null> <item|null> [<reason>]
_pp_verdict() {
    jq -n -c --arg shape "$2" --argjson issue "$3" --arg flag "$4" --argjson val "$5" \
        --arg prio "$6" --arg item "$7" --arg reason "${8:-}" '
        { shape: $shape, issue: $issue }
        + { ($flag): $val }
        + (if $prio == "" then { priority: null } else { priority: $prio } end)
        + { itemId: (if $item == "" then null else $item end) }
        + (if $reason == "" then {} else { reason: $reason } end)
        | if $flag == "removed" then del(.priority) else . end'
}

# _pp_field_ids <owner> <number> <field-name> <option-name>
# Prints "<project-id> <field-id> <option-id>". Returns 1 project-unreadable,
# 3 field-missing, 4 option-missing.
_pp_field_ids() {
    local gh="${GH_BIN:-gh}" owner="$1" number="$2" field="$3" option="$4"
    local pid fields fid oid
    pid="$("${gh}" project view "${number}" --owner "${owner}" --format json 2>/dev/null | jq -r '.id // empty')" || pid=""
    [ -n "${pid}" ] || return 1
    fields="$("${gh}" project field-list "${number}" --owner "${owner}" --format json 2>/dev/null)" || return 1
    fid="$(printf '%s' "${fields}" | jq -r --arg f "${field}" '.fields[]? | select(.name == $f) | .id' 2>/dev/null | head -1)"
    [ -n "${fid}" ] || return 3
    oid="$(printf '%s' "${fields}" | jq -r --arg f "${field}" --arg o "${option}" '.fields[]? | select(.name == $f) | .options[]? | select(.name == $o) | .id' 2>/dev/null | head -1)"
    [ -n "${oid}" ] || return 4
    printf '%s %s %s\n' "${pid}" "${fid}" "${oid}"
}

# pick_publish <issue> [<priority>]
pick_publish() {
    local issue="$1" priority="${2:-}" cfg shape
    cfg="$(harness_config_resolve)" || return 2
    shape="$(printf '%s' "${cfg}" | jq -r '.pick.shape')"
    if [ "${shape}" != "project" ]; then
        _pp_verdict publish labels "${issue}" projected false "" ""
        return 0
    fi
    local gh="${GH_BIN:-gh}" slug owner number field order
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
    owner="$(printf '%s' "${cfg}" | jq -r '.repo.owner')"
    number="$(printf '%s' "${cfg}" | jq -r '.pick.project.number')"
    field="$(printf '%s' "${cfg}" | jq -r '.pick.project.priority_field // "Priority"')"
    order="$(printf '%s' "${cfg}" | jq -c '.pick.project.order // ["P0","P1","P2"]')"
    [ -n "${priority}" ] || priority="$(printf '%s' "${order}" | jq -r 'last')"
    if ! printf '%s' "${order}" | jq -e --arg p "${priority}" 'index($p) != null' >/dev/null; then
        _pp_err "priority '${priority}' is not in the configured order ${order}"
        _pp_verdict publish project "${issue}" projected false "${priority}" "" unknown-priority
        return 2
    fi
    local url ids pid fid oid item
    url="$("${gh}" issue view "${issue}" --repo "${slug}" --json url 2>/dev/null | jq -r '.url // empty')" || url=""
    if [ -z "${url}" ]; then
        _pp_verdict publish project "${issue}" projected false "${priority}" "" issue-unreadable; return 1
    fi
    ids="$(_pp_field_ids "${owner}" "${number}" "${field}" "${priority}")"
    case $? in
        0) ;;
        3) _pp_verdict publish project "${issue}" projected false "${priority}" "" field-missing; return 1 ;;
        4) _pp_verdict publish project "${issue}" projected false "${priority}" "" option-missing; return 1 ;;
        *) _pp_verdict publish project "${issue}" projected false "${priority}" "" project-unreadable; return 1 ;;
    esac
    read -r pid fid oid <<<"${ids}"
    item="$("${gh}" project item-add "${number}" --owner "${owner}" --url "${url}" --format json 2>/dev/null | jq -r '.id // empty')" || item=""
    if [ -z "${item}" ]; then
        _pp_verdict publish project "${issue}" projected false "${priority}" "" item-add-failed; return 1
    fi
    if ! "${gh}" project item-edit --project-id "${pid}" --id "${item}" --field-id "${fid}" --single-select-option-id "${oid}" >/dev/null 2>&1; then
        _pp_err "issue #${issue} is on project ${number} but its ${field} could not be set to ${priority}"
        _pp_verdict publish project "${issue}" projected true "${priority}" "${item}" priority-edit-failed; return 1
    fi
    _pp_verdict publish project "${issue}" projected true "${priority}" "${item}"
}

# pick_unpublish <issue>
pick_unpublish() {
    local issue="$1" cfg shape
    cfg="$(harness_config_resolve)" || return 2
    shape="$(printf '%s' "${cfg}" | jq -r '.pick.shape')"
    if [ "${shape}" != "project" ]; then
        _pp_verdict unpublish labels "${issue}" removed false "" ""
        return 0
    fi
    local gh="${GH_BIN:-gh}" slug owner number items item
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
    owner="$(printf '%s' "${cfg}" | jq -r '.repo.owner')"
    number="$(printf '%s' "${cfg}" | jq -r '.pick.project.number')"
    items="$("${gh}" project item-list "${number}" --owner "${owner}" --format json --limit 1000 2>/dev/null)" || {
        _pp_verdict unpublish project "${issue}" removed false "" "" item-list-unreadable; return 1; }
    item="$(printf '%s' "${items}" | jq -r --argjson n "${issue}" --arg slug "${slug}" '
        .items[]? | select(.content.number == $n and ((.content.repository // $slug) == $slug)) | .id' 2>/dev/null | head -1)"
    if [ -z "${item}" ]; then
        _pp_verdict unpublish project "${issue}" removed false "" ""
        return 0
    fi
    if ! "${gh}" project item-delete "${number}" --owner "${owner}" --id "${item}" >/dev/null 2>&1; then
        _pp_verdict unpublish project "${issue}" removed false "" "${item}" item-delete-failed; return 1
    fi
    _pp_verdict unpublish project "${issue}" removed true "" "${item}"
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
