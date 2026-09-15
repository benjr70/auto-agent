#!/usr/bin/env bash
# docs-only-gate.sh: decide whether a PR may be merged as docs-only.
#
# Why this exists: research PRs touch nothing but the Target Project's research
# docs. They carry no code risk, so they must not spend a review and verify
# tail (or a human's review round) before they land. But "merge it, it's only
# docs" is a judgement call, and a judgement call made by an agent mid-Fire is
# exactly the kind of thing that eventually merges a stray code edit. This
# script owns the rule instead: one tested gate, one command shape, no
# discretion.
#
# The gate DECIDES; it never mutates. It reads a diff (and, with
# `--check-state`, the PR's check list) and prints the verdict plus the exact
# merge command the caller must run. The pickup skill owns the merge itself, so
# the one call site that can land a commit on the default branch stays
# reviewable in the skill. No code path in this file can merge anything.
#
# Usage:
#   lib/docs-only-gate.sh --head <sha> --pr <N> [--base <ref>] [--check-state]
#
# Every Target Project fact comes from the Harness config (ADR 0002), resolved
# through harness_config_resolve (HARNESS_CONFIG_JSON, or AUTO_AGENT_TARGET_DIR
# from the Host env): the repo slug (every gh call and the merge command carry
# it), the detected default branch (`--base` defaults to `origin/<default>`),
# the research prefix and the required checks.
#
# Output (stdout): one compact JSON verdict, nothing else:
#   { "docsOnly": <bool>, "sha": "<head>", "changed": [ "<path>", ... ],
#     "mergeCmd": "gh pr merge <N> --repo <slug> --squash --admin \
#                  --match-head-commit <sha>",
#     "reason": "<why refused>" }   # absent when the verdict approves
#
# The verdict is "docs-only" only when the diff is non-empty AND every changed
# path is under the research prefix (the rule is lib/docs-research-paths.sh,
# shared with pr-triage.sh). The diff is a three-dot (merge-base) diff with
# `--no-renames`, so a file *moved into* the research prefix still shows its
# original path outside it and is refused: renames are exactly how a code
# change would otherwise sneak past a prefix test.
#
# Merge recipe: harness_merge_recipe (lib/harness-config.sh), the one
# admin-squash recipe shared with deps-gate.sh. `--match-head-commit` pins the
# merge to the sha the gate inspected: if anything is pushed between gate and
# merge, the merge fails rather than landing unreviewed code.
#
# Exit codes (two, as the caller contract says; the JSON `reason` says why):
#   0  approved: docs-only (and, with --check-state, every check green)
#   1  refused. `.reason` is one of
#        not-docs-only     a changed path lives outside the research prefix
#        checks-not-green  a check is failing or still pending
#        checks-missing    the check list is EMPTY (nothing ran, so nothing
#                          vouches for the PR; an admin merge also bypasses
#                          branch protection's required-check list), or a
#                          check named in the config's required_checks is
#                          absent or skipped rather than passed
#        checks-unreadable the check list could not be read
#        head-missing      the head object is not in this clone (fetch it and
#                          re-run); the gate could not run and says nothing
#                          about the PR's contents
#        git-failed        git could not produce the diff (gate could not run)
#        usage             bad/missing arguments, or no Harness config could be
#                          resolved (gate could not run)
#      The caller MUST report the last three as harness errors, not as verdicts
#      about the PR.
#
# Env:
#   GIT_BIN  git CLI (default: git), injectable for tests
#   GH_BIN   gh CLI  (default: gh),  injectable for tests

set -uo pipefail

_docs_only_gate_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_docs_only_gate_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_docs_only_gate_lib_dir}/host-env.sh"
# shellcheck source=docs-research-paths.sh
. "${_docs_only_gate_lib_dir}/docs-research-paths.sh"

BASE=''
HEAD_SHA=''
PR=''
CHECK_STATE=0

MERGE_CMD=''

emit() { # emit <docsOnly> <changedJson> <reason|''>
    jq -cn --argjson docsOnly "$1" --argjson changed "$2" \
        --arg sha "${HEAD_SHA}" --arg cmd "${MERGE_CMD}" --arg reason "$3" \
        '{docsOnly: $docsOnly, sha: $sha, changed: $changed, mergeCmd: $cmd}
         + (if $reason == "" then {} else {reason: $reason} end)'
}

refuse() { # refuse <docsOnly> <changedJson> <reason> <stderr line>
    echo "docs-only-gate: $4" >&2
    emit "$1" "$2" "$3"
    exit 1
}

while [ $# -gt 0 ]; do
    case "$1" in
        --base)        BASE="${2:-}"; shift 2 ;;
        --head)        HEAD_SHA="${2:-}"; shift 2 ;;
        --pr)          PR="${2:-}"; shift 2 ;;
        --check-state) CHECK_STATE=1; shift ;;
        *) refuse false '[]' usage "unknown arg $1" ;;
    esac
done

if [ -z "${HEAD_SHA}" ] || [ -z "${PR}" ]; then
    refuse false '[]' usage '--head and --pr are required'
fi

host_env_load
CFG="$(harness_config_resolve 2>/dev/null)" \
    || refuse false '[]' usage 'no Harness config could be resolved'
REPO="$(printf '%s' "${CFG}" | jq -r '.repo.slug // empty')"
DEFAULT_BRANCH="$(printf '%s' "${CFG}" | jq -r '.repo.default_branch // empty')"
PREFIX="$(docs_research_prefix "${CFG}")" \
    || refuse false '[]' usage 'no Harness config could be resolved: docs_research_prefix missing'
if [ -z "${REPO}" ] || [ -z "${DEFAULT_BRANCH}" ]; then
    refuse false '[]' usage 'no Harness config could be resolved: repo slug or default branch missing'
fi
[ -n "${BASE}" ] || BASE="origin/${DEFAULT_BRANCH}"

GIT="${GIT_BIN:-git}"
GH="${GH_BIN:-gh}"

MERGE_CMD="$(harness_merge_recipe "${REPO}" "${PR}" "${HEAD_SHA}")"

# The head object is often absent locally: a Fire fetches only the default
# branch, and the PR branch may have been pruned or pushed from another
# checkout. A three-dot diff against an unknown sha fails (or, with a
# partially-fetched ref, lies), so probe first and refuse with reason
# head-missing. The caller fetches the head and re-runs; it must never read
# this as a verdict about the PR's contents.
if ! "${GIT}" cat-file -e "${HEAD_SHA}^{commit}" 2>/dev/null; then
    refuse false '[]' head-missing \
        "head object ${HEAD_SHA} not present locally; fetch it first"
fi

DIFF="$("${GIT}" diff --name-only --no-renames "${BASE}...${HEAD_SHA}" 2>/dev/null)" \
    || refuse false '[]' git-failed "git diff failed for ${BASE}...${HEAD_SHA}"

CHANGED_JSON="$(printf '%s' "${DIFF}" \
    | jq -R -s 'split("\n") | map(select(length > 0))' 2>/dev/null)"
[ -n "${CHANGED_JSON}" ] || CHANGED_JSON='[]'

DOCS_ONLY="$(printf '%s' "${CHANGED_JSON}" | docs_research_only "${PREFIX}")"

if [ "${DOCS_ONLY}" != "true" ]; then
    refuse false "${CHANGED_JSON}" not-docs-only \
        "a changed path lives outside ${PREFIX}"
fi

if [ "${CHECK_STATE}" -eq 0 ]; then
    emit true "${CHANGED_JSON}" ''
    exit 0
fi

# --check-state: green checks are the only thing standing in for the review the
# PR is skipping, so read them and fail SAFE: an unreadable list, an EMPTY list
# (nothing ran), a pending check or a red one all refuse. `skipping` is benign.
# Bucket vocabulary note: `pass`/`skipping` are green here, everything else is
# not; lib/ci-wait.sh applies the same GitHub bucket vocabulary from the other
# side (it counts `pending`/`fail`). Change one, check the other.
CHECKS="$("${GH}" pr checks "${PR}" --repo "${REPO}" --json name,bucket 2>/dev/null)" || CHECKS=''

if ! printf '%s' "${CHECKS}" | jq -e 'type == "array"' >/dev/null 2>&1; then
    refuse true "${CHANGED_JSON}" checks-unreadable "check state for PR ${PR} is unreadable"
fi

if [ "$(printf '%s' "${CHECKS}" | jq 'length')" = "0" ]; then
    refuse true "${CHANGED_JSON}" checks-missing \
        "PR ${PR} has no checks at all; nothing vouches for it"
fi

NOT_GREEN="$(printf '%s' "${CHECKS}" | jq \
    '[.[] | select((.bucket // "") != "pass" and (.bucket // "") != "skipping")] | length')"

if [ "${NOT_GREEN}" != "0" ]; then
    refuse true "${CHANGED_JSON}" checks-not-green \
        "${NOT_GREEN} check(s) on PR ${PR} are failing or pending"
fi

# The config's required_checks must each have actually run AND passed. An
# absent one is not "nothing to worry about", and `skipping` is the same
# nothing: the list test above counts it as green, so only an explicit `pass`
# bucket on a named check vouches for it. An empty list demands no named check.
MISSING_REQUIRED="$(printf '%s' "${CHECKS}" | jq -r --argjson req "$(printf '%s' "${CFG}" | jq -c '.required_checks // []')" \
    '. as $checks
     | [$req[] as $n | $n
        | select(any($checks[]; (.name // "") == $n and (.bucket // "") == "pass") | not)]
     | first // empty')"

if [ -n "${MISSING_REQUIRED}" ]; then
    refuse true "${CHANGED_JSON}" checks-missing \
        "a required check did not run and pass: ${MISSING_REQUIRED}"
fi

emit true "${CHANGED_JSON}" ''
exit 0
