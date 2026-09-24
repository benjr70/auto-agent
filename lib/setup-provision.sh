#!/usr/bin/env bash
# Setup's Proxmox entry point (ADR 0004, ADR 0009): provision a new Host VM on
# Proxmox, then carry straight on into the bring-your-own-VM entry point
# (lib/setup-remote.sh) against it, so one command goes from nothing to a
# running Daemon. `bin/auto-agent setup --provision proxmox` lands here
# (lib/setup.sh routes it).
#
# How a run goes: the operator stage checks this machine (terraform besides
# what the remote entry point needs); the provision stage applies the
# terraform environment in infra/terraform/proxmox (bpg/proxmox: an Ubuntu
# 24.04 Server cloud image, cloud-init first boot with the operator's SSH key,
# 12 GB / 80 GB defaults), waits for SSH and for cloud-init to finish, then
# every stage of `setup --host <user>@<vm-ip> --name <name>` runs as usual.
#
# Usage:
#   setup-provision.sh setup --provision proxmox [--name <name>] [options] [<target-dir>]
#       <name> (default auto-agent) is the VM name and the Host inventory
#       entry. Every provision setting is remembered beside that entry
#       (<inventory>/<name>.proxmox.json, no secret), so a re-run needs only
#       --name and the secrets the Host lacks. Provision options:
#         --proxmox-endpoint <url>   PROXMOX_VE_ENDPOINT: https://<pve>:8006/
#         --proxmox-insecure         a self-signed Proxmox certificate
#         --proxmox-token-file <f>   PROXMOX_VE_API_TOKEN (the value):
#                                    user@realm!tokenid=secret; asked for on
#                                    every run, never stored
#         --node <node>              the Proxmox node
#         --ipv4 <cidr> --gateway <ip>
#                                    the VM's static address (Setup reaches it
#                                    there over SSH; the stock cloud image has
#                                    no guest agent to report a DHCP lease)
#         --dns <ip>                 repeatable; the gateway's resolver if none
#         --vm-id <n>                a static VMID (the next free one otherwise)
#         --datastore <id>           VM disk and cloud-init drive (local-lvm)
#         --image-datastore <id>     where the cloud image lands (local)
#         --image-url <url>          the cloud image (Ubuntu 24.04 amd64)
#         --bridge <bridge>          vmbr0;  --vlan <tag>  none
#         --cores <n>                4;  --memory-mb <mb>  12288;  --disk-gb <gb>  80
#         --vm-user <user>           the Host user cloud-init creates (auto-agent)
#         --ssh-public-key <file>    the key cloud-init installs (default:
#                                    --ssh-identity's .pub, else
#                                    ~/.ssh/id_ed25519.pub, else id_rsa.pub)
#       Every other option is the remote entry point's (--ssh-identity,
#       --tailscale, --tailscale-authkey-file, --ref, --repo, --gh-login,
#       --gh-token-file, --auth-mode, --claude-token-file, --config, --set,
#       --rotate, --unattended; see lib/setup-remote.sh) and is handed on.
#
# Re-running converges: terraform finds the VM and changes nothing; a VM that
# was destroyed is created again (its stale SSH host key forgotten), and the
# shared stages rebuild the Host from the inventory and the secrets passed.
# `upgrade <name>` and `check <name>` work on a provisioned Host unchanged.
#
# Output: `setup: operator: ...`, `setup: provision: ok|changed|FAIL — ...`,
# then the remote entry point's lines. Exit codes: the remote entry point's,
# plus 15 when provisioning fails (terraform, or the new Host never answers
# SSH).
#
# Secrets never enter terraform: no variable of the environment is a secret
# and the API token reaches terraform only in its environment (the provider
# reads PROXMOX_VE_API_TOKEN), so the state file
# (<inventory>/<name>.proxmox.tfstate, beside the inventory entry) holds
# none. The GitHub and Claude secrets travel as in the remote entry point.
#
# Env (test seams): TERRAFORM_BIN, SSH_KEYGEN_BIN, SETUP_PROVISION_WAIT_SECS
# (900: first boot upgrades packages), SETUP_PROVISION_POLL_SECS (5), and the
# remote entry point's.

_provision_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=setup-remote.sh
. "${_provision_lib_dir}/setup-remote.sh"

PROVISION_TF_DIR="${AUTO_AGENT_ROOT}/infra/terraform/proxmox"
# The settings terraform takes as numbers, not strings.
PROVISION_NUMBERS="vm_id vlan_tag cores memory_mb disk_gb"

# _provision_tf <args...> : terraform on the environment, this Host's data dir
_provision_tf() {
    TF_DATA_DIR="${P_TF_DATA}" TF_PLUGIN_CACHE_DIR="${P_TF_PLUGINS}" TF_IN_AUTOMATION=1 TF_INPUT=0 \
        "${TERRAFORM_BIN:-terraform}" -chdir="${PROVISION_TF_DIR}" "$@"
}

# _provision_tf_token <args...> : the same, with the API token in its
# environment (and only there)
_provision_tf_token() { PROXMOX_VE_API_TOKEN="${P_TOKEN}" _provision_tf "$@"; }

_provision_fail() {
    tail -20 "${R_TMP}/terraform.log" >&2
    _setup_line provision FAIL "$1 (its output is above)"
    return 15
}

# _provision_settings <file> <flags-json> : the remembered settings with this
# run's flags laid over them, written back; prints the merged JSON
_provision_settings() {
    local file="$1" flags="$2" old='{}' merged
    [ -f "${file}" ] && old="$(cat "${file}")"
    merged="$(jq -n --argjson old "${old}" --argjson new "${flags}" '$old + $new')" || return 1
    mkdir -p "$(dirname "${file}")" && chmod 700 "$(dirname "${file}")" 2>/dev/null
    printf '%s\n' "${merged}" > "${file}" || return 1
    printf '%s\n' "${merged}"
}

# _provision_default_pubkey <identity> : the public key cloud-init installs
_provision_default_pubkey() {
    local f
    for f in ${1:+"$1.pub"} "${HOME}/.ssh/id_ed25519.pub" "${HOME}/.ssh/id_rsa.pub"; do
        [ -r "${f}" ] && { printf '%s\n' "${f}"; return 0; }
    done
    return 1
}

# _provision_wait : SSH answers and cloud-init has finished
_provision_wait() {
    local waited=0 limit="${SETUP_PROVISION_WAIT_SECS:-900}" poll="${SETUP_PROVISION_POLL_SECS:-5}"
    until _remote_ssh 'cloud-init status --wait >/dev/null 2>&1; true' >/dev/null 2>&1; do
        [ "${waited}" -ge "${limit}" ] && return 1
        sleep "${poll}"; waited=$((waited + poll))
    done
}

provision_setup() {
    local provisioner="" name="" identity="" token_file="" pubkey_file="" rest=() dns=()
    local -A f=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --provision) provisioner="${2:-}"; shift ;;
            --name) name="${2:-}"; shift ;;
            --proxmox-endpoint) f[proxmox_endpoint]="${2:-}"; shift ;;
            --proxmox-insecure) f[proxmox_insecure]=true ;;
            --proxmox-token-file) token_file="${2:-}"; shift ;;
            --node) f[node]="${2:-}"; shift ;;
            --ipv4) f[ipv4_cidr]="${2:-}"; shift ;;
            --gateway) f[gateway]="${2:-}"; shift ;;
            --dns) dns+=("${2:-}"); shift ;;
            --vm-id) f[vm_id]="${2:-}"; shift ;;
            --datastore) f[datastore]="${2:-}"; shift ;;
            --image-datastore) f[image_datastore]="${2:-}"; shift ;;
            --image-url) f[image_url]="${2:-}"; shift ;;
            --bridge) f[bridge]="${2:-}"; shift ;;
            --vlan) f[vlan_tag]="${2:-}"; shift ;;
            --cores) f[cores]="${2:-}"; shift ;;
            --memory-mb) f[memory_mb]="${2:-}"; shift ;;
            --disk-gb) f[disk_gb]="${2:-}"; shift ;;
            --vm-user) f[vm_user]="${2:-}"; shift ;;
            --ssh-public-key) pubkey_file="${2:-}"; shift ;;
            --ssh-identity) identity="${2:-}"; shift ;;
            --host) _remote_err "--provision and --host are two entry points: pick one"; return 2 ;;
            --ssh-port|--install-dir) _remote_err "$1 does not apply to a provisioned Host"; return 2 ;;
            -h|--help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; return 0 ;;
            # The remote entry point's options that take a value, handed on whole.
            --ref|--harness-repo|--repo|--gh-login|--gh-token-file|--auth-mode|--claude-token-file|--config|--set|--tailscale-authkey-file)
                [ $# -ge 2 ] || { _remote_err "$1 needs a value"; return 2; }
                rest+=("$1" "$2"); shift ;;
            *) rest+=("$1") ;;
        esac
        shift
    done
    [ "${provisioner}" = "proxmox" ] || { _remote_err "--provision: the one Provisioner is proxmox (got '${provisioner}')"; return 2; }
    name="${name:-auto-agent}"
    case "${name}" in ''|*[!A-Za-z0-9-]*|-*) _remote_err "invalid Host name '${name}': letters, digits and dashes (it is the VM's name)"; return 2 ;; esac
    local k
    for k in ${PROVISION_NUMBERS}; do
        case "${f[$k]-1}" in ''|*[!0-9]*) _remote_err "--${k//_/-}: a whole number (got '${f[$k]}')"; return 2 ;; esac
    done

    _remote_common_init || return 1
    local token_env="${PROXMOX_VE_API_TOKEN:-}"
    unset PROXMOX_VE_API_TOKEN

    local tf="${TERRAFORM_BIN:-terraform}"
    SETUP_OPERATOR_COMMANDS="${SETUP_OPERATOR_COMMANDS:-${REMOTE_OPERATOR_COMMANDS}} ${tf}" _remote_operator_doctor || return $?
    R_OPERATOR_DONE=1

    # The settings: this run's flags over the remembered ones.
    local inv; inv="$(setup_inventory_dir)"
    local settings_file="${inv}/${name}.proxmox.json" flags="{}" settings
    [ -z "${f[proxmox_endpoint]:-}" ] && [ ! -f "${settings_file}" ] && [ -n "${PROXMOX_VE_ENDPOINT:-}" ] && f[proxmox_endpoint]="${PROXMOX_VE_ENDPOINT}"
    for k in "${!f[@]}"; do
        case " proxmox_insecure ${PROVISION_NUMBERS} " in
            *" ${k} "*) flags="$(jq -c --arg k "${k}" --argjson v "${f[$k]}" '. + {($k): $v}' <<< "${flags}")" ;;
            *) flags="$(jq -c --arg k "${k}" --arg v "${f[$k]}" '. + {($k): $v}' <<< "${flags}")" ;;
        esac
    done
    [ "${#dns[@]}" -gt 0 ] && flags="$(jq -c '. + {dns_servers: $ARGS.positional}' --args "${dns[@]}" <<< "${flags}")"
    if [ -n "${pubkey_file}" ] || [ ! -f "${settings_file}" ]; then
        pubkey_file="${pubkey_file:-$(_provision_default_pubkey "${identity}")}" || {
            _remote_err "no SSH public key for the VM: pass --ssh-public-key (or --ssh-identity with a .pub beside it)"; return 2; }
        local pub; pub="$(head -1 "${pubkey_file}" 2>/dev/null)"
        case "${pub}" in ssh-*|ecdsa-*|sk-*) ;; *) _remote_err "${pubkey_file} is not an SSH public key"; return 2 ;; esac
        flags="$(jq -c --arg k "${pub}" '. + {ssh_public_keys: [$k]}' <<< "${flags}")"
    fi
    flags="$(jq -c --arg n "${name}" '. + {name: $n}' <<< "${flags}")"
    settings="$(_provision_settings "${settings_file}" "${flags}")" || {
        _setup_line provision FAIL "cannot write ${settings_file}"; return 15; }
    local missing=() m
    for m in proxmox_endpoint:--proxmox-endpoint node:--node ipv4_cidr:--ipv4 gateway:--gateway; do
        jq -e --arg k "${m%%:*}" '.[$k] // "" | length > 0' <<< "${settings}" >/dev/null || missing+=("${m#*:}")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        _remote_err "a new Proxmox Host needs ${missing[*]} (remembered in ${settings_file} after this)"; return 2
    fi
    case "$(jq -r .ipv4_cidr <<< "${settings}")" in */*) ;; *) _remote_err "--ipv4 takes CIDR form, e.g. 192.168.1.50/24"; return 2 ;; esac

    # The token: this run only, never written anywhere.
    if [ -n "${token_file}" ]; then
        P_TOKEN="$(_setup_read_secret_file "${token_file}")" || {
            _setup_line provision FAIL "cannot read a Proxmox API token from ${token_file}"; return 15; }
    else
        P_TOKEN="${token_env}"
        [ -n "${P_TOKEN}" ] || P_TOKEN="$(_setup_ask "Proxmox API token (user@realm!tokenid=secret)" secret)" || {
            _setup_line provision FAIL "no Proxmox API token: pass --proxmox-token-file or set PROXMOX_VE_API_TOKEN"; return 15; }
    fi

    # terraform: state beside the inventory entry, working data in the cache.
    local cache="${XDG_CACHE_HOME:-${HOME}/.cache}/auto-agent/terraform"
    P_TF_DATA="${cache}/${name}"; P_TF_PLUGINS="${cache}/plugins"
    mkdir -p "${P_TF_DATA}" "${P_TF_PLUGINS}" || { _setup_line provision FAIL "cannot create ${cache}"; return 15; }
    local state="${inv}/${name}.proxmox.tfstate" vars="${R_TMP}/provision.tfvars.json" plan="${R_TMP}/provision.tfplan" log="${R_TMP}/terraform.log"
    jq . <<< "${settings}" > "${vars}"
    _provision_tf init -input=false -reconfigure -backend-config="path=${state}" > "${log}" 2>&1 \
        || { _provision_fail "terraform init exited $?"; return 15; }
    _provision_tf_token plan -input=false -var-file="${vars}" -out="${plan}" >> "${log}" 2>&1 \
        || { _provision_fail "terraform plan exited $? against $(jq -r .proxmox_endpoint <<< "${settings}")"; return 15; }
    local changes created
    changes="$(_provision_tf show -json "${plan}" 2>>"${log}")" || { _provision_fail "terraform show exited $?"; return 15; }
    created="$(jq -r '[.resource_changes[]? | select(.type == "proxmox_virtual_environment_vm" and (.change.actions | index("create")))] | length' <<< "${changes}")"
    changes="$(jq -r '[.resource_changes[]? | select(.change.actions != ["no-op"] and .change.actions != ["read"])] | length' <<< "${changes}")"
    if [ "${changes}" != "0" ]; then
        _provision_tf_token apply -input=false "${plan}" >> "${log}" 2>&1 \
            || { _provision_fail "terraform apply exited $?"; return 15; }
    fi
    rm -f "${plan}" "${vars}"
    local host
    host="$(_provision_tf output -json host 2>>"${log}")" || { _provision_fail "terraform output exited $?"; return 15; }
    local ip user vmid node
    ip="$(jq -r .ip <<< "${host}")"; user="$(jq -r .user <<< "${host}")"
    vmid="$(jq -r .vmid <<< "${host}")"; node="$(jq -r .node <<< "${host}")"

    # A VM created again answers with a new host key: forget the old one.
    AUTO_AGENT_HOST_SSH="${user}@${ip}"; AUTO_AGENT_HOST_SSH_PORT=22; AUTO_AGENT_HOST_SSH_IDENTITY="${identity}"
    if [ "${created}" != "0" ] && [ -f "${HOME}/.ssh/known_hosts" ]; then
        "${SSH_KEYGEN_BIN:-ssh-keygen}" -R "${ip}" >/dev/null 2>&1 || true
    fi
    if ! _provision_wait; then
        _setup_line provision FAIL "${name} (vmid ${vmid}) never answered SSH as ${AUTO_AGENT_HOST_SSH} within ${SETUP_PROVISION_WAIT_SECS:-900}s: check its console on ${node} (cloud-init, the address, your key)"
        return 15
    fi
    local shape; shape="$(jq -r '"\(.cores) cores, \(.memory_mb) MB, \(.disk_gb) GB"' <<< "${host}")"
    if [ "${changes}" != "0" ]; then
        _setup_line provision changed "${name} (vmid ${vmid}) on ${node} at ${ip}, ${shape}$([ "${created}" != "0" ] && echo "; created, cloud-init done")"
    else
        _setup_line provision ok "${name} (vmid ${vmid}) on ${node} at ${ip}, ${shape}; unchanged"
    fi
    P_TOKEN=""

    # The rest is the bring-your-own-VM entry point against the new VM.
    R_PROVISIONER=proxmox
    local pass=(--host "${AUTO_AGENT_HOST_SSH}" --name "${name}")
    [ -n "${identity}" ] && pass+=(--ssh-identity "${identity}")
    remote_setup "${pass[@]}" "${rest[@]+"${rest[@]}"}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    set -uo pipefail
    [ "${1:-}" = "setup" ] && shift
    provision_setup "$@"
    exit $?
fi
