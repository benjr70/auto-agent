#!/usr/bin/env bash
# Tests for lib/fire.sh (and, through it, lib/host-env.sh and lib/fire-record.sh)
#
# Run: bash lib/fire.test.sh
#
# Strategy: drive `bin/auto-agent fire` with a CLAUDE_BIN stub that logs its
# arguments and environment, replays a canned stream-json and exits with a
# chosen code; GH_BIN and GIT_BIN stubs answer the config load, log the
# checkout hygiene and record every lock write. Assert only what a reader of
# the State dir and the stable stdout lines sees: the Fire record, the tap's
# files, the log, the lines, the exit code, and which gh/git calls were made.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"
FIXTURE="${ROOT_DIR}/plugin/fixtures/target-project"
CANNED_NOOP="${SCRIPT_DIR}/testdata/dry-run.stream.jsonl"
CANNED_PICKUP="${SCRIPT_DIR}/testdata/pickup-dry-run.stream.jsonl"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# make_env [<stream-file>] [<exit-code>] -> dir with claude-stub, gh-stub, git-stub, state/, home/
# The gh stub answers `repo view` with the branch in branch.out and logs every
# other call (lock writes) to gh.log; the git stub logs every call to git.log
# and answers `remote get-url origin` with a fixed slug.
make_env() {
    local stream="${1:-${CANNED_PICKUP}}" code="${2:-0}"
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/state" "${dir}/home"
    cp "${stream}" "${dir}/stream.jsonl"
    cat > "${dir}/claude-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/claude.log"
pwd >> "${dir}/cwd.log"
env | grep -E '^(AUTO_AGENT_ROOT|AUTO_AGENT_TARGET_DIR|AUTO_AGENT_STATE_DIR|HARNESS_CONFIG_JSON)=' >> "${dir}/claude.env"
cat "${dir}/stream.jsonl"
echo "stub stderr line" >&2
exit ${code}
STUB
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/gh.log"
case "\$*" in
    "repo view "*) cat "${dir}/branch.out"; exit \$(cat "${dir}/branch.code") ;;
    *) exit 0 ;;
esac
STUB
    cat > "${dir}/git-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/git.log"
case "\$*" in
    *"remote get-url origin"*) echo 'https://github.com/acme/widgets.git'; exit 0 ;;
    *" checkout "*) exit \$(cat "${dir}/checkout.code") ;;
    *" diff --cached --quiet"*) exit \$(cat "${dir}/diff.code") ;;
    *) exit 0 ;;
esac
STUB
    chmod +x "${dir}/claude-stub" "${dir}/gh-stub" "${dir}/git-stub"
    echo main > "${dir}/branch.out"; echo 0 > "${dir}/branch.code"; echo 0 > "${dir}/checkout.code"; echo 0 > "${dir}/diff.code"
    echo "${dir}"
}

# run_fire <dir> [args...] : the CLI with the stubs, HOME and Host env isolated
run_fire() {
    local dir="$1"; shift
    HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" \
    CLAUDE_BIN="${dir}/claude-stub" GH_BIN="${dir}/gh-stub" GIT_BIN="${dir}/git-stub" \
    AUTO_AGENT_GATE_VERDICT_FILE= AUTO_AGENT_FIRE_MODEL= HARNESS_CONFIG_JSON= \
        bash "${CLI}" fire "$@"
}

record_of() { ls "$1"/state/fires/*.json 2>/dev/null | head -1; }

# with_text <dir> <canned> <text> : replay <canned> with the assistant and result text replaced
with_text() {
    jq -c --arg t "$3" 'if .type == "assistant" then .message.content[0].text = $t elif .type == "result" then .result = $t else . end' "$2" > "$1/stream.jsonl"
}

#-------------------------------------------------------------------------------
test_noop_from_canned_stream() {
    echo "TEST: a --noop Fire produces a Fire record and rate-limits.json (AC 1, 3, 4)"
    local dir; dir="$(make_env "${CANNED_NOOP}")"
    local out rc; out="$(run_fire "${dir}" --noop)"; rc=$?
    if [ "${rc}" -eq 0 ]; then pass "exits 0"; else fail "exits 0" "rc=${rc}
${out}"; fi

    local rec; rec="$(record_of "${dir}")"
    if [ -n "${rec}" ]; then pass "a Fire record exists under state/fires"; else fail "a Fire record exists under state/fires"; return; fi
    local got
    got="$(jq -c '{kind, exit, phase, dryRun, prompt, plugin: {loaded: .plugin.loaded, listed: .plugin.skillListed, skill: .plugin.skill}, subtype: .result.subtype, isError: .result.isError, cost: .result.totalCostUsd, rl: .rateLimit.rateLimitType, gate: .gate.sensor, issue, work: .work.kind}' "${rec}")"
    local want='{"kind":"noop","exit":0,"phase":"claude","dryRun":true,"prompt":"/auto-agent:dry-run","plugin":{"loaded":true,"listed":true,"skill":"auto-agent:dry-run"},"subtype":"success","isError":false,"cost":0.0204001,"rl":"five_hour","gate":"none","issue":null,"work":null}'
    if [ "${got}" = "${want}" ]; then pass "record carries kind, exit, plugin-loaded, result, rate limit, a gate block and a work block"
    else fail "record carries kind, exit, plugin-loaded, result, rate limit, a gate block and a work block" "${got}"; fi
    if jq -e '.startedAt and .endedAt and .fireId and .target and (.log.stream | test("\\.stream\\.jsonl$")) and (.log.stderr | test("\\.stderr\\.log$"))' "${rec}" >/dev/null; then
        pass "record carries start, end, id, target and both log paths"
    else fail "record carries start, end, id, target and both log paths" "$(cat "${rec}")"; fi
    local stream; stream="$(jq -r .log.stream "${rec}")"
    if cmp -s "${stream}" "${CANNED_NOOP}"; then pass "the stream log is the raw stream, unchanged"
    else fail "the stream log is the raw stream, unchanged"; fi
    if grep -q 'stub stderr line' "$(jq -r .log.stderr "${rec}")"; then pass "claude's stderr lands in the stderr log"
    else fail "claude's stderr lands in the stderr log"; fi
    if [ "$(jq -r '.windows.five_hour.usedPct' "${dir}/state/rate-limits.json")" = "21" ] \
       && [ "$(jq -r .fireId "${dir}/state/rate-limits.json")" = "$(jq -r .fireId "${rec}")" ] \
       && [ "$(wc -l < "${dir}/state/rate-limits.jsonl")" -eq 1 ]; then
        pass "rate-limits.json holds the Fire's last event and jsonl one line"
    else fail "rate-limits.json holds the Fire's last event and jsonl one line" "$(cat "${dir}/state/rate-limits.json")"; fi

    if printf '%s\n' "${out}" | grep -q '^fire: plugin auto-agent loaded=yes skill=auto-agent:dry-run listed=yes$' \
       && printf '%s\n' "${out}" | grep -q '^fire: noop ok=yes$' \
       && printf '%s\n' "${out}" | grep -q "^fire: id=.* kind=noop exit=0 record=${rec}$"; then
        pass "stable stdout lines name the record, the plugin and the noop verdict"
    else fail "stable stdout lines name the record, the plugin and the noop verdict" "${out}"; fi
    if [ "$(cat "${dir}/cwd.log")" = "${FIXTURE}" ]; then pass "claude runs inside the fixture Target Project"
    else fail "claude runs inside the fixture Target Project" "$(cat "${dir}/cwd.log")"; fi
    if [ ! -e "${dir}/gh.log" ] && [ ! -e "${dir}/git.log" ]; then pass "a noop Fire makes no gh or git call"
    else fail "a noop Fire makes no gh or git call" "gh: $(cat "${dir}/gh.log" 2>/dev/null) git: $(cat "${dir}/git.log" 2>/dev/null)"; fi
    rm -rf "${dir}"
}

test_noop_fails_when_plugin_missing_from_stream() {
    echo "TEST: a noop whose stream shows no plugin or no ok line fails"
    local dir; dir="$(make_env "${CANNED_NOOP}")"
    jq -c 'if .subtype == "init" then .plugins = [] | .slash_commands = ["commit"] else . end' "${CANNED_NOOP}" > "${dir}/stream.jsonl"
    local out rc; out="$(run_fire "${dir}" --noop)"; rc=$?
    if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q 'loaded=no skill=auto-agent:dry-run listed=no' \
       && printf '%s\n' "${out}" | grep -q 'noop ok=no'; then
        pass "plugin absent from init: exit 1, loaded=no, ok=no"
    else fail "plugin absent from init: exit 1, loaded=no, ok=no" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"

    dir="$(make_env "${CANNED_NOOP}")"
    jq -c 'if .type == "result" then .result = "something else" else . end' "${CANNED_NOOP}" > "${dir}/stream.jsonl"
    out="$(run_fire "${dir}" --noop)"; rc=$?
    if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q 'loaded=yes' && printf '%s\n' "${out}" | grep -q 'noop ok=no'; then
        pass "ok line missing: exit 1, loaded=yes, ok=no"
    else fail "ok line missing: exit 1, loaded=yes, ok=no" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"
}

test_dry_run_pickup_reports_no_work() {
    echo "TEST: a dry-run Fire runs the pickup skill's dry-run and ends on AGENT_RUN_NO_WORK (issue #28 AC 4)"
    local dir; dir="$(make_env)"
    local out rc; out="$(run_fire "${dir}" --dry-run)"; rc=$?
    if [ "${rc}" -eq 0 ]; then pass "exits 0"; else fail "exits 0" "rc=${rc}
${out}"; fi
    local rec; rec="$(record_of "${dir}")"
    local got; got="$(jq -c '{kind, dryRun, prompt, listed: .plugin.skillListed, work: .work.kind, line: .work.line, issue}' "${rec}")"
    if [ "${got}" = '{"kind":"dry-run","dryRun":true,"prompt":"/auto-agent:afk-pickup --dry-run","listed":true,"work":"none","line":"afk-pickup: no eligible issue","issue":null}' ]; then
        pass "record: kind dry-run, the pickup skill prompted with --dry-run, work none"
    else fail "record: kind dry-run, the pickup skill prompted with --dry-run, work none" "${got}"; fi
    if grep -q '/auto-agent:afk-pickup --dry-run$' "${dir}/claude.log"; then pass "claude was prompted with the namespaced pickup skill and --dry-run"
    else fail "claude was prompted with the namespaced pickup skill and --dry-run" "$(cat "${dir}/claude.log")"; fi
    if printf '%s\n' "${out}" | grep -q '^fire: work=none$' && printf '%s\n' "${out}" | grep -q '^AGENT_RUN_NO_WORK=1$' && printf '%s\n' "${out}" | grep -q '^fire: dry-run ok=yes$'; then
        pass "stable lines: work=none, AGENT_RUN_NO_WORK=1, dry-run ok=yes"
    else fail "stable lines: work=none, AGENT_RUN_NO_WORK=1, dry-run ok=yes" "${out}"; fi
    if grep -q '^HARNESS_CONFIG_JSON=' "${dir}/claude.env" && grep -q "^AUTO_AGENT_TARGET_DIR=${FIXTURE}$" "${dir}/claude.env" \
       && grep -q "^AUTO_AGENT_STATE_DIR=${dir}/state$" "${dir}/claude.env" && grep -q "^AUTO_AGENT_ROOT=${ROOT_DIR}$" "${dir}/claude.env"; then
        pass "the resolved config, root, target and State dir are exported into the Fire"
    else fail "the resolved config, root, target and State dir are exported into the Fire" "$(cat "${dir}/claude.env")"; fi
    if [ "$(grep -o '"default_branch":"[a-z]*"' "${dir}/claude.env")" = '"default_branch":"main"' ]; then pass "HARNESS_CONFIG_JSON carries the detected default branch"
    else fail "HARNESS_CONFIG_JSON carries the detected default branch" "$(cat "${dir}/claude.env")"; fi
    if [ "$(grep -c 'repo view acme/widgets' "${dir}/gh.log")" -eq 1 ] && [ "$(wc -l < "${dir}/gh.log")" -eq 1 ]; then pass "exactly one gh call: the default branch"
    else fail "exactly one gh call: the default branch" "$(cat "${dir}/gh.log")"; fi
    if ! grep -qE 'checkout|reset|fetch' "${dir}/git.log"; then pass "a dry-run does no checkout hygiene"
    else fail "a dry-run does no checkout hygiene" "$(cat "${dir}/git.log")"; fi
    rm -rf "${dir}"
}

test_dry_run_pickup_reports_a_would_pick() {
    echo "TEST: a dry-run that would pick reports the pick and no AGENT_RUN_NO_WORK (issue #28 AC 4)"
    local dir; dir="$(make_env)"
    with_text "${dir}" "${CANNED_PICKUP}" "=== /auto-agent:afk-pickup 2026-09-15T13:00:00Z ===
picked:   #30 Budget gate per auth mode
afk-pickup: would-pick #30 Budget gate per auth mode"
    local out rc; out="$(run_fire "${dir}" --dry-run)"; rc=$?
    if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^fire: work=dry-run afk-pickup: would-pick #30 Budget gate per auth mode$' \
       && ! printf '%s\n' "${out}" | grep -q 'AGENT_RUN_NO_WORK' && printf '%s\n' "${out}" | grep -q '^fire: dry-run ok=yes$'; then
        pass "work line names the would-pick, no no-work line, ok=yes"
    else fail "work line names the would-pick, no no-work line, ok=yes" "rc=${rc}
${out}"; fi
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -c '[.work.kind, .work.issue, .issue]' "${rec}")" = '["dry-run",30,30]' ]; then pass "record work: dry-run, issue 30"
    else fail "record work: dry-run, issue 30" "$(jq -c .work "${rec}")"; fi
    if [ ! -s "${dir}/gh.log" ] || [ "$(grep -vc 'repo view' "${dir}/gh.log")" -eq 0 ]; then pass "no gh write happened"
    else fail "no gh write happened" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"
}

test_resolve_dry_run_reports_a_would_open() {
    echo "TEST: a --resolve-dry-run Fire prompts the resolve skill's dry-run and reports its would-open line (issue #29 AC 1)"
    local dir; dir="$(make_env)"
    with_text "${dir}" "${CANNED_PICKUP}" "resolve: #13 research research-claude-auth-modes
afk-resolve: would-open PR research/research-claude-auth-modes (docs/research/reusable-auto-agent/research-claude-auth-modes.md)"
    # the canned init predates the resolve skill: list it, as a Harness install carrying #29 does
    jq -c 'if .type == "system" and .subtype == "init" then .slash_commands += ["auto-agent:afk-resolve"] else . end' "${dir}/stream.jsonl" > "${dir}/s2" && mv "${dir}/s2" "${dir}/stream.jsonl"
    local out rc; out="$(run_fire "${dir}" --resolve-dry-run 13)"; rc=$?
    if grep -q '/auto-agent:afk-resolve --issue 13 --dry-run$' "${dir}/claude.log"; then pass "claude was prompted with the namespaced resolve skill, the issue and --dry-run"
    else fail "claude was prompted with the namespaced resolve skill, the issue and --dry-run" "$(cat "${dir}/claude.log")"; fi
    if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^fire: work=dry-run afk-resolve: would-open PR research/research-claude-auth-modes (docs/research/reusable-auto-agent/research-claude-auth-modes.md)$' \
       && ! printf '%s\n' "${out}" | grep -q 'AGENT_RUN_NO_WORK' && printf '%s\n' "${out}" | grep -q '^fire: dry-run ok=yes$' \
       && printf '%s\n' "${out}" | grep -q '^fire: plugin auto-agent loaded=yes skill=auto-agent:afk-resolve listed=yes$'; then
        pass "work line names the would-open, ok=yes, the resolve skill listed"
    else fail "work line names the would-open, ok=yes, the resolve skill listed" "rc=${rc}
${out}"; fi
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -c '[.kind, .dryRun, .prompt, .work.kind, .work.issue, .work.slug, .issue]' "${rec}")" = '["resolve-dry-run",true,"/auto-agent:afk-resolve --issue 13 --dry-run","dry-run",13,"research-claude-auth-modes",13]' ]; then pass "record: kind resolve-dry-run, work dry-run on issue 13 with the slug"
    else fail "record: kind resolve-dry-run, work dry-run on issue 13 with the slug" "$(jq -c '{kind, dryRun, prompt, work}' "${rec}")"; fi
    if [ ! -s "${dir}/gh.log" ] || [ "$(grep -vc 'repo view' "${dir}/gh.log")" -eq 0 ]; then pass "no gh write happened"
    else fail "no gh write happened" "$(cat "${dir}/gh.log")"; fi
    if ! grep -qE 'checkout|reset|fetch' "${dir}/git.log"; then pass "a resolve dry-run does no checkout hygiene"
    else fail "a resolve dry-run does no checkout hygiene" "$(cat "${dir}/git.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    with_text "${dir}" "${CANNED_PICKUP}" "resolve: #13 research research-claude-auth-modes
afk-resolve: would-fail #13 no-sources"
    jq -c 'if .type == "system" and .subtype == "init" then .slash_commands += ["auto-agent:afk-resolve"] else . end' "${dir}/stream.jsonl" > "${dir}/s2" && mv "${dir}/s2" "${dir}/stream.jsonl"
    out="$(run_fire "${dir}" --resolve-dry-run 13)"; rc=$?
    if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^fire: work=dry-run afk-resolve: would-fail #13 no-sources$' && printf '%s\n' "${out}" | grep -q '^fire: dry-run ok=yes$'; then
        pass "a would-fail dry run still reached a verdict: ok=yes with the line reported"
    else fail "a would-fail dry run still reached a verdict: ok=yes with the line reported" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    with_text "${dir}" "${CANNED_PICKUP}" "I read the ticket but ran out of ideas."
    out="$(run_fire "${dir}" --resolve-dry-run 13)"; rc=$?
    if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^fire: work=unknown$' && printf '%s\n' "${out}" | grep -q '^fire: dry-run ok=no$'; then
        pass "a resolve dry-run with no would- line fails as work=unknown"
    else fail "a resolve dry-run with no would- line fails as work=unknown" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"

    dir="$(make_env)"
    run_fire "${dir}" --resolve-dry-run >/dev/null 2>&1; rc=$?
    if [ "${rc}" -eq 2 ]; then pass "--resolve-dry-run without an issue number is a usage error"
    else fail "--resolve-dry-run without an issue number is a usage error" "rc=${rc}"; fi
    run_fire "${dir}" --resolve-dry-run abc >/dev/null 2>&1; rc=$?
    if [ "${rc}" -eq 2 ]; then pass "--resolve-dry-run with a non-number is a usage error"
    else fail "--resolve-dry-run with a non-number is a usage error" "rc=${rc}"; fi
    rm -rf "${dir}"
}

test_dry_run_fails_without_a_verdict_line() {
    echo "TEST: a dry-run that never reached a pickup verdict, or lost the plugin, fails"
    local dir; dir="$(make_env)"
    with_text "${dir}" "${CANNED_PICKUP}" "I could not decide."
    local out rc; out="$(run_fire "${dir}" --dry-run)"; rc=$?
    if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^fire: work=unknown$' && printf '%s\n' "${out}" | grep -q '^fire: dry-run ok=no$'; then
        pass "no verdict line: exit 1, work=unknown, ok=no"
    else fail "no verdict line: exit 1, work=unknown, ok=no" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"
    dir="$(make_env)"
    with_text "${dir}" "${CANNED_PICKUP}" "afk-pickup: would-pick #30 Budget gate"
    out="$(run_fire "${dir}" --dry-run)"; rc=$?
    if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^fire: work=dry-run afk-pickup: would-pick #30 Budget gate$' && printf '%s\n' "${out}" | grep -q '^fire: dry-run ok=no$'; then
        pass "a would-line without the picked: block line: work reported, ok=no (issue #28 AC 4)"
    else fail "a would-line without the picked: block line: work reported, ok=no (issue #28 AC 4)" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"
    dir="$(make_env)"
    jq -c 'if .subtype == "init" then .plugins = [] | .slash_commands = ["commit"] else . end' "${CANNED_PICKUP}" > "${dir}/stream.jsonl"
    out="$(run_fire "${dir}" --dry-run)"; rc=$?
    if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q 'loaded=no skill=auto-agent:afk-pickup listed=no' && printf '%s\n' "${out}" | grep -q 'dry-run ok=no'; then
        pass "plugin absent: exit 1, loaded=no, ok=no"
    else fail "plugin absent: exit 1, loaded=no, ok=no" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"
}

test_pickup_fire_resets_the_checkout_first() {
    echo "TEST: a plain Fire puts the checkout on the tip of the detected default branch before the skill runs"
    local dir; dir="$(make_env)"
    echo trunk > "${dir}/branch.out"
    local out rc; out="$(run_fire "${dir}" "${FIXTURE}")"; rc=$?
    local want="-C ${FIXTURE} fetch --quiet origin trunk
-C ${FIXTURE} reset --hard --quiet
-C ${FIXTURE} checkout --quiet trunk
-C ${FIXTURE} reset --hard --quiet origin/trunk"
    if [ "${rc}" -eq 0 ] && [ "$(grep -v 'remote get-url' "${dir}/git.log")" = "${want}" ]; then pass "fetch, reset, checkout, reset to origin, on the detected branch"
    else fail "fetch, reset, checkout, reset to origin, on the detected branch" "rc=${rc}
$(cat "${dir}/git.log")"; fi
    if printf '%s\n' "${out}" | grep -q '^fire: checkout reset to trunk$'; then pass "the reset is reported"
    else fail "the reset is reported" "${out}"; fi
    local order; order="$(grep -nE 'checkout --quiet' "${dir}/git.log" | cut -d: -f1)"
    if [ -n "${order}" ] && [ -s "${dir}/claude.log" ]; then pass "hygiene ran before claude"; else fail "hygiene ran before claude"; fi
    if printf '%s\n' "${out}" | grep -q '^fire: work=none$' && printf '%s\n' "${out}" | grep -q '^AGENT_RUN_NO_WORK=1$'; then
        pass "an empty queue prints work=none and AGENT_RUN_NO_WORK=1"
    else fail "an empty queue prints work=none and AGENT_RUN_NO_WORK=1" "${out}"; fi
    rm -rf "${dir}"

    dir="$(make_env)"; echo 1 > "${dir}/checkout.code"
    out="$(run_fire "${dir}" "${FIXTURE}" 2>&1)"; rc=$?
    local rec; rec="$(record_of "${dir}")"
    if [ "${rc}" -eq 2 ] && [ ! -e "${dir}/claude.log" ] && [ "$(jq -c '{exit, phase}' "${rec}")" = '{"exit":2,"phase":"preflight"}' ] && printf '%s\n' "${out}" | grep -q 'cannot check out'; then
        pass "a checkout that fails is a preflight failure: exit 2, record, claude never ran"
    else fail "a checkout that fails is a preflight failure: exit 2, record, claude never ran" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"
}

test_crashed_pick_clears_its_lock() {
    echo "TEST: a crashed pick Fire clears the lock it took: AFK:in-progress -> AFK:failed with a comment (issue #28)"
    local dir; dir="$(make_env "${CANNED_PICKUP}" 7)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #291 feat: thing
working on it"
    local out rc; out="$(run_fire "${dir}" "${FIXTURE}")"; rc=$?
    if [ "${rc}" -eq 7 ]; then pass "claude's exit code is returned"; else fail "claude's exit code is returned" "rc=${rc}"; fi
    if grep -q '^issue edit 291 --repo acme/widgets --remove-label AFK:in-progress --add-label AFK:failed$' "${dir}/gh.log" \
       && grep -q '^issue comment 291 --repo acme/widgets --body Fire failed at .*Lock cleared (AFK:in-progress -> AFK:failed)' "${dir}/gh.log"; then
        pass "lock flipped to AFK:failed on the configured repo and a comment posted"
    else fail "lock flipped to AFK:failed on the configured repo and a comment posted" "$(cat "${dir}/gh.log")"; fi
    if printf '%s\n' "${out}" | grep -q '^fire: work=pick #291$' && printf '%s\n' "${out}" | grep -q '^fire: lock pick #291 AFK:in-progress -> AFK:failed$'; then
        pass "the work and lock lines say what happened"
    else fail "the work and lock lines say what happened" "${out}"; fi
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -c '[.exit, .issue, .work.kind]' "${rec}")" = '[7,291,"pick"]' ]; then pass "record: exit 7, issue 291, work pick"
    else fail "record: exit 7, issue 291, work pick" "$(jq -c '{exit, issue, work}' "${rec}")"; fi
    rm -rf "${dir}"
}

test_exhausted_pick_pauses() {
    echo "TEST: an EXHAUSTED pick Fire pauses its work: wip commit, AFK:in-progress -> AFK:paused, exit 0 (issue #30)"
    local dir; dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #291 feat: thing
You've hit your session limit · resets 10:50pm (America/New_York)"
    echo 1 > "${dir}/diff.code"   # partial edits are staged
    local out rc; out="$(EC_NOW=1789430400 run_fire "${dir}" "${FIXTURE}")"; rc=$?
    if [ "${rc}" -eq 0 ]; then pass "an exhausted Fire exits 0 (paused, not failed)"; else fail "an exhausted Fire exits 0 (paused, not failed)" "rc=${rc}
${out}"; fi
    if grep -q '^issue edit 291 --repo acme/widgets --remove-label AFK:in-progress --add-label AFK:paused$' "${dir}/gh.log" \
       && grep -q '^issue comment 291 --repo acme/widgets --body Fire paused at .*usage exhausted mid-Fire. Branch kept for resume. Budget resets at 2026-09-15T02:50:00.000Z.$' "${dir}/gh.log" \
       && ! grep -q 'AFK:failed' "${dir}/gh.log"; then
        pass "lock flipped to AFK:paused with the pause comment, never AFK:failed"
    else fail "lock flipped to AFK:paused with the pause comment, never AFK:failed" "$(cat "${dir}/gh.log")"; fi
    if grep -q "^-C ${FIXTURE} add -A$" "${dir}/git.log" && grep -q "^-C ${FIXTURE} commit --quiet -m wip: freeze partial work on #291 (usage exhausted)$" "${dir}/git.log"; then
        pass "partial work frozen in a wip: commit"
    else fail "partial work frozen in a wip: commit" "$(cat "${dir}/git.log")"; fi
    if printf '%s\n' "${out}" | grep -q '^AGENT_RUN_RESET_AT=2026-09-15T02:50:00.000Z$' \
       && printf '%s\n' "${out}" | grep -q '^fire: lock pick #291 AFK:in-progress -> AFK:paused (usage exhausted)$' \
       && printf '%s\n' "${out}" | grep -q '^fire: outcome EXHAUSTED source=limit-strings limit=session resetAt=2026-09-15T02:50:00.000Z$'; then
        pass "the reset instant is on a stable line for the Daemon"
    else fail "the reset instant is on a stable line for the Daemon" "${out}"; fi
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -c '[.exit, .outcome.status, .outcome.source, .outcome.limitType, .outcome.resetAt]' "${rec}")" = '[1,"EXHAUSTED","limit-strings","session","2026-09-15T02:50:00.000Z"]' ]; then pass "record: the outcome block"
    else fail "record: the outcome block" "$(jq -c .outcome "${rec}")"; fi
    rm -rf "${dir}"
    # Nothing staged: no wip commit, still paused.
    dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #291 feat: thing
Claude AI usage limit reached|1789449000"
    run_fire "${dir}" "${FIXTURE}" >/dev/null
    if ! grep -q ' commit ' "${dir}/git.log" && grep -q 'add-label AFK:paused' "${dir}/gh.log"; then pass "a clean tree pauses without a commit"
    else fail "a clean tree pauses without a commit" "$(cat "${dir}/git.log")"; fi
    rm -rf "${dir}"
}

test_rejected_event_is_the_outcome() {
    echo "TEST: a rejected rate-limit event in this Fire's stream is the exhaustion signal, its resetsAt the reset (ADR 0008 addendum)"
    local dir; dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #291 feat: thing
API Error: something went wrong"
    echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789449000,"rateLimitType":"seven_day","utilization":0.99},"session_id":"s1"}' >> "${dir}/stream.jsonl"
    local out; out="$(run_fire "${dir}" "${FIXTURE}")"
    if printf '%s\n' "${out}" | grep -q '^AGENT_RUN_RESET_AT=2026-09-15T05:10:00Z$' && printf '%s\n' "${out}" | grep -q '^fire: outcome EXHAUSTED source=stream-events'; then
        pass "the event's reset is the Daemon's reset"
    else fail "the event's reset is the Daemon's reset" "${out}"; fi
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -c '[.outcome.status, .outcome.source, .outcome.limitType, .rateLimit.status]' "${rec}")" = '["EXHAUSTED","stream-events","seven_day","rejected"]' ]; then pass "record: outcome from stream-events, the last event rejected"
    else fail "record: outcome from stream-events, the last event rejected" "$(jq -c '{outcome, rateLimit}' "${rec}")"; fi
    if grep -q 'add-label AFK:paused' "${dir}/gh.log"; then pass "paused"; else fail "paused" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"
}

test_auth_dead_fire_pauses_and_parks() {
    echo "TEST: an AUTH_DEAD Fire pauses its work and parks the Daemon (issue #30 AC 4)"
    local dir; dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #291 feat: thing
Failed to authenticate: OAuth session expired and could not be refreshed"
    local out rc; out="$(run_fire "${dir}" "${FIXTURE}")"; rc=$?
    if [ "${rc}" -eq 1 ]; then pass "claude's exit code is returned"; else fail "claude's exit code is returned" "rc=${rc}"; fi
    if grep -q '^issue edit 291 --repo acme/widgets --remove-label AFK:in-progress --add-label AFK:paused$' "${dir}/gh.log" \
       && grep -q '^issue comment 291 --repo acme/widgets --body Fire paused at .*credential dead mid-Fire' "${dir}/gh.log" \
       && ! grep -q 'AFK:failed' "${dir}/gh.log"; then
        pass "the work is paused, never failed (not the ticket's fault)"
    else fail "the work is paused, never failed (not the ticket's fault)" "$(cat "${dir}/gh.log")"; fi
    if grep -q '^issue list --repo acme/widgets --label AFK:needs-human --state open' "${dir}/gh.log" \
       && grep -q '^issue create --repo acme/widgets --label AFK:needs-human --title Daemon parked on' "${dir}/gh.log"; then
        pass "the needs-human issue is opened (or reused)"
    else fail "the needs-human issue is opened (or reused)" "$(cat "${dir}/gh.log")"; fi
    if [ "$(jq -r '.parked' "${dir}/state/parked.json" 2>/dev/null)" = "true" ] && jq -e '.reason | test("authentication_failed in Fire ")' "${dir}/state/parked.json" >/dev/null; then
        pass "parked.json in the State dir names the Fire"
    else fail "parked.json in the State dir names the Fire" "$(cat "${dir}/state/parked.json" 2>/dev/null)"; fi
    if printf '%s\n' "${out}" | grep -q '^AGENT_RUN_AUTH_DEAD=1$' && printf '%s\n' "${out}" | grep -q '^fire: outcome AUTH_DEAD$' \
       && ! printf '%s\n' "${out}" | grep -q '^AGENT_RUN_RESET_AT='; then
        pass "the stable line tells the Daemon to park, no reset line"
    else fail "the stable line tells the Daemon to park, no reset line" "${out}"; fi
    if [ "$(jq -r .outcome.status "$(record_of "${dir}")")" = "AUTH_DEAD" ]; then pass "record: outcome AUTH_DEAD"; else fail "record: outcome AUTH_DEAD"; fi
    rm -rf "${dir}"
}

test_per_model_limit_does_not_hold_the_daemon() {
    echo "TEST: a per-model limit pauses the work but tells the Daemon to re-gate, not to sleep (story 20)"
    local dir; dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #291 feat: thing
You've hit your Fable limit · resets 2026-09-18T19:00:00Z"
    local out; out="$(run_fire "${dir}" "${FIXTURE}")"
    if printf '%s\n' "${out}" | grep -q '^AGENT_RUN_MODEL_LIMIT=fable$' && printf '%s\n' "${out}" | grep -q '^AGENT_RUN_RESET_AT=$' \
       && grep -q 'add-label AFK:paused' "${dir}/gh.log"; then
        pass "paused, AGENT_RUN_MODEL_LIMIT=fable, empty reset"
    else fail "paused, AGENT_RUN_MODEL_LIMIT=fable, empty reset" "${out}"; fi
    rm -rf "${dir}"
    dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #291 feat: thing
API Error"
    echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789758000,"rateLimitType":"seven_day_opus","utilization":1},"session_id":"s1"}' >> "${dir}/stream.jsonl"
    out="$(run_fire "${dir}" "${FIXTURE}")"
    if printf '%s\n' "${out}" | grep -q '^AGENT_RUN_MODEL_LIMIT=seven_day_opus$' && printf '%s\n' "${out}" | grep -q '^AGENT_RUN_RESET_AT=$'; then
        pass "a rejected per-model window from the stream does the same"
    else fail "a rejected per-model window from the stream does the same" "${out}"; fi
    rm -rf "${dir}"
}

test_exhausted_resolve_restarts() {
    echo "TEST: an exhausted resolve Fire drops its lock and research branch: the next Fire restarts it"
    local dir; dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "resolve: #13 research research-claude-auth-modes
You've hit your weekly limit · resets Sep 18, 7pm (America/New_York)"
    local out; out="$(run_fire "${dir}" "${FIXTURE}")"
    if grep -q '^issue edit 13 --repo acme/widgets --remove-label AFK:in-progress$' "${dir}/gh.log" && ! grep -q 'AFK:failed\|AFK:paused' "${dir}/gh.log" \
       && grep -q "^-C ${FIXTURE} push --quiet origin --delete research/research-claude-auth-modes$" "${dir}/git.log"; then
        pass "lock dropped, no label, branch deleted"
    else fail "lock dropped, no label, branch deleted" "$(cat "${dir}/gh.log") / $(cat "${dir}/git.log")"; fi
    if printf '%s\n' "${out}" | grep -q '^fire: lock resolve #13 AFK:in-progress dropped, restarts next Fire (usage exhausted)$'; then pass "the lock line"; else fail "the lock line" "${out}"; fi
    rm -rf "${dir}"
}

test_gate_verdict_model_switch() {
    echo "TEST: the Gate verdict's fireModel picks the Fire's --model unless the Host env pins one (story 20)"
    local dir; dir="$(make_env "${CANNED_NOOP}")"
    echo '{"authMode":"setup-token","sensor":"stream-events","state":"stale","remainPct":70,"resetAt":null,"shouldFire":true,"observedAt":"2026-09-15T00:00:00Z","limits":[],"warnings":[],"fireModel":"opus","fireModelUntil":"2026-09-18T19:00:00Z"}' > "${dir}/gate.json"
    HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" AUTO_AGENT_FIRE_MODEL= \
        CLAUDE_BIN="${dir}/claude-stub" AUTO_AGENT_GATE_VERDICT_FILE="${dir}/gate.json" bash "${CLI}" fire --noop >/dev/null
    if grep -q -- '--model opus' "${dir}/claude.log" && [ "$(jq -r .model "$(record_of "${dir}")")" = "opus" ]; then pass "fireModel opus becomes --model opus and the record's model"
    else fail "fireModel opus becomes --model opus and the record's model" "$(cat "${dir}/claude.log")"; fi
    rm -rf "${dir}/state/fires"; : > "${dir}/claude.log"
    HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" AUTO_AGENT_FIRE_MODEL=haiku \
        CLAUDE_BIN="${dir}/claude-stub" AUTO_AGENT_GATE_VERDICT_FILE="${dir}/gate.json" bash "${CLI}" fire --noop >/dev/null
    if grep -q -- '--model haiku' "${dir}/claude.log" && ! grep -q -- '--model opus' "${dir}/claude.log"; then pass "AUTO_AGENT_FIRE_MODEL wins over the verdict"
    else fail "AUTO_AGENT_FIRE_MODEL wins over the verdict" "$(cat "${dir}/claude.log")"; fi
    rm -rf "${dir}"
}

test_crashed_reconcile_restores_done() {
    echo "TEST: a crashed reconcile Fire restores AFK:done and comments on the PR, never AFK:failed"
    local dir; dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   reconcile PR #47 (issue #27)"
    local out; out="$(run_fire "${dir}" "${FIXTURE}")"
    if grep -q '^issue edit 27 --repo acme/widgets --remove-label AFK:in-progress --add-label AFK:done$' "${dir}/gh.log" \
       && grep -q '^pr comment 47 --repo acme/widgets --body Reconcile Fire crashed' "${dir}/gh.log" && ! grep -q 'AFK:failed' "${dir}/gh.log"; then
        pass "AFK:done restored on the issue, breadcrumb on the PR"
    else fail "AFK:done restored on the issue, breadcrumb on the PR" "$(cat "${dir}/gh.log")"; fi
    if printf '%s\n' "${out}" | grep -q '^fire: work=reconcile PR #47$'; then pass "work line names the PR"; else fail "work line names the PR" "${out}"; fi
    rm -rf "${dir}"

    dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   reconcile PR #48 (issue #null)"
    out="$(run_fire "${dir}" "${FIXTURE}")"
    if [ "$(grep -vc 'repo view' "${dir}/gh.log")" -eq 0 ] && printf '%s\n' "${out}" | grep -q 'no backing issue'; then
        pass "a reconcile without a backing issue touches no label"
    else fail "a reconcile without a backing issue touches no label" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"
}

test_crashed_resolve_respects_its_terminal_line() {
    echo "TEST: a crashed resolve Fire fails the ticket unless its terminal resolve: line already settled it"
    local dir out
    dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #12 Decide the thing
resolve: #12 research decide-the-thing"
    run_fire "${dir}" "${FIXTURE}" >/dev/null
    if grep -q '^issue edit 12 --repo acme/widgets --remove-label AFK:in-progress --add-label AFK:failed$' "${dir}/gh.log" && grep -q '^issue comment 12 .*Resolve Fire crashed' "${dir}/gh.log"; then
        pass "unsettled resolve: AFK:failed plus a comment"
    else fail "unsettled resolve: AFK:failed plus a comment" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "resolve: #12 task decide-the-thing
resolve: DONE — #12 closed (task)"
    out="$(run_fire "${dir}" "${FIXTURE}")"
    if grep -q '^issue edit 12 --repo acme/widgets --remove-label AFK:in-progress --add-label AFK:done$' "${dir}/gh.log" && ! grep -q 'comment' "${dir}/gh.log"; then
        pass "settled DONE: AFK:done restored, no comment"
    else fail "settled DONE: AFK:done restored, no comment" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "resolve: #12 task decide-the-thing
resolve: DONE — #12 relabelled HITL (needs code)"
    out="$(run_fire "${dir}" "${FIXTURE}")"
    if [ "$(grep -vc 'repo view' "${dir}/gh.log")" -eq 0 ] && printf '%s\n' "${out}" | grep -q 'relabelled HITL'; then
        pass "settled HITL: nothing touched"
    else fail "settled HITL: nothing touched" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "resolve: #12 research decide-the-thing
resolve: FAILED — #12 gate refused"
    run_fire "${dir}" "${FIXTURE}" >/dev/null
    if grep -q -- '--add-label AFK:failed' "${dir}/gh.log" && ! grep -q 'comment' "${dir}/gh.log"; then
        pass "settled FAILED: AFK:failed re-asserted, no second comment"
    else fail "settled FAILED: AFK:failed re-asserted, no second comment" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"
}

test_success_and_nothing_picked_never_touch_a_lock() {
    echo "TEST: a clean Fire, and a crashed Fire that picked nothing, write no label"
    local dir out
    dir="$(make_env)"
    with_text "${dir}" "${CANNED_PICKUP}" "picked:   #5 thing
dispatch: PASS"
    run_fire "${dir}" "${FIXTURE}" >/dev/null
    if [ "$(grep -vc 'repo view' "${dir}/gh.log")" -eq 0 ]; then pass "exit 0: the skill owns its end state, the wrapper writes nothing"
    else fail "exit 0: the skill owns its end state, the wrapper writes nothing" "$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"

    dir="$(make_env "${CANNED_PICKUP}" 1)"
    with_text "${dir}" "${CANNED_PICKUP}" "afk-pickup: triage verdict=in-flight
afk-pickup: skip — 1 in flight"
    out="$(run_fire "${dir}" "${FIXTURE}")"
    if [ "$(grep -vc 'repo view' "${dir}/gh.log")" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^fire: lock nothing picked, nothing to clear$' \
       && printf '%s\n' "${out}" | grep -q '^AGENT_RUN_NO_WORK=1$'; then
        pass "a skip that crashed clears nothing and still says no work"
    else fail "a skip that crashed clears nothing and still says no work" "${out}
$(cat "${dir}/gh.log")"; fi
    rm -rf "${dir}"
}

test_tap_degrades_without_windows() {
    echo "TEST: the tap degrades to the top-level fields when unifiedWindows is absent (AC 3)"
    local dir; dir="$(make_env "${CANNED_NOOP}")"
    jq -c 'if .type == "rate_limit_event" then .rate_limit_info |= (del(.unifiedWindows) | .status = "rejected" | .utilization = 1) else . end' "${CANNED_NOOP}" > "${dir}/stream.jsonl"
    run_fire "${dir}" --noop >/dev/null
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
    local dir; dir="$(make_env "${CANNED_NOOP}" 7)"
    head -2 "${CANNED_NOOP}" > "${dir}/stream.jsonl"   # truncated: no result, no event
    local out rc; out="$(run_fire "${dir}" "${FIXTURE}")"; rc=$?
    local rec; rec="$(record_of "${dir}")"
    if [ "${rc}" -eq 7 ] && [ -n "${rec}" ]; then pass "claude's exit code is returned and a record exists"
    else fail "claude's exit code is returned and a record exists" "rc=${rc}"; return; fi
    local got; got="$(jq -c '{kind, exit, phase, prompt, loaded: .plugin.loaded, listed: .plugin.skillListed, subtype: .result.subtype, rl: .rateLimit, work: .work.kind}' "${rec}")"
    if [ "${got}" = '{"kind":"pickup","exit":7,"phase":"claude","prompt":"/auto-agent:afk-pickup","loaded":true,"listed":false,"subtype":null,"rl":null,"work":null}' ]; then
        pass "record: kind pickup, exit 7, no result, no rate limit, no work"
    else fail "record: kind pickup, exit 7, no result, no rate limit, no work" "${got}"; fi
    if [ ! -e "${dir}/state/rate-limits.json" ]; then pass "no rate-limits.json when no event arrived"
    else fail "no rate-limits.json when no event arrived"; fi
    if printf '%s\n' "${out}" | grep -q '^fire: work=unknown$' && ! printf '%s\n' "${out}" | grep -q 'AGENT_RUN_NO_WORK'; then
        pass "a stream with no verdict is work=unknown, never no-work"
    else fail "a stream with no verdict is work=unknown, never no-work" "${out}"; fi
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
    if [ ! -e "${dir}/gh.log" ] && [ ! -e "${dir}/git.log" ]; then pass "no gh or git call before the schema check passes"
    else fail "no gh or git call before the schema check passes"; fi
    rm -rf "${dir}"
}

test_unresolvable_repo_fails_preflight() {
    echo "TEST: a repo whose default branch cannot be detected fails preflight with exit 3 and a record"
    local dir; dir="$(make_env)"; echo 1 > "${dir}/branch.code"
    local out rc; out="$(run_fire "${dir}" --dry-run 2>&1)"; rc=$?
    local rec; rec="$(record_of "${dir}")"
    if [ "${rc}" -eq 3 ] && [ -n "${rec}" ] && [ ! -e "${dir}/claude.log" ] && [ "$(jq -c '{exit, phase}' "${rec}")" = '{"exit":3,"phase":"preflight"}' ] \
       && printf '%s\n' "${out}" | grep -q 'cannot resolve the repo or default branch'; then
        pass "exit 3, phase preflight, claude never ran"
    else fail "exit 3, phase preflight, claude never ran" "rc=${rc}
${out}"; fi
    rm -rf "${dir}"
}

test_missing_baseline_writes_record_and_skips_claude() {
    echo "TEST: a missing settings baseline fails closed with a record (AC 4)"
    local dir; dir="$(make_env)"
    local root="${dir}/install"; mkdir -p "${root}/lib" "${root}/plugin/settings" "${root}/bin"
    cp "${ROOT_DIR}"/lib/*.sh "${root}/lib/"; cp "${ROOT_DIR}/bin/auto-agent" "${root}/bin/"
    cp -r "${ROOT_DIR}/plugin/schema" "${ROOT_DIR}/plugin/fixtures" "${root}/plugin/"
    local out rc
    out="$(HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" CLAUDE_BIN="${dir}/claude-stub" GH_BIN="${dir}/gh-stub" GIT_BIN="${dir}/git-stub" bash "${root}/bin/auto-agent" fire --dry-run 2>&1)"; rc=$?
    local rec; rec="$(record_of "${dir}")"
    if [ "${rc}" -eq 2 ] && [ -n "${rec}" ] && [ ! -e "${dir}/claude.log" ]; then pass "exit 2, record written, claude never invoked"
    else fail "exit 2, record written, claude never invoked" "rc=${rc} rec=${rec}
${out}"; return; fi
    if [ "$(jq -c '{exit, phase}' "${rec}")" = '{"exit":2,"phase":"preflight"}' ] && printf '%s\n' "${out}" | grep -q 'settings baseline missing'; then
        pass "record says preflight exit 2 and the error names the baseline"
    else fail "record says preflight exit 2 and the error names the baseline" "${out}"; fi
    rm -rf "${dir}"
}

test_settings_and_plugin_flags_on_every_invocation() {
    echo "TEST: --plugin-dir, --settings baseline, stream-json and bypass are passed on every Fire (AC 1, 2)"
    local dir; dir="$(make_env)"
    run_fire "${dir}" --noop >/dev/null
    run_fire "${dir}" "${FIXTURE}" >/dev/null
    HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" \
        CLAUDE_BIN="${dir}/claude-stub" GH_BIN="${dir}/gh-stub" GIT_BIN="${dir}/git-stub" AUTO_AGENT_FIRE_MODEL=claude-opus-5 bash "${CLI}" fire --dry-run >/dev/null
    local n; n="$(wc -l < "${dir}/claude.log")"
    local ok=0 line
    while IFS= read -r line; do
        case "${line}" in
            *"--print"*"--permission-mode bypassPermissions"*"--plugin-dir ${ROOT_DIR}/plugin"*"--settings ${ROOT_DIR}/plugin/settings/baseline.json"*"--output-format stream-json"*"--verbose"*) ok=$((ok + 1)) ;;
        esac
    done < "${dir}/claude.log"
    if [ "${n}" -eq 3 ] && [ "${ok}" -eq 3 ]; then pass "all 3 invocations carry the flags"
    else fail "all 3 invocations carry the flags" "$(cat "${dir}/claude.log")"; fi
    if grep -q -- '--model claude-opus-5 /auto-agent:afk-pickup --dry-run$' "${dir}/claude.log" && [ "$(grep -c -- '--model' "${dir}/claude.log")" -eq 1 ]; then
        pass "AUTO_AGENT_FIRE_MODEL pins --model; unset means no --model"
    else fail "AUTO_AGENT_FIRE_MODEL pins --model; unset means no --model" "$(cat "${dir}/claude.log")"; fi
    if grep -q '/auto-agent:afk-pickup$' "${dir}/claude.log" && grep -q '/auto-agent:dry-run$' "${dir}/claude.log"; then
        pass "a plain Fire prompts the namespaced pickup skill; --noop the no-op skill"
    else fail "a plain Fire prompts the namespaced pickup skill; --noop the no-op skill" "$(cat "${dir}/claude.log")"; fi
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
    local dir; dir="$(make_env "${CANNED_NOOP}")"
    # No AUTO_AGENT_STATE_DIR anywhere: default under $HOME/.local/state.
    ( unset AUTO_AGENT_STATE_DIR; HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/missing.env" CLAUDE_BIN="${dir}/claude-stub" bash "${CLI}" fire --noop >/dev/null )
    if [ "$(ls "${dir}/home/.local/state/auto-agent/fires" 2>/dev/null | wc -l)" -eq 1 ]; then pass "default is \$HOME/.local/state/auto-agent"
    else fail "default is \$HOME/.local/state/auto-agent" "$(find "${dir}/home" -type f)"; fi
    # The Host env file names it.
    printf '# Host env\nAUTO_AGENT_STATE_DIR="%s/from-host-env"\nCLAUDE_AUTH_MODE=login\n' "${dir}" > "${dir}/host.env"
    ( unset AUTO_AGENT_STATE_DIR; HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" CLAUDE_BIN="${dir}/claude-stub" bash "${CLI}" fire --noop >/dev/null )
    local rec; rec="$(ls "${dir}/from-host-env/fires/"*.json 2>/dev/null | head -1)"
    if [ -n "${rec}" ]; then pass "AUTO_AGENT_STATE_DIR in the Host env file is honoured (quotes stripped)"
    else fail "AUTO_AGENT_STATE_DIR in the Host env file is honoured (quotes stripped)"; return; fi
    if [ "$(jq -r .gate.authMode "${rec}")" = "login" ]; then pass "CLAUDE_AUTH_MODE from the Host env reaches the gate block"
    else fail "CLAUDE_AUTH_MODE from the Host env reaches the gate block" "$(jq -c .gate "${rec}")"; fi
    # The environment wins over the file.
    ( HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" CLAUDE_BIN="${dir}/claude-stub" bash "${CLI}" fire --noop >/dev/null )
    if [ "$(ls "${dir}/state/fires" | wc -l)" -eq 1 ] && [ "$(ls "${dir}/from-host-env/fires" | wc -l)" -eq 1 ]; then pass "an exported AUTO_AGENT_STATE_DIR wins over the Host env file"
    else fail "an exported AUTO_AGENT_STATE_DIR wins over the Host env file"; fi
    rm -rf "${dir}"
}

test_gate_verdict_file_is_embedded() {
    echo "TEST: a Gate verdict handed to the Fire is embedded verbatim"
    local dir; dir="$(make_env "${CANNED_NOOP}")"
    echo '{"authMode":"setup-token","sensor":"stream-events","state":"stale","remainPct":79,"resetAt":null,"shouldFire":true,"observedAt":"2026-09-15T00:00:00Z","limits":[],"warnings":[]}' > "${dir}/gate.json"
    HOME="${dir}/home" AUTO_AGENT_HOST_ENV="${dir}/host.env" AUTO_AGENT_STATE_DIR="${dir}/state" \
        CLAUDE_BIN="${dir}/claude-stub" AUTO_AGENT_GATE_VERDICT_FILE="${dir}/gate.json" bash "${CLI}" fire --noop >/dev/null
    local rec; rec="$(record_of "${dir}")"
    if [ "$(jq -c '.gate | {sensor, remainPct}' "${rec}")" = '{"sensor":"stream-events","remainPct":79}' ]; then pass "gate block is the supplied verdict"
    else fail "gate block is the supplied verdict" "$(jq -c .gate "${rec}")"; fi
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

test_noop_from_canned_stream
test_noop_fails_when_plugin_missing_from_stream
test_dry_run_pickup_reports_no_work
test_dry_run_pickup_reports_a_would_pick
test_resolve_dry_run_reports_a_would_open
test_dry_run_fails_without_a_verdict_line
test_pickup_fire_resets_the_checkout_first
test_crashed_pick_clears_its_lock
test_exhausted_pick_pauses
test_rejected_event_is_the_outcome
test_auth_dead_fire_pauses_and_parks
test_per_model_limit_does_not_hold_the_daemon
test_exhausted_resolve_restarts
test_gate_verdict_model_switch
test_crashed_reconcile_restores_done
test_crashed_resolve_respects_its_terminal_line
test_success_and_nothing_picked_never_touch_a_lock
test_tap_degrades_without_windows
test_failed_fire_still_gets_a_record
test_preflight_failure_writes_record_and_skips_claude
test_unresolvable_repo_fails_preflight
test_missing_baseline_writes_record_and_skips_claude
test_settings_and_plugin_flags_on_every_invocation
test_settings_baseline_shape
test_state_dir_from_host_env_and_default
test_gate_verdict_file_is_embedded
test_usage_errors

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
