#!/usr/bin/env bash
# deps-gate.sh: decide whether a Dependabot PR may be squash-merged.
#
# Why this exists: the deps-land lane lands dependency bumps on the default
# branch with an admin squash and no human in the loop. "It's just a bump,
# merge it" is a judgement call, and a judgement call made by an agent
# mid-Fire is exactly the kind of thing that eventually merges a major version
# bump with red CI, or a stray commit somebody pushed onto the bot's branch.
# This script owns the rule instead: one tested gate, one command shape, no
# discretion. It is a sibling of docs-only-gate.sh and deliberately shares its
# contract, vocabulary, exit codes and merge recipe; the two gates are read
# side by side in the pickup skill.
#
# The gate DECIDES; it never mutates. It reads the PR (one `gh pr view`) and
# its check list (one `gh pr checks`) and prints the verdict plus the exact
# merge command the caller must run. The lane owns the merge itself, so the
# one call site that can land a commit on the default branch stays reviewable
# in the skill. No code path in this file can merge, edit or comment on
# anything.
#
# Usage:
#   lib/deps-gate.sh --pr <N> --head <sha> [--major true|false] [--security true|false]
#
# Every Target Project fact comes from the Harness config (ADR 0002), resolved
# through harness_config_resolve (HARNESS_CONFIG_JSON, or AUTO_AGENT_TARGET_DIR
# from the Host env): the repo slug (every gh call and the merge command carry
# it), the required checks and, through lib/deps-lane.sh, the fix cap.
#
# `--major` defaults to false and must be exactly true|false; misreading it
# would auto-merge a breaking bump. `--security` is accepted for the caller's
# convenience (its per-Fire report line carries security=y/n) and NEVER changes
# the verdict: a security bump is merged on the same evidence as any other, it
# is only titled differently, which the title test below already covers.
# Because it cannot move the verdict, its VALUE is not validated: refusing an
# otherwise mergeable bump over `--security yes` would be a harness error about
# nothing.
#
# Output (stdout): one compact JSON verdict, nothing else:
#   { "approved": <bool>, "sha": "<head>",
#     "mergeCmd": "gh pr merge <N> --repo <slug> --squash --admin \
#                  --match-head-commit <sha>",
#     "reason": "<why refused>" }   # absent when the verdict approves
#
# The verdict approves only when ALL of these hold:
#   * the PR is open and not a draft; a draft bot PR is one a human parked;
#   * author AND branch pass the Dependabot test: login (lowercased) exactly
#     `app/dependabot` and a head branch starting with the Dependabot prefix.
#     This must agree with pr-triage.sh's `dependabot_pr`: a user account can
#     be renamed `dependabot`, and a `dependabot/...` branch is a shape anyone
#     can push, so only both together buy the lane's admin merge;
#   * the head sha carries BOTH a `tierA=green` and a `tierB=PASS` marker, and
#     the PR has not moved past it (`headRefOid` still equals `--head`).
#     Markers are parsed by lib/deps-lane.sh, which owns the marker format; a
#     marker for any other sha vouches for code that would not merge;
#   * every check is green and at least one ran (same bucket vocabulary as the
#     docs gate: `pass`/`skipping` are green, everything else is not), and
#     every check named in the config's required_checks is present with
#     bucket `pass` (not `skipping`: a path-filtered job that did not run
#     vouches for nothing). A Target Project whose PR-title lint decides
#     release notes names that check here, so a title nothing linted is
#     refused;
#   * the title starts `fix(deps):` or `chore(deps):`;
#   * no human has requested changes: `reviewDecision != CHANGES_REQUESTED`,
#     at any bump size;
#   * the bump is not major, or `reviewDecision == APPROVED`; a breaking change
#     needs a human.
#
# Merge recipe: harness_merge_recipe (lib/harness-config.sh), the one
# admin-squash recipe shared with docs-only-gate.sh. `--match-head-commit`
# pins the merge to the sha the markers vouch for: if anything is pushed
# between gate and merge, the merge fails rather than landing unverified code.
#
# Exit codes (two, as the caller contract says; the JSON `reason` says why):
#   0  approved: merge it with `.mergeCmd`, verbatim
#   1  refused. `.reason` is one of
#        not-dependabot    author or branch is not the Dependabot app's
#        draft-or-closed   the PR is not open, or is a draft
#        markers-stale     tier A/B markers are absent, carry another sha, or
#                          the PR moved past --head
#        checks-not-green  a check is failing or still pending
#        checks-missing    the check list is EMPTY (nothing ran, so nothing
#                          vouches for the PR; an admin merge also bypasses
#                          branch protection's required-check list), or a
#                          check named in the config's required_checks is
#                          absent or skipped rather than passed
#        title-not-deps    the title is not a deps title
#        changes-requested a human reviewed the bump and requested changes
#        major-unapproved  a major bump without an approving review
#        checks-unreadable PR state or the check list could not be read
#        usage             bad/missing arguments, or no Harness config could be
#                          resolved (gate could not run)
#      The caller MUST report the last two as harness errors, not as verdicts
#      about the PR.
#
# Env:
#   GH_BIN   gh CLI (default: gh), injectable for tests

set -uo pipefail

_deps_gate_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_deps_gate_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_deps_gate_lib_dir}/host-env.sh"
# shellcheck source=pr-checks.sh
. "${_deps_gate_lib_dir}/pr-checks.sh"
# shellcheck source=deps-lane.sh
. "${_deps_gate_lib_dir}/deps-lane.sh"

PR=''
HEAD_SHA=''
MAJOR='false'
SECURITY='false'

MERGE_CMD=''

emit() { # emit <approved> <reason|''>
    jq -cn --argjson approved "$1" --arg sha "${HEAD_SHA}" \
        --arg cmd "${MERGE_CMD}" --arg reason "$2" \
        '{approved: $approved, sha: $sha, mergeCmd: $cmd}
         + (if $reason == "" then {} else {reason: $reason} end)'
}

refuse() { # refuse <reason> <stderr line>
    echo "deps-gate: $2" >&2
    emit false "$1"
    exit 1
}

# Every flag this gate takes carries a value. A trailing value-less flag is a
# usage error, NOT a reason to spin: with $#=1 a `shift 2` fails and the loop
# never advances, so `deps-gate.sh --pr` would hang a Fire forever instead of
# refusing. Check the arity before consuming.
while [ $# -gt 0 ]; do
    if [ $# -lt 2 ]; then
        refuse usage "$1 requires a value"
    fi
    case "$1" in
        --pr)       PR="$2"; shift 2 ;;
        --head)     HEAD_SHA="$2"; shift 2 ;;
        --major)    MAJOR="$2"; shift 2 ;;
        --security) SECURITY="$2"; shift 2 ;;
        *) refuse usage "unknown arg $1" ;;
    esac
done

if [ -z "${PR}" ] || [ -z "${HEAD_SHA}" ]; then
    refuse usage '--pr and --head are required'
fi

# --major must be exactly true|false. Reading an unrecognised `True`/`1`/`yes`
# as "not a major bump" would hand a breaking version bump the unreviewed
# auto-merge path, the one refusal in this file a human is meant to resolve.
# --security is NOT validated: it cannot change the verdict, so refusing a
# mergeable bump over its spelling would be a harness error about a flag this
# gate only echoes back to the caller's report line.
if [ "${MAJOR}" != "true" ] && [ "${MAJOR}" != "false" ]; then
    refuse usage "--major must be exactly true|false, got: ${MAJOR:-<none>}"
fi
: "${SECURITY}" # accepted, deliberately unused by the verdict

host_env_load
CFG="$(harness_config_resolve 2>/dev/null)" \
    || refuse usage 'no Harness config could be resolved'
export HARNESS_CONFIG_JSON="${CFG}"   # deps-lane.sh reads the fix cap from it
REPO="$(printf '%s' "${CFG}" | jq -r '.repo.slug // empty')"
if [ -z "${REPO}" ]; then
    refuse usage 'no Harness config could be resolved: repo slug missing'
fi

GH="${GH_BIN:-gh}"

MERGE_CMD="$(harness_merge_recipe "${REPO}" "${PR}" "${HEAD_SHA}")"

# ONE read of the PR: every field the verdict needs, including the comment
# bodies the markers live in. A second read could see a different PR (a push
# lands mid-gate), which would let one field's answer vouch for another field's
# code.
VIEW="$("${GH}" pr view "${PR}" --repo "${REPO}" \
    --json state,isDraft,author,headRefName,headRefOid,title,reviewDecision,comments \
    2>/dev/null)" || VIEW=''

if ! printf '%s' "${VIEW}" | jq -e 'type == "object"' >/dev/null 2>&1; then
    refuse checks-unreadable "PR ${PR} state is unreadable"
fi

field() { printf '%s' "${VIEW}" | jq -r "$1 // \"\"" 2>/dev/null; }

STATE="$(field '.state')"
IS_DRAFT="$(field '.isDraft')"
AUTHOR="$(field '.author.login | ascii_downcase')"
BRANCH="$(field '.headRefName')"
HEAD_OID="$(field '.headRefOid')"
TITLE="$(field '.title')"
REVIEW="$(field '.reviewDecision')"

if [ "${STATE}" != "OPEN" ] || [ "${IS_DRAFT}" = "true" ]; then
    refuse draft-or-closed "PR ${PR} is ${STATE}$([ "${IS_DRAFT}" = "true" ] && echo ' and a draft')"
fi

case "${BRANCH}" in
    "${HARNESS_BRANCH_DEPENDABOT_PREFIX}"*) ;;
    *) refuse not-dependabot "PR ${PR} head branch ${BRANCH:-<none>} is not a ${HARNESS_BRANCH_DEPENDABOT_PREFIX} branch" ;;
esac

if [ "${AUTHOR}" != "app/dependabot" ]; then
    refuse not-dependabot "PR ${PR} author ${AUTHOR:-<none>} is not the Dependabot app"
fi

# The PR moving is the same staleness as a marker for another sha: what the lane
# verified is no longer what would merge. Caught here rather than left to
# --match-head-commit so the caller gets a verdict instead of a failed merge.
if [ "${HEAD_OID}" != "${HEAD_SHA}" ]; then
    refuse markers-stale \
        "PR ${PR} head is ${HEAD_OID:-<none>}, not the ${HEAD_SHA} the gate was asked about"
fi

MARKERS="$(printf '%s' "${VIEW}" | jq -r '.comments[]?.body // ""' 2>/dev/null \
    | deps_lane_marker_parse "${HEAD_SHA}")" || MARKERS=''

if ! printf '%s' "${MARKERS}" | jq -e 'type == "object"' >/dev/null 2>&1; then
    refuse markers-stale "marker state for PR ${PR} at ${HEAD_SHA} could not be read"
fi

if [ "$(printf '%s' "${MARKERS}" | jq -r '.tierA and .tierB')" != "true" ]; then
    refuse markers-stale \
        "PR ${PR} has no tierA=green + tierB=PASS markers for ${HEAD_SHA}"
fi

# Green checks stand in for the review this merge is skipping. lib/pr-checks.sh
# owns the reading (shared with the docs-only gate) and fails SAFE: unreadable,
# empty, red or pending all refuse, and each configured required check must
# have run and passed. The PAYLOAD decides, not gh's exit code: `gh pr checks`
# exits non-zero on a red or pending list while still printing it.
CHECKS="$("${GH}" pr checks "${PR}" --repo "${REPO}" --json name,bucket 2>/dev/null)"
REQUIRED="$(printf '%s' "${CFG}" | jq -c '.required_checks // []')"

if ! VERDICT="$(printf '%s' "${CHECKS}" | pr_checks_verdict "${REQUIRED}")"; then
    refuse "${VERDICT%%$'\t'*}" "PR ${PR}: ${VERDICT#*$'\t'}"
fi

case "${TITLE}" in
    'fix(deps):'*|'chore(deps):'*) ;;
    *) refuse title-not-deps "PR ${PR} title is not a deps title: ${TITLE:-<none>}" ;;
esac

# A human who reviewed the bump and requested changes has rejected it, whatever
# its size: a Dependabot PR the maintainer rejects is left alone by the Daemon,
# and that is not scoped to majors. Checked before the major test so the
# refusal names what actually happened rather than the bump's size.
if [ "${REVIEW}" = "CHANGES_REQUESTED" ]; then
    refuse changes-requested \
        "PR ${PR} has a human review requesting changes"
fi

if [ "${MAJOR}" = "true" ] && [ "${REVIEW}" != "APPROVED" ]; then
    refuse major-unapproved \
        "PR ${PR} is a major bump with reviewDecision ${REVIEW:-<none>}"
fi

emit true ''
exit 0
