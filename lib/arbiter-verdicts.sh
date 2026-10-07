#!/usr/bin/env bash
# arbiter-verdicts.sh: the reconcile loop's reading of the Arbiter's verdicts
# and of the implementer's per-thread replies, and its round arithmetic
# (issue #77: the Arbiter rules disputes on bot threads).
#
# Sourceable library. /auto-agent:pr-reconcile §2 spawns an implementer per
# round and, after round 1, one Arbiter over every disputed thread; the three
# rules that must survive any rewrite of that runbook are executable here so
# a test can pin them, instead of only grepping the prose:
#
#   av_parse_verdicts <threads_json> <reply>
#       -> JSON array, one element per thread in <threads_json> (the disputed
#         threads the Arbiter was given, each `{threadId, authored, ...}`):
#         [ { "threadId", "authored", "verdict": "fix"|"dismiss"|"ambiguity"|"unruled",
#             "text": "<instruction | reason | decision JSON>", "reason": "<why unruled>" } ]
#         A verdict line is `<threadId>: fix — <text>` / `dismiss — ` /
#         `ambiguity — `. A thread with no line, or more than one, or a verdict
#         word outside the three, is `unruled` (the caller collects it like an
#         ambiguity, never dismisses it). A `fix` or `dismiss` on a
#         human-authored thread is refused and is `unruled` too: a human's
#         thread is never dismissed, or ruled, on the Arbiter's word. A line
#         for a threadId that was not given is ignored.
#
#   av_parse_replies <threads_json> <reply> <round>
#       -> JSON array, one element per thread in <threads_json> (the threads
#         the implementer was given; one the Arbiter ruled carries
#         `"ruled": "fix"`):
#         [ { "threadId", "action": "fixed"|"no-change"|"dispute"|"cannot"|"unaddressed", "text" } ]
#         `<threadId>: revise-dispute — <reason>` is a `dispute` only in round 1
#         on an unruled thread. On a thread the Arbiter ruled `fix`, and in any
#         round after the first (the Arbiter runs once, after round 1, so no
#         ruling remains for a later dispute), the line is refused and read as
#         `cannot`. On a thread carrying a human `ruling` (the Thread
#         Reconciler's key, issue #76) it is refused and read as `no-change`
#         with the ruling as the text: a human ruling is never carried to a
#         dispute. `<threadId>: no-change — <what the ruling settled>` is
#         `no-change`; `<threadId>: cannot — <reason>` is `cannot`. Any other
#         `<threadId>: <text>` is `fixed`; no line is `unaddressed`.
#
#   av_next_round <round> <cap> <ruled_fixes_pending>
#       -> stdout: the next implementer round number, or `cap` when no
#         implementer round remains. The cap counts implementer rounds only
#         (an Arbiter run or a dismissal is not an input). The round that
#         applies the Arbiter's `fix` verdicts is guaranteed: after round 1,
#         with at least one ruled fix pending, the next round is 2 even when
#         the cap is 1. That ruled round is then the Fire's last under a cap
#         of 1.
#
# No gh, no git, no network: pure text in, JSON out.

# _av_lines <reply> -> the reply as a JSON array of lines
_av_lines() {
    printf '%s' "$1" | jq -Rs 'split("\n")'
}

# av_parse_verdicts <threads_json> <reply>
av_parse_verdicts() {
    local threads="${1:?av_parse_verdicts: threads json required}" reply="${2-}"
    jq -c --argjson lines "$(_av_lines "${reply}")" '
        ($lines
          | map(capture("^(?<id>[^:[:space:]]+): (?<word>[A-Za-z-]+)( — | -- | - )(?<text>.*)$"; "")
                | select(. != null)))
          as $parsed
        | [ .[]
            | . as $t
            | ($parsed | map(select(.id == $t.threadId))) as $mine
            | ($mine | map(select(.word == "fix" or .word == "dismiss" or .word == "ambiguity"))) as $ok
            | if ($mine | length) == 0 then
                { threadId: $t.threadId, authored: ($t.authored // "bot"), verdict: "unruled", text: "", reason: "no verdict line" }
              elif ($mine | length) > 1 then
                { threadId: $t.threadId, authored: ($t.authored // "bot"), verdict: "unruled", text: "", reason: "threadId doubled" }
              elif ($ok | length) == 0 then
                { threadId: $t.threadId, authored: ($t.authored // "bot"), verdict: "unruled", text: "", reason: ("verdict word outside fix/dismiss/ambiguity: " + $mine[0].word) }
              elif ($t.authored // "bot") == "human" and $ok[0].word != "ambiguity" then
                { threadId: $t.threadId, authored: "human", verdict: "unruled", text: $ok[0].text, reason: ("human-authored thread: " + $ok[0].word + " refused, collected for the human") }
              else
                { threadId: $t.threadId, authored: ($t.authored // "bot"), verdict: $ok[0].word, text: $ok[0].text, reason: "" }
              end ]' <<<"${threads}"
}

# av_parse_replies <threads_json> <reply> <round>
av_parse_replies() {
    local threads="${1:?av_parse_replies: threads json required}" reply="${2-}" round="${3:?av_parse_replies: round required}"
    case "${round}" in *[!0-9]*|'') echo "av_parse_replies: round must be a positive integer: ${round}" >&2; return 2 ;; esac
    jq -c --argjson lines "$(_av_lines "${reply}")" --argjson round "${round}" '
        ($lines
          | map(capture("^(?<id>[^:[:space:]]+): (?<text>.*)$"; "")
                | select(. != null)))
          as $parsed
        | [ .[]
            | . as $t
            | ($parsed | map(select(.id == $t.threadId)) | .[0]) as $line
            | if $line == null then
                { threadId: $t.threadId, action: "unaddressed", text: "" }
              elif ($line.text | test("^revise-dispute( — | -- | - )")) and (($t.ruling // null) != null) then
                { threadId: $t.threadId, action: "no-change", text: $t.ruling }
              elif ($line.text | test("^revise-dispute( — | -- | - )")) then
                { threadId: $t.threadId,
                  action: (if (($t.ruled // "") == "fix") or ($round > 1) then "cannot" else "dispute" end),
                  text: ($line.text | sub("^revise-dispute( — | -- | - )"; "")) }
              elif ($line.text | test("^no-change( — | -- | - )")) then
                { threadId: $t.threadId, action: "no-change", text: ($line.text | sub("^no-change( — | -- | - )"; "")) }
              elif ($line.text | test("^cannot( — | -- | - )")) then
                { threadId: $t.threadId, action: "cannot", text: ($line.text | sub("^cannot( — | -- | - )"; "")) }
              else
                { threadId: $t.threadId, action: "fixed", text: $line.text }
              end ]' <<<"${threads}"
}

# av_next_round <round> <cap> <ruled_fixes_pending>
av_next_round() {
    local round="${1:?av_next_round: round required}" cap="${2:?av_next_round: cap required}" pending="${3:?av_next_round: ruled fixes pending required}"
    local arg
    for arg in "${round}" "${cap}" "${pending}"; do
        case "${arg}" in *[!0-9]*|'') echo "av_next_round: integers required: ${round} ${cap} ${pending}" >&2; return 2 ;; esac
    done
    if [ "${round}" -eq 1 ] && [ "${pending}" -gt 0 ]; then
        echo 2
    elif [ "${round}" -lt "${cap}" ]; then
        echo $((round + 1))
    else
        echo cap
    fi
}
