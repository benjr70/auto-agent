#!/usr/bin/env bash
# caveman.sh: UserPromptSubmit hook that asks a Fire for terse output.
#
# Why this exists: a Fire's prose is read by nobody in real time. It lands in a
# transcript and a Fire record, and every word of it is output the account's
# budget pays for. The first Target Project carried this as a hook in its own
# settings; it moved here with the cut-over (ticket #17) so every Target
# Project's Fires get it and none has to commit it.
#
# Scoped to a Fire on purpose: the wrapper exports AUTO_AGENT_FIRE=1, and
# without it this prints nothing. The plugin is also loaded in sessions a
# human is talking to (the Setup conversation, a planning skill), and those
# keep their full sentences.
#
# What the context must never cost is a contract: the lines a skill tells the
# session to write verbatim (`picked:   #N`, `manual-verify: ...`), commit
# messages, PR bodies and code are named as exempt in the text itself.
#
# Off switch: AUTO_AGENT_CAVEMAN=off in the Host env.
#
# Reads the hook JSON on stdin and ignores it. Always exits 0: a hook that
# shapes tone must never be able to block a prompt.

set -uo pipefail

cat >/dev/null 2>&1 || true

[ "${AUTO_AGENT_FIRE:-}" = "1" ] || exit 0
[ "${AUTO_AGENT_CAVEMAN:-on}" = "off" ] && exit 0

context='Activate caveman mode (full intensity) for your own prose in this session: drop articles and filler, keep verbs terse, preserve 100% technical accuracy. Target about 75% fewer tokens. Exempt, write these in full and verbatim: every line a skill tells you to emit exactly, commit messages, PR and issue bodies and comments, and code blocks.'

if command -v jq >/dev/null 2>&1; then
    jq -cn --arg c "${context}" '{hookSpecificOutput: {hookEventName: "UserPromptSubmit", additionalContext: $c}}'
fi
exit 0
