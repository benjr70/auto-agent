#!/usr/bin/env bash
# Fire record writer: the JSON the Daemon writes into the State dir for every
# Fire, the Dashboard's source for history and current state (ADR 0006).
#
# Source this file, then:
#
#   fire_record_path <state-dir> <fire-id>
#       Prints <state-dir>/fires/<fire-id>.json.
#
#   fire_record_write <state-dir> <fire-id> <record-json>
#       Writes the record atomically. Returns non-zero only when the JSON is
#       invalid or the directory cannot be written.
#
#   fire_record_summarize_stream <stream-file> <plugin-name> <skill-name>
#       Prints the part of a record that comes from the logged stream-json:
#       session, model, whether the plugin was in the init event's plugin list
#       and the namespaced skill in its slash commands, the result line's
#       verdict, the last rate-limit event, and the issue a `picked:` line
#       named. Tolerates a truncated or empty stream (every field null/false).
#
#   fire_record_dry_run_ok <record-file> <ok-line>
#       True when the record proves a dry-run Fire: exit 0, the plugin in the
#       init event's plugin list, the skill in its slash commands, and <ok-line>
#       as one whole line of the result text.
#
# Record shape (the wrapper assembles it; keys are stable for the Dashboard):
#
#   {
#     "fireId", "kind": "dry-run" | "pickup", "prompt", "startedAt", "endedAt",
#     "exit": <int>, "phase": "preflight" | "claude", "dryRun": <bool>,
#     "target": "<abs path>", "model": <requested model or null>,
#     "issue": <int> | null, "log": { "stream", "stderr" },
#     "plugin": { "name", "loaded": <bool>, "skill", "skillListed": <bool> },
#     "result": { "subtype", "isError", "totalCostUsd", "numTurns", "sessionId", "model", "text" },
#     "rateLimit": { "status", "rateLimitType", "resetsAt" } | null,
#     "gate": <Gate verdict, ADR 0008>
#   }

FIRE_RECORD_DIRNAME="fires"

fire_record_path() {
    printf '%s/%s/%s.json\n' "${1:?state dir required}" "${FIRE_RECORD_DIRNAME}" "${2:?fire id required}"
}

fire_record_write() {
    local state="${1:?state dir required}" fire="${2:?fire id required}" json="${3:?record json required}"
    local path; path="$(fire_record_path "${state}" "${fire}")"
    mkdir -p "${path%/*}" || return 1
    printf '%s\n' "${json}" | jq . > "${path}.tmp" || { rm -f "${path}.tmp"; return 1; }
    mv "${path}.tmp" "${path}"
}

# fire_record_summarize_stream <stream-file> <plugin-name> <skill-name>
fire_record_summarize_stream() {
    local file="${1:?stream file required}" plugin="${2:-auto-agent}" skill="${3:-}"
    local input="${file}"
    [ -f "${file}" ] || input=/dev/null
    # `-R` + `fromjson?` so a truncated last line cannot break the summary.
    jq -R -s -c --arg plugin "${plugin}" --arg skill "${skill}" '
        [ split("\n")[] | select(length > 0) | (fromjson? // empty) ] as $ev
        | ($ev | map(select(.type == "system" and .subtype == "init")) | first) as $init
        | ($ev | map(select(.type == "result")) | last) as $res
        | ($ev | map(select(.type == "rate_limit_event")) | last) as $rl
        | ($ev | map(select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text)
              | map(capture("(?m)^[[:space:]]*picked:[[:space:]]+#(?<n>[0-9]+)") | .n | tonumber)
              | first) as $issue
        | {
            plugin: {
              name: $plugin,
              loaded: (($init.plugins // []) | any(.name == $plugin)),
              skill: (if $skill == "" then null else $skill end),
              skillListed: ($skill != "" and (($init.slash_commands // []) | index($skill)) != null)
            },
            result: {
              subtype: ($res.subtype // null),
              isError: (if ($res // {}) | has("is_error") then $res.is_error else null end),
              totalCostUsd: ($res.total_cost_usd // null),
              numTurns: ($res.num_turns // null),
              sessionId: ($res.session_id // $init.session_id // null),
              model: ($init.model // null),
              text: ($res.result // null)
            },
            rateLimit: (if $rl == null then null else {
              status: ($rl.rate_limit_info.status // null),
              rateLimitType: ($rl.rate_limit_info.rateLimitType // null),
              resetsAt: ($rl.rate_limit_info.resetsAt // null)
            } end),
            issue: ($issue // null)
          }' "${input}"
}

# fire_record_dry_run_ok <record-file> <ok-line>
fire_record_dry_run_ok() {
    local file="${1:?record file required}" line="${2:?ok line required}"
    jq -e --arg l "${line}" '
        .exit == 0 and .plugin.loaded == true and .plugin.skillListed == true
        and ((.result.text // "") | split("\n") | index($l) != null)' "${file}" >/dev/null 2>&1
}
