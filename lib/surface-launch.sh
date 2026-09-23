#!/usr/bin/env bash
# surface-launch.sh: the launchers and MCP wrappers a verification round drives
# a Surface through, chosen by the Surface's declared kind (ADR 0003).
#
# Why this exists: the verifier needs real tools against a real Surface — a
# headful browser for a `browser` Surface, the Target Project's own desktop app
# attached over the Chrome DevTools Protocol for an `electron` one. Which of
# those a round gets is not a judgement anyone makes per round: it follows from
# the declaration. So the kind selects the launcher here, the Surface's
# `url_key` says where to point it, and its `viewport` says what shape to
# capture — nothing in the harness knows the Target Project's app.
#
# `cli` and `api` Surfaces have no launcher on purpose: they are evidence-only
# (ADR 0003), driven by the verifier's own shell and HTTP calls.
#
# Usage:
#   lib/surface-launch.sh mcp        <surface> [<target-dir>] [-- <extra MCP args>]
#   lib/surface-launch.sh mcp-config [<target-dir>] [--out <file>]
#   lib/surface-launch.sh start <surface> [--pr <N>] [<target-dir>]
#   lib/surface-launch.sh stop  <surface> [<target-dir>]
#
# `mcp` execs the MCP server for a Surface; it is what an `.mcp.json` entry
# points at, so the server registers at session start and the round drives it
# later. For a `browser` Surface that is a headful browser on the Host display,
# on a fresh profile per run. For an `electron` Surface it is an attach to the
# app's CDP endpoint — it hands off even when the endpoint is not up yet,
# because the app is launched minutes after the server registers; the client
# dials lazily, so a premature tool call fails on its own instead of costing
# the session its Electron tools.
#
# `mcp-config` renders the MCP server registry a Fire loads: one entry per
# `browser` and `electron` Surface, each running `mcp` for that Surface. The
# Surface names come from the Harness config, so the registry is rendered per
# Target Project rather than shipped as a static file.
#
# `start`/`stop` are the `electron` lifecycle: launcher-start, CDP-attach,
# drive, launcher-stop. `start` needs the Surface's `launcher` (the Target
# Project's executable that opens its app) and the environment block the
# provider's `up` printed; it blocks until CDP answers and records a pidfile in
# the State dir. `stop` is idempotent and safe on every exit path, including
# one where `start` never ran.
#
# Sandbox: `start` resolves the Electron sandbox mode (lib/display-env.sh) and
# prints `sandbox: OK|DEGRADED — <detail>` on stdout. DEGRADED means the app
# started with its sandbox off because no AppArmor profile grants it user
# namespaces on this Host — the round goes on and SAYS SO; it is never silent
# and never a reason to skip.
#
# Exit codes:
#   0  handed off (`mcp`), app up (`start`), nothing running (`stop`)
#   2  usage error, no Harness config, unknown Surface, or a kind with no
#      launcher (`cli`/`api` are evidence-only)
#   3  no display on this Host (an infra finding; never a headless fallback)
#   4  the Surface declares no `launcher`, or its `url_key` is not in the
#      environment the provider's `up` printed
#   5  the app's CDP endpoint never answered
#
# Env:
#   SURFACE_LAUNCH_CDP_PORT    the app's remote-debugging port (default 9222)
#   SURFACE_LAUNCH_PROBE_CMD   CDP readiness probe (default a curl of it)
#   SURFACE_LAUNCH_RETRIES     bounded readiness attempts (default 60 for
#                              `start`, 5 for `mcp` — the MCP runtime drops a
#                              server that takes too long to register)
#   SURFACE_LAUNCH_INTERVAL    seconds between attempts (default 1)
#   SURFACE_LAUNCH_MCP_CMD     the MCP server command (default the Playwright
#                              MCP server through npx)
#   SURFACE_LAUNCH_RUN_DIR     pidfiles and profiles (default <state>/verify)

set -uo pipefail

_sl_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_sl_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_sl_lib_dir}/host-env.sh"
# shellcheck source=display-env.sh
. "${_sl_lib_dir}/display-env.sh"
# shellcheck source=surfaces.sh
. "${_sl_lib_dir}/surfaces.sh"

SURFACE_LAUNCH_CDP_PORT="${SURFACE_LAUNCH_CDP_PORT:-9222}"
SURFACE_LAUNCH_INTERVAL="${SURFACE_LAUNCH_INTERVAL:-1}"

_sl_log() { echo "[surface-launch] $*" >&2; }

_sl_run_dir() {
    printf '%s\n' "${SURFACE_LAUNCH_RUN_DIR:-$(host_env_state_dir)/verify}"
}

_sl_pidfile() {
    printf '%s\n' "$(_sl_run_dir)/$1.pid"
}

_sl_cdp_endpoint() {
    printf 'http://127.0.0.1:%s\n' "${SURFACE_LAUNCH_CDP_PORT}"
}

# _sl_wait_cdp <retries> : poll the CDP probe; 0 once it answers
_sl_wait_cdp() {
    local retries="$1" probe attempt=0
    probe="${SURFACE_LAUNCH_PROBE_CMD:-curl -sf $(_sl_cdp_endpoint)/json/version}"
    while [ "${attempt}" -lt "${retries}" ]; do
        bash -c "${probe}" >/dev/null 2>&1 && return 0
        attempt=$((attempt + 1))
        sleep "${SURFACE_LAUNCH_INTERVAL}"
    done
    return 1
}

# _sl_mcp_cmd : the MCP server command, as an array in SL_MCP_CMD
_sl_mcp_cmd() {
    if [ -n "${SURFACE_LAUNCH_MCP_CMD:-}" ]; then
        # shellcheck disable=SC2206
        SL_MCP_CMD=(${SURFACE_LAUNCH_MCP_CMD})
    else
        SL_MCP_CMD=(npx -y @playwright/mcp@latest)
    fi
}

# surface_launch_mcp <cfg> <name> [extra args...] : exec the Surface's MCP server
surface_launch_mcp() {
    local cfg="$1" name="$2"; shift 2
    local kind viewport
    kind="$(surfaces_kind "${cfg}" "${name}")" || {
        echo "surface-launch: no Surface '${name}' is declared in the Harness config" >&2
        return 2
    }
    _sl_mcp_cmd

    case "${kind}" in
        browser)
            DISPLAY_ENV_LOG_PREFIX="surface-launch" display_env_resolve || return $?
            viewport="$(surfaces_viewport "${cfg}" "${name}")"
            local run_dir profile
            run_dir="$(_sl_run_dir)/profiles"
            mkdir -p "${run_dir}" || return 2
            # A fresh profile per run: no cookie, permission or session from an
            # earlier round can change what this round sees.
            profile="$(mktemp -d "${run_dir}/${name}-XXXXXX")" || return 2
            _sl_log "browser Surface ${name}: headful on DISPLAY=${DISPLAY}, profile ${profile}"
            exec "${SL_MCP_CMD[@]}" \
                --browser chrome \
                --user-data-dir "${profile}" \
                --viewport-size "${viewport/x/,}" \
                "$@"
            ;;
        electron)
            local endpoint retries
            endpoint="$(_sl_cdp_endpoint)"
            retries="${SURFACE_LAUNCH_RETRIES:-5}"
            if _sl_wait_cdp "${retries}"; then
                _sl_log "electron Surface ${name}: CDP at ${endpoint} is up — attaching"
            else
                _sl_log "electron Surface ${name}: CDP at ${endpoint} did not answer yet."
                _sl_log "Registering the MCP server anyway so its tools exist for this session;"
                _sl_log "the client dials the endpoint lazily, on the first tool call. Start the"
                _sl_log "app with 'surface-launch start ${name}' BEFORE calling a tool."
            fi
            exec "${SL_MCP_CMD[@]}" --cdp-endpoint "${endpoint}" "$@"
            ;;
        *)
            echo "surface-launch: ${kind} Surface '${name}' has no launcher — ${kind} Surfaces are evidence-only (ADR 0003)" >&2
            return 2
            ;;
    esac
}

# surface_launch_mcp_config <cfg> <target-hint> : the MCP server registry for
# this Target Project's UI Surfaces, as JSON on stdout. One entry per `browser`
# and `electron` Surface, each pointing back at this CLI: the Surface names
# come from the config, so the registry cannot be a static file in the plugin.
# `cli` and `api` Surfaces are evidence-only and get no server.
surface_launch_mcp_config() {
    local cfg="$1" target_hint="${2:-}" cli target
    cli="$(cd "${_sl_lib_dir}/.." && pwd)/bin/auto-agent"
    target="${target_hint}"
    [ -n "${target}" ] || target="$(harness_config_target_dir "${cfg}" 2>/dev/null)" || target=''
    printf '%s' "${cfg}" | jq --arg cli "${cli}" --arg target "${target}" '
        {mcpServers: ((.surfaces // {})
          | with_entries(select(.value.kind == "browser" or .value.kind == "electron"))
          | to_entries
          | map({key: ("surface-" + .key),
                 value: {command: $cli,
                         args: (["surface-launch", "mcp", .key] + (if $target == "" then [] else [$target] end))}})
          | from_entries)}'
}

# surface_launch_start <cfg> <name> <pr> : launch an electron Surface's app
surface_launch_start() {
    local cfg="$1" name="$2" pr="$3"
    local kind launcher url_key url target abs mode pidfile

    kind="$(surfaces_kind "${cfg}" "${name}")" || {
        echo "surface-launch: no Surface '${name}' is declared in the Harness config" >&2
        return 2
    }
    if [ "${kind}" != "electron" ]; then
        echo "surface-launch start: ${kind} Surface '${name}' has no app to launch (only electron Surfaces do)" >&2
        return 2
    fi

    launcher="$(printf '%s' "${cfg}" | jq -r --arg n "${name}" '(.surfaces[$n].launcher // "")')"
    if [ -z "${launcher}" ]; then
        echo "surface-launch start: electron Surface '${name}' declares no launcher; the harness does not know how to open this project's app" >&2
        return 4
    fi
    url_key="$(printf '%s' "${cfg}" | jq -r --arg n "${name}" '.surfaces[$n].url_key')"
    url="${!url_key:-}"
    if [ -z "${url}" ]; then
        echo "surface-launch start: ${url_key} is not in the environment — the provider's up block has to be exported before the app is launched" >&2
        return 4
    fi

    target="$(harness_config_target_dir "${cfg}")" || return 2
    abs="${launcher}"
    case "${launcher}" in /*) ;; *) abs="${target}/${launcher}" ;; esac
    if [ ! -x "${abs}" ]; then
        echo "surface-launch start: the launcher ${launcher} is not an executable file under ${target}" >&2
        return 4
    fi

    DISPLAY_ENV_LOG_PREFIX="surface-launch" display_env_resolve || return $?
    # The mode is read out of a command substitution, so the export the lib
    # does inside it dies with that subshell: set the flag here, where the app
    # is actually launched from.
    mode="$(display_sandbox_mode "${abs}")"
    if display_sandbox_degraded "${mode}"; then
        export ELECTRON_DISABLE_SANDBOX=1
    fi

    mkdir -p "$(_sl_run_dir)" || return 2
    pidfile="$(_sl_pidfile "${name}")"

    _sl_log "starting ${launcher} for Surface ${name} (pr ${pr}) on DISPLAY=${DISPLAY}, CDP on port ${SURFACE_LAUNCH_CDP_PORT}"
    ( cd "${target}" && "${abs}" "--remote-debugging-port=${SURFACE_LAUNCH_CDP_PORT}" ) &
    local child=$!
    echo "${child}" > "${pidfile}"

    if ! _sl_wait_cdp "${SURFACE_LAUNCH_RETRIES:-60}"; then
        _sl_log "the app's CDP endpoint never answered — stopping it so a failed start leaves nothing behind"
        kill "${child}" 2>/dev/null
        rm -f "${pidfile}"
        return 5
    fi

    if display_sandbox_degraded "${mode}"; then
        echo "sandbox: DEGRADED — ${name} started with ELECTRON_DISABLE_SANDBOX=1 (no AppArmor profile grants user namespaces to ${launcher} on this Host)"
    else
        echo "sandbox: OK — ${name} started with its sandbox on (${mode})"
    fi
    return 0
}

# surface_launch_stop <cfg> <name> : idempotent teardown of a launched app
surface_launch_stop() {
    local name="$2" pidfile pid
    pidfile="$(_sl_pidfile "${name}")"
    if [ ! -f "${pidfile}" ]; then
        _sl_log "no pidfile at ${pidfile} — nothing to stop"
        return 0
    fi
    pid="$(cat "${pidfile}" 2>/dev/null)"
    if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
        _sl_log "stopping the app for Surface ${name} (pid ${pid})"
        kill "${pid}" 2>/dev/null
    else
        _sl_log "pid ${pid:-<empty>} is already gone — clearing the stale pidfile"
    fi
    rm -f "${pidfile}"
    return 0
}

_sl_usage() { sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; }

surface_launch_main() {
    local sub="${1:-}"; shift || true
    case "${sub}" in
        -h|--help|help) _sl_usage; return 0 ;;
        mcp-config)
            local out='' target=''
            while [ $# -gt 0 ]; do
                case "$1" in
                    --out) out="${2:-}"; shift 2 ;;
                    --out=*) out="${1#--out=}"; shift ;;
                    -*) echo "surface-launch: unknown option '$1'" >&2; return 2 ;;
                    *) target="$1"; shift ;;
                esac
            done
            local cfg_json
            cfg_json="$(harness_config_resolve "${target}")" || return 2
            if [ -n "${out}" ]; then
                mkdir -p "$(dirname "${out}")" || return 2
                surface_launch_mcp_config "${cfg_json}" "${target}" > "${out}" || return 2
                printf '%s\n' "${out}"
            else
                surface_launch_mcp_config "${cfg_json}" "${target}"
            fi
            return 0 ;;
        mcp|start|stop) ;;
        '') _sl_usage >&2; return 2 ;;
        *) echo "surface-launch: unknown subcommand '${sub}'" >&2; _sl_usage >&2; return 2 ;;
    esac

    local name="${1:-}"; shift || true
    if [ -z "${name}" ] || [ "${name#-}" != "${name}" ]; then
        echo "surface-launch ${sub}: a Surface name is required" >&2
        return 2
    fi

    local pr=0 target_arg=''
    local -a extra=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --) shift; extra=("$@"); break ;;
            --pr) pr="${2:-}"; shift 2 ;;
            --pr=*) pr="${1#--pr=}"; shift ;;
            -*) echo "surface-launch: unknown option '$1'" >&2; return 2 ;;
            *) target_arg="$1"; shift ;;
        esac
    done
    case "${pr}" in ''|*[!0-9]*) echo "surface-launch: --pr needs a number" >&2; return 2 ;; esac

    local cfg
    cfg="$(harness_config_resolve "${target_arg}")" || return 2

    case "${sub}" in
        mcp) surface_launch_mcp "${cfg}" "${name}" ${extra+"${extra[@]}"} ;;
        start) surface_launch_start "${cfg}" "${name}" "${pr}" ;;
        stop) surface_launch_stop "${cfg}" "${name}" ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    surface_launch_main "$@"
fi
