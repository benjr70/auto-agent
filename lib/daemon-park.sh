#!/usr/bin/env bash
# Daemon park: what the Daemon does when its Claude credential is dead
# (ADR 0008). Credential death is never exhaustion: no Fires, one reused
# `AFK:needs-human` issue open in the Target Project, an hourly re-probe of
# `claude auth status`, and un-parking (closing the issue) when it passes, so
# re-running `/login` over SSH is the whole fix.
#
# Source this file, then:
#
#   park_probe
#       `claude auth status`: 0 when logged in, 1 otherwise. Zero cost.
#
#   park_enter <reason>
#       Parks: writes <state-dir>/parked.json and opens the needs-human issue,
#       or reuses the open one (matched by its body marker) with a comment.
#       Idempotent. Prints `park: parked issue=#N reason=<reason>`.
#       Returns 0; 1 when the issue could not be opened (the Daemon still
#       parks: parked.json is written first, the issue is retried on the next
#       re-probe).
#
#   park_leave
#       Un-parks: closes the issue with a comment, removes parked.json.
#       Prints `park: un-parked issue=#N`.
#
#   park_reprobe
#       The hourly re-probe: not parked -> `park: not parked`, 0; parked and
#       the probe passes -> park_leave, 0; parked and it fails -> counts the
#       probe, `park: still parked issue=#N probes=K`, 1.
#
#   park_status
#       Prints parked.json, or {"parked":false}.
#
# parked.json:
#   { "parked": true, "parkedAt": "<ISO>", "reason": "<text>",
#     "issue": <N> | null, "probes": <int>, "lastProbeAt": "<ISO>" | null }
#
# Environment:
#   the Harness config through harness_config_resolve (HARNESS_CONFIG_JSON or
#   AUTO_AGENT_TARGET_DIR): the repo slug the issue lives in
#   CLAUDE_BIN, GH_BIN   injected for tests
#   AUTO_AGENT_STATE_DIR the State dir (Host env; see lib/host-env.sh)
#   PARK_NOW             epoch "now" for tests

_park_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_park_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_park_lib_dir}/host-env.sh"

PARK_FILE="parked.json"
PARK_MARKER="<!-- auto-agent:parked -->"

_park_now() { date -u -d "@${PARK_NOW:-$(date -u +%s)}" +%Y-%m-%dT%H:%M:%SZ; }
_park_err() { echo "park: $*" >&2; }
_park_path() { printf '%s/%s' "$(host_env_state_dir)" "${PARK_FILE}"; }

_park_write() {
    local path; path="$(_park_path)"
    mkdir -p "${path%/*}" || return 1
    printf '%s\n' "$1" | jq . > "${path}.tmp" && mv "${path}.tmp" "${path}"
}

park_status() {
    local path; path="$(_park_path)"
    if [ -f "${path}" ] && jq -e '.parked == true' "${path}" >/dev/null 2>&1; then jq -c . "${path}"
    else echo '{"parked":false}'; fi
}

park_probe() {
    local out
    out="$("${CLAUDE_BIN:-claude}" auth status --json 2>/dev/null)" || return 1
    printf '%s' "${out}" | jq -e '.loggedIn == true' >/dev/null 2>&1
}

# _park_slug -> the repo slug, or 1 with a stderr line
_park_slug() {
    local cfg slug
    cfg="$(harness_config_resolve 2>/dev/null)" || { _park_err "no Harness config (set HARNESS_CONFIG_JSON or AUTO_AGENT_TARGET_DIR)"; return 1; }
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug // empty')"
    [ -n "${slug}" ] || { _park_err "the Harness config names no repo slug"; return 1; }
    printf '%s' "${slug}"
}

# _park_find_issue <slug> -> the open needs-human issue carrying the marker, or ""
_park_find_issue() {
    "${GH_BIN:-gh}" issue list --repo "$1" --label "${HARNESS_LABEL_NEEDS_HUMAN}" --state open \
        --json number,body --jq "[.[] | select(.body | contains(\"${PARK_MARKER}\"))] | sort_by(.number) | .[0].number // empty" 2>/dev/null || true
}

# _park_ensure_issue <slug> <reason> <ts> -> the issue number; opens or reuses
_park_ensure_issue() {
    local slug="$1" reason="$2" ts="$3" gh="${GH_BIN:-gh}" n host
    host="$(hostname 2>/dev/null || echo host)"
    n="$(_park_find_issue "${slug}")"
    if [ -n "${n}" ]; then
        "${gh}" issue comment "${n}" --repo "${slug}" --body "Parked again at ${ts} on ${host}: ${reason}. The Daemon re-probes \`claude auth status\` hourly and closes this issue when a login is back." >/dev/null 2>&1 || true
        printf '%s' "${n}"
        return 0
    fi
    n="$("${gh}" issue create --repo "${slug}" --label "${HARNESS_LABEL_NEEDS_HUMAN}" \
        --title "Daemon parked on ${host}: Claude credential dead" \
        --body "${PARK_MARKER}
The Daemon on \`${host}\` parked at ${ts}: ${reason}.

A dead credential is never exhaustion: no Fire runs until a human logs in again on the Host (re-run \`/login\` over SSH, or replace \`CLAUDE_CODE_OAUTH_TOKEN\` in the Host env and restart the Daemon). The Daemon re-probes \`claude auth status\` hourly and closes this issue itself when the probe passes." 2>/dev/null | grep -oE '[0-9]+$' | tail -1)"
    [ -n "${n}" ] || return 1
    printf '%s' "${n}"
}

park_enter() {
    local reason="${1:-credential dead}" ts slug n="" rc=0 existing
    ts="$(_park_now)"
    existing="$(park_status)"
    if [ "$(printf '%s' "${existing}" | jq -r .parked)" = "true" ]; then
        # Already parked: one park is one event; keep its instant and reason.
        n="$(printf '%s' "${existing}" | jq -r '.issue // empty')"
        ts="$(printf '%s' "${existing}" | jq -r .parkedAt)"
        reason="$(printf '%s' "${existing}" | jq -r .reason)"
    fi
    if [ -z "${n}" ]; then
        if slug="$(_park_slug)"; then
            n="$(_park_ensure_issue "${slug}" "${reason}" "${ts}")" || { _park_err "could not open the needs-human issue; retried on the next re-probe"; rc=1; }
        else
            rc=1
        fi
    fi
    _park_write "$(jq -n -c --arg at "${ts}" --arg reason "${reason}" --arg n "${n}" \
        --argjson probes "$(printf '%s' "${existing}" | jq '.probes // 0')" \
        --argjson last "$(printf '%s' "${existing}" | jq '.lastProbeAt // null')" '
        { parked: true, parkedAt: $at, reason: $reason,
          issue: (if $n == "" then null else ($n | tonumber) end),
          probes: $probes, lastProbeAt: $last }')" || return 1
    echo "park: parked issue=${n:+#}${n:-none} reason=${reason}"
    return "${rc}"
}

park_leave() {
    local existing n slug ts
    existing="$(park_status)"
    [ "$(printf '%s' "${existing}" | jq -r .parked)" = "true" ] || { echo "park: not parked"; return 0; }
    n="$(printf '%s' "${existing}" | jq -r '.issue // empty')"
    ts="$(_park_now)"
    if [ -n "${n}" ] && slug="$(_park_slug)"; then
        "${GH_BIN:-gh}" issue close "${n}" --repo "${slug}" \
            --comment "Un-parked at ${ts}: \`claude auth status\` passes again. Fires resume." >/dev/null 2>&1 \
            || _park_err "could not close issue #${n}; closing it by hand is fine"
    fi
    rm -f "$(_park_path)"
    echo "park: un-parked issue=${n:+#}${n:-none}"
}

park_reprobe() {
    local existing n probes
    existing="$(park_status)"
    [ "$(printf '%s' "${existing}" | jq -r .parked)" = "true" ] || { echo "park: not parked"; return 0; }
    if park_probe; then
        park_leave
        return 0
    fi
    n="$(printf '%s' "${existing}" | jq -r '.issue // empty')"
    if [ -z "${n}" ]; then
        # The issue never opened (gh was down when we parked): retry now.
        park_enter "$(printf '%s' "${existing}" | jq -r .reason)" >/dev/null || true
        existing="$(park_status)"
        n="$(printf '%s' "${existing}" | jq -r '.issue // empty')"
    fi
    probes="$(printf '%s' "${existing}" | jq '(.probes // 0) + 1')"
    _park_write "$(printf '%s' "${existing}" | jq -c --argjson p "${probes}" --arg at "$(_park_now)" '.probes = $p | .lastProbeAt = $at')"
    echo "park: still parked issue=${n:+#}${n:-none} probes=${probes}"
    return 1
}

_park_main() {
    local cmd="${1:-status}"; shift || true
    host_env_load
    case "${cmd}" in
        status) park_status ;;
        probe)  if park_probe; then echo "park: probe ok"; else echo "park: probe failed"; return 1; fi ;;
        enter)  local reason=""
                while [ $# -gt 0 ]; do
                    case "$1" in
                        --reason) reason="${2:-}"; shift 2 || { echo "park: --reason requires a value" >&2; return 2; } ;;
                        *) echo "park: unknown arg $1" >&2; return 2 ;;
                    esac
                done
                park_enter "${reason:-credential dead}" ;;
        leave)  park_leave ;;
        reprobe) park_reprobe ;;
        -h|--help|help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' ;;
        *) echo "usage: daemon-park.sh status|probe|enter [--reason <text>]|leave|reprobe" >&2; return 2 ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    set -uo pipefail
    _park_main "$@"
    exit $?
fi
