#!/usr/bin/env bash
# shellcheck disable=SC2034
# Host env reader: the per-Host declaration of account and machine facts the
# Daemon reads alongside the Harness config (ADR 0002, ADR 0005). Under systemd
# the unit's EnvironmentFile already exports it; this lib makes a by-hand run
# (`bin/auto-agent fire`) see the same keys.
#
# Source this file, then:
#
#   host_env_load
#       Exports every key in the Host env file when it exists; a no-op when it
#       does not. Never fails; a line it cannot read is named on stderr and
#       skipped. Accepts the systemd EnvironmentFile shape: KEY=value, one
#       layer of quotes, `export` prefix, comments, CRLF. Keys already in the
#       environment win, so a unit drop-in or a test can override the file.
#
#   host_env_state_dir
#       Prints the State dir: AUTO_AGENT_STATE_DIR from the Host env, else
#       $XDG_STATE_HOME/auto-agent, else ~/.local/state/auto-agent. Always
#       outside any checkout.
#
# Environment:
#   AUTO_AGENT_HOST_ENV   path of the Host env file (default ~/.config/auto-agent/env)
#   AUTO_AGENT_STATE_DIR  the State dir, when already declared

HOST_ENV_DEFAULT_FILE="${HOME}/.config/auto-agent/env"

host_env_file() {
    printf '%s\n' "${AUTO_AGENT_HOST_ENV:-${HOST_ENV_DEFAULT_FILE}}"
}

host_env_load() {
    local file; file="$(host_env_file)"
    [ -f "${file}" ] || return 0
    # Keys already exported win: read the file into a subshell-free scratch
    # environment and only fill in what is missing.
    local line key value
    while IFS= read -r line || [ -n "${line}" ]; do
        line="${line%$'\r'}"                       # CRLF
        line="${line#"${line%%[![:space:]]*}"}"    # leading whitespace
        line="${line#export }"                     # `export KEY=value`
        case "${line}" in
            ''|'#'*) continue ;;
            *=*) ;;
            *) echo "host-env: ignoring line without '=': ${line}" >&2; continue ;;
        esac
        key="${line%%=*}"
        value="${line#*=}"
        case "${key}" in
            [A-Za-z_]*) case "${key}" in *[!A-Za-z0-9_]*) key="" ;; esac ;;
            *) key="" ;;
        esac
        if [ -z "${key}" ]; then
            echo "host-env: ignoring line with an invalid key: ${line%%=*}" >&2
            continue
        fi
        # Strip one layer of matching quotes, as systemd's EnvironmentFile does.
        case "${value}" in
            \"*\") value="${value#\"}"; value="${value%\"}" ;;
            \'*\') value="${value#\'}"; value="${value%\'}" ;;
        esac
        if [ -z "${!key+x}" ]; then
            export "${key}=${value}"
        fi
    done < "${file}"
    return 0
}

host_env_state_dir() {
    printf '%s\n' "${AUTO_AGENT_STATE_DIR:-${XDG_STATE_HOME:-${HOME}/.local/state}/auto-agent}"
}
