#!/usr/bin/env bash
# display-env.sh: display truth for a verification round, and the Electron
# sandbox decision that goes with it (ticket #19).
#
# Why this exists: a round that cannot find the Host's display used to conclude
# it was running headless and defer its most valuable items — while the Host
# had a display the whole time. So there is exactly one place that answers "is
# there a display, and which one", every launcher sources it, and it NEVER
# falls back to headless: a resolved display or an infra finding, nothing in
# between.
#
# On the reference Host the display is Xvfb on a fixed `DISPLAY` written into
# the Host env (ticket #19 finding 3): no logged-in desktop session, no
# rotating X-authority file to glob for. An unset `DISPLAY` in the round's own
# shell is never evidence of a headless Host — the Daemon's shell simply does
# not inherit it; only this lib failing is a display problem.
#
# The Electron sandbox is the second half of the same finding. On a Host that
# restricts unprivileged user namespaces, Electron cannot start its own sandbox
# unless an AppArmor profile grants `userns` to the app binary — the mechanism
# the OS already uses for the system browser. Setup writes that profile. When
# it is not there, the round still runs, with `ELECTRON_DISABLE_SANDBOX=1` and
# a DEGRADED report: never silently, and never as a reason to skip the round.
#
# Source this file, then:
#
#   display_env_resolve
#       Exports DISPLAY from the Host env and probes it. 0 with the display
#       exported, 3 when there is none (an infra finding, not an item verdict).
#
#   display_sandbox_mode <app-binary>
#       Prints the Electron sandbox mode for this Host: `sandbox` (the Host
#       does not restrict unprivileged user namespaces), `apparmor` (a loaded
#       profile grants them to this binary), or `disabled` — which also exports
#       ELECTRON_DISABLE_SANDBOX=1 and is what the caller reports as DEGRADED.
#       Always exits 0: the mode is the answer.
#
#   display_sandbox_degraded <mode>
#       0 when the mode is the degraded one, so callers phrase the report once.
#
# Env:
#   DISPLAY                            the Host's display (from the Host env)
#   AUTO_AGENT_DISPLAY                 an override read when DISPLAY is unset
#   AUTO_AGENT_ELECTRON_SANDBOX        `disabled` to force the degraded path
#   DISPLAY_PROBE_CMD                  readiness probe for the display
#                                      (default `xdpyinfo -display <display>`)
#   DISPLAY_USERNS_RESTRICT_FILE       the kernel switch that decides whether a
#                                      profile is needed at all
#   DISPLAY_APPARMOR_PROFILES_FILE     the loaded-profile list to search
#   DISPLAY_ENV_LOG_PREFIX             log prefix (default display-env)

DISPLAY_USERNS_RESTRICT_FILE="${DISPLAY_USERNS_RESTRICT_FILE:-/proc/sys/kernel/apparmor_restrict_unprivileged_userns}"
DISPLAY_APPARMOR_PROFILES_FILE="${DISPLAY_APPARMOR_PROFILES_FILE:-/sys/kernel/security/apparmor/profiles}"

_de_log() { echo "[${DISPLAY_ENV_LOG_PREFIX:-display-env}] $*" >&2; }

# display_env_resolve : export the Host's DISPLAY, or return 3.
display_env_resolve() {
    local display="${DISPLAY:-${AUTO_AGENT_DISPLAY:-}}"
    if [ -z "${display}" ]; then
        _de_log "ERROR: no display — neither DISPLAY nor AUTO_AGENT_DISPLAY is set in the Host env."
        _de_log "       A tour is captured on a real display (the reference Host runs Xvfb on a"
        _de_log "       fixed DISPLAY); refusing to fall back to headless. This is an infra"
        _de_log "       finding for the round to report, never an item verdict."
        return 3
    fi

    local probe="${DISPLAY_PROBE_CMD:-}"
    if [ -z "${probe}" ]; then
        if command -v xdpyinfo >/dev/null 2>&1; then
            probe="xdpyinfo -display ${display}"
        else
            export DISPLAY="${display}"
            _de_log "resolved DISPLAY=${display} (no xdpyinfo on this Host: the declared display is taken as given)"
            return 0
        fi
    fi

    if ! bash -c "${probe}" >/dev/null 2>&1; then
        _de_log "ERROR: DISPLAY=${display} is declared but does not answer (${probe})."
        _de_log "       The Host's display server is down; refusing to fall back to headless."
        return 3
    fi

    export DISPLAY="${display}"
    _de_log "resolved DISPLAY=${display}"
    return 0
}

# _de_userns_restricted : 0 when the Host restricts unprivileged user namespaces
_de_userns_restricted() {
    [ -r "${DISPLAY_USERNS_RESTRICT_FILE}" ] || return 1
    local v; v="$(cat "${DISPLAY_USERNS_RESTRICT_FILE}" 2>/dev/null)"
    [ "${v}" = "1" ]
}

# _de_apparmor_grants <bin> : 0 when a loaded profile names this binary
_de_apparmor_grants() {
    local bin="$1"
    [ -r "${DISPLAY_APPARMOR_PROFILES_FILE}" ] || return 1
    grep -Fq -- "${bin}" "${DISPLAY_APPARMOR_PROFILES_FILE}" 2>/dev/null
}

# display_sandbox_mode <app-binary> : sandbox | apparmor | disabled
display_sandbox_mode() {
    local bin="${1:-}"

    if [ "${AUTO_AGENT_ELECTRON_SANDBOX:-}" = "disabled" ]; then
        export ELECTRON_DISABLE_SANDBOX=1
        _de_log "sandbox disabled by the Host env (AUTO_AGENT_ELECTRON_SANDBOX=disabled)"
        printf 'disabled\n'
        return 0
    fi

    if ! _de_userns_restricted; then
        printf 'sandbox\n'
        return 0
    fi

    if [ -n "${bin}" ] && _de_apparmor_grants "${bin}"; then
        _de_log "AppArmor profile grants user namespaces to ${bin}"
        printf 'apparmor\n'
        return 0
    fi

    export ELECTRON_DISABLE_SANDBOX=1
    _de_log "no AppArmor profile grants user namespaces to ${bin:-the app binary} on a Host that"
    _de_log "restricts them: starting with ELECTRON_DISABLE_SANDBOX=1. The round is DEGRADED,"
    _de_log "not skipped — Setup writes the profile that removes this."
    printf 'disabled\n'
    return 0
}

# display_sandbox_degraded <mode> : 0 when this mode is the degraded one
display_sandbox_degraded() {
    [ "${1:-}" = "disabled" ]
}
