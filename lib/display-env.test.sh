#!/usr/bin/env bash
# Tests for lib/display-env.sh
#
# Run: bash lib/display-env.test.sh
#
# Strategy: the lib is sourced into a subshell with the two kernel files it
# reads faked in a temp dir and the display probe injected, so every branch is
# exercised without a display server, an AppArmor profile or root. The cases
# are the two findings of ticket #19: display truth comes from a plain DISPLAY
# in the Host env and never degrades to headless, and a Host with no AppArmor
# profile for the app runs Electron with its sandbox off — reported, not
# silent (AC 4).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/display-env.sh"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# probe <script> : run a script with the lib sourced, stdout captured
probe() {
    env -u DISPLAY -u AUTO_AGENT_DISPLAY -u AUTO_AGENT_ELECTRON_SANDBOX -u ELECTRON_DISABLE_SANDBOX \
        "$@" bash -c '. "'"${LIB}"'"; '"${SCRIPT}"
}

echo "TEST: the display comes from the Host env's DISPLAY, probed before it is trusted"
SCRIPT='display_env_resolve && echo "DISPLAY=${DISPLAY}"'
out="$(probe DISPLAY=:99 DISPLAY_PROBE_CMD=true 2>/dev/null)"; rc=$?
if [ "${rc}" -eq 0 ] && [ "${out}" = "DISPLAY=:99" ]; then pass "resolve: a declared display"; else fail "resolve: a declared display" "rc=${rc} out=${out}"; fi

echo "TEST: AUTO_AGENT_DISPLAY answers when the round's own shell has no DISPLAY"
out="$(probe AUTO_AGENT_DISPLAY=:7 DISPLAY_PROBE_CMD=true 2>/dev/null)"
if [ "${out}" = "DISPLAY=:7" ]; then pass "resolve: AUTO_AGENT_DISPLAY"; else fail "resolve: AUTO_AGENT_DISPLAY" "out=${out}"; fi

echo "TEST: no display at all is exit 3 — an infra finding, never a headless fallback"
SCRIPT='display_env_resolve; echo "rc=$?"'
out="$(probe DISPLAY_PROBE_CMD=true 2>"${WORK}/err")"
if [ "${out}" = "rc=3" ] && grep -q 'refusing to fall back to headless' "${WORK}/err"; then pass "resolve: no display"; else fail "resolve: no display" "out=${out} err=$(cat "${WORK}/err")"; fi

echo "TEST: a declared display that does not answer is exit 3, not a silent pass"
out="$(probe DISPLAY=:99 DISPLAY_PROBE_CMD=false 2>"${WORK}/err")"
if [ "${out}" = "rc=3" ] && grep -q 'does not answer' "${WORK}/err"; then pass "resolve: a dead display"; else fail "resolve: a dead display" "out=${out} err=$(cat "${WORK}/err")"; fi

echo "TEST: a Host that does not restrict user namespaces needs no profile"
printf '0\n' > "${WORK}/userns-off"
SCRIPT='display_sandbox_mode /opt/app/electron; echo "flag=${ELECTRON_DISABLE_SANDBOX:-unset}"'
out="$(probe DISPLAY_USERNS_RESTRICT_FILE="${WORK}/userns-off" 2>/dev/null)"
if [ "${out}" = "$(printf 'sandbox\nflag=unset')" ]; then pass "sandbox: unrestricted Host"; else fail "sandbox: unrestricted Host" "out=${out}"; fi

echo "TEST: a restricted Host with a profile naming the app binary keeps the sandbox on"
printf '1\n' > "${WORK}/userns-on"
printf '/opt/app/electron (unconfined)\n/usr/bin/other (enforce)\n' > "${WORK}/profiles"
out="$(probe DISPLAY_USERNS_RESTRICT_FILE="${WORK}/userns-on" DISPLAY_APPARMOR_PROFILES_FILE="${WORK}/profiles" 2>/dev/null)"
if [ "${out}" = "$(printf 'apparmor\nflag=unset')" ]; then pass "sandbox: the AppArmor profile"; else fail "sandbox: the AppArmor profile" "out=${out}"; fi

echo "TEST: a restricted Host with no profile is DEGRADED, with the flag set — never silent (AC 4)"
printf '/usr/bin/other (enforce)\n' > "${WORK}/profiles-other"
out="$(probe DISPLAY_USERNS_RESTRICT_FILE="${WORK}/userns-on" DISPLAY_APPARMOR_PROFILES_FILE="${WORK}/profiles-other" 2>"${WORK}/err")"
if [ "${out}" = "$(printf 'disabled\nflag=1')" ] && grep -q 'ELECTRON_DISABLE_SANDBOX=1' "${WORK}/err"; then pass "sandbox: degraded fallback"; else fail "sandbox: degraded fallback" "out=${out} err=$(cat "${WORK}/err")"; fi

echo "TEST: the Host env can force the degraded path, and it still says so"
out="$(probe AUTO_AGENT_ELECTRON_SANDBOX=disabled DISPLAY_USERNS_RESTRICT_FILE="${WORK}/userns-off" 2>/dev/null)"
if [ "${out}" = "$(printf 'disabled\nflag=1')" ]; then pass "sandbox: forced by the Host env"; else fail "sandbox: forced by the Host env" "out=${out}"; fi

echo "TEST: a missing kernel switch reads as unrestricted, not as a crash"
out="$(probe DISPLAY_USERNS_RESTRICT_FILE="${WORK}/not-there" 2>/dev/null)"
if [ "${out}" = "$(printf 'sandbox\nflag=unset')" ]; then pass "sandbox: no kernel switch"; else fail "sandbox: no kernel switch" "out=${out}"; fi

echo "TEST: only the degraded mode reads as degraded"
SCRIPT='for m in sandbox apparmor disabled; do display_sandbox_degraded "$m" && echo "$m degraded" || echo "$m ok"; done'
out="$(probe 2>/dev/null)"
if [ "${out}" = "$(printf 'sandbox ok\napparmor ok\ndisabled degraded')" ]; then pass "degraded: one mode"; else fail "degraded: one mode" "out=${out}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
