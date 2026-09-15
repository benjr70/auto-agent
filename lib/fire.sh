#!/usr/bin/env bash
# Fire wrapper: one Daemon pass. Replaces Smart-Smoker-V2's `agent-run`
# invocation (ADR 0001): every Fire loads the pinned Harness install as a
# plugin with the `--settings` baseline, runs `claude -p` in stream-json
# through the rate-limit tap, and ends by writing a Fire record. Text mode is
# gone. Lane orchestration (branch hygiene, pause on exhaustion, lock cleanup)
# is not here yet; it arrives with the core-loop Slices.
#
# Source this file, then:
#
#   fire_run [--dry-run] [<target-dir>]
#       Runs one Fire against the Target Project at <target-dir> (default:
#       AUTO_AGENT_TARGET_DIR from the Host env; for --dry-run, the fixture
#       Target Project in the plugin). Validates the Harness config first and
#       fails closed; a preflight failure is still a Fire and still gets a
#       record (exit 1, phase "preflight"). Returns the claude exit code, or 1
#       when a dry-run did not prove the plugin loaded and the skill ran.
#
# Writes into the State dir (host_env_state_dir):
#   logs/<fire-id>.stream.jsonl   the raw stream-json the Fire produced
#   logs/<fire-id>.stderr.log     claude's stderr
#   rate-limits.json / .jsonl     from the tap (lib/rate-limits-tap.sh)
#   fires/<fire-id>.json          the Fire record (lib/fire-record.sh)
#
# Prints stable lines on stdout for the Daemon and Setup's verify stage:
#   fire: id=<id> kind=<kind> exit=<n> record=<path>
#   fire: plugin auto-agent loaded=<yes|no> skill=<name> listed=<yes|no>
#   fire: dry-run ok=<yes|no>            (dry-run only)
#
# Environment:
#   CLAUDE_BIN                    injected for tests (default: claude)
#   AUTO_AGENT_ROOT               the Harness install (default: the parent of lib/)
#   AUTO_AGENT_STATE_DIR          the State dir (Host env; see lib/host-env.sh)
#   AUTO_AGENT_TARGET_DIR         the Target Project checkout (Host env)
#   AUTO_AGENT_FIRE_MODEL         pins --model for the whole Fire (model policy)
#   AUTO_AGENT_GATE_VERDICT_FILE  the Gate verdict JSON to embed; without one the
#                                 record carries a `sensor: none` verdict

_fire_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTO_AGENT_ROOT="${AUTO_AGENT_ROOT:-$(cd "${_fire_lib_dir}/.." && pwd)}"
# shellcheck source=harness-config.sh
. "${_fire_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_fire_lib_dir}/host-env.sh"
# shellcheck source=rate-limits-tap.sh
. "${_fire_lib_dir}/rate-limits-tap.sh"
# shellcheck source=fire-record.sh
. "${_fire_lib_dir}/fire-record.sh"

FIRE_PLUGIN_NAME="auto-agent"
FIRE_PLUGIN_DIR="${AUTO_AGENT_ROOT}/plugin"
FIRE_SETTINGS_BASELINE="${FIRE_PLUGIN_DIR}/settings/baseline.json"
FIRE_FIXTURE_TARGET="${FIRE_PLUGIN_DIR}/fixtures/target-project"
FIRE_SKILL_DRY_RUN="${FIRE_PLUGIN_NAME}:dry-run"
FIRE_SKILL_PICKUP="${FIRE_PLUGIN_NAME}:afk-pickup"
FIRE_DRY_RUN_OK_LINE="dry-run: ok"

_fire_err() { echo "fire: $*" >&2; }
_fire_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# _fire_gate_verdict -> JSON: the file the Daemon handed over, else a
# `sensor: none` verdict in the ADR 0008 shape so the record's gate block is
# never missing.
_fire_gate_verdict() {
    local file="${AUTO_AGENT_GATE_VERDICT_FILE:-}"
    if [ -n "${file}" ] && [ -f "${file}" ] && jq -e 'type == "object"' "${file}" >/dev/null 2>&1; then
        jq -c . "${file}"
        return 0
    fi
    jq -n -c --arg mode "${CLAUDE_AUTH_MODE:-}" --arg now "$(_fire_now)" '{
        authMode: (if $mode == "" then null else $mode end),
        sensor: "none", state: "unavailable", remainPct: null, resetAt: null,
        shouldFire: true, observedAt: $now, limits: [],
        warnings: ["no Gate verdict was supplied to this Fire"]
    }'
}

# _fire_write_record <state> <id> <kind> <prompt> <dry-run> <target> <started> <exit> <phase> <stream> <stderr>
_fire_write_record() {
    local state="$1" id="$2" kind="$3" prompt="$4" dry="$5" target="$6" started="$7" rc="$8" phase="$9" stream="${10}" stderr="${11}"
    local summary skill="${FIRE_SKILL_PICKUP}"
    [ "${dry}" = "true" ] && skill="${FIRE_SKILL_DRY_RUN}"
    summary="$(fire_record_summarize_stream "${stream}" "${FIRE_PLUGIN_NAME}" "${skill}")"
    local record
    record="$(jq -n -c \
        --arg id "${id}" --arg kind "${kind}" --arg prompt "${prompt}" --argjson dry "${dry}" \
        --arg target "${target}" --arg started "${started}" --arg ended "$(_fire_now)" \
        --argjson rc "${rc}" --arg phase "${phase}" --arg model "${AUTO_AGENT_FIRE_MODEL:-}" \
        --arg stream "${stream}" --arg stderr "${stderr}" \
        --argjson summary "${summary}" --argjson gate "$(_fire_gate_verdict)" '
        {
          fireId: $id, kind: $kind, prompt: $prompt, startedAt: $started, endedAt: $ended,
          exit: $rc, phase: $phase, dryRun: $dry, target: $target,
          model: (if $model == "" then null else $model end),
          issue: $summary.issue,
          log: { stream: $stream, stderr: $stderr },
          plugin: $summary.plugin, result: $summary.result, rateLimit: $summary.rateLimit,
          gate: $gate
        }')"
    fire_record_write "${state}" "${id}" "${record}"
}

# fire_run [--dry-run] [<target-dir>]
fire_run() {
    local dry=false target=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --dry-run) dry=true ;;
            --) shift; break ;;
            -*) _fire_err "unknown option '$1'"; return 2 ;;
            *) target="$1" ;;
        esac
        shift
    done
    [ -n "${target}" ] || target="${1:-}"

    host_env_load
    if [ -z "${target}" ]; then
        target="${AUTO_AGENT_TARGET_DIR:-}"
        [ -n "${target}" ] || { [ "${dry}" = "true" ] && target="${FIRE_FIXTURE_TARGET}"; }
    fi
    if [ -z "${target}" ]; then
        _fire_err "no Target Project: pass <target-dir> or set AUTO_AGENT_TARGET_DIR in the Host env"
        return 2
    fi
    if [ ! -d "${target}" ]; then
        _fire_err "target dir not found: ${target}"
        return 2
    fi
    target="$(cd "${target}" && pwd)"

    local state; state="$(host_env_state_dir)"
    mkdir -p "${state}/logs" "${state}/${FIRE_RECORD_DIRNAME}" || {
        _fire_err "cannot create the State dir ${state}"
        return 2
    }

    local id started kind prompt
    id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
    started="$(_fire_now)"
    if [ "${dry}" = "true" ]; then kind="dry-run"; prompt="/${FIRE_SKILL_DRY_RUN}"
    else kind="pickup"; prompt="/${FIRE_SKILL_PICKUP}"; fi
    local stream="${state}/logs/${id}.stream.jsonl" stderr="${state}/logs/${id}.stderr.log"
    local record; record="$(fire_record_path "${state}" "${id}")"

    # Preflight: fail closed on the Harness config before anything else runs.
    if ! harness_config_check "${target}"; then
        _fire_write_record "${state}" "${id}" "${kind}" "${prompt}" "${dry}" "${target}" "${started}" 1 preflight "${stream}" "${stderr}"
        echo "fire: id=${id} kind=${kind} exit=1 record=${record}"
        _fire_err "preflight failed: the Harness config of ${target} is invalid"
        return 1
    fi
    if [ ! -f "${FIRE_SETTINGS_BASELINE}" ]; then
        _fire_err "settings baseline missing: ${FIRE_SETTINGS_BASELINE}"
        return 2
    fi

    local -a model_args=()
    [ -n "${AUTO_AGENT_FIRE_MODEL:-}" ] && model_args=(--model "${AUTO_AGENT_FIRE_MODEL}")

    local rc
    ( cd "${target}" && "${CLAUDE_BIN:-claude}" \
        --print \
        --permission-mode bypassPermissions \
        --plugin-dir "${FIRE_PLUGIN_DIR}" \
        --settings "${FIRE_SETTINGS_BASELINE}" \
        --output-format stream-json \
        --verbose \
        "${model_args[@]}" \
        "${prompt}" < /dev/null 2> "${stderr}" ) \
      | rate_limits_tap "${state}" "${id}" > "${stream}"
    rc=${PIPESTATUS[0]}

    _fire_write_record "${state}" "${id}" "${kind}" "${prompt}" "${dry}" "${target}" "${started}" "${rc}" claude "${stream}" "${stderr}"

    local loaded listed skill
    loaded="$(jq -r 'if .plugin.loaded then "yes" else "no" end' "${record}")"
    listed="$(jq -r 'if .plugin.skillListed then "yes" else "no" end' "${record}")"
    skill="$(jq -r '.plugin.skill' "${record}")"
    echo "fire: id=${id} kind=${kind} exit=${rc} record=${record}"
    echo "fire: plugin ${FIRE_PLUGIN_NAME} loaded=${loaded} skill=${skill} listed=${listed}"

    if [ "${dry}" = "true" ]; then
        local ok=no
        if [ "${rc}" -eq 0 ] && [ "${loaded}" = "yes" ] && [ "${listed}" = "yes" ] \
           && jq -e --arg l "${FIRE_DRY_RUN_OK_LINE}" '.result.text // "" | split("\n") | index($l) != null' "${record}" >/dev/null; then
            ok=yes
        fi
        echo "fire: dry-run ok=${ok}"
        [ "${ok}" = "yes" ] || { [ "${rc}" -ne 0 ] && return "${rc}"; return 1; }
    fi
    return "${rc}"
}
