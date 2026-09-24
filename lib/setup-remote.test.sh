#!/usr/bin/env bash
# Tests for lib/setup-remote.sh and the engine's upgrade subset (issue #39)
#
# Run: bash lib/setup-remote.test.sh
#
# Strategy: the Operator machine and the Host share this machine under two
# HOMEs. The ssh stub runs each command under the Host's HOME, so the in-VM
# engine really runs "over SSH" with the same stubs lib/setup.test.sh uses;
# the install play's stub places a Harness install whose refs v1 and v2 are
# real commits over this repo's files, and writes the setup handoff it was
# handed, the way the play's no_log task does. The play itself is covered by
# `ansible-playbook --syntax-check`; its real run belongs to #42.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"
FIXTURE="${ROOT_DIR}/plugin/fixtures/target-project"

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

# op <args...> : bin/auto-agent on the Operator machine (its own HOME)
op() {
    run_cli env HOME="${H}/op" STUB_HOST_HOME="${H}/home" SSH_BIN="${H}/bin/ssh" \
        SETUP_OPERATOR_COMMANDS="bash jq" "${OP_ENV[@]+"${OP_ENV[@]}"}" bash "${CLI}" "$@"
}
OP_ENV=()
make_op() { make_host; mkdir -p "${H}/op"; printf '%s\n' "${GH_SECRET}" > "${H}/op/pat"; printf '%s\n' "${CLAUDE_SECRET}" > "${H}/op/claude"; }
INV() { printf '%s/op/.config/auto-agent/hosts/%s.env' "${H}" "$1"; }
hostenv() { cat "${H}/home/.config/auto-agent/env"; }
out_has() { grep -qF -- "$1" "${H}/out"; }
remote_first() {
    op setup --host agent@vm1 --ref v1 --harness-repo https://example.invalid/auto-agent.git \
        --gh-login widget-bot --gh-token-file "${H}/op/pat" \
        --auth-mode setup-token --claude-token-file "${H}/op/claude" "$@" "${T}"
}

test_remote_setup() {
    echo "TEST: setup --host runs the shared stages over SSH and writes a secret-free inventory entry (AC 1, AC 3)"
    make_op
    remote_first
    check "setup --host exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -8 "${H}/out") $(tail -5 "${H}/err")"
    check "the Operator side ran first: operator, ssh, install, inventory" \
        '[ "$(grep -Eo "^setup: [a-z-]+:" "${H}/out" | head -4 | tr -d : | cut -d" " -f2 | tr "\n" " ")" = "operator ssh install inventory " ]' \
        "$(grep -Eo "^setup: [a-z-]+:" "${H}/out" | tr '\n' ' ')"
    local s
    for s in baseline doctor github claude config configure extension verify enable bootstrap; do
        check "the engine's ${s} stage ran on the Host" 'grep -Eq "^setup: ${s}: (ok|changed|skipped) — " "${H}/out"' "$(grep "^setup: ${s}" "${H}/out")"
    done
    check "the engine ran from the Harness install over SSH, unattended" \
        'grep -q "${H}/home/auto-agent/bin/auto-agent setup --unattended --gh-login widget-bot --auth-mode setup-token" "${H}/log/ssh.calls"'
    check "the install sits at the pinned ref" '[ "$(git -C "${H}/home/auto-agent" rev-parse HEAD)" = "$(git -C "${H}/home/auto-agent" rev-parse v1)" ]'
    check "the play got the ref, the repo and the install dir" \
        '[ "$(jq -r "[.aa_harness_ref, .aa_harness_repo, .aa_install_dir] | join(\" \")" "${H}/log/install.vars")" = "v1 https://example.invalid/auto-agent.git ${H}/home/auto-agent" ]'
    check "the Host env carries both secrets and the ref" \
        'hostenv | grep -qx "GH_TOKEN=${GH_SECRET}" && hostenv | grep -qx "CLAUDE_CODE_OAUTH_TOKEN=${CLAUDE_SECRET}" && hostenv | grep -qx "AUTO_AGENT_HARNESS_REF=v1"'
    check "both units enabled on the Host" 'grep -qx "enabled auto-agent-daemon.service" "${H}/log/units"'

    local inv; inv="$(INV vm1)"
    check "the inventory entry exists under the Operator's ~/.config/auto-agent/hosts" '[ -f "${inv}" ]'
    check "it says how to reach the Host again" \
        'grep -qx "AUTO_AGENT_HOST_SSH=agent@vm1" "${inv}" && grep -qx "AUTO_AGENT_HARNESS_REF=v1" "${inv}" && grep -qx "AUTO_AGENT_TARGET_DIR=${T}" "${inv}" && grep -qx "AUTO_AGENT_INSTALL_DIR=${H}/home/auto-agent" "${inv}"' "$(cat "${inv}")"
    check "it holds no secret and no token key" '! grep -Eq "SENTINEL|TOKEN" "${inv}"' "$(cat "${inv}")"

    # AC 3: the secrets travelled only through the play's handoff.
    check "the handoff carried both secrets" '[ "$(jq -c "[.handoff_has_gh, .handoff_has_claude]" "${H}/log/install.vars")" = "[true,true]" ]'
    check "the play's vars file was 0600 and is gone" '[ "$(cat "${H}/log/install.varsmode")" = 600 ] && [ ! -e "$(cat "${H}/log/install.varsfile")" ]'
    check "the engine consumed and deleted the handoff" '[ ! -e "${H}/home/.config/auto-agent/setup-handoff" ]'
    check "no secret on any command line (ssh, ansible, gh, git, claude)" '[ ! -e "${H}/log/argv-leak" ]' "$(cat "${H}/log/argv-leak" 2>/dev/null)"
    check "no secret on stdout or stderr" '! grep -q SENTINEL "${H}/out" "${H}/err"'
    check "no secret at rest on the Operator machine beyond the files it was given" \
        '[ -z "$(grep -rl SENTINEL "${H}/op" | grep -v -e "/op/pat$" -e "/op/claude$")" ]' "$(grep -rl SENTINEL "${H}/op")"
    check "on the Host the Host env is the only file holding a secret" \
        '[ "$(grep -rl SENTINEL "${H}/home" | tr "\n" " ")" = "${H}/home/.config/auto-agent/env " ]' "$(grep -rl SENTINEL "${H}/home")"
}

test_remote_config_draft() {
    echo "TEST: setup --host hands the skill's draft and PR body to the engine, then removes them (issue #41)"
    make_op
    git -C "${T}" rm -rq .auto-agent; git -C "${T}" -c user.name=t -c user.email=t@t commit -qm x; git -C "${T}" push -q origin main
    jq '.host = {docker: true}' "${FIXTURE}/.auto-agent/harness.json" > "${H}/op/draft.json"
    printf 'Drafted by /auto-agent:setup.\n' > "${H}/op/body.md"
    remote_first --config "${H}/op/draft.json" --config-pr-body "${H}/op/body.md"
    check "setup --host with a draft and a body exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out") $(tail -3 "${H}/err")"
    check "the engine was handed both files on the Host" \
        'grep -q -- "--config ${H}/home/.config/auto-agent/setup-config-draft.json" "${H}/log/ssh.calls" && grep -q -- "--config-pr-body ${H}/home/.config/auto-agent/setup-config-pr-body.md" "${H}/log/ssh.calls"' "$(grep 'bin/auto-agent setup' "${H}/log/ssh.calls")"
    check "the proposed config is the draft and the PR body the drafted one" \
        'git -C "${H}/remote/widget.git" show auto-agent/harness-config:.auto-agent/harness.json | jq -e ".host.docker == true" >/dev/null && grep -qx "Drafted by /auto-agent:setup." "${H}/log/pr-body"'
    check "both files are gone from the Host afterwards" \
        '[ ! -e "${H}/home/.config/auto-agent/setup-config-draft.json" ] && [ ! -e "${H}/home/.config/auto-agent/setup-config-pr-body.md" ]'
}

test_remote_rerun_by_name() {
    echo "TEST: setup --host <name> converges from the inventory, asking for no secret the Host holds"
    make_op
    remote_first
    : > "${H}/log/systemctl.writes"
    op setup --host vm1
    check "re-run by name exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out") $(tail -3 "${H}/err")"
    check "no secret was handed over again" '[ "$(jq -c "[.handoff_has_gh, .handoff_has_claude]" "${H}/log/install.vars")" = "[false,false]" ]'
    check "the ref, target and auth mode came back from the inventory and the Host" \
        'grep -q "setup --unattended --gh-login widget-bot --auth-mode setup-token --set AUTO_AGENT_HARNESS_REF=v1 ${T}\$" "${H}/log/ssh.calls"' "$(tail -1 "${H}/log/ssh.calls")"
    check "install and inventory report ok" 'grep -q "^setup: install: ok" "${H}/out" && grep -q "^setup: inventory: ok" "${H}/out"' "$(grep -E '^setup: (install|inventory)' "${H}/out")"
    check "the engine converged" 'out_has "setup: converged — nothing changed"' "$(tail -3 "${H}/out")"
    check "nothing was restarted" '[ ! -s "${H}/log/systemctl.writes" ]'

    op setup --host vm1 --rotate --gh-token-file "${H}/op/pat" --claude-token-file "${H}/op/claude"
    check "--rotate hands the secrets over again" '[ "${RC}" -eq 0 ] && [ "$(jq -c "[.handoff_has_gh, .handoff_has_claude]" "${H}/log/install.vars")" = "[true,true]" ]' "rc=${RC}"
    op setup --host vm1 --rotate
    check "--rotate with no secret and no terminal fails naming the override" \
        '[ "${RC}" -eq 5 ] && out_has "pass --gh-token-file or set AUTO_AGENT_SETUP_GH_TOKEN"' "$(tail -3 "${H}/out")"
}

test_remote_failures() {
    echo "TEST: the remote entry point fails early and names why"
    make_op
    OP_ENV=(STUB_REMOTE_DISTRO="Debian 12")
    remote_first
    OP_ENV=()
    check "a wrong distribution fails the baseline: exit 3" \
        '[ "${RC}" -eq 3 ] && out_has "setup: baseline: FAIL — this Host runs Debian 12; the reference Host is Ubuntu 24.04 LTS"' "rc=${RC} $(cat "${H}/out")"
    check "nothing ran on the Host and no inventory was written" \
        '! grep -q "bin/auto-agent" "${H}/log/ssh.calls" && [ ! -e "$(INV vm1)" ] && [ ! -e "${H}/home/.config/auto-agent/env" ]'

    make_op
    OP_ENV=(STUB_SSH_DOWN=1)
    remote_first
    OP_ENV=()
    check "an unreachable Host: exit 13, no play" '[ "${RC}" -eq 13 ] && out_has "setup: ssh: FAIL — cannot reach agent@vm1" && [ ! -e "${H}/log/install.vars" ]' "rc=${RC} $(cat "${H}/out")"

    make_op
    op setup --host agent@vm1 --ref v1 --harness-repo https://example.invalid/a.git --gh-login widget-bot "${T}"
    check "no PAT on a fresh Host: exit 5 naming the flag and the env key, before any install" \
        '[ "${RC}" -eq 5 ] && out_has "pass --gh-token-file or set AUTO_AGENT_SETUP_GH_TOKEN" && [ ! -e "${H}/log/install.vars" ]' "rc=${RC} $(cat "${H}/out")"

    make_op
    OP_ENV=(SETUP_OPERATOR_COMMANDS="bash ansible-playbook-missing-xyz")
    remote_first
    OP_ENV=()
    check "a missing Operator prerequisite: exit 4" '[ "${RC}" -eq 4 ] && out_has "setup: operator: FAIL — missing on this machine: ansible-playbook-missing-xyz"' "rc=${RC} $(cat "${H}/out")"

    make_op
    op setup --host agent@vm1 --ref v1 --harness-repo https://example.invalid/a.git
    check "no Target Project anywhere: usage, exit 2" '[ "${RC}" -eq 2 ] && grep -q "name the Target Project checkout on the Host" "${H}/err"' "rc=${RC} $(cat "${H}/err")"
}

test_upgrade_and_check() {
    echo "TEST: upgrade moves the ref and restarts; check reads the facts back over SSH (AC 2)"
    make_op
    remote_first
    op check vm1
    check "check <name> exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(cat "${H}/out") $(tail -3 "${H}/err")"
    check "check reads the install's ref back" 'grep -Eq "^check: harness: ok — ${H}/home/auto-agent at v1 \([0-9a-f]{12}\)" "${H}/out"' "$(grep harness "${H}/out")"
    check "check runs the in-VM verify over SSH" \
        'out_has "check: ssh: ok" && out_has "check: host-env: ok" && out_has "check: github: ok" && out_has "check: fire: ok"' "$(cat "${H}/out")"
    check "check writes nothing: no play, no setup stage" '! grep -q "^setup:" "${H}/out"'

    : > "${H}/log/systemctl.writes"
    printf '#!/usr/bin/env bash\necho "ext: $1"\n' > "${T}/.auto-agent/host-extension"; chmod +x "${T}/.auto-agent/host-extension"
    op upgrade vm1 --ref v2
    check "upgrade exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -8 "${H}/out") $(tail -3 "${H}/err")"
    check "the install moved to v2" '[ "$(git -C "${H}/home/auto-agent" rev-parse HEAD)" = "$(git -C "${H}/home/auto-agent" rev-parse v2)" ]'
    check "install reports the move" 'grep -q "^setup: install: changed — Harness install .* at v2" "${H}/out"' "$(grep '^setup: install' "${H}/out")"
    check "configure and the Host extension re-ran, told it is an upgrade" \
        'grep -q "^setup: configure: " "${H}/out" && out_has "setup: extension: | ext: upgrade"' "$(grep -E 'configure|extension' "${H}/out")"
    check "both units restarted" \
        'grep -qx "restart auto-agent-daemon.service" "${H}/log/systemctl.writes" && grep -qx "restart auto-agent-dashboard.service" "${H}/log/systemctl.writes"' "$(cat "${H}/log/systemctl.writes")"
    check "upgrade runs no verify, enable or bootstrap" '! grep -Eq "^setup: (verify|enable|bootstrap):" "${H}/out"'
    check "no secret was handed over" '[ "$(jq -c "[.handoff_has_gh, .handoff_has_claude]" "${H}/log/install.vars")" = "[false,false]" ]'
    check "the Host env and the inventory record v2" 'hostenv | grep -qx AUTO_AGENT_HARNESS_REF=v2 && grep -qx AUTO_AGENT_HARNESS_REF=v2 "$(INV vm1)"'
    op check vm1
    check "check after the upgrade reads v2 back" '[ "${RC}" -eq 0 ] && grep -q "^check: harness: ok — .* at v2" "${H}/out"' "rc=${RC} $(grep harness "${H}/out")"

    # The install drifting from the inventory is what check exists to catch.
    git -C "${H}/home/auto-agent" checkout -q --detach v1
    op check vm1
    check "an install off its ref fails check naming upgrade" '[ "${RC}" -eq 10 ] && out_has "not the inventory'"'"'s ref v2" && out_has "run upgrade vm1"' "rc=${RC} $(grep harness "${H}/out")"

    OP_ENV=(STUB_SSH_DOWN=1)
    op check vm1
    OP_ENV=()
    check "check on an unreachable Host: exit 10" '[ "${RC}" -eq 10 ] && out_has "check: ssh: FAIL"' "rc=${RC}"
    op upgrade no-such-host
    check "upgrade of an unknown name: exit 2 naming the inventory" '[ "${RC}" -eq 2 ] && grep -q "no Host named .no-such-host. in ${H}/op/.config/auto-agent/hosts" "${H}/err"' "rc=${RC} $(cat "${H}/err")"
    op check no-such-host
    check "check of an unknown name: exit 2" '[ "${RC}" -eq 2 ]' "rc=${RC} $(cat "${H}/err")"
}

test_in_vm_upgrade_and_handoff() {
    echo "TEST: the engine's in-VM upgrade subset, and the setup handoff"
    make_op
    run_cli bash "${CLI}" upgrade "${T}"
    check "upgrade before setup: exit 2" '[ "${RC}" -eq 2 ] && grep -q "run setup first" "${H}/err"' "rc=${RC} $(cat "${H}/err")"

    # The handoff counts as the overrides, is deleted, and carries nothing else.
    mkdir -p "${H}/home/.config/auto-agent"
    printf 'AUTO_AGENT_SETUP_GH_TOKEN=%s\nDAEMON_GH_LOGIN=intruder\nGH_BIN=/bin/false\n' "${GH_SECRET}" > "${H}/home/.config/auto-agent/setup-handoff"
    run_cli bash "${CLI}" setup --gh-login widget-bot "${T}"
    check "setup takes the PAT from the handoff" '[ "${RC}" -eq 0 ] && hostenv | grep -qx "GH_TOKEN=${GH_SECRET}"' "rc=${RC} $(tail -5 "${H}/out")"
    check "the handoff is gone" '[ ! -e "${H}/home/.config/auto-agent/setup-handoff" ]'
    check "only the two secret keys are read from it" '! hostenv | grep -q intruder'

    : > "${H}/log/systemctl.writes"; : > "${H}/log/gh.calls"
    run_cli bash "${CLI}" upgrade
    check "in-VM upgrade exits 0, reading the target from the Host env" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out") $(tail -3 "${H}/err")"
    check "the restart stage restarted both units" 'out_has "setup: restart: changed — restarted auto-agent-daemon.service, auto-agent-dashboard.service"'
    check "upgrade opened nothing on GitHub" '! grep -Eq "^(pr|issue) create" "${H}/log/gh.calls"'

    # An upgrade never proposes the Harness config.
    git -C "${T}" rm -rq .auto-agent; git -C "${T}" -c user.name=t -c user.email=t@t commit -qm x; git -C "${T}" push -q origin main
    run_cli bash "${CLI}" upgrade
    check "upgrade without a Harness config waits and opens no PR" \
        '[ "${RC}" -eq 0 ] && out_has "(setup proposes one, upgrade never does)" && ! grep -q "^pr create" "${H}/log/gh.calls"' "rc=${RC} $(grep 'setup: config' "${H}/out")"
    sed -i '/auto-agent-daemon.service/d' "${H}/log/units"
    run_cli bash "${CLI}" upgrade
    check "upgrade refuses a Host whose units were never enabled: exit 11" '[ "${RC}" -eq 11 ] && out_has "is not enabled: run setup, not upgrade"' "rc=${RC}"
}

test_install_play_parses() {
    echo "TEST: the install play parses and its handoff is no_log"
    if ! command -v ansible-playbook >/dev/null 2>&1; then
        echo "  SKIP: ansible-playbook not installed"; return
    fi
    local out
    out="$(ANSIBLE_CONFIG="${ROOT_DIR}/infra/ansible/ansible.cfg" ansible-playbook -i localhost, -c local \
        "${ROOT_DIR}/infra/ansible/install.yml" --syntax-check 2>&1)"
    check "ansible-playbook --syntax-check passes" 'printf "%s" "${out}" | grep -q "^playbook: "' "${out}"
    check "the handoff task is no_log" \
        'awk "/name: The setup handoff/{f=1} f&&/no_log: true/{print; exit}" "${ROOT_DIR}/infra/ansible/install.yml" | grep -q no_log'
    check "the distribution is asserted before anything is installed" \
        '[ "$(grep -n -m1 "ansible.builtin.assert" "${ROOT_DIR}/infra/ansible/install.yml" | cut -d: -f1)" -lt "$(grep -n -m1 "ansible.builtin.apt" "${ROOT_DIR}/infra/ansible/install.yml" | cut -d: -f1)" ]'
}

test_review_followups() {
    echo "TEST: a moved install restarts the units; a failed run leaves no handoff and records no ref"
    make_op
    remote_first
    : > "${H}/log/systemctl.writes"
    op setup --host vm1 --ref v2
    check "setup --host at a new ref exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out")"
    check "install names the move" 'grep -q "^setup: install: changed — .* at v2 ([0-9a-f]*); moved from " "${H}/out"' "$(grep '^setup: install' "${H}/out")"
    check "enable restarted both units on the moved install" \
        'grep -qx "restart auto-agent-daemon.service" "${H}/log/systemctl.writes" && grep -qx "restart auto-agent-dashboard.service" "${H}/log/systemctl.writes"' "$(cat "${H}/log/systemctl.writes")"

    make_op
    mkdir -p "${H}/op/.config/auto-agent"; : > "${H}/op/.config/auto-agent/hosts"
    remote_first
    check "an inventory that cannot be written: exit 14" '[ "${RC}" -eq 14 ] && out_has "setup: inventory: FAIL"' "rc=${RC} $(tail -3 "${H}/out")"
    check "the handoff the play wrote was deleted" '[ ! -e "${H}/home/.config/auto-agent/setup-handoff" ]'
    check "the engine never ran" '! grep -q "bin/auto-agent setup" "${H}/log/ssh.calls"'

    make_op
    remote_first
    sed -i '/auto-agent-daemon.service/d' "${H}/log/units"
    op upgrade vm1 --ref v2
    check "a failed upgrade exits with the engine's code" '[ "${RC}" -eq 11 ]' "rc=${RC}"
    check "the inventory keeps the old ref" 'grep -qx AUTO_AGENT_HARNESS_REF=v1 "$(INV vm1)" && ! out_has "setup: inventory:"'
    op check vm1
    check "check then reports the install off its recorded ref" '[ "${RC}" -eq 10 ] && out_has "run upgrade vm1"' "rc=${RC} $(grep harness "${H}/out")"
}

test_remote_setup
test_remote_config_draft
test_review_followups
test_remote_rerun_by_name
test_remote_failures
test_upgrade_and_check
test_in_vm_upgrade_and_handoff
test_install_play_parses

echo
echo "setup-remote.test.sh: ${TESTS_RUN} run, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
