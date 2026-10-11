#!/usr/bin/env bash
# PROTOTYPE stub claude: a rejected rate-limit event, then a limit message
echo '{"type":"system","subtype":"init","session_id":"stub-1","model":"stub","permissionMode":"dontAsk","slash_commands":["factory:review-correctness"]}'; echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1791590400,"rateLimitType":"five_hour"}}'; echo '{"type":"result","subtype":"success","is_error":true,"result":"You have hit your limit, resets 3am"}'; exit 1
