#!/usr/bin/env bash
# review-poster.sh: the Review Poster: render, post, and track the automated
# one-time code review's footprint on an Agent PR (the Fire's review round).
#
# Sourceable library owning the two machine markers that distinguish the
# automated review from human activity, behind stable functions so the skill
# never hand-rolls REST calls or marker strings. thread-reconciler.sh stays the
# only owner of thread enumeration/reply/resolve; this lib composes with it
# (`tr_unresolved_threads ... | rp_filter_agent_threads`).
#
# The repo every call posts to is the Target Project's slug from the Harness
# config (ADR 0002); no function takes or carries a repo argument. A call
# that cannot resolve the config returns non-zero with a stderr line and
# makes no gh call.
#
#   rp_render_finding <axis> <category> <severity> <summary> <failure_scenario>
#       -> stdout: the uniform inline-comment body: RP_MARKER first line, then
#         the visible 🤖 header (axis · category · severity), summary, failure
#         scenario, dispute footer.
#
#   rp_post_inline <pr> <commit_sha> <path> <line> <body>
#       -> posts one inline review comment anchored to a right-hand diff line
#         (REST pulls/comments with commit_id/path/line/side=RIGHT). Non-zero
#         on API failure; the skill's 422 fallback keys off this.
#
#   rp_filter_agent_threads
#       -> pure stdin filter over tr_unresolved_threads' JSON array: keeps only
#         elements whose first-comment body contains RP_MARKER. The ONLY thread
#         set the fix loop may touch; human threads never pass.
#
#   rp_done_marker_present <pr>
#       -> exit 0 iff any top-level PR comment carries RP_DONE_MARKER. The
#         once-per-PR idempotency gate the review round checks before it
#         starts and the skill's pre-flight re-checks.
#
#   rp_post_done_marker <pr> <n_findings> <m_fixed> <reviewed_sha> <fix_sha>
#       -> posts the top-level completion comment: machine marker with both
#         SHAs (`fixes=none` when nothing was pushed) + human-readable tally.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        or AUTO_AGENT_TARGET_DIR): repo.slug
#   GH_BIN               gh CLI (default: gh), so tests stub the network
#                        away; behavior under test is the arguments passed
#                        and the parse of the responses, never a live API.

_review_poster_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_review_poster_lib_dir}/harness-config.sh"


RP_MARKER='<!-- pr-review-bot -->'
RP_DONE_MARKER='<!-- pr-review-done'

# _rp_slug -> the Target Project's owner/repo, or return 2 with a stderr line.
_rp_slug() { harness_config_slug review-poster; }

# rp_render_finding <axis> <category> <severity> <summary> <failure_scenario>
rp_render_finding() {
    local axis="$1" category="$2" severity="$3" summary="$4" scenario="$5"
    printf '%s\n' \
        "${RP_MARKER}" \
        "🤖 **pr-review** · ${axis} · ${category} · ${severity}" \
        "" \
        "${summary}" \
        "" \
        "**Failure scenario:** ${scenario}" \
        "" \
        "_Automated one-time review (the Fire's review round). A fix round follows; reply to dispute._"
}

# rp_post_inline <pr> <commit_sha> <path> <line> <body>
rp_post_inline() {
    local pr="$1" commit="$2" path="$3" line="$4" body="$5" repo
    repo="$(_rp_slug)" || return $?
    "${GH_BIN:-gh}" api "repos/${repo}/pulls/${pr}/comments" \
        -f body="${body}" \
        -f commit_id="${commit}" \
        -f path="${path}" \
        -F line="${line}" \
        -f side=RIGHT >/dev/null
}

# rp_filter_agent_threads  (stdin: tr_unresolved_threads JSON array)
rp_filter_agent_threads() {
    jq -c --arg marker "${RP_MARKER}" \
        '[ .[] | select(.body | contains($marker)) ]'
}

# rp_done_marker_present <pr>
rp_done_marker_present() {
    local pr="$1" repo resp
    repo="$(_rp_slug)" || return $?
    resp="$("${GH_BIN:-gh}" api "repos/${repo}/issues/${pr}/comments" --paginate)" || return 1
    printf '%s' "${resp}" | jq -e --arg marker "${RP_DONE_MARKER}" \
        '[ .[] | select(.body | contains($marker)) ] | length > 0' >/dev/null
}

# rp_post_done_marker <pr> <n_findings> <m_fixed> <reviewed_sha> <fix_sha>
rp_post_done_marker() {
    local pr="$1" n="$2" m="$3" reviewed="$4" fix="$5" repo body
    repo="$(_rp_slug)" || return $?
    body="$(printf '%s\n' \
        "${RP_DONE_MARKER} reviewed=${reviewed} fixes=${fix} -->" \
        "🤖 pr-review: ${n} findings, ${m} fixed (reviewed ${reviewed})")"
    "${GH_BIN:-gh}" api "repos/${repo}/issues/${pr}/comments" \
        -f body="${body}" >/dev/null
}
