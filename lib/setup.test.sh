#!/usr/bin/env bash
# Tests for lib/setup.sh (issue #38)
#
# Run: bash lib/setup.test.sh
#
# Strategy: drive `bin/auto-agent setup` and `check` end to end on a
# throwaway Host: a copy of the fixture Target Project as a git checkout, a
# Host env and State dir under a scratch HOME, and recording stubs for every
# binary that would touch the machine or the network (gh, claude, sudo,
# systemctl, ansible-playbook, the dry-run Fire). The Ansible stub behaves like
# the real run where Setup depends on it: it writes the Host env content it
# was handed (0600) and reports changed=0 when handed the same vars twice.
# The Provider check and the bootstrap issue run for real against the copy.
# The role itself is covered by `ansible-playbook --syntax-check`; its real
# run belongs to the end-to-end Setup test on a VM (#42).

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

GH_SECRET="ghp_SENTINELghtoken0123456789abcdef"
CLAUDE_SECRET="sk-ant-oat01-SENTINELclaudetoken987"

# make_host : a scratch Host in $H — stubs, a fixture checkout, a HOME
make_host() {
    H="$(mktemp -d)"
    mkdir -p "${H}/home" "${H}/bin" "${H}/log" "${H}/systemd-run" "${H}/remote"
    printf 'ID=ubuntu\nVERSION_ID="24.04"\nPRETTY_NAME="Ubuntu 24.04.3 LTS"\n' > "${H}/os-release"
    printf '%s\n' "${GH_SECRET}" > "${H}/gh-token"
    printf '%s\n' "${CLAUDE_SECRET}" > "${H}/claude-token"

    # The fixture as a Target Project checkout of its own. Its provider sources
    # provider-lib by a path two levels up, so the copy gets that symlink.
    mkdir -p "${H}/src/proj"
    cp -R "${FIXTURE}" "${H}/src/proj/target"
    ln -s "${ROOT_DIR}/plugin/providers" "${H}/src/providers"
    T="${H}/src/proj/target"
    git init -q --bare "${H}/remote/widget.git"
    git -C "${T}" init -q -b main
    git -C "${T}" -c user.name=t -c user.email=t@t add -A
    git -C "${T}" -c user.name=t -c user.email=t@t commit -qm init
    git -C "${T}" remote add origin "${H}/remote/widget.git"
    git -C "${T}" push -q origin main

    # git: the origin reads as a GitHub remote; everything else is real git.
    cat > "${H}/bin/git" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do case "${a}" in *SENTINEL*) echo "SECRET IN ARGV: git" >> "${STUB_LOG}/argv-leak" ;; esac; done
if [ "${3:-}" = "remote" ] && [ "${4:-}" = "get-url" ]; then echo "https://github.com/acme/widget.git"; exit 0; fi
exec /usr/bin/git "$@"
EOF

    # gh: the machine user is `widget-bot`; GH_TOKEN must be the sentinel.
    cat > "${H}/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/gh.calls"
for a in "$@"; do case "${a}" in *SENTINEL*) echo "SECRET IN ARGV: gh" >> "${STUB_LOG}/argv-leak" ;; esac; done
authed() { [ "${GH_TOKEN:-}" = "${STUB_GH_TOKEN}" ]; }
case "$1 $2" in
    "api user") authed || exit 1; echo "${STUB_GH_LOGIN:-widget-bot}" ;;
    "api -i")   authed || exit 1; printf 'HTTP/2.0 200 OK\r\n'
                scopes="${STUB_GH_SCOPES-project, repo, workflow}"
                [ -n "${scopes}" ] && printf 'X-Oauth-Scopes: %s\r\n' "${scopes}"
                printf '\r\n{"login":"x"}\n' ;;
    "api repos/acme/widget") authed || exit 1; echo "${STUB_GH_ADMIN:-true}" ;;
    "repo view") echo main ;;
    "repo clone") exit 1 ;;
    "pr list") cat "${STUB_LOG}/open-pr" 2>/dev/null; true ;;
    "pr create") authed || exit 1; echo 41 > "${STUB_LOG}/open-pr"; echo "https://github.com/acme/widget/pull/41" ;;
    "issue list") echo '[]' ;;
    "issue create") authed || exit 1; echo "https://github.com/acme/widget/issues/42" ;;
    *) echo "gh stub: unhandled: $*" >&2; exit 1 ;;
esac
EOF

    # claude: logged in with /login unless a setup-token token is exported.
    cat > "${H}/bin/claude" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do case "${a}" in *SENTINEL*) echo "SECRET IN ARGV: claude" >> "${STUB_LOG}/argv-leak" ;; esac; done
[ "${STUB_CLAUDE_LOGGED_OUT:-0}" = "1" ] && { echo '{"loggedIn":false}'; exit 1; }
if [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then echo '{"loggedIn":true,"authMethod":"oauth_token"}'
else echo '{"loggedIn":true,"authMethod":"claude.ai"}'; fi
EOF

    cat > "${H}/bin/sudo" <<'EOF'
#!/usr/bin/env bash
[ "$1" = "-n" ] && shift
exec "$@"
EOF

    # systemctl: units remembered in a state file; every write logged.
    cat > "${H}/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
state="${STUB_LOG}/units"; touch "${state}"
case "$1" in
    is-enabled) grep -qx "enabled $2" "${state}" && { echo enabled; exit 0; }; echo disabled; exit 1 ;;
    is-active)  grep -qx "active $2" "${state}" && { echo active; exit 0; }; echo inactive; exit 3 ;;
    enable) echo "enable $*" >> "${STUB_LOG}/systemctl.writes"; shift
            for u in "$@"; do [ "$u" = --now ] && continue; echo "enabled $u" >> "${state}"; echo "active $u" >> "${state}"; done ;;
    restart) echo "restart $2" >> "${STUB_LOG}/systemctl.writes" ;;
    *) echo "systemctl stub: $*" >> "${STUB_LOG}/systemctl.writes" ;;
esac
EOF

    # ansible-playbook: checks the vars file, writes the Host env like the
    # role does, and is idempotent over identical vars.
    cat > "${H}/bin/ansible-playbook" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/ansible.calls"
for a in "$@"; do case "${a}" in *SENTINEL*) echo "SECRET IN ARGV: ansible" >> "${STUB_LOG}/argv-leak" ;; esac; done
vars=""; while [ $# -gt 0 ]; do [ "$1" = -e ] && vars="${2#@}"; shift; done
echo "${vars}" > "${STUB_LOG}/ansible.varsfile"
stat -c %a "${vars}" > "${STUB_LOG}/ansible.varsmode"
jq 'del(.aa_host_env_content)' "${vars}" > "${STUB_LOG}/ansible.vars"
[ "${STUB_ANSIBLE_FAIL:-0}" = "1" ] && { echo "TASK [host : Base packages] fatal"; exit 2; }
dest="$(jq -r .aa_host_env_path "${vars}")"
mkdir -p "$(dirname "${dest}")"
( umask 077; jq -j .aa_host_env_content "${vars}" > "${dest}.new" ) && mv "${dest}.new" "${dest}"
sum="$(sha256sum "${vars}" | cut -d' ' -f1)"
changed=0; [ "$(cat "${STUB_LOG}/ansible.last" 2>/dev/null)" = "${sum}" ] || changed=7
echo "${sum}" > "${STUB_LOG}/ansible.last"
printf 'PLAY RECAP *********\nlocalhost : ok=20 changed=%s unreachable=0 failed=0 skipped=3\n' "${changed}"
EOF
    chmod +x "${H}/bin/"*
}

# run_setup <args...> : `bin/auto-agent setup` on the scratch Host; output in
# ${H}/out and ${H}/err, exit code in RC
run_cli() {
    env -u AUTO_AGENT_TARGET_DIR -u AUTO_AGENT_STATE_DIR -u GH_TOKEN -u DAEMON_GH_LOGIN -u CLAUDE_AUTH_MODE \
        -u CLAUDE_CODE_OAUTH_TOKEN -u ANTHROPIC_API_KEY -u AUTO_AGENT_HOST_USER -u HARNESS_CONFIG_JSON -u DISPLAY \
        HOME="${H}/home" XDG_STATE_HOME="${H}/home/.local/state" AUTO_AGENT_HOST_ENV="${H}/home/.config/auto-agent/env" \
        STUB_LOG="${H}/log" STUB_GH_TOKEN="${GH_SECRET}" \
        GH_BIN="${H}/bin/gh" GIT_BIN="${H}/bin/git" CLAUDE_BIN="${H}/bin/claude" \
        ANSIBLE_PLAYBOOK_BIN="${H}/bin/ansible-playbook" SETUP_SUDO_BIN="${H}/bin/sudo" SYSTEMCTL_BIN="${H}/bin/systemctl" \
        SETUP_OS_RELEASE="${H}/os-release" SETUP_SYSTEMD_RUN_DIR="${H}/systemd-run" SETUP_ARCH=x86_64 \
        SETUP_NET_PROBE_CMD=true SETUP_DOCTOR_COMMANDS="bash jq" \
        SETUP_FIRE_CMD="echo 'fire: dry-run'; echo 'afk-pickup: would-pick #7 (label-only pick)'; true" \
        AUTO_AGENT_SETUP_UNATTENDED=1 \
        "$@" > "${H}/out" 2> "${H}/err" < /dev/null
    RC=$?
}
run_setup() { run_cli bash "${CLI}" setup "$@"; }
# with <VAR=value...> -- <setup args...>
setup_with() {
    local envs=()
    while [ "$1" != "--" ]; do envs+=("$1"); shift; done; shift
    run_cli env "${envs[@]}" bash "${CLI}" setup "$@"
}

FIRST_RUN_ARGS=(--gh-login widget-bot --gh-token-file "__H__/gh-token")
first_run() { run_setup "${FIRST_RUN_ARGS[@]/__H__/${H}}" "${T}"; }
hostenv() { cat "${H}/home/.config/auto-agent/env"; }
out_has() { grep -qF -- "$1" "${H}/out"; }

test_unattended_setup() {
    echo "TEST: unattended setup on the fixture ends enabled with a green dry-run Fire (AC 1)"
    make_host
    first_run
    check "setup exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out") $(tail -5 "${H}/err")"
    local s
    for s in baseline doctor github claude config configure extension verify enable bootstrap; do
        check "stage ${s} reported, in order" 'grep -Eq "^setup: ${s}: (ok|changed|skipped) — " "${H}/out"' "$(grep "^setup: ${s}" "${H}/out")"
    done
    check "stages ran in the fixed order" \
        '[ "$(grep -Eo "^setup: [a-z]+:" "${H}/out" | tr -d : | cut -d" " -f2 | tr "\n" " ")" = "baseline doctor github claude config configure extension verify enable bootstrap " ]' \
        "$(grep -Eo "^setup: [a-z]+:" "${H}/out" | tr '\n' ' ')"
    check "both units enabled and started" 'grep -qx "enabled auto-agent-daemon.service" "${H}/log/units" && grep -qx "enabled auto-agent-dashboard.service" "${H}/log/units"'
    check "the verify stage ran one dry-run Fire, green" 'out_has "check: fire: ok — dry-run Fire green: afk-pickup: would-pick #7"'
    check "the Provider check ran for real and passed" 'grep -Eq "^check: provider: ok — provider-check: PASS" "${H}/out"' "$(grep 'check: provider' "${H}/out")"
    check "the bootstrap stage asks and owes nothing (hermetic tier declared)" 'grep -Eq "^setup: bootstrap: ok — no issue owed" "${H}/out"' "$(grep 'setup: bootstrap' "${H}/out")"
    check "first run reports what changed" 'grep -Eq "^setup: done — [0-9]+ changed: .*configure.*enable" "${H}/out"'
    check "configure derived display+browser needs from the fixture Surfaces, no docker" \
        '[ "$(jq -c .aa_needs "${H}/log/ansible.vars")" = "{\"browser\":true,\"electron\":false,\"docker\":false,\"display\":true}" ]' "$(jq -c .aa_needs "${H}/log/ansible.vars")"
    check "ansible runs the in-repo playbook locally" 'grep -q -- "-i localhost, -c local -e @.* ${ROOT_DIR}/infra/ansible/configure.yml" "${H}/log/ansible.calls"'
    check "the Host env names the Target Project, the machine user, the mode and the display" \
        'hostenv | grep -qx "AUTO_AGENT_TARGET_DIR=${T}" && hostenv | grep -qx "DAEMON_GH_LOGIN=widget-bot" && hostenv | grep -qx "CLAUDE_AUTH_MODE=login" && hostenv | grep -qx "DISPLAY=:99" && hostenv | grep -qx "AUTO_AGENT_DASHBOARD_BIND=127.0.0.1"'
    check "the checkout commits as the machine user" '[ "$(git -C "${T}" config --get user.email)" = "widget-bot@users.noreply.github.com" ]'
}

test_rerun_converges() {
    echo "TEST: re-running setup changes nothing and reports converged; check runs verify alone (AC 2)"
    make_host
    first_run
    : > "${H}/log/systemctl.writes"; : > "${H}/log/gh.calls"
    local env_before; env_before="$(hostenv | sha256sum)"
    # No token or login on the second run: both are read back from the Host env.
    run_setup "${T}"
    check "second run exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out") $(tail -3 "${H}/err")"
    check "second run reports converged" 'out_has "setup: converged — nothing changed"' "$(tail -3 "${H}/out")"
    check "no stage reports changed" '! grep -Eq "^setup: [a-z]+: changed" "${H}/out"' "$(grep ': changed' "${H}/out")"
    check "configure reports converged (Ansible changed=0)" 'grep -Eq "^setup: configure: ok — converged" "${H}/out"'
    check "no unit was enabled or restarted again" '[ ! -s "${H}/log/systemctl.writes" ]' "$(cat "${H}/log/systemctl.writes")"
    check "the Host env is byte-identical" '[ "$(hostenv | sha256sum)" = "${env_before}" ]'
    check "no PR or issue was opened" '! grep -Eq "^(pr|issue) create" "${H}/log/gh.calls"'

    local ansible_before; ansible_before="$(wc -l < "${H}/log/ansible.calls")"
    run_cli bash "${CLI}" check
    check "check exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(cat "${H}/out") $(tail -3 "${H}/err")"
    check "check prints the verify items, reading the target from the Host env" \
        'out_has "check: host-env: ok" && out_has "check: github: ok" && out_has "check: claude: ok" && out_has "check: config: ok" && out_has "check: provider: ok" && out_has "check: fire: ok"' "$(cat "${H}/out")"
    check "check runs no other stage" '! grep -q "^setup:" "${H}/out" && [ "$(wc -l < "${H}/log/ansible.calls")" -eq "${ansible_before}" ] && [ ! -s "${H}/log/systemctl.writes" ]'
}

test_baseline_fails_first() {
    echo "TEST: the baseline assertion fails first on a wrong distribution (AC 3)"
    make_host
    printf 'ID=debian\nVERSION_ID="12"\n' > "${H}/os-release"
    # Doctor would fail too; the distribution is still the first failure.
    setup_with SETUP_DOCTOR_COMMANDS="no-such-command-xyz" SETUP_NET_PROBE_CMD=false -- \
        --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "exit 3 (baseline)" '[ "${RC}" -eq 3 ]' "rc=${RC}"
    check "the first and only stage line is the distribution failure" \
        '[ "$(grep -c "^setup:" "${H}/out")" -eq 1 ] && grep -q "^setup: baseline: FAIL — this Host runs debian 12; the reference Host is Ubuntu 24.04 LTS" "${H}/out"' "$(cat "${H}/out")"
    check "nothing was touched: no gh call, no Ansible run, no Host env" \
        '[ ! -e "${H}/log/gh.calls" ] && [ ! -e "${H}/log/ansible.calls" ] && [ ! -e "${H}/home/.config/auto-agent/env" ]'

    make_host
    printf 'ID=ubuntu\nVERSION_ID="22.04"\n' > "${H}/os-release"
    first_run
    check "Ubuntu 22.04 fails the baseline too" '[ "${RC}" -eq 3 ] && out_has "ubuntu 22.04"' "$(cat "${H}/out")"

    make_host
    setup_with SETUP_ARCH=riscv64 -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "an unsupported architecture fails the baseline" '[ "${RC}" -eq 3 ] && out_has "architecture riscv64"'
    make_host
    rmdir "${H}/systemd-run"
    first_run
    check "no systemd fails the baseline" '[ "${RC}" -eq 3 ] && out_has "systemd is not the init system"'
    make_host
    setup_with SETUP_SUDO_BIN=false -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "no passwordless sudo fails the baseline" '[ "${RC}" -eq 3 ] && out_has "passwordless sudo"'
    make_host
    setup_with SETUP_NET_PROBE_CMD=false -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "no outbound internet fails the baseline" '[ "${RC}" -eq 3 ] && out_has "no outbound internet"'
}

test_secrets() {
    echo "TEST: the Host env is 0600, holds every secret, and no secret reaches output or logs (AC 4)"
    make_host
    run_setup --gh-login widget-bot --gh-token-file "${H}/gh-token" \
        --auth-mode setup-token --claude-token-file "${H}/claude-token" "${T}"
    check "setup-token setup exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out")"
    local f="${H}/home/.config/auto-agent/env"
    check "the Host env is mode 0600" '[ "$(stat -c %a "${f}")" = 600 ]' "$(stat -c %a "${f}")"
    check "the Host env holds GH_TOKEN and CLAUDE_CODE_OAUTH_TOKEN" \
        'grep -qx "GH_TOKEN=${GH_SECRET}" "${f}" && grep -qx "CLAUDE_CODE_OAUTH_TOKEN=${CLAUDE_SECRET}" "${f}" && grep -qx "CLAUDE_AUTH_MODE=setup-token" "${f}"'
    check "no secret on stdout or stderr" '! grep -q SENTINEL "${H}/out" "${H}/err"'
    check "no secret in the State dir (logs included)" '! grep -rq SENTINEL "${H}/home/.local/state"' "$(grep -rl SENTINEL "${H}/home/.local/state")"
    check "no secret on any command line" '[ ! -e "${H}/log/argv-leak" ]' "$(cat "${H}/log/argv-leak" 2>/dev/null)"
    check "the Ansible vars file was 0600 and is gone" '[ "$(cat "${H}/log/ansible.varsmode")" = 600 ] && [ ! -e "$(cat "${H}/log/ansible.varsfile")" ]'
    check "the secrets file is the only place on disk the token landed" \
        '[ "$(grep -rl SENTINEL "${H}/home" | tr "\n" " ")" = "${f} " ]' "$(grep -rl SENTINEL "${H}/home")"

    # Back to /login: the setup-token token is dropped, not left to contradict the mode.
    run_setup --auth-mode login "${T}"
    check "switching to login drops CLAUDE_CODE_OAUTH_TOKEN" '[ "${RC}" -eq 0 ] && ! grep -q CLAUDE_CODE_OAUTH_TOKEN "${f}" && grep -qx CLAUDE_AUTH_MODE=login "${f}"' "rc=${RC}"

    run_cli bash "${CLI}" check
    check "check stays silent about secrets" '[ "${RC}" -eq 0 ] && ! grep -q SENTINEL "${H}/out" "${H}/err"'
    chmod 644 "${f}"
    run_cli bash "${CLI}" check
    check "check fails a Host env that is not 0600" '[ "${RC}" -eq 10 ] && out_has "check: host-env: FAIL — ${f} is mode 644"' "$(cat "${H}/out")"
}

test_prompts_have_overrides() {
    echo "TEST: every prompt has a flag or an environment override"
    make_host
    run_setup "${T}"
    check "no login and no terminal: exit 5 naming the flag and the env key" \
        '[ "${RC}" -eq 5 ] && out_has "pass --gh-login or set AUTO_AGENT_SETUP_GH_LOGIN"' "$(cat "${H}/out")"
    run_setup --gh-login widget-bot "${T}"
    check "no PAT: exit 5 naming the flag and the env key" \
        '[ "${RC}" -eq 5 ] && out_has "pass --gh-token-file or set AUTO_AGENT_SETUP_GH_TOKEN"'
    setup_with AUTO_AGENT_SETUP_GH_LOGIN=widget-bot AUTO_AGENT_SETUP_GH_TOKEN="${GH_SECRET}" -- --auth-mode setup-token "${T}"
    check "no setup-token token: exit 6 naming the flag and the env key" \
        '[ "${RC}" -eq 6 ] && out_has "pass --claude-token-file or set AUTO_AGENT_SETUP_CLAUDE_TOKEN"'
    setup_with AUTO_AGENT_SETUP_GH_LOGIN=widget-bot AUTO_AGENT_SETUP_GH_TOKEN="${GH_SECRET}" \
        AUTO_AGENT_SETUP_AUTH_MODE=setup-token AUTO_AGENT_SETUP_CLAUDE_TOKEN="${CLAUDE_SECRET}" AUTO_AGENT_SETUP_TARGET="${T}" --
    check "the environment alone drives a whole run" '[ "${RC}" -eq 0 ] && ! grep -q SENTINEL "${H}/out" "${H}/err"' "rc=${RC} $(tail -3 "${H}/out")"
    run_setup --set 'bad key=1' "${T}"
    check "--set refuses an invalid key" '[ "${RC}" -eq 2 ]'
}

test_identity_failures() {
    echo "TEST: the github and claude stages refuse the wrong identity"
    make_host
    setup_with STUB_GH_LOGIN=benjr70 -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "a PAT for another account: exit 5" '[ "${RC}" -eq 5 ] && out_has "the PAT belongs to benjr70, not the machine user widget-bot"' "$(cat "${H}/out")"
    setup_with STUB_GH_SCOPES="repo, workflow" -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "a PAT without the project scope: exit 5" '[ "${RC}" -eq 5 ] && out_has "lacks the project scope"'
    setup_with STUB_GH_SCOPES="" -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "a fine-grained PAT (no scopes header): exit 5" '[ "${RC}" -eq 5 ] && out_has "classic PAT"'
    setup_with STUB_GH_ADMIN=false -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "a machine user without admin: exit 5" '[ "${RC}" -eq 5 ] && out_has "is not an admin collaborator on acme/widget"'
    setup_with STUB_CLAUDE_LOGGED_OUT=1 -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "claude not logged in: exit 6 naming the precondition" '[ "${RC}" -eq 6 ] && out_has "run \`claude auth login\` here first"' "$(cat "${H}/out")"
    run_setup --gh-login widget-bot --gh-token-file "${H}/gh-token" --auth-mode api-key "${T}"
    check "api-key refuses: exit 6" '[ "${RC}" -eq 6 ] && out_has "refuses to start until spend pacing exists"'
    check "no failed identity wrote a Host env" '[ ! -e "${H}/home/.config/auto-agent/env" ]'
}

test_config_scaffold() {
    echo "TEST: a missing Harness config is proposed as a machine-user PR, once"
    make_host
    git -C "${T}" rm -rq .auto-agent
    git -C "${T}" -c user.name=t -c user.email=t@t commit -qm "no config"
    git -C "${T}" push -q origin main
    first_run
    check "setup exits 0 with the config pending" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -8 "${H}/out") $(tail -3 "${H}/err")"
    check "the config stage opened the PR" 'grep -Eq "^setup: config: changed — .*proposed .auto-agent/harness.json in 41" "${H}/out"' "$(grep 'setup: config' "${H}/out")"
    check "gh pr create targets the default branch from the fixed branch" \
        'grep -q "^pr create --repo acme/widget --base main --head auto-agent/harness-config" "${H}/log/gh.calls"'
    check "the branch carries a schema-valid harness.json committed by the machine user" \
        'git -C "${H}/remote/widget.git" show auto-agent/harness-config:.auto-agent/harness.json | jq -e ".pick.labels" >/dev/null && [ "$(git -C "${H}/remote/widget.git" log -1 --format=%ae auto-agent/harness-config)" = "widget-bot@users.noreply.github.com" ]'
    check "the checkout is left on its default branch" '[ "$(git -C "${T}" symbolic-ref --short HEAD)" = main ] && [ ! -e "${T}/.auto-agent" ]'
    check "verify skips the Fire while the config waits" 'out_has "check: fire: skipped — waiting for Harness config"'
    check "the Daemon is enabled anyway (ADR 0009)" 'grep -qx "enabled auto-agent-daemon.service" "${H}/log/units"'
    check "bootstrap waits for the config" 'grep -q "^setup: bootstrap: skipped — waiting for Harness config" "${H}/out"'

    : > "${H}/log/gh.calls"
    run_setup "${T}"
    check "re-run finds the open PR and opens no second one" \
        '[ "${RC}" -eq 0 ] && out_has "setup: config: ok — waiting for Harness config: PR #41 is open" && ! grep -q "^pr create" "${H}/log/gh.calls"' "$(grep 'setup: config' "${H}/out")"
    check "re-run with the PR open converges" 'out_has "setup: converged"' "$(tail -3 "${H}/out")"

    # A draft from the skill is what gets proposed.
    make_host
    git -C "${T}" rm -rq .auto-agent
    git -C "${T}" -c user.name=t -c user.email=t@t commit -qm "no config"
    git -C "${T}" push -q origin main
    jq '.host = {docker: true}' "${FIXTURE}/.auto-agent/harness.json" > "${H}/draft.json"
    run_setup --gh-login widget-bot --gh-token-file "${H}/gh-token" --config "${H}/draft.json" "${T}"
    check "--config's draft is the proposed file, and its needs drive configure" \
        '[ "${RC}" -eq 0 ] && git -C "${H}/remote/widget.git" show auto-agent/harness-config:.auto-agent/harness.json | jq -e ".host.docker == true" >/dev/null && [ "$(jq -r .aa_needs.docker "${H}/log/ansible.vars")" = true ]' "rc=${RC}"
    make_host
    echo '{"commit_scopes": []}' > "${H}/bad.json"
    git -C "${T}" rm -rq .auto-agent; git -C "${T}" -c user.name=t -c user.email=t@t commit -qm x; git -C "${T}" push -q origin main
    run_setup --gh-login widget-bot --gh-token-file "${H}/gh-token" --config "${H}/bad.json" "${T}"
    check "an invalid draft fails the config stage: exit 7" '[ "${RC}" -eq 7 ] && out_has "the draft Harness config does not validate"'
}

test_host_env_keeps_operator_lines() {
    echo "TEST: the Host env keeps what the operator added; --set overrides"
    make_host
    mkdir -p "${H}/home/.config/auto-agent"
    printf '# my notes\nAUTO_AGENT_MODEL_PRIMARY=opus\nAUTO_AGENT_DASHBOARD_PORT=9000\n' > "${H}/home/.config/auto-agent/env"
    run_setup --gh-login widget-bot --gh-token-file "${H}/gh-token" --set AUTO_AGENT_MEMORY_MAX=6G --set DISPLAY=:42 "${T}"
    check "setup exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out")"
    check "operator keys and comments survive" 'hostenv | grep -qx "AUTO_AGENT_MODEL_PRIMARY=opus" && hostenv | grep -qx "# my notes"'
    check "an operator's Dashboard port is not reset to the default" 'hostenv | grep -qx "AUTO_AGENT_DASHBOARD_PORT=9000" && [ "$(hostenv | grep -c "^AUTO_AGENT_DASHBOARD_PORT=")" -eq 1 ]'
    check "--set lands, and DISPLAY feeds the Xvfb unit" 'hostenv | grep -qx "AUTO_AGENT_MEMORY_MAX=6G" && [ "$(jq -r .aa_display "${H}/log/ansible.vars")" = ":42" ]'
    check "the summary names the operator's Dashboard port" 'out_has "Dashboard: http://127.0.0.1:9000/"'
    run_setup "${T}"
    check "a re-run without --set keeps the --set values and converges" \
        '[ "${RC}" -eq 0 ] && hostenv | grep -qx "AUTO_AGENT_MEMORY_MAX=6G" && hostenv | grep -qx "DISPLAY=:42" && out_has "setup: converged"' "$(tail -3 "${H}/out")"
}

test_extension() {
    echo "TEST: the Host extension runs after configure, without secrets; a failure stops Setup"
    make_host
    cat > "${T}/.auto-agent/host-extension" <<'EOF'
#!/usr/bin/env bash
echo "ext: $1 in $(pwd) token=${GH_TOKEN:-none}"
EOF
    chmod +x "${T}/.auto-agent/host-extension"
    first_run
    check "setup exits 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(tail -5 "${H}/out")"
    check "the extension ran with 'setup' in the checkout, its output prefixed" 'out_has "setup: extension: | ext: setup in ${T} token=none"' "$(grep extension "${H}/out")"
    check "the extension stage is ok" 'out_has "setup: extension: ok"'
    printf '#!/usr/bin/env bash\nexit 3\n' > "${T}/.auto-agent/host-extension"
    run_setup "${T}"
    check "a failing extension: exit 9, and nothing is enabled after it" \
        '[ "${RC}" -eq 9 ] && out_has "setup: extension: FAIL — host-extension exited 3" && ! grep -q "^setup: verify" "${H}/out"'
}

test_verify_gates_enable() {
    echo "TEST: a failed verify stops Setup before the units are enabled"
    make_host
    setup_with SETUP_FIRE_CMD="echo boom; false" -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "exit 10 (verify)" '[ "${RC}" -eq 10 ]' "rc=${RC}"
    check "the Fire check names the failure" 'out_has "check: fire: FAIL — dry-run Fire exited 1"'
    check "no unit was enabled" '[ ! -e "${H}/log/units" ] || ! grep -q enabled "${H}/log/units"'
    setup_with STUB_ANSIBLE_FAIL=1 -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "a failed configure: exit 8 naming the log" '[ "${RC}" -eq 8 ] && out_has "setup: configure: FAIL — ansible-playbook exited 2 (log: "'
}

test_one_daemon_per_host() {
    echo "TEST: one Daemon per Target Project per Host"
    make_host
    first_run
    mkdir -p "${H}/src/other"
    run_setup "${H}/src/other"
    check "a second Target Project fails the baseline" '[ "${RC}" -eq 3 ] && out_has "this Host already serves ${T}"' "$(cat "${H}/out")"
}

test_doctor() {
    echo "TEST: doctor names each missing prerequisite with its install command, never installs"
    make_host
    setup_with SETUP_DOCTOR_COMMANDS="bash ansible-playbook-missing-xyz" -- --gh-login widget-bot --gh-token-file "${H}/gh-token" "${T}"
    check "exit 4" '[ "${RC}" -eq 4 ]' "rc=${RC}"
    check "the missing command and its install line" 'out_has "setup: doctor: missing ansible-playbook-missing-xyz — install it with: sudo apt-get install -y ansible-playbook-missing-xyz"'
}

test_base_needs() {
    echo "TEST: base needs follow the Surfaces and host.docker"
    # shellcheck source=setup.sh
    . "${ROOT_DIR}/lib/setup.sh"
    local d; d="$(mktemp -d)"
    echo '{"surfaces":{"cli":{"kind":"cli","url_key":"X","paths":["a"]}}}' > "${d}/a.json"
    check "a cli-only project needs nothing" '[ "$(setup_base_needs "${d}/a.json")" = "{\"browser\":false,\"electron\":false,\"docker\":false,\"display\":false}" ]'
    echo '{"surfaces":{"app":{"kind":"electron","url_key":"X","paths":["a"],"launcher":"bin/app"}},"host":{"docker":true}}' > "${d}/b.json"
    check "an electron Surface needs a display; host.docker needs Docker" '[ "$(setup_base_needs "${d}/b.json")" = "{\"browser\":false,\"electron\":true,\"docker\":true,\"display\":true}" ]'
    check "the electron Surface gets an AppArmor grant for its launcher and binary" \
        '[ "$(setup_electron_profiles "${d}/b.json" /srv/proj)" = "{\"launchers\":[\"/srv/proj/bin/app\"],\"binaries\":[\"/srv/proj/**/node_modules/electron/dist/electron\"]}" ]' \
        "$(setup_electron_profiles "${d}/b.json" /srv/proj)"
    echo '{"surfaces":{"a":{"kind":"electron","url_key":"X","paths":["a"],"launcher":"bin/app"},"b":{"kind":"electron","url_key":"Y","paths":["b"],"launcher":"bin/app"},"c":{"kind":"electron","url_key":"Z","paths":["c"]}}}' > "${d}/d.json"
    check "Surfaces sharing a launcher and binary get each grant once" \
        '[ "$(setup_electron_profiles "${d}/d.json" /srv/proj | jq -c "[.launchers, .binaries | length]")" = "[1,1]" ]'
    check "no electron Surface, no grant" '[ "$(setup_electron_profiles "${d}/a.json" /srv/proj)" = "{\"launchers\":[],\"binaries\":[]}" ]'
    echo '{}' > "${d}/c.json"
    check "no surfaces block at all needs nothing" '[ "$(setup_base_needs "${d}/c.json" | jq -r .display)" = false ]'
}

test_playbook_parses() {
    echo "TEST: the configure playbook parses"
    if ! command -v ansible-playbook >/dev/null 2>&1; then
        echo "  SKIP: ansible-playbook not installed"; return
    fi
    local out
    out="$(ANSIBLE_CONFIG="${ROOT_DIR}/infra/ansible/ansible.cfg" ansible-playbook -i localhost, -c local \
        "${ROOT_DIR}/infra/ansible/configure.yml" --syntax-check 2>&1)"
    check "ansible-playbook --syntax-check passes" '[ $? -eq 0 ] || printf "%s" "${out}" | grep -q "^playbook: "' "${out}"
    local d; d="$(mktemp -d)"
    cat > "${d}/render.yml" <<YML
- hosts: all
  gather_facts: false
  vars:
    aa_host_user: agent
    aa_display: ":99"
    aa_xvfb_screen: 1920x1080x24
    aa_electron: { launchers: [/srv/proj/bin/app], binaries: ["/srv/proj/**/node_modules/electron/dist/electron"] }
  tasks:
    - ansible.builtin.template: { src: "${ROOT_DIR}/infra/ansible/roles/host/templates/auto-agent-xvfb.service.j2", dest: "${d}/auto-agent-xvfb.service" }
    - ansible.builtin.template: { src: "${ROOT_DIR}/infra/ansible/roles/host/templates/auto-agent-electron.apparmor.j2", dest: "${d}/auto-agent-electron" }
YML
    ansible-playbook -i localhost, -c local "${d}/render.yml" > "${d}/log" 2>&1
    check "the role's templates render" 'grep -q "ExecStart=/usr/bin/Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp" "${d}/auto-agent-xvfb.service" && grep -q "^profile /srv/proj/bin/app flags=(unconfined)" "${d}/auto-agent-electron"' "$(tail -5 "${d}/log")"
    if command -v apparmor_parser >/dev/null 2>&1; then
        check "the AppArmor grant parses" 'apparmor_parser -Q -K -I /etc/apparmor.d "${d}/auto-agent-electron" 2>/dev/null'
    else
        echo "  SKIP: apparmor_parser not installed"
    fi
    check "the Host env task is no_log" \
        'awk "/name: The Host env \\(0600\\)/{f=1} f&&/no_log: true/{print; exit}" "${ROOT_DIR}/infra/ansible/roles/host/tasks/main.yml" | grep -q no_log'
}

test_unattended_setup
test_rerun_converges
test_baseline_fails_first
test_secrets
test_prompts_have_overrides
test_identity_failures
test_config_scaffold
test_host_env_keeps_operator_lines
test_extension
test_verify_gates_enable
test_one_daemon_per_host
test_doctor
test_base_needs
test_playbook_parses

echo
echo "setup.test.sh: ${TESTS_RUN} run, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
