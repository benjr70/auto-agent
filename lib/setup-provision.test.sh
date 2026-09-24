#!/usr/bin/env bash
# Tests for lib/setup-provision.sh: `setup --provision proxmox` (issue #40)
#
# Run: bash lib/setup-provision.test.sh
#
# Strategy: lib/setup-remote.test.sh's two-HOME Host (the ssh stub runs every
# command under the Host's HOME), plus a terraform stub that behaves like the
# real environment where Setup depends on it: plan needs the API token in its
# environment, answers create while the VM is missing from the fake Proxmox
# (a marker file), no-op over identical vars, and apply writes a state that
# records every variable it was given, the way the real state records the
# resources' attributes. The real terraform is checked with `fmt`; the real
# plan and apply against a Proxmox belong to #42.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"
FIXTURE="${ROOT_DIR}/plugin/fixtures/target-project"
TF_DIR="${ROOT_DIR}/infra/terraform"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && printf '    %s\n' "$2"
}
check() { if eval "$2"; then pass "$1"; else fail "$1" "${3:-}"; fi; }

# shellcheck source=testdata/setup-host.sh
. "${SCRIPT_DIR}/testdata/setup-host.sh"

PVE_SECRET="root@pam!setup=SENTINEL-pve-0123-4567"
TS_SECRET="tskey-auth-SENTINELtailscale42"

OP_ENV=()
# op <args...> : bin/auto-agent on the Operator machine (its own HOME)
op() {
    run_cli env HOME="${H}/op" STUB_HOST_HOME="${H}/home" SSH_BIN="${H}/bin/ssh" \
        TERRAFORM_BIN="${H}/bin/terraform" SSH_KEYGEN_BIN="${H}/bin/ssh-keygen" \
        STUB_PVE="${H}/pve" STUB_PVE_TOKEN="${PVE_SECRET}" \
        SETUP_OPERATOR_COMMANDS="bash jq" SETUP_PROVISION_WAIT_SECS=0 \
        "${OP_ENV[@]+"${OP_ENV[@]}"}" bash "${CLI}" "$@"
}

make_op() {
    make_host
    mkdir -p "${H}/op/.ssh" "${H}/pve" "${H}/home/.local/bin"
    printf '%s\n' "${GH_SECRET}" > "${H}/op/pat"
    printf '%s\n' "${CLAUDE_SECRET}" > "${H}/op/claude"
    printf '%s\n' "${PVE_SECRET}" > "${H}/op/pve-token"
    printf '%s\n' "${TS_SECRET}" > "${H}/op/ts-key"
    echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOperatorKey op@laptop" > "${H}/op/.ssh/id_ed25519.pub"
    echo "10.0.0.50 ssh-ed25519 AAAAoldhostkey" > "${H}/op/.ssh/known_hosts"
    # This box may run a real tailscale: the Host's own reports "not joined"
    # until the install play joins it.
    printf '#!/usr/bin/env bash\nexit 1\n' > "${H}/home/.local/bin/tailscale"; chmod +x "${H}/home/.local/bin/tailscale"

    cat > "${H}/bin/ssh-keygen" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/ssh-keygen.calls"
EOF

    cat > "${H}/bin/terraform" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do case "${a}" in *SENTINEL*) echo "SECRET IN ARGV: terraform" >> "${STUB_LOG}/argv-leak" ;; esac; done
sub="$2"; shift 2
printf '%s token=%s\n' "${sub}" "${PROXMOX_VE_API_TOKEN:+set}" >> "${STUB_LOG}/tf.calls"
[ "${STUB_TF_FAIL:-}" = "${sub}" ] && { echo "Error: ${sub} failed (stub)"; exit 1; }
arg() { local p="$1" a; shift; for a in "$@"; do case "${a}" in "${p}"*) printf '%s' "${a#"${p}"}"; return ;; esac; done; }
case "${sub}" in
    init)
        arg -backend-config=path= "$@" > "${STUB_LOG}/tf.backend" ;;
    plan)
        [ "${PROXMOX_VE_API_TOKEN:-}" = "${STUB_PVE_TOKEN}" ] || { echo "Error: 401 Unauthorized"; exit 1; }
        vars="$(arg -var-file= "$@")"; out="$(arg -out= "$@")"; state="$(cat "${STUB_LOG}/tf.backend")"
        cp "${vars}" "${STUB_LOG}/tf.vars"
        name="$(jq -r .name "${vars}")"
        if [ ! -e "${STUB_PVE}/${name}" ] || [ ! -f "${state}" ]; then action=create
        elif [ "$(jq -S '.resources[0].attributes' "${state}")" != "$(jq -S . "${vars}")" ]; then action=update
        else action=no-op; fi
        jq -n --arg a "${action}" --slurpfile v "${vars}" '{vars: $v[0], resource_changes: [
            {type: "proxmox_download_file", change: {actions: ["no-op"]}},
            {type: "proxmox_virtual_environment_vm", change: {actions: [$a]}}]}' > "${out}" ;;
    show)
        cat "${2}" ;;
    apply)
        [ "${PROXMOX_VE_API_TOKEN:-}" = "${STUB_PVE_TOKEN}" ] || { echo "Error: 401 Unauthorized"; exit 1; }
        state="$(cat "${STUB_LOG}/tf.backend")"; mkdir -p "$(dirname "${state}")"
        jq '{version: 4, resources: [{type: "proxmox_virtual_environment_vm", attributes: .vars}],
             outputs: {host: {value: {name: .vars.name, vmid: 101, node: .vars.node,
                                      ip: (.vars.ipv4_cidr | split("/")[0]), user: (.vars.vm_user // "auto-agent"),
                                      cores: (.vars.cores // 4), memory_mb: (.vars.memory_mb // 12288), disk_gb: (.vars.disk_gb // 80)}}}}' "$2" > "${state}"
        touch "${STUB_PVE}/$(jq -r .vars.name "$2")" ;;
    output)
        jq -c '.outputs.host.value' "$(cat "${STUB_LOG}/tf.backend")" ;;
    *) echo "terraform stub: $sub $*" >&2; exit 1 ;;
esac
EOF
    chmod +x "${H}/bin/terraform" "${H}/bin/ssh-keygen"
}

INV() { printf '%s/op/.config/auto-agent/hosts/%s' "${H}" "$1"; }
hostenv() { cat "${H}/home/.config/auto-agent/env"; }
out_has() { grep -qF -- "$1" "${H}/out"; }
provision_first() {
    op setup --provision proxmox --name h1 --proxmox-endpoint https://pve.invalid:8006/ \
        --proxmox-token-file "${H}/op/pve-token" --node pve1 --ipv4 10.0.0.50/24 --gateway 10.0.0.1 \
        --ref v1 --harness-repo https://example.invalid/auto-agent.git \
        --gh-login widget-bot --gh-token-file "${H}/op/pat" \
        --auth-mode setup-token --claude-token-file "${H}/op/claude" --repo acme/widget "$@" "${T}"
}
# no_secret_at_rest : the secrets are nowhere on either machine but where they belong
no_secret_at_rest() {
    [ ! -e "${H}/log/argv-leak" ] \
        && ! grep -q SENTINEL "${H}/out" "${H}/err" \
        && [ -z "$(grep -rl SENTINEL "${H}/op" | grep -v -e "/op/pat$" -e "/op/claude$" -e "/op/pve-token$" -e "/op/ts-key$")" ] \
        && [ "$(grep -rl SENTINEL "${H}/home" | tr "\n" " ")" = "${H}/home/.config/auto-agent/env " ]
}

test_provision_end_to_end() {
    echo "TEST: setup --provision proxmox creates the VM and carries on into the shared stages (AC 1)"
    make_op
    provision_first
    check "one command exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -8 "${H}/out") $(tail -5 "${H}/err")"
    check "the stages in order: operator, provision, ssh, install, inventory" \
        '[ "$(grep -Eo "^setup: [a-z-]+:" "${H}/out" | head -5 | tr -d : | cut -d" " -f2 | tr "\n" " ")" = "operator provision ssh install inventory " ]' \
        "$(grep -Eo "^setup: [a-z-]+:" "${H}/out" | tr '\n' ' ')"
    check "operator names terraform" 'grep -q "^setup: operator: ok — on PATH: .*/terraform" "${H}/out"' "$(grep '^setup: operator' "${H}/out")"
    check "provision reports the created VM with the 12 GB / 80 GB defaults" \
        'out_has "setup: provision: changed — h1 (vmid 101) on pve1 at 10.0.0.50, 4 cores, 12288 MB, 80 GB; created, cloud-init done"' "$(grep '^setup: provision' "${H}/out")"
    local s
    for s in baseline doctor github claude config configure extension verify enable bootstrap; do
        check "the engine's ${s} stage ran on the new VM" 'grep -Eq "^setup: ${s}: (ok|changed|skipped) — " "${H}/out"' "$(grep "^setup: ${s}" "${H}/out")"
    done
    check "Setup reached the VM as the cloud-init user at its address" \
        'grep -q "auto-agent@10.0.0.50 .*bin/auto-agent setup --unattended --gh-login widget-bot" "${H}/log/ssh.calls"'
    check "the operator's key went to cloud-init" \
        '[ "$(jq -r ".ssh_public_keys[0]" "${H}/log/tf.vars")" = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOperatorKey op@laptop" ]'
    check "the VM's stale host key was forgotten" 'grep -qx -- "-R 10.0.0.50" "${H}/log/ssh-keygen.calls"'
    check "state sits beside the inventory entry" '[ "$(cat "${H}/log/tf.backend")" = "$(INV h1).proxmox.tfstate" ] && [ -f "$(INV h1).proxmox.tfstate" ]'
    check "the settings are remembered" \
        '[ "$(jq -c "[.proxmox_endpoint, .node, .ipv4_cidr, .gateway]" "$(INV h1).proxmox.json")" = "[\"https://pve.invalid:8006/\",\"pve1\",\"10.0.0.50/24\",\"10.0.0.1\"]" ]' "$(cat "$(INV h1).proxmox.json")"
    check "the inventory knows the Host is provisioned and how to rebuild its checkout" \
        'grep -qx AUTO_AGENT_HOST_PROVISIONER=proxmox "$(INV h1).env" && grep -qx AUTO_AGENT_HOST_SSH=auto-agent@10.0.0.50 "$(INV h1).env" && grep -qx AUTO_AGENT_TARGET_REPO=acme/widget "$(INV h1).env"' "$(cat "$(INV h1).env")"
    check "the Host env carries the ref and loopback Dashboard" 'hostenv | grep -qx AUTO_AGENT_HARNESS_REF=v1 && hostenv | grep -qx AUTO_AGENT_DASHBOARD_BIND=127.0.0.1'
}

test_state_free_of_secrets() {
    echo "TEST: no secret enters terraform, its state or the Operator machine (AC 2)"
    make_op
    provision_first
    check "the run exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out")"
    check "the state file holds no secret" '! grep -q SENTINEL "$(INV h1).proxmox.tfstate"'
    check "the vars terraform was handed hold no secret" '! grep -q SENTINEL "${H}/log/tf.vars"'
    check "the API token reached plan and apply in their environment" \
        'grep -qx "plan token=set" "${H}/log/tf.calls" && grep -qx "apply token=set" "${H}/log/tf.calls"'
    check "and no other terraform command" '! grep -Eq "^(init|show|output) token=set" "${H}/log/tf.calls"' "$(cat "${H}/log/tf.calls")"
    check "no secret on argv, stdout, or at rest outside the Host env" 'no_secret_at_rest' \
        "$(cat "${H}/log/argv-leak" 2>/dev/null) $(grep -rl SENTINEL "${H}/op" "${H}/home")"
    check "the environment declares no secret variable" \
        '! grep -rEq "sensitive|variable \"[a-z_]*(token|password|secret)" "${TF_DIR}" && ! grep -Eq "api_token|password" "${TF_DIR}/proxmox/providers.tf"'
}

test_rerun_and_rebuild() {
    echo "TEST: re-running converges; a destroyed VM is rebuilt to the same result (AC 3)"
    make_op
    provision_first
    local env1 inv1; env1="$(hostenv)"; inv1="$(cat "$(INV h1).env")"
    : > "${H}/log/tf.calls"; : > "${H}/log/ssh-keygen.calls"; : > "${H}/log/systemctl.writes"
    OP_ENV=(PROXMOX_VE_API_TOKEN="${PVE_SECRET}")
    op setup --provision proxmox --name h1
    check "a re-run by name exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out") $(tail -3 "${H}/err")"
    check "terraform found nothing to change and applied nothing" \
        'out_has "setup: provision: ok — h1 (vmid 101) on pve1 at 10.0.0.50, 4 cores, 12288 MB, 80 GB; unchanged" && ! grep -q "^apply" "${H}/log/tf.calls"' "$(grep '^setup: provision' "${H}/out")"
    check "no host key forgotten, no secret handed over, nothing restarted" \
        '[ ! -s "${H}/log/ssh-keygen.calls" ] && [ "$(jq -c "[.handoff_has_gh, .handoff_has_claude]" "${H}/log/install.vars")" = "[false,false]" ] && [ ! -s "${H}/log/systemctl.writes" ]'
    check "the engine converged" 'out_has "setup: converged — nothing changed"' "$(tail -3 "${H}/out")"

    # The VM is destroyed: Proxmox loses it, the Host's disk and units go with it.
    rm -f "${H}/pve/h1"
    rm -rf "${H}/home"; mkdir -p "${H}/home/.local/bin"; : > "${H}/log/units"
    printf '#!/usr/bin/env bash\nexit 1\n' > "${H}/home/.local/bin/tailscale"; chmod +x "${H}/home/.local/bin/tailscale"
    op setup --provision proxmox --name h1 --gh-token-file "${H}/op/pat" --claude-token-file "${H}/op/claude"
    OP_ENV=()
    check "the rebuild exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -8 "${H}/out") $(tail -3 "${H}/err")"
    check "terraform created the VM again" 'grep -q "^setup: provision: changed — .*; created, cloud-init done" "${H}/out" && grep -q "^apply" "${H}/log/tf.calls"' "$(grep '^setup: provision' "${H}/out")"
    check "the new VM's host key is not refused" 'grep -qx -- "-R 10.0.0.50" "${H}/log/ssh-keygen.calls"'
    check "the Host env is the same as before" '[ "$(hostenv)" = "${env1}" ]' "$(diff <(echo "${env1}") <(hostenv))"
    check "the inventory is the same as before" '[ "$(cat "$(INV h1).env")" = "${inv1}" ]' "$(diff <(echo "${inv1}") "$(INV h1).env")"
    check "both units enabled again" 'grep -qx "enabled auto-agent-daemon.service" "${H}/log/units" && grep -qx "enabled auto-agent-dashboard.service" "${H}/log/units"'
    check "still no secret at rest" 'no_secret_at_rest'
}

test_tailscale() {
    echo "TEST: --tailscale joins the tailnet and opens the Dashboard on all interfaces"
    make_op
    provision_first --tailscale --tailscale-authkey-file "${H}/op/ts-key"
    check "the run exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out") $(tail -3 "${H}/err")"
    check "the install play got tailscale and the key, named after the Host" \
        '[ "$(jq -c "[.aa_tailscale, .has_ts_key, .aa_tailscale_hostname]" "${H}/log/install.vars")" = "[true,true,\"h1\"]" ]' "$(cat "${H}/log/install.vars")"
    check "install says so" 'grep -q "^setup: install: changed — .*; on the tailnet (joined with the auth key, no_log)" "${H}/out"' "$(grep '^setup: install' "${H}/out")"
    check "the Dashboard binds all interfaces" 'hostenv | grep -qx AUTO_AGENT_DASHBOARD_BIND=0.0.0.0'
    check "the inventory remembers tailscale" 'grep -qx AUTO_AGENT_HOST_TAILSCALE=1 "$(INV h1).env"'
    check "the auth key is nowhere but the file it came from" 'no_secret_at_rest' "$(grep -rl SENTINEL "${H}/op" "${H}/home")"

    OP_ENV=(PROXMOX_VE_API_TOKEN="${PVE_SECRET}")
    op setup --provision proxmox --name h1
    OP_ENV=()
    check "a re-run on the tailnet asks for no key and keeps the bind" \
        '[ "${RC}" -eq 0 ] && [ "$(jq -c "[.aa_tailscale, .has_ts_key]" "${H}/log/install.vars")" = "[true,false]" ] && hostenv | grep -qx AUTO_AGENT_DASHBOARD_BIND=0.0.0.0' \
        "rc=${RC} $(tail -3 "${H}/out")"

    make_op
    provision_first --tailscale
    check "tailscale off the tailnet with no key: exit 14 naming the override, before the play" \
        '[ "${RC}" -eq 14 ] && out_has "pass --tailscale-authkey-file or set AUTO_AGENT_SETUP_TAILSCALE_AUTHKEY" && [ ! -e "${H}/log/install.vars" ]' "rc=${RC} $(tail -3 "${H}/out")"
}

test_failures() {
    echo "TEST: the Proxmox entry point fails early and names why"
    make_op
    op setup --provision proxmox --name h1 --proxmox-endpoint https://pve.invalid:8006/ --node pve1 \
        --ipv4 10.0.0.50/24 --gateway 10.0.0.1 --gh-login widget-bot "${T}"
    check "no API token: exit 15 naming the flag and the env key, before terraform" \
        '[ "${RC}" -eq 15 ] && out_has "pass --proxmox-token-file or set PROXMOX_VE_API_TOKEN" && [ ! -e "${H}/log/tf.calls" ]' "rc=${RC} $(tail -3 "${H}/out")"

    make_op
    op setup --provision proxmox --name h1 --proxmox-token-file "${H}/op/pve-token" "${T}"
    check "a new Host without its settings: usage naming each flag" \
        '[ "${RC}" -eq 2 ] && grep -q "needs --proxmox-endpoint --node --ipv4 --gateway" "${H}/err"' "rc=${RC} $(cat "${H}/err")"

    make_op
    OP_ENV=(STUB_TF_FAIL=plan)
    provision_first
    OP_ENV=()
    check "a failed plan: exit 15, terraform's output shown, nothing reached" \
        '[ "${RC}" -eq 15 ] && out_has "setup: provision: FAIL — terraform plan exited 1 against https://pve.invalid:8006/" && grep -q "plan failed (stub)" "${H}/err" && [ ! -e "${H}/log/ssh.calls" ] && ! grep -q SENTINEL "${H}/out" "${H}/err"' "rc=${RC} $(tail -3 "${H}/out")"

    make_op
    OP_ENV=(STUB_SSH_DOWN=1)
    provision_first
    OP_ENV=()
    check "a VM that never answers SSH: exit 15 pointing at its console" \
        '[ "${RC}" -eq 15 ] && out_has "setup: provision: FAIL — h1 (vmid 101) never answered SSH as auto-agent@10.0.0.50" && [ ! -e "$(INV h1).env" ]' "rc=${RC} $(tail -3 "${H}/out")"

    make_op
    OP_ENV=(TERRAFORM_BIN="${H}/no-such/terraform")
    provision_first
    OP_ENV=()
    check "no terraform on this machine: exit 4 with its install pointer" \
        '[ "${RC}" -eq 4 ] && out_has "missing terraform — install it from" && out_has "setup: operator: FAIL"' "rc=${RC} $(cat "${H}/out")"

    make_op
    op setup --provision proxmox --host agent@vm1 "${T}"
    check "--provision with --host: usage" '[ "${RC}" -eq 2 ] && grep -q "two entry points" "${H}/err"' "rc=${RC}"
    op setup --provision aws "${T}"
    check "another Provisioner: usage" '[ "${RC}" -eq 2 ] && grep -q "the one Provisioner is proxmox" "${H}/err"' "rc=${RC}"
    op setup --provision proxmox --name h1 --memory-mb 12g "${T}"
    check "a non-numeric size: usage" '[ "${RC}" -eq 2 ] && grep -q -- "--memory-mb: a whole number" "${H}/err"' "rc=${RC} $(cat "${H}/err")"
    op setup --provision proxmox --name h1 --vm-id "" "${T}"
    check "an empty number: usage" '[ "${RC}" -eq 2 ] && grep -q -- "--vm-id: a whole number" "${H}/err"' "rc=${RC} $(cat "${H}/err")"
}

test_terraform_environment() {
    echo "TEST: the terraform environment's defaults and shape"
    local v="${TF_DIR}/proxmox/variables.tf"
    check "12 GB and 80 GB by default (ADR 0004)" \
        'grep -A3 "variable \"memory_mb\"" "${v}" | grep -q "default *= 12288" && grep -A3 "variable \"disk_gb\"" "${v}" | grep -q "default *= 80"'
    check "an Ubuntu 24.04 Server cloud image by default" 'grep -q "noble-server-cloudimg-amd64.img" "${v}"'
    check "cloud-init installs the operator's key and no password" \
        'grep -q "keys *= var.ssh_public_keys" "${TF_DIR}/modules/proxmox-vm/main.tf" && ! grep -q "password *=" "${TF_DIR}/modules/proxmox-vm/main.tf"'
    check "the provider is bpg/proxmox, locked" \
        'grep -q "source *= \"bpg/proxmox\"" "${TF_DIR}/proxmox/versions.tf" && grep -q "registry.terraform.io/bpg/proxmox" "${TF_DIR}/proxmox/.terraform.lock.hcl"'
    check "the output is the contract setup-provision.sh reads" \
        'for k in name vmid node ip user cores memory_mb disk_gb; do grep -Eq "^ *${k} *= " "${TF_DIR}/proxmox/outputs.tf" || exit 1; done'
    if command -v terraform >/dev/null 2>&1; then
        check "terraform fmt is clean" 'terraform fmt -check -recursive "${TF_DIR}" >/dev/null'
    else
        echo "  SKIP: terraform not installed (fmt)"
    fi
    check "the install play's tailscale join is no_log and keeps the key off argv" \
        'awk "/name: Join the tailnet/{f=1} f&&/no_log: true/{print; exit}" "${ROOT_DIR}/infra/ansible/install.yml" | grep -q no_log && grep -q "file:/dev/stdin" "${ROOT_DIR}/infra/ansible/install.yml"'
}

test_provision_end_to_end
test_state_free_of_secrets
test_rerun_and_rebuild
test_tailscale
test_failures
test_terraform_environment

echo
echo "setup-provision.test.sh: ${TESTS_RUN} run, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
