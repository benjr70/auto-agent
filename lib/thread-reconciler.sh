#!/usr/bin/env bash
# thread-reconciler.sh: the Thread Reconciler: enumerate, answer, and close
# review-comment threads on an Agent PR.
#
# Sourceable library wrapping the three GitHub operations the reconcile round's
# comment loop needs, behind stable functions so the skill never hand-rolls
# GraphQL. Carried over from the Smart Smoker harness; the repo every call
# names is now the Target Project's slug from the Harness config (ADR 0002),
# so no function takes or carries a repo argument. A call that cannot resolve
# the config returns 2 with a stderr line and makes no gh call.
#
#   tr_unresolved_threads <pr>
#       -> JSON array on stdout, one element per UNRESOLVED review thread:
#         [ { "threadId": "<graphql node id>", "path": "<file|null>",
#             "line": <n|null>, "commentDatabaseId": <rest id>,
#             "body": "<first comment body>",
#             "authored": "bot" | "human",
#             "replies": [ { "databaseId": <rest id>, "body": "<text>",
#                            "agent": true|false }, ... ],
#             "ruling": "<latest human reply>" | null } ]
#         Resolved and outdated-but-resolved threads are excluded: the loop
#         only ever works threads a human still considers open. Exit 0 even
#         when the array is empty; non-zero only when the API call itself fails.
#
#         Authorship is by marker, never by login: the machine user replies to
#         its own review under the same login as the review. A thread is
#         `authored: bot` iff its first comment carries RP_MARKER
#         (`<!-- pr-review-bot -->`, owned by lib/review-poster.sh); anything
#         else is `human`. `replies` is every later comment in order, `agent`
#         true iff the body carries one of the loop's own markers below.
#         `ruling` is the last reply's body when a human spoke last, null when
#         there are no replies or the loop spoke last: a human reply after the
#         loop's marked reply rules the thread; a loop reply after the human's
#         means the ruling was already acted on.
#
#   tr_reply <pr> <comment_database_id> <body> [<marker>]
#       -> posts an in-thread reply (REST `in_reply_to`) so the answer lands on
#         the reviewer's comment, not as a detached PR comment. With a marker,
#         the body is prefixed by it on its own first line, so the next
#         tr_unresolved_threads read flags the reply `agent`.
#
#   tr_resolve <thread_id>
#       -> marks the thread resolved (GraphQL resolveReviewThread). The human
#         reopens the thread if the fix missed; that reopening is the signal
#         to re-apply `AFK:revise`.
#
#   tr_resolve_with_reply <pr> <comment_database_id> <thread_id> <marker> <text>
#       -> tr_reply with the marker, then tr_resolve: resolves a thread with a
#         marked reply and no commit, for an Arbiter dismissal
#         (TR_MARKER_ARBITER) or a Ruling applied with no code change
#         (TR_MARKER_RULING). A failed reply returns non-zero and never
#         resolves.
#
#   TR_MARKER_FIX / TR_MARKER_ARBITER / TR_MARKER_RULING
#       -> the loop's three reply markers; every reply the reconcile loop
#         posts carries exactly one of them.
#
# lib/review-poster.sh composes with this lib (`tr_unresolved_threads <pr> |
# rp_filter_agent_threads`) and owns the automated review's markers; this lib
# sources it for RP_MARKER (the bot-thread marker) and owns the reply markers.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        or AUTO_AGENT_TARGET_DIR): repo.slug
#   GH_BIN               gh CLI (default: gh), so tests stub the network away;
#                        behavior under test is the arguments passed and the
#                        parse of the responses, never a live API.

_tr_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_tr_lib_dir}/harness-config.sh"
# shellcheck source=review-poster.sh
. "${_tr_lib_dir}/review-poster.sh"   # RP_MARKER: the bot-thread marker it owns

# The loop's own voice. Every reply the reconcile loop posts carries exactly
# one of these hidden markers as its first line, so a thread's later comments
# can be told apart by marker alone: the machine user answers its own review
# under the same login as the review, so the login says nothing. A comment
# carrying none of them is a human's.
TR_MARKER_FIX='<!-- auto-agent:fix -->'          # a fix commit recorded in-thread
TR_MARKER_ARBITER='<!-- auto-agent:arbiter -->'  # an Arbiter verdict (dismissal)
TR_MARKER_RULING='<!-- auto-agent:ruling -->'    # a Ruling applied
# jq regex matching any loop marker; kept in step with the three above.
_TR_AGENT_REPLY_RE='<!-- auto-agent:(fix|arbiter|ruling) -->'

# _tr_slug -> the configured repo slug, or 2 with a stderr line
_tr_slug() { harness_config_slug thread-reconciler; }

# tr_unresolved_threads <pr>
tr_unresolved_threads() {
    local pr="${1:?tr_unresolved_threads: pr number required}" slug owner name resp
    slug="$(_tr_slug)" || return $?
    owner="${slug%%/*}"
    name="${slug##*/}"

    resp="$("${GH_BIN:-gh}" api graphql \
        -f query='
query($owner: String!, $name: String!, $pr: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          path
          line
          comments(first: 100) {
            nodes { databaseId body }
          }
        }
      }
    }
  }
}' \
        -F owner="${owner}" -F name="${name}" -F pr="${pr}")" || return 1

    printf '%s' "${resp}" | jq -c --arg bot "${RP_MARKER}" --arg agent_re "${_TR_AGENT_REPLY_RE}" '
        [ .data.repository.pullRequest.reviewThreads.nodes[]?
          | select(.isResolved == false)
          | (.comments.nodes[0]) as $first
          | ([ .comments.nodes[1:][]
               | { databaseId: .databaseId,
                   body: (.body // ""),
                   agent: ((.body // "") | test($agent_re)) } ]) as $replies
          | { threadId: .id,
              path: .path,
              line: .line,
              commentDatabaseId: $first.databaseId,
              body: ($first.body // ""),
              authored: (if (($first.body // "") | contains($bot)) then "bot" else "human" end),
              replies: $replies,
              ruling: (if ($replies | length) > 0 and ($replies[-1].agent | not)
                       then $replies[-1].body else null end) } ]'
}

# tr_reply <pr> <comment_database_id> <body> [<marker>]
tr_reply() {
    local pr="${1:?tr_reply: pr number required}" comment_id="${2:?tr_reply: comment id required}" body="${3:?tr_reply: body required}" marker="${4:-}" slug
    slug="$(_tr_slug)" || return $?
    [ -n "${marker}" ] && body="${marker}"$'\n'"${body}"
    "${GH_BIN:-gh}" api "repos/${slug}/pulls/${pr}/comments" \
        -f body="${body}" \
        -F in_reply_to="${comment_id}" >/dev/null
}

# tr_resolve_with_reply <pr> <comment_database_id> <thread_id> <marker> <text>
tr_resolve_with_reply() {
    local pr="${1:?tr_resolve_with_reply: pr number required}" comment_id="${2:?tr_resolve_with_reply: comment id required}"
    local thread_id="${3:?tr_resolve_with_reply: thread id required}" marker="${4:?tr_resolve_with_reply: marker required}" text="${5:?tr_resolve_with_reply: text required}"
    tr_reply "${pr}" "${comment_id}" "${text}" "${marker}" || return $?
    tr_resolve "${thread_id}"
}

# tr_resolve <thread_id>
tr_resolve() {
    local thread_id="${1:?tr_resolve: thread id required}"
    "${GH_BIN:-gh}" api graphql \
        -f query='
mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}' \
        -F threadId="${thread_id}" >/dev/null
}
