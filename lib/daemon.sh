#!/usr/bin/env bash
# Daemon: always on and budget-paced, it runs one Fire after another against
# one Target Project. Replaces Smart-Smoker-V2's `agent-daemon`: the
# loop, the Sleep Planner, the Work Probe wake and the fail cap are carried
# over; the sensor is now the usage sensor (lib/usage-sensor.sh, ADR 0008), a
# Fire is `bin/auto-agent fire` (lib/fire.sh), and the Daemon knows the Target
# Project only through the Host env and the Harness config the Fire loads.
#
# One cycle (a cycle may or may not fire):
#   1. Parked: when the Daemon is parked behind a dead credential
#      (lib/daemon-park.sh), it sleeps AUTO_AGENT_PARK_REPROBE_SECS and runs
#      `park reprobe`; no Fire until the probe passes.
#   2. Gate: `usage-sensor` prints the Gate verdict, written to
#      <state>/gate-verdict.json and handed to the Fire as
#      AUTO_AGENT_GATE_VERDICT_FILE (the record embeds it, the model policy's
#      `fireModel` switches the Fire's model). Sensor exit codes:
#        0  branch on .shouldFire
#        4  auth-dead: `park enter`, then the parked cycle above
#        5  mode mismatch: no Fire, re-gate after AUTO_AGENT_GATE_RETRY_SECS
#        6  api-key mode: the Daemon refuses to start and exits 6 (the unit
#           does not restart it)
#        other / no JSON: no Fire, the Sleep Planner's degraded sleep
#   3. Fire: `fire` runs one unit of work and writes one Fire record into the
#      State dir. Its stable lines steer the next step:
#        AGENT_RUN_AUTH_DEAD=1       the Fire parked the Daemon: next cycle
#        AGENT_RUN_NO_WORK=1         sleep to the verdict's reset in chunks,
#                                    waking early when the Work Probe sees work
#        AGENT_RUN_MODEL_LIMIT=<s>   a per-model limit: re-gate at once (the
#                                    next verdict switches the model), once;
#                                    a second in a row sleeps like EXHAUSTED
#        AGENT_RUN_RESET_AT=<iso>    budget ran out mid-Fire: sleep to it (to
#                                    the verdict's reset when empty)
#        non-zero exit, no marker    a failed Fire: probe-sleep, and after
#                                    AUTO_AGENT_DAEMON_FAIL_CAP failures in a
#                                    row sleep deaf to the reset
#        clean exit                  straight back to the gate
#   4. Sleep: the Sleep Planner (lib/sleep-planner.sh) turns a reset into a
#      sleep plus post-wake polls of the gate; an unknown reset is its
#      degraded default.
#
# Single flight: within a Target Project the `AFK:in-progress` label is the
# lock (lib/single-flight-lock.sh), taken and released by the Fire; the
# Daemon adds only a `flock` on <state>/daemon.lock so a second Daemon on the
# same State dir exits (7) instead of racing the first.
#
# The Daemon writes only into the State dir (gate-verdict.json, daemon.lock,
# daemon-state.json, logs/, plus what the Fire, the sensor and the park write
# there); the Target Project checkout is the Fire's to touch, never the
# Daemon's. Logs go to
# stdout (the journal under systemd).
#
# Run it (`bin/auto-agent daemon [<target-dir>]`, the unit's ExecStart) or
# source it and call daemon_main. <target-dir> defaults to
# AUTO_AGENT_TARGET_DIR from the Host env.
#
# Environment (Host env unless noted):
#   AUTO_AGENT_TARGET_DIR          the Target Project checkout
#   AUTO_AGENT_STATE_DIR           the State dir (lib/host-env.sh)
#   AUTO_AGENT_DAEMON_ID           exported to the sensor and the Fire (keys the
#                                  sensor's 403 mark); default host-pid-epoch
#   AUTO_AGENT_DAEMON_FIRE_ARGS    extra `fire` arguments (e.g. --dry-run)
#   AUTO_AGENT_DAEMON_FAIL_CAP     failed Fires in a row before a deaf sleep (3)
#   AUTO_AGENT_WORK_PROBE_INTERVAL secs between Work Probe scans (300)
#   AUTO_AGENT_PARK_REPROBE_SECS   secs between parked re-probes (3600)
#   AUTO_AGENT_GATE_RETRY_SECS     secs before re-gating on a mode mismatch (3600)
# The Sleep Planner's own tunables keep their carried-over names:
#   SLEEP_POLL_INTERVAL, SLEEP_POLL_MAX, SLEEP_DEGRADED_SECS (lib/sleep-planner.sh)
# Test seams (not Host env):
#   DAEMON_MAX_CYCLES              stop after N cycles (0, unbounded)
#   DAEMON_SENSOR_CMD, DAEMON_FIRE_CMD, DAEMON_PARK_CMD, WORK_PROBE_CMD
#                                  commands eval'd for the four collaborators
#                                  (default: the matching bin/auto-agent command)
#   SLEEP_CMD                      the sleep command (sleep)
#   DAEMON_NOW                     pins the Sleep Planner's "now"
#
# daemon-state.json is what the Daemon is doing now, rewritten at every step,
# so the Dashboard reads it instead of parsing the journal (ADR 0006):
#
#   { "state": "starting" | "firing" | "fire_complete" | "model_regate" |
#               "budget_low" | "budget_poll" | "queue_empty" | "woke_early" |
#               "exhausted" | "fire_failed" | "fail_cap" | "degraded" |
#               "mismatch" | "parked" | "stopped",
#     "detail": "<the log line>", "at": "<ISO>", "resetAt": "<ISO>" | null,
#     "fails": <int>, "failCap": <int>, "daemonId": "<id>" }
#
# Errors never stop the Daemon: it runs without errexit and every step is
# checked where it matters. The lock fd (9) is closed for every child, so a
# Fire's leftover process can never hold the lock past a Daemon restart.

_daemon_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTO_AGENT_ROOT="${AUTO_AGENT_ROOT:-$(cd "${_daemon_lib_dir}/.." && pwd)}"
# shellcheck source=host-env.sh
. "${_daemon_lib_dir}/host-env.sh"
# shellcheck source=sleep-planner.sh
. "${_daemon_lib_dir}/sleep-planner.sh"
# shellcheck source=work-probe.sh
. "${_daemon_lib_dir}/work-probe.sh"

DAEMON_VERDICT_FILE="gate-verdict.json"
DAEMON_LOCK_FILE="daemon.lock"
DAEMON_STATE_FILE="daemon-state.json"

_daemon_log() { echo "[daemon $(date -u +%Y-%m-%dT%H:%M:%SZ)] $*"; }

# _daemon_log_state <state> <reset-at> <message>: log the message and record
# it as the Daemon's current state. A failed write only costs the Dashboard a stale
# tile, never the cycle.
_daemon_log_state() {
    local file="${DAEMON_STATE}/${DAEMON_STATE_FILE}"
    _daemon_log "$3"
    jq -n -c --arg state "$1" --arg reset "$2" --arg detail "$3" --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        --argjson fails "${DAEMON_FAILS:-0}" --argjson cap "${AUTO_AGENT_DAEMON_FAIL_CAP:-3}" \
        --arg id "${AUTO_AGENT_DAEMON_ID:-}" '{
            state: $state, detail: $detail, at: $at,
            resetAt: (if $reset == "" then null else $reset end),
            fails: $fails, failCap: $cap, daemonId: $id }' > "${file}.tmp" 2>/dev/null \
        && mv "${file}.tmp" "${file}" || rm -f "${file}.tmp"
}

# _daemon_read_gate: one sensor read. Writes the verdict file and sets the
# globals GATE_RC SHOULD_FIRE RESET_AT REMAIN_PCT SENSOR GATE_STATE FIRE_MODEL.
_daemon_read_gate() {
    local out verdict="${DAEMON_STATE}/${DAEMON_VERDICT_FILE}"
    # stderr goes to the journal: a failed gate must leave its reason there.
    out="$(eval "${DAEMON_SENSOR_CMD}" 9>&-)" && GATE_RC=0 || GATE_RC=$?
    if printf '%s' "${out}" | jq -e 'type == "object" and (.shouldFire | type) == "boolean"' >/dev/null 2>&1; then
        printf '%s\n' "${out}" | jq -c . > "${verdict}.tmp" && mv "${verdict}.tmp" "${verdict}"
        # One jq call; the fields are never empty strings, so `read` splits cleanly.
        read -r SHOULD_FIRE RESET_AT REMAIN_PCT SENSOR GATE_STATE FIRE_MODEL < <(printf '%s' "${out}" \
            | jq -r '[.shouldFire, (.resetAt // "-"), (.remainPct // "-"), (.sensor // "-"), (.state // "-"), (.fireModel // "-")] | map(tostring) | join(" ")')
        [ "${RESET_AT}" != "-" ] || RESET_AT=""
        [ "${FIRE_MODEL}" != "-" ] || FIRE_MODEL=""
    else
        rm -f "${verdict}"
        SHOULD_FIRE=false RESET_AT="" REMAIN_PCT="-" SENSOR="-" GATE_STATE="-" FIRE_MODEL=""
        [ "${GATE_RC}" -ne 0 ] || GATE_RC=1
    fi
    [ "${GATE_RC}" -eq 0 ] || SHOULD_FIRE=false
}

# _daemon_poll <interval> <attempts>: after a wake the reset estimate may be
# early; poll the gate until it fires, stops being a plain verdict, or the cap.
_daemon_poll() {
    local interval="$1" attempts="$2" i
    for ((i = 0; i < attempts; i++)); do
        _daemon_read_gate
        if [ "${SHOULD_FIRE}" = "true" ] || [ "${GATE_RC}" -ne 0 ]; then
            return 0
        fi
        _daemon_log_state budget_poll "${RESET_AT}" "budget not yet replenished (poll $((i + 1))/${attempts}); waiting ${interval}s"
        eval "${SLEEP_CMD}" "${interval}"
    done
}

# _daemon_plan <reset-at> -> "<sleep> <interval> <attempts>"
# Never empty: an unreadable plan falls back to the planner's degraded
# defaults, so a broken plan cannot turn into a zero sleep and a hot loop.
_daemon_plan() {
    local plan sleep_secs interval attempts
    plan="$(sleep_planner "$1" "${DAEMON_NOW:-}" | jq -r '"\(.sleepSecs) \(.pollIntervalSecs) \(.pollMaxAttempts)"' 2>/dev/null)"
    read -r sleep_secs interval attempts <<< "${plan}"
    case "${sleep_secs}${interval}${attempts}" in
        ''|*[!0-9]*) echo "${SLEEP_DEGRADED_SECS} ${SLEEP_POLL_INTERVAL} ${SLEEP_POLL_MAX}" ;;
        *) echo "${sleep_secs} ${interval} ${attempts}" ;;
    esac
}

# _daemon_sleep_and_poll <reset-at>: sleep to the reset, then poll the gate.
_daemon_sleep_and_poll() {
    local sleep_secs interval attempts
    read -r sleep_secs interval attempts < <(_daemon_plan "$1")
    _daemon_log "sleeping ${sleep_secs}s until reset ${1:-unknown}"
    eval "${SLEEP_CMD}" "${sleep_secs}"
    _daemon_poll "${interval}" "${attempts}"
}

# _daemon_probe_sleep <reset-at>: the same sleep in Work Probe chunks, waking
# early when work appears (wp_decide's rules, against the baselines taken
# here). A probe is pure `gh`: zero Claude cost.
_daemon_probe_sleep() {
    local sleep_secs interval attempts elapsed=0 chunk reason scan baseline_scan baseline pr_baseline
    local every="${AUTO_AGENT_WORK_PROBE_INTERVAL:-300}"
    read -r sleep_secs interval attempts < <(_daemon_plan "$1")
    baseline_scan="$(eval "${WORK_PROBE_CMD}" 2>/dev/null 9>&- || true)"
    baseline="$(printf '%s' "${baseline_scan}" | jq -r '.pickSig // ""' 2>/dev/null || echo '')"
    pr_baseline="$(printf '%s' "${baseline_scan}" | jq -r '.prSig // ""' 2>/dev/null || echo '')"

    _daemon_log "sleeping ${sleep_secs}s until reset ${1:-unknown} (work probe every ${every}s)"
    while [ "${elapsed}" -lt "${sleep_secs}" ]; do
        chunk=$((sleep_secs - elapsed))
        [ "${chunk}" -gt "${every}" ] && chunk="${every}"
        eval "${SLEEP_CMD}" "${chunk}"
        elapsed=$((elapsed + chunk))
        scan="$(eval "${WORK_PROBE_CMD}" 2>/dev/null 9>&- || true)"
        [ -n "${scan}" ] || continue
        if reason="$(printf '%s' "${scan}" | wp_decide "${baseline}" "${pr_baseline}")"; then
            _daemon_log_state woke_early "" "work appeared mid-window (${reason}), waking early"
            return 0
        fi
    done
    _daemon_poll "${interval}" "${attempts}"
}

# _daemon_parked: true when parked.json says so.
_daemon_parked() {
    eval "${DAEMON_PARK_CMD}" status 2>/dev/null 9>&- | jq -e '.parked == true' >/dev/null 2>&1
}

# _daemon_fire: one Fire. Sets FIRE_RC and FIRE_OUT (the captured stdout).
_daemon_fire() {
    local out="${DAEMON_STATE}/logs/daemon-fire.out" record
    local -a extra=()
    # Word-split on purpose: the Host env carries the args as one string.
    # shellcheck disable=SC2206
    [ -n "${AUTO_AGENT_DAEMON_FIRE_ARGS:-}" ] && extra=(${AUTO_AGENT_DAEMON_FIRE_ARGS})
    eval "${DAEMON_FIRE_CMD}" '"${extra[@]}"' '"${DAEMON_TARGET}"' 2>&1 9>&- | tee "${out}"
    FIRE_RC="${PIPESTATUS[0]}"
    FIRE_OUT="${out}"
    record="$(sed -n 's/^fire: id=.* record=\(.*\)$/\1/p' "${out}" | tail -1)"
    if [ -n "${record}" ] && [ -f "${record}" ]; then
        _daemon_log "fire exit=${FIRE_RC} record=${record}"
    else
        _daemon_log "WARN: fire exit=${FIRE_RC} named no Fire record"
    fi
}

# _daemon_marker <name> -> the marker's value (empty when absent); rc 1 when absent
_daemon_marker() {
    grep -q "^$1=" "${FIRE_OUT}" || return 1
    grep -m1 "^$1=" "${FIRE_OUT}" | cut -d= -f2-
}

# _daemon_fire_and_follow: fire, then sleep (or not) by the Fire's markers.
# Reads and updates DAEMON_FAILS and DAEMON_MODEL_REGATE.
_daemon_fire_and_follow() {
    local fail_cap="${AUTO_AGENT_DAEMON_FAIL_CAP:-3}" run_reset limit regated=0
    _daemon_log_state firing "" "budget above min, firing"
    _daemon_fire
    if [ "${FIRE_RC}" -eq 0 ] || grep -q '^AGENT_RUN_' "${FIRE_OUT}"; then
        DAEMON_FAILS=0
    fi
    if _daemon_marker AGENT_RUN_AUTH_DEAD >/dev/null; then
        _daemon_log_state parked "" "Fire found the credential dead: parked"
    elif _daemon_marker AGENT_RUN_NO_WORK >/dev/null; then
        _daemon_log_state queue_empty "${RESET_AT}" "queue empty, sleeping until the reset (with work probe)"
        _daemon_probe_sleep "${RESET_AT}"
    elif limit="$(_daemon_marker AGENT_RUN_MODEL_LIMIT)" && [ "${DAEMON_MODEL_REGATE}" -eq 0 ]; then
        # The next verdict switches the model; a second limit in a row means
        # the switch did not help, so that one sleeps like any exhaustion.
        regated=1
        _daemon_log_state model_regate "" "per-model limit ${limit}: re-gating so the model policy can switch"
    elif run_reset="$(_daemon_marker AGENT_RUN_RESET_AT)"; then
        [ -n "${run_reset}" ] || run_reset="${RESET_AT}"
        _daemon_log_state exhausted "${run_reset}" "Fire exhausted the budget, sleeping until ${run_reset:-unknown}"
        _daemon_sleep_and_poll "${run_reset}"
    elif [ "${FIRE_RC}" -ne 0 ]; then
        # A genuine failure. Re-firing at once hot-loops on a broken pick; a
        # deaf sleep makes a transient failure cost the window. Probe-sleep,
        # and go deaf only after the cap.
        DAEMON_FAILS=$((DAEMON_FAILS + 1))
        if [ "${DAEMON_FAILS}" -lt "${fail_cap}" ]; then
            _daemon_log_state fire_failed "${RESET_AT}" "WARN: Fire failed (exit ${FIRE_RC}), probe-sleeping (fail ${DAEMON_FAILS}/${fail_cap})"
            _daemon_probe_sleep "${RESET_AT}"
        else
            _daemon_log_state fail_cap "${RESET_AT}" "WARN: Fire failed (exit ${FIRE_RC}), fail cap reached (${DAEMON_FAILS}/${fail_cap}), sleeping until window reset"
            _daemon_sleep_and_poll "${RESET_AT}"
            DAEMON_FAILS=0
        fi
    else
        _daemon_log_state fire_complete "" "Fire complete, re-checking the gate"
    fi
    DAEMON_MODEL_REGATE="${regated}"
}

# _daemon_cycle: one cycle. Returns 6 when the Daemon must stop.
_daemon_cycle() {
    local reprobe="${AUTO_AGENT_PARK_REPROBE_SECS:-3600}" retry="${AUTO_AGENT_GATE_RETRY_SECS:-3600}"
    if _daemon_parked; then
        _daemon_log_state parked "" "parked behind a dead credential: re-probing in ${reprobe}s"
        eval "${SLEEP_CMD}" "${reprobe}"
        if eval "${DAEMON_PARK_CMD}" reprobe 9>&-; then
            _daemon_log "re-probe passed, un-parked"
        fi
        return 0
    fi
    _daemon_read_gate
    _daemon_log "gate rc=${GATE_RC} sensor=${SENSOR} state=${GATE_STATE} remainPct=${REMAIN_PCT} shouldFire=${SHOULD_FIRE} resetAt=${RESET_AT:-unknown} fireModel=${FIRE_MODEL:-default}"
    case "${GATE_RC}" in
        0)
            if [ "${SHOULD_FIRE}" = "true" ]; then
                _daemon_fire_and_follow
            else
                _daemon_log_state budget_low "${RESET_AT}" "budget below min, not firing"
                _daemon_sleep_and_poll "${RESET_AT}"
            fi ;;
        4)
            _daemon_log_state parked "" "credential dead: parking"
            eval "${DAEMON_PARK_CMD}" enter --reason '"usage sensor: auth-dead"' 9>&- || true ;;
        5)
            _daemon_log_state mismatch "" "WARN: CLAUDE_AUTH_MODE contradicts the credential; no Fire, re-gating in ${retry}s"
            eval "${SLEEP_CMD}" "${retry}" ;;
        6)
            _daemon_log_state stopped "" "api-key auth mode has no spend pacing yet: refusing to start"
            return 6 ;;
        *)
            _daemon_log_state degraded "" "WARN: usage sensor failed (rc ${GATE_RC}); no Fire"
            _daemon_sleep_and_poll "" ;;
    esac
}

# daemon_main [<target-dir>]
daemon_main() {
    set -uo pipefail
    host_env_load
    DAEMON_TARGET="${1:-${AUTO_AGENT_TARGET_DIR:-}}"
    if [ -z "${DAEMON_TARGET}" ] || [ ! -d "${DAEMON_TARGET}" ]; then
        _daemon_log "no Target Project: pass <target-dir> or set AUTO_AGENT_TARGET_DIR in the Host env (got '${DAEMON_TARGET}')" >&2
        return 2
    fi
    DAEMON_TARGET="$(cd "${DAEMON_TARGET}" && pwd)"
    DAEMON_STATE="$(host_env_state_dir)"
    mkdir -p "${DAEMON_STATE}/logs"
    export AUTO_AGENT_STATE_DIR="${DAEMON_STATE}" AUTO_AGENT_TARGET_DIR="${DAEMON_TARGET}" AUTO_AGENT_ROOT
    export AUTO_AGENT_GATE_VERDICT_FILE="${DAEMON_STATE}/${DAEMON_VERDICT_FILE}"
    export AUTO_AGENT_DAEMON_ID="${AUTO_AGENT_DAEMON_ID:-$(hostname -s 2>/dev/null || echo host)-$$-$(date -u +%s)}"

    local cli="\"${AUTO_AGENT_ROOT}/bin/auto-agent\""
    DAEMON_SENSOR_CMD="${DAEMON_SENSOR_CMD:-${cli} usage-sensor}"
    DAEMON_FIRE_CMD="${DAEMON_FIRE_CMD:-${cli} fire}"
    DAEMON_PARK_CMD="${DAEMON_PARK_CMD:-${cli} park}"
    WORK_PROBE_CMD="${WORK_PROBE_CMD:-wp_scan \"${DAEMON_TARGET}\"}"
    SLEEP_CMD="${SLEEP_CMD:-sleep}"
    DAEMON_FAILS=0 DAEMON_MODEL_REGATE=0
    local max_cycles="${DAEMON_MAX_CYCLES:-0}" cycles=0 rc

    exec 9>"${DAEMON_STATE}/${DAEMON_LOCK_FILE}"
    if ! flock -n 9; then
        _daemon_log "another Daemon holds ${DAEMON_STATE}/${DAEMON_LOCK_FILE}, exiting" >&2
        return 7
    fi

    _daemon_log_state starting "" "starting id=${AUTO_AGENT_DAEMON_ID} target=${DAEMON_TARGET} state=${DAEMON_STATE} max_cycles=${max_cycles}"
    while true; do
        _daemon_cycle && rc=0 || rc=$?
        [ "${rc}" -eq 0 ] || return "${rc}"
        cycles=$((cycles + 1))
        if [ "${max_cycles}" -ne 0 ] && [ "${cycles}" -ge "${max_cycles}" ]; then
            _daemon_log "reached max cycles (${max_cycles}), exiting"
            return 0
        fi
    done
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    daemon_main "$@"
    exit $?
fi
