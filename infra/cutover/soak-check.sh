#!/usr/bin/env bash
# soak-check.sh: has a cut-over Host soaked? The read-only answer to "may the
# deletion PR merge", which is what ends the rollback window (ticket #17).
#
# Why this exists: the soak is two Fires that happen on the Daemon's schedule,
# hours apart, with a human approval in the middle. Nobody should decide the
# rollback window is over from memory. This reads the Host as it is (the Host
# env, the units, the Fire records, the Dashboard, GitHub) and says which of
# the cut-over's acceptance criteria hold yet.
#
# Usage:
#   infra/cutover/soak-check.sh [--old-units "<unit> <unit>"]
#
#   --old-units   the old daemon's units, which must be neither enabled nor
#                 active (default: "agent-daemon.service agent-dashboard.service")
#
# Items, each one line `soak: <item>: ok|waiting|FAIL — <detail>`:
#   identity    the Host env's machine user is who its token logs in as, and is
#               not the repository's owner (ADR 0005)
#   units       both harness units are active; no old unit is enabled or active
#   afk-ticket  a pick Fire ended OK from this install, and the issue it
#               picked has a merged Agent PR the machine user opened
#   resolve     a resolve Fire ended OK and settled its ticket
#   dashboard   /api/status answers and lists this State dir's Fire history
# and a last line: `soak: PASS — ...`, `soak: NOT YET — <items>` (only
# `waiting` items: the Daemon has not got there) or `soak: FAIL — <items>`.
#
# `waiting` is not a failure: it means the Fire or the merge has not happened.
#
# Exit codes: 0 PASS, 1 FAIL, 3 NOT YET, 2 usage or no Host env.
#
# Env:
#   AUTO_AGENT_HOST_ENV  the Host env (default ~/.config/auto-agent/env)
#   GH_BIN, SYSTEMCTL_BIN, CURL_BIN   injected for tests

set -uo pipefail

_sc_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOAK_ROOT="$(cd "${_sc_dir}/../.." && pwd)"
# shellcheck source=../../lib/host-env.sh
. "${SOAK_ROOT}/lib/host-env.sh"
# shellcheck source=../../lib/harness-config.sh
. "${SOAK_ROOT}/lib/harness-config.sh"

GH="${GH_BIN:-gh}"
SYSTEMCTL="${SYSTEMCTL_BIN:-systemctl}"
CURL="${CURL_BIN:-curl}"

FAILED=(); WAITING=()
line() {
    printf 'soak: %s: %s — %s\n' "$1" "$2" "$3"
    case "$2" in FAIL) FAILED+=("$1") ;; waiting) WAITING+=("$1") ;; esac
    return 0
}

old_units="agent-daemon.service agent-dashboard.service"
while [ $# -gt 0 ]; do
    case "$1" in
        --old-units)
            [ -n "${2:-}" ] || { echo "soak-check: --old-units needs at least one unit" >&2; exit 2; }
            old_units="$2"; shift ;;
        -h|--help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "soak-check: unknown argument '$1'" >&2; exit 2 ;;
    esac
    shift
done

if [ ! -f "$(host_env_file)" ]; then
    echo "soak-check: no Host env at $(host_env_file): Setup has not run on this Host" >&2
    exit 2
fi
# The Host env's own values, as the units see them: every key the file names
# is dropped from this shell first, so an exported one cannot stand in for it.
while IFS= read -r _l || [ -n "${_l}" ]; do
    _l="${_l#"${_l%%[![:space:]]*}"}"; _l="${_l#export }"
    case "${_l}" in [A-Za-z_]*=*) unset "${_l%%=*}" 2>/dev/null ;; esac
done < "$(host_env_file)"
unset GH_TOKEN DAEMON_GH_LOGIN AUTO_AGENT_TARGET_DIR AUTO_AGENT_STATE_DIR HARNESS_CONFIG_JSON
host_env_load
state="$(host_env_state_dir)"
target="${AUTO_AGENT_TARGET_DIR:-}"
slug="$(_harness_config_repo_slug "${target}" 2>/dev/null)" || slug=''
if [ -z "${slug}" ]; then
    echo "soak-check: the Host env's AUTO_AGENT_TARGET_DIR (${target:-unset}) is not a checkout with a GitHub origin" >&2
    exit 2
fi

# -- identity
login="$(GH_TOKEN="${GH_TOKEN:-}" "${GH}" api user --jq .login 2>/dev/null)" || login=''
if [ -z "${DAEMON_GH_LOGIN:-}" ]; then
    line identity FAIL "the Host env names no machine user (DAEMON_GH_LOGIN)"
elif [ "${login}" != "${DAEMON_GH_LOGIN}" ]; then
    line identity FAIL "the Host env's token logs in as ${login:-nobody}, not ${DAEMON_GH_LOGIN}"
elif [ "${login}" = "${slug%%/*}" ]; then
    line identity FAIL "${login} owns ${slug}: the Daemon must act as a machine user, never the owner's account"
else
    line identity ok "the Daemon acts as ${login} on ${slug}"
fi

# -- units
bad=()
for u in auto-agent-daemon.service auto-agent-dashboard.service; do
    [ "$("${SYSTEMCTL}" is-active "${u}" 2>/dev/null)" = "active" ] || bad+=("${u} is not active")
done
for u in ${old_units}; do
    [ "$("${SYSTEMCTL}" is-active "${u}" 2>/dev/null)" = "active" ] && bad+=("${u} is still active")
    [ "$("${SYSTEMCTL}" is-enabled "${u}" 2>/dev/null)" = "enabled" ] && bad+=("${u} is still enabled")
done
if [ "${#bad[@]}" -gt 0 ]; then
    line units FAIL "$(IFS=';'; printf '%s' "${bad[*]}" | sed 's/;/; /g')"
else
    line units ok "auto-agent-daemon and auto-agent-dashboard active; no old unit enabled or active"
fi

# The Fire records that ended OK, oldest first.
records() {
    local f
    for f in "${state}/fires"/*.json; do
        [ -f "${f}" ] || continue
        jq -c 'select(.exit == 0 and (.dryRun | not) and ((.outcome.status // "OK") == "OK"))
               | {fireId, kind: .work.kind, issue: .work.issue, pr: .work.pr, settled: .work.settled}' "${f}" 2>/dev/null
    done
}
all="$(records)"

# -- afk-ticket
picked="$(printf '%s\n' "${all}" | jq -r 'select(.kind == "pick" and .issue != null) | .issue' 2>/dev/null | sort -un)"
if [ -z "${picked}" ]; then
    line afk-ticket waiting "no pick Fire has ended OK from this install yet (${state}/fires)"
else
    done_issue=''; open_note=''
    for n in ${picked}; do
        pr="$(GH_TOKEN="${GH_TOKEN:-}" "${GH}" pr list --repo "${slug}" --state merged --head "${HARNESS_BRANCH_FEATURE_PREFIX}${n}" \
            --json number,author --jq '.[0] | select(. != null) | "\(.number) \(.author.login)"' 2>/dev/null)" || pr=''
        if [ -n "${pr}" ] && [ "${pr#* }" = "${DAEMON_GH_LOGIN:-}" ]; then
            done_issue="#${n} by merged PR #${pr% *} (opened by ${pr#* })"; break
        elif [ -n "${pr}" ]; then
            open_note="#${n}'s merged PR #${pr% *} was opened by ${pr#* }, not by the machine user: it does not count"
        else
            open_note="#${n} was picked; its Agent PR has not merged"
        fi
    done
    if [ -n "${done_issue}" ]; then line afk-ticket ok "${done_issue}"
    else line afk-ticket waiting "${open_note}"; fi
fi

# -- resolve
resolved="$(printf '%s\n' "${all}" | jq -r 'select(.kind == "resolve" and .settled == "done") | "#\(.issue) (Fire \(.fireId))"' 2>/dev/null | head -1)"
if [ -n "${resolved}" ]; then
    line resolve ok "a resolve Fire settled ${resolved}"
elif printf '%s\n' "${all}" | jq -e 'select(.kind == "resolve")' >/dev/null 2>&1; then
    line resolve waiting "a resolve Fire ran but did not settle its ticket as done"
else
    line resolve waiting "no resolve Fire has ended OK from this install yet"
fi

# -- dashboard
port="${AUTO_AGENT_DASHBOARD_PORT:-8090}"
status="$("${CURL}" -fsS --max-time 20 "http://127.0.0.1:${port}/api/status" 2>/dev/null)" || status=''
if [ -z "${status}" ]; then
    line dashboard FAIL "http://127.0.0.1:${port}/api/status did not answer"
elif [ "$(printf '%s' "${status}" | jq -r '.host.stateDir // empty' 2>/dev/null)" != "${state}" ]; then
    line dashboard FAIL "the Dashboard reads $(printf '%s' "${status}" | jq -r '.host.stateDir // "nothing"' 2>/dev/null), not ${state}"
else
    n="$(printf '%s' "${status}" | jq -r '.fires.items | length' 2>/dev/null)"
    if [ "${n:-0}" -gt 0 ]; then line dashboard ok "/api/status lists ${n} Fire(s) from ${state}"
    else line dashboard waiting "/api/status answers from ${state} but lists no Fire yet"; fi
fi

if [ "${#FAILED[@]}" -gt 0 ]; then
    echo "soak: FAIL — ${FAILED[*]}"
    exit 1
fi
if [ "${#WAITING[@]}" -gt 0 ]; then
    echo "soak: NOT YET — ${WAITING[*]}"
    exit 3
fi
echo "soak: PASS — the deletion PR may merge; that ends the rollback window"
