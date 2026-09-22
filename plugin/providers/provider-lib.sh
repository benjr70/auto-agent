#!/usr/bin/env bash
# provider-lib.sh: the sourceable helpers a bash Environment provider may use
# (ADR 0003, ticket #18). Nothing in the harness requires it: a provider is
# judged by `bin/auto-agent provider-check`, not by what it is written in. It
# exists so that the three things every provider re-derives — a port block from
# the PR number, a bounded health wait, a per-PR compose project name — are
# written once, correctly, instead of once per Target Project.
#
# Use it either way:
#   . "${AUTO_AGENT_ROOT:?}/plugin/providers/provider-lib.sh"   # from the Harness install
#   . "$(dirname "$0")/provider-lib.sh"                         # copied in beside the provider
#
# Every function prints its own diagnostics on stderr, because a provider's
# stdout is the KEY=value block and nothing else.
#
# API:
#   provider_need <cmd>...              every command is on PATH, else return 3
#   provider_pr_arg <args...>           echo the --pr value, else return 2
#   provider_port_block <pr> [stride] [base]
#                                       echo the first port of this PR's block
#   provider_compose_project <prefix> <pr>
#                                       echo the per-PR compose project name
#   provider_wait_healthy <url> [secs] [interval]
#                                       poll until the URL answers, else return 1
#   provider_key <KEY> <value>          print one contract line, else return 2
#
# Env:
#   PROVIDER_CURL_BIN  curl (default: curl), injectable for tests

_provider_err() { echo "provider-lib: $*" >&2; }

# provider_need <cmd>... : the prerequisite check every `up` opens with. Returns
# 3, the contract's "prerequisite missing" code, so a provider can write
# `provider_need docker curl || exit $?`.
provider_need() {
    local missing=() cmd
    for cmd in "$@"; do
        command -v "${cmd}" >/dev/null 2>&1 || missing+=("${cmd}")
    done
    [ "${#missing[@]}" -eq 0 ] && return 0
    _provider_err "prerequisite missing: ${missing[*]}"
    return 3
}

# provider_pr_arg <args...> : the PR number from a `--pr N` argument list. The
# contract passes it to `up` and `down` and to nothing else, so every provider
# parses the same two shapes (`--pr N` and `--pr=N`) the same way.
provider_pr_arg() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --pr) [ -n "${2:-}" ] || { _provider_err "--pr needs a number"; return 2; }
                  printf '%s\n' "$2"; return 0 ;;
            --pr=*) printf '%s\n' "${1#--pr=}"; return 0 ;;
            *) shift ;;
        esac
    done
    _provider_err "--pr is required"
    return 2
}

# provider_port_block <pr> [stride] [base] : the first port of this PR's block.
# Two Daemons on one Host, or two PRs mid-retry, must never collide, and the
# contract deliberately carries no port base (tenancy, ticket #11): the block
# is the provider's business. PR numbers wrap at 1000 so a long-lived repo
# stays inside the ephemeral range.
provider_port_block() {
    local pr="${1:?provider_port_block: pr required}" stride="${2:-10}" base="${3:-20000}"
    case "${pr}" in ''|*[!0-9]*) _provider_err "port block: '${pr}' is not a PR number"; return 2 ;; esac
    echo $(( base + (pr % 1000) * stride ))
}

# provider_compose_project <prefix> <pr> : the per-PR compose project name, the
# handle `down` needs to tear down exactly what `up` started. Lower-cased and
# stripped to [a-z0-9_-], the character set compose accepts.
provider_compose_project() {
    local prefix="${1:?provider_compose_project: prefix required}"
    local pr="${2:?provider_compose_project: pr required}"
    case "${pr}" in ''|*[!0-9]*) _provider_err "compose project: '${pr}' is not a PR number"; return 2 ;; esac
    prefix="$(printf '%s' "${prefix}" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9_-' '-')"
    printf '%s-pr-%s\n' "${prefix%-}" "${pr}"
}

# provider_wait_healthy <url> [secs] [interval] : poll until the URL answers,
# bounded. `up` returning only when the environment is healthy is the whole
# point of the contract (ADR 0003: readiness is the provider's business), and
# an unbounded wait turns a boot failure into a hung Fire.
provider_wait_healthy() {
    local url="${1:?provider_wait_healthy: url required}" secs="${2:-30}" interval="${3:-0.25}"
    local curl="${PROVIDER_CURL_BIN:-curl}" deadline
    deadline=$(( $(date +%s) + secs ))
    while :; do
        "${curl}" -fsS -o /dev/null "${url}" 2>/dev/null && return 0
        [ "$(date +%s)" -lt "${deadline}" ] || break
        sleep "${interval}"
    done
    _provider_err "health wait timed out after ${secs}s: ${url}"
    return 1
}

# provider_key <KEY> <value> : one line of the contract block on stdout. Keys
# are uppercase shell identifiers and values run to end of line, so a key the
# harness could not export — or a value carrying a newline, which would read as
# a second key — is refused here rather than failing the Provider check.
provider_key() {
    local key="${1:?provider_key: key required}" value="${2-}"
    case "${key}" in
        [A-Z]*) ;;
        *) _provider_err "key '${key}' must start with an uppercase letter"; return 2 ;;
    esac
    case "${key}" in
        *[!A-Z0-9_]*) _provider_err "key '${key}' is not an uppercase shell identifier"; return 2 ;;
    esac
    case "${value}" in
        *$'\n'*) _provider_err "the value of '${key}' carries a newline"; return 2 ;;
    esac
    printf '%s=%s\n' "${key}" "${value}"
}
