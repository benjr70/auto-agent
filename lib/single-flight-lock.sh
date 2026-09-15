#!/usr/bin/env bash
# Single-flight lock: the `AFK:in-progress` label. While any issue in the
# Target Project holds it, every other Fire skips. This is the one reader the
# pickup triage and the Work Probe share.
#
# Source this file, then:
#
#   single_flight_inflight <repo-slug>
#       Prints the number of open issues holding the lock. Fails SAFE: a gh
#       error or an unreadable count prints 1, so a flake can never start a
#       second in-flight run or a wake-fire-skip loop against a held lock.
#
# Environment:
#   GH_BIN   (default: gh)   injected for tests

_single_flight_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_single_flight_lib_dir}/harness-config.sh"

single_flight_inflight() {
    local slug="${1:?single_flight_inflight: repo slug required}" count
    count="$("${GH_BIN:-gh}" issue list --repo "${slug}" --label "${HARNESS_LABEL_IN_PROGRESS}" --state open \
        --json number --jq 'length' 2>/dev/null || echo '')"
    case "${count}" in
        ''|*[!0-9]*) count=1 ;;
    esac
    printf '%s\n' "${count}"
}
