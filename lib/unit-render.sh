#!/usr/bin/env bash
# Unit render: the Daemon's and the Dashboard's systemd units, rendered from
# the templates in infra/systemd/ and the Host env of this Harness install
# (ADR 0004, ADR 0006). Setup's configure step installs the result; the
# templates are the only copy anyone edits.
#
# Usage:
#   unit-render.sh daemon|dashboard [--out <dir>]
#       Prints the rendered unit, or writes <dir>/auto-agent-<name>.service
#       and prints its path. Exit 0 rendered, 1 a value is missing or unsafe
#       for a unit file (whitespace in a path, a newline, & or a backslash, a
#       relative path, a placeholder left over), 2 usage.
#
# Placeholders and where their values come from (Host env unless noted):
#   @INSTALL@               this Harness install (AUTO_AGENT_ROOT, the parent of lib/)
#   @USER@                  AUTO_AGENT_HOST_USER, else the invoking user
#   @HOST_ENV@              the Host env file (AUTO_AGENT_HOST_ENV, else
#                           ~/.config/auto-agent/env); it must exist
#   @PATH@                  AUTO_AGENT_UNIT_PATH, else ~/.local/bin plus the
#                           system dirs
#   @MEMORY_MAX@            AUTO_AGENT_MEMORY_MAX, the Daemon's cgroup cap (8G:
#                           the Daemon peaks near 6.4 GB on the 12 GB default VM)
#   @DASHBOARD_MEMORY_MAX@  AUTO_AGENT_DASHBOARD_MEMORY_MAX (512M)
# A `%` in a value is escaped to `%%`, the unit-file literal.

_unit_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTO_AGENT_ROOT="${AUTO_AGENT_ROOT:-$(cd "${_unit_lib_dir}/.." && pwd)}"
# shellcheck source=host-env.sh
. "${_unit_lib_dir}/host-env.sh"

UNIT_TEMPLATE_DIR="${AUTO_AGENT_ROOT}/infra/systemd"
UNIT_NAMES="daemon dashboard"

_unit_err() { echo "unit-render: $*" >&2; }

# _unit_value <key> <value> <kind: path|pathlist|size|user>: validate, escape, print.
_unit_value() {
    local key="$1" value="$2" kind="$3"
    if [ -z "${value}" ]; then _unit_err "${key} is empty"; return 1; fi
    case "${value}" in
        *$'\n'*) _unit_err "${key} contains a newline"; return 1 ;;
        *'&'*|*'\'*) _unit_err "${key} contains & or a backslash: ${value}"; return 1 ;;
    esac
    case "${kind}" in
        path)
            case "${value}" in /*) ;; *) _unit_err "${key} must be absolute: ${value}"; return 1 ;; esac
            case "${value}" in *[[:space:]]*) _unit_err "${key} contains whitespace: ${value}"; return 1 ;; esac ;;
        pathlist)
            case "${value}" in *[[:space:]]*) _unit_err "${key} contains whitespace: ${value}"; return 1 ;; esac ;;
        size)
            if ! printf '%s' "${value}" | grep -Eq '^([0-9]+[KMGT]?|[0-9]+%|infinity)$'; then
                _unit_err "${key} is not a systemd size (e.g. 8G, 75%, infinity): ${value}"; return 1
            fi ;;
        user)
            if ! printf '%s' "${value}" | grep -Eq '^[A-Za-z_][A-Za-z0-9_.-]*$'; then
                _unit_err "${key} is not a user name: ${value}"; return 1
            fi ;;
    esac
    printf '%s' "${value//%/%%}"
}

# unit_render <name> -> the rendered unit on stdout
unit_render() {
    local name="$1" template="${UNIT_TEMPLATE_DIR}/auto-agent-${1}.service.in"
    [ -f "${template}" ] || { _unit_err "no template for '${name}' (${template})"; return 2; }
    host_env_load
    local host_env; host_env="$(host_env_file)"
    [ -f "${host_env}" ] || { _unit_err "Host env not found: ${host_env}"; return 1; }

    local install user hostenv path mem dmem
    install="$(_unit_value @INSTALL@ "${AUTO_AGENT_ROOT}" path)" || return 1
    user="$(_unit_value @USER@ "${AUTO_AGENT_HOST_USER:-$(id -un)}" user)" || return 1
    hostenv="$(_unit_value @HOST_ENV@ "${host_env}" path)" || return 1
    path="$(_unit_value @PATH@ "${AUTO_AGENT_UNIT_PATH:-${HOME}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin}" pathlist)" || return 1
    mem="$(_unit_value @MEMORY_MAX@ "${AUTO_AGENT_MEMORY_MAX:-8G}" size)" || return 1
    dmem="$(_unit_value @DASHBOARD_MEMORY_MAX@ "${AUTO_AGENT_DASHBOARD_MEMORY_MAX:-512M}" size)" || return 1

    local out
    out="$(INSTALL="${install}" USER_="${user}" HOSTENV="${hostenv}" UPATH="${path}" MEM="${mem}" DMEM="${dmem}" \
        awk '{
            gsub(/@INSTALL@/, ENVIRON["INSTALL"]); gsub(/@USER@/, ENVIRON["USER_"]);
            gsub(/@HOST_ENV@/, ENVIRON["HOSTENV"]); gsub(/@PATH@/, ENVIRON["UPATH"]);
            gsub(/@DASHBOARD_MEMORY_MAX@/, ENVIRON["DMEM"]); gsub(/@MEMORY_MAX@/, ENVIRON["MEM"]);
            print }' "${template}")" || return 1
    if printf '%s\n' "${out}" | grep -Eq '@[A-Z_]+@'; then
        _unit_err "placeholder left unrendered: $(printf '%s\n' "${out}" | grep -Eo '@[A-Z_]+@' | sort -u | tr '\n' ' ')"
        return 1
    fi
    printf '%s\n' "${out}"
}

_unit_main() {
    local name="${1:-}" out_dir=""
    case "${name}" in
        -h|--help|help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; return 0 ;;
        '') _unit_err "name a unit (one of: ${UNIT_NAMES})"; return 2 ;;
    esac
    case " ${UNIT_NAMES} " in *" ${name} "*) ;; *) _unit_err "unknown unit '${name}' (one of: ${UNIT_NAMES})"; return 2 ;; esac
    shift
    while [ $# -gt 0 ]; do
        case "$1" in
            --out) out_dir="${2:-}"; [ -n "${out_dir}" ] || { _unit_err "--out needs a directory"; return 2; }; shift ;;
            *) _unit_err "unknown option '$1'"; return 2 ;;
        esac
        shift
    done
    local rendered; rendered="$(unit_render "${name}")" || return $?
    if [ -z "${out_dir}" ]; then
        printf '%s\n' "${rendered}"
        return 0
    fi
    mkdir -p "${out_dir}" || return 1
    local file="${out_dir}/auto-agent-${name}.service"
    printf '%s\n' "${rendered}" > "${file}.tmp" && mv "${file}.tmp" "${file}" || return 1
    echo "${file}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    set -uo pipefail
    _unit_main "$@"
    exit $?
fi
