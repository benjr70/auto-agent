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
#   lib/provider-check.sh [--pr <N>] [<target-dir>]
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

# _pc_tail <file> : the last lines of a provider's stderr, on one line, for a
# verdict that has to say WHY the provider could not boot.
_pc_tail() {
    local f="$1"
    [ -s "${f}" ] || { printf 'no stderr\n'; return 0; }
    tail -n "${PC_STDERR_LINES}" "${f}" | tr '\n' ' ' | sed 's/  */ /g; s/ $//'
}

# _pc_run <target> <command> <errfile> <args...> : one provider invocation from
# the Target Project root, stdout captured, stderr to <errfile>. Returns the
# provider's own exit code.
_pc_run() {
    local target="$1" command="$2" err="$3"; shift 3
    ( cd "${target}" && "${command}" "$@" ) 2>"${err}"
}

# _pc_block_violation <block> : echo the reason the KEY=value block is not one,
# or nothing when it conforms. The grammar is ADR 0003's: uppercase shell
# identifier, `=`, value to end of line.
_pc_block_violation() {
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

# _pc_missing_url_key <block> <config> : echo `<surface> url_key <KEY>` for the
# first declared Surface whose key the block does not carry. A missing url_key
# is an infra-error in a live round (ADR 0003); here it is the failure the
# check exists to catch before the round.
_pc_missing_url_key() {
    local block="$1" cfg="$2" name key
    while IFS=$'\t' read -r name key; do
        [ -n "${key}" ] || continue
        printf '%s\n' "${block}" | grep -q "^${key}=" && continue
        printf '%s\t%s\n' "${name}" "${key}"
        return 0
    done < <(printf '%s' "${cfg}" | jq -r '(.surfaces // {}) | to_entries[] | "\(.key)\t\(.value.url_key)"')
    return 0
}

# provider_check [--pr <N>] [<target-dir>] : the whole run. One stdout verdict.
provider_check() {
    local pr=0 target_arg=''
    while [ $# -gt 0 ]; do
        case "$1" in
            --pr) [ $# -ge 2 ] || { echo "provider-check: --pr needs a number" >&2; return 2; }
                  pr="$2"; shift 2 ;;
            --pr=*) pr="${1#--pr=}"; shift ;;
            -h|--help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; return 0 ;;
            -*) echo "provider-check: unknown option '$1'" >&2; return 2 ;;
            *) target_arg="$1"; shift ;;
        esac
    done
    case "${pr}" in ''|*[!0-9]*) echo "provider-check: --pr needs a number" >&2; return 2 ;; esac

    local cfg
    cfg="$(harness_config_resolve "${target_arg}")" || return 2

    local hermetic command smoke target
    hermetic="$(printf '%s' "${cfg}" | jq -c '.verification.hermetic // null')"
    if [ "${hermetic}" = "null" ]; then
        _pc_verdict 3 "the Harness config declares no hermetic tier (Bootstrap state): no Environment provider to check"
        return 3
    fi
    command="$(printf '%s' "${hermetic}" | jq -r '.command')"
    smoke="$(printf '%s' "${hermetic}" | jq -r '.smoke')"
    target="$(harness_config_target_dir "${cfg}")" || {
        echo "provider-check: the resolved Harness config carries no config_dir" >&2
        return 2
    }

    # The provider is resolved against the Target Project root, the directory
    # every lane runs it from, so `verify/provider` means the same thing here
    # as it does in a verification round.
    local abs="${command}"
    case "${command}" in /*) ;; *) abs="${target}/${command}" ;; esac
    if [ ! -x "${abs}" ]; then
        _pc_verdict 1 "the hermetic command ${command} is not an executable file under ${target}"
        return 1
    fi

    local err; err="$(mktemp)"
    # shellcheck disable=SC2064
    trap "rm -f '${err}'" RETURN

    local checks=0 rc out reason

    # 1. down before the first up (ADR 0003: down runs on every path, and it is
    #    idempotent, so a down with nothing to stop still exits 0).
    _pc_step "down --pr ${pr} (before the first up)"
    _pc_run "${target}" "${abs}" "${err}" down --pr "${pr}" >/dev/null
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        _pc_verdict 1 "down before the first up exited ${rc}, want 0 (down is idempotent): $(_pc_tail "${err}")"
        return 1
    fi
    checks=$((checks + 1))

    # 2. up, with the harness's one retry. Only exit 4 (boot failed) is
    #    retried, and only after another down; exit 3 says the prerequisite is
    #    missing and nothing booted, so a retry would fail the same way.
    local attempt=1 max=2
    while :; do
        _pc_step "up --pr ${pr} (attempt ${attempt}/${max})"
        out="$(_pc_run "${target}" "${abs}" "${err}" up --pr "${pr}")"
        rc=$?
        [ "${rc}" -eq 4 ] && [ "${attempt}" -lt "${max}" ] || break
        _pc_step "up exited 4 (boot failed); down --pr ${pr} before the one retry"
        _pc_run "${target}" "${abs}" "${err}.down" down --pr "${pr}" >/dev/null
        local down_rc=$?
        if [ "${down_rc}" -ne 0 ]; then
            _pc_verdict 1 "down between the one retry exited ${down_rc}, want 0: $(_pc_tail "${err}.down")"
            rm -f "${err}.down"
            return 1
        fi
        rm -f "${err}.down"
        attempt=$((attempt + 1))
    done
    case "${rc}" in
        0) ;;
        3) _pc_verdict 4 "up exited 3 (prerequisite missing): $(_pc_tail "${err}")"; return 4 ;;
        4) _pc_verdict 4 "up exited 4 (boot failed) on both attempts: $(_pc_tail "${err}")"; return 4 ;;
        *) _pc_verdict 1 "up exited ${rc}, want 0 healthy, 3 prerequisite missing or 4 boot failed: $(_pc_tail "${err}")"; return 1 ;;
    esac
    checks=$((checks + 1))

    # From here every exit path tears the environment down before returning.
    _pc_down_and_verdict() {
        local vrc="$1" vtext="$2" drc
        _pc_step "down --pr ${pr} (after the run)"
        _pc_run "${target}" "${abs}" "${err}.post" down --pr "${pr}" >/dev/null
        drc=$?
        rm -f "${err}.post"
        if [ "${vrc}" -eq 0 ] && [ "${drc}" -ne 0 ]; then
            _pc_verdict 1 "down after up exited ${drc}, want 0 (down is idempotent)"
            return 1
        fi
        _pc_verdict "${vrc}" "${vtext}"
        return "${vrc}"
    }

    # 3. the block grammar
    reason="$(_pc_block_violation "${out}")"
    if [ -n "${reason}" ]; then
        _pc_down_and_verdict 1 "${reason}"
        return 1
    fi
    checks=$((checks + 1))

    # 4. every declared Surface's url_key is in the block
    local miss miss_name miss_key
    miss="$(_pc_missing_url_key "${out}" "${cfg}")"
    if [ -n "${miss}" ]; then
        miss_name="${miss%%$'\t'*}"; miss_key="${miss##*$'\t'}"
        _pc_down_and_verdict 1 "surface ${miss_name} declares url_key ${miss_key}, which the up block does not carry"
        return 1
    fi
    checks=$((checks + 1))

    # 5. smoke, with the up block exported into its environment
    if [ "${smoke}" = "true" ]; then
        _pc_step "smoke (with the up block exported)"
        local smoke_out last
        smoke_out="$( {
            while IFS='=' read -r k v; do [ -n "${k}" ] && export "${k}=${v}"; done <<<"${out}"
            cd "${target}" && "${abs}" smoke
        } 2>"${err}" )"
        rc=$?
        # The contract puts the verdict on the LAST stdout line, so that is the
        # line read, not the first `smoke:` anywhere in the output.
        last="$(printf '%s\n' "${smoke_out}" | grep -v '^[[:space:]]*$' | tail -1)"
        case "${rc}" in
            0)
                if [[ "${last}" != smoke:\ PASS* ]]; then
                    _pc_down_and_verdict 1 "smoke exited 0 but its last stdout line is not 'smoke: PASS (…)': ${last:-no output}"
                    return 1
                fi ;;
            1)
                if [[ "${last}" != smoke:\ FAIL* ]]; then
                    _pc_down_and_verdict 1 "smoke exited 1 but its last stdout line is not 'smoke: FAIL (…)': ${last:-no output}"
                    return 1
                fi
                _pc_down_and_verdict 1 "smoke exited 1: ${last}"
                return 1 ;;
            2)
                _pc_down_and_verdict 4 "smoke exited 2 (could not run): $(_pc_tail "${err}")"
                return 4 ;;
            *)
                _pc_down_and_verdict 1 "smoke exited ${rc}, want 0 pass, 1 fail or 2 could not run"
                return 1 ;;
        esac
        checks=$((checks + 1))
    fi

    # 6. down after the run
    checks=$((checks + 1))
    _pc_down_and_verdict 0 "${command} conforms (${checks} checks, pr ${pr})"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    provider_check "$@"
fi
