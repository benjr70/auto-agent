#!/usr/bin/env bash
# provider-contract.sh: the one implementation of ADR 0003's driving rules —
# the `down` before the first `up`, the single retry, the `KEY=value` block
# grammar, and the declared Surfaces' `url_key`s.
#
# Why this exists: two front-ends drive the same contract for different
# reasons. `provider-check.sh` is the conformance run a maintainer iterates
# against, and turns every deviation into one verdict line; `verify-boot.sh`
# boots the environment a verification round runs in, and turns the same
# deviations into an infra-error the round reports. Encoded twice, a contract
# change means two edits in two suites, and the check and the round can drift
# into disagreeing about what conformance is. So the driving lives here and
# each front-end keeps only its own reporting.
#
# Source this file, then:
#
#   provider_contract_resolve <cfg> [<tier>]
#       Prints the provider's absolute path. <tier> is `hermetic` (the
#       default) or `deployed`. 3 when the Harness config declares no such
#       tier (for hermetic, the Bootstrap state), 2 when the command does not
#       resolve to an executable file under the Target Project. Both print
#       nothing; the caller phrases the message.
#
#   provider_contract_down <abs> <target> <pr> [<errfile>]
#       One `down --pr N`. Returns the provider's own exit code (the contract
#       says 0; who cares, and how loudly, is the caller's business).
#
#   provider_contract_up <abs> <target> <pr> <errfile>
#       `up --pr N` with the harness's single retry: on exit 4 it runs
#       `down --pr N` once and calls `up` again; exit 3 is never retried,
#       because a missing prerequisite will still be missing a second later.
#       Prints the provider's stdout, returns its exit code, and reports the
#       retry path in two globals: PROVIDER_CONTRACT_ATTEMPTS (1 or 2) and
#       PROVIDER_CONTRACT_RETRY_DOWN_RC (the exit code of the `down` between
#       the attempts, empty when there was no retry).
#
#   provider_contract_block_violation <block>
#       Echoes why the block is not a `KEY=value` block with uppercase shell
#       identifier keys, or nothing when it conforms. The message names `up`,
#       the contract's verdict wording; a `status` caller relabels it.
#
#   provider_contract_missing_url_key <block> <cfg>
#       Echoes `<surface><TAB><KEY>` for the first declared Surface whose
#       `url_key` the block does not carry, or nothing.
#
#   provider_contract_stderr_tail <file> [<lines>]
#       The last lines of one call's stderr, on one line, for a message that
#       has to say WHY.

_provider_contract_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_provider_contract_lib_dir}/harness-config.sh"

PROVIDER_CONTRACT_ATTEMPTS=0
PROVIDER_CONTRACT_RETRY_DOWN_RC=''

provider_contract_resolve() {
    local cfg="${1:?provider_contract_resolve: config required}" tier="${2:-hermetic}" block command target abs
    block="$(printf '%s' "${cfg}" | jq -c --arg t "${tier}" '.verification[$t] // null')"
    [ "${block}" = "null" ] && return 3
    command="$(printf '%s' "${block}" | jq -r '.command')"
    target="$(harness_config_target_dir "${cfg}")" || return 2
    abs="${command}"
    case "${command}" in /*) ;; *) abs="${target}/${command}" ;; esac
    [ -x "${abs}" ] || return 2
    printf '%s\n' "${abs}"
}

provider_contract_down() {
    local abs="$1" target="$2" pr="$3" err="${4:-/dev/null}"
    ( cd "${target}" && "${abs}" down --pr "${pr}" ) >/dev/null 2>>"${err}"
}

provider_contract_up() {
    local abs="$1" target="$2" pr="$3" err="$4" out rc attempt=1
    PROVIDER_CONTRACT_RETRY_DOWN_RC=''
    while :; do
        out="$( cd "${target}" && "${abs}" up --pr "${pr}" 2>"${err}" )"
        rc=$?
        PROVIDER_CONTRACT_ATTEMPTS="${attempt}"
        # Only a failed boot is retried, and only once, with a `down` between.
        if [ "${rc}" -ne 4 ] || [ "${attempt}" -ge 2 ]; then break; fi
        provider_contract_down "${abs}" "${target}" "${pr}" "${err}"
        PROVIDER_CONTRACT_RETRY_DOWN_RC=$?
        [ "${PROVIDER_CONTRACT_RETRY_DOWN_RC}" -eq 0 ] || break
        attempt=$((attempt + 1))
    done
    printf '%s' "${out}"
    return "${rc}"
}

provider_contract_block_violation() {
    local block="$1" line key
    [ -n "${block}" ] || { echo "up printed no keys"; return 0; }
    while IFS= read -r line; do
        [ -n "${line}" ] || continue
        case "${line}" in
            *=*) key="${line%%=*}" ;;
            *) echo "up printed a line that is not KEY=value: ${line}"; return 0 ;;
        esac
        case "${key}" in
            [A-Z]*) ;;
            *) echo "up printed a key that is not an uppercase shell identifier: ${key}"; return 0 ;;
        esac
        case "${key}" in
            *[!A-Z0-9_]*) echo "up printed a key that is not an uppercase shell identifier: ${key}"; return 0 ;;
        esac
    done <<<"${block}"
    return 0
}

provider_contract_missing_url_key() {
    local block="$1" cfg="$2" name key
    while IFS=$'\t' read -r name key; do
        [ -n "${key}" ] || continue
        # The key is config-supplied: matched as a literal field, never spliced
        # into a regex.
        printf '%s\n' "${block}" | awk -F= -v k="${key}" '$1 == k { found = 1 } END { exit !found }' && continue
        printf '%s\t%s\n' "${name}" "${key}"
        return 0
    done < <(printf '%s' "${cfg}" | jq -r '(.surfaces // {}) | to_entries[] | "\(.key)\t\(.value.url_key)"')
    return 0
}

provider_contract_stderr_tail() {
    local f="$1" n="${2:-3}"
    [ -s "${f}" ] || { printf 'no stderr\n'; return 0; }
    tail -n "${n}" "${f}" | tr '\n' ' ' | sed 's/  */ /g; s/ $//'
}
