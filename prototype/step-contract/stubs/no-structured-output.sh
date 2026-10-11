#!/usr/bin/env bash
# PROTOTYPE stub claude: a success result with no structured_output
echo '{"type":"system","subtype":"init","session_id":"stub-1","model":"stub","permissionMode":"dontAsk","slash_commands":["factory:review-correctness"]}'; echo '{"type":"result","subtype":"success","is_error":false,"result":"I found two issues...","num_turns":3}'
