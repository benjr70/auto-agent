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
#         only ever works threads a human still considers open. Every
#         unresolved thread's comments are read whole: a thread longer than
#         one page is followed page by page before anything is derived from
#         it. Exit 0 even when the array is empty; non-zero only when an API
#         call itself fails.
#
#         Authorship is by marker, never by login: the machine user replies to
#         its own review under the same login as the review. A thread is
#         `authored: bot` iff the FIRST LINE of its first comment is RP_MARKER
#         (`<!-- pr-review-bot -->`, owned by lib/review-poster.sh); anything
#         else is `human`, including a human comment that quotes the marker
#         further down. `replies` is every later comment in order; `agent` is
#         true iff the reply's first line is one of the loop's own markers
#         below (a quoted marker inside a human reply does not count).
#         `ruling` is the last reply's body when a human spoke last AFTER the
#         loop had spoken in that thread; null when there are no replies, the
#         loop never replied, or the loop spoke last. A human reply that
#         answers the loop's marked reply rules the thread; a human follow-up
#         on a thread the loop has not answered is just more of the thread
#         (the implementer reads it whole either way); a loop reply after the
#         human's means the ruling was already acted on.
#
#   tr_reply <pr> <comment_database_id> <marker> <text>
#       -> posts an in-thread reply (REST `in_reply_to`) so the answer lands on
#         the reviewer's comment, not as a detached PR comment. The marker is
#         required and must be one of TR_MARKER_*; the body is the marker on
#         its own first line, then the text, so the next
#         tr_unresolved_threads read flags the reply `agent`. An unknown
#         marker returns 2 with a stderr line and posts nothing.
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
#   TR_MARKER_FIX / TR_MARKER_ARBITER / TR_MARKER_RULING / TR_MARKER_ESCALATE
#       -> the loop's reply markers; every reply the reconcile loop posts
#         carries exactly one of them, and each means one thing (see below).
#         _TR_MARKERS is the list the lib derives from them; nothing else in
#         the lib names a marker string twice.
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
# whose first line is none of them is a human's.
TR_MARKER_FIX='<!-- auto-agent:fix -->'            # a fix commit recorded in-thread
TR_MARKER_ARBITER='<!-- auto-agent:arbiter -->'    # an Arbiter verdict (dismissal)
TR_MARKER_RULING='<!-- auto-agent:ruling -->'      # a Ruling applied (with or without a commit)
TR_MARKER_ESCALATE='<!-- auto-agent:escalate -->'  # parked for human triage; resolves nothing
# Every loop marker, derived from the constants above: the one place the
# lib enumerates them. A new marker is added here and nowhere else.
_TR_MARKERS=("${TR_MARKER_FIX}" "${TR_MARKER_ARBITER}" "${TR_MARKER_RULING}" "${TR_MARKER_ESCALATE}")

# _tr_slug -> the configured repo slug, or 2 with a stderr line
_tr_slug() { harness_config_slug thread-reconciler; }

# _tr_markers_json -> the marker list as a JSON array, for jq
_tr_markers_json() { printf '%s\n' "${_TR_MARKERS[@]}" | jq -Rc . | jq -sc .; }

# _tr_is_marker <string> -> 0 iff the string is exactly one of the loop's markers
_tr_is_marker() {
    local m
    for m in "${_TR_MARKERS[@]}"; do [ "$1" = "${m}" ] && return 0; done
    return 1
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
          comments(first: 100) {
            pageInfo { hasNextPage endCursor }
            nodes { databaseId body }
          }
        }
      }
    }
  }
}' \
        -F owner="${owner}" -F name="${name}" -F pr="${pr}")" || return 1

    # Follow the comment pages of every unresolved thread longer than one
    # page, so replies and ruling come from the whole thread, never a stale
    # tail. The list of threads to follow is fixed up front; each one's pages
    # are appended into resp before anything is derived from it.
    local tid cursor page
    while IFS=$'\t' read -r tid cursor; do
        [ -n "${tid}" ] || continue
        while [ -n "${cursor}" ]; do
            page="$("${GH_BIN:-gh}" api graphql \
                -f query='
query($id: ID!, $after: String!) {
  node(id: $id) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId body }
      }
    }
  }
}' \
                -F id="${tid}" -F after="${cursor}")" || return 1
            resp="$(printf '%s' "${resp}" | jq -c --arg id "${tid}" --argjson page "${page}" '
                ($page.data.node.comments) as $more
                | (.data.repository.pullRequest.reviewThreads.nodes[] | select(.id == $id) | .comments)
                  |= (.nodes += ($more.nodes // []) | .pageInfo = $more.pageInfo)')" || return 1
            cursor="$(printf '%s' "${page}" | jq -r '
                .data.node.comments.pageInfo
                | if .hasNextPage == true then (.endCursor // "") else "" end')" || return 1
        done
    done < <(printf '%s' "${resp}" | jq -r '
        .data.repository.pullRequest.reviewThreads.nodes[]?
        | select(.isResolved == false and .comments.pageInfo.hasNextPage == true)
        | [.id, (.comments.pageInfo.endCursor // "")] | @tsv')

    printf '%s' "${resp}" | jq -c --arg bot "${RP_MARKER}" --argjson markers "$(_tr_markers_json)" '
        # The marker is the first line of a body, and only there: a quoted
        # marker further down is someone talking about the marker.
        def first_line: (. // "") | split("\n")[0] | sub("[[:space:]]+$"; "");
        [ .data.repository.pullRequest.reviewThreads.nodes[]?
          | select(.isResolved == false)
          | (.comments.nodes[0]) as $first
          | ([ .comments.nodes[1:][]
               | { databaseId: .databaseId,
                   body: (.body // ""),
                   agent: ((.body | first_line) as $l | any($markers[]; . == $l)) } ]) as $replies
          | { threadId: .id,
              path: .path,
              line: .line,
              commentDatabaseId: $first.databaseId,
              body: ($first.body // ""),
              authored: (if ($first.body | first_line) == $bot then "bot" else "human" end),
              replies: $replies,
              # a ruling is a human reply that answers the loop: the last
              # reply is human AND the loop has replied earlier in the thread
              ruling: (if ($replies | length) > 0
                          and ($replies[-1].agent | not)
                          and any($replies[]; .agent)
                       then $replies[-1].body else null end) } ]'
}

# tr_reply <pr> <comment_database_id> <marker> <text>
tr_reply() {
    local pr="${1:?tr_reply: pr number required}" comment_id="${2:?tr_reply: comment id required}"
    local marker="${3:?tr_reply: marker required (one of TR_MARKER_*)}" text="${4:?tr_reply: text required}" slug
    if ! _tr_is_marker "${marker}"; then
        echo "thread-reconciler: tr_reply: unknown marker '${marker}'; every reply carries exactly one of TR_MARKER_FIX / TR_MARKER_ARBITER / TR_MARKER_RULING / TR_MARKER_ESCALATE" >&2
        return 2
    fi
    slug="$(_tr_slug)" || return $?
    "${GH_BIN:-gh}" api "repos/${slug}/pulls/${pr}/comments" \
        -f body="${marker}"$'\n'"${text}" \
        -F in_reply_to="${comment_id}" >/dev/null
}

# tr_resolve_with_reply <pr> <comment_database_id> <thread_id> <marker> <text>
tr_resolve_with_reply() {
    local pr="${1:?tr_resolve_with_reply: pr number required}" comment_id="${2:?tr_resolve_with_reply: comment id required}"
    local thread_id="${3:?tr_resolve_with_reply: thread id required}" marker="${4:?tr_resolve_with_reply: marker required}" text="${5:?tr_resolve_with_reply: text required}"
    tr_reply "${pr}" "${comment_id}" "${marker}" "${text}" || return $?
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
