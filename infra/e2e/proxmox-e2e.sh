#!/usr/bin/env bash
# The harness's end-to-end test (Spec #23, Testing Decisions: the Setup seam):
# provision a throwaway VM on Proxmox, run unattended Setup against the
# fixture Target Project, wait for the Daemon's first green Fire, check the
# Dashboard's /api/status, and destroy the VM whatever happened, keeping the
# logs. Run on demand by a human with Proxmox credentials, never per PR. The
# runbook is docs/runbooks/e2e-proxmox.md.
#
# Usage:
#   proxmox-e2e.sh --fixture-repo <owner/name> --gh-login <login> \
#       --gh-token-file <f> --claude-token-file <f> --proxmox-token-file <f> \
#       [--name <name>] [--model <model>] [--fire-timeout <secs>] \
#       [--log-dir <dir>] [--keep-vm] [setup --provision proxmox options...]
#   proxmox-e2e.sh --teardown --name <name> --proxmox-token-file <f> [--log-dir <dir>]
#
#   --fixture-repo    E2E_FIXTURE_REPO: a GitHub repo kept for this test. The
#                     run commits plugin/fixtures/target-project (with
#                     provider-lib.sh copied in beside its provider) onto its
#                     default branch; it must have no open issue or PR, so
#                     the Fire finds no work and ends quickly
#   --gh-login, --gh-token-file
#                     the machine user (admin on the fixture repo) and its
#                     classic PAT; the fixture push uses the same token
#   --claude-token-file
#                     a `claude setup-token` token: the run is unattended, so
#                     the Host runs in setup-token auth mode
#   --proxmox-token-file
#                     PROXMOX_VE_API_TOKEN's value; used for the provision
#                     and again for the destroy
#   --name            the VM and Host inventory name (auto-agent-e2e). Its
#                     provision settings are remembered, so a later run needs
#                     only the secrets
#   --model           AUTO_AGENT_FIRE_MODEL on the Host (haiku: cheap)
#   --fire-timeout    how long to wait for the first finished Fire (1800)
#   --log-dir         where the logs go (${XDG_STATE_HOME:-~/.local/state}/auto-agent-e2e/<name>-<utc>)
#   --keep-vm         skip the destroy, to debug a failure on the live VM;
#                     tear it down afterwards with --teardown
#   Every other option is `setup --provision proxmox`'s (--proxmox-endpoint,
#   --node, --ipv4, --gateway, --ssh-identity, --ref, ...), handed on. The
#   Harness install on the VM is --ref, else this clone's HEAD, which must be
#   pushed. The Target Project checkout on the Host is ~/e2e-fixture.
#
# Stages, each printing `e2e: <stage>: ok|FAIL|skipped — <detail>`:
#   preflight  inputs, this clone's HEAD is pushed, no VM left from a
#              previous run under the same name
#   fixture    the fixture committed to the fixture repo; no open issue or PR
#   setup      `bin/auto-agent setup --provision proxmox ... --unattended`
#   fire       the Daemon's first finished pickup Fire, read from the
#              Dashboard's /api/status over SSH: green is exit 0, outcome OK
#   status     /api/status healthy: the Daemon unit active and not parked,
#              the Fire history fresh, the repo the fixture, no bootstrap warning
#   collect    status.json, the units' journal and the State dir into the logs
#   destroy    the VM destroyed (lib/setup-provision.sh destroy)
# then `e2e: PASS — ...` or `e2e: FAIL — <stage> (exit <n>); logs in <dir>`.
# collect and destroy run whenever a VM may exist, on failure and on Ctrl-C.
#
# Exit codes: 0 pass, 2 usage, 20 preflight, 21 fixture, 22 setup, 23 fire,
# 24 status, 25 destroy failed (wins over every other: a VM may still run),
# 130 interrupted.
#
# Secrets stay in the files they are named by: never argv, never a log. The
# fixture push reads the PAT into GH_TOKEN for gh and git only.
#
# Env (test seams): E2E_CLI (bin/auto-agent), E2E_PROVISION_LIB
# (lib/setup-provision.sh), E2E_FIXTURE_URL (the fixture repo's clone URL),
# E2E_POLL_SECS (20), GH_BIN, GIT_BIN, SSH_BIN, AUTO_AGENT_INVENTORY_DIR.

set -uo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTO_AGENT_ROOT="$(cd "${E2E_DIR}/../.." && pwd)"
# shellcheck source=../../lib/setup-remote.sh
. "${AUTO_AGENT_ROOT}/lib/setup-remote.sh"

E2E_CLI="${E2E_CLI:-${AUTO_AGENT_ROOT}/bin/auto-agent}"
E2E_PROVISION_LIB="${E2E_PROVISION_LIB:-${AUTO_AGENT_ROOT}/lib/setup-provision.sh}"
E2E_FIXTURE="${AUTO_AGENT_ROOT}/plugin/fixtures/target-project"
E2E_TARGET_DIR="e2e-fixture"

E2E_LINES=()
_e2e_line() {
    local line="e2e: $1: $2 — $3"
    printf '%s\n' "${line}"
    E2E_LINES+=("${line}")
    [ -n "${E_LOG:-}" ] && [ -d "${E_LOG}" ] && printf '%s\n' "${line}" >> "${E_LOG}/summary.txt"
    return 0
}
_e2e_err() { echo "e2e: $*" >&2; }
_e2e_gh() { "${GH_BIN:-gh}" "$@"; }
_e2e_git() { "${GIT_BIN:-git}" "$@"; }

# _e2e_host_loaded : the inventory entry of the VM, when Setup wrote one
_e2e_host_loaded() {
    local k; for k in ${REMOTE_INVENTORY_KEYS}; do printf -v "${k}" '%s' ""; done
    AUTO_AGENT_HOST_SSH_PORT=22
    _remote_inventory_load "${E_NAME}" 2>/dev/null || return 1
    AUTO_AGENT_HOST_SSH_PORT="${AUTO_AGENT_HOST_SSH_PORT:-22}"
    [ -n "${AUTO_AGENT_HOST_SSH_IDENTITY:-}" ] || AUTO_AGENT_HOST_SSH_IDENTITY="${E_IDENTITY}"
    [ -n "${AUTO_AGENT_HOST_SSH}" ]
}

# _e2e_status : /api/status from the Dashboard on the Host, over SSH
_e2e_status() {
    # shellcheck disable=SC2016
    _remote_ssh 'f="${AUTO_AGENT_HOST_ENV:-$HOME/.config/auto-agent/env}"
        port="$(sed -n "s/^AUTO_AGENT_DASHBOARD_PORT=//p" "$f" 2>/dev/null | tail -1)"
        curl -fsS --max-time 30 "http://127.0.0.1:${port:-8090}/api/status"' 2>/dev/null
}

# ------------------------------------------------------------ stages

_e2e_preflight() {
    local missing=() v c
    for v in fixture_repo:--fixture-repo gh_login:--gh-login gh_token_file:--gh-token-file \
             claude_token_file:--claude-token-file; do
        c="E_${v%%:*}"; c="${c^^}"
        [ -n "${!c}" ] || missing+=("${v#*:}")
    done
    [ -n "${E_PVE_TOKEN_FILE}" ] || [ -n "${PROXMOX_VE_API_TOKEN:-}" ] || missing+=("--proxmox-token-file")
    if [ "${#missing[@]}" -gt 0 ]; then
        _e2e_err "missing ${missing[*]} (see --help and docs/runbooks/e2e-proxmox.md)"; return 2
    fi
    case "${E_FIXTURE_REPO}" in */*) ;; *) _e2e_err "--fixture-repo takes owner/name"; return 2 ;; esac
    for v in "${E_GH_TOKEN_FILE}" "${E_CLAUDE_TOKEN_FILE}" ${E_PVE_TOKEN_FILE:+"${E_PVE_TOKEN_FILE}"}; do
        _setup_read_secret_file "${v}" >/dev/null || { _e2e_line preflight FAIL "cannot read a secret from ${v}"; return 20; }
    done
    for c in jq "${GH_BIN:-gh}" "${GIT_BIN:-git}"; do
        command -v "${c}" >/dev/null 2>&1 || { _e2e_line preflight FAIL "missing on this machine: ${c}"; return 20; }
    done
    local inv; inv="$(setup_inventory_dir)"
    if [ -f "${inv}/${E_NAME}.env" ] || [ -f "${inv}/${E_NAME}.proxmox.tfstate" ]; then
        _e2e_line preflight FAIL "a Host named ${E_NAME} is still in ${inv}: a previous run's VM may be up. Tear it down first: $0 --teardown --name ${E_NAME} --proxmox-token-file <f>"
        return 20
    fi
    local ref="${E_REF}"
    if [ -z "${ref}" ]; then
        # The VM clones the install from origin: HEAD has to be there.
        ref="$(_e2e_git -C "${AUTO_AGENT_ROOT}" rev-parse HEAD 2>/dev/null)"
        if [ -z "$(_e2e_git -C "${AUTO_AGENT_ROOT}" branch -r --contains HEAD 2>/dev/null)" ]; then
            _e2e_line preflight FAIL "this clone's HEAD ${ref:0:12} is on no remote branch: push it, or pass --ref"
            return 20
        fi
        [ -n "$(_e2e_git -C "${AUTO_AGENT_ROOT}" status --porcelain 2>/dev/null)" ] \
            && echo "e2e: note — uncommitted changes in this clone do not reach the VM (it installs ${ref:0:12})"
    fi
    _e2e_line preflight ok "VM ${E_NAME}, fixture ${E_FIXTURE_REPO}, install ${ref:0:12}, model ${E_MODEL}, logs in ${E_LOG}"
}

_e2e_fixture() {
    local token work="${E_TMP}/fixture" url="${E2E_FIXTURE_URL:-https://github.com/${E_FIXTURE_REPO}.git}" branch n
    token="$(_setup_read_secret_file "${E_GH_TOKEN_FILE}")" || { _e2e_line fixture FAIL "cannot read ${E_GH_TOKEN_FILE}"; return 21; }
    local cred=(-c credential.helper= -c "credential.helper=!GH_TOKEN=\"\$E2E_GH_TOKEN\" ${GH_BIN:-gh} auth git-credential")
    branch="$(GH_TOKEN="${token}" _e2e_gh repo view "${E_FIXTURE_REPO}" --json defaultBranchRef --jq '.defaultBranchRef.name // ""' 2>>"${E_LOG}/fixture.log")" \
        || { _e2e_line fixture FAIL "cannot read ${E_FIXTURE_REPO} as ${E_GH_LOGIN} (does it exist, and does the PAT reach it?)"; return 21; }
    branch="${branch:-main}"
    if ! E2E_GH_TOKEN="${token}" _e2e_git "${cred[@]}" clone -q "${url}" "${work}" >>"${E_LOG}/fixture.log" 2>&1; then
        _e2e_line fixture FAIL "cannot clone ${url} (see fixture.log)"; return 21
    fi
    (
        cd "${work}" || exit 1
        if _e2e_git rev-parse -q --verify "refs/remotes/origin/${branch}" >/dev/null; then
            _e2e_git checkout -q -B "${branch}" "origin/${branch}"
        else
            _e2e_git checkout -q -b "${branch}" 2>/dev/null || true
        fi
        find . -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
        cp -R "${E2E_FIXTURE}/." . && cp "${AUTO_AGENT_ROOT}/plugin/providers/provider-lib.sh" verify/
        # The fixture's $schema points into the plugin, which a standalone repo lacks.
        jq 'del(."$schema")' .auto-agent/harness.json > .auto-agent/harness.json.new \
            && mv .auto-agent/harness.json.new .auto-agent/harness.json
        _e2e_git add -A
    ) >>"${E_LOG}/fixture.log" 2>&1 || { _e2e_line fixture FAIL "cannot lay the fixture into the clone (see fixture.log)"; return 21; }
    local what
    if _e2e_git -C "${work}" diff --cached --quiet 2>/dev/null && _e2e_git -C "${work}" rev-parse -q --verify HEAD >/dev/null 2>&1; then
        what="already the fixture at ${branch}"
    else
        local sha; sha="$(_e2e_git -C "${AUTO_AGENT_ROOT}" rev-parse --short HEAD 2>/dev/null)"
        if ! _e2e_git -C "${work}" -c user.name="auto-agent e2e" -c user.email="auto-agent-e2e@users.noreply.github.com" \
                commit -q -m "chore: sync the fixture Target Project from auto-agent ${sha}" >>"${E_LOG}/fixture.log" 2>&1 \
           || ! E2E_GH_TOKEN="${token}" _e2e_git -C "${work}" "${cred[@]}" push -q origin "HEAD:refs/heads/${branch}" >>"${E_LOG}/fixture.log" 2>&1; then
            _e2e_line fixture FAIL "cannot commit and push the fixture to ${E_FIXTURE_REPO} ${branch} (see fixture.log)"; return 21
        fi
        what="fixture pushed to ${branch}"
    fi
    n="$(GH_TOKEN="${token}" _e2e_gh issue list -R "${E_FIXTURE_REPO}" --state open --json number --jq length 2>>"${E_LOG}/fixture.log")" \
        || { _e2e_line fixture FAIL "cannot list ${E_FIXTURE_REPO}'s issues"; return 21; }
    local p; p="$(GH_TOKEN="${token}" _e2e_gh pr list -R "${E_FIXTURE_REPO}" --state open --json number --jq length 2>>"${E_LOG}/fixture.log")" \
        || { _e2e_line fixture FAIL "cannot list ${E_FIXTURE_REPO}'s PRs"; return 21; }
    if [ "${n}" != "0" ] || [ "${p}" != "0" ]; then
        _e2e_line fixture FAIL "${E_FIXTURE_REPO} has ${n} open issue(s) and ${p} open PR(s): close them, so the Fire finds no work"
        return 21
    fi
    _e2e_line fixture ok "${what}; no open issue or PR"
}

_e2e_setup() {
    local cmd=("${E2E_CLI}" setup --provision proxmox --name "${E_NAME}" --unattended
        --auth-mode setup-token --repo "${E_FIXTURE_REPO}" --gh-login "${E_GH_LOGIN}"
        --gh-token-file "${E_GH_TOKEN_FILE}" --claude-token-file "${E_CLAUDE_TOKEN_FILE}"
        --set "AUTO_AGENT_FIRE_MODEL=${E_MODEL}")
    [ -n "${E_PVE_TOKEN_FILE}" ] && cmd+=(--proxmox-token-file "${E_PVE_TOKEN_FILE}")
    [ -n "${E_IDENTITY}" ] && cmd+=(--ssh-identity "${E_IDENTITY}")
    [ -n "${E_REF}" ] && cmd+=(--ref "${E_REF}")
    cmd+=("${E_PASS[@]+"${E_PASS[@]}"}" "${E2E_TARGET_DIR}")
    E_VM_MAYBE=1
    "${cmd[@]}" < /dev/null 2>&1 | tee "${E_LOG}/setup.log"
    local rc="${PIPESTATUS[0]}"
    if [ "${rc}" -ne 0 ]; then
        local why; why="$(grep -m1 -E '^setup: [a-z-]+: FAIL' "${E_LOG}/setup.log" | sed 's/^setup: //')"
        _e2e_line setup FAIL "setup exited ${rc}: ${why:-see setup.log}"
        return 22
    fi
    _e2e_host_loaded || { _e2e_line setup FAIL "setup exited 0 but left no inventory entry for ${E_NAME}"; return 22; }
    _e2e_line setup ok "${E_NAME} at ${AUTO_AGENT_HOST_SSH}: $(tail -1 "${E_LOG}/setup.log" | sed 's/^setup: //')"
}

_e2e_fire() {
    local waited=0 limit="${E_FIRE_TIMEOUT}" poll="${E2E_POLL_SECS:-20}" status fire answered=0
    while :; do
        if status="$(_e2e_status)" && jq -e .fires <<< "${status}" >/dev/null 2>&1; then
            answered=1
            printf '%s\n' "${status}" > "${E_LOG}/status.json"
            fire="$(jq -c '[.fires.items[]? | select(.kind == "pickup" and .inFlight == false)] | last // empty' <<< "${status}")"
            [ -n "${fire}" ] && break
        fi
        if [ "${waited}" -ge "${limit}" ]; then
            if [ "${answered}" = 1 ]; then
                _e2e_line fire FAIL "no pickup Fire finished within ${limit}s (daemon: $(jq -r '.daemon.state + " — " + (.daemon.stateDetail // "")' "${E_LOG}/status.json"))"
            else
                _e2e_line fire FAIL "the Dashboard's /api/status never answered within ${limit}s (see journal.log)"
            fi
            return 23
        fi
        sleep "${poll}"; waited=$((waited + poll))
    done
    local id summary; id="$(jq -r .id <<< "${fire}")"; summary="$(jq -r '.summary // ""' <<< "${fire}")"
    if ! jq -e '.exit == 0 and .outcome == "OK" and .phase == "claude"' <<< "${fire}" >/dev/null; then
        _e2e_line fire FAIL "Fire ${id} was not green: exit $(jq -r .exit <<< "${fire}"), outcome $(jq -r .outcome <<< "${fire}"), phase $(jq -r .phase <<< "${fire}"): ${summary}"
        return 23
    fi
    _e2e_line fire ok "Fire ${id} green: ${summary}, \$$(jq -r '.costUsd // "?"' <<< "${fire}") on $(jq -r '.model // "?"' <<< "${fire}")"
}

_e2e_health() {
    local status bad
    status="$(_e2e_status)" && jq -e . <<< "${status}" >/dev/null 2>&1 || {
        _e2e_line status FAIL "/api/status did not answer with JSON"; return 24; }
    printf '%s\n' "${status}" > "${E_LOG}/status.json"
    bad="$(jq -r --arg repo "${E_FIXTURE_REPO}" '[
        (if .daemon.unit.active != "active" then "the Daemon unit is \(.daemon.unit.active)" else empty end),
        (if .daemon.parked != null then "the Daemon is parked" else empty end),
        (if .fires.stale != false or .fires.error != null then "the Fire history is stale (\(.fires.error))" else empty end),
        (if .host.repo != $repo then "the repo is \(.host.repo), not \($repo)" else empty end),
        (if .bootstrap.warning == true then "a bootstrap warning (the fixture declares a hermetic tier)" else empty end)
      ] | join("; ")' <<< "${status}")"
    if [ -n "${bad}" ]; then
        _e2e_line status FAIL "${bad}"; return 24
    fi
    _e2e_line status ok "healthy: Daemon $(jq -r .daemon.state <<< "${status}"), $(jq -r '.fires.items | length' <<< "${status}") Fire(s) recorded, repo ${E_FIXTURE_REPO}"
}

_e2e_collect() {
    if ! _e2e_host_loaded; then
        _e2e_line collect skipped "no inventory entry for ${E_NAME}: Setup never reached the VM"; return 0
    fi
    local got=()
    _e2e_status > "${E_LOG}/status.final.json" && got+=(status.final.json) || rm -f "${E_LOG}/status.final.json"
    # shellcheck disable=SC2016
    _remote_ssh 'sudo -n journalctl -u auto-agent-daemon.service -u auto-agent-dashboard.service --no-pager -o short-iso 2>/dev/null \
        || journalctl -u auto-agent-daemon.service -u auto-agent-dashboard.service --no-pager -o short-iso' \
        > "${E_LOG}/journal.log" 2>/dev/null && got+=(journal.log)
    # shellcheck disable=SC2016
    _remote_ssh 'f="${AUTO_AGENT_HOST_ENV:-$HOME/.config/auto-agent/env}"
        d="$(sed -n "s/^AUTO_AGENT_STATE_DIR=//p" "$f" 2>/dev/null | tail -1)"; d="${d:-$HOME/.local/state/auto-agent}"
        [ -d "$d" ] && tar -C "$d" -czf - .' > "${E_LOG}/state.tgz" 2>/dev/null && [ -s "${E_LOG}/state.tgz" ] \
        && got+=(state.tgz) || rm -f "${E_LOG}/state.tgz"
    if [ "${#got[@]}" -eq 0 ]; then
        _e2e_line collect FAIL "nothing read back from ${AUTO_AGENT_HOST_SSH}"
    else
        _e2e_line collect ok "${got[*]}"
    fi
}

_e2e_destroy() {
    if [ "${E_KEEP}" = 1 ]; then
        _e2e_line destroy skipped "--keep-vm: ${E_NAME} is still running; tear it down with: $0 --teardown --name ${E_NAME} --proxmox-token-file <f>"
        return 0
    fi
    local cmd=(bash "${E2E_PROVISION_LIB}" destroy --name "${E_NAME}")
    [ -n "${E_PVE_TOKEN_FILE}" ] && cmd+=(--proxmox-token-file "${E_PVE_TOKEN_FILE}")
    "${cmd[@]}" < /dev/null 2>&1 | tee "${E_LOG}/destroy.log"
    local rc="${PIPESTATUS[0]}"
    if [ "${rc}" -ne 0 ]; then
        _e2e_line destroy FAIL "destroy exited ${rc}: ${E_NAME} may still be running on Proxmox. Fix the cause in destroy.log, then: $0 --teardown --name ${E_NAME} --proxmox-token-file <f>"
        return 25
    fi
    local last; last="$(grep '^setup: destroy: ' "${E_LOG}/destroy.log" | tail -1)"; last="${last#setup: destroy: }"
    case "${last}" in
        "skipped — "*) _e2e_line destroy skipped "${last#skipped — }" ;;
        *) _e2e_line destroy ok "${last#changed — }" ;;
    esac
}

# _e2e_finish <rc> : collect and destroy once, then the verdict
_e2e_finish() {
    local rc="$1" drc=0
    [ "${E_FINISHED:-0}" = 1 ] && return
    E_FINISHED=1
    trap - INT TERM
    if [ "${E_VM_MAYBE:-0}" = 1 ]; then
        _e2e_collect
        _e2e_destroy || drc=$?
    fi
    [ "${drc}" -ne 0 ] && rc="${drc}"
    rm -rf "${E_TMP}"
    if [ "${rc}" -eq 0 ]; then
        echo "e2e: PASS — ${E_MODE_DONE}; logs in ${E_LOG}" | tee -a "${E_LOG}/summary.txt"
    else
        local where; where="$(printf '%s\n' "${E2E_LINES[@]}" | grep -m1 ': FAIL — ' | cut -d: -f2 | tr -d ' ')"
        [ "${rc}" -eq 130 ] && where="interrupted"
        echo "e2e: FAIL — ${where:-usage} (exit ${rc}); logs in ${E_LOG}" | tee -a "${E_LOG}/summary.txt"
    fi
    exit "${rc}"
}

e2e_main() {
    E_FIXTURE_REPO="${E2E_FIXTURE_REPO:-}"; E_GH_LOGIN=""; E_GH_TOKEN_FILE=""; E_CLAUDE_TOKEN_FILE=""
    E_PVE_TOKEN_FILE=""; E_NAME="auto-agent-e2e"; E_MODEL="haiku"; E_FIRE_TIMEOUT=1800; E_LOG=""
    E_KEEP=0; E_TEARDOWN=0; E_IDENTITY=""; E_REF=""; E_PASS=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --fixture-repo) E_FIXTURE_REPO="${2:-}"; shift ;;
            --gh-login) E_GH_LOGIN="${2:-}"; shift ;;
            --gh-token-file) E_GH_TOKEN_FILE="${2:-}"; shift ;;
            --claude-token-file) E_CLAUDE_TOKEN_FILE="${2:-}"; shift ;;
            --proxmox-token-file) E_PVE_TOKEN_FILE="${2:-}"; shift ;;
            --name) E_NAME="${2:-}"; shift ;;
            --model) E_MODEL="${2:-}"; shift ;;
            --fire-timeout) E_FIRE_TIMEOUT="${2:-}"; shift ;;
            --log-dir) E_LOG="${2:-}"; shift ;;
            --keep-vm) E_KEEP=1 ;;
            --teardown) E_TEARDOWN=1 ;;
            --ssh-identity) E_IDENTITY="${2:-}"; shift ;;
            --ref) E_REF="${2:-}"; shift ;;
            --auth-mode|--unattended|--repo|--set|--host)
                _e2e_err "$1 is the test's to set (setup-token, unattended, the fixture repo, the model)"; return 2 ;;
            -h|--help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; return 0 ;;
            --proxmox-insecure) E_PASS+=("$1") ;;
            --*) [ $# -ge 2 ] || { _e2e_err "$1 needs a value"; return 2; }
                 E_PASS+=("$1" "$2"); shift ;;
            *) _e2e_err "unexpected argument '$1'"; return 2 ;;
        esac
        shift
    done
    case "${E_NAME}" in ''|*[!A-Za-z0-9-]*|-*) _e2e_err "--name: letters, digits and dashes"; return 2 ;; esac
    case "${E_FIRE_TIMEOUT}" in ''|*[!0-9]*) _e2e_err "--fire-timeout: whole seconds"; return 2 ;; esac
    E_LOG="${E_LOG:-${XDG_STATE_HOME:-${HOME}/.local/state}/auto-agent-e2e/${E_NAME}-$(date -u +%Y%m%dT%H%M%SZ)}"
    mkdir -p "${E_LOG}" || { _e2e_err "cannot create ${E_LOG}"; return 2; }
    E_TMP="$(mktemp -d)" || return 1
    E_VM_MAYBE=0
    trap '_e2e_finish 130' INT TERM

    if [ "${E_TEARDOWN}" = 1 ]; then
        E_MODE_DONE="${E_NAME} torn down"
        E_VM_MAYBE=1
        _e2e_finish 0
    fi
    E_MODE_DONE="a green Fire on ${E_NAME} and a healthy /api/status, VM destroyed"
    local rc=0
    _e2e_preflight || rc=$?
    if [ "${rc}" -eq 2 ]; then rm -rf "${E_TMP}"; return 2; fi
    [ "${rc}" -eq 0 ] && { _e2e_fixture || rc=$?; }
    [ "${rc}" -eq 0 ] && { _e2e_setup || rc=$?; }
    [ "${rc}" -eq 0 ] && { _e2e_fire || rc=$?; }
    [ "${rc}" -eq 0 ] && { _e2e_health || rc=$?; }
    _e2e_finish "${rc}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    e2e_main "$@"
    exit $?
fi
