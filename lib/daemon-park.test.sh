#!/usr/bin/env bash
# Tests for lib/daemon-park.sh
#
# Run: bash lib/daemon-park.test.sh
#
# Strategy: drive `bin/auto-agent park` with a CLAUDE_BIN stub whose
# `auth status` answer is a file, and a GH_BIN stub that records every call
# and answers `issue list` / `issue create` from files. Assert what a reader
# of the State dir, the stdout lines and the gh log sees (issue #30 AC 4).

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

CFG='{"repo":{"slug":"acme/widgets","default_branch":"main"},"pick":{"labels":{}}}'

make_env() {
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/state"
    cat > "${dir}/claude-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/claude.log"
cat "${dir}/auth.out"; exit \$(cat "${dir}/auth.code")
STUB
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/gh.log"
case "\$*" in
    "issue list "*) jq -r "\${@: -1}" "${dir}/list.out" ;;
    "issue create "*) [ -f "${dir}/create.fail" ] && exit 1; echo "https://github.com/acme/widgets/issues/77" ;;
    *) exit 0 ;;
esac
STUB
    chmod +x "${dir}/claude-stub" "${dir}/gh-stub"
    echo '{"loggedIn":false,"authMethod":"none"}' > "${dir}/auth.out"; echo 1 > "${dir}/auth.code"
    printf '' > "${dir}/list.out"
    echo "${dir}"
}

run_park() {
    local dir="$1"; shift
    HOME="${dir}" AUTO_AGENT_HOST_ENV="${dir}/missing.env" AUTO_AGENT_STATE_DIR="${dir}/state" \
    CLAUDE_BIN="${dir}/claude-stub" GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON="${CFG}" PARK_NOW=1789430400 \
        bash "${CLI}" park "$@"
}

test_status_when_not_parked() {
    echo "TEST: status and reprobe when not parked"
    local dir; dir="$(make_env)"
    local out; out="$(run_park "${dir}" status)"
    if [ "${out}" = '{"parked":false}' ]; then pass "status is {parked:false}"; else fail "status is {parked:false}" "${out}"; fi
    out="$(run_park "${dir}" reprobe)"; local rc=$?
    if [ "${rc}" -eq 0 ] && [ "${out}" = "park: not parked" ]; then pass "reprobe: not parked, 0, no gh call"; else fail "reprobe: not parked, 0, no gh call" "rc=${rc} ${out}"; fi
    if [ ! -e "${dir}/gh.log" ]; then pass "gh never called"; else fail "gh never called" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"
}

test_enter_opens_the_issue() {
    echo "TEST: park enter opens the needs-human issue and writes parked.json (AC 4)"
    local dir; dir="$(make_env)"
    local out; out="$(run_park "${dir}" enter --reason "authentication_failed in Fire f1")"; local rc=$?
    if [ "${rc}" -eq 0 ] && [ "${out}" = "park: parked issue=#77 reason=authentication_failed in Fire f1" ]; then pass "stable line names the issue"; else fail "stable line names the issue" "rc=${rc} ${out}"; fi
    if grep -q '^issue list --repo acme/widgets --label AFK:needs-human --state open' "${dir}/gh.log" \
       && grep -q '^issue create --repo acme/widgets --label AFK:needs-human --title Daemon parked on .*: Claude credential dead --body <!-- auto-agent:parked -->' "${dir}/gh.log"; then
        pass "searched the open needs-human issues, then created one with the marker"
    else fail "searched the open needs-human issues, then created one with the marker" "$(cat "${dir}/gh.log")"; fi
    local got; got="$(jq -c '[.parked, .parkedAt, .reason, .issue, .probes, .lastProbeAt]' "${dir}/state/parked.json")"
    if [ "${got}" = '[true,"2026-09-15T00:00:00Z","authentication_failed in Fire f1",77,0,null]' ]; then pass "parked.json"; else fail "parked.json" "${got}"; fi
    # Entering again is idempotent: no second issue, no gh call at all.
    : > "${dir}/gh.log"
    out="$(run_park "${dir}" enter --reason again)"
    if [ "${out}" = "park: parked issue=#77 reason=authentication_failed in Fire f1" ] && [ ! -s "${dir}/gh.log" ]; then pass "a second enter keeps the first park and issue"; else fail "a second enter keeps the first park and issue" "${out} / $(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"
}

test_enter_reuses_the_open_issue() {
    echo "TEST: park enter reuses the open needs-human issue with the marker (AC 4)"
    local dir; dir="$(make_env)"
    echo '[{"number":41,"body":"unrelated needs-human issue"},{"number":52,"body":"<!-- auto-agent:parked -->\nold park"}]' > "${dir}/list.out"
    local out; out="$(run_park "${dir}" enter --reason "401 from the usage endpoint")"
    if [ "${out}" = "park: parked issue=#52 reason=401 from the usage endpoint" ]; then pass "the marked issue is reused, the unrelated one is not"; else fail "the marked issue is reused, the unrelated one is not" "${out}"; fi
    if ! grep -q '^issue create' "${dir}/gh.log" && grep -q '^issue comment 52 --repo acme/widgets --body Parked again at 2026-09-15T00:00:00Z' "${dir}/gh.log"; then pass "no new issue; a comment on the reused one"; else fail "no new issue; a comment on the reused one" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"
}

test_enter_without_gh_still_parks() {
    echo "TEST: parking never depends on gh; the issue is retried on the next re-probe"
    local dir; dir="$(make_env)"
    touch "${dir}/create.fail"
    local out; out="$(run_park "${dir}" enter --reason dead 2>/dev/null)"; local rc=$?
    if [ "${rc}" -eq 1 ] && [ "${out}" = "park: parked issue=none reason=dead" ] && [ "$(jq -r .issue "${dir}/state/parked.json")" = "null" ]; then pass "parked with no issue, rc 1"; else fail "parked with no issue, rc 1" "rc=${rc} ${out}"; fi
    rm -f "${dir}/create.fail"
    out="$(run_park "${dir}" reprobe)"; rc=$?
    if [ "${rc}" -eq 1 ] && [ "${out}" = "park: still parked issue=#77 probes=1" ]; then pass "the re-probe opened the issue and counted the probe"; else fail "the re-probe opened the issue and counted the probe" "rc=${rc} ${out}"; fi
    rm -rf "${dir}"
}

test_tick_unparks_when_the_probe_passes() {
    echo "TEST: the hourly tick un-parks when claude auth status passes (AC 4)"
    local dir; dir="$(make_env)"
    run_park "${dir}" enter --reason dead >/dev/null
    local out; out="$(run_park "${dir}" reprobe)"; local rc=$?
    if [ "${rc}" -eq 1 ] && [ "${out}" = "park: still parked issue=#77 probes=1" ]; then pass "probe fails: still parked, rc 1"; else fail "probe fails: still parked, rc 1" "rc=${rc} ${out}"; fi
    out="$(run_park "${dir}" reprobe)"
    if [ "$(jq -c '[.probes, .lastProbeAt]' "${dir}/state/parked.json")" = '[2,"2026-09-15T00:00:00Z"]' ]; then pass "probes are counted"; else fail "probes are counted" "$(cat "${dir}/state/parked.json")"; fi
    if ! grep -q '^issue close' "${dir}/gh.log"; then pass "the issue stays open while parked"; else fail "the issue stays open while parked"; fi
    echo '{"loggedIn":true,"authMethod":"claude.ai"}' > "${dir}/auth.out"; echo 0 > "${dir}/auth.code"
    out="$(run_park "${dir}" reprobe)"; rc=$?
    if [ "${rc}" -eq 0 ] && [ "${out}" = "park: un-parked issue=#77" ]; then pass "probe passes: un-parked, rc 0"; else fail "probe passes: un-parked, rc 0" "rc=${rc} ${out}"; fi
    if grep -q '^issue close 77 --repo acme/widgets --comment Un-parked at 2026-09-15T00:00:00Z' "${dir}/gh.log"; then pass "the issue is closed with a comment"; else fail "the issue is closed with a comment" "$(cat "${dir}/gh.log")"; fi
    if [ ! -e "${dir}/state/parked.json" ] && [ "$(run_park "${dir}" status)" = '{"parked":false}' ]; then pass "parked.json removed"; else fail "parked.json removed"; fi
    if grep -c '^auth status --json' "${dir}/claude.log" | grep -q '^3$'; then pass "each re-probe ran one probe"; else fail "each re-probe ran one probe" "$(cat "${dir}/claude.log")"; fi
    rm -rf "${dir}"
}

test_probe_and_leave() {
    echo "TEST: probe and leave stand alone"
    local dir; dir="$(make_env)"
    local out; out="$(run_park "${dir}" probe)"; local rc=$?
    if [ "${rc}" -eq 1 ] && [ "${out}" = "park: probe failed" ]; then pass "probe: exit 1 from auth status is a failed probe"; else fail "probe: exit 1 from auth status is a failed probe" "rc=${rc} ${out}"; fi
    echo '{"loggedIn":false}' > "${dir}/auth.out"; echo 0 > "${dir}/auth.code"
    out="$(run_park "${dir}" probe)"; rc=$?
    if [ "${rc}" -eq 1 ]; then pass "probe: loggedIn false is a failed probe even on exit 0"; else fail "probe: loggedIn false is a failed probe even on exit 0" "rc=${rc}"; fi
    echo '{"loggedIn":true}' > "${dir}/auth.out"
    out="$(run_park "${dir}" probe)"; rc=$?
    if [ "${rc}" -eq 0 ] && [ "${out}" = "park: probe ok" ]; then pass "probe ok"; else fail "probe ok" "rc=${rc} ${out}"; fi
    out="$(run_park "${dir}" leave)"
    if [ "${out}" = "park: not parked" ]; then pass "leave when not parked is a no-op"; else fail "leave when not parked is a no-op" "${out}"; fi
    out="$(run_park "${dir}" bogus 2>&1)"; rc=$?
    if [ "${rc}" -eq 2 ]; then pass "unknown command: 2"; else fail "unknown command: 2" "rc=${rc}"; fi
    rm -rf "${dir}"
}

test_status_when_not_parked
test_enter_opens_the_issue
test_enter_reuses_the_open_issue
test_enter_without_gh_still_parks
test_tick_unparks_when_the_probe_passes
test_probe_and_leave

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
