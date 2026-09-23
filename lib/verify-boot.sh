#!/usr/bin/env bash
# verify-boot.sh: the preflight and boot of one verification round — the
# Environment provider driven per ADR 0003, plus the launchers the round's
# touched Surfaces need, in one call.
#
# Why this exists: a round used to walk the prerequisite checks, the provider
# boot, the one-retry rule and each app launch as separate agent turns, every
# one of them re-reading the session's context, and each one a place to improvise.
# They are one command here, so the round makes one call and gets the
# environment block back — and so the rules that must not be improvised (the
# `down` before the first `up`, the single retry, the refusal to invent a
# verdict when nothing booted) are executed, not merely written down.
#
# The Harness config is resolved from the checkout this runs in, which in a
# round is the PR head (ADR 0007): the PR that ADDS a provider is verified by
# the provider it adds.
#
# Usage:
#   lib/verify-boot.sh up   --pr <N> [--surface <name>]... [<target-dir>]
#   lib/verify-boot.sh down --pr <N> [<target-dir>]
#
# `up` prints ONLY the sourceable contract on stdout — the provider's own
# `KEY=value` block, plus the harness's own `AUTO_AGENT_SANDBOX` and
# `AUTO_AGENT_SANDBOX_DETAIL` when an app was launched. Export it the way the
# contract says (key, `=`, value to end of line):
#
#   while IFS='=' read -r k v; do [ -n "$k" ] && export "$k=$v"; done <<<"$BLOCK"
#
# Progress, the provider's stderr and every launcher's log go to stderr.
#
# `--surface <name>` names a Surface whose app the round has to drive; repeat
# it. Only `electron` Surfaces have an app to launch — a `browser` one is
# driven through its MCP server, and `cli`/`api` Surfaces are evidence-only —
# so naming any other kind is a no-op, not an error.
#
# `down` is the teardown: every launched app stopped, then the provider's
# `down --pr N`. It is idempotent and runs on EVERY exit path of a round,
# including one that never booted.
#
# Exit codes:
#   0  healthy (and every requested app up)
#   2  usage error, no Harness config, or the provider is not executable
#   3  the Harness config declares no hermetic tier — the Bootstrap state.
#      Nothing booted, and this is not a failure: the round labels the PR for a
#      human verifier and says so
#   4  the environment did not boot (`up` exited 3, or exited 4 twice, or
#      printed no usable block). Teardown has already run. This is an
#      infra-error for the round to report — never an item verdict
#   5  the environment is healthy but an app launch failed. The environment is
#      LEFT UP so the round can retry the launch or continue without that
#      Surface
#
# Env:
#   VERIFY_BOOT_STDERR_LINES  lines of provider stderr quoted in a message (3)

set -uo pipefail

_vb_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_vb_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_vb_lib_dir}/host-env.sh"
# shellcheck source=surfaces.sh
. "${_vb_lib_dir}/surfaces.sh"
# shellcheck source=surface-launch.sh
. "${_vb_lib_dir}/surface-launch.sh"

VB_STDERR_LINES="${VERIFY_BOOT_STDERR_LINES:-3}"

_vb_log() { echo "verify-boot: $*" >&2; }

# _vb_provider <cfg> : the provider's absolute path, or 2/3 with a message
_vb_provider() {
    local cfg="$1" hermetic command target abs
    hermetic="$(printf '%s' "${cfg}" | jq -c '.verification.hermetic // null')"
    if [ "${hermetic}" = "null" ]; then
        _vb_log "the Harness config declares no hermetic tier (Bootstrap state): there is no environment to boot"
        return 3
    fi
    command="$(printf '%s' "${hermetic}" | jq -r '.command')"
    target="$(harness_config_target_dir "${cfg}")" || return 2
    abs="${command}"
    case "${command}" in /*) ;; *) abs="${target}/${command}" ;; esac
    if [ ! -x "${abs}" ]; then
        _vb_log "the hermetic command ${command} is not an executable file under ${target}"
        return 2
    fi
    printf '%s\n' "${abs}"
}

# _vb_tail <file> : the last lines of a provider's stderr, on one line
_vb_tail() {
    [ -s "$1" ] || { printf 'no stderr\n'; return 0; }
    tail -n "${VB_STDERR_LINES}" "$1" | tr '\n' ' ' | sed 's/  */ /g; s/ $//'
}

# _vb_block_keys_ok <block> : 0 when every line is KEY=value with an uppercase
# shell identifier for a key. A round cannot export anything else.
_vb_block_keys_ok() {
    local line key
    [ -n "$1" ] || return 1
    while IFS= read -r line; do
        [ -n "${line}" ] || continue
        case "${line}" in *=*) key="${line%%=*}" ;; *) return 1 ;; esac
        case "${key}" in [A-Z]*) ;; *) return 1 ;; esac
        case "${key}" in *[!A-Z0-9_]*) return 1 ;; esac
    done <<<"$1"
    return 0
}

# _vb_missing_url_key <block> <cfg> : the first declared Surface whose url_key
# the block does not carry, as `<name> (<KEY>)`. A missing one is an
# infra-error in a live round (ADR 0003) — the round cannot reach that Surface.
_vb_missing_url_key() {
    local block="$1" cfg="$2" name key
    while IFS=$'\t' read -r name key; do
        [ -n "${key}" ] || continue
        printf '%s\n' "${block}" | awk -F= -v k="${key}" '$1 == k { found = 1 } END { exit !found }' && continue
        printf '%s (%s)\n' "${name}" "${key}"
        return 0
    done < <(printf '%s' "${cfg}" | jq -r '(.surfaces // {}) | to_entries[] | "\(.key)\t\(.value.url_key)"')
    return 0
}

# verify_boot_up <cfg> <pr> <surface>... : boot, then launch each app
verify_boot_up() {
    local cfg="$1" pr="$2"; shift 2
    local provider rc out err attempt=1 miss
    provider="$(_vb_provider "${cfg}")" || return $?

    err="$(mktemp)" || return 2
    # shellcheck disable=SC2064
    trap "rm -f '${err}'" RETURN

    local target
    target="$(harness_config_target_dir "${cfg}")" || return 2

    # `down` before the first `up`: a killed Fire can leave an environment
    # behind, and `down` is idempotent (ADR 0003).
    _vb_log "down --pr ${pr} (before the first up)"
    ( cd "${target}" && "${provider}" down --pr "${pr}" ) >/dev/null 2>>"${err}"

    while :; do
        _vb_log "up --pr ${pr} (attempt ${attempt}/2)"
        out="$( cd "${target}" && "${provider}" up --pr "${pr}" 2>"${err}" )"
        rc=$?
        if [ "${rc}" -ne 4 ] || [ "${attempt}" -ge 2 ]; then break; fi
        _vb_log "up exited 4 (boot failed); down --pr ${pr} before the one retry"
        ( cd "${target}" && "${provider}" down --pr "${pr}" ) >/dev/null 2>>"${err}"
        attempt=$((attempt + 1))
    done

    case "${rc}" in
        0) ;;
        3) _vb_log "up exited 3 (prerequisite missing): $(_vb_tail "${err}")"
           ( cd "${target}" && "${provider}" down --pr "${pr}" ) >/dev/null 2>&1
           return 4 ;;
        4) _vb_log "up exited 4 (boot failed) on both attempts: $(_vb_tail "${err}")"
           ( cd "${target}" && "${provider}" down --pr "${pr}" ) >/dev/null 2>&1
           return 4 ;;
        *) _vb_log "up exited ${rc}, want 0 healthy, 3 prerequisite missing or 4 boot failed: $(_vb_tail "${err}")"
           ( cd "${target}" && "${provider}" down --pr "${pr}" ) >/dev/null 2>&1
           return 4 ;;
    esac

    if ! _vb_block_keys_ok "${out}"; then
        _vb_log "up exited 0 but its stdout is not a KEY=value block (progress belongs on stderr)"
        ( cd "${target}" && "${provider}" down --pr "${pr}" ) >/dev/null 2>&1
        return 4
    fi

    miss="$(_vb_missing_url_key "${out}" "${cfg}")"
    if [ -n "${miss}" ]; then
        _vb_log "the up block does not carry the url_key of Surface ${miss}: the round cannot reach it"
        ( cd "${target}" && "${provider}" down --pr "${pr}" ) >/dev/null 2>&1
        return 4
    fi

    # Every launcher reads the Surface's URL out of the block, so export it
    # here exactly as the round will.
    local k v
    while IFS='=' read -r k v; do [ -n "${k}" ] && export "${k}=${v}"; done <<<"${out}"

    local name kind sandbox='' detail='' line lrc
    for name in "$@"; do
        [ -n "${name}" ] || continue
        kind="$(surfaces_kind "${cfg}" "${name}")" || {
            _vb_log "no Surface '${name}' is declared in the Harness config"
            printf '%s\n' "${out}"
            return 5
        }
        [ "${kind}" = "electron" ] || continue
        line="$(surface_launch_start "${cfg}" "${name}" "${pr}")"
        lrc=$?
        if [ "${lrc}" -ne 0 ]; then
            _vb_log "launching Surface ${name} failed (${lrc}); the environment is left up"
            printf '%s\n' "${out}"
            return 5
        fi
        case "${line}" in
            "sandbox: DEGRADED"*) sandbox=DEGRADED; detail="${line#sandbox: DEGRADED — }" ;;
            *) [ -n "${sandbox}" ] || sandbox=OK ;;
        esac
        _vb_log "${line}"
    done

    printf '%s\n' "${out}"
    if [ -n "${sandbox}" ]; then
        printf 'AUTO_AGENT_SANDBOX=%s\n' "${sandbox}"
        [ -n "${detail}" ] && printf 'AUTO_AGENT_SANDBOX_DETAIL=%s\n' "${detail}"
    fi
    return 0
}

# verify_boot_down <cfg> <pr> : stop every launched app, then the environment
verify_boot_down() {
    local cfg="$1" pr="$2" provider target name kind rc=0
    while IFS=$'\t' read -r name kind; do
        [ "${kind}" = "electron" ] || continue
        surface_launch_stop "${cfg}" "${name}"
    done < <(printf '%s' "${cfg}" | jq -r '(.surfaces // {}) | to_entries[] | "\(.key)\t\(.value.kind)"')

    provider="$(_vb_provider "${cfg}")" || return 0   # nothing to tear down
    target="$(harness_config_target_dir "${cfg}")" || return 2
    _vb_log "down --pr ${pr}"
    ( cd "${target}" && "${provider}" down --pr "${pr}" ) >/dev/null || rc=$?
    [ "${rc}" -eq 0 ] || _vb_log "down exited ${rc}, want 0 (down is idempotent)"
    return "${rc}"
}

_vb_usage() { sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; }

verify_boot_main() {
    local sub="${1:-}"; shift || true
    case "${sub}" in
        -h|--help|help) _vb_usage; return 0 ;;
        up|down) ;;
        '') _vb_usage >&2; return 2 ;;
        *) echo "verify-boot: unknown subcommand '${sub}'" >&2; _vb_usage >&2; return 2 ;;
    esac

    local pr='' target_arg=''
    local -a want=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --pr) pr="${2:-}"; shift 2 ;;
            --pr=*) pr="${1#--pr=}"; shift ;;
            --surface) [ -n "${2:-}" ] || { echo "verify-boot: --surface needs a name" >&2; return 2; }
                       want+=("$2"); shift 2 ;;
            --surface=*) want+=("${1#--surface=}"); shift ;;
            -*) echo "verify-boot: unknown option '$1'" >&2; return 2 ;;
            *) target_arg="$1"; shift ;;
        esac
    done
    case "${pr}" in ''|*[!0-9]*) echo "verify-boot: --pr <N> is required" >&2; return 2 ;; esac

    local cfg
    cfg="$(harness_config_resolve "${target_arg}")" || return 2

    case "${sub}" in
        up) verify_boot_up "${cfg}" "${pr}" ${want+"${want[@]}"} ;;
        down) verify_boot_down "${cfg}" "${pr}" ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    verify_boot_main "$@"
fi
