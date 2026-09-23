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
#   lib/verify-boot.sh up   --pr <N> [--surface <name>]... [--head] [<target-dir>]
#   lib/verify-boot.sh down --pr <N> [--head] [<target-dir>]
#
# `--head` reads the Harness config from the checkout rather than from an
# inherited HARNESS_CONFIG_JSON, so a round obeys the config the PR head
# carries (ADR 0007) — the PR that ADDS a provider is verified by it.
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
# shellcheck source=provider-contract.sh
. "${_vb_lib_dir}/provider-contract.sh"

VB_STDERR_LINES="${VERIFY_BOOT_STDERR_LINES:-3}"

_vb_log() { echo "verify-boot: $*" >&2; }

# _vb_provider <cfg> : the provider's absolute path, or 2/3 with a message.
# The resolution, the driving and the grammar all come from the one contract
# driver (lib/provider-contract.sh); this lib only decides how a deviation
# reads to a round.
_vb_provider() {
    local cfg="$1" abs rc
    abs="$(provider_contract_resolve "${cfg}")"; rc=$?
    case "${rc}" in
        0) printf '%s\n' "${abs}" ;;
        3) _vb_log "the Harness config declares no hermetic tier (Bootstrap state): there is no environment to boot" ;;
        *) _vb_log "the hermetic command $(printf '%s' "${cfg}" | jq -r '.verification.hermetic.command // ""') is not an executable file under $(harness_config_target_dir "${cfg}")" ;;
    esac
    return "${rc}"
}

# verify_boot_up <cfg> <pr> <surface>... : boot, then launch each app
verify_boot_up() {
    local cfg="$1" pr="$2"; shift 2
    local provider rc out err miss reason target
    provider="$(_vb_provider "${cfg}")" || return $?

    err="$(mktemp)" || return 2
    # shellcheck disable=SC2064
    trap "rm -f '${err}'" RETURN

    target="$(harness_config_target_dir "${cfg}")" || return 2
    # Every failing path below tears the environment down first: a boot that
    # failed can still have left half of one behind (ADR 0003).
    local -a down=(provider_contract_down "${provider}" "${target}" "${pr}")

    # `down` before the first `up`: a killed Fire can leave an environment
    # behind, and `down` is idempotent (ADR 0003).
    _vb_log "down --pr ${pr} (before the first up)"
    "${down[@]}" "${err}"

    _vb_log "up --pr ${pr} (with the one retry)"
    out="$(provider_contract_up "${provider}" "${target}" "${pr}" "${err}")"
    rc=$?

    case "${rc}" in
        0) ;;
        3) _vb_log "up exited 3 (prerequisite missing): $(provider_contract_stderr_tail "${err}" "${VB_STDERR_LINES}")"
           "${down[@]}"; return 4 ;;
        4) _vb_log "up exited 4 (boot failed) on ${PROVIDER_CONTRACT_ATTEMPTS} attempt(s): $(provider_contract_stderr_tail "${err}" "${VB_STDERR_LINES}")"
           "${down[@]}"; return 4 ;;
        *) _vb_log "up exited ${rc}, want 0 healthy, 3 prerequisite missing or 4 boot failed: $(provider_contract_stderr_tail "${err}" "${VB_STDERR_LINES}")"
           "${down[@]}"; return 4 ;;
    esac

    reason="$(provider_contract_block_violation "${out}")"
    if [ -n "${reason}" ]; then
        _vb_log "up exited 0 but its stdout is not a KEY=value block (progress belongs on stderr): ${reason}"
        "${down[@]}"; return 4
    fi

    miss="$(provider_contract_missing_url_key "${out}" "${cfg}")"
    if [ -n "${miss}" ]; then
        _vb_log "the up block does not carry the url_key of Surface ${miss%%$'\t'*} (${miss##*$'\t'}): the round cannot reach it"
        "${down[@]}"; return 4
    fi

    # Every launcher reads the Surface's URL out of the block, so export it
    # here exactly as the round will.
    local k v
    while IFS='=' read -r k v; do [ -n "${k}" ] && export "${k}=${v}"; done <<<"${out}"

    local name kind sandbox='' detail='' lrc
    for name in "$@"; do
        [ -n "${name}" ] || continue
        kind="$(surfaces_kind "${cfg}" "${name}")" || {
            _vb_log "no Surface '${name}' is declared in the Harness config"
            printf '%s\n' "${out}"
            return 5
        }
        surfaces_app_kind "${kind}" || continue
        # Called directly, not through a command substitution: the launcher
        # sets the sandbox globals (and the sandbox environment the app is
        # launched with) in THIS shell. Its human line joins the progress on
        # stderr, where everything but the block belongs.
        surface_launch_start "${cfg}" "${name}" "${pr}" >&2
        lrc=$?
        if [ "${lrc}" -ne 0 ]; then
            _vb_log "launching Surface ${name} failed (${lrc}); the environment is left up"
            printf '%s\n' "${out}"
            return 5
        fi
        # The launcher's verdict is a value it sets, not the prose it printed:
        # one DEGRADED app makes the whole round's sandbox DEGRADED.
        if display_sandbox_degraded "${SURFACE_LAUNCH_SANDBOX_MODE}"; then
            sandbox=DEGRADED
            detail="${SURFACE_LAUNCH_SANDBOX_DETAIL}"
        else
            [ -n "${sandbox}" ] || sandbox=OK
        fi
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
        surfaces_app_kind "${kind}" || continue
        surface_launch_stop "${name}"
    done < <(printf '%s' "${cfg}" | jq -r '(.surfaces // {}) | to_entries[] | "\(.key)\t\(.value.kind)"')

    provider="$(_vb_provider "${cfg}")" || return 0   # nothing to tear down
    target="$(harness_config_target_dir "${cfg}")" || return 2
    _vb_log "down --pr ${pr}"
    provider_contract_down "${provider}" "${target}" "${pr}" /dev/stderr || rc=$?
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

    local pr='' target_arg='' head=0
    local -a want=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --pr) pr="${2:-}"; shift 2 ;;
            --pr=*) pr="${1#--pr=}"; shift ;;
            --surface) [ -n "${2:-}" ] || { echo "verify-boot: --surface needs a name" >&2; return 2; }
                       want+=("$2"); shift 2 ;;
            --surface=*) want+=("${1#--surface=}"); shift ;;
            --head) head=1; shift ;;
            -*) echo "verify-boot: unknown option '$1'" >&2; return 2 ;;
            *) target_arg="$1"; shift ;;
        esac
    done
    case "${pr}" in ''|*[!0-9]*) echo "verify-boot: --pr <N> is required" >&2; return 2 ;; esac

    local cfg
    if [ "${head}" -eq 1 ]; then
        cfg="$(harness_config_resolve_head "${target_arg}")" || return 2
    else
        cfg="$(harness_config_resolve "${target_arg}")" || return 2
    fi

    case "${sub}" in
        up) verify_boot_up "${cfg}" "${pr}" ${want+"${want[@]}"} ;;
        down) verify_boot_down "${cfg}" "${pr}" ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    verify_boot_main "$@"
fi
