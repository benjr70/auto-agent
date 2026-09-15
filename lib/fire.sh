#!/usr/bin/env bash
# Fire wrapper: one Daemon pass. Replaces Smart-Smoker-V2's `agent-run`
# invocation (ADR 0001): every Fire loads the pinned Harness install as a
# plugin with the `--settings` baseline, runs `claude -p` in stream-json
# through the rate-limit tap, and ends by writing a Fire record. Text mode is
# gone. Lane orchestration (branch hygiene, pause on exhaustion, lock cleanup)
# is not here yet; it arrives with the core-loop Slices, as does the
# `/auto-agent:afk-pickup` skill a plain Fire prompts: until then a plain Fire
# runs a prompt the plugin cannot answer and its record says so
# (`plugin.skillListed: false`).
#
# The Fire runs `--permission-mode bypassPermissions`, carried over from
# `agent-run`: a Daemon has no human to answer prompts. The `--settings`
# baseline's deny list still binds in that mode (verified live: a forced push
# is refused), and the Target Project's own `.claude/settings.json` merges on
# top of the baseline (verified live: its hooks run).
#
# Source this file, then:
#
#   fire_run [--dry-run] [<target-dir>]
#       Runs one Fire against the Target Project at <target-dir> (default:
#       AUTO_AGENT_TARGET_DIR from the Host env; for --dry-run, the fixture
#       Target Project in the plugin). Validates the Harness config and the
#       settings baseline first and fails closed; a preflight failure is still
#       a Fire and still gets a record (phase "preflight", exit 1 for a bad
#       config, 2 for a missing baseline). Returns the claude exit code, or 1
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

# _fire_write_record <exit> <phase>
# Reads the Fire context from the caller's scope (bash dynamic scoping):
# state id kind prompt skill dry target started stream stderr.
_fire_write_record() {
    local rc="$1" phase="$2" summary record
    summary="$(fire_record_summarize_stream "${stream}" "${FIRE_PLUGIN_NAME}" "${skill}")"
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

    # The Fire context every helper reads.
    local id started kind skill prompt
    id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
    started="$(_fire_now)"
    if [ "${dry}" = "true" ]; then kind="dry-run"; skill="${FIRE_SKILL_DRY_RUN}"
    else kind="pickup"; skill="${FIRE_SKILL_PICKUP}"; fi
    prompt="/${skill}"
    local stream="${state}/logs/${id}.stream.jsonl" stderr="${state}/logs/${id}.stderr.log"
    local record; record="$(fire_record_path "${state}" "${id}")"

    # Preflight: fail closed before anything else runs. A preflight failure is
    # still a Fire and gets a record.
    if ! harness_config_check "${target}"; then
        _fire_write_record 1 preflight
        echo "fire: id=${id} kind=${kind} exit=1 record=${record}"
        _fire_err "preflight failed: the Harness config of ${target} is invalid"
        return 1
    fi
    if [ ! -f "${FIRE_SETTINGS_BASELINE}" ]; then
        _fire_write_record 2 preflight
        echo "fire: id=${id} kind=${kind} exit=2 record=${record}"
        _fire_err "preflight failed: settings baseline missing: ${FIRE_SETTINGS_BASELINE}"
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

    _fire_write_record "${rc}" claude

    local loaded_listed
    loaded_listed="$(jq -r '(if .plugin.loaded then "yes" else "no" end) + " " + (if .plugin.skillListed then "yes" else "no" end)' "${record}")"
    echo "fire: id=${id} kind=${kind} exit=${rc} record=${record}"
    echo "fire: plugin ${FIRE_PLUGIN_NAME} loaded=${loaded_listed% *} skill=${skill} listed=${loaded_listed#* }"

    if [ "${dry}" = "true" ]; then
        if fire_record_dry_run_ok "${record}" "${FIRE_DRY_RUN_OK_LINE}"; then
            echo "fire: dry-run ok=yes"
        else
            echo "fire: dry-run ok=no"
            [ "${rc}" -ne 0 ] && return "${rc}"
            return 1
        fi
    fi
    return "${rc}"
}
