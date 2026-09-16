#!/usr/bin/env bash
# smoke-trailer.sh: the `smoke:` trailer hook (Stop and SubagentStop).
#
# Port of the source harness's task-completed smoke hook. There
# is no task list any more: the verifier subagent lands the dispatch commit and
# then stops, and the dispatch session stops after it, so both stops are the
# moment to check that HEAD carries a `smoke: PASS|FAIL|SKIPPED` trailer. When
# it does not, the stop is blocked (exit 2) and the feedback on stderr tells the
# agent to amend the commit.
#
# Only a dispatch in flight is checked: the dispatch skill opens
# <git-dir>/auto-agent/review-state.json for its branch at pre-flight and
# removes it when the issue ends, so a Fire that never dispatched (a dry-run,
# an idle Fire, a reconcile) is never blocked on an unrelated HEAD such as a
# squash-merged default-branch commit that also carries `Closes #<N>`. Within
# a dispatch, only a dispatch commit is checked: a conventional-commit subject
# AND a `Closes #<N>` line, which is what the implementer and verifier produce.
# Any other HEAD (a merge, a `wip:` freeze, a fix(ci) round) passes.
#
# Reads the hook JSON on stdin and lets a second stop through when
# `stop_hook_active` is true, so a commit the agent cannot amend never pins the
# session in a loop; the human reviewer still reads commit bodies in PRs.
#
# Exit codes:
#   0  trailer present, or nothing to check; allow the stop
#   2  trailer missing; block the stop and send feedback to the agent
#
# Graceful fallback: if git fails or jq is missing, exit 0.
set -uo pipefail

input="$(cat 2>/dev/null || true)"
if command -v jq >/dev/null 2>&1 && [ -n "${input}" ]; then
    if [ "$(printf '%s' "${input}" | jq -r '.stop_hook_active // false' 2>/dev/null)" = "true" ]; then
        exit 0
    fi
fi

cd "$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
git_dir="$(git rev-parse --git-dir 2>/dev/null)" || exit 0
state="${git_dir}/auto-agent/review-state.json"
[ -f "${state}" ] || exit 0
branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)" || exit 0
if command -v jq >/dev/null 2>&1; then
    [ "$(jq -r '.branch // ""' "${state}" 2>/dev/null)" = "${branch}" ] || exit 0
fi
body="$(git log -1 --format=%B 2>/dev/null)" || exit 0

printf '%s\n' "${body}" | grep -qE '^[a-z]+(\([^)]+\))?!?: ' || exit 0
printf '%s\n' "${body}" | grep -qE '^Closes #[0-9]+' || exit 0

if printf '%s\n' "${body}" | grep -qE '^smoke: (PASS|FAIL|SKIPPED)\b'; then
    exit 0
fi

cat >&2 <<'MSG'
commit missing `smoke:` trailer: the verifier must land the dispatch commit
with one of these as its last line:

  smoke: PASS — <detail>
  smoke: FAIL — <detail>      (then do not commit: report the failure instead)
  smoke: SKIPPED — <reason>

Amend HEAD (`git commit --amend`) with the trailer before stopping. See the
/auto-agent:afk-dispatch skill and the auto-agent:verifier agent for the contract.
MSG
exit 2
