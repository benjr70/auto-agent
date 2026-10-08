#!/usr/bin/env bash
# ruling.sh: the Ruling request: compose, post, parse, apply (issue #78).
#
# Sourceable library (and a CLI, `bin/auto-agent ruling <sub> …`) owning the
# one consolidated comment through which everything on an Agent PR that needs
# the human reaches them — a product ambiguity, or a dispute on a thread the
# human wrote — and the one-line Ruling (`1A 2B`) they answer with. Both
# /auto-agent:pr-reconcile and /auto-agent:afk-pickup post through here;
# neither hand-rolls the comment, the grammar, or the `AFK:ruling` label.
# The rendered shapes are the prototype's, docs/prototypes/ruling-request-pr722.md,
# which lib/ruling.test.sh matches section for section.
#
# A decision is one JSON object; a request is an array of them, numbered 1..n
# by position:
#   { "title": "<heading>", "scenario": "<one line>",
#     "now": "<what the code does>", "wants": "<what the reviewer wants>",
#     "why": "<why this is the human's call>",
#     "thread": { "threadId", "commentDatabaseId", "path", "line" } | absent,
#     "options": [ { "letter": "A" (optional; the position's letter when absent),
#                    "text": "<the option>", "cost": "<what it costs>",
#                    "recommended": true|false, "fix": true|false } ] }
# `fix: true` marks an option that changes code when chosen (the footer says
# whether the all-recommended reply ends the PR or needs one more Fire).
#
#   ruling_compose <decisions_json> <head_sha> <evidence> [<supersedes_id>]
#       -> stdout: the request comment: RULING_REQUEST_MARKER_PREFIX line
#         (`head=<sha> decisions=<n>`), heading, reply instructions, one
#         section per decision with its option table, a footer stating the
#         head's verification <evidence> and what the all-recommended reply
#         does — and, last, a hidden block carrying the decisions themselves
#         (base64 JSON) so ruling_pending can read them back from GitHub.
#         <supersedes_id> names an earlier, still-open request this one
#         replaces (its decisions carried over first, numbers unchanged, the
#         new ones after): the instructions say so, and ask for one reply
#         here — ruling_pending reads only the latest request, so there is
#         never more than one to answer.
#
#   ruling_post <pr> <decisions_json> <head_sha> <evidence> [<supersedes_id>]
#       -> posts the composed request as a top-level PR comment, then applies
#         `AFK:ruling` (HARNESS_LABEL_RULING) to the PR. Prints the comment id.
#         Never touches AFK:revise-failed: a request is a wait, not a failure.
#         A failed post applies no label and returns non-zero.
#
#   ruling_parse <decisions_json> <reply>
#       -> JSON: { "status": "full"|"partial"|"invalid",
#                  "answers": { "<n>": "<LETTER>", … }, "missing": [<n>, …],
#                  "ruling": "1A 2B" (the canonical form), "reason": "<why invalid>" }
#         The grammar: a reply is a Ruling iff, after case and spacing are
#         dropped, it consists ONLY of decision-letter pairs (`1a 2B`, `1A2B`
#         and `1A 2B` are the same Ruling); every pair names a decision in
#         1..n and one of its letters; the same decision answered twice with
#         different letters contradicts itself. `partial` names some but not
#         all decisions (apply what is named, re-post the rest); `invalid`
#         applies nothing (`I agree, resolve it`, `1Z`, an empty reply).
#
#   ruling_remaining <decisions_json> <answers_json>
#       -> the decisions not named in <answers_json>, renumbered from 1: what a
#         partial reply's re-posted request holds.
#
#   ruling_compose_applied <results_json> <head_sha> <ruling> <evidence>
#       -> stdout: the `## ✅ Ruling applied · <ruling>` comment, one line per
#         decision from <results_json> (`[ { "n", "letter", "summary",
#         "sha"|null, "thread": "<path:line>"|null } ]`: a sha means a fix
#         landed, a thread means it was resolved), then the re-run evidence.
#
#   ruling_post_applied <pr> <body>
#       -> posts the applied comment, then removes `AFK:ruling`. The label
#         comes off only after the comment is up, so a crash between leaves the
#         PR still waiting rather than silently unlabelled.
#
#   ruling_thread_reply <n> <letter> <summary> [<sha>]
#       -> stdout: the one-line text of the in-thread reply a ruled thread
#         gets (`Ruling 1A: <summary> Resolving, no change.` / `… Fixed in
#         \`<sha>\`; resolving.`). It is posted through the Thread Reconciler
#         (tr_reply / tr_resolve_with_reply) under RULING_THREAD_MARKER —
#         `$TR_MARKER_RULING`, the one marker that lib owns for a Ruling
#         applied — never through this lib.
#
#   ruling_nudge <pr> <decisions_json> <reason>
#       -> posts the ONE marked reply an invalid answer gets: what was
#         expected (the decisions and their letters), never a label change.
#
#   ruling_pending <pr>
#       -> JSON: { "request": { "id", "head", "decisions": [...], "createdAt" } | null,
#                  "reply":   { "id", "body", "status", "answers", "missing",
#                               "ruling", "reason", "nudged": bool } | null }
#         The latest request comment on the PR not yet followed by an applied
#         comment, with its decisions decoded; and the human's answer to it,
#         read from EVERY later comment that is not the loop's own (not under
#         the machine user's login, no hidden marker on its first line):
#         each comment that parses as a Ruling contributes its pairs, a later
#         letter for a decision replacing an earlier one, so `1A` then `2B`
#         in two comments is the one Ruling `1A 2B`. `id`/`body` are the
#         latest human comment's; the reply is `invalid` (with that comment's
#         reason) only when no comment parsed. `nudged` says a nudge already
#         went out on this request (one nudge per request, however many
#         non-Rulings follow it). Exit 0 with both null when nothing is
#         pending; exit 1 when the machine user's login cannot be resolved.
#
#   ruling_decisions_from_body <comment_body>
#       -> the decisions JSON a request comment carries (the hidden block).
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        or AUTO_AGENT_TARGET_DIR): repo.slug
#   GH_BIN               gh CLI (default: gh), so tests stub the network away;
#                        behaviour under test is the arguments passed and the
#                        parse of the responses, never a live API.
#   RULING_AGENT_LOGIN   the machine user's login (default: DAEMON_GH_LOGIN,
#                        else `gh api user`); its comments are never read as
#                        a reply. ruling_pending refuses to guess without it.

_ruling_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_ruling_lib_dir}/harness-config.sh"
# shellcheck source=thread-reconciler.sh
. "${_ruling_lib_dir}/thread-reconciler.sh"   # TR_MARKER_RULING: the in-thread voice of a Ruling applied

RULING_REQUEST_MARKER_PREFIX='<!-- auto-agent:ruling-request'
RULING_APPLIED_MARKER_PREFIX='<!-- auto-agent:ruling-applied'
RULING_DECISIONS_MARKER_PREFIX='<!-- auto-agent:ruling-decisions'
RULING_NUDGE_MARKER='<!-- auto-agent:ruling-nudge -->'
RULING_THREAD_MARKER="${TR_MARKER_RULING}"

# _ruling_slug -> the configured repo slug, or 2 with a stderr line
_ruling_slug() { harness_config_slug ruling; }

# The letter rule, in one place: an option's letter is its own when given,
# else its position's (A, B, C, …), always upper-case. Compose and parse both
# read it through _ruling_normalize, so they cannot disagree on which letters
# a decision accepts.
_RULING_JQ_LETTER_RULE='
    def pos_letter: ["A","B","C","D","E","F","G","H","I","J","K","L","M","N","O","P","Q","R","S","T","U","V","W","X","Y","Z"][.];
    def fill_letters: .options |= [ to_entries[] | .value + { letter: ((.value.letter // (.key | pos_letter)) | ascii_upcase) } ];'

# _ruling_normalize <decisions_json> -> the decisions with every option's
# letter filled in (upper-case), so renderers read one shape.
_ruling_normalize() {
    printf '%s' "$1" | jq -c "${_RULING_JQ_LETTER_RULE}"' [ .[] | fill_letters ]'
}

# _ruling_letters <decisions_json> -> [[ "A","B","C" ], [ "A","B" ]]: each
# decision's letters, read off the normalized shape.
_ruling_letters() {
    _ruling_normalize "$1" | jq -c '[ .[] | [ .options[].letter ] ]'
}

# _ruling_all_recommended <normalized> -> "1A 2A": the all-recommended reply
# (the first option when a decision marks none).
_ruling_all_recommended() {
    printf '%s' "$1" | jq -r '
        [ to_entries[] | ((.key + 1) | tostring) + ((.value.options | map(select(.recommended == true)) | .[0] // .value.options[0]) | .letter) ]
        | join(" ")'
}

# ruling_compose <decisions_json> <head_sha> <evidence> [<supersedes_id>]
ruling_compose() {
    local decisions="${1:?ruling_compose: decisions json required}" head="${2:?ruling_compose: head sha required}" evidence="${3:?ruling_compose: evidence required}"
    local supersedes="${4:-}" norm n all ends noun
    norm="$(_ruling_normalize "${decisions}")" || return 2
    n="$(printf '%s' "${norm}" | jq 'length')"
    [ "${n}" -gt 0 ] || { echo "ruling: ruling_compose: no decisions to ask" >&2; return 2; }
    all="$(_ruling_all_recommended "${norm}")"
    # the all-recommended reply ends the PR unless a recommended option is a fix
    if printf '%s' "${norm}" | jq -e '
        any(.[]; ((.options | map(select(.recommended == true)) | .[0] // .options[0]) | .fix == true))' >/dev/null; then
        ends="applies the fix and re-runs verification in one more Fire."
    else
        ends="leaves the PR ready to merge with no further Fire."
    fi
    noun="decisions"; [ "${n}" -eq 1 ] && noun="decision"

    printf '%s\n' \
        "${RULING_REQUEST_MARKER_PREFIX} head=${head} decisions=${n} -->" \
        "## 🧑‍⚖️ Ruling request · ${n} ${noun}" \
        "" \
        "Reply with one line, one letter per decision: \`${all}\`. Nothing else in the reply is read." \
        ""
    [ -z "${supersedes}" ] || printf '%s\n' \
        "This request supersedes the earlier one (comment ${supersedes}): its decisions are carried here under the same numbers, with the new ones after. Reply to this one — a reply to the earlier one is not read." \
        ""
    printf '%s' "${norm}" | jq -r '
        to_entries[]
        | "### \(.key + 1). \(.value.title)\n\n\(.value.scenario)\n\n- **Now:** \(.value.now)\n- **Reviewer wants:** \(.value.wants)\n- **Why this is yours:** \(.value.why)\n\n| | Option | What it costs |\n|---|---|---|"
          + ( [ .value.options[] | "\n| **\(.letter)** | \(.text) | \(.cost)\(if .recommended == true then " _Recommended._" else "" end) |" ] | join("") )
          + "\n"'
    printf '%s\n' \
        "---" \
        "" \
        "Head \`${head}\`: ${evidence}. A reply of \`${all}\` ${ends}" \
        "${RULING_DECISIONS_MARKER_PREFIX} $(printf '%s' "${norm}" | base64 | tr -d '\n') -->"
}

# ruling_decisions_from_body <comment_body>
ruling_decisions_from_body() {
    local body="${1-}" enc
    enc="$(printf '%s\n' "${body}" | sed -nE "s|^${RULING_DECISIONS_MARKER_PREFIX} ([A-Za-z0-9+/=]+) -->[[:space:]]*$|\1|p" | head -1)"
    [ -n "${enc}" ] || { echo "ruling: no decisions block in that comment" >&2; return 1; }
    printf '%s' "${enc}" | base64 -d | jq -c .
}

# ruling_parse <decisions_json> <reply>
ruling_parse() {
    local decisions="${1:?ruling_parse: decisions json required}" reply="${2-}" letters compact
    letters="$(_ruling_letters "${decisions}")" || return 2
    compact="$(printf '%s' "${reply}" | tr -d '[:space:]' | tr '[:lower:]' '[:upper:]')"
    jq -nc --argjson letters "${letters}" --arg compact "${compact}" '
        def invalid(r): { status: "invalid", answers: {}, missing: [range(1; ($letters | length) + 1)], ruling: "", reason: r };
        ($letters | length) as $n
        | if $compact == "" then invalid("empty reply; expected decision-letter pairs such as 1A 2B")
          elif ($compact | test("^([0-9]+[A-Z])+$") | not) then invalid("not only decision-letter pairs; free text is not read")
          else
            ([ $compact | scan("[0-9]+[A-Z]") | { n: (.[:-1] | tonumber), letter: .[-1:] } ]) as $pairs
            | (
                [ $pairs[] | select(.n < 1 or .n > $n) | .n ] | unique
              ) as $unknown
            | (
                [ $pairs[] | select(.n >= 1 and .n <= $n) | . as $p | select(($letters[$p.n - 1] | index($p.letter)) == null) | "\($p.n)\($p.letter)" ] | unique
              ) as $badletter
            | (
                [ $pairs | group_by(.n)[] | select((map(.letter) | unique | length) > 1) | .[0].n ] | unique
              ) as $contradicted
            | if ($unknown | length) > 0 then invalid("no decision \($unknown | map(tostring) | join(", ")); decisions are 1..\($n)")
              elif ($badletter | length) > 0 then invalid("no such option \($badletter | join(", ")); the letters are in the request")
              elif ($contradicted | length) > 0 then invalid("decision \($contradicted | map(tostring) | join(", ")) answered twice with different letters")
              else
                ([ $pairs[] | { key: (.n | tostring), value: .letter } ] | unique | from_entries) as $answers
                | ([ range(1; $n + 1) | select(($answers[tostring]) == null) ]) as $missing
                | { status: (if ($missing | length) == 0 then "full" else "partial" end),
                    answers: $answers,
                    missing: $missing,
                    ruling: ([ $answers | to_entries[] | "\(.key)\(.value)" ] | sort_by(.[:-1] | tonumber) | join(" ")),
                    reason: "" }
              end
          end'
}

# ruling_remaining <decisions_json> <answers_json>
ruling_remaining() {
    local decisions="${1:?ruling_remaining: decisions json required}" answers="${2:?ruling_remaining: answers json required}"
    printf '%s' "${decisions}" | jq -c --argjson a "${answers}" '
        [ to_entries[] | select(($a[(.key + 1) | tostring]) == null) | .value ]'
}

# ruling_compose_applied <results_json> <head_sha> <ruling> <evidence>
ruling_compose_applied() {
    local results="${1:?ruling_compose_applied: results json required}" head="${2:?ruling_compose_applied: head sha required}"
    local ruling="${3:?ruling_compose_applied: ruling required}" evidence="${4:?ruling_compose_applied: evidence required}"
    printf '%s\n' \
        "${RULING_APPLIED_MARKER_PREFIX} head=${head} ruling=\"${ruling}\" -->" \
        "## ✅ Ruling applied · \`${ruling}\`" \
        ""
    printf '%s' "${results}" | jq -r '
        .[]
        | "\(.n). **\(.letter)** — \(.summary)"
          + (if .sha != null and .sha != "" then
               " Fixed in `\(.sha)`" + (if .thread != null and .thread != "" then "; thread on `\(.thread)` resolved." else "." end)
             elif .thread != null and .thread != "" then
               " Thread on `\(.thread)` resolved, no change."
             else
               " No change."
             end)'
    printf '%s\n' "" "Verification re-ran on \`${head}\`: ${evidence}"
}

# ruling_thread_reply <n> <letter> <summary> [<sha>]
ruling_thread_reply() {
    local n="${1:?ruling_thread_reply: decision number required}" letter="${2:?ruling_thread_reply: letter required}"
    local summary="${3:?ruling_thread_reply: summary required}" sha="${4:-}"
    if [ -n "${sha}" ]; then
        printf 'Ruling %s%s: %s Fixed in `%s`; resolving.\n' "${n}" "${letter}" "${summary}" "${sha}"
    else
        printf 'Ruling %s%s: %s Resolving, no change.\n' "${n}" "${letter}" "${summary}"
    fi
}

# _ruling_post_comment <slug> <pr> <body> -> prints the new comment's id
_ruling_post_comment() {
    local slug="$1" pr="$2" body="$3" resp
    resp="$("${GH_BIN:-gh}" api "repos/${slug}/issues/${pr}/comments" -f body="${body}")" || return 1
    printf '%s' "${resp}" | jq -r '.id'
}

# ruling_post <pr> <decisions_json> <head_sha> <evidence> [<supersedes_id>]
ruling_post() {
    local pr="${1:?ruling_post: pr number required}" decisions="${2:?ruling_post: decisions json required}"
    local head="${3:?ruling_post: head sha required}" evidence="${4:?ruling_post: evidence required}" supersedes="${5:-}" slug body id
    slug="$(_ruling_slug)" || return $?
    body="$(ruling_compose "${decisions}" "${head}" "${evidence}" "${supersedes}")" || return $?
    id="$(_ruling_post_comment "${slug}" "${pr}" "${body}")" || {
        echo "ruling: posting the request on PR #${pr} failed; AFK:ruling not applied" >&2
        return 1
    }
    "${GH_BIN:-gh}" pr edit "${pr}" --repo "${slug}" --add-label "${HARNESS_LABEL_RULING}" >/dev/null || {
        echo "ruling: request posted (comment ${id}) but AFK:ruling could not be applied on PR #${pr}" >&2
        return 1
    }
    printf '%s\n' "${id}"
}

# ruling_post_applied <pr> <body>
ruling_post_applied() {
    local pr="${1:?ruling_post_applied: pr number required}" body="${2:?ruling_post_applied: body required}" slug id
    slug="$(_ruling_slug)" || return $?
    id="$(_ruling_post_comment "${slug}" "${pr}" "${body}")" || {
        echo "ruling: posting the applied comment on PR #${pr} failed; AFK:ruling left on" >&2
        return 1
    }
    "${GH_BIN:-gh}" pr edit "${pr}" --repo "${slug}" --remove-label "${HARNESS_LABEL_RULING}" >/dev/null || {
        echo "ruling: applied comment posted (comment ${id}) but AFK:ruling could not be removed from PR #${pr}" >&2
        return 1
    }
    printf '%s\n' "${id}"
}

# ruling_nudge <pr> <decisions_json> <reason>
ruling_nudge() {
    local pr="${1:?ruling_nudge: pr number required}" decisions="${2:?ruling_nudge: decisions json required}" reason="${3:?ruling_nudge: reason required}"
    local slug letters expected all body
    slug="$(_ruling_slug)" || return $?
    letters="$(_ruling_letters "${decisions}")" || return 2
    expected="$(printf '%s' "${letters}" | jq -r '
        to_entries[] | "- decision \(.key + 1): " + (if (.value | length) > 1 then ((.value[:-1] | join(", ")) + " or " + .value[-1]) else .value[0] end)')"
    all="$(_ruling_all_recommended "$(_ruling_normalize "${decisions}")")"
    body="$(printf '%s\n' \
        "${RULING_NUDGE_MARKER}" \
        "That reply is not a Ruling (${reason}), so nothing was applied. Reply with one line of decision-letter pairs, nothing else — for example \`${all}\`:" \
        "" \
        "${expected}")"
    _ruling_post_comment "${slug}" "${pr}" "${body}" >/dev/null
}

# _ruling_agent_login -> the machine user's login: RULING_AGENT_LOGIN, else
# DAEMON_GH_LOGIN (the Host env), else what gh is logged in as (`gh api user`,
# the same source lib/work-probe.sh falls back to). Never a guess: when none
# answers, return 1 — a comment can only be told ours-or-the-human's by login.
_ruling_agent_login() {
    local login="${RULING_AGENT_LOGIN:-${DAEMON_GH_LOGIN:-}}"
    if [ -z "${login}" ]; then
        login="$("${GH_BIN:-gh}" api user -q .login 2>/dev/null || true)"
    fi
    [ -n "${login}" ] || { echo "ruling: cannot tell the machine user's comments from the human's: set DAEMON_GH_LOGIN (or RULING_AGENT_LOGIN), or log gh in as the machine user" >&2; return 1; }
    printf '%s' "${login}"
}

# ruling_pending <pr>
ruling_pending() {
    local pr="${1:?ruling_pending: pr number required}" slug agent resp req decisions humans reply parsed acc pairs c
    slug="$(_ruling_slug)" || return $?
    agent="$(_ruling_agent_login)" || return 1
    resp="$("${GH_BIN:-gh}" api "repos/${slug}/issues/${pr}/comments" --paginate)" || return 1
    # --paginate concatenates pages as separate arrays; flatten them into one.
    resp="$(printf '%s' "${resp}" | jq -sc 'if (.[0] | type) == "array" then add // [] else . end')" || return 1

    # The latest request not followed by an applied comment.
    req="$(printf '%s' "${resp}" | jq -c --arg rq "${RULING_REQUEST_MARKER_PREFIX}" --arg ap "${RULING_APPLIED_MARKER_PREFIX}" '
        def first_line: (.body // "") | split("\n")[0];
        reduce .[] as $c (null;
            if ($c | first_line | startswith($rq)) then $c
            elif ($c | first_line | startswith($ap)) then null
            else . end)')"
    if [ "${req}" = "null" ]; then
        printf '%s\n' '{"request":null,"reply":null}'
        return 0
    fi
    decisions="$(ruling_decisions_from_body "$(printf '%s' "${req}" | jq -r '.body')")" || return 1

    # The human's comments after the request, in order, and whether this
    # request was already nudged (one nudge per request: a second free-text
    # reply earns no second nudge). The loop's own comments are never a
    # reply: anything the machine user wrote (by login — the human answers
    # under their own account, ADR 0005), and anything carrying a hidden
    # marker on its first line (the loop's voice under any login). No
    # first-line guessing beyond that: a plain-text comment under another
    # login is the human's.
    humans="$(printf '%s' "${resp}" | jq -c --argjson rid "$(printf '%s' "${req}" | jq '.id')" --arg nudge "${RULING_NUDGE_MARKER}" --arg agent "${agent}" '
        def first_line: (.body // "") | split("\n")[0] | sub("[[:space:]]+$"; "");
        def ours: ((.user.login // "") == $agent) or (first_line | startswith("<!--"));
        [ .[] | select(.id > $rid) ] as $later
        | { comments: [ $later[] | select(ours | not) | { id, body } ],
            nudged: ([ $later[] | select(first_line == $nudge) ] | length > 0) }')"
    if [ "$(printf '%s' "${humans}" | jq '.comments | length')" -eq 0 ]; then
        reply="null"
    else
        # Every comment that parses as a Ruling (full or partial) contributes
        # its pairs; a later comment's letter for a decision replaces an
        # earlier one (the human changed their mind), so `1A` then `2B` in two
        # comments is the one Ruling `1A 2B`. Only when no comment parses is
        # the reply invalid, with the latest comment's reason.
        acc='{}'; parsed=""
        while IFS= read -r c; do
            parsed="$(ruling_parse "${decisions}" "$(printf '%s' "${c}" | jq -r '.body')")" || return 1
            acc="$(jq -nc --argjson a "${acc}" --argjson p "${parsed}" 'if $p.status == "invalid" then $a else $a + $p.answers end')"
        done < <(printf '%s' "${humans}" | jq -c '.comments[]')
        if [ "$(printf '%s' "${acc}" | jq 'length')" -gt 0 ]; then
            pairs="$(printf '%s' "${acc}" | jq -r '[ to_entries[] | "\(.key)\(.value)" ] | join(" ")')"
            parsed="$(ruling_parse "${decisions}" "${pairs}")" || return 1
        fi
        reply="$(printf '%s' "${humans}" | jq -c --argjson p "${parsed}" '.comments[-1] + $p + { nudged: .nudged }')"
    fi
    jq -nc --argjson req "${req}" --argjson d "${decisions}" --argjson reply "${reply}" '
        { request: { id: $req.id,
                     head: ($req.body | split("\n")[0] | capture("head=(?<h>[^ ]+)") | .h),
                     decisions: $d,
                     createdAt: $req.created_at },
          reply: $reply }'
}

_ruling_usage() { sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; }

# _ruling_read_json <path|-> -> the path's JSON (stdin for -). The path may be
# a regular file or a process substitution (`<(jq …)`, a pipe under /dev/fd),
# as the runbooks call it: anything readable is accepted.
_ruling_read_json() {
    if [ -z "${1:-}" ] || [ "$1" = "-" ]; then cat; else
        [ -e "$1" ] || { echo "ruling: file not found: $1" >&2; return 2; }
        cat -- "$1" || { echo "ruling: cannot read: $1" >&2; return 2; }
    fi
}

ruling_main() {
    local sub="${1:-}"; shift || true
    local file="" pr="" head="" evidence="" ruling="" reason="" supersedes="" arg json
    case "${sub}" in
        compose|post|parse|remaining|compose-applied|post-applied|nudge|pending|thread-reply) ;;
        -h|--help|help) _ruling_usage; return 0 ;;
        *) echo "ruling: unknown subcommand '${sub}'" >&2; _ruling_usage >&2; return 2 ;;
    esac
    case "${sub}" in
        parse)
            file="${1:-}"; shift || true
            json="$(_ruling_read_json "${file}")" || return $?
            ruling_parse "${json}" "${1-}"; return $? ;;
        remaining)
            file="${1:-}"; shift || true
            json="$(_ruling_read_json "${file}")" || return $?
            ruling_remaining "${json}" "${1:?ruling remaining: answers json required}"; return $? ;;
        thread-reply)
            ruling_thread_reply "$@"; return $? ;;
    esac
    # the flag-taking subcommands: the first positional is the JSON file (or -)
    while [ $# -gt 0 ]; do
        arg="$1"; shift
        case "${arg}" in
            --pr) pr="${1:-}"; shift ;;
            --head) head="${1:-}"; shift ;;
            --evidence) evidence="${1:-}"; shift ;;
            --ruling) ruling="${1:-}"; shift ;;
            --reason) reason="${1:-}"; shift ;;
            --supersedes) supersedes="${1:-}"; shift ;;
            *) file="${arg}" ;;
        esac
    done
    if [ "${sub}" != "pending" ]; then
        json="$(_ruling_read_json "${file}")" || return $?
    fi
    case "${sub}" in
        compose)         ruling_compose "${json}" "${head}" "${evidence}" "${supersedes}" ;;
        post)            ruling_post "${pr}" "${json}" "${head}" "${evidence}" "${supersedes}" ;;
        compose-applied) ruling_compose_applied "${json}" "${head}" "${ruling}" "${evidence}" ;;
        post-applied)    ruling_post_applied "${pr}" "${json}" ;;
        nudge)           ruling_nudge "${pr}" "${json}" "${reason}" ;;
        pending)         ruling_pending "${pr}" ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    ruling_main "$@"
fi
