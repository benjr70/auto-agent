#!/usr/bin/env bash
# provider-check.sh: the Provider check, the harness-owned conformance run that
# drives a Target Project's Environment provider through the ADR 0003 contract
# and prints one verdict (ticket #18, item 4).
#
# Why this exists: the contract is four exit codes, a stdout grammar and an
# ordering rule (`down` before the first `up`, and between the one retry).
# Without a checker the first thing that reads a new provider is a live
# verification round, where a missing `url_key` surfaces as an infra-error
# comment on an Agent PR an hour later. This answers "does my provider
# conform" in one command, with no checklist round, no PR and no Claude: it is
# what Setup's verify stage runs, and what the maintainer writing a provider
# iterates against.
#
# Usage:
#   lib/provider-check.sh [--pr <N> | --pr=<N>] [<target-dir>]
#
# The Harness config comes from harness_config_resolve (HARNESS_CONFIG_JSON,
# <target-dir>, or AUTO_AGENT_TARGET_DIR from the Host env). `--pr` is the PR
# number the environment is booted for (default 0); the check never writes to
# GitHub and the number only has to be one nothing else is using.
#
# What it drives, in this order:
#   1. `down --pr N`   before the first `up`, and its exit code must be 0
#   2. `up --pr N`     exit 0 healthy | 3 prerequisite missing | 4 boot failed;
#                      on 4 it runs `down --pr N` again and retries ONCE
#   3. the block       every stdout line of a healthy `up` is KEY=value with an
#                      uppercase shell identifier for a key
#   4. the url_keys    every Surface's `url_key` is a key of that block
#   5. `smoke`         only when `hermetic.smoke` is true, with the whole block
#                      exported into its environment; its last stdout line is
#                      `smoke: PASS (…)` or `smoke: FAIL (…)`, exit 0 | 1 | 2
#   6. `down --pr N`   again, on every path, and its exit code must be 0
#
# Output (stdout): exactly one verdict line, the last thing printed.
#   provider-check: PASS — <command> conforms (<n> checks, pr <N>)
#   provider-check: FAIL — <reason>
#   provider-check: BOOTSTRAP — <reason>   (exit 3: there is no provider yet)
# Progress and the provider's own stderr go to stderr.
#
# Exit codes:
#   0  the provider conforms
#   1  a contract violation; the verdict names it
#   2  usage error, or no Harness config could be resolved
#   3  the Harness config declares no hermetic tier (Bootstrap state): there is
#      no provider to check, which is not a failure
#   4  the environment could not be booted on this machine: `up` exited 3
#      (prerequisite missing) or exited 4 twice, or `smoke` exited 2. The
#      provider may still be conformant; this machine could not run it.
#
# Env:
#   PROVIDER_CHECK_STDERR_LINES  lines of provider stderr quoted in a verdict
#                                (default 3)

set -uo pipefail

_provider_check_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_provider_check_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_provider_check_lib_dir}/host-env.sh"
# shellcheck source=provider-contract.sh
. "${_provider_check_lib_dir}/provider-contract.sh"

PC_STDERR_LINES="${PROVIDER_CHECK_STDERR_LINES:-3}"

_pc_step() { echo "provider-check: $*" >&2; }

# _pc_verdict <rc> <text> : the one stdout line, then the exit code
_pc_verdict() {
    local rc="$1" label; shift
    case "${rc}" in
        0) label=PASS ;;
        3) label=BOOTSTRAP ;;   # nothing to check is not a failure
        *) label=FAIL ;;
    esac
    echo "provider-check: ${label} — $*"
    return "${rc}"
}

# _pc_tail <errname> : the last lines of one call's stderr, on one line, for a
# verdict that has to say WHY the provider could not boot.
_pc_tail() {
    provider_contract_stderr_tail "${_PC_TMP}/$1" "${PC_STDERR_LINES}"
}

# The run's state, set once by provider_check: the Target Project root, the
# resolved provider, the PR number every call carries, and the scratch dir
# holding each call's stderr. They are what every _pc_ helper below would
# otherwise take as four parameters at a dozen call sites.
_PC_TARGET=''; _PC_ABS=''; _PC_PR=''; _PC_TMP=''

# _pc_run <errname> <args...> : one provider invocation from the Target Project
# root, stdout captured, stderr into <_PC_TMP>/<errname>. Returns the
# provider's own exit code.
_pc_run() {
    local err="${_PC_TMP}/$1"; shift
    ( cd "${_PC_TARGET}" && "${_PC_ABS}" "$@" ) 2>"${err}"
}

# _pc_teardown_verdict <rc> <text> : the exit path from a booted environment.
# Every path after a successful `up` goes through here, including a failed one
# (ADR 0003: `down` runs on every path, and a failed boot can leave half a
# stack behind). A failing `down` becomes the verdict only when there is no
# earlier failure to report: the first thing that went wrong is the useful one.
_pc_teardown_verdict() {
    local vrc="$1" vtext="$2" drc
    _pc_step "down --pr ${_PC_PR} (after the run)"
    _pc_run down.err down --pr "${_PC_PR}" >/dev/null
    drc=$?
    if [ "${vrc}" -eq 0 ] && [ "${drc}" -ne 0 ]; then
        _pc_verdict 1 "down after up exited ${drc}, want 0 (down is idempotent)"
        return
    fi
    _pc_verdict "${vrc}" "${vtext}"
}

# provider_check [--pr <N>] [<target-dir>] : the whole run. One stdout verdict.
provider_check() {
    local pr=0 target_arg=''
    while [ $# -gt 0 ]; do
        case "$1" in
            --pr) [ $# -ge 2 ] || { echo "provider-check: --pr needs a number" >&2; return 2; }
                  pr="$2"; shift 2 ;;
            --pr=*) pr="${1#--pr=}"; shift ;;
            -h|--help) sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; return 0 ;;
            -*) echo "provider-check: unknown option '$1'" >&2; return 2 ;;
            *) target_arg="$1"; shift ;;
        esac
    done
    case "${pr}" in ''|*[!0-9]*) echo "provider-check: --pr needs a number" >&2; return 2 ;; esac

    local cfg
    cfg="$(harness_config_resolve "${target_arg}")" || return 2

    local command smoke rrc
    command="$(printf '%s' "${cfg}" | jq -r '.verification.hermetic.command // ""')"
    smoke="$(printf '%s' "${cfg}" | jq -r '.verification.hermetic.smoke // false')"
    # The provider is resolved against the Target Project root, the directory
    # every lane runs it from, so `verify/provider` means the same thing here
    # as it does in a verification round.
    _PC_ABS="$(provider_contract_resolve "${cfg}")"; rrc=$?
    _PC_TARGET="$(harness_config_target_dir "${cfg}")" || {
        echo "provider-check: the resolved Harness config carries no config_dir" >&2
        return 2
    }
    _PC_PR="${pr}"
    case "${rrc}" in
        0) ;;
        3) _pc_verdict 3 "the Harness config declares no hermetic tier (Bootstrap state): no Environment provider to check"
           return ;;
        *) _pc_verdict 1 "the hermetic command ${command} is not an executable file under ${_PC_TARGET}"
           return ;;
    esac

    # One scratch dir for every call's stderr, removed on every return.
    _PC_TMP="$(mktemp -d)" || { echo "provider-check: cannot create a scratch dir" >&2; return 2; }
    # shellcheck disable=SC2064
    trap "rm -rf '${_PC_TMP}'" RETURN

    local checks=0 rc out reason

    # 1. down before the first up (ADR 0003: down runs on every path, and it is
    #    idempotent, so a down with nothing to stop still exits 0).
    _pc_step "down --pr ${pr} (before the first up)"
    _pc_run pre.err down --pr "${pr}" >/dev/null
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        _pc_verdict 1 "down before the first up exited ${rc}, want 0 (down is idempotent): $(_pc_tail pre.err)"
        return
    fi
    checks=$((checks + 1))

    # 2. up, with the harness's one retry (lib/provider-contract.sh drives it:
    #    only exit 4 is retried, after another down). The check is the one
    #    front-end that also JUDGES that down: a down which cannot clear a
    #    failed boot is a contract violation, not just bad luck.
    _pc_step "up --pr ${pr} (with the one retry)"
    out="$(provider_contract_up "${_PC_ABS}" "${_PC_TARGET}" "${pr}" "${_PC_TMP}/up.err")"
    rc=$?
    if [ -n "${PROVIDER_CONTRACT_RETRY_DOWN_RC}" ] && [ "${PROVIDER_CONTRACT_RETRY_DOWN_RC}" -ne 0 ]; then
        _pc_verdict 1 "down between the one retry exited ${PROVIDER_CONTRACT_RETRY_DOWN_RC}, want 0 (down is idempotent): $(_pc_tail up.err)"
        return
    fi
    case "${rc}" in
        0) ;;
        3) _pc_teardown_verdict 4 "up exited 3 (prerequisite missing): $(_pc_tail up.err)"; return ;;
        4) _pc_teardown_verdict 4 "up exited 4 (boot failed) on both attempts: $(_pc_tail up.err)"; return ;;
        *) _pc_teardown_verdict 1 "up exited ${rc}, want 0 healthy, 3 prerequisite missing or 4 boot failed: $(_pc_tail up.err)"; return ;;
    esac
    checks=$((checks + 1))

    # 3. the block grammar
    reason="$(provider_contract_block_violation "${out}")"
    if [ -n "${reason}" ]; then
        _pc_teardown_verdict 1 "${reason}"
        return
    fi
    checks=$((checks + 1))

    # 4. every declared Surface's url_key is in the block
    local miss miss_name miss_key
    miss="$(provider_contract_missing_url_key "${out}" "${cfg}")"
    if [ -n "${miss}" ]; then
        miss_name="${miss%%$'\t'*}"; miss_key="${miss##*$'\t'}"
        _pc_teardown_verdict 1 "surface ${miss_name} declares url_key ${miss_key}, which the up block does not carry"
        return
    fi
    checks=$((checks + 1))

    # 5. smoke, with the up block exported into its environment
    if [ "${smoke}" = "true" ]; then
        _pc_step "smoke (with the up block exported)"
        local smoke_out last
        smoke_out="$( {
            while IFS='=' read -r k v; do [ -n "${k}" ] && export "${k}=${v}"; done <<<"${out}"
            cd "${_PC_TARGET}" && "${_PC_ABS}" smoke
        } 2>"${_PC_TMP}/smoke.err" )"
        rc=$?
        # The contract puts the verdict on the LAST stdout line, so that is the
        # line read, not the first `smoke:` anywhere in the output.
        last="$(printf '%s\n' "${smoke_out}" | grep -v '^[[:space:]]*$' | tail -1)"
        case "${rc}" in
            0)
                if [[ "${last}" != smoke:\ PASS* ]]; then
                    _pc_teardown_verdict 1 "smoke exited 0 but its last stdout line is not 'smoke: PASS (…)': ${last:-no output}"
                    return
                fi ;;
            1)
                if [[ "${last}" != smoke:\ FAIL* ]]; then
                    _pc_teardown_verdict 1 "smoke exited 1 but its last stdout line is not 'smoke: FAIL (…)': ${last:-no output}"
                    return
                fi
                _pc_teardown_verdict 1 "smoke exited 1: ${last}"
                return ;;
            2)
                _pc_teardown_verdict 4 "smoke exited 2 (could not run): $(_pc_tail smoke.err)"
                return ;;
            *)
                _pc_teardown_verdict 1 "smoke exited ${rc}, want 0 pass, 1 fail or 2 could not run"
                return ;;
        esac
        checks=$((checks + 1))
    fi

    # 6. down after the run
    checks=$((checks + 1))
    _pc_teardown_verdict 0 "${command} conforms (${checks} checks, pr ${pr})"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    provider_check "$@"
fi
