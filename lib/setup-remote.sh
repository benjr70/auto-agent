#!/usr/bin/env bash
# Setup's bring-your-own-VM entry point (ADR 0004, ADR 0009): Setup driven from
# the Operator machine over SSH, converging on the same engine and the same
# configure step as the in-VM entry point. `bin/auto-agent setup --host`,
# `upgrade <name>` and `check <name>` land here (lib/setup.sh routes them).
#
# How a run goes: the Operator machine asks for what Setup needs (secrets
# included), an Ansible play over SSH (infra/ansible/install.yml) readies the
# Host (distribution asserted, the engine's prerequisites, the Harness install
# at its pinned ref, the secrets as the setup handoff under no_log), then the
# in-VM engine runs on the Host over SSH and its stage lines stream back. The
# Operator machine keeps a non-secret Host inventory entry so `upgrade` and
# `check` can reach the Host again; everything else is read back from the
# Host over SSH.
#
# Usage:
#   setup-remote.sh setup --host <user@vm | name> [options] [<target-dir>]
#       <target-dir> is the Target Project checkout on the Host (relative to
#       the Host user's home when not absolute); the inventory's, else the
#       Host env's, on a re-run. Options, besides every in-VM setup option
#       (--repo --gh-login --gh-token-file --auth-mode --claude-token-file
#       --config --set --rotate --unattended, read here and handed on):
#         --name <name>           the inventory name (default: the host part)
#         --ref <ref>             AUTO_AGENT_SETUP_REF: the Harness install's
#                                 ref, a tag or SHA (or a branch, which floats);
#                                 default the inventory's, else the tag at this
#                                 clone's HEAD, else its SHA
#         --harness-repo <url>    AUTO_AGENT_SETUP_HARNESS_REPO: where the Host
#                                 clones the install from (default this clone's
#                                 origin, as https)
#         --install-dir <dir>     on the Host (default ~/auto-agent)
#         --ssh-port <port>       default 22
#         --ssh-identity <file>   the private key ssh and Ansible use (a path;
#                                 the key never leaves this machine)
#         --tailscale             join the Host to your tailnet (the install
#                                 play installs tailscale) and open the
#                                 Dashboard on all interfaces (ADR 0006);
#                                 remembered in the inventory
#         --tailscale-authkey-file <f>
#                                 AUTO_AGENT_SETUP_TAILSCALE_AUTHKEY (the
#                                 value): the auth key, asked for only while
#                                 the Host is not on the tailnet; implies
#                                 --tailscale
#
#   setup-remote.sh upgrade <name> [--ref <ref>] [--set KEY=VALUE]...
#       Moves the Harness install to <ref> (default: the inventory's, which
#       re-pulls a floating branch), re-runs configure and the Host extension,
#       restarts both units, and records the ref in the inventory.
#
#   setup-remote.sh check <name>
#       Reads the facts back over SSH: the install's ref and commit, then the
#       in-VM `check` (every `check:` line). Writes nothing anywhere.
#
# Output: this side's own lines share the engine's shape
# (`setup: <stage>: ok|changed|FAIL — ...`, `check: <item>: ...`): stages
# operator (this machine's prerequisites), ssh, tailscale (only when the
# auth key it needs is missing), install, claude-login, inventory; then the
# engine's own stages. The Proxmox entry point
# (lib/setup-provision.sh) runs its operator and provision stages first and
# then this, with R_OPERATOR_DONE=1.
#
# Exit codes: the engine's (see lib/setup.sh), plus 2 usage, 3 when the
# install play's distribution assertion fails, 4 operator prerequisites,
# 13 the Host cannot be reached over SSH, 14 the install play or the
# inventory write failed, or --tailscale has no auth key to join with; check 0 or 10, 2 for a name the inventory lacks.
#
# Secrets: prompted here (or read from --gh-token-file, --claude-token-file,
# AUTO_AGENT_SETUP_GH_TOKEN, AUTO_AGENT_SETUP_CLAUDE_TOKEN) only when the Host
# env lacks them or --rotate is given (the tailscale auth key only while the
# Host is off the tailnet); they travel in one 0600 Ansible vars
# file in a 0700 scratch dir, deleted when this exits, and reach the Host only
# through the play's no_log handoff task. Never argv, never the inventory.
#
# Env (test seams): SSH_BIN, ANSIBLE_PLAYBOOK_BIN, GIT_BIN,
# SETUP_OPERATOR_COMMANDS, AUTO_AGENT_INVENTORY_DIR.

_remote_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=setup.sh
. "${_remote_lib_dir}/setup.sh"

REMOTE_INSTALL_PLAY="${SETUP_ANSIBLE_DIR}/install.yml"
# What this machine needs (SETUP_OPERATOR_COMMANDS overrides).
REMOTE_OPERATOR_COMMANDS="ssh ansible-playbook jq git"
# The keys a Host inventory entry may carry: how to reach the Host, never a
# secret (ADR 0009).
REMOTE_INVENTORY_KEYS="AUTO_AGENT_HOST_NAME AUTO_AGENT_HOST_SSH AUTO_AGENT_HOST_SSH_PORT AUTO_AGENT_HOST_SSH_IDENTITY AUTO_AGENT_HOST_PROVISIONER AUTO_AGENT_HOST_TAILSCALE AUTO_AGENT_HARNESS_REPO AUTO_AGENT_HARNESS_REF AUTO_AGENT_INSTALL_DIR AUTO_AGENT_TARGET_DIR AUTO_AGENT_TARGET_REPO AUTO_AGENT_HOST_GH_LOGIN AUTO_AGENT_HOST_AUTH_MODE"

_remote_err() { echo "setup: $*" >&2; }

# ------------------------------------------------------------ the Operator

_remote_operator_doctor() {
    [ "${R_OPERATOR_DONE:-0}" = "1" ] && return 0
    local c missing=()
    for c in ${SETUP_OPERATOR_COMMANDS:-${REMOTE_OPERATOR_COMMANDS}}; do
        command -v "${c}" >/dev/null 2>&1 || missing+=("${c}")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        for c in "${missing[@]}"; do
            case "${c}" in
                ansible-playbook) echo "setup: operator: missing ansible-playbook — install it with: pipx install ansible-core (or your package manager's ansible-core)" ;;
                ssh) echo "setup: operator: missing ssh — install your system's OpenSSH client" ;;
                terraform|*/terraform) echo "setup: operator: missing terraform — install it from https://developer.hashicorp.com/terraform/install" ;;
                *) echo "setup: operator: missing ${c} — install it with your package manager" ;;
            esac
        done
        _setup_line operator FAIL "missing on this machine: ${missing[*]} (setup never installs here)"
        return 4
    fi
    _setup_line operator ok "on PATH: ${SETUP_OPERATOR_COMMANDS:-${REMOTE_OPERATOR_COMMANDS}}"
}

# _remote_https <url> : a GitHub SSH remote as https, the Host needing no key
_remote_https() {
    case "$1" in
        git@github.com:*) printf 'https://github.com/%s\n' "${1#git@github.com:}" ;;
        ssh://git@github.com/*) printf 'https://github.com/%s\n' "${1#ssh://git@github.com/}" ;;
        *) printf '%s\n' "$1" ;;
    esac
}

# _remote_default_ref : the tag at this clone's HEAD, else its commit
_remote_default_ref() {
    local git="${GIT_BIN:-git}"
    "${git}" -C "${AUTO_AGENT_ROOT}" describe --tags --exact-match HEAD 2>/dev/null \
        || "${git}" -C "${AUTO_AGENT_ROOT}" rev-parse HEAD 2>/dev/null
}

# ------------------------------------------------------------ the inventory

_remote_inventory_file() { printf '%s/%s.env\n' "$(setup_inventory_dir)" "$1"; }

# _remote_inventory_load <name> : exports the entry's keys (allowlisted only)
_remote_inventory_load() {
    local file line key; file="$(_remote_inventory_file "$1")"
    [ -f "${file}" ] || return 1
    while IFS= read -r line || [ -n "${line}" ]; do
        key="${line%%=*}"
        case " ${REMOTE_INVENTORY_KEYS} " in
            *" ${key} "*) printf -v "${key}" '%s' "${line#*=}" ;;
        esac
    done < "${file}"
}

# _remote_inventory_write <name> : the entry from the AUTO_AGENT_* variables
# of REMOTE_INVENTORY_KEYS, refusing any value that is a secret this run holds.
# Prints ok or changed.
_remote_inventory_write() {
    local name="$1" file new k v s
    file="$(_remote_inventory_file "${name}")"
    new="# auto-agent Host inventory (ADR 0009): how Setup reaches this Host again. Never a secret."
    for k in ${REMOTE_INVENTORY_KEYS}; do
        v="${!k:-}"
        for s in "${R_GH_TOKEN}" "${R_CLAUDE_TOKEN}" "${R_TS_KEY}"; do
            if [ -n "${s}" ] && [[ "${v}" == *"${s}"* ]]; then
                _remote_err "inventory: refusing to write a secret into ${file}"; return 1
            fi
        done
        new+=$'\n'"${k}=${v}"
    done
    if [ -f "${file}" ] && [ "$(cat "${file}")" = "${new}" ]; then echo ok; return 0; fi
    mkdir -p "$(dirname "${file}")" && chmod 700 "$(dirname "${file}")" 2>/dev/null
    printf '%s\n' "${new}" > "${file}" || return 1
    echo changed
}

# ------------------------------------------------------------ SSH

# _remote_ssh [-t] <command> : one command on the Host, as the Host user,
# with the engine's PATH (the native Claude installer's bin dir first)
_remote_ssh() {
    local tty=()
    [ "$1" = "-t" ] && { tty=(-t); shift; }
    local args=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -p "${AUTO_AGENT_HOST_SSH_PORT}")
    [ -n "${AUTO_AGENT_HOST_SSH_IDENTITY}" ] && args+=(-i "${AUTO_AGENT_HOST_SSH_IDENTITY}")
    "${SSH_BIN:-ssh}" "${tty[@]}" "${args[@]}" "${AUTO_AGENT_HOST_SSH}" \
        "PATH=\"\$HOME/.local/bin:\$PATH\"; $1"
}

# _remote_q <args...> : each argument quoted for the Host's shell
_remote_q() { local a out=""; for a in "$@"; do out+=" $(printf '%q' "${a}")"; done; printf '%s' "${out# }"; }

# _remote_facts : reads HOME and the Host env's non-secret facts into R_FACT_*;
# a secret is reported present or absent, never its value.
_remote_facts() {
    local out line
    # shellcheck disable=SC2016
    out="$(_remote_ssh 'f="${AUTO_AGENT_HOST_ENV:-$HOME/.config/auto-agent/env}"
        echo "HOME=$HOME"
        command -v tailscale >/dev/null 2>&1 && tailscale status >/dev/null 2>&1 && echo "TAILSCALE=up"
        [ -f "$f" ] || exit 0
        sed -n -e "s/^\(GH_TOKEN\|CLAUDE_CODE_OAUTH_TOKEN\)=..*/\1=present/p" \
               -e "/^\(DAEMON_GH_LOGIN\|CLAUDE_AUTH_MODE\|AUTO_AGENT_TARGET_DIR\)=/p" "$f"' 2>/dev/null)" || return 1
    R_FACT_HOME=""; R_FACT_GH_TOKEN=""; R_FACT_CLAUDE_TOKEN=""; R_FACT_LOGIN=""; R_FACT_MODE=""; R_FACT_TARGET=""; R_FACT_TAILSCALE=""
    while IFS= read -r line; do
        case "${line}" in
            HOME=*) R_FACT_HOME="${line#HOME=}" ;;
            GH_TOKEN=present) R_FACT_GH_TOKEN=1 ;;
            CLAUDE_CODE_OAUTH_TOKEN=present) R_FACT_CLAUDE_TOKEN=1 ;;
            DAEMON_GH_LOGIN=*) R_FACT_LOGIN="${line#*=}" ;;
            CLAUDE_AUTH_MODE=*) R_FACT_MODE="${line#*=}" ;;
            AUTO_AGENT_TARGET_DIR=*) R_FACT_TARGET="${line#*=}" ;;
            TAILSCALE=up) R_FACT_TAILSCALE=1 ;;
        esac
    done <<< "${out}"
    [ -n "${R_FACT_HOME}" ]
}

_remote_reach() {
    local what="$1"
    R_FACT_HOME=""
    if ! _remote_facts; then
        _setup_line ssh FAIL "cannot reach ${AUTO_AGENT_HOST_SSH} over SSH (port ${AUTO_AGENT_HOST_SSH_PORT}${AUTO_AGENT_HOST_SSH_IDENTITY:+, key ${AUTO_AGENT_HOST_SSH_IDENTITY}}): is it up, and is your public key in its ~/.ssh/authorized_keys?"
        return 13
    fi
    _setup_line ssh ok "reached ${AUTO_AGENT_HOST_SSH} (${what:-Host user home ${R_FACT_HOME}})"
}

# _remote_abs_on_host <path> : absolute on the Host
_remote_abs_on_host() {
    case "$1" in
        /*) printf '%s\n' "${1%/}" ;;
        '~/'*) printf '%s/%s\n' "${R_FACT_HOME}" "${1#\~/}" ;;
        *) printf '%s/%s\n' "${R_FACT_HOME}" "${1%/}" ;;
    esac
}

# ------------------------------------------------------------ secrets

# _remote_secret <var> <file> <override> <held-on-host> <question> <names>
# Sets <var> to the secret to hand over, or "" when the Host env already holds
# one (and no --rotate). Returns 1 with the reason in SETUP_SECRET_WHY.
_remote_secret() {
    local var="$1" file="$2" override="$3" held="$4" question="$5" names="$6" v=""
    if [ -n "${file}" ]; then
        v="$(_setup_read_secret_file "${file}")" || { SETUP_SECRET_WHY="cannot read a token from ${file}"; return 1; }
    elif [ -n "${override}" ]; then v="${override}"
    elif [ "${S_ROTATE}" != "1" ] && [ -n "${held}" ]; then v=""
    else v="$(_setup_ask "${question}" secret)" || { SETUP_SECRET_WHY="no ${names}"; return 1; }
    fi
    printf -v "${var}" '%s' "${v}"
}

# ------------------------------------------------------------ install play

# _remote_install : runs install.yml over SSH. Hands over R_GH_TOKEN and
# R_CLAUDE_TOKEN when set, the R_CONFIG_DRAFT file when given, and tailscale
# (R_TS_KEY, the auth key, rides the same 0600 vars file) when the Host wants it.
_remote_install() {
    local vars="${R_TMP}/install-vars.json" log="${R_TMP}/install.log" rc changed host user
    local handoff=""
    [ -n "${R_GH_TOKEN}" ] && handoff+="AUTO_AGENT_SETUP_GH_TOKEN=${R_GH_TOKEN}"$'\n'
    [ -n "${R_CLAUDE_TOKEN}" ] && handoff+="AUTO_AGENT_SETUP_CLAUDE_TOKEN=${R_CLAUDE_TOKEN}"$'\n'
    local draft_content=""
    if [ -n "${R_CONFIG_DRAFT}" ]; then
        draft_content="$(cat "${R_CONFIG_DRAFT}" 2>/dev/null)" || {
            _setup_line install FAIL "cannot read the draft ${R_CONFIG_DRAFT}"; return 7; }
    fi
    # Secrets reach jq through its environment only, never its argv.
    ( umask 077
      AA_HANDOFF="${handoff}" AA_DRAFT="${draft_content}" \
      AA_INSTALL="${AUTO_AGENT_INSTALL_DIR}" AA_REPO="${AUTO_AGENT_HARNESS_REPO}" AA_REF="${AUTO_AGENT_HARNESS_REF}" \
      AA_HANDOFF_PATH="${R_FACT_HOME}/.config/auto-agent/setup-handoff" AA_DRAFT_PATH="${R_DRAFT_ON_HOST}" \
      AA_TS="${AUTO_AGENT_HOST_TAILSCALE:-0}" AA_TS_KEY="${R_TS_KEY}" AA_TS_NAME="${AUTO_AGENT_HOST_NAME:-}" \
      jq -n '{
          aa_install_dir: env.AA_INSTALL, aa_harness_repo: env.AA_REPO, aa_harness_ref: env.AA_REF,
          aa_handoff_path: env.AA_HANDOFF_PATH, aa_handoff_content: env.AA_HANDOFF,
          aa_config_draft_path: env.AA_DRAFT_PATH, aa_config_draft_content: env.AA_DRAFT,
          aa_tailscale: (env.AA_TS == "1"), aa_tailscale_authkey: env.AA_TS_KEY,
          aa_tailscale_hostname: env.AA_TS_NAME
      }' > "${vars}" ) || { _setup_line install FAIL "cannot write the install vars"; return 14; }

    host="${AUTO_AGENT_HOST_SSH#*@}"; user=""
    [ "${host}" != "${AUTO_AGENT_HOST_SSH}" ] && user="${AUTO_AGENT_HOST_SSH%%@*}"
    local args=(-i "${host}," -e "ansible_port=${AUTO_AGENT_HOST_SSH_PORT}")
    [ -n "${user}" ] && args+=(-e "ansible_user=${user}")
    [ -n "${AUTO_AGENT_HOST_SSH_IDENTITY}" ] && args+=(--private-key "${AUTO_AGENT_HOST_SSH_IDENTITY}")
    local q before; q="$(_remote_q "${AUTO_AGENT_INSTALL_DIR}")"
    before="$(_remote_ssh "git -C ${q} rev-parse HEAD 2>/dev/null" 2>/dev/null)" || before=""
    ANSIBLE_CONFIG="${SETUP_ANSIBLE_DIR}/ansible.cfg" \
        "${ANSIBLE_PLAYBOOK_BIN:-ansible-playbook}" "${args[@]}" -e "@${vars}" "${REMOTE_INSTALL_PLAY}" > "${log}" 2>&1
    rc=$?
    rm -f "${vars}"
    if [ "${rc}" -ne 0 ]; then
        local why; why="$(grep -o 'baseline: this Host runs[^"]*' "${log}" | head -1)"
        if [ -n "${why}" ]; then
            _setup_line baseline FAIL "${why#baseline: }"; return 3
        fi
        tail -20 "${log}" >&2
        _setup_line install FAIL "the install play exited ${rc} on ${AUTO_AGENT_HOST_SSH} (its output is above)"; return 14
    fi
    changed="$(awk '/^PLAY RECAP/ { r = 1; next } r && /changed=/ { for (i = 1; i <= NF; i++) if ($i ~ /^changed=/) { split($i, a, "="); s += a[2] } } END { print s + 0 }' "${log}")"
    local after; after="$(_remote_ssh "git -C ${q} rev-parse HEAD" 2>/dev/null)" || after=""
    R_SHA="${after:0:12}"; [ -n "${R_SHA}" ] || R_SHA="?"
    # A Harness install that moved under enabled units needs them restarted.
    R_MOVED=0; [ -n "${before}" ] && [ "${before}" != "${after}" ] && R_MOVED=1
    local detail="Harness install ${AUTO_AGENT_INSTALL_DIR} at ${AUTO_AGENT_HARNESS_REF} (${R_SHA})"
    [ "${R_MOVED}" = "1" ] && detail+="; moved from ${before:0:12}"
    [ -n "${handoff}" ] && detail+="; secrets handed over (no_log)"
    [ "${AUTO_AGENT_HOST_TAILSCALE:-0}" = "1" ] && detail+="; on the tailnet${R_TS_KEY:+ (joined with the auth key, no_log)}"
    if [ "${changed}" -gt 0 ]; then _setup_line install changed "${detail}"
    else _setup_line install ok "${detail}"; fi
}

# ------------------------------------------------------------ the engine

# _remote_engine <engine-args...> : bin/auto-agent on the Host, streamed
_remote_engine() {
    _remote_ssh "cd && AUTO_AGENT_SETUP_ENTRY=ssh AUTO_AGENT_SETUP_INSTALL_MOVED=${R_MOVED:-0} $(_remote_q "${AUTO_AGENT_INSTALL_DIR}/bin/auto-agent" "$@")"
}

# _remote_drop_handoff : a run that stops between the install play and the
# engine deletes the handoff itself, so no secret waits on the Host
_remote_drop_handoff() {
    [ -n "${R_GH_TOKEN}${R_CLAUDE_TOKEN}" ] || return 0
    _remote_ssh "rm -f \"\$HOME/.config/auto-agent/setup-handoff\"" >/dev/null 2>&1
}

# _remote_claude_login : in the login mode an attended run offers
# `claude auth login` on the Host over `ssh -t` when it is not logged in
_remote_claude_login() {
    [ "${R_MODE}" = "login" ] || return 0
    _setup_unattended && return 0
    _remote_ssh "claude auth status --json >/dev/null 2>&1" && return 0
    echo "setup: claude-login: Claude is not logged in on ${AUTO_AGENT_HOST_SSH}; starting \`claude auth login\` there"
    _remote_ssh -t "claude auth login" < /dev/tty || true
    if _remote_ssh "claude auth status --json >/dev/null 2>&1"; then
        _setup_line claude-login changed "logged in on ${AUTO_AGENT_HOST_SSH}"
    else
        _setup_line claude-login FAIL "still not logged in on ${AUTO_AGENT_HOST_SSH}"; return 6
    fi
}

# ------------------------------------------------------------ commands

_remote_common_init() {
    # One scratch dir per run, even when the Proxmox entry point calls in.
    if [ -z "${R_TMP:-}" ] || [ ! -d "${R_TMP}" ]; then
        R_TMP="$(mktemp -d)" || return 1
        chmod 700 "${R_TMP}"
        # The vars file may hold both secrets: gone however this exits.
        # shellcheck disable=SC2064
        trap "rm -rf '${R_TMP}'" EXIT
        trap 'exit 130' INT TERM HUP
    fi
    R_GH_TOKEN=""; R_CLAUDE_TOKEN=""; R_TS_KEY=""; R_CONFIG_DRAFT=""; R_DRAFT_ON_HOST=""; R_SHA=""; R_MOVED=0
    AUTO_AGENT_HOST_SSH_PORT="${AUTO_AGENT_HOST_SSH_PORT:-22}"
}

remote_setup() {
    local host="" name="" target="" ssh_port="" identity="" ref="${AUTO_AGENT_SETUP_REF:-}" repo_url="${AUTO_AGENT_SETUP_HARNESS_REPO:-}"
    local install="" gh_login="" gh_token_file="" auth_mode="" claude_token_file="" repo="${AUTO_AGENT_SETUP_REPO:-}"
    local tailscale="" ts_key_file="" sets=() pass=()
    S_ROTATE="${AUTO_AGENT_SETUP_ROTATE:-0}"
    local gh_env="${AUTO_AGENT_SETUP_GH_TOKEN:-}" claude_env="${AUTO_AGENT_SETUP_CLAUDE_TOKEN:-}" ts_env="${AUTO_AGENT_SETUP_TAILSCALE_AUTHKEY:-}"
    unset AUTO_AGENT_SETUP_GH_TOKEN AUTO_AGENT_SETUP_CLAUDE_TOKEN AUTO_AGENT_SETUP_TAILSCALE_AUTHKEY
    local config="${AUTO_AGENT_SETUP_CONFIG:-}"
    while [ $# -gt 0 ]; do
        case "$1" in
            --host) host="${2:-}"; shift ;;
            --name) name="${2:-}"; shift ;;
            --ref) ref="${2:-}"; shift ;;
            --harness-repo) repo_url="${2:-}"; shift ;;
            --install-dir) install="${2:-}"; shift ;;
            --ssh-port) ssh_port="${2:-}"; shift ;;
            --ssh-identity) identity="${2:-}"; shift ;;
            --tailscale) tailscale=1 ;;
            --tailscale-authkey-file) tailscale=1; ts_key_file="${2:-}"; shift ;;
            --repo) repo="${2:-}"; shift ;;
            --gh-login) gh_login="${2:-}"; shift ;;
            --gh-token-file) gh_token_file="${2:-}"; shift ;;
            --auth-mode) auth_mode="${2:-}"; shift ;;
            --claude-token-file) claude_token_file="${2:-}"; shift ;;
            --config) config="${2:-}"; shift ;;
            --set) setup_valid_set "${2:-}" || return 2; sets+=("$2"); shift ;;
            --rotate) S_ROTATE=1 ;;
            --unattended) AUTO_AGENT_SETUP_UNATTENDED=1 ;;
            -h|--help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; return 0 ;;  # this file's header, not setup.sh's
            -*) _remote_err "unknown option '$1'"; return 2 ;;
            *) target="$1" ;;
        esac
        shift
    done
    [ -n "${host}" ] || { _remote_err "--host needs user@vm or an inventory name"; return 2; }
    _remote_common_init || return 1

    # A known name reaches the Host the inventory remembers.
    local k; for k in ${REMOTE_INVENTORY_KEYS}; do printf -v "${k}" '%s' ""; done
    AUTO_AGENT_HOST_SSH_PORT=22
    if _setup_is_host_name "${host}"; then
        name="${name:-${host}}"
        _remote_inventory_load "${host}"
    else
        name="${name:-${host#*@}}"
        _remote_inventory_load "${name}" 2>/dev/null || true
        AUTO_AGENT_HOST_SSH="${host}"
    fi
    case "${name}" in ''|*[!A-Za-z0-9._-]*) _remote_err "invalid Host name '${name}' (pass --name)"; return 2 ;; esac
    AUTO_AGENT_HOST_NAME="${name}"
    [ -n "${ssh_port}" ] && AUTO_AGENT_HOST_SSH_PORT="${ssh_port}"
    [ -n "${identity}" ] && AUTO_AGENT_HOST_SSH_IDENTITY="${identity}"
    [ -n "${tailscale}" ] && AUTO_AGENT_HOST_TAILSCALE=1
    AUTO_AGENT_HOST_PROVISIONER="${R_PROVISIONER:-${AUTO_AGENT_HOST_PROVISIONER:-}}"

    _remote_operator_doctor || return $?

    AUTO_AGENT_HARNESS_REPO="${repo_url:-${AUTO_AGENT_HARNESS_REPO:-$(_remote_https "$("${GIT_BIN:-git}" -C "${AUTO_AGENT_ROOT}" remote get-url origin 2>/dev/null)")}}"
    AUTO_AGENT_HARNESS_REF="${ref:-${AUTO_AGENT_HARNESS_REF:-$(_remote_default_ref)}}"
    if [ -z "${AUTO_AGENT_HARNESS_REPO}" ] || [ -z "${AUTO_AGENT_HARNESS_REF}" ]; then
        _remote_err "cannot tell where the Host should clone the Harness install from: pass --harness-repo and --ref"; return 2
    fi

    _remote_reach "" || return $?

    target="${target:-${AUTO_AGENT_TARGET_DIR:-${R_FACT_TARGET}}}"
    [ -n "${target}" ] || { _remote_err "name the Target Project checkout on the Host (setup --host ${host} <target-dir>)"; return 2; }
    AUTO_AGENT_TARGET_DIR="$(_remote_abs_on_host "${target}")"
    AUTO_AGENT_INSTALL_DIR="$(_remote_abs_on_host "${install:-${AUTO_AGENT_INSTALL_DIR:-auto-agent}}")"
    # Remembered so a Host rebuilt from scratch clones its checkout again.
    repo="${repo:-${AUTO_AGENT_TARGET_REPO}}"; AUTO_AGENT_TARGET_REPO="${repo}"

    # What the engine would ask for, asked here, where the operator is.
    # The inventory's copy outlives the Host env, so a rebuilt VM asks for
    # nothing but its secrets.
    gh_login="${gh_login:-${AUTO_AGENT_SETUP_GH_LOGIN:-${R_FACT_LOGIN:-${AUTO_AGENT_HOST_GH_LOGIN}}}}"
    [ -n "${gh_login}" ] || gh_login="$(_setup_ask "GitHub machine user login")" || {
        _setup_line github FAIL "no machine user: pass --gh-login or set AUTO_AGENT_SETUP_GH_LOGIN"; return 5; }
    _remote_secret R_GH_TOKEN "${gh_token_file}" "${gh_env}" "${R_FACT_GH_TOKEN}" \
        "Classic PAT for ${gh_login} (repo, project, workflow)" \
        "PAT for ${gh_login}: pass --gh-token-file or set AUTO_AGENT_SETUP_GH_TOKEN" || {
        _setup_line github FAIL "${SETUP_SECRET_WHY}"; return 5; }
    R_MODE="${auth_mode:-${AUTO_AGENT_SETUP_AUTH_MODE:-${R_FACT_MODE:-${AUTO_AGENT_HOST_AUTH_MODE:-login}}}}"
    AUTO_AGENT_HOST_GH_LOGIN="${gh_login}"; AUTO_AGENT_HOST_AUTH_MODE="${R_MODE}"
    if [ "${R_MODE}" = "setup-token" ]; then
        _remote_secret R_CLAUDE_TOKEN "${claude_token_file}" "${claude_env}" "${R_FACT_CLAUDE_TOKEN}" \
            "claude setup-token token" \
            "setup-token token: pass --claude-token-file or set AUTO_AGENT_SETUP_CLAUDE_TOKEN" || {
            _setup_line claude FAIL "${SETUP_SECRET_WHY}"; return 6; }
    fi
    # The auth key only while the Host is off the tailnet; once joined, the
    # Host stays joined and the key is never asked for again.
    if [ "${AUTO_AGENT_HOST_TAILSCALE}" = "1" ] && [ -z "${R_FACT_TAILSCALE}" ]; then
        if [ -n "${ts_key_file}" ]; then
            R_TS_KEY="$(_setup_read_secret_file "${ts_key_file}")" || {
                _setup_line tailscale FAIL "cannot read an auth key from ${ts_key_file}"; return 14; }
        else
            R_TS_KEY="${ts_env}"
            [ -n "${R_TS_KEY}" ] || R_TS_KEY="$(_setup_ask "Tailscale auth key for ${name}" secret)" || {
                _setup_line tailscale FAIL "${AUTO_AGENT_HOST_SSH} is not on the tailnet and there is no auth key: pass --tailscale-authkey-file or set AUTO_AGENT_SETUP_TAILSCALE_AUTHKEY"; return 14; }
        fi
    fi
    if [ -n "${config}" ]; then
        R_CONFIG_DRAFT="${config}"
        R_DRAFT_ON_HOST="${R_FACT_HOME}/.config/auto-agent/setup-config-draft.json"
    fi

    _remote_install || return $?
    local rc
    _remote_claude_login || { rc=$?; _remote_drop_handoff; return "${rc}"; }

    local inv; inv="$(_remote_inventory_write "${name}")" || {
        _remote_drop_handoff
        _setup_line inventory FAIL "cannot write $(_remote_inventory_file "${name}")"; return 14; }
    _setup_line inventory "${inv}" "$(_remote_inventory_file "${name}") (no secret: how to reach ${name} again)"

    # The engine itself, on the Host. Its secrets are in the handoff; only
    # non-secret answers travel on its command line.
    pass=(setup --unattended --gh-login "${gh_login}" --auth-mode "${R_MODE}" --set "AUTO_AGENT_HARNESS_REF=${AUTO_AGENT_HARNESS_REF}")
    # On the tailnet the Dashboard opts into all interfaces (ADR 0006); a
    # --set of its own still wins, coming later.
    [ "${AUTO_AGENT_HOST_TAILSCALE}" = "1" ] && pass+=(--set AUTO_AGENT_DASHBOARD_BIND=0.0.0.0)
    [ -n "${repo}" ] && pass+=(--repo "${repo}")
    [ -n "${R_DRAFT_ON_HOST}" ] && pass+=(--config "${R_DRAFT_ON_HOST}")
    [ "${S_ROTATE}" = "1" ] && pass+=(--rotate)
    local kv; for kv in "${sets[@]+"${sets[@]}"}"; do pass+=(--set "${kv}"); done
    pass+=("${AUTO_AGENT_TARGET_DIR}")
    _remote_engine "${pass[@]}"
    rc=$?
    [ -n "${R_DRAFT_ON_HOST}" ] && _remote_ssh "rm -f $(_remote_q "${R_DRAFT_ON_HOST}")" >/dev/null 2>&1
    return "${rc}"
}

remote_upgrade() {
    local name="$1"; shift
    local ref="" sets=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --ref) ref="${2:-}"; shift ;;
            --set) setup_valid_set "${2:-}" || return 2; sets+=("$2"); shift ;;
            *) _remote_err "upgrade <name>: unknown argument '$1'"; return 2 ;;
        esac
        shift
    done
    _remote_common_init || return 1
    _remote_inventory_load "${name}" || { _remote_err "no Host named ${name} in $(setup_inventory_dir)"; return 2; }
    local was="${AUTO_AGENT_HARNESS_REF}"
    AUTO_AGENT_HARNESS_REF="${ref:-${AUTO_AGENT_HARNESS_REF}}"
    [ -n "${AUTO_AGENT_HARNESS_REF}" ] || { _remote_err "upgrade: no ref (pass --ref)"; return 2; }
    _remote_operator_doctor || return $?
    _remote_reach "upgrading ${name} from ${was:-an unrecorded ref} to ${AUTO_AGENT_HARNESS_REF}" || return $?
    _remote_install || return $?
    local pass=(upgrade --set "AUTO_AGENT_HARNESS_REF=${AUTO_AGENT_HARNESS_REF}") kv
    for kv in "${sets[@]+"${sets[@]}"}"; do pass+=(--set "${kv}"); done
    pass+=("${AUTO_AGENT_TARGET_DIR}")
    _remote_engine "${pass[@]}"
    local rc=$?
    # The inventory records the ref once the upgrade finished; until then
    # `check` reports the install off its recorded ref.
    [ "${rc}" -eq 0 ] || return "${rc}"
    local inv; inv="$(_remote_inventory_write "${name}")" || {
        _setup_line inventory FAIL "cannot write $(_remote_inventory_file "${name}")"; return 14; }
    _setup_line inventory "${inv}" "$(_remote_inventory_file "${name}") records ${AUTO_AGENT_HARNESS_REF}"
}

remote_check() {
    local name="$1"
    R_TMP=""; R_GH_TOKEN=""; R_CLAUDE_TOKEN=""
    _remote_inventory_load "${name}" || { _remote_err "no Host named ${name} in $(setup_inventory_dir)"; return 2; }
    AUTO_AGENT_HOST_SSH_PORT="${AUTO_AGENT_HOST_SSH_PORT:-22}"
    if ! command -v "${SSH_BIN:-ssh}" >/dev/null 2>&1; then
        _check_line ssh FAIL "no ssh on this machine"; return 10
    fi
    if ! _remote_facts; then
        _check_line ssh FAIL "cannot reach ${AUTO_AGENT_HOST_SSH} over SSH"; return 10
    fi
    _check_line ssh ok "reached ${AUTO_AGENT_HOST_SSH} (${name})"
    local q; q="$(_remote_q "${AUTO_AGENT_INSTALL_DIR}")"
    local head want
    head="$(_remote_ssh "git -C ${q} rev-parse HEAD" 2>/dev/null)" || head=""
    want="$(_remote_ssh "git -C ${q} rev-parse --verify $(_remote_q "${AUTO_AGENT_HARNESS_REF}^{commit}")" 2>/dev/null)" || want=""
    local rc=0
    if [ -z "${head}" ]; then
        _check_line harness FAIL "no Harness install at ${AUTO_AGENT_INSTALL_DIR}"; rc=10
    elif [ "${head}" != "${want}" ]; then
        _check_line harness FAIL "${AUTO_AGENT_INSTALL_DIR} is at ${head:0:12}, not the inventory's ref ${AUTO_AGENT_HARNESS_REF}${want:+ (${want:0:12})}: run upgrade ${name}"; rc=10
    else
        _check_line harness ok "${AUTO_AGENT_INSTALL_DIR} at ${AUTO_AGENT_HARNESS_REF} (${head:0:12})"
    fi
    _remote_engine check "${AUTO_AGENT_TARGET_DIR}" || rc=10
    return "${rc}"
}

_remote_main() {
    local cmd="${1:-}"; shift || true
    case "${cmd}" in
        setup) remote_setup "$@" ;;
        upgrade) [ -n "${1:-}" ] || { _remote_err "upgrade <name>"; return 2; }; remote_upgrade "$@" ;;
        check) [ -n "${1:-}" ] || { _remote_err "check <name>"; return 2; }; remote_check "$1" ;;
        *) _remote_err "unknown command '${cmd}' (setup --host, upgrade <name>, check <name>)"; return 2 ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    set -uo pipefail
    _remote_main "$@"
    exit $?
fi
