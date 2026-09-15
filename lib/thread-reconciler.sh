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
#             "body": "<first comment body>" } ]
#         Resolved and outdated-but-resolved threads are excluded: the loop
#         only ever works threads a human still considers open. Exit 0 even
#         when the array is empty; non-zero only when the API call itself fails.
#
#   tr_reply <pr> <comment_database_id> <body>
#       -> posts an in-thread reply (REST `in_reply_to`) so the answer lands on
#         the reviewer's comment, not as a detached PR comment.
#
#   tr_resolve <thread_id>
#       -> marks the thread resolved (GraphQL resolveReviewThread). The human
#         reopens the thread if the fix missed; that reopening is the signal
#         to re-apply `AFK:revise`.
#
# lib/review-poster.sh composes with this lib (`tr_unresolved_threads <pr> |
# rp_filter_agent_threads`) and owns the automated review's markers.
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

# _tr_slug -> the configured repo slug, or 2 with a stderr line
_tr_slug() {
    local cfg slug
    cfg="$(harness_config_resolve)" || { echo "thread-reconciler: no Harness config" >&2; return 2; }
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug // empty')"
    [ -n "${slug}" ] || { echo "thread-reconciler: the Harness config names no repo" >&2; return 2; }
    printf '%s\n' "${slug}"
}

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
          comments(first: 1) {
            nodes { databaseId body }
          }
        }
      }
    }
  }
}' \
        -F owner="${owner}" -F name="${name}" -F pr="${pr}")" || return 1

    printf '%s' "${resp}" | jq -c '
        [ .data.repository.pullRequest.reviewThreads.nodes[]?
          | select(.isResolved == false)
          | { threadId: .id,
              path: .path,
              line: .line,
              commentDatabaseId: (.comments.nodes[0].databaseId),
              body: (.comments.nodes[0].body // "") } ]'
}

# tr_reply <pr> <comment_database_id> <body>
tr_reply() {
    local pr="${1:?tr_reply: pr number required}" comment_id="${2:?tr_reply: comment id required}" body="${3:?tr_reply: body required}" slug
    slug="$(_tr_slug)" || return $?
    "${GH_BIN:-gh}" api "repos/${slug}/pulls/${pr}/comments" \
        -f body="${body}" \
        -F in_reply_to="${comment_id}" >/dev/null
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
