#!/usr/bin/env bash
# review-gate.sh: the reviewer change-request gate (Stop).
#
# Port of the source harness's idle-time review hook (from the old
# multi-agent flow). The implementer is no longer a separate agent that goes
# idle; it is the dispatch session itself, and the moment it could abandon an
# unanswered reviewer change-request is when it stops. The dispatch skill records every reviewer
# verdict in the review-state file before acting on it:
#
#   <git-dir>/auto-agent/review-state.json
#   { "branch": "feat/issue-<N>", "issue": <N>, "round": <r>,
#     "verdict": "change-request" | "approved", "asks": ["..."] }
#
# and deletes the file when the issue ends. While the file says
# `change-request` for the branch that is checked out, the stop is blocked
# (exit 2) and the asks are fed back. Another branch's file is stale debris,
# not a gate.
#
# Reads the hook JSON on stdin and lets a second stop through when
# `stop_hook_active` is true, so an ask the agent cannot satisfy never pins the
# session in a loop.
#
# Exit codes:
#   0  nothing pending; allow the stop
#   2  a change-request is pending on this branch; block and send feedback
#
# Graceful fallback: if jq is missing or git fails, exit 0.
set -uo pipefail

command -v jq >/dev/null 2>&1 || exit 0

input="$(cat 2>/dev/null || true)"
if [ -n "${input}" ] && [ "$(printf '%s' "${input}" | jq -r '.stop_hook_active // false' 2>/dev/null)" = "true" ]; then
    exit 0
fi

git_dir="$(git rev-parse --git-dir 2>/dev/null)" || exit 0
state="${git_dir}/auto-agent/review-state.json"
[ -f "${state}" ] || exit 0

branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)" || exit 0
pending="$(jq -r --arg b "${branch}" '
    select(.verdict == "change-request" and .branch == $b)
    | (.asks // []) | if length > 0 then .[] else "change requested (no asks recorded)" end' \
    "${state}" 2>/dev/null)" || exit 0
[ -n "${pending}" ] || exit 0

{
    echo "the reviewer has change-requests on this branch you have not addressed:"
    printf '%s\n' "${pending}" | sed 's/^/  - /'
    echo ""
    echo "address each one, re-stage, and re-run the review round before stopping."
    echo "(the /auto-agent:afk-dispatch skill records the verdict in ${state})"
} >&2
exit 2
