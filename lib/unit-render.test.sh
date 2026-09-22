#!/usr/bin/env bash
# Tests for lib/unit-render.sh
#
# Run: bash lib/unit-render.test.sh
#
# Strategy: render both units through `bin/auto-agent unit-render` from a
# throwaway Host env and assert the rendered file: every placeholder filled
# from the Host env, and `systemd-analyze verify` silent (it exits 0 on
# unknown keys and bad values, so its output must be empty too). Skips the
# verify step where systemd-analyze is missing (issue #31 AC 3).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

make_env() {
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/home"
    printf 'CLAUDE_AUTH_MODE=login\n' > "${dir}/host.env"
    echo "${dir}"
}

# render <dir> <name> [env assignments...] : stdout to <dir>/out, stderr to <dir>/err
render() {
    local dir="$1" name="$2"; shift 2
    env -u AUTO_AGENT_HOST_USER -u AUTO_AGENT_UNIT_PATH -u AUTO_AGENT_MEMORY_MAX -u AUTO_AGENT_DASHBOARD_MEMORY_MAX \
        HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" "$@" \
        bash "${CLI}" unit-render "${name}" --out "${dir}/units" > "${dir}/out" 2> "${dir}/err"
}

test_render_from_host_env() {
    echo "TEST: both units render from the Host env and pass systemd-analyze verify (AC 3)"
    local dir; dir="$(make_env)"
    local me; me="$(id -un)"
    cat >> "${dir}/host.env" <<EOF
AUTO_AGENT_HOST_USER=${me}
AUTO_AGENT_MEMORY_MAX=6G
AUTO_AGENT_UNIT_PATH=/opt/claude/bin:/usr/bin:/bin
EOF
    render "${dir}" daemon; local rc=$?
    local unit="${dir}/units/auto-agent-daemon.service"
    if [ "${rc}" -eq 0 ] && [ "$(cat "${dir}/out")" = "${unit}" ] && [ -f "${unit}" ]; then pass "daemon: writes the unit and prints its path"
    else fail "daemon: writes the unit and prints its path" "rc=${rc} $(cat "${dir}/err")"; return; fi

    local want got
    want="User=${me}
WorkingDirectory=${ROOT_DIR}
Environment=PATH=/opt/claude/bin:/usr/bin:/bin
EnvironmentFile=${dir}/host.env
ExecStart=${ROOT_DIR}/bin/auto-agent daemon
MemoryMax=6G"
    got="$(grep -E '^(User|WorkingDirectory|Environment|EnvironmentFile|ExecStart|MemoryMax)=' "${unit}")"
    if [ "${got}" = "${want}" ]; then pass "daemon: user, install, PATH, EnvironmentFile, ExecStart and MemoryMax come from the Host env and the install"
    else fail "daemon: user, install, PATH, EnvironmentFile, ExecStart and MemoryMax come from the Host env and the install" "${got}"; fi
    if grep -q '^RestartPreventExitStatus=2 6 7$' "${unit}" && grep -q '^Restart=always$' "${unit}"; then
        pass "daemon: restarts always, except on the Daemon's refusal codes"
    else fail "daemon: restarts always, except on the Daemon's refusal codes"; fi

    render "${dir}" dashboard; rc=$?
    local dunit="${dir}/units/auto-agent-dashboard.service"
    if [ "${rc}" -eq 0 ] && grep -q "^ExecStart=${ROOT_DIR}/bin/auto-agent dashboard$" "${dunit}" \
       && grep -q '^MemoryMax=512M$' "${dunit}" && grep -q "^EnvironmentFile=${dir}/host.env$" "${dunit}"; then
        pass "dashboard: ExecStart from the install, its own MemoryMax default, the same EnvironmentFile"
    else fail "dashboard: ExecStart from the install, its own MemoryMax default, the same EnvironmentFile" "rc=${rc} $(cat "${dir}/err")"; fi

    if ! grep -hEq '@[A-Z_]+@' "${unit}" "${dunit}"; then pass "no placeholder is left"
    else fail "no placeholder is left" "$(grep -hE '@[A-Z_]+@' "${unit}" "${dunit}")"; fi

    if command -v systemd-analyze >/dev/null 2>&1; then
        local out; out="$(systemd-analyze verify "${unit}" "${dunit}" 2>&1)"; rc=$?
        if [ "${rc}" -eq 0 ] && [ -z "${out}" ]; then pass "systemd-analyze verify is silent on both units"
        else fail "systemd-analyze verify is silent on both units" "rc=${rc} ${out}"; fi
    else
        echo "  SKIP: systemd-analyze not installed"
    fi
    rm -rf "${dir}"
}

test_defaults_and_stdout() {
    echo "TEST: without the optional keys the unit takes the defaults and can go to stdout"
    local dir; dir="$(make_env)"
    local out rc
    out="$(env -u AUTO_AGENT_HOST_USER -u AUTO_AGENT_UNIT_PATH -u AUTO_AGENT_MEMORY_MAX \
        HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" bash "${CLI}" unit-render daemon)"; rc=$?
    if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q "^User=$(id -un)$" \
       && printf '%s\n' "${out}" | grep -q '^MemoryMax=8G$' \
       && printf '%s\n' "${out}" | grep -q "^Environment=PATH=${dir}/home/.local/bin:/usr/local/sbin:"; then
        pass "the invoking user, 8G and ~/.local/bin on PATH"
    else fail "the invoking user, 8G and ~/.local/bin on PATH" "rc=${rc} ${out}"; fi
    rm -rf "${dir}"
}

test_refuses_unsafe_values() {
    echo "TEST: a value that would break the unit is refused, nothing is written"
    local dir; dir="$(make_env)"
    local case_ args
    for case_ in "AUTO_AGENT_MEMORY_MAX=lots|systemd size" "AUTO_AGENT_HOST_USER=bad user|user name" \
                 "AUTO_AGENT_UNIT_PATH=/a b:/usr/bin|whitespace" "AUTO_AGENT_UNIT_PATH=/a&b:/usr/bin|& or a backslash"; do
        args="${case_%%|*}"
        render "${dir}" daemon "${args}"; local rc=$?
        if [ "${rc}" -eq 1 ] && grep -q "${case_#*|}" "${dir}/err" && [ ! -e "${dir}/units/auto-agent-daemon.service" ]; then
            pass "${args%%=*}: exit 1 and says why"
        else fail "${args%%=*}: exit 1 and says why" "rc=${rc} $(cat "${dir}/err")"; fi
    done
    rm -f "${dir}/host.env"
    render "${dir}" daemon; local rc=$?
    if [ "${rc}" -eq 1 ] && grep -q 'Host env not found' "${dir}/err"; then pass "a missing Host env: exit 1"
    else fail "a missing Host env: exit 1" "rc=${rc} $(cat "${dir}/err")"; fi
    rm -rf "${dir}"
}

test_percent_is_escaped() {
    echo "TEST: a % in a value is written as the unit-file literal %%"
    local dir; dir="$(make_env)"
    render "${dir}" daemon AUTO_AGENT_MEMORY_MAX=75%
    if grep -q '^MemoryMax=75%%$' "${dir}/units/auto-agent-daemon.service"; then pass "75% renders as 75%%"
    else fail "75% renders as 75%%" "$(grep MemoryMax "${dir}/units/auto-agent-daemon.service" 2>/dev/null) $(cat "${dir}/err")"; fi
    rm -rf "${dir}"
}

test_usage_errors() {
    echo "TEST: usage errors exit 2"
    local rc
    bash "${CLI}" unit-render nope >/dev/null 2>&1; rc=$?
    if [ "${rc}" -eq 2 ]; then pass "an unknown unit"; else fail "an unknown unit" "rc=${rc}"; fi
    bash "${CLI}" unit-render daemon --bogus >/dev/null 2>&1; rc=$?
    if [ "${rc}" -eq 2 ]; then pass "an unknown option"; else fail "an unknown option" "rc=${rc}"; fi
    bash "${CLI}" unit-render >/dev/null 2>&1; rc=$?
    if [ "${rc}" -eq 2 ]; then pass "no unit named"; else fail "no unit named" "rc=${rc}"; fi
}

test_render_from_host_env
test_defaults_and_stdout
test_refuses_unsafe_values
test_percent_is_escaped
test_usage_errors

echo ""
echo "=========================================="
echo "Ran: ${TESTS_RUN} | Failed: ${TESTS_FAILED}"
for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
echo "=========================================="
[ "${TESTS_FAILED}" -eq 0 ]
