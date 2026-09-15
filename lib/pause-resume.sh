#!/usr/bin/env bash
# pause-resume.sh: the Pause/Resume state logic, what to do with paused work
# before anything else. Carried over from the Smart Smoker harness; the cap now comes
# from the Harness config's `rounds.pause_resume` (ADR 0002) through the caller.
#
# Sourceable library exposing one pure function, `pause_resume_action`. Given
# whether an `AFK:paused` issue exists and how many times it has already paused,
# it emits a compact JSON verdict:
#
#     { "action": "resume|pick-new|fail", "issue": <number|null>,
#       "pauseCount": <int> }
#
#   resume    hand issue `.issue`'s existing branch back to the implementer in
#             resume (its partial work is preserved, not restarted).
#   pick-new  no paused work; proceed to the normal fresh pick.
#   fail      issue `.issue` has paused too many times; the caller applies
#             `AFK:failed` and a human takes over.
#
# The one property that matters is the cap: an issue that has paused <cap>
# times must `fail` rather than `resume`, so a genuinely-too-big issue is
# handed to a human instead of bouncing across windows forever. Below the cap
# a paused issue always resumes: in-flight work finishes before any new pick.
#
# The function is pure: it reads only its arguments. The caller discovers the
# paused issue, counts its pauses (the pause comments on the issue timeline)
# and reads the cap from the config.
#
# Usage:  pause_resume_action "<pausedIssue|empty>" "<pauseCount>" [<cap>]
#
# The cap is `rounds.pause_resume` from the Harness config, one of the five
# documented round caps (ADR 0002), so there is no env override: a missing or
# unreadable cap is the schema default, 3.

# pause_resume_action: print the next-action JSON on stdout. Always exits 0; the
# pacing loop must never crash on this decision.
pause_resume_action() {
    local paused_issue="${1:-}" pause_count="${2:-}" cap="${3:-3}"

    # An unreadable cap falls back to the schema default rather than to "never
    # fail" or "always fail".
    if ! [[ "${cap}" =~ ^[0-9]+$ ]] || [ "${cap}" -lt 1 ]; then
        cap=3
    fi

    # No paused issue: nothing in flight to finish; proceed to the normal pick.
    if [ -z "${paused_issue}" ] || ! [[ "${paused_issue}" =~ ^[0-9]+$ ]]; then
        printf '{"action":"pick-new","issue":null,"pauseCount":0}\n'
        return 0
    fi

    # A non-numeric or missing count means the count could not be read; treat
    # it as a single pause so the issue resumes rather than being failed on a
    # read glitch (fail-safe toward finishing the work).
    if ! [[ "${pause_count}" =~ ^[0-9]+$ ]] || [ "${pause_count}" -lt 1 ]; then
        pause_count=1
    fi

    if [ "${pause_count}" -ge "${cap}" ]; then
        printf '{"action":"fail","issue":%s,"pauseCount":%s}\n' \
            "${paused_issue}" "${pause_count}"
    else
        printf '{"action":"resume","issue":%s,"pauseCount":%s}\n' \
            "${paused_issue}" "${pause_count}"
    fi
    return 0
}
