#!/usr/bin/env bash
# arbiter-fixture.sh: run the five technical disputes through the Arbiter as
# a fixture (issue #77 AC 4).
#
# lib/testdata/arbiter-disputes.json holds the five bot-thread disputes that
# parked an Agent PR for a human on the first Target Project (its PRs 720,
# 721 and 723) before the Arbiter existed. AC 4 says the escalation test in
# plugin/agents/arbiter.md rules every one of them (none escalates). That is
# a claim about what the agent does with its prompt, so the fixture run is an
# agent run; this script is the two deterministic halves around it:
#
#   arbiter-fixture.sh render [FIXTURE]
#       -> stdout: the Arbiter's prompt for the five disputes, in the shape
#          /auto-agent:pr-reconcile §2 step 3b sends — per thread the Finding,
#          `threadId` (the fixture id), `path:line`, `authored: bot` and the
#          implementer's dispute line verbatim; plus the requirement each
#          thread quotes, standing in for the issue text. Pipe it to the agent:
#            claude -p --agent auto-agent:arbiter "$(bash lib/arbiter-fixture.sh render)" > reply.txt
#
#   arbiter-fixture.sh check <REPLY_FILE|-> [FIXTURE]
#       -> reads the agent's reply through av_parse_verdicts (the same reader
#          the runbook uses) and exits 0 iff every fixture dispute is ruled —
#          a `fix` or a `dismiss` — and none is `ambiguity` or unruled. Prints
#          one line per dispute: `<id>: <verdict> — <text>`. Exit 1 on any
#          escalation or malformed line, 2 on usage.
#
# lib/arbiter-fixture.test.sh drives both halves with canned replies; the
# agent run itself needs a model and is done by hand (or by a CI job with a
# credential) when the escalation test or the fixture changes.

set -uo pipefail

_af_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=arbiter-verdicts.sh
. "${_af_lib_dir}/arbiter-verdicts.sh"

AF_DEFAULT_FIXTURE="${_af_lib_dir}/testdata/arbiter-disputes.json"

af_usage() {
    echo "usage: arbiter-fixture.sh render [FIXTURE] | check <REPLY_FILE|-> [FIXTURE]" >&2
    return 2
}

# af_threads <fixture> -> the disputes as the threads JSON the Arbiter reader takes
af_threads() {
    jq -c '[ .disputes[] | { threadId: .id, authored: "bot" } ]' "$1"
}

af_render() {
    local fixture="${1:-${AF_DEFAULT_FIXTURE}}"
    [ -f "${fixture}" ] || { echo "arbiter-fixture: fixture not found: ${fixture}" >&2; return 2; }
    cat <<'EOS'
You are the Arbiter for a reconcile Fire. The implementer disputed the
bot-authored review threads below. For each, the Finding is the thread's first
comment verbatim and the Dispute is the implementer's revise-dispute line
verbatim; you have nothing else of the implementer's. Where the Finding
quotes a requirement, the issue text that requirement comes from is given as
"Requirement quoted". Apply the escalation test in your prompt and reply with
exactly one verdict line per thread, nothing before the first line:
`<threadId>: fix — <instruction>`, `<threadId>: dismiss — <reason>` or
`<threadId>: ambiguity — <decision JSON>`.

EOS
    jq -r '.disputes[] | "## Thread `\(.id)` — \(.source)\nauthored: bot\n\nFinding:\n\(.finding)\n\nRequirement quoted: \(if .requirement_quoted == "" then "(none)" else .requirement_quoted end)\n\nDispute (implementer):\n\(.id): revise-dispute — \(.dispute)\n"' "${fixture}"
}

af_check() {
    local reply_file="${1-}" fixture="${2:-${AF_DEFAULT_FIXTURE}}" reply verdicts escalated
    [ -n "${reply_file}" ] || af_usage || return $?
    [ -f "${fixture}" ] || { echo "arbiter-fixture: fixture not found: ${fixture}" >&2; return 2; }
    if [ "${reply_file}" = "-" ]; then reply="$(cat)"; else
        [ -f "${reply_file}" ] || { echo "arbiter-fixture: reply file not found: ${reply_file}" >&2; return 2; }
        reply="$(cat "${reply_file}")"
    fi
    verdicts="$(av_parse_verdicts "$(af_threads "${fixture}")" "${reply}")" || return 1
    printf '%s' "${verdicts}" | jq -r '.[] | "\(.threadId): \(.verdict) — \(if .verdict == "unruled" then .reason else .text end)"'
    escalated="$(printf '%s' "${verdicts}" | jq -r '[.[] | select(.verdict != "fix" and .verdict != "dismiss")] | length')"
    if [ "${escalated}" -gt 0 ]; then
        echo "arbiter-fixture: ${escalated} of $(printf '%s' "${verdicts}" | jq 'length') dispute(s) escalated or unruled; AC 4 says every one is ruled" >&2
        return 1
    fi
    echo "arbiter-fixture: every dispute ruled"
    return 0
}

af_main() {
    case "${1-}" in
        render) af_render "${2-}" ;;
        check) af_check "${2-}" "${3-}" ;;
        *) af_usage ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    af_main "$@"
fi
