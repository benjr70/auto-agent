#!/usr/bin/env bash
# PROTOTYPE stub claude: dies with no result message
echo '{"type":"system","subtype":"init","session_id":"stub-1","model":"stub","permissionMode":"dontAsk","slash_commands":["factory:review-correctness"]}'; echo 'segfault' >&2; exit 139
