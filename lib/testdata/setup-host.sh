#!/usr/bin/env bash
# A scratch Host for the Setup suites (lib/setup.test.sh, lib/setup-remote.test.sh):
# a copy of the fixture Target Project as a git checkout, a Host env and State
# dir under a scratch HOME, and recording stubs for every binary that would
# touch the machine or the network (gh, git's origin, claude, sudo, systemctl,
# ansible-playbook). Source it with ROOT_DIR and FIXTURE set.
#
# The Ansible stub behaves like the real runs where Setup depends on them:
# configure.yml writes the Host env content it was handed (0600) and reports
# changed=0 over identical vars; install.yml (the remote entry point's play)
# fails the distribution assertion on STUB_REMOTE_DISTRO, places a Harness
# install whose refs v1 and v2 are real commits over this repo's files, and
# writes the setup handoff it was handed (0600).

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
case "$*" in *infra/ansible/install.yml*) exec "${STUB_BIN}/ansible-install-play" "$@" ;; esac
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

    # The remote entry point's install play, run from the Operator machine.
    cat > "${H}/bin/ansible-install-play" <<'EOF'
#!/usr/bin/env bash
vars=""; while [ $# -gt 0 ]; do [ "$1" = -e ] && case "$2" in @*) vars="${2#@}" ;; esac; shift; done
echo "${vars}" > "${STUB_LOG}/install.varsfile"
stat -c %a "${vars}" > "${STUB_LOG}/install.varsmode"
jq '{aa_install_dir, aa_harness_repo, aa_harness_ref, aa_handoff_path, aa_config_draft_path,
     handoff_has_gh: (.aa_handoff_content | contains("AUTO_AGENT_SETUP_GH_TOKEN=")),
     handoff_has_claude: (.aa_handoff_content | contains("AUTO_AGENT_SETUP_CLAUDE_TOKEN="))}' "${vars}" > "${STUB_LOG}/install.vars"
if [ -n "${STUB_REMOTE_DISTRO:-}" ]; then
    echo "fatal: [vm]: FAILED! => {\"assertion\": \"ansible_distribution == 'Ubuntu'\", \"msg\": \"baseline: this Host runs ${STUB_REMOTE_DISTRO}; the reference Host is Ubuntu 24.04 LTS (ADR 0004)\"}"
    exit 2
fi
install="$(jq -r .aa_install_dir "${vars}")"; ref="$(jq -r .aa_harness_ref "${vars}")"
g() { /usr/bin/git -C "${install}" -c user.name=t -c user.email=t@t "$@"; }
changed=0
if [ ! -d "${install}/.git" ]; then
    mkdir -p "${install}"
    for d in bin lib infra plugin dashboard; do ln -s "${STUB_ROOT}/${d}" "${install}/${d}"; done
    g init -q; g commit -q --allow-empty -m v1; g tag v1; g commit -q --allow-empty -m v2; g tag v2
    changed=1
fi
before="$(g rev-parse HEAD)"
g checkout -q --detach "${ref}" 2>/dev/null || { echo "fatal: [vm]: FAILED! git: no ref ${ref}"; exit 2; }
[ "$(g rev-parse HEAD)" = "${before}" ] || changed=$((changed + 1))
if [ "$(jq -r '.aa_handoff_content | length' "${vars}")" -gt 0 ]; then
    dest="$(jq -r .aa_handoff_path "${vars}")"; mkdir -p "$(dirname "${dest}")"
    ( umask 077; jq -j .aa_handoff_content "${vars}" > "${dest}" ); changed=$((changed + 1))
fi
if [ "$(jq -r '.aa_config_draft_content | length' "${vars}")" -gt 0 ]; then
    jq -j .aa_config_draft_content "${vars}" > "$(jq -r .aa_config_draft_path "${vars}")"
fi
printf 'PLAY RECAP *********\nvm : ok=9 changed=%s unreachable=0 failed=0 skipped=1\n' "${changed}"
EOF

    # ssh: the Host is this machine under the Host's HOME.
    cat > "${H}/bin/ssh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/ssh.calls"
for a in "$@"; do case "${a}" in *SENTINEL*) echo "SECRET IN ARGV: ssh" >> "${STUB_LOG}/argv-leak" ;; esac; done
[ "${STUB_SSH_DOWN:-0}" = "1" ] && { echo "ssh: connect to host vm port 22: Connection refused" >&2; exit 255; }
while [ $# -gt 0 ]; do case "$1" in -t) shift ;; -o|-p|-i) shift 2 ;; *) break ;; esac; done
shift
cd "${STUB_HOST_HOME}" && exec env HOME="${STUB_HOST_HOME}" bash -c "$*"
EOF
    chmod +x "${H}/bin/"*
}

# run_setup <args...> : `bin/auto-agent setup` on the scratch Host; output in
# ${H}/out and ${H}/err, exit code in RC
run_cli() {
    env -u AUTO_AGENT_TARGET_DIR -u AUTO_AGENT_STATE_DIR -u GH_TOKEN -u DAEMON_GH_LOGIN -u CLAUDE_AUTH_MODE \
        -u CLAUDE_CODE_OAUTH_TOKEN -u ANTHROPIC_API_KEY -u AUTO_AGENT_HOST_USER -u HARNESS_CONFIG_JSON -u DISPLAY \
        HOME="${H}/home" XDG_STATE_HOME="${H}/home/.local/state" AUTO_AGENT_HOST_ENV="${H}/home/.config/auto-agent/env" \
        STUB_LOG="${H}/log" STUB_GH_TOKEN="${GH_SECRET}" STUB_BIN="${H}/bin" STUB_ROOT="${ROOT_DIR}" \
        GH_BIN="${H}/bin/gh" GIT_BIN="${H}/bin/git" CLAUDE_BIN="${H}/bin/claude" \
        ANSIBLE_PLAYBOOK_BIN="${H}/bin/ansible-playbook" SETUP_SUDO_BIN="${H}/bin/sudo" SYSTEMCTL_BIN="${H}/bin/systemctl" \
        SETUP_OS_RELEASE="${H}/os-release" SETUP_SYSTEMD_RUN_DIR="${H}/systemd-run" SETUP_ARCH=x86_64 \
        SETUP_NET_PROBE_CMD=true SETUP_DOCTOR_COMMANDS="bash jq" \
        SETUP_FIRE_CMD="echo 'fire: dry-run'; echo 'afk-pickup: would-pick #7 (label-only pick)'; true" \
        AUTO_AGENT_SETUP_UNATTENDED=1 \
        "$@" > "${H}/out" 2> "${H}/err" < /dev/null
    RC=$?
}
