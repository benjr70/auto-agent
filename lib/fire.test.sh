#!/usr/bin/env bash
# Tests for lib/fire.sh (and, through it, lib/host-env.sh and lib/fire-record.sh)
#
# Run: bash lib/fire.test.sh
#
# Strategy: drive `bin/auto-agent fire` and fire_run with a CLAUDE_BIN stub
# that logs its arguments, replays a canned stream-json and exits with a chosen
# code. Assert only what a reader of the State dir sees: the Fire record, the
# tap's files, the log, the stable stdout lines and the exit code.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"
FIXTURE="${ROOT_DIR}/plugin/fixtures/target-project"
CANNED="${SCRIPT_DIR}/testdata/dry-run.stream.jsonl"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# make_env [<stream-file>] [<exit-code>] -> dir with claude-stub, state/, home/
make_env() {
    local stream="${1:-${CANNED}}" code="${2:-0}"
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/state" "${dir}/home"
    cp "${stream}" "${dir}/stream.jsonl"
    cat > "${dir}/claude-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/claude.log"
pwd >> "${dir}/cwd.log"
cat "${dir}/stream.jsonl"
echo "stub stderr line" >&2
exit ${code}
STUB
    chmod +x "${dir}/claude-stub"
    echo "${dir}"
}

# run_fire <dir> [args...] : the CLI with the stub, HOME and Host env isolated
run_fire() {
    local dir="$1"; shift
    HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" \
    CLAUDE_BIN="${dir}/claude-stub" AUTO_AGENT_GATE_VERDICT_FILE= AUTO_AGENT_FIRE_MODEL= \
        bash "${CLI}" fire "$@"
}

record_of() { ls "$1"/state/fires/*.json 2>/dev/null | head -1; }

#-------------------------------------------------------------------------------
test_dry_run_from_canned_stream() {
    echo "TEST: a dry-run Fire produces a Fire record and rate-limits.json (AC 1, 3, 4)"
    local dir; dir="$(make_env)"
    local out rc; out="$(run_fire "${dir}" --dry-run)"; rc=$?
    if [ "${rc}" -eq 0 ]; then pass "exits 0"; else fail "exits 0" "rc=${rc}
${out}"; fi

    local rec; rec="$(record_of "${dir}")"
    if [ -n "${rec}" ]; then pass "a Fire record exists under state/fires"; else fail "a Fire record exists under state/fires"; return; fi
    local got
    got="$(jq -c '{kind, exit, phase, dryRun, prompt, plugin: {loaded: .plugin.loaded, listed: .plugin.skillListed, skill: .plugin.skill}, subtype: .result.subtype, isError: .result.isError, cost: .result.totalCostUsd, rl: .rateLimit.rateLimitType, gate: .gate.sensor, issue}' "${rec}")"
    local want='{"kind":"dry-run","exit":0,"phase":"claude","dryRun":true,"prompt":"/auto-agent:dry-run","plugin":{"loaded":true,"listed":true,"skill":"auto-agent:dry-run"},"subtype":"success","isError":false,"cost":0.0204001,"rl":"five_hour","gate":"none","issue":null}'
    if [ "${got}" = "${want}" ]; then pass "record carries kind, exit, plugin-loaded, result, rate limit and a gate block"
    else fail "record carries kind, exit, plugin-loaded, result, rate limit and a gate block" "${got}"; fi
    if jq -e '.startedAt and .endedAt and .fireId and .target and (.log.stream | test("\\.stream\\.jsonl$")) and (.log.stderr | test("\\.stderr\\.log$"))' "${rec}" >/dev/null; then
        pass "record carries start, end, id, target and both log paths"
    else fail "record carries start, end, id, target and both log paths" "$(cat "${rec}")"; fi
    local stream; stream="$(jq -r .log.stream "${rec}")"
    if cmp -s "${stream}" "${CANNED}"; then pass "the stream log is the raw stream, unchanged"
    else fail "the stream log is the raw stream, unchanged"; fi
    if grep -q 'stub stderr line' "$(jq -r .log.stderr "${rec}")"; then pass "claude's stderr lands in the stderr log"
    else fail "claude's stderr lands in the stderr log"; fi

    if [ "$(jq -r '.windows.five_hour.usedPct' "${dir}/state/rate-limits.json")" = "21" ] \
       && [ "$(jq -r .fireId "${dir}/state/rate-limits.json")" = "$(jq -r .fireId "${rec}")" ] \
       && [ "$(wc -l < "${dir}/state/rate-limits.jsonl")" -eq 1 ]; then
        pass "rate-limits.json holds the Fire's last event and jsonl one line"
    else fail "rate-limits.json holds the Fire's last event and jsonl one line" "$(cat "${dir}/state/rate-limits.json")"; fi

    if printf '%s\n' "${out}" | grep -q '^fire: plugin auto-agent loaded=yes skill=auto-agent:dry-run listed=yes$' \
       && printf '%s\n' "${out}" | grep -q '^fire: dry-run ok=yes$' \
       && printf '%s\n' "${out}" | grep -q "^fire: id=.* kind=dry-run exit=0 record=${rec}$"; then
        pass "stable stdout lines name the record, the plugin and the dry-run verdict"
    else fail "stable stdout lines name the record, the plugin and the dry-run verdict" "${out}"; fi
    if [ "$(cat "${dir}/cwd.log")" = "${FIXTURE}" ]; then pass "claude runs inside the fixture Target Project"
    else fail "claude runs inside the fixture Target Project" "$(cat "${dir}/cwd.log")"; fi
    rm -rf "${dir}"
}

test_dry_run_fails_when_plugin_missing_from_stream() {
    echo "TEST: a dry-run whose stream shows no plugin or no ok line fails"
    local dir; dir="$(make_env)"
    jq -c 'if .subtype == "init" then .plugins = [] | .slash_commands = ["commit"] else . end' "${CANNED}" > "${dir}/stream.jsonl"
    local out rc; out="$(run_fire "${dir}" --dry-run)"; rc=$?
    if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q 'loaded=no skill=auto-agent:dry-run listed=no' \
       && printf '%s\n' "${out}" | grep -q 'dry-run ok=no'; then
        pass "plugin absent from init: exit 1, loaded=no, ok=no"
    else fail "plugin absent from init: exit 1, loaded=no, ok=no" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    jq -c 'if .type == "result" then .result = "something else" else . end' "${CANNED}" > "${dir}/stream.jsonl"
    out="$(run_fire "${dir}" --dry-run)"; rc=$?
    if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q 'loaded=yes' && printf '%s\n' "${out}" | grep -q 'dry-run ok=no'; then
        pass "ok line missing: exit 1, loaded=yes, ok=no"
    else fail "ok line missing: exit 1, loaded=yes, ok=no" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"
}

test_tap_degrades_without_windows() {
    echo "TEST: the tap degrades to the top-level fields when unifiedWindows is absent (AC 3)"
    local dir; dir="$(make_env)"
    jq -c 'if .type == "rate_limit_event" then .rate_limit_info |= (del(.unifiedWindows) | .status = "rejected" | .utilization = 1) else . end' "${CANNED}" > "${dir}/stream.jsonl"
    run_fire "${dir}" --dry-run >/dev/null
    local got; got="$(jq -c '{source, status, rateLimitType, resetsAt, utilization, windows}' "${dir}/state/rate-limits.json")"
    local want='{"source":"binding","status":"rejected","rateLimitType":"five_hour","resetsAt":1789491600,"utilization":1,"windows":{"five_hour":{"usedPct":100,"resetsAt":1789491600,"resetsAtIso":"2026-09-15T17:00:00Z"}}}'
    if [ "${got}" = "${want}" ]; then pass "binding window recorded from status, rateLimitType, resetsAt, utilization"
    else fail "binding window recorded from status, rateLimitType, resetsAt, utilization" "${got}"; fi
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -c .rateLimit "${rec}")" = '{"status":"rejected","rateLimitType":"five_hour","resetsAt":1789491600}' ]; then
        pass "the Fire record's rateLimit block reflects the rejected event"
    else fail "the Fire record's rateLimit block reflects the rejected event" "$(jq -c .rateLimit "${rec}")"; fi
    rm -rf "${dir}"
}

test_failed_fire_still_gets_a_record() {
    echo "TEST: a failed Fire still writes a Fire record (AC 4)"
    local dir; dir="$(make_env)"
    head -2 "${CANNED}" > "${dir}/stream.jsonl"   # truncated: no result, no event
    sed -i 's/^exit 0$/exit 7/' "${dir}/claude-stub"
    local out rc; out="$(run_fire "${dir}" "${FIXTURE}")"; rc=$?
    local rec; rec="$(record_of "${dir}")"
    if [ "${rc}" -eq 7 ] && [ -n "${rec}" ]; then pass "claude's exit code is returned and a record exists"
    else fail "claude's exit code is returned and a record exists" "rc=${rc}"; return; fi
    local got; got="$(jq -c '{kind, exit, phase, prompt, loaded: .plugin.loaded, listed: .plugin.skillListed, subtype: .result.subtype, rl: .rateLimit}' "${rec}")"
    if [ "${got}" = '{"kind":"pickup","exit":7,"phase":"claude","prompt":"/auto-agent:afk-pickup","loaded":true,"listed":false,"subtype":null,"rl":null}' ]; then
        pass "record: kind pickup, exit 7, no result, no rate limit"
    else fail "record: kind pickup, exit 7, no result, no rate limit" "${got}"; fi
    if [ ! -e "${dir}/state/rate-limits.json" ]; then pass "no rate-limits.json when no event arrived"
    else fail "no rate-limits.json when no event arrived"; fi
    rm -rf "${dir}"
}

test_preflight_failure_writes_record_and_skips_claude() {
    echo "TEST: an invalid Harness config fails closed before claude runs, with a record (AC 4)"
    local dir; dir="$(make_env)"
    local target="${dir}/target"; mkdir -p "${target}/.auto-agent"
    echo '{"commit_scopes": [], "commands": {}}' > "${target}/.auto-agent/harness.json"
    local out rc; out="$(run_fire "${dir}" "${target}" 2>&1)"; rc=$?
    local rec; rec="$(record_of "${dir}")"
    if [ "${rc}" -eq 1 ] && [ -n "${rec}" ] && [ ! -e "${dir}/claude.log" ]; then pass "exit 1, record written, claude never invoked"
    else fail "exit 1, record written, claude never invoked" "rc=${rc} rec=${rec} claude.log=$(cat "${dir}/claude.log" 2>/dev/null)"; return; fi
    local got; got="$(jq -c '{exit, phase, kind, loaded: .plugin.loaded, gate: .gate.shouldFire}' "${rec}")"
    if [ "${got}" = '{"exit":1,"phase":"preflight","kind":"pickup","loaded":false,"gate":true}' ]; then pass "record says phase preflight"
    else fail "record says phase preflight" "${got}"; fi
    if printf '%s\n' "${out}" | grep -q 'does not match the Harness config schema'; then pass "the schema error is printed"
    else fail "the schema error is printed" "${out}"; fi
    rm -rf "${dir}"
}

test_settings_and_plugin_flags_on_every_invocation() {
    echo "TEST: --plugin-dir, --settings baseline, stream-json and bypass are passed on every Fire (AC 1, 2)"
    local dir; dir="$(make_env)"
    run_fire "${dir}" --dry-run >/dev/null
    run_fire "${dir}" "${FIXTURE}" >/dev/null
    HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" \
        CLAUDE_BIN="${dir}/claude-stub" AUTO_AGENT_FIRE_MODEL=claude-opus-5 bash "${CLI}" fire --dry-run >/dev/null
    local n; n="$(wc -l < "${dir}/claude.log")"
    local ok=0 line
    while IFS= read -r line; do
        case "${line}" in
            *"--print"*"--permission-mode bypassPermissions"*"--plugin-dir ${ROOT_DIR}/plugin"*"--settings ${ROOT_DIR}/plugin/settings/baseline.json"*"--output-format stream-json"*"--verbose"*) ok=$((ok + 1)) ;;
        esac
    done < "${dir}/claude.log"
    if [ "${n}" -eq 3 ] && [ "${ok}" -eq 3 ]; then pass "all 3 invocations carry the flags"
    else fail "all 3 invocations carry the flags" "$(cat "${dir}/claude.log")"; fi
    if grep -q -- '--model claude-opus-5 /auto-agent:dry-run$' "${dir}/claude.log" && [ "$(grep -c -- '--model' "${dir}/claude.log")" -eq 1 ]; then
        pass "AUTO_AGENT_FIRE_MODEL pins --model; unset means no --model"
    else fail "AUTO_AGENT_FIRE_MODEL pins --model; unset means no --model" "$(cat "${dir}/claude.log")"; fi
    if grep -q '/auto-agent:afk-pickup$' "${dir}/claude.log"; then pass "a plain Fire prompts the namespaced pickup skill"
    else fail "a plain Fire prompts the namespaced pickup skill"; fi
    if [ "$(ls "${dir}/state/fires" | wc -l)" -eq 3 ]; then pass "three Fires, three records"
    else fail "three Fires, three records" "$(ls "${dir}/state/fires")"; fi
    rm -rf "${dir}"
}

test_settings_baseline_shape() {
    echo "TEST: the settings baseline carries env, permissions and a deny list (AC 2)"
    local f="${ROOT_DIR}/plugin/settings/baseline.json"
    if jq -e '.env | type == "object" and length > 0' "${f}" >/dev/null \
       && jq -e '.permissions.allow | type == "array" and length > 0' "${f}" >/dev/null \
       && jq -e '.permissions.deny | type == "array" and (index("Bash(git push --force*)") != null) and (index("Bash(git reset --hard*)") != null)' "${f}" >/dev/null; then
        pass "env, allow and deny (force-push and reset --hard denied)"
    else fail "env, allow and deny (force-push and reset --hard denied)"; fi
    if jq -e '.env.CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS == "0"' "${f}" >/dev/null; then pass "background-task ceiling lifted for --print (carried over from agent-run)"
    else fail "background-task ceiling lifted for --print (carried over from agent-run)"; fi
}

test_state_dir_from_host_env_and_default() {
    echo "TEST: the State dir comes from the Host env and defaults outside the checkout (AC 5)"
    local dir; dir="$(make_env)"
    # No AUTO_AGENT_STATE_DIR anywhere: default under $HOME/.local/state.
    ( unset AUTO_AGENT_STATE_DIR; HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/missing.env" CLAUDE_BIN="${dir}/claude-stub" bash "${CLI}" fire --dry-run >/dev/null )
    if [ "$(ls "${dir}/home/.local/state/auto-agent/fires" 2>/dev/null | wc -l)" -eq 1 ]; then pass "default is \$HOME/.local/state/auto-agent"
    else fail "default is \$HOME/.local/state/auto-agent" "$(find "${dir}/home" -type f)"; fi
    # The Host env file names it.
    printf '# Host env\nAUTO_AGENT_STATE_DIR="%s/from-host-env"\nCLAUDE_AUTH_MODE=login\n' "${dir}" > "${dir}/host.env"
    ( unset AUTO_AGENT_STATE_DIR; HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" CLAUDE_BIN="${dir}/claude-stub" bash "${CLI}" fire --dry-run >/dev/null )
    local rec; rec="$(ls "${dir}/from-host-env/fires/"*.json 2>/dev/null | head -1)"
    if [ -n "${rec}" ]; then pass "AUTO_AGENT_STATE_DIR in the Host env file is honoured (quotes stripped)"
    else fail "AUTO_AGENT_STATE_DIR in the Host env file is honoured (quotes stripped)"; return; fi
    if [ "$(jq -r .gate.authMode "${rec}")" = "login" ]; then pass "CLAUDE_AUTH_MODE from the Host env reaches the gate block"
    else fail "CLAUDE_AUTH_MODE from the Host env reaches the gate block" "$(jq -c .gate "${rec}")"; fi
    # The environment wins over the file.
    ( HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" CLAUDE_BIN="${dir}/claude-stub" bash "${CLI}" fire --dry-run >/dev/null )
    if [ "$(ls "${dir}/state/fires" | wc -l)" -eq 1 ] && [ "$(ls "${dir}/from-host-env/fires" | wc -l)" -eq 1 ]; then pass "an exported AUTO_AGENT_STATE_DIR wins over the Host env file"
    else fail "an exported AUTO_AGENT_STATE_DIR wins over the Host env file"; fi
    rm -rf "${dir}"
}

test_gate_verdict_file_is_embedded() {
    echo "TEST: a Gate verdict handed to the Fire is embedded verbatim"
    local dir; dir="$(make_env)"
    echo '{"authMode":"setup-token","sensor":"stream-events","state":"stale","remainPct":79,"resetAt":null,"shouldFire":true,"observedAt":"2026-09-15T00:00:00Z","limits":[],"warnings":[]}' > "${dir}/gate.json"
    HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" \
        CLAUDE_BIN="${dir}/claude-stub" AUTO_AGENT_GATE_VERDICT_FILE="${dir}/gate.json" bash "${CLI}" fire --dry-run >/dev/null
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -c '.gate | {sensor, remainPct}' "${rec}")" = '{"sensor":"stream-events","remainPct":79}' ]; then pass "gate block is the supplied verdict"
    else fail "gate block is the supplied verdict" "$(jq -c .gate "${rec}")"; fi
    rm -rf "${dir}"
}

test_picked_issue_lands_in_record() {
    echo "TEST: a picked: #N line in the assistant text sets the record's issue"
    local dir; dir="$(make_env)"
    jq -c 'if .type == "assistant" then .message.content[0].text = "picked:   #291 feat: thing\nworking" else . end' "${CANNED}" > "${dir}/stream.jsonl"
    run_fire "${dir}" "${FIXTURE}" >/dev/null
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -r .issue "${rec}")" = "291" ]; then pass "issue 291 recorded"
    else fail "issue 291 recorded" "$(jq -c '{issue}' "${rec}")"; fi
    rm -rf "${dir}"
}

test_usage_errors() {
    echo "TEST: usage errors"
    local dir; dir="$(make_env)"
    local rc
    ( unset AUTO_AGENT_TARGET_DIR; run_fire "${dir}" >/dev/null 2>&1 ); rc=$?
    if [ "${rc}" -eq 2 ]; then pass "no target and no Host env target: exit 2"; else fail "no target and no Host env target: exit 2" "rc=${rc}"; fi
    run_fire "${dir}" --bogus >/dev/null 2>&1; rc=$?
    if [ "${rc}" -eq 2 ]; then pass "unknown option: exit 2"; else fail "unknown option: exit 2" "rc=${rc}"; fi
    run_fire "${dir}" "${dir}/nope" >/dev/null 2>&1; rc=$?
    if [ "${rc}" -eq 2 ]; then pass "missing target dir: exit 2"; else fail "missing target dir: exit 2" "rc=${rc}"; fi
    if [ ! -e "${dir}/claude.log" ]; then pass "claude never ran"; else fail "claude never ran"; fi
    rm -rf "${dir}"
}

test_dry_run_from_canned_stream
test_dry_run_fails_when_plugin_missing_from_stream
test_tap_degrades_without_windows
test_failed_fire_still_gets_a_record
test_preflight_failure_writes_record_and_skips_claude
test_settings_and_plugin_flags_on_every_invocation
test_settings_baseline_shape
test_state_dir_from_host_env_and_default
test_gate_verdict_file_is_embedded
test_picked_issue_lands_in_record
test_usage_errors

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
