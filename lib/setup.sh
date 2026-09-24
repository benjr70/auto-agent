#!/usr/bin/env bash
# Setup engine: the fixed stages that turn a Target Project plus the Host
# this runs in into a running Daemon and Dashboard (ADR 0004, ADR 0005, ADR 0009).
# `/auto-agent:setup` is the conversation in front of it; this lib is
# everything mechanical and the only thing that writes to a Host, so the
# skill can never do what the unattended path cannot.
#
# This is the in-VM entry point: this repo checked out inside the Host is then
# the Harness install. The remote entry points (bring your own VM,
# Proxmox) reach the same configure step over SSH.
#
# Usage:
#   setup.sh setup [options] [<target-dir>]
#       Runs every stage in order and stops at the first that fails:
#         baseline   Ubuntu 24.04 (distribution first), x86_64 or arm64,
#                    systemd, passwordless sudo, outbound internet, and one
#                    Daemon per Target Project on this Host. It runs before
#                    doctor here because in this entry point the Operator
#                    machine is the Host: doctor's install lines assume the
#                    distribution the baseline asserts (AC: the baseline
#                    fails first on a wrong distribution)
#         doctor     the commands this machine needs; names each missing one
#                    with its install command, never installs
#         github     the machine user's classic PAT: the expected login, the
#                    repo/project/workflow scopes, admin on the repo
#         claude     the Claude auth mode and its login (`claude auth status`
#                    in the declared mode; `/login` is this entry point's
#                    precondition, so the stage is a verify)
#         config     the Target Project checkout (cloned when missing) and its
#                    Harness config, scaffolded as a machine-user PR when the
#                    checkout has none (skipped once it exists, or while that
#                    PR is open)
#         configure  the Ansible configure step (infra/ansible): base needs
#                    derived from the Surfaces and host.docker, Xvfb, fonts,
#                    the Electron AppArmor grant, the Host env (no_log), units
#         extension  the Target Project's Host extension, when it has one
#         verify     what `check` runs (below)
#         enable     both units enabled and started (restarted when the
#                    configure step changed anything, or when the remote entry
#                    point moved the Harness install: AUTO_AGENT_SETUP_INSTALL_MOVED=1)
#         bootstrap  the one bootstrap issue, when the Harness config has no
#                    hermetic tier (idempotent by marker)
#         summary    one line per stage, then `setup: converged` when no stage
#                    changed anything, else `setup: done — <n> changed: ...`
#       Re-running converges: secrets are asked for only when the Host env
#       lacks them (unless --rotate), the scaffold is skipped when the config
#       exists, Ansible is idempotent, the bootstrap issue is found by marker.
#
#   setup.sh upgrade [--set KEY=VALUE]... [<target-dir>]
#       The subset an upgrade runs once the Harness install sits at its new
#       ref: baseline, doctor, github and claude (from the Host env, never
#       prompting), config (never opens a PR), configure, extension, then
#       restart both units. Moving the ref is the caller's job: the remote
#       entry point's install play moves it before this runs; in-VM, move this
#       checkout first (a running script must not rewrite itself).
#
#   setup.sh check [<target-dir>]
#       The verify stage alone, reading everything from the Host env: the Host
#       env (0600, every secret present), the machine login and admin, `claude
#       auth status` in the declared mode, the schema, the Provider check (when
#       a hermetic tier is declared) and one dry-run Fire. Writes nothing.
#
# Remote entry point: `setup --host <user@vm|name> ...`, `upgrade <name>` and
# `check <name>` (a name the Host inventory holds) run from the Operator
# machine through lib/setup-remote.sh, which drives these same commands on the
# Host over SSH.
#
# Proxmox entry point: `setup --provision proxmox ...` (lib/setup-provision.sh)
# provisions the Host with terraform, then runs the remote entry point against it.
#
# Every stage prints `setup: <stage>: ok|changed|skipped|FAIL — <detail>` (and
# `check` prints `check: <item>: ...`); the skill reads those lines.
#
# Options (each prompt has a flag and an environment override; with neither,
# setup asks on the terminal, and fails naming both when there is none or
# --unattended is given):
#   <target-dir>              AUTO_AGENT_SETUP_TARGET, else the Host env's
#                             AUTO_AGENT_TARGET_DIR
#   --repo <owner/name>       AUTO_AGENT_SETUP_REPO: clone it to <target-dir>
#                             when the checkout is missing
#   --gh-login <login>        AUTO_AGENT_SETUP_GH_LOGIN: the machine user
#   --gh-token-file <file>    AUTO_AGENT_SETUP_GH_TOKEN (the value): its PAT
#   --auth-mode <mode>        AUTO_AGENT_SETUP_AUTH_MODE: login (default) or
#                             setup-token; api-key refuses (no spend pacing yet)
#   --claude-token-file <f>   AUTO_AGENT_SETUP_CLAUDE_TOKEN (the value): the
#                             `claude setup-token` token, setup-token mode only
#   --config <file>           AUTO_AGENT_SETUP_CONFIG: the harness.json draft
#                             the config stage proposes (the skill writes it);
#                             a minimal label-only skeleton otherwise
#   --config-pr-body <file>   AUTO_AGENT_SETUP_CONFIG_PR_BODY: the body of the
#                             PR that proposes it (the skill drafts it from the
#                             interview); the engine's review checklist otherwise
#   --set KEY=VALUE           any Host env key (repeatable), e.g. DISPLAY,
#                             AUTO_AGENT_DASHBOARD_BIND, AUTO_AGENT_MEMORY_MAX
#   --rotate                  AUTO_AGENT_SETUP_ROTATE=1: ask for the secrets
#                             again even though the Host env holds them
#   --unattended              AUTO_AGENT_SETUP_UNATTENDED=1: never prompt
#
# Exit codes (setup: the failing stage's; check: 0 or 10):
#   0 done or converged, 2 usage, 3 baseline, 4 doctor, 5 github, 6 claude,
#   7 config, 8 configure, 9 extension, 10 verify, 11 enable (or restart),
#   12 bootstrap (the remote entry points add 13 ssh, 14 install or inventory,
#   15 provision)
#
# Secrets never reach argv, stdout, stderr or a log: they travel in the
# environment of the one command that needs them, and into the Host env only
# through a 0600 vars file the Ansible run reads and Setup deletes. A remote
# entry point's secrets arrive through the setup handoff (below), which setup
# reads and deletes before its first stage.
#
# Setup handoff: `setup-handoff` beside the Host env (AUTO_AGENT_SETUP_HANDOFF
# overrides), 0600, written only by the remote entry point's Ansible play
# (no_log). It may carry AUTO_AGENT_SETUP_GH_TOKEN and
# AUTO_AGENT_SETUP_CLAUDE_TOKEN, nothing else, and counts as those overrides.
#
# Env (test seams): GH_BIN, GIT_BIN, CLAUDE_BIN, ANSIBLE_PLAYBOOK_BIN,
# SETUP_SUDO_BIN (sudo), SYSTEMCTL_BIN (systemctl), SETUP_OS_RELEASE
# (/etc/os-release), SETUP_SYSTEMD_RUN_DIR (/run/systemd/system), SETUP_ARCH
# (uname -m), SETUP_NET_PROBE_CMD, SETUP_FIRE_CMD (the dry-run Fire),
# SETUP_PROVIDER_CHECK_CMD, SETUP_BOOTSTRAP_CMD, SETUP_DOCTOR_COMMANDS.

_setup_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTO_AGENT_ROOT="${AUTO_AGENT_ROOT:-$(cd "${_setup_lib_dir}/.." && pwd)}"
# shellcheck source=harness-config.sh
. "${_setup_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_setup_lib_dir}/host-env.sh"
# shellcheck source=usage-sensor.sh
. "${_setup_lib_dir}/usage-sensor.sh"

SETUP_CLI="${AUTO_AGENT_ROOT}/bin/auto-agent"
SETUP_ANSIBLE_DIR="${AUTO_AGENT_ROOT}/infra/ansible"
SETUP_UNITS="auto-agent-daemon.service auto-agent-dashboard.service"
SETUP_BASELINE_VERSION="24.04"
# The classic-PAT scopes every harness operation needs (ADR 0005).
SETUP_GH_SCOPES="repo project workflow"
SETUP_DISPLAY_DEFAULT=":99"

# Stage bookkeeping for the summary.
SETUP_RESULTS=()
SETUP_CHANGED=()

_setup_err() { echo "setup: $*" >&2; }

# _setup_line <stage> <status> <detail> : the stage's one machine line
_setup_line() {
    local stage="$1" status="$2" detail="$3"
    printf 'setup: %s: %s — %s\n' "${stage}" "${status}" "${detail}"
    SETUP_RESULTS+=("${stage}: ${status} — ${detail}")
    [ "${status}" = "changed" ] && SETUP_CHANGED+=("${stage}")
    return 0
}

_setup_sudo() { "${SETUP_SUDO_BIN:-sudo}" "$@"; }
_setup_systemctl() { _setup_sudo "${SYSTEMCTL_BIN:-systemctl}" "$@"; }

# _setup_unattended : 0 when no prompt may be shown
_setup_unattended() {
    [ "${AUTO_AGENT_SETUP_UNATTENDED:-0}" = "1" ] && return 0
    [ -t 0 ] || return 0
    return 1
}

# _setup_ask <question> [secret] : one answer from the terminal, or 1
_setup_ask() {
    local question="$1" secret="${2:-}" answer=""
    _setup_unattended && return 1
    if [ -n "${secret}" ]; then
        read -r -s -p "${question}: " answer < /dev/tty || return 1
        echo >&2
    else
        read -r -p "${question}: " answer < /dev/tty || return 1
    fi
    [ -n "${answer}" ] || return 1
    printf '%s' "${answer}"
}

# _setup_read_secret_file <file> : its first line, trimmed
_setup_read_secret_file() {
    local file="$1" v
    [ -r "${file}" ] || return 1
    IFS= read -r v < "${file}" || [ -n "${v}" ] || return 1
    v="${v%$'\r'}"
    v="${v//[[:space:]]/}"
    [ -n "${v}" ] || return 1
    printf '%s' "${v}"
}

# _setup_secret <var> <file-flag-value> <env-override-value> <held> <question> <names>
# Sets <var> to one secret, from the first source that has it: the flag's
# file, the env override, the Host env (unless --rotate), the terminal.
# Returns 1 with the reason in SETUP_SECRET_WHY. Assigns rather than prints,
# so the reason is not lost in a command substitution's subshell.
_setup_secret() {
    local var="$1" file="$2" override="$3" held="$4" question="$5" names="$6" v=""
    if [ -n "${file}" ]; then
        v="$(_setup_read_secret_file "${file}")" || { SETUP_SECRET_WHY="cannot read a token from ${file}"; return 1; }
    elif [ -n "${override}" ]; then v="${override}"
    elif [ "${S_ROTATE}" != "1" ] && [ -n "${held}" ]; then v="${held}"
    else v="$(_setup_ask "${question}" secret)" || { SETUP_SECRET_WHY="no ${names}"; return 1; }
    fi
    printf -v "${var}" '%s' "${v}"
}

# _setup_existing <key> : the value the Host env file holds for <key>, if any.
# Read from the file itself so an exported value from elsewhere never passes
# for what the Host carries.
_setup_existing() {
    local key="$1" file; file="$(host_env_file)"
    [ -f "${file}" ] || return 1
    env -i HOME="${HOME}" PATH="${PATH}" AUTO_AGENT_HOST_ENV="${file}" bash -c '
        . "$1/host-env.sh"; host_env_load 2>/dev/null
        [ -n "${!2:-}" ] || exit 1
        printf "%s" "${!2}"' _ "${_setup_lib_dir}" "${key}"
}

# setup_inventory_dir : the Host inventory on the Operator machine (ADR 0009)
setup_inventory_dir() {
    printf '%s\n' "${AUTO_AGENT_INVENTORY_DIR:-${HOME}/.config/auto-agent/hosts}"
}

setup_handoff_file() {
    printf '%s\n' "${AUTO_AGENT_SETUP_HANDOFF:-$(dirname "$(host_env_file)")/setup-handoff}"
}

# _setup_handoff_consume : moves the remote entry point's secrets into the two
# override variables and deletes the file, whatever this run does next, so a
# secret never outlives the run it was handed to.
_setup_handoff_consume() {
    local file line key; file="$(setup_handoff_file)"
    [ -f "${file}" ] || return 0
    while IFS= read -r line || [ -n "${line}" ]; do
        key="${line%%=*}"
        case "${key}" in
            AUTO_AGENT_SETUP_GH_TOKEN|AUTO_AGENT_SETUP_CLAUDE_TOKEN)
                [ -n "${line#*=}" ] && printf -v "${key}" '%s' "${line#*=}" ;;
        esac
    done < "${file}"
    rm -f "${file}"
}

# _setup_abs <path> : absolute, without resolving symlinks away
_setup_abs() {
    case "$1" in
        /*) printf '%s\n' "${1%/}" ;;
        *) printf '%s/%s\n' "$(pwd)" "${1%/}" ;;
    esac
}

# ---------------------------------------------------------------- baseline

setup_stage_baseline() {
    local target="$1" os="${SETUP_OS_RELEASE:-/etc/os-release}" id="" ver="" arch
    # Distribution first (ADR 0004): every later check assumes it.
    if [ -r "${os}" ]; then
        id="$(. "${os}" 2>/dev/null; printf '%s' "${ID:-}")"
        ver="$(. "${os}" 2>/dev/null; printf '%s' "${VERSION_ID:-}")"
    fi
    if [ "${id}" != "ubuntu" ] || [ "${ver}" != "${SETUP_BASELINE_VERSION}" ]; then
        _setup_line baseline FAIL "this Host runs ${id:-an unknown distribution} ${ver}; the reference Host is Ubuntu ${SETUP_BASELINE_VERSION} LTS (ADR 0004)"
        return 3
    fi
    arch="${SETUP_ARCH:-$(uname -m)}"
    case "${arch}" in
        x86_64|aarch64|arm64) ;;
        *) _setup_line baseline FAIL "architecture ${arch}; the reference Host is x86_64 or arm64"; return 3 ;;
    esac
    if [ ! -d "${SETUP_SYSTEMD_RUN_DIR:-/run/systemd/system}" ]; then
        _setup_line baseline FAIL "systemd is not the init system here; the Daemon and Dashboard run as systemd units"
        return 3
    fi
    if ! _setup_sudo -n true >/dev/null 2>&1; then
        _setup_line baseline FAIL "passwordless sudo is required (the configure step installs packages and units)"
        return 3
    fi
    if ! bash -c "${SETUP_NET_PROBE_CMD:-curl -fsS -o /dev/null --max-time 15 https://api.github.com}" >/dev/null 2>&1; then
        _setup_line baseline FAIL "no outbound internet (https://api.github.com did not answer)"
        return 3
    fi
    # One Daemon per Target Project per Host (ticket #11).
    local held; held="$(_setup_existing AUTO_AGENT_TARGET_DIR)" || held=""
    if [ -n "${held}" ] && [ "${held%/}" != "${target}" ]; then
        _setup_line baseline FAIL "this Host already serves ${held} (its Host env says so); one Daemon per Target Project per Host"
        return 3
    fi
    _setup_line baseline ok "Ubuntu ${ver} ${arch}, systemd, passwordless sudo, outbound internet"
}

# ------------------------------------------------------------------ doctor

# _setup_install_hint <command> : how to install it on the reference Host
_setup_install_hint() {
    case "$1" in
        ansible-playbook) echo "sudo apt-get install -y ansible-core" ;;
        claude) echo "curl -fsSL https://claude.ai/install.sh | bash" ;;
        python3) echo "sudo apt-get install -y python3" ;;
        systemctl) echo "(systemd is part of the reference Host image)" ;;
        *) echo "sudo apt-get install -y $1" ;;
    esac
}

setup_stage_doctor() {
    local c missing=()
    for c in ${SETUP_DOCTOR_COMMANDS:-git gh jq curl python3 claude ansible-playbook}; do
        command -v "${c}" >/dev/null 2>&1 || missing+=("${c}")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        for c in "${missing[@]}"; do
            echo "setup: doctor: missing ${c} — install it with: $(_setup_install_hint "${c}")"
        done
        _setup_line doctor FAIL "missing: ${missing[*]} (install them and run setup again; setup never installs on this machine itself)"
        return 4
    fi
    _setup_line doctor ok "every prerequisite is on PATH"
}

# ------------------------------------------------------------------ github

# _setup_slug <target> <repo-flag> : owner/name
_setup_slug() {
    local target="$1" repo="$2"
    if [ -n "${repo}" ]; then printf '%s\n' "${repo}"; return 0; fi
    [ -d "${target}" ] || return 1
    _harness_config_repo_slug "${target}"
}

# _setup_gh_verify <token> <login> <slug> : "" when the PAT is the machine
# user's and fit for the harness, else the reason. The token only ever travels
# in gh's environment.
_setup_gh_verify() {
    local token="$1" login="$2" slug="$3" gh="${GH_BIN:-gh}" got headers scopes s admin
    got="$(GH_TOKEN="${token}" "${gh}" api user --jq .login 2>/dev/null)" || {
        printf 'the PAT was refused by GitHub (gh api user failed)'; return 0; }
    if [ "${got}" != "${login}" ]; then
        printf 'the PAT belongs to %s, not the machine user %s' "${got:-nobody}" "${login}"; return 0
    fi
    headers="$(GH_TOKEN="${token}" "${gh}" api -i user 2>/dev/null)" || headers=""
    scopes="$(printf '%s\n' "${headers}" | tr -d '\r' | awk 'tolower($0) ~ /^x-oauth-scopes:/ { sub(/^[^:]*:[ ]*/, ""); print; exit }')"
    if [ -z "${scopes}" ]; then
        printf 'the PAT carries no OAuth scopes header: the harness needs a classic PAT (%s), ADR 0005' "${SETUP_GH_SCOPES// /, }"; return 0
    fi
    for s in ${SETUP_GH_SCOPES}; do
        case ", ${scopes}," in
            *", ${s},"*) ;;
            *) printf 'the PAT lacks the %s scope (it has: %s)' "${s}" "${scopes}"; return 0 ;;
        esac
    done
    admin="$(GH_TOKEN="${token}" "${gh}" api "repos/${slug}" --jq .permissions.admin 2>/dev/null)" || admin=""
    if [ "${admin}" != "true" ]; then
        printf '%s is not an admin collaborator on %s' "${login}" "${slug}"; return 0
    fi
    printf ''
}

# setup_stage_github <target> : sets SETUP_GH_LOGIN, SETUP_GH_TOKEN, SETUP_SLUG
setup_stage_github() {
    local target="$1" had_login had_token
    had_login="$(_setup_existing DAEMON_GH_LOGIN)" || had_login=""
    had_token="$(_setup_existing GH_TOKEN)" || had_token=""

    SETUP_GH_LOGIN="${S_GH_LOGIN:-${AUTO_AGENT_SETUP_GH_LOGIN:-${had_login}}}"
    [ -n "${SETUP_GH_LOGIN}" ] || SETUP_GH_LOGIN="$(_setup_ask "GitHub machine user login")" || {
        _setup_line github FAIL "no machine user: pass --gh-login or set AUTO_AGENT_SETUP_GH_LOGIN"; return 5; }

    _setup_secret SETUP_GH_TOKEN "${S_GH_TOKEN_FILE}" "${S_GH_TOKEN_ENV}" "${had_token}" \
        "Classic PAT for ${SETUP_GH_LOGIN} (repo, project, workflow)" \
        "PAT for ${SETUP_GH_LOGIN}: pass --gh-token-file or set AUTO_AGENT_SETUP_GH_TOKEN" || {
        _setup_line github FAIL "${SETUP_SECRET_WHY}"; return 5; }

    SETUP_SLUG="$(_setup_slug "${target}" "${S_REPO:-}")" || {
        _setup_line github FAIL "no repo: ${target} is not a checkout with a GitHub origin, and no --repo was given"; return 5; }

    local why; why="$(_setup_gh_verify "${SETUP_GH_TOKEN}" "${SETUP_GH_LOGIN}" "${SETUP_SLUG}")"
    if [ -n "${why}" ]; then
        _setup_line github FAIL "${why}"; return 5
    fi
    if [ "${SETUP_GH_LOGIN}" = "${had_login}" ] && [ "${SETUP_GH_TOKEN}" = "${had_token}" ]; then
        _setup_line github ok "${SETUP_GH_LOGIN} (classic PAT, admin on ${SETUP_SLUG})"
    else
        _setup_line github changed "${SETUP_GH_LOGIN} verified (classic PAT, admin on ${SETUP_SLUG}); the Host env will carry it"
    fi
}

# ------------------------------------------------------------------ claude

# _setup_claude_verify <mode> <token> : "" when `claude auth status` agrees
# with the declared mode, else the reason.
_setup_claude_verify() {
    local mode="$1" token="$2" status rc reason
    # The token is exported inside the subshell only, never put on a command line.
    status="$(unset ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN
        [ -z "${token}" ] || export CLAUDE_CODE_OAUTH_TOKEN="${token}"
        "${CLAUDE_BIN:-claude}" auth status --json 2>/dev/null)"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        case "${mode}" in
            login)
                if [ "${AUTO_AGENT_SETUP_ENTRY:-}" = "ssh" ]; then
                    printf 'claude is not logged in on this Host: run `claude auth login` on it over `ssh -t`, or run setup attended, which offers the login'
                else
                    printf 'claude is not logged in on this Host: run `claude auth login` here first (the in-VM entry point'"'"'s precondition)'
                fi ;;
            *) printf 'claude auth status refused the setup-token token (exit %s)' "${rc}" ;;
        esac
        return 0
    fi
    reason="$(unset ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN
        [ -z "${token}" ] || export CLAUDE_CODE_OAUTH_TOKEN="${token}"
        CLAUDE_AUTH_MODE="${mode}" _usage_mode_check "${status}" 0)"
    printf '%s' "${reason}"
}

# setup_stage_claude : sets SETUP_AUTH_MODE, SETUP_CLAUDE_TOKEN
setup_stage_claude() {
    local had_mode had_token
    had_mode="$(_setup_existing CLAUDE_AUTH_MODE)" || had_mode=""
    had_token="$(_setup_existing CLAUDE_CODE_OAUTH_TOKEN)" || had_token=""
    SETUP_AUTH_MODE="${S_AUTH_MODE:-${AUTO_AGENT_SETUP_AUTH_MODE:-${had_mode:-login}}}"
    SETUP_CLAUDE_TOKEN=""
    case "${SETUP_AUTH_MODE}" in
        login) ;;
        setup-token)
            _setup_secret SETUP_CLAUDE_TOKEN "${S_CLAUDE_TOKEN_FILE}" "${S_CLAUDE_TOKEN_ENV}" "${had_token}" \
                "claude setup-token token" \
                "setup-token token: pass --claude-token-file or set AUTO_AGENT_SETUP_CLAUDE_TOKEN" || {
                _setup_line claude FAIL "${SETUP_SECRET_WHY}"; return 6; } ;;
        api-key)
            _setup_line claude FAIL "the api-key auth mode refuses to start until spend pacing exists (ADR 0008); use login or setup-token"
            return 6 ;;
        *)
            _setup_line claude FAIL "unknown auth mode '${SETUP_AUTH_MODE}' (login or setup-token)"
            return 6 ;;
    esac
    local why; why="$(_setup_claude_verify "${SETUP_AUTH_MODE}" "${SETUP_CLAUDE_TOKEN}")"
    if [ -n "${why}" ]; then
        _setup_line claude FAIL "${why}"; return 6
    fi
    if [ "${SETUP_AUTH_MODE}" = "${had_mode}" ] && [ "${SETUP_CLAUDE_TOKEN}" = "${had_token}" ]; then
        _setup_line claude ok "claude auth status agrees with CLAUDE_AUTH_MODE=${SETUP_AUTH_MODE}"
    else
        _setup_line claude changed "CLAUDE_AUTH_MODE=${SETUP_AUTH_MODE} verified by claude auth status; the Host env will carry it"
    fi
}

# ------------------------------------------------------------------ config

# _setup_git <target> <args...> : git in the checkout, as the machine user
_setup_git() {
    local target="$1"; shift
    GH_TOKEN="${SETUP_GH_TOKEN}" "${GIT_BIN:-git}" -C "${target}" "$@"
}

# _setup_skeleton : the minimal Harness config a Target Project starts from
_setup_skeleton() {
    jq -n '{
        commit_scopes: ["core"],
        commands: { install: "true", test: "true" },
        pick: { labels: {} }
    }'
}

# _setup_git_identity <target> : the checkout commits and pushes as the
# machine user (ADR 0005); 0 when it already did, 10 when it changed.
_setup_git_identity() {
    local target="$1" email changed=0 key want have
    email="${SETUP_GH_LOGIN}@users.noreply.github.com"
    for key in user.name user.email credential.https://github.com.helper; do
        case "${key}" in
            user.name) want="${SETUP_GH_LOGIN}" ;;
            user.email) want="${email}" ;;
            *) want='!gh auth git-credential' ;;
        esac
        have="$("${GIT_BIN:-git}" -C "${target}" config --local --get "${key}" 2>/dev/null)" || have=""
        if [ "${have}" != "${want}" ]; then
            if [ "${key}" = "credential.https://github.com.helper" ]; then
                # An empty helper first resets any inherited one for github.com.
                "${GIT_BIN:-git}" -C "${target}" config --local --unset-all "${key}" 2>/dev/null
                "${GIT_BIN:-git}" -C "${target}" config --local --add "${key}" "" || return 1
                "${GIT_BIN:-git}" -C "${target}" config --local --add "${key}" "${want}" || return 1
            else
                "${GIT_BIN:-git}" -C "${target}" config --local "${key}" "${want}" || return 1
            fi
            changed=1
        fi
    done
    [ "${changed}" -eq 1 ] && return 10
    return 0
}

# setup_stage_config <target> : sets SETUP_CONFIG_FILE (the harness.json the
# configure step derives base needs from) and SETUP_CONFIG_STATE
# (present | pending)
setup_stage_config() {
    local target="$1" gh="${GH_BIN:-gh}" notes=() changed=0
    local cfg_rel="${HARNESS_CONFIG_DIRNAME}/${HARNESS_CONFIG_FILENAME}"

    if [ ! -d "${target}" ]; then
        if [ -z "${S_REPO:-}" ]; then
            _setup_line config FAIL "${target} does not exist; pass --repo <owner/name> to clone it there"; return 7
        fi
        mkdir -p "$(dirname "${target}")" || { _setup_line config FAIL "cannot create $(dirname "${target}")"; return 7; }
        if ! GH_TOKEN="${SETUP_GH_TOKEN}" "${gh}" repo clone "${SETUP_SLUG}" "${target}" >/dev/null 2>&1; then
            _setup_line config FAIL "gh repo clone ${SETUP_SLUG} ${target} failed"; return 7
        fi
        notes+=("cloned ${SETUP_SLUG} to ${target}"); changed=1
    fi

    _setup_git_identity "${target}"
    case $? in
        0) ;;
        10) notes+=("the checkout commits as ${SETUP_GH_LOGIN}"); changed=1 ;;
        *) _setup_line config FAIL "cannot set the checkout's git identity"; return 7 ;;
    esac

    if [ -f "${target}/${cfg_rel}" ]; then
        if ! harness_config_check "${target}" >/dev/null 2>"${SETUP_TMP}/config.err"; then
            _setup_line config FAIL "${cfg_rel} does not validate: $(head -3 "${SETUP_TMP}/config.err" | tr '\n' ' ')"; return 7
        fi
        SETUP_CONFIG_FILE="${target}/${cfg_rel}"; SETUP_CONFIG_STATE="present"
        notes+=("${cfg_rel} present")
        _setup_config_line "${changed}" "${notes[@]}"
        return 0
    fi

    # No Harness config yet: propose one as a machine-user PR (ADR 0009). The
    # Daemon is still enabled; its preflight fails closed on the default
    # branch until the PR merges.
    local branch="${HARNESS_BRANCH_SETUP_CONFIG}" draft="${SETUP_TMP}/harness.json" existing
    if [ -n "${S_CONFIG:-}" ]; then
        cp "${S_CONFIG}" "${draft}" 2>/dev/null || { _setup_line config FAIL "cannot read the draft ${S_CONFIG}"; return 7; }
    else
        _setup_skeleton > "${draft}"
    fi
    if ! harness_config_validate "${draft}" 2>"${SETUP_TMP}/config.err"; then
        _setup_line config FAIL "the draft Harness config does not validate: $(head -3 "${SETUP_TMP}/config.err" | tr '\n' ' ')"; return 7
    fi
    SETUP_CONFIG_FILE="${draft}"; SETUP_CONFIG_STATE="pending"

    existing="$(GH_TOKEN="${SETUP_GH_TOKEN}" "${gh}" pr list --repo "${SETUP_SLUG}" --head "${branch}" --state open \
        --json number --jq '.[0].number // empty' 2>/dev/null)" || {
        _setup_line config FAIL "gh could not list ${SETUP_SLUG}'s open PRs"; return 7; }
    if [ -n "${existing}" ]; then
        # The PR head holds what the configure step should derive needs from.
        if _setup_git "${target}" fetch --quiet origin "${branch}" >/dev/null 2>&1 \
            && _setup_git "${target}" show "FETCH_HEAD:${cfg_rel}" > "${draft}.head" 2>/dev/null \
            && harness_config_validate "${draft}.head" 2>/dev/null; then
            mv "${draft}.head" "${draft}"
        fi
        notes+=("waiting for Harness config: PR #${existing} is open")
        _setup_config_line "${changed}" "${notes[@]}"
        return 0
    fi

    if [ "${SETUP_MODE}" = "upgrade" ]; then
        notes+=("waiting for Harness config: none yet and no PR open (setup proposes one, upgrade never does)")
        _setup_config_line "${changed}" "${notes[@]}"
        return 0
    fi

    local body="${SETUP_TMP}/pr-body.md"
    if [ -n "${S_CONFIG_PR_BODY:-}" ]; then
        cp "${S_CONFIG_PR_BODY}" "${body}" 2>/dev/null && [ -s "${body}" ] || {
            _setup_line config FAIL "cannot read the PR body ${S_CONFIG_PR_BODY}"; return 7; }
    else
        _setup_config_pr_body > "${body}"
    fi
    local base; base="$(_harness_config_default_branch "${SETUP_SLUG}")" || {
        _setup_line config FAIL "cannot detect ${SETUP_SLUG}'s default branch"; return 7; }
    local wt="${SETUP_TMP}/config-worktree"
    if ! { _setup_git "${target}" fetch --quiet origin "${base}" \
        && _setup_git "${target}" worktree add --quiet -B "${branch}" "${wt}" FETCH_HEAD; } >/dev/null 2>&1; then
        _setup_line config FAIL "cannot branch ${branch} from origin/${base}"; return 7
    fi
    mkdir -p "${wt}/${HARNESS_CONFIG_DIRNAME}" && cp "${draft}" "${wt}/${cfg_rel}"
    local pushed=1
    if _setup_git "${wt}" add "${cfg_rel}" >/dev/null 2>&1 \
        && _setup_git "${wt}" -c "user.name=${SETUP_GH_LOGIN}" -c "user.email=${SETUP_GH_LOGIN}@users.noreply.github.com" \
            commit --quiet -m "chore: adopt the auto-agent harness" >/dev/null 2>&1 \
        && _setup_git "${wt}" push --quiet --force origin "${branch}" >/dev/null 2>&1; then
        pushed=0
    fi
    _setup_git "${target}" worktree remove --force "${wt}" >/dev/null 2>&1
    _setup_git "${target}" branch -D "${branch}" >/dev/null 2>&1
    if [ "${pushed}" -ne 0 ]; then
        _setup_line config FAIL "cannot commit and push ${branch} as ${SETUP_GH_LOGIN}"; return 7
    fi
    local url
    url="$(GH_TOKEN="${SETUP_GH_TOKEN}" "${gh}" pr create --repo "${SETUP_SLUG}" --base "${base}" --head "${branch}" \
        --title "chore: adopt the auto-agent harness" --body-file "${body}" 2>/dev/null)" || {
        _setup_line config FAIL "gh pr create failed for ${branch}"; return 7; }
    notes+=("proposed ${cfg_rel} in ${url##*/} (${url}); the Daemon waits for it to merge")
    _setup_config_line 1 "${notes[@]}"
}

_setup_config_line() {
    local changed="$1"; shift
    local detail; detail="$(IFS=';'; printf '%s' "$*")"; detail="${detail//;/; }"
    if [ "${changed}" -eq 1 ]; then _setup_line config changed "${detail}"
    else _setup_line config ok "${detail}"; fi
}

_setup_config_pr_body() {
    cat <<'EOF'
Adopts the auto-agent harness: one `.auto-agent/harness.json` (ADR 0002), opened by the machine user during Setup.

The Daemon is already enabled on its Host. Until this merges, every Fire's preflight fails closed on the default branch and the Dashboard shows "waiting for Harness config".

Before merging, check:

- [ ] `commit_scopes` are the scopes this repo's commits use
- [ ] `commands.install` and `commands.test` are this repo's real commands (the skeleton's are `true`)
- [ ] `pick` is the pick signal you want (label-only, or a Project with a Priority field)
- [ ] `surfaces` name what the verifier can drive; without `verification.hermetic` the Daemon asks itself to write an Environment provider (the bootstrap issue)

The schema is `plugin/schema/harness.schema.json` in the auto-agent repo.
EOF
}

# --------------------------------------------------------------- configure

# setup_base_needs <harness.json> : the base Host needs the config implies
# (ADR 0004): a browser Surface implies a display plus Chrome, an electron
# Surface a display plus the Electron runtime, host.docker is explicit.
setup_base_needs() {
    jq -c '{
        browser: ([.surfaces // {} | .[] | select(.kind == "browser")] | length > 0),
        electron: ([.surfaces // {} | .[] | select(.kind == "electron")] | length > 0),
        docker: (.host.docker // false)
    } | .display = (.browser or .electron)' "$1"
}

# setup_electron_profiles <harness.json> <target> : the AppArmor grants the
# electron Surfaces need (ticket #19): each launcher, and the Electron binary
# they start (AUTO_AGENT_ELECTRON_BINARY, else any node_modules copy under the
# checkout); each path once, however many Surfaces share it.
setup_electron_profiles() {
    local bin="${AUTO_AGENT_ELECTRON_BINARY:-${2}/**/node_modules/electron/dist/electron}"
    TARGET="$2" BIN="${bin}" jq -c '[.surfaces // {} | .[] | select(.kind == "electron")] as $e | {
        launchers: ([$e[] | .launcher | select(. != null)
                     | if startswith("/") then . else env.TARGET + "/" + . end] | unique),
        binaries: (if ($e | length) > 0 then [env.BIN] else [] end) }' "$1"
}

# _setup_host_env_render <needs-json> : the Host env this Setup writes. Keeps
# every line it does not manage; rewrites the keys it does, in a fixed order,
# so an unchanged Host renders byte-identical and the Ansible copy is "ok".
_setup_host_env_render() {
    local needs="$1" file; file="$(host_env_file)"
    local -a keys=() vals=() drop=()
    _hk() { keys+=("$1"); vals+=("$2"); }
    local existing_display; existing_display="$(_setup_existing DISPLAY)" || existing_display=""

    _hk GH_TOKEN "${SETUP_GH_TOKEN}"
    _hk DAEMON_GH_LOGIN "${SETUP_GH_LOGIN}"
    _hk CLAUDE_AUTH_MODE "${SETUP_AUTH_MODE}"
    if [ "${SETUP_AUTH_MODE}" = "setup-token" ]; then
        _hk CLAUDE_CODE_OAUTH_TOKEN "${SETUP_CLAUDE_TOKEN}"
    else
        drop+=(CLAUDE_CODE_OAUTH_TOKEN)
    fi
    _hk AUTO_AGENT_TARGET_DIR "${SETUP_TARGET}"
    _hk AUTO_AGENT_STATE_DIR "${SETUP_STATE_DIR}"
    _hk AUTO_AGENT_HOST_USER "${SETUP_HOST_USER}"
    if [ "$(printf '%s' "${needs}" | jq -r .display)" = "true" ]; then
        _hk DISPLAY "${existing_display:-${SETUP_DISPLAY_DEFAULT}}"
    fi
    # The Dashboard's keys, written with their defaults only when absent.
    local k d
    for k in AUTO_AGENT_DASHBOARD_BIND=127.0.0.1 AUTO_AGENT_DASHBOARD_PORT=8090 \
             AUTO_AGENT_DASHBOARD_SUMMARY=on AUTO_AGENT_DASHBOARD_SUMMARY_MODEL=haiku; do
        d="$(_setup_existing "${k%%=*}")" || d="${k#*=}"
        _hk "${k%%=*}" "${d}"
    done
    # --set wins over everything above.
    local kv i found
    for kv in "${S_SETS[@]+"${S_SETS[@]}"}"; do
        found=0
        for i in "${!keys[@]}"; do
            if [ "${keys[$i]}" = "${kv%%=*}" ]; then vals[i]="${kv#*=}"; found=1; fi
        done
        [ "${found}" -eq 1 ] || _hk "${kv%%=*}" "${kv#*=}"
    done

    # Every line already in the file stays where it is: a managed key is
    # rewritten in place, a dropped one removed, anything else kept as is.
    # Managed keys the file lacks are appended, so a second run over the same
    # answers renders the same bytes.
    local line key emitted=()
    if [ -f "${file}" ]; then
        while IFS= read -r line || [ -n "${line}" ]; do
            key="${line#"${line%%[![:space:]]*}"}"; key="${key#export }"; key="${key%%=*}"
            case "${line}" in \#*|'') printf '%s\n' "${line}"; continue ;; esac
            for i in "${drop[@]+"${drop[@]}"}"; do [ "${key}" = "${i}" ] && continue 2; done
            for i in "${!keys[@]}"; do
                if [ "${key}" = "${keys[$i]}" ]; then
                    [ -z "${emitted[$i]:-}" ] && printf '%s=%s\n' "${keys[$i]}" "${vals[$i]}"
                    emitted[i]=1
                    continue 2
                fi
            done
            printf '%s\n' "${line}"
        done < "${file}"
    else
        printf '%s\n' "${SETUP_HOST_ENV_HEADER}"
    fi
    for i in "${!keys[@]}"; do
        [ -n "${emitted[$i]:-}" ] || printf '%s=%s\n' "${keys[$i]}" "${vals[$i]}"
    done
}
SETUP_HOST_ENV_HEADER="# auto-agent Host env (ADR 0005): 0600, the Daemon's and the Dashboard's EnvironmentFile. Setup rewrites the keys it manages in place and keeps every other line."

setup_stage_configure() {
    local needs profiles vars="${SETUP_TMP}/configure-vars.json" log rc changed
    needs="$(setup_base_needs "${SETUP_CONFIG_FILE}")" || { _setup_line configure FAIL "cannot read ${SETUP_CONFIG_FILE}"; return 8; }
    profiles="$(setup_electron_profiles "${SETUP_CONFIG_FILE}" "${SETUP_TARGET}")" || profiles='{"launchers":[],"binaries":[]}'
    local content; content="$(_setup_host_env_render "${needs}")" || { _setup_line configure FAIL "cannot render the Host env"; return 8; }
    case "${content}" in *$'\r'*) _setup_line configure FAIL "a Host env value holds a carriage return"; return 8 ;; esac
    local display; display="$(printf '%s\n' "${content}" | sed -n 's/^DISPLAY=//p' | tail -1)"

    # The vars file holds every secret: 0600 inside the 0700 scratch dir, read
    # through the environment so no secret reaches jq's argv, and deleted with
    # the scratch dir when setup exits.
    ( umask 077
      AA_CONTENT="${content}" SETUP_HOST_USER="${SETUP_HOST_USER}" SETUP_TARGET="${SETUP_TARGET}" \
      SETUP_STATE_DIR="${SETUP_STATE_DIR}" SETUP_HOST_ENV="${SETUP_HOST_ENV}" AUTO_AGENT_ROOT="${AUTO_AGENT_ROOT}" \
      AA_NEEDS="${needs}" AA_PROFILES="${profiles}" AA_DISPLAY="${display:-${SETUP_DISPLAY_DEFAULT}}" \
      jq -n '{
          aa_host_user: env.SETUP_HOST_USER, aa_install_dir: env.AUTO_AGENT_ROOT,
          aa_target_dir: env.SETUP_TARGET, aa_state_dir: env.SETUP_STATE_DIR,
          aa_host_env_path: env.SETUP_HOST_ENV, aa_host_env_content: (env.AA_CONTENT + "\n"),
          aa_needs: (env.AA_NEEDS | fromjson), aa_display: env.AA_DISPLAY,
          aa_electron: (env.AA_PROFILES | fromjson)
      }' > "${vars}" ) || { _setup_line configure FAIL "cannot write the configure vars"; return 8; }

    mkdir -p "${SETUP_STATE_DIR}/setup" && chmod 700 "${SETUP_STATE_DIR}" 2>/dev/null
    log="${SETUP_STATE_DIR}/setup/configure-$(date -u +%Y%m%dT%H%M%SZ).log"
    ANSIBLE_CONFIG="${SETUP_ANSIBLE_DIR}/ansible.cfg" \
        "${ANSIBLE_PLAYBOOK_BIN:-ansible-playbook}" -i localhost, -c local \
        -e "@${vars}" "${SETUP_ANSIBLE_DIR}/configure.yml" > "${log}" 2>&1
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        tail -20 "${log}" >&2
        _setup_line configure FAIL "ansible-playbook exited ${rc} (log: ${log})"; return 8
    fi
    changed="$(awk '/^PLAY RECAP/ { r = 1; next } r && /changed=/ { for (i = 1; i <= NF; i++) if ($i ~ /^changed=/) { split($i, a, "="); s += a[2] } } END { print s + 0 }' "${log}")"
    local what; what="$(printf '%s' "${needs}" | jq -r '[to_entries[] | select(.value) | .key] | if length == 0 then "no display, no Docker" else join(", ") end')"
    if [ "${changed}" -gt 0 ]; then
        SETUP_CONFIGURE_CHANGED=1
        _setup_line configure changed "${changed} task(s) changed; base needs: ${what} (log: ${log})"
    else
        _setup_line configure ok "converged; base needs: ${what}"
    fi
}

# --------------------------------------------------------------- extension

setup_stage_extension() {
    local ext="${SETUP_TARGET}/${HARNESS_CONFIG_DIRNAME}/${HARNESS_HOST_EXTENSION_FILENAME}" rc
    if [ ! -e "${ext}" ]; then
        _setup_line extension skipped "the Target Project has no ${HARNESS_CONFIG_DIRNAME}/${HARNESS_HOST_EXTENSION_FILENAME}"
        return 0
    fi
    [ -x "${ext}" ] || { _setup_line extension FAIL "${ext} is not executable"; return 9; }
    # No secret reaches the extension: it adds Host needs, never identity. Its
    # one argument says which run this is: setup or upgrade.
    ( cd "${SETUP_TARGET}" && env -u GH_TOKEN -u CLAUDE_CODE_OAUTH_TOKEN -u ANTHROPIC_API_KEY \
        AUTO_AGENT_ROOT="${AUTO_AGENT_ROOT}" AUTO_AGENT_TARGET_DIR="${SETUP_TARGET}" \
        AUTO_AGENT_HOST_ENV="${SETUP_HOST_ENV}" "${ext}" "${SETUP_MODE}" ) 2>&1 | sed 's/^/setup: extension: | /'
    rc="${PIPESTATUS[0]}"
    if [ "${rc}" -ne 0 ]; then
        _setup_line extension FAIL "${HARNESS_HOST_EXTENSION_FILENAME} exited ${rc}; it must be idempotent and exit 0"; return 9
    fi
    _setup_line extension ok "${HARNESS_HOST_EXTENSION_FILENAME} ran (exit 0)"
}

# ------------------------------------------------------------------ verify

# _check_line <item> <status> <detail>
_check_line() {
    printf 'check: %s: %s — %s\n' "$1" "$2" "$3"
    [ "$2" = "FAIL" ] && CHECK_FAILED+=("$1")
    return 0
}

# setup_check <target> : the verify stage. Reads the Host env the way the
# Daemon's unit does (in this subshell only) and writes nothing.
setup_check() (
    local target="$1" file mode k missing=() cfg_present=0
    CHECK_FAILED=()
    file="$(host_env_file)"
    if [ ! -f "${file}" ]; then
        _check_line host-env FAIL "no Host env at ${file}"
        return 10
    fi
    # The file's own values, not whatever the calling shell exported: every
    # key the file names is dropped first, then loaded from it.
    local line
    while IFS= read -r line || [ -n "${line}" ]; do
        line="${line#"${line%%[![:space:]]*}"}"; line="${line#export }"
        case "${line}" in [A-Za-z_]*=*) unset "${line%%=*}" 2>/dev/null ;; esac
    done < "${file}"
    unset GH_TOKEN DAEMON_GH_LOGIN CLAUDE_AUTH_MODE CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_API_KEY HARNESS_CONFIG_JSON
    host_env_load
    mode="$(stat -c %a "${file}" 2>/dev/null)"
    for k in GH_TOKEN DAEMON_GH_LOGIN CLAUDE_AUTH_MODE AUTO_AGENT_TARGET_DIR; do
        [ -n "${!k:-}" ] || missing+=("${k}")
    done
    [ "${CLAUDE_AUTH_MODE:-}" = "setup-token" ] && [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] && missing+=(CLAUDE_CODE_OAUTH_TOKEN)
    if [ "${mode}" != "600" ]; then
        _check_line host-env FAIL "${file} is mode ${mode}; it holds every secret and must be 0600"
    elif [ "${#missing[@]}" -gt 0 ]; then
        _check_line host-env FAIL "${file} lacks ${missing[*]}"
    else
        _check_line host-env ok "${file} is 0600 and holds every secret"
    fi
    target="${target:-${AUTO_AGENT_TARGET_DIR:-}}"
    [ -n "${target}" ] || { _check_line target FAIL "no Target Project"; return 10; }

    local slug why
    if slug="$(_harness_config_repo_slug "${target}")"; then
        why="$(_setup_gh_verify "${GH_TOKEN:-}" "${DAEMON_GH_LOGIN:-}" "${slug}")"
        if [ -n "${why}" ]; then _check_line github FAIL "${why}"
        else _check_line github ok "${DAEMON_GH_LOGIN} is admin on ${slug} with a classic PAT"; fi
    else
        _check_line github FAIL "${target} has no GitHub origin remote"
    fi

    why="$(_setup_claude_verify "${CLAUDE_AUTH_MODE:-}" "${CLAUDE_CODE_OAUTH_TOKEN:-}")"
    if [ -n "${why}" ]; then _check_line claude FAIL "${why}"
    else _check_line claude ok "claude auth status agrees with CLAUDE_AUTH_MODE=${CLAUDE_AUTH_MODE}"; fi

    local cfg_rel="${HARNESS_CONFIG_DIRNAME}/${HARNESS_CONFIG_FILENAME}" err
    if [ ! -f "${target}/${cfg_rel}" ]; then
        _check_line config skipped "waiting for Harness config: ${target} has no ${cfg_rel} yet"
    elif err="$(harness_config_check "${target}" 2>&1 >/dev/null)"; then
        _check_line config ok "${cfg_rel} matches the schema"; cfg_present=1
    else
        _check_line config FAIL "$(printf '%s' "${err}" | head -3 | tr '\n' ' ')"
    fi

    if [ "${cfg_present}" -eq 1 ]; then
        local out rc
        out="$(bash -c "${SETUP_PROVIDER_CHECK_CMD:-\"${SETUP_CLI}\" provider-check}"' "$@"' _ "${target}" 2>&1)"; rc=$?
        case "${rc}" in
            0) _check_line provider ok "$(printf '%s\n' "${out}" | grep -E '^provider-check:' | tail -1)" ;;
            3) _check_line provider skipped "no hermetic tier yet (the Bootstrap state)" ;;
            *) _check_line provider FAIL "provider-check exited ${rc}: $(printf '%s\n' "${out}" | grep -E '^provider-check:' | tail -1)" ;;
        esac
        out="$(bash -c "${SETUP_FIRE_CMD:-\"${SETUP_CLI}\" fire --dry-run}"' "$@"' _ "${target}" 2>&1)"; rc=$?
        local verdict; verdict="$(printf '%s\n' "${out}" | grep -Eo 'afk-pickup: (would-[a-z-]+|no eligible issue).*' | tail -1)"
        if [ "${rc}" -eq 0 ]; then _check_line fire ok "dry-run Fire green: ${verdict:-exit 0}"
        else
            printf '%s\n' "${out}" | tail -15 >&2
            _check_line fire FAIL "dry-run Fire exited ${rc}${verdict:+ (${verdict})}"
        fi
    else
        _check_line provider skipped "no Harness config to read a hermetic tier from"
        _check_line fire skipped "waiting for Harness config: a Fire's preflight fails closed without it"
    fi

    [ "${#CHECK_FAILED[@]}" -eq 0 ] || return 10
    return 0
)

setup_stage_verify() {
    local out rc
    out="$(setup_check "${SETUP_TARGET}")"; rc=$?
    printf '%s\n' "${out}"
    if [ "${rc}" -ne 0 ]; then
        _setup_line verify FAIL "$(printf '%s\n' "${out}" | grep -c ': FAIL —') check(s) failed; the Daemon is not enabled"
        return 10
    fi
    _setup_line verify ok "$(printf '%s\n' "${out}" | grep -c ': ok —') check(s) ok, $(printf '%s\n' "${out}" | grep -c ': skipped —') skipped"
}

# ------------------------------------------------------------------ enable

setup_stage_enable() {
    local u did=() enabled active
    for u in ${SETUP_UNITS}; do
        enabled="$(_setup_systemctl is-enabled "${u}" 2>/dev/null)"
        active="$(_setup_systemctl is-active "${u}" 2>/dev/null)"
        if [ "${enabled}" != "enabled" ] || [ "${active}" != "active" ]; then
            _setup_systemctl enable --now "${u}" >/dev/null 2>&1 || {
                _setup_line enable FAIL "systemctl enable --now ${u} failed"; return 11; }
            did+=("started ${u}")
        elif [ "${SETUP_CONFIGURE_CHANGED:-0}" = "1" ] || [ "${AUTO_AGENT_SETUP_INSTALL_MOVED:-0}" = "1" ]; then
            _setup_systemctl restart "${u}" >/dev/null 2>&1 || {
                _setup_line enable FAIL "systemctl restart ${u} failed"; return 11; }
            did+=("restarted ${u}")
        fi
    done
    for u in ${SETUP_UNITS}; do
        active="$(_setup_systemctl is-active "${u}" 2>/dev/null)"
        [ "${active}" = "active" ] || { _setup_line enable FAIL "${u} is ${active:-not active} after start (journalctl -u ${u})"; return 11; }
    done
    if [ "${#did[@]}" -gt 0 ]; then
        _setup_line enable changed "$(IFS=,; printf '%s' "${did[*]}" | sed 's/,/, /g')"
    else
        _setup_line enable ok "both units enabled and active"
    fi
}

# upgrade's last stage: the Daemon and Dashboard pick up the moved install
setup_stage_restart() {
    local u enabled
    for u in ${SETUP_UNITS}; do
        enabled="$(_setup_systemctl is-enabled "${u}" 2>/dev/null)"
        [ "${enabled}" = "enabled" ] || { _setup_line restart FAIL "${u} is not enabled: run setup, not upgrade"; return 11; }
        _setup_systemctl restart "${u}" >/dev/null 2>&1 || { _setup_line restart FAIL "systemctl restart ${u} failed"; return 11; }
    done
    for u in ${SETUP_UNITS}; do
        [ "$(_setup_systemctl is-active "${u}" 2>/dev/null)" = "active" ] || {
            _setup_line restart FAIL "${u} is not active after the restart (journalctl -u ${u})"; return 11; }
    done
    _setup_line restart changed "restarted $(printf '%s' "${SETUP_UNITS}" | sed 's/ /, /g')"
}

# --------------------------------------------------------------- bootstrap

setup_stage_bootstrap() {
    if [ "${SETUP_CONFIG_STATE}" != "present" ]; then
        _setup_line bootstrap skipped "waiting for Harness config; the first Fire after it merges opens the bootstrap issue if it is owed"
        return 0
    fi
    local out rc
    out="$(GH_TOKEN="${SETUP_GH_TOKEN}" AUTO_AGENT_HOST_ENV="${SETUP_HOST_ENV}" \
        bash -c "${SETUP_BOOTSTRAP_CMD:-\"${SETUP_CLI}\" bootstrap issue}"' "$@"' _ "${SETUP_TARGET}" 2>&1)"; rc=$?
    local line; line="$(printf '%s\n' "${out}" | grep -E '^bootstrap:' | tail -1)"
    if [ "${rc}" -ne 0 ]; then
        _setup_line bootstrap FAIL "bootstrap issue exited ${rc}: ${line:-$(printf '%s' "${out}" | tail -1)}"; return 12
    fi
    case "${line}" in
        *created*) _setup_line bootstrap changed "${line#bootstrap: }" ;;
        *) _setup_line bootstrap ok "${line#bootstrap: }" ;;
    esac
}

# ----------------------------------------------------------------- summary

setup_stage_summary() {
    local r bind port
    echo
    echo "setup: summary"
    for r in "${SETUP_RESULTS[@]}"; do echo "  ${r}"; done
    bind="$(_setup_existing AUTO_AGENT_DASHBOARD_BIND)" || bind="127.0.0.1"
    port="$(_setup_existing AUTO_AGENT_DASHBOARD_PORT)" || port="8090"
    echo "  Dashboard: http://${bind}:${port}/ (loopback unless AUTO_AGENT_DASHBOARD_BIND says otherwise)"
    if [ "${#SETUP_CHANGED[@]}" -eq 0 ]; then
        echo "setup: converged — nothing changed"
    else
        echo "setup: done — ${#SETUP_CHANGED[@]} changed: ${SETUP_CHANGED[*]}"
    fi
}

# -------------------------------------------------------------------- main

_setup_usage() { sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

# setup_valid_set <KEY=VALUE> : 0 when --set may write it, else says why
setup_valid_set() {
    case "$1" in
        [A-Za-z_]*=*) ;;
        *) _setup_err "--set needs KEY=VALUE"; return 1 ;;
    esac
    case "${1%%=*}" in *[!A-Za-z0-9_]*) _setup_err "--set: invalid key '${1%%=*}'"; return 1 ;; esac
    case "$1" in *$'\n'*) _setup_err "--set: a value cannot hold a newline"; return 1 ;; esac
}

# setup_run <args> : the whole of setup, or with SETUP_MODE=upgrade its subset
setup_run() {
    SETUP_MODE="${SETUP_MODE:-setup}"
    _setup_handoff_consume
    S_REPO="${AUTO_AGENT_SETUP_REPO:-}"; S_GH_LOGIN=""; S_GH_TOKEN_FILE=""; S_AUTH_MODE=""
    S_CLAUDE_TOKEN_FILE=""; S_CONFIG="${AUTO_AGENT_SETUP_CONFIG:-}"
    S_CONFIG_PR_BODY="${AUTO_AGENT_SETUP_CONFIG_PR_BODY:-}"; S_ROTATE="${AUTO_AGENT_SETUP_ROTATE:-0}"
    S_SETS=()
    # The two secret overrides are read once and taken out of the environment,
    # so no child (Ansible, the Host extension, the Fire) ever inherits them.
    S_GH_TOKEN_ENV="${AUTO_AGENT_SETUP_GH_TOKEN:-}"; S_CLAUDE_TOKEN_ENV="${AUTO_AGENT_SETUP_CLAUDE_TOKEN:-}"
    unset AUTO_AGENT_SETUP_GH_TOKEN AUTO_AGENT_SETUP_CLAUDE_TOKEN
    local target="${AUTO_AGENT_SETUP_TARGET:-}"
    while [ $# -gt 0 ]; do
        case "$1" in
            --repo) S_REPO="${2:-}"; shift ;;
            --gh-login) S_GH_LOGIN="${2:-}"; shift ;;
            --gh-token-file) S_GH_TOKEN_FILE="${2:-}"; shift ;;
            --auth-mode) S_AUTH_MODE="${2:-}"; shift ;;
            --claude-token-file) S_CLAUDE_TOKEN_FILE="${2:-}"; shift ;;
            --config) S_CONFIG="${2:-}"; shift ;;
            --config-pr-body) S_CONFIG_PR_BODY="${2:-}"; shift ;;
            --set) setup_valid_set "${2:-}" || return 2; S_SETS+=("$2"); shift ;;
            --rotate) S_ROTATE=1 ;;
            --unattended) AUTO_AGENT_SETUP_UNATTENDED=1 ;;
            -h|--help) _setup_usage; return 0 ;;
            -*) _setup_err "unknown option '$1'"; return 2 ;;
            *) target="$1" ;;
        esac
        shift
    done
    [ -n "${target}" ] || target="$(_setup_existing AUTO_AGENT_TARGET_DIR)" || target=""
    if [ -z "${target}" ]; then
        _setup_err "name the Target Project checkout (setup <target-dir>, or AUTO_AGENT_SETUP_TARGET)"; return 2
    fi
    SETUP_TARGET="$(_setup_abs "${target}")"
    SETUP_HOST_ENV="$(host_env_file)"
    if [ "${SETUP_MODE}" = "upgrade" ]; then
        [ -f "${SETUP_HOST_ENV}" ] || { _setup_err "upgrade: no Host env at ${SETUP_HOST_ENV}: run setup first"; return 2; }
        # An upgrade reuses what the Host holds; it never asks.
        AUTO_AGENT_SETUP_UNATTENDED=1; S_ROTATE=0
    fi
    SETUP_HOST_USER="${AUTO_AGENT_HOST_USER:-$(id -un)}"
    SETUP_STATE_DIR="$(_setup_existing AUTO_AGENT_STATE_DIR)" || SETUP_STATE_DIR="$(host_env_state_dir)"
    SETUP_CONFIGURE_CHANGED=0
    SETUP_TMP="$(mktemp -d)" || return 1
    chmod 700 "${SETUP_TMP}"
    # shellcheck disable=SC2064
    trap "rm -rf '${SETUP_TMP}'" EXIT

    setup_stage_baseline "${SETUP_TARGET}" || return $?
    setup_stage_doctor || return $?
    setup_stage_github "${SETUP_TARGET}" || return $?
    setup_stage_claude || return $?
    setup_stage_config "${SETUP_TARGET}" || return $?
    setup_stage_configure || return $?
    setup_stage_extension || return $?
    if [ "${SETUP_MODE}" = "upgrade" ]; then
        setup_stage_restart || return $?
        setup_stage_summary
        return 0
    fi
    setup_stage_verify || return $?
    setup_stage_enable || return $?
    setup_stage_bootstrap || return $?
    setup_stage_summary
    return 0
}

# _setup_is_host_name <arg> : 0 when <arg> names a Host in the inventory
_setup_name_shaped() { case "$1" in ''|*/*|-*) return 1 ;; esac; }
_setup_is_host_name() { _setup_name_shaped "$1" && [ -f "$(setup_inventory_dir)/$1.env" ]; }

_setup_remote() { exec bash "${_setup_lib_dir}/setup-remote.sh" "$@"; }

# _setup_unknown_name <cmd> <arg> : 2 when <arg> reads as a Host name (no
# slash, not a directory here) that the inventory does not hold
_setup_unknown_name() {
    _setup_name_shaped "$2" || return 0
    [ -d "$2" ] && return 0
    _setup_err "$1: no Host named '$2' in $(setup_inventory_dir), and no directory $2 here"
    return 2
}

_setup_main() {
    local cmd="${1:-}" a
    shift || true
    case "${cmd}" in
        setup)
            for a in "$@"; do [ "${a}" = "--provision" ] && exec bash "${_setup_lib_dir}/setup-provision.sh" setup "$@"; done
            for a in "$@"; do [ "${a}" = "--host" ] && _setup_remote setup "$@"; done
            setup_run "$@" ;;
        upgrade)
            _setup_is_host_name "${1:-}" && _setup_remote upgrade "$@"
            _setup_unknown_name upgrade "${1:-}" || return 2
            SETUP_MODE=upgrade setup_run "$@" ;;
        check)
            _setup_is_host_name "${1:-}" && _setup_remote check "$@"
            _setup_unknown_name check "${1:-}" || return 2
            case "${1:-}" in -h|--help) _setup_usage; return 0 ;; -*) _setup_err "unknown option '$1'"; return 2 ;; esac
            local t="${1:-}"
            [ -z "${t}" ] || t="$(_setup_abs "${t}")"
            setup_check "${t}" ;;
        -h|--help|help|'') _setup_usage ;;
        *) _setup_err "unknown command '${cmd}' (setup, upgrade or check)"; return 2 ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    set -uo pipefail
    _setup_main "$@"
    exit $?
fi
