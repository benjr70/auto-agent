#!/usr/bin/env bash
# Tests for infra/e2e/proxmox-e2e.sh: the end-to-end test's orchestration (#42)
#
# Run: bash infra/e2e/proxmox-e2e.test.sh
#
# Strategy: the run itself (a real Proxmox, a real Host, a real Fire) is the
# human's (docs/runbooks/e2e-proxmox.md). This suite stubs each boundary the
# script drives: Setup (E2E_CLI writes the inventory entry and state a real
# provision leaves), the destroy (E2E_PROVISION_LIB; the real one is tested in
# lib/setup-provision.test.sh), the Host over SSH (a sequence of /api/status
# answers, the journal, the State dir) and gh. The fixture push goes through
# real git into a local bare repo.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
E2E="${SCRIPT_DIR}/proxmox-e2e.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && printf '    %s\n' "$2"
}
check() { if eval "$2"; then pass "$1"; else fail "$1" "${3:-}"; fi; }

GH_SECRET="ghp_SENTINELgithub0123456789"
CLAUDE_SECRET="sk-ant-oat01-SENTINELclaude"
PVE_SECRET="root@pam!e2e=SENTINEL-pve"

H=""
cleanup() { [ -n "${H}" ] && rm -rf "${H}"; }
trap cleanup EXIT

make_env() {
    cleanup
    H="$(mktemp -d)"
    mkdir -p "${H}/bin" "${H}/log" "${H}/op" "${H}/inv" "${H}/status" "${H}/logs"
    printf '%s\n' "${GH_SECRET}" > "${H}/op/pat"
    printf '%s\n' "${CLAUDE_SECRET}" > "${H}/op/claude"
    printf '%s\n' "${PVE_SECRET}" > "${H}/op/pve"
    git init -q --bare -b main "${H}/fixture.git"

    cat > "${H}/bin/cli" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "${STUB_LOG}/cli.argv"
for a in "$@"; do case "${a}" in *SENTINEL*) echo "SECRET IN ARGV: cli" >> "${STUB_LOG}/argv-leak" ;; esac; done
name=""; prev=""; for a in "$@"; do [ "${prev}" = "--name" ] && name="${a}"; prev="${a}"; done
rc="${STUB_SETUP_RC:-0}"
if [ "${rc}" = 4 ]; then echo "setup: operator: FAIL — missing on this machine: terraform"; exit 4; fi
echo "setup: provision: changed — ${name} (vmid 101) on pve1 at 10.0.0.50; created, cloud-init done"
echo '{}' > "${AUTO_AGENT_INVENTORY_DIR}/${name}.proxmox.tfstate"
printf 'AUTO_AGENT_HOST_NAME=%s\nAUTO_AGENT_HOST_SSH=auto-agent@10.0.0.50\nAUTO_AGENT_HOST_SSH_PORT=22\n' "${name}" > "${AUTO_AGENT_INVENTORY_DIR}/${name}.env"
if [ "${rc}" != 0 ]; then echo "setup: verify: FAIL — the Provider check failed"; exit "${rc}"; fi
echo "setup: done — 9 changed: provision install configure enable"
EOF

    cat > "${H}/bin/destroy" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/destroy.calls"
for a in "$@"; do case "${a}" in *SENTINEL*) echo "SECRET IN ARGV: destroy" >> "${STUB_LOG}/argv-leak" ;; esac; done
name="$3"
[ "${STUB_DESTROY_RC:-0}" = 0 ] || { echo "setup: destroy: FAIL — terraform destroy exited non-zero for ${name}"; exit "${STUB_DESTROY_RC}"; }
if [ ! -f "${AUTO_AGENT_INVENTORY_DIR}/${name}.proxmox.tfstate" ]; then
    echo "setup: destroy: skipped — ${name}: no terraform state"; exit 0; fi
rm -f "${AUTO_AGENT_INVENTORY_DIR}/${name}.proxmox.tfstate" "${AUTO_AGENT_INVENTORY_DIR}/${name}.env"
echo "setup: destroy: changed — ${name} at 10.0.0.50 destroyed"
EOF

    # ssh: the Host. /api/status answers from ${STUB_STATUS}/<n>.json in turn,
    # staying on the last one.
    cat > "${H}/bin/ssh" <<'EOF'
#!/usr/bin/env bash
cmd="${!#}"
printf '%s\n' "${cmd}" | head -1 >> "${STUB_LOG}/ssh.calls"
case "${cmd}" in
    *api/status*)
        [ "${STUB_STATUS_DOWN:-0}" = 1 ] && exit 7
        n="$(cat "${STUB_LOG}/status.n" 2>/dev/null || echo 0)"; n=$((n + 1)); echo "${n}" > "${STUB_LOG}/status.n"
        f="${STUB_STATUS}/${n}.json"; [ -f "${f}" ] || f="$(ls "${STUB_STATUS}"/*.json | sort -V | tail -1)"
        cat "${f}" ;;
    *journalctl*) echo "2026-09-28T10:00:00+0000 host daemon[1]: [daemon] fire_complete" ;;
    *tar*) d="$(mktemp -d)"; mkdir -p "${d}/fires"; echo '{}' > "${d}/fires/f1.json"; tar -C "${d}" -czf - .; rm -rf "${d}" ;;
    *) exit 0 ;;
esac
EOF

    cat > "${H}/bin/gh" <<'EOF'
#!/usr/bin/env bash
[ "${GH_TOKEN:-}" = "${STUB_GH_TOKEN}" ] || { echo "gh: not authenticated" >&2; exit 4; }
case "$1 $2" in
    "repo view") echo main ;;
    "issue list") echo "${STUB_OPEN_ISSUES:-0}" ;;
    "pr list") echo 0 ;;
    "auth git-credential") exit 0 ;;
    *) echo "gh stub: $*" >&2; exit 1 ;;
esac
EOF
    # git: the real one, but this clone's HEAD can be made unpushed.
    cat > "${H}/bin/git" <<'EOF'
#!/usr/bin/env bash
case "$*" in *"branch -r --contains HEAD"*) [ "${STUB_UNPUSHED:-0}" = 1 ] && exit 0; echo "  origin/main"; exit 0 ;; esac
exec git "$@"
EOF
    chmod +x "${H}/bin/"*
}

# status <file-n> <fires-json> [jq-filter] : one /api/status answer
status() {
    jq -n --argjson items "$2" '{
        generatedAt: "2026-09-28T10:00:00+00:00",
        host: {repo: "acme/e2e-fixture", port: 8090},
        daemon: {unit: {active: "active"}, state: "queue_empty", stateDetail: "queue empty", parked: null},
        fires: {items: $items, current: null, stale: false, error: null},
        bootstrap: {warning: false}
    }' | jq "${3:-.}" > "${H}/status/$1.json"
}
DRY='{"id":"f1","kind":"dry-run","inFlight":false,"exit":0,"phase":"claude","outcome":"OK","summary":"would-pick"}'
FLY='{"id":"f2","kind":"pickup","inFlight":true,"exit":null,"phase":"claude","outcome":null,"summary":"in flight"}'
GREEN='{"id":"f2","kind":"pickup","inFlight":false,"exit":0,"phase":"claude","outcome":"OK","summary":"no work — queue empty","costUsd":0.05,"model":"claude-haiku-4-5"}'
RED='{"id":"f2","kind":"pickup","inFlight":false,"exit":1,"phase":"claude","outcome":"FAILED","summary":"FAILED — exit 1"}'

E2E_ENV=()
# e2e <args...> : the script with every boundary stubbed; RC, ${H}/out, ${H}/err
e2e() {
    env -u PROXMOX_VE_API_TOKEN -u GH_TOKEN \
        STUB_LOG="${H}/log" STUB_STATUS="${H}/status" STUB_GH_TOKEN="${GH_SECRET}" \
        AUTO_AGENT_INVENTORY_DIR="${H}/inv" E2E_CLI="${H}/bin/cli" E2E_PROVISION_LIB="${H}/bin/destroy" \
        E2E_FIXTURE_URL="${H}/fixture.git" E2E_POLL_SECS=0 \
        GH_BIN="${H}/bin/gh" GIT_BIN="${H}/bin/git" SSH_BIN="${H}/bin/ssh" \
        "${E2E_ENV[@]+"${E2E_ENV[@]}"}" bash "${E2E}" "$@" > "${H}/out" 2> "${H}/err" < /dev/null
    RC=$?
}
run() {
    e2e --fixture-repo acme/e2e-fixture --gh-login e2e-bot --gh-token-file "${H}/op/pat" \
        --claude-token-file "${H}/op/claude" --proxmox-token-file "${H}/op/pve" \
        --proxmox-endpoint https://pve.invalid:8006/ --node pve1 --ipv4 10.0.0.50/24 --gateway 10.0.0.1 \
        --log-dir "${H}/logs" "$@"
}
out_has() { grep -qF -- "$1" "${H}/out"; }
stages() { grep -Eo '^e2e: [a-z]+: [a-zA-Z]+' "${H}/out" | cut -d' ' -f2- | tr -d : | tr '\n' ',' ; }
destroyed() { grep -qx -- "destroy --name auto-agent-e2e --proxmox-token-file ${H}/op/pve" "${H}/log/destroy.calls"; }
no_secret() { [ ! -e "${H}/log/argv-leak" ] && ! grep -rqa SENTINEL "${H}/out" "${H}/err" "${H}/logs"; }

test_green_cycle() {
    echo "TEST: one command runs the whole cycle and exits 0 on a green Fire and a healthy status (AC 1, 2)"
    make_env
    status 1 "[${DRY}]"; status 2 "[${FLY},${DRY}]"; status 3 "[${GREEN},${DRY}]"
    run
    check "exit 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(cat "${H}/out" "${H}/err")"
    check "every stage in order" \
        '[ "$(stages)" = "preflight ok,fixture ok,setup ok,fire ok,status ok,collect ok,destroy ok," ]' "$(stages)"
    check "the verdict names the pass and the logs" 'out_has "e2e: PASS — a green Fire on auto-agent-e2e and a healthy /api/status, VM destroyed; logs in ${H}/logs"'
    check "the fire stage waited through the in-flight Fire to the green one" \
        'out_has "e2e: fire: ok — Fire f2 green: no work — queue empty, \$0.05 on claude-haiku-4-5" && [ "$(cat "${H}/log/status.n")" -ge 3 ]' "$(grep 'e2e: fire' "${H}/out")"
    check "setup ran unattended, setup-token, on the fixture, with the cheap model" \
        'grep -q -- "setup --provision proxmox --name auto-agent-e2e --unattended --auth-mode setup-token --repo acme/e2e-fixture --gh-login e2e-bot --gh-token-file ${H}/op/pat --claude-token-file ${H}/op/claude --set AUTO_AGENT_FIRE_MODEL=haiku --proxmox-token-file ${H}/op/pve" "${H}/log/cli.argv"' "$(cat "${H}/log/cli.argv")"
    check "the provision options were handed on, and the Host checkout named" \
        'grep -q -- "--proxmox-endpoint https://pve.invalid:8006/ --node pve1 --ipv4 10.0.0.50/24 --gateway 10.0.0.1 e2e-fixture$" "${H}/log/cli.argv"'
    check "the fixture repo holds the fixture, provider-lib beside its provider, no dangling \$schema" \
        'git --git-dir="${H}/fixture.git" cat-file -e main:verify/provider-lib.sh && git --git-dir="${H}/fixture.git" cat-file -e main:app/server.py && ! git --git-dir="${H}/fixture.git" show main:.auto-agent/harness.json | grep -q "\$schema"'
    check "the VM was destroyed" 'destroyed' "$(cat "${H}/log/destroy.calls" 2>/dev/null)"
    check "the logs are kept" \
        '(for f in setup.log destroy.log status.json status.final.json journal.log state.tgz summary.txt; do [ -s "${H}/logs/${f}" ] || exit 1; done) && tar -tzf "${H}/logs/state.tgz" | grep -q fires/f1.json'
    check "the summary holds every stage line and the verdict" '[ "$(grep -c "^e2e: " "${H}/logs/summary.txt")" -eq 8 ]' "$(cat "${H}/logs/summary.txt")"
    check "no secret on argv, stdout, stderr or in the logs" 'no_secret'

    # A second run: the fixture repo is already the fixture.
    rm -rf "${H}/logs"; : > "${H}/log/destroy.calls"; rm -f "${H}/log/status.n"
    run
    check "a re-run pushes nothing and passes again" \
        '[ "${RC}" -eq 0 ] && out_has "e2e: fixture: ok — already the fixture at main" && [ "$(git --git-dir="${H}/fixture.git" rev-list --count main)" -eq 1 ]' "rc=${RC} $(grep 'e2e: fixture' "${H}/out")"
}

test_failures_destroy_the_vm() {
    echo "TEST: every failure after the provision still collects and destroys, with logs kept (AC 2)"
    make_env
    status 1 "[${GREEN}]"
    E2E_ENV=(STUB_SETUP_RC=10); run; E2E_ENV=()
    check "a failed setup: exit 22 naming its failing stage" \
        '[ "${RC}" -eq 22 ] && out_has "e2e: setup: FAIL — setup exited 10: verify: FAIL — the Provider check failed"' "rc=${RC} $(cat "${H}/out")"
    check "and the VM is destroyed, the logs kept" 'destroyed && [ -s "${H}/logs/setup.log" ] && [ -s "${H}/logs/journal.log" ]'
    check "the verdict names the stage" 'out_has "e2e: FAIL — setup (exit 22); logs in ${H}/logs"' "$(tail -1 "${H}/out")"

    make_env
    status 1 "[${RED},${DRY}]"
    run
    check "a Fire that is not green: exit 23 saying why" \
        '[ "${RC}" -eq 23 ] && out_has "e2e: fire: FAIL — Fire f2 was not green: exit 1, outcome FAILED, phase claude: FAILED — exit 1"' "rc=${RC} $(cat "${H}/out")"
    check "and the VM is destroyed" 'destroyed'

    make_env
    status 1 "[${FLY},${DRY}]"
    run --fire-timeout 0
    check "no Fire finished in time: exit 23 with the Daemon's state" \
        '[ "${RC}" -eq 23 ] && out_has "e2e: fire: FAIL — no pickup Fire finished within 0s (daemon: queue_empty — queue empty)" && destroyed' "rc=${RC} $(cat "${H}/out")"

    make_env
    status 1 "[]"
    E2E_ENV=(STUB_STATUS_DOWN=1); run --fire-timeout 0; E2E_ENV=()
    check "a Dashboard that never answers: exit 23 saying so" \
        '[ "${RC}" -eq 23 ] && out_has "e2e: fire: FAIL — the Dashboard'"'"'s /api/status never answered within 0s" && destroyed' "rc=${RC} $(cat "${H}/out")"

    make_env
    status 1 "[${GREEN}]" '.daemon.unit.active = "failed" | .host.repo = "acme/other"'
    run
    check "an unhealthy status route: exit 24 naming each problem" \
        '[ "${RC}" -eq 24 ] && out_has "e2e: status: FAIL — the Daemon unit is failed; the repo is acme/other, not acme/e2e-fixture" && destroyed' "rc=${RC} $(cat "${H}/out")"

    make_env
    status 1 "[${GREEN}]"
    E2E_ENV=(STUB_DESTROY_RC=15); run; E2E_ENV=()
    check "a failed destroy wins over a pass: exit 25 with the teardown command" \
        '[ "${RC}" -eq 25 ] && out_has "e2e: destroy: FAIL — destroy exited 15: auto-agent-e2e may still be running on Proxmox" && out_has "--teardown --name auto-agent-e2e" && out_has "e2e: FAIL — destroy (exit 25)"' "rc=${RC} $(cat "${H}/out")"

    make_env
    status 1 "[${GREEN}]"
    E2E_ENV=(STUB_SETUP_RC=4); run; E2E_ENV=()
    check "setup failing before the provision: nothing to collect, destroy has nothing to do" \
        '[ "${RC}" -eq 22 ] && out_has "e2e: collect: skipped" && out_has "e2e: destroy: skipped — auto-agent-e2e: no terraform state"' "rc=${RC} $(cat "${H}/out")"
    check "still no secret anywhere" 'no_secret'

    make_env
    status 1 "[${GREEN}]"
    printf '#!/usr/bin/env bash\nexit 124\n' > "${H}/bin/timeout"; chmod +x "${H}/bin/timeout"
    E2E_ENV=(E2E_TIMEOUT_BIN="${H}/bin/timeout"); run --setup-timeout 60; E2E_ENV=()
    check "a hung setup: exit 22 after --setup-timeout" \
        '[ "${RC}" -eq 22 ] && out_has "e2e: setup: FAIL — setup did not finish within 60s (--setup-timeout)"' "rc=${RC} $(cat "${H}/out")"

    # A closed terminal mid-wait: the VM is still collected and destroyed.
    make_env
    status 1 "[${FLY}]"
    env -u PROXMOX_VE_API_TOKEN -u GH_TOKEN \
        STUB_LOG="${H}/log" STUB_STATUS="${H}/status" STUB_GH_TOKEN="${GH_SECRET}" \
        AUTO_AGENT_INVENTORY_DIR="${H}/inv" E2E_CLI="${H}/bin/cli" E2E_PROVISION_LIB="${H}/bin/destroy" \
        E2E_FIXTURE_URL="${H}/fixture.git" E2E_POLL_SECS=1 \
        GH_BIN="${H}/bin/gh" GIT_BIN="${H}/bin/git" SSH_BIN="${H}/bin/ssh" \
        bash "${E2E}" --fixture-repo acme/e2e-fixture --gh-login e2e-bot --gh-token-file "${H}/op/pat" \
        --claude-token-file "${H}/op/claude" --proxmox-token-file "${H}/op/pve" --log-dir "${H}/logs" \
        > "${H}/out" 2> "${H}/err" < /dev/null &
    local pid=$! i
    for i in $(seq 1 100); do [ -s "${H}/log/status.n" ] && break; sleep 0.1; done
    kill -HUP "${pid}"; wait "${pid}"; RC=$?
    check "a HUP mid-wait: exit 130, and the VM destroyed" \
        '[ "${RC}" -eq 130 ] && destroyed && out_has "e2e: FAIL — interrupted (exit 130)"' "rc=${RC} $(cat "${H}/out")"
}

test_preflight_and_fixture() {
    echo "TEST: preflight and the fixture refuse before any VM exists"
    make_env
    status 1 "[${GREEN}]"
    e2e --gh-login e2e-bot --log-dir "${H}/logs"
    check "missing inputs: usage naming each" \
        '[ "${RC}" -eq 2 ] && grep -q "missing --fixture-repo --gh-token-file --claude-token-file --proxmox-token-file" "${H}/err"' "rc=${RC} $(cat "${H}/err")"
    run --auth-mode login
    check "an option the test owns: usage" '[ "${RC}" -eq 2 ] && grep -q "is the test'"'"'s to set" "${H}/err"' "rc=${RC}"

    echo x > "${H}/inv/auto-agent-e2e.proxmox.tfstate"
    run
    check "a VM left from an earlier run: exit 20, the teardown command, no setup, no destroy" \
        '[ "${RC}" -eq 20 ] && out_has "--teardown --name auto-agent-e2e" && [ ! -e "${H}/log/cli.argv" ] && [ ! -e "${H}/log/destroy.calls" ]' "rc=${RC} $(cat "${H}/out")"
    rm -f "${H}/inv/auto-agent-e2e.proxmox.tfstate"

    E2E_ENV=(STUB_UNPUSHED=1); run; E2E_ENV=()
    check "an unpushed HEAD without --ref: exit 20" \
        '[ "${RC}" -eq 20 ] && out_has "is on no remote branch: push it, or pass --ref" && [ ! -e "${H}/log/cli.argv" ]' "rc=${RC} $(cat "${H}/out")"
    E2E_ENV=(STUB_UNPUSHED=1); run --ref v9; E2E_ENV=()
    check "--ref skips that check and reaches setup" '[ "${RC}" -eq 0 ] && grep -q -- "--ref v9" "${H}/log/cli.argv"' "rc=${RC} $(cat "${H}/out")"

    make_env
    status 1 "[${GREEN}]"
    E2E_ENV=(STUB_OPEN_ISSUES=2); run; E2E_ENV=()
    check "open issues in the fixture repo: exit 21 before any VM" \
        '[ "${RC}" -eq 21 ] && out_has "acme/e2e-fixture has 2 open issue(s) and 0 open PR(s)" && [ ! -e "${H}/log/cli.argv" ] && [ ! -e "${H}/log/destroy.calls" ]' "rc=${RC} $(cat "${H}/out")"

    make_env
    E2E_ENV=(STUB_GH_TOKEN=other); run; E2E_ENV=()
    check "a PAT that cannot read the fixture repo: exit 21" \
        '[ "${RC}" -eq 21 ] && out_has "cannot read acme/e2e-fixture as e2e-bot"' "rc=${RC} $(cat "${H}/out")"
}

test_teardown_and_keep() {
    echo "TEST: --keep-vm leaves the VM up; --teardown collects and destroys it"
    make_env
    status 1 "[${GREEN}]"
    run --keep-vm
    check "--keep-vm passes without a destroy and says how to tear down" \
        '[ "${RC}" -eq 0 ] && out_has "e2e: destroy: skipped — --keep-vm: auto-agent-e2e is still running" && [ ! -e "${H}/log/destroy.calls" ] && [ -f "${H}/inv/auto-agent-e2e.env" ]' "rc=${RC} $(cat "${H}/out")"
    rm -rf "${H}/logs"
    e2e --teardown --name auto-agent-e2e --proxmox-token-file "${H}/op/pve" --log-dir "${H}/logs"
    check "--teardown collects, destroys and exits 0" \
        '[ "${RC}" -eq 0 ] && [ "$(stages)" = "collect ok,destroy ok," ] && destroyed && [ ! -e "${H}/inv/auto-agent-e2e.env" ] && [ -s "${H}/logs/journal.log" ]' "rc=${RC} $(cat "${H}/out")"
}

test_runbook() {
    echo "TEST: the runbook says where credentials come from and what each stage's failure means (AC 3)"
    local rb="${ROOT_DIR}/docs/runbooks/e2e-proxmox.md"
    check "the runbook exists" '[ -f "${rb}" ]'
    local s
    for s in preflight fixture setup fire status collect destroy; do
        check "it explains a ${s} failure" 'grep -Eq "^\| \`${s}\` \|" "${rb}"'
    done
    for s in "Proxmox API token" "setup-token" "classic PAT" "--teardown"; do
        check "it covers ${s}" 'grep -qF -- "${s}" "${rb}"'
    done
    check "every exit code the script documents is in the runbook" \
        '(for c in 20 21 22 23 24 25; do grep -Eq "\| ([0-9]+ / )?${c} \|" "${rb}" || exit 1; done)'
}

test_green_cycle
test_failures_destroy_the_vm
test_preflight_and_fixture
test_teardown_and_keep
test_runbook

echo
echo "proxmox-e2e.test.sh: ${TESTS_RUN} run, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
