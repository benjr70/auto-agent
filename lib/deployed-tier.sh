#!/usr/bin/env bash
# deployed-tier.sh: the Deployed tier — the optional lane that runs a merged
# Agent PR's deferred checklist items read-only against a live environment.
#
# Why this exists: a hermetic round defers what it cannot prove in a per-PR
# environment (the item needs the real deployment) and demands a
# `<!-- post-deploy: … -->`-tagged spec for it. Without this lane those items
# stay unticked for ever. With it, a Target Project that declares
# `verification.deployed` gets them run after merge, by the same verifier core
# and the same checklist protocol, against the environment its own command
# resolves (ADR 0003: `status` in place of `up`/`down`). Four questions a Fire
# must never improvise live here, each one a tested answer:
#
#   lane    is the lane on? Only when the block exists and `enabled` is not
#           false (an omitted `enabled` is on) — a lane can never be on without
#           its inputs, and a declared-but-disabled lane says so.
#   items   which of a PR's items are deferred: the UNCHECKED items of the two
#           verification sections carrying the `<!-- post-deploy:` tag.
#   pick    which merged Agent PR the lane works next: the oldest merged
#           `feat/issue-<N>` PR with deferred items and a round left under
#           `rounds.manual_verify` (rounds are counted by their comment header,
#           `### Deployed verification — round`), once it has been quiet for
#           the wait window: that long since it merged (its deploy has had
#           time to land) and since its last round (the Daemon cycles again
#           straight after a Fire that worked, and must not spend every round
#           on one PR inside a few minutes).
#   status  the live environment's `KEY=value` block from `<command> status`.
#           The tier calls `status` and NOTHING else: never `up`, never `down`.
#
# The Harness config is the Fire's (the default branch's): the PR has merged,
# so there is no PR head to read it from.
#
# Usage:
#   lib/deployed-tier.sh lane   [<target-dir>]
#   lib/deployed-tier.sh items  [<body-file>]      # stdin when no file
#   lib/deployed-tier.sh pick   [<target-dir>]
#   lib/deployed-tier.sh status [<target-dir>]
#
# `lane` prints one line: `deployed-lane: on — <command>` or
# `deployed-lane: off — <why>`.
# `items` prints `<section><TAB><item text>` per deferred item (section is
# `manual` or `human`, as `checklist parse` has it), nothing when none.
# `pick` prints one JSON object:
#   { "pr", "issue": <N>|null, "title", "branch", "mergedAt",
#     "round": <this round>, "max": <the cap>,
#     "items": [ { "section", "text" } ] }
# `status` prints ONLY the block on stdout; the command's own stderr and every
# warning go to stderr. Export it the way the contract says:
#   while IFS='=' read -r k v; do [ -n "$k" ] && export "$k=$v"; done <<<"$BLOCK"
# A declared Surface whose `url_key` the block does not carry is a warning,
# not a failure: a live environment need not expose every Surface a PR
# environment does, and an item that needs one fails on its own evidence.
#
# Exit codes:
#   0  printed (lane on; a pick; a healthy block)
#   1  pick: no merged Agent PR has deferred items with a round left (an
#      unreadable PR list is reported on stderr and reads as none)
#   2  usage error, no Harness config, or the command broke the contract (not
#      executable, an exit outside 0/1/3, a block that is not KEY=value)
#   3  the lane is off (no block, or `enabled` is false). Nothing ran
#   4  status: the live environment is not reachable (`status` exited 1
#      unhealthy or 3 prerequisite missing). An infra-error for the round to
#      report, never an item verdict
#
# Env:
#   GH_BIN                      gh CLI (default: gh), injected for tests
#   DEPLOYED_TIER_PR_LIMIT      merged PRs `pick` looks back over (50)
#   DEPLOYED_TIER_WAIT_MINS     the wait window, in minutes (30)
#   DEPLOYED_TIER_NOW           "now" as epoch seconds, injected for tests
#   DEPLOYED_TIER_STDERR_LINES  lines of `status` stderr quoted in a message (3)

set -uo pipefail

_dt_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_dt_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_dt_lib_dir}/host-env.sh"
# shellcheck source=provider-contract.sh
. "${_dt_lib_dir}/provider-contract.sh"
# shellcheck source=checklist.sh
. "${_dt_lib_dir}/checklist.sh"

DEPLOYED_TIER_TAG='<!-- post-deploy:'
DEPLOYED_TIER_ROUND_MARKER='### Deployed verification — round'

_dt_log() { echo "deployed-tier: $*" >&2; }

# deployed_tier_lane <cfg> : the lane line; 0 on, 3 off
deployed_tier_lane() {
    local cfg="$1"
    if [ "$(printf '%s' "${cfg}" | jq -r '.lanes.deployed.present // false')" != "true" ]; then
        echo "deployed-lane: off — the Harness config declares no verification.deployed block"
        return 3
    fi
    if [ "$(printf '%s' "${cfg}" | jq -r '.lanes.deployed.enabled // false')" != "true" ]; then
        echo "deployed-lane: off — verification.deployed.enabled is false"
        return 3
    fi
    echo "deployed-lane: on — $(printf '%s' "${cfg}" | jq -r '.verification.deployed.command')"
}

# deployed_tier_items < body : the deferred items, as `checklist parse` prints them
deployed_tier_items() {
    checklist_parse | grep -F -- "${DEPLOYED_TIER_TAG}"
    return 0
}

# deployed_tier_pick <cfg> : the next merged Agent PR the lane works
deployed_tier_pick() {
    local cfg="$1" gh="${GH_BIN:-gh}" slug cap list now wait
    deployed_tier_lane "${cfg}" >/dev/null || return 3
    now="${DEPLOYED_TIER_NOW:-$(date +%s)}"
    wait="${DEPLOYED_TIER_WAIT_MINS:-30}"
    case "${now}${wait}" in *[!0-9]*) _dt_log "DEPLOYED_TIER_NOW and DEPLOYED_TIER_WAIT_MINS must be integers"; return 2 ;; esac
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
    cap="$(printf '%s' "${cfg}" | jq -r '.rounds.manual_verify // 3')"

    list="$("${gh}" pr list --repo "${slug}" --state merged --limit "${DEPLOYED_TIER_PR_LIMIT:-50}" \
        --json number,title,headRefName,mergedAt,body,comments 2>/dev/null)"
    if ! printf '%s' "${list}" | jq -e 'type == "array"' >/dev/null 2>&1; then
        _dt_log "could not list the merged PRs of ${slug}: no deployed pick this Fire"
        return 1
    fi

    # Cheap filters in jq (an Agent PR branch, a tag in the body, rounds left,
    # quiet for the wait window); the section-aware parse of the survivors in
    # the one checklist parser.
    local row pr body items issue
    while IFS= read -r row; do
        pr="$(printf '%s' "${row}" | jq -c '{pr: .number, title, branch: .headRefName, mergedAt, round, max: $cap}' --argjson cap "${cap}")"
        body="$(printf '%s' "${row}" | jq -r '.body')"
        items="$(printf '%s\n' "${body}" | deployed_tier_items \
            | jq -R -s -c 'split("\n") | map(select(length > 0) | split("\t") | {section: .[0], text: (.[1:] | join("\t"))})')"
        [ "${items}" != "[]" ] || continue
        issue="$(printf '%s' "${pr}" | jq -r --arg p "${HARNESS_BRANCH_FEATURE_PREFIX}" '.branch | ltrimstr($p)')"
        case "${issue}" in ''|*[!0-9]*) issue=null ;; esac
        printf '%s' "${pr}" | jq -c --argjson issue "${issue}" --argjson items "${items}" \
            '{pr, issue: $issue, title, branch, mergedAt, round, max, items: $items}'
        return 0
    done < <(printf '%s' "${list}" | jq -c --arg p "${HARNESS_BRANCH_FEATURE_PREFIX}" --arg tag "${DEPLOYED_TIER_TAG}" \
                --arg marker "${DEPLOYED_TIER_ROUND_MARKER}" --argjson cap "${cap}" \
                --argjson now "${now}" --argjson wait "${wait}" '
        def epoch: (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601? // 0);
        map(select((.headRefName // "") | startswith($p))
            | select((.body // "") | contains($tag))
            | [(.comments // [])[] | select((.body // "") | contains($marker))] as $rounds
            | . + {round: (($rounds | length) + 1)}
            | select(.round <= $cap)
            | ([(.mergedAt // "" | epoch)] + [$rounds[] | (.createdAt // "" | epoch)] | max) as $last
            | select($last <= $now - $wait * 60))
        | sort_by(.mergedAt) | .[]')
    return 1
}

# deployed_tier_status <cfg> : `<command> status`, and only that
deployed_tier_status() {
    local cfg="$1" abs rc target err out reason name key
    deployed_tier_lane "${cfg}" >&2 || return 3
    abs="$(provider_contract_resolve "${cfg}" deployed)" || {
        _dt_log "the deployed command $(printf '%s' "${cfg}" | jq -r '.verification.deployed.command') is not an executable file under $(harness_config_target_dir "${cfg}")"
        return 2
    }
    target="$(harness_config_target_dir "${cfg}")" || return 2
    err="$(mktemp)" || return 2
    # shellcheck disable=SC2064
    trap "rm -f '${err}'" RETURN

    out="$( cd "${target}" && "${abs}" status 2>"${err}" )"
    rc=$?
    cat "${err}" >&2
    local tail; tail="$(provider_contract_stderr_tail "${err}" "${DEPLOYED_TIER_STDERR_LINES:-3}")"
    case "${rc}" in
        0) ;;
        1) _dt_log "status exited 1 (the deployed environment is unhealthy): ${tail}"; return 4 ;;
        3) _dt_log "status exited 3 (prerequisite missing): ${tail}"; return 4 ;;
        *) _dt_log "status exited ${rc}, want 0 healthy, 1 unhealthy or 3 prerequisite missing: ${tail}"; return 2 ;;
    esac

    # The contract's grammar, worded for `up`; here it was `status` that printed.
    reason="$(provider_contract_block_violation "${out}")"
    reason="${reason/#up printed/status printed}"
    if [ -n "${reason}" ]; then
        _dt_log "status exited 0 but its stdout is not a KEY=value block (progress belongs on stderr): ${reason}"
        return 2
    fi

    while IFS=$'\t' read -r name key; do
        [ -n "${key}" ] || continue
        printf '%s\n' "${out}" | awk -F= -v k="${key}" '$1 == k { found = 1 } END { exit !found }' && continue
        _dt_log "warning: Surface ${name} (${key}) is not in the status block: items that need it cannot be exercised live"
    done < <(printf '%s' "${cfg}" | jq -r '(.surfaces // {}) | to_entries[] | "\(.key)\t\(.value.url_key)"')

    printf '%s\n' "${out}"
}

_dt_usage() { sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; }

deployed_tier_main() {
    local sub="${1:-}"; shift || true
    case "${sub}" in
        -h|--help|help) _dt_usage; return 0 ;;
        items)
            if [ -n "${1:-}" ] && [ "$1" != "-" ]; then
                [ -f "$1" ] || { echo "deployed-tier: body file not found: $1" >&2; return 2; }
                deployed_tier_items < "$1"
            else
                deployed_tier_items
            fi
            return 0 ;;
        lane|pick|status) ;;
        '') _dt_usage >&2; return 2 ;;
        *) echo "deployed-tier: unknown subcommand '${sub}'" >&2; _dt_usage >&2; return 2 ;;
    esac
    case "${1:-}" in -*) echo "deployed-tier: unknown option '$1'" >&2; return 2 ;; esac

    local cfg
    cfg="$(harness_config_resolve "${1:-}")" || return 2
    case "${sub}" in
        lane) deployed_tier_lane "${cfg}" ;;
        pick) deployed_tier_pick "${cfg}" ;;
        status) deployed_tier_status "${cfg}" ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    deployed_tier_main "$@"
fi
