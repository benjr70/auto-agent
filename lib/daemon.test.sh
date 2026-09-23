#!/usr/bin/env bash
# Tests for lib/daemon.sh
#
# Run: bash lib/daemon.test.sh
#
# Strategy: drive `bin/auto-agent daemon` with its collaborators injected, as
# Smart-Smoker-V2's agent-daemon.test.sh did: a sensor stub that replays
# canned Gate verdicts (one "<rc> <json>" line per call), a Fire stub that
# logs "fired" and prints the markers a test chooses, a sleep stub that logs
# the seconds, a park stub and a Work Probe stub. DAEMON_MAX_CYCLES
# caps the loop. The last tests run the real `fire --dry-run` under the loop
# against a git copy of the fixture Target Project (issue #31 AC 1 and 2).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"
FIXTURE="${ROOT_DIR}/plugin/fixtures/target-project"
CANNED_PICKUP="${SCRIPT_DIR}/testdata/pickup-dry-run.stream.jsonl"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

NOW_EPOCH=$(date -u -d '2026-07-05T20:00:00Z' +%s)
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

# verdict <shouldFire> [<resetAt>] [<fireModel>] -> a Gate verdict line
verdict() {
    jq -n -c --argjson f "$1" --arg r "${2:-}" --arg m "${3:-}" '{
        authMode: "login", sensor: "usage-endpoint", state: "ok",
        remainPct: (if $f then 80 else 5 end), resetAt: (if $r == "" then null else $r end),
        shouldFire: $f, observedAt: "2026-07-05T20:00:00Z", limits: [], warnings: [],
        fireModel: (if $m == "" then null else $m end), fireModelUntil: null }'
}

FRESH="0 $(verdict true "$(iso $((NOW_EPOCH + 18000)))")"
SPENT="0 $(verdict false "$(iso $((NOW_EPOCH + 3600)))")"
QUIET_PROBE='{"locked":false,"reconcile":null,"paused":null,"pickSig":""}'

# make_env -> dir with sensor, fire, sleep, park and probe stubs, state/, target/
#   sensor.seq   one "<rc> <json>" line per call; the last line repeats
#   fire.out     what the Fire stub prints (markers); fire.code its exit code
#   park.status  what `park status` prints; park.reprobe the reprobe exit code
make_env() {
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/state" "${dir}/target" "${dir}/home"
    : > "${dir}/calls.log"; : > "${dir}/fire.out"; echo 0 > "${dir}/fire.code"
    echo '{"parked":false}' > "${dir}/park.status"; echo 1 > "${dir}/park.reprobe"
    printf '%s\n' "${QUIET_PROBE}" > "${dir}/probe.out"
    printf '%s\n' "${FRESH}" > "${dir}/sensor.seq"
    cat > "${dir}/sensor-stub" <<STUB
#!/usr/bin/env bash
n=\$(cat "${dir}/sensor.n" 2>/dev/null || echo 0); echo \$((n + 1)) > "${dir}/sensor.n"
total=\$(wc -l < "${dir}/sensor.seq")
[ "\${n}" -lt "\${total}" ] || n=\$((total - 1))
line="\$(sed -n "\$((n + 1))p" "${dir}/sensor.seq")"
echo "gated" >> "${dir}/calls.log"
printf '%s\n' "\${line#* }"
exit "\${line%% *}"
STUB
    cat > "${dir}/fire-stub" <<STUB
#!/usr/bin/env bash
echo "fired \$*" >> "${dir}/calls.log"
[ -e /proc/self/fd/9 ] && echo "lock fd leaked" >> "${dir}/calls.log"
echo "verdict=\$(jq -c '{shouldFire, fireModel}' "\${AUTO_AGENT_GATE_VERDICT_FILE}" 2>/dev/null) daemon_id=\${AUTO_AGENT_DAEMON_ID:-}" >> "${dir}/fire.env"
cat "${dir}/fire.out"
exit \$(cat "${dir}/fire.code")
STUB
    cat > "${dir}/sleep-stub" <<STUB
#!/usr/bin/env bash
echo "slept \$*" >> "${dir}/calls.log"
STUB
    cat > "${dir}/park-stub" <<STUB
#!/usr/bin/env bash
echo "park \$*" >> "${dir}/calls.log"
case "\$1" in
    status) cat "${dir}/park.status" ;;
    reprobe) exit \$(cat "${dir}/park.reprobe") ;;
esac
STUB
    cat > "${dir}/probe-stub" <<STUB
#!/usr/bin/env bash
cat "${dir}/probe.out"
STUB
    chmod +x "${dir}"/*-stub
    echo "${dir}"
}

# run_daemon <dir> <max-cycles> [extra env assignments...]
run_daemon() {
    local dir="$1" cycles="$2"; shift 2
    env HOME="${dir}/home" AUTO_AGENT_HOST_ENV=/nonexistent AUTO_AGENT_STATE_DIR="${dir}/state" \
        AUTO_AGENT_TARGET_DIR="${dir}/target" DAEMON_MAX_CYCLES="${cycles}" DAEMON_NOW="${NOW_EPOCH}" \
        DAEMON_SENSOR_CMD="${dir}/sensor-stub" DAEMON_FIRE_CMD="${dir}/fire-stub" \
        DAEMON_PARK_CMD="${dir}/park-stub" WORK_PROBE_CMD="${dir}/probe-stub" SLEEP_CMD="${dir}/sleep-stub" \
        AUTO_AGENT_DAEMON_ID=test-daemon AUTO_AGENT_DAEMON_FIRE_ARGS= "$@" \
        bash "${CLI}" daemon > "${dir}/daemon.out" 2>&1
}

count() { grep -c "$1" "$2" || true; }

#-------------------------------------------------------------------------------
test_fresh_budget_fires() {
    echo "TEST: a firing verdict runs one Fire and hands it the verdict"
    local dir; dir="$(make_env)"
    run_daemon "${dir}" 1
    if [ "$(count '^fired' "${dir}/calls.log")" = 1 ]; then pass "fires once"
    else fail "fires once" "$(cat "${dir}/calls.log")"; fi
    if grep -q "^fired ${dir}/target$" "${dir}/calls.log"; then pass "the Fire is pointed at the Target Project"
    else fail "the Fire is pointed at the Target Project" "$(cat "${dir}/calls.log")"; fi
    if grep -q '^verdict={"shouldFire":true,"fireModel":null} daemon_id=test-daemon$' "${dir}/fire.env"; then
        pass "the Fire reads the verdict from AUTO_AGENT_GATE_VERDICT_FILE and gets the Daemon id"
    else fail "the Fire reads the verdict from AUTO_AGENT_GATE_VERDICT_FILE and gets the Daemon id" "$(cat "${dir}/fire.env")"; fi
    if ! grep -q 'lock fd leaked' "${dir}/calls.log"; then pass "the Fire does not inherit the Daemon's lock fd"
    else fail "the Fire does not inherit the Daemon's lock fd"; fi
    if jq -e '.shouldFire == true' "${dir}/state/gate-verdict.json" >/dev/null 2>&1; then pass "the verdict is kept in the State dir"
    else fail "the verdict is kept in the State dir"; fi
    rm -rf "${dir}"
}

test_spent_budget_sleeps_to_reset() {
    echo "TEST: a refusing verdict sleeps to its reset and does not fire"
    local dir; dir="$(make_env)"
    printf '%s\n' "${SPENT}" > "${dir}/sensor.seq"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=2
    if [ "$(count '^fired' "${dir}/calls.log")" = 0 ]; then pass "does not fire"
    else fail "does not fire" "$(cat "${dir}/calls.log")"; fi
    if [ "$(sed -n 's/^slept //p' "${dir}/calls.log" | head -1)" = 3600 ]; then pass "sleeps exactly to the verdict's reset"
    else fail "sleeps exactly to the verdict's reset" "$(cat "${dir}/calls.log")"; fi
    if [ "$(count '^gated' "${dir}/calls.log")" = 3 ]; then pass "then polls the gate up to the planner's cap"
    else fail "then polls the gate up to the planner's cap" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_poll_stops_when_budget_returns() {
    echo "TEST: the post-wake poll stops as soon as the gate would fire"
    local dir; dir="$(make_env)"
    printf '%s\n' "${SPENT}" "${SPENT}" "${FRESH}" > "${dir}/sensor.seq"
    run_daemon "${dir}" 2 SLEEP_POLL_MAX=12
    if [ "$(count '^gated' "${dir}/calls.log")" = 4 ] && [ "$(count '^fired' "${dir}/calls.log")" = 1 ]; then
        pass "two polls, then the next cycle fires"
    else fail "two polls, then the next cycle fires" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_unknown_reset_sleeps_degraded() {
    echo "TEST: a refusing verdict without a reset takes the planner's degraded sleep"
    local dir; dir="$(make_env)"
    printf '%s\n' "0 $(verdict false)" > "${dir}/sensor.seq"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=0
    if [ "$(sed -n 's/^slept //p' "${dir}/calls.log")" = 18000 ]; then pass "sleeps SLEEP_DEGRADED_SECS"
    else fail "sleeps SLEEP_DEGRADED_SECS" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_sensor_failure_stays_alive() {
    echo "TEST: a sensor that prints no verdict never fires and never crashes"
    local dir; dir="$(make_env)"
    printf '%s\n' "1 not json" > "${dir}/sensor.seq"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=0; local rc=$?
    if [ "${rc}" -eq 0 ] && [ "$(count '^fired' "${dir}/calls.log")" = 0 ] && grep -q '^slept 18000$' "${dir}/calls.log"; then
        pass "exits the cycle cleanly after a degraded sleep"
    else fail "exits the cycle cleanly after a degraded sleep" "rc=${rc} $(cat "${dir}/calls.log")"; fi
    if [ ! -e "${dir}/state/gate-verdict.json" ]; then pass "no stale verdict file is left for a Fire to read"
    else fail "no stale verdict file is left for a Fire to read"; fi
    rm -rf "${dir}"
}

test_clean_run_refires_same_window() {
    echo "TEST: a clean Fire goes straight back to the gate"
    local dir; dir="$(make_env)"
    run_daemon "${dir}" 2
    if [ "$(count '^fired' "${dir}/calls.log")" = 2 ] && [ "$(count '^slept' "${dir}/calls.log")" = 0 ]; then
        pass "fires twice without sleeping"
    else fail "fires twice without sleeping" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_no_work_probe_sleeps() {
    echo "TEST: an empty queue sleeps to the reset in Work Probe chunks"
    local dir; dir="$(make_env)"
    printf 'afk-pickup: no eligible issue\nAGENT_RUN_NO_WORK=1\n' > "${dir}/fire.out"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=0
    if [ "$(count '^fired' "${dir}/calls.log")" = 1 ] && [ "$(count '^slept 300$' "${dir}/calls.log")" = 60 ]; then
        pass "fires once, then 60 chunks of 300s to the 5h reset"
    else fail "fires once, then 60 chunks of 300s to the 5h reset" "$(sort "${dir}/calls.log" | uniq -c)"; fi
    rm -rf "${dir}"
}

test_no_work_probe_wakes_early() {
    echo "TEST: the Work Probe wakes the empty-queue sleep when work appears"
    local dir; dir="$(make_env)"
    printf 'AGENT_RUN_NO_WORK=1\n' > "${dir}/fire.out"
    printf '%s\n' '{"locked":false,"reconcile":305,"paused":null,"pickSig":""}' > "${dir}/probe.out"
    run_daemon "${dir}" 2
    if grep -q 'waking early' "${dir}/daemon.out" && grep -q 'reconcile PR #305' "${dir}/daemon.out"; then
        pass "logs the early wake and names the work"
    else fail "logs the early wake and names the work" "$(tail -5 "${dir}/daemon.out")"; fi
    if [ "$(count '^fired' "${dir}/calls.log")" = 2 ] && [ "$(count '^slept' "${dir}/calls.log")" = 2 ]; then
        pass "one chunk per cycle, then the next Fire"
    else fail "one chunk per cycle, then the next Fire" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_no_work_probe_suppresses_baseline() {
    echo "TEST: an unchanged candidate signature does not wake the Daemon"
    local dir; dir="$(make_env)"
    printf 'AGENT_RUN_NO_WORK=1\n' > "${dir}/fire.out"
    printf '%s\n' '{"locked":false,"reconcile":null,"paused":null,"pickSig":"290"}' > "${dir}/probe.out"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=0
    if ! grep -q 'waking early' "${dir}/daemon.out" && [ "$(count '^fired' "${dir}/calls.log")" = 1 ]; then
        pass "sleeps the whole window"
    else fail "sleeps the whole window" "$(grep 'waking' "${dir}/daemon.out")"; fi
    rm -rf "${dir}"
}

test_no_work_probe_wakes_on_pr_shrink() {
    echo "TEST: blocker PRs leaving the open set wake the Daemon"
    local dir; dir="$(make_env)"
    printf 'AGENT_RUN_NO_WORK=1\n' > "${dir}/fire.out"
    cat > "${dir}/probe-stub" <<STUB
#!/usr/bin/env bash
if [ ! -e "${dir}/probe.seen" ]; then
    touch "${dir}/probe.seen"
    printf '%s\n' '{"locked":false,"reconcile":null,"paused":null,"pickSig":"","prSig":"354,367"}'
else
    printf '%s\n' '{"locked":false,"reconcile":null,"paused":null,"pickSig":"","prSig":""}'
fi
STUB
    run_daemon "${dir}" 2
    if grep -q 'PR(s) left the open set #354,367' "${dir}/daemon.out" && [ "$(count '^fired' "${dir}/calls.log")" = 2 ]; then
        pass "wakes, names the PRs, fires again"
    else fail "wakes, names the PRs, fires again" "$(tail -5 "${dir}/daemon.out")"; fi
    rm -rf "${dir}"
}

test_exhausted_sleeps_to_fire_reset() {
    echo "TEST: a Fire that ran out of budget sleeps to its own reset"
    local dir; dir="$(make_env)"
    printf 'AGENT_RUN_RESET_AT=%s\n' "$(iso $((NOW_EPOCH + 7200)))" > "${dir}/fire.out"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=0
    if [ "$(sed -n 's/^slept //p' "${dir}/calls.log")" = 7200 ]; then pass "sleeps to the Fire's reset, not the verdict's"
    else fail "sleeps to the Fire's reset, not the verdict's" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_exhausted_empty_reset_uses_verdict() {
    echo "TEST: an exhausted Fire with no reset sleeps to the verdict's reset"
    local dir; dir="$(make_env)"
    printf 'AGENT_RUN_RESET_AT=\n' > "${dir}/fire.out"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=0
    if [ "$(sed -n 's/^slept //p' "${dir}/calls.log")" = 18000 ]; then pass "sleeps to the verdict's reset"
    else fail "sleeps to the verdict's reset" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_model_limit_regates_once() {
    echo "TEST: a per-model limit re-gates at once, a second in a row sleeps"
    local dir; dir="$(make_env)"
    printf 'AGENT_RUN_MODEL_LIMIT=fable\nAGENT_RUN_RESET_AT=\n' > "${dir}/fire.out"
    printf '%s\n' "${FRESH}" "0 $(verdict true "$(iso $((NOW_EPOCH + 18000)))" opus)" > "${dir}/sensor.seq"
    run_daemon "${dir}" 2 SLEEP_POLL_MAX=0
    if [ "$(count '^fired' "${dir}/calls.log")" = 2 ] && [ "$(sed -n '1,3p' "${dir}/calls.log" | grep -c '^slept')" = 0 ]; then
        pass "the second Fire follows the first with no sleep"
    else fail "the second Fire follows the first with no sleep" "$(cat "${dir}/calls.log")"; fi
    if [ "$(sed -n 2p "${dir}/fire.env" | sed 's/ daemon_id.*//')" = 'verdict={"shouldFire":true,"fireModel":"opus"}' ]; then
        pass "the re-gated Fire gets the switched model in its verdict"
    else fail "the re-gated Fire gets the switched model in its verdict" "$(cat "${dir}/fire.env")"; fi
    if [ "$(count '^slept' "${dir}/calls.log")" = 1 ]; then pass "a second per-model limit in a row sleeps"
    else fail "a second per-model limit in a row sleeps" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_failed_fire_probe_sleeps_then_caps() {
    echo "TEST: failed Fires probe-sleep, the cap goes deaf, a clean Fire resets the count"
    local dir; dir="$(make_env)"
    echo 1 > "${dir}/fire.code"
    run_daemon "${dir}" 3 SLEEP_POLL_MAX=0
    if grep -q 'probe-sleeping (fail 1/3)' "${dir}/daemon.out" && grep -q 'probe-sleeping (fail 2/3)' "${dir}/daemon.out" \
       && grep -q 'fail cap reached (3/3), sleeping until window reset' "${dir}/daemon.out"; then
        pass "fail 1/3, 2/3, then the cap"
    else fail "fail 1/3, 2/3, then the cap" "$(grep -i fail "${dir}/daemon.out")"; fi
    if [ "$(count '^fired' "${dir}/calls.log")" = 3 ]; then pass "one Fire per cycle, no hot loop"
    else fail "one Fire per cycle, no hot loop" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    cat > "${dir}/fire-stub" <<STUB
#!/usr/bin/env bash
echo "fired" >> "${dir}/calls.log"
n=\$(cat "${dir}/fire.n" 2>/dev/null || echo 0); echo \$((n + 1)) > "${dir}/fire.n"
[ "\${n}" -eq 1 ] && exit 0
exit 1
STUB
    run_daemon "${dir}" 3 SLEEP_POLL_MAX=0
    if [ "$(count 'probe-sleeping (fail 1/3)' "${dir}/daemon.out")" = 2 ] && ! grep -q '(fail 2/3)' "${dir}/daemon.out"; then
        pass "a clean Fire between failures resets the count"
    else fail "a clean Fire between failures resets the count" "$(grep -i fail "${dir}/daemon.out")"; fi
    rm -rf "${dir}"
}

test_auth_dead_parks_and_reprobes() {
    echo "TEST: a dead credential parks the Daemon and re-probes hourly"
    local dir; dir="$(make_env)"
    printf '%s\n' "4 $(jq -n -c '{sensor:"usage-endpoint",state:"auth-dead",shouldFire:false}')" > "${dir}/sensor.seq"
    run_daemon "${dir}" 1
    if grep -q '^park enter --reason usage sensor: auth-dead$' "${dir}/calls.log" && [ "$(count '^fired' "${dir}/calls.log")" = 0 ]; then
        pass "sensor rc 4 runs park enter and no Fire"
    else fail "sensor rc 4 runs park enter and no Fire" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    echo '{"parked":true,"issue":12}' > "${dir}/park.status"
    run_daemon "${dir}" 2
    if [ "$(count '^slept 3600$' "${dir}/calls.log")" = 2 ] && [ "$(count '^park reprobe' "${dir}/calls.log")" = 2 ] \
       && [ "$(count '^gated' "${dir}/calls.log")" = 0 ] && [ "$(count '^fired' "${dir}/calls.log")" = 0 ]; then
        pass "a parked Daemon only sleeps an hour and re-probes, never gates or fires"
    else fail "a parked Daemon only sleeps an hour and re-probes, never gates or fires" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    printf 'AGENT_RUN_AUTH_DEAD=1\n' > "${dir}/fire.out"; echo 1 > "${dir}/fire.code"
    run_daemon "${dir}" 1
    if grep -q 'Fire found the credential dead' "${dir}/daemon.out" && [ "$(count '^slept' "${dir}/calls.log")" = 0 ] \
       && ! grep -q 'fail 1/3' "${dir}/daemon.out"; then
        pass "a Fire that parked is not counted as a failure and does not sleep"
    else fail "a Fire that parked is not counted as a failure and does not sleep" "$(cat "${dir}/daemon.out")"; fi
    rm -rf "${dir}"
}

test_mode_mismatch_and_api_key() {
    echo "TEST: a mode mismatch holds the Daemon, api-key mode stops it"
    local dir; dir="$(make_env)"
    printf '%s\n' "5 $(verdict false)" > "${dir}/sensor.seq"
    run_daemon "${dir}" 1
    if [ "$(count '^fired' "${dir}/calls.log")" = 0 ] && grep -q '^slept 3600$' "${dir}/calls.log"; then
        pass "rc 5: no Fire, re-gate after AUTO_AGENT_GATE_RETRY_SECS"
    else fail "rc 5: no Fire, re-gate after AUTO_AGENT_GATE_RETRY_SECS" "$(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    printf '%s\n' "6 $(verdict false)" > "${dir}/sensor.seq"
    run_daemon "${dir}" 5; local rc=$?
    if [ "${rc}" -eq 6 ] && [ "$(count '^gated' "${dir}/calls.log")" = 1 ] && [ "$(count '^fired' "${dir}/calls.log")" = 0 ]; then
        pass "rc 6: the Daemon exits 6 on the first cycle"
    else fail "rc 6: the Daemon exits 6 on the first cycle" "rc=${rc} $(cat "${dir}/calls.log")"; fi
    rm -rf "${dir}"
}

test_single_daemon_per_state_dir() {
    echo "TEST: a second Daemon on the same State dir exits 7"
    local dir; dir="$(make_env)"
    ( exec 9>"${dir}/state/daemon.lock"; flock 9; touch "${dir}/held"; sleep 5 ) &
    local holder=$!
    while [ ! -e "${dir}/held" ]; do sleep 0.05; done
    run_daemon "${dir}" 1; local rc=$?
    kill "${holder}" 2>/dev/null; wait "${holder}" 2>/dev/null
    if [ "${rc}" -eq 7 ] && [ "$(count '^gated' "${dir}/calls.log")" = 0 ]; then pass "exits 7 before gating"
    else fail "exits 7 before gating" "rc=${rc} $(cat "${dir}/daemon.out")"; fi
    rm -rf "${dir}"
}

test_missing_target_is_usage_error() {
    echo "TEST: no Target Project is a usage error"
    local dir; dir="$(make_env)"
    run_daemon "${dir}" 1 AUTO_AGENT_TARGET_DIR="${dir}/nope"; local rc=$?
    if [ "${rc}" -eq 2 ] && grep -q 'no Target Project' "${dir}/daemon.out"; then pass "exits 2 and says why"
    else fail "exits 2 and says why" "rc=${rc} $(cat "${dir}/daemon.out")"; fi
    rm -rf "${dir}"
}

#-------------------------------------------------------------------------------
# The loop over the real Fire wrapper: `fire --dry-run` with a claude stub
# that replays the canned pickup dry-run, against a git copy of the fixture.
make_fixture_env() {
    local dir; dir="$(make_env)"
    rm -rf "${dir}/target"
    cp -r "${FIXTURE}" "${dir}/target"
    find "${dir}/target" -name __pycache__ -prune -exec rm -rf {} +
    git -C "${dir}/target" init -q -b main
    git -C "${dir}/target" -c user.email=t@t -c user.name=t add -A
    git -C "${dir}/target" -c user.email=t@t -c user.name=t commit -q -m fixture
    git -C "${dir}/target" remote add origin https://github.com/acme/widgets.git
    cat > "${dir}/claude-stub" <<STUB
#!/usr/bin/env bash
cat "${CANNED_PICKUP}"
STUB
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
case "\$*" in
    "repo view "*) echo main ;;
esac
STUB
    chmod +x "${dir}/claude-stub" "${dir}/gh-stub"
    echo "${dir}"
}

run_fixture_daemon() {
    local dir="$1" cycles="$2"; shift 2
    run_daemon "${dir}" "${cycles}" DAEMON_FIRE_CMD="\"${CLI}\" fire" AUTO_AGENT_DAEMON_FIRE_ARGS=--dry-run \
        CLAUDE_BIN="${dir}/claude-stub" GH_BIN="${dir}/gh-stub" AUTO_AGENT_FIRE_MODEL= HARNESS_CONFIG_JSON= \
        AUTO_AGENT_GATE_VERDICT_FILE= "$@"
}

test_state_file_names_what_the_daemon_does() {
    echo "TEST: daemon-state.json says what the Daemon is doing, for the Dashboard"
    local dir; dir="$(make_env)"
    local reset; reset="$(iso $((NOW_EPOCH + 3600)))"
    printf '%s\n' "${SPENT}" > "${dir}/sensor.seq"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=0
    if jq -e --arg r "${reset}" '.state == "budget_low" and .resetAt == $r and .daemonId == "test-daemon"
            and (.detail | test("budget below min")) and (.at | test("Z$")) and .failCap == 3' \
            "${dir}/state/daemon-state.json" >/dev/null 2>&1; then
        pass "a refusing gate records budget_low with the reset it sleeps to"
    else fail "a refusing gate records budget_low with the reset it sleeps to" "$(cat "${dir}/state/daemon-state.json" 2>&1)"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    printf 'boom\n' > "${dir}/fire.out"; echo 1 > "${dir}/fire.code"
    run_daemon "${dir}" 1 SLEEP_POLL_MAX=0
    if jq -e '.state == "run_failed" and .fails == 1 and .failCap == 3' "${dir}/state/daemon-state.json" >/dev/null 2>&1; then
        pass "a failed Fire records run_failed with the fail count"
    else fail "a failed Fire records run_failed with the fail count" "$(cat "${dir}/state/daemon-state.json" 2>&1)"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    run_daemon "${dir}" 1
    if jq -e '.state == "run_complete"' "${dir}/state/daemon-state.json" >/dev/null 2>&1; then
        pass "a clean Fire records run_complete"
    else fail "a clean Fire records run_complete" "$(cat "${dir}/state/daemon-state.json" 2>&1)"; fi
    rm -rf "${dir}"
}

test_loop_writes_one_record_per_fire() {
    echo "TEST: a Daemon loop of dry-run Fires writes one Fire record per Fire and sleeps by the verdict (AC 1)"
    local dir; dir="$(make_fixture_env)"
    printf '%s\n' "${FRESH}" "${FRESH}" "${SPENT}" > "${dir}/sensor.seq"
    run_fixture_daemon "${dir}" 3 SLEEP_POLL_MAX=0; local rc=$?
    local records; records="$(ls "${dir}/state/fires"/*.json 2>/dev/null | wc -l)"
    if [ "${rc}" -eq 0 ] && [ "${records}" -eq 2 ] && [ "$(count 'fire: dry-run ok=yes' "${dir}/daemon.out")" = 2 ]; then
        pass "two firing verdicts, two dry-run Fires, two Fire records"
    else fail "two firing verdicts, two dry-run Fires, two Fire records" "rc=${rc} records=${records}
$(tail -20 "${dir}/daemon.out")"; fi
    if [ "$(count 'record=.*/state/fires/.*\.json$' "${dir}/daemon.out")" -ge 2 ] && ! grep -q 'named no Fire record' "${dir}/daemon.out"; then
        pass "the Daemon logs each Fire's record"
    else fail "the Daemon logs each Fire's record" "$(grep 'record' "${dir}/daemon.out")"; fi
    if [ "$(jq -s -c 'map(.gate | {sensor, shouldFire}) | unique' "${dir}/state/fires"/*.json)" = '[{"sensor":"usage-endpoint","shouldFire":true}]' ]; then
        pass "each record embeds the verdict the Daemon fired on"
    else fail "each record embeds the verdict the Daemon fired on" "$(jq -c .gate "${dir}/state/fires"/*.json)"; fi
    # The canned dry-run finds no eligible issue, so each Fire ends on
    # AGENT_RUN_NO_WORK=1: 60 probe chunks to the 5h reset, then the third,
    # refusing verdict sleeps to its own reset.
    if [ "$(count '^slept 300$' "${dir}/calls.log")" = 120 ] && [ "$(sed -n 's/^slept //p' "${dir}/calls.log" | tail -1)" = 3600 ]; then
        pass "each empty-queue Fire probe-sleeps to the verdict's reset, the refusing verdict sleeps to its own"
    else fail "each empty-queue Fire probe-sleeps to the verdict's reset, the refusing verdict sleeps to its own" "$(sort "${dir}/calls.log" | uniq -c)"; fi
    rm -rf "${dir}"
}

test_loop_leaves_checkout_untouched() {
    echo "TEST: nothing under the Target Project checkout is written by the Daemon (AC 2)"
    local dir; dir="$(make_fixture_env)"
    local before; before="$(cd "${dir}/target" && find . -path ./.git -prune -o -print0 | sort -z | xargs -0 stat -c '%n %s %Y' | sha256sum)"
    printf '%s\n' "${FRESH}" "${SPENT}" > "${dir}/sensor.seq"
    run_fixture_daemon "${dir}" 2 SLEEP_POLL_MAX=0
    local after; after="$(cd "${dir}/target" && find . -path ./.git -prune -o -print0 | sort -z | xargs -0 stat -c '%n %s %Y' | sha256sum)"
    if [ -z "$(git -C "${dir}/target" status --porcelain --ignored)" ] && [ "${before}" = "${after}" ]; then
        pass "git status is clean (ignored files included) and no file changed"
    else fail "git status is clean (ignored files included) and no file changed" "$(git -C "${dir}/target" status --porcelain --ignored)"; fi
    if [ -f "${dir}/state/gate-verdict.json" ] && [ -f "${dir}/state/daemon.lock" ] && ls "${dir}/state/logs"/*.stream.jsonl >/dev/null 2>&1; then
        pass "the verdict, the lock and the Fire logs are in the State dir"
    else fail "the verdict, the lock and the Fire logs are in the State dir" "$(find "${dir}/state")"; fi
    rm -rf "${dir}"
}

test_fresh_budget_fires
test_spent_budget_sleeps_to_reset
test_poll_stops_when_budget_returns
test_unknown_reset_sleeps_degraded
test_sensor_failure_stays_alive
test_clean_run_refires_same_window
test_no_work_probe_sleeps
test_no_work_probe_wakes_early
test_no_work_probe_suppresses_baseline
test_no_work_probe_wakes_on_pr_shrink
test_exhausted_sleeps_to_fire_reset
test_exhausted_empty_reset_uses_verdict
test_model_limit_regates_once
test_failed_fire_probe_sleeps_then_caps
test_auth_dead_parks_and_reprobes
test_mode_mismatch_and_api_key
test_single_daemon_per_state_dir
test_missing_target_is_usage_error
test_state_file_names_what_the_daemon_does
test_loop_writes_one_record_per_fire
test_loop_leaves_checkout_untouched

echo ""
echo "=========================================="
echo "Ran: ${TESTS_RUN} | Failed: ${TESTS_FAILED}"
for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
echo "=========================================="
[ "${TESTS_FAILED}" -eq 0 ]
