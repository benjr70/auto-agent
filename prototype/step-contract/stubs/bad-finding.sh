#!/usr/bin/env bash
# PROTOTYPE stub claude: schema-valid, but a defect with no failure and a path that does not exist
echo '{"type":"system","subtype":"init","session_id":"stub-1","model":"stub","permissionMode":"dontAsk","slash_commands":["factory:review-correctness"]}'; echo '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"findings":[{"kind":"defect","path":"lib/nope.sh","line":3,"summary":"looks off","failure_scenario":"","quoted_requirement":""}]}}'
