#!/usr/bin/env bash
# Tests for plugin/hooks/smoke-trailer.sh and plugin/hooks/review-gate.sh
#
# Run: bash plugin/hooks/hooks.test.sh
#
# Strategy: each hook is driven as Claude Code drives it (hook JSON on stdin,
# cwd inside a repo) against throwaway git repositories whose HEAD commit and
# review-state file are the behavior under test. Assert the exit code and the
# stderr feedback only.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SMOKE="${SCRIPT_DIR}/smoke-trailer.sh"
GATE="${SCRIPT_DIR}/review-gate.sh"
HOOKS_JSON="${SCRIPT_DIR}/hooks.json"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# make_repo <commit message> -> dir with one commit on branch feat/issue-7
make_repo() {
    local dir; dir="$(mktemp -d)"
    git -C "${dir}" init -q -b feat/issue-7
    git -C "${dir}" -c user.name=t -c user.email=t@t commit -q --allow-empty -m "$1"
    echo "${dir}"
}

# run_hook <hook> <dir> [stdin-json] -> exit code; stderr in HOOK_ERR
run_hook() {
    local hook="$1" dir="$2" json="${3:-{\}}"
    HOOK_ERR="$(cd "${dir}" && printf '%s' "${json}" | bash "${hook}" 2>&1 >/dev/null)"
    return "${PIPESTATUS[0]:-0}"
}
hook_rc() { local hook="$1" dir="$2" json="${3:-{\}}"; (cd "${dir}" && printf '%s' "${json}" | bash "${hook}" >/dev/null 2>"${dir}/err"); echo $?; }

echo "hooks.json"
t="hooks.json registers smoke-trailer on Stop and SubagentStop and review-gate on Stop, through CLAUDE_PLUGIN_ROOT"
if [ "$(jq -r '[.hooks.Stop[0].hooks[].command, .hooks.SubagentStop[0].hooks[].command] | join(" ")' "${HOOKS_JSON}")" = \
     '${CLAUDE_PLUGIN_ROOT}/hooks/smoke-trailer.sh ${CLAUDE_PLUGIN_ROOT}/hooks/review-gate.sh ${CLAUDE_PLUGIN_ROOT}/hooks/smoke-trailer.sh' ]; then pass "$t"; else fail "$t" "$(jq -c .hooks "${HOOKS_JSON}")"; fi
t="both hooks are executable"
if [ -x "${SMOKE}" ] && [ -x "${GATE}" ]; then pass "$t"; else fail "$t"; fi

echo "smoke-trailer.sh (issue #28 behaviour 3)"
d="$(make_repo "feat(app): add the thing

Closes #7")"
rc="$(hook_rc "${SMOKE}" "${d}")"
t="a dispatch commit without a smoke: trailer blocks the stop (exit 2) with the amend instruction"
if [ "${rc}" -eq 2 ] && grep -q 'missing `smoke:` trailer' "${d}/err" && grep -q 'commit --amend' "${d}/err"; then pass "$t"; else fail "$t" "rc=${rc} $(cat "${d}/err")"; fi
rc="$(hook_rc "${SMOKE}" "${d}" '{"stop_hook_active":true,"hook_event_name":"Stop"}')"
t="the same commit passes when stop_hook_active is true (no loop)"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${d}"

for trailer in 'smoke: PASS — 2/2 probes green' 'smoke: FAIL — /api/items 500' 'smoke: SKIPPED — no Environment provider (Bootstrap state)'; do
    d="$(make_repo "fix(verify): thing

Closes #7
${trailer}")"
    rc="$(hook_rc "${SMOKE}" "${d}" '{"hook_event_name":"SubagentStop"}')"
    t="a dispatch commit with '${trailer%% —*}' passes"
    if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
    rm -rf "${d}"
done

for msg in 'wip: freeze partial work on #7 (usage exhausted)' 'Merge branch x' 'fix(ci): pr-watch round 1 — auto-fix failing checks' 'feat(app): no closes line'; do
    d="$(make_repo "${msg}")"
    rc="$(hook_rc "${SMOKE}" "${d}")"
    t="a non-dispatch HEAD ('${msg%% *}') is not checked"
    if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
    rm -rf "${d}"
done

d="$(mktemp -d)"
rc="$(hook_rc "${SMOKE}" "${d}")"
t="outside a repo the hook allows the stop"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${d}"

echo "review-gate.sh"
d="$(make_repo "feat(app): x")"
mkdir -p "${d}/.git/auto-agent"
echo '{"branch":"feat/issue-7","issue":7,"round":1,"verdict":"change-request","asks":["cover the empty-list case","drop the mock of ItemStore"]}' > "${d}/.git/auto-agent/review-state.json"
rc="$(hook_rc "${GATE}" "${d}")"
t="a pending change-request on the checked-out branch blocks the stop and lists the asks"
if [ "${rc}" -eq 2 ] && grep -q 'cover the empty-list case' "${d}/err" && grep -q 'drop the mock of ItemStore' "${d}/err"; then pass "$t"; else fail "$t" "rc=${rc} $(cat "${d}/err")"; fi
rc="$(hook_rc "${GATE}" "${d}" '{"stop_hook_active":true}')"
t="the same state passes when stop_hook_active is true"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
echo '{"branch":"feat/issue-7","issue":7,"round":2,"verdict":"approved","asks":[]}' > "${d}/.git/auto-agent/review-state.json"
rc="$(hook_rc "${GATE}" "${d}")"
t="an approved verdict allows the stop"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
echo '{"branch":"feat/issue-9","issue":9,"round":1,"verdict":"change-request","asks":["x"]}' > "${d}/.git/auto-agent/review-state.json"
rc="$(hook_rc "${GATE}" "${d}")"
t="a change-request recorded for another branch is stale debris, not a gate"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
echo '{"branch":"feat/issue-7","verdict":"change-request"}' > "${d}/.git/auto-agent/review-state.json"
rc="$(hook_rc "${GATE}" "${d}")"
t="a change-request with no asks recorded still blocks"
if [ "${rc}" -eq 2 ] && grep -q 'no asks recorded' "${d}/err"; then pass "$t"; else fail "$t" "rc=${rc}"; fi
echo 'not json' > "${d}/.git/auto-agent/review-state.json"
rc="$(hook_rc "${GATE}" "${d}")"
t="an unreadable state file allows the stop"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -f "${d}/.git/auto-agent/review-state.json"
rc="$(hook_rc "${GATE}" "${d}")"
t="no state file allows the stop"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${d}"

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
