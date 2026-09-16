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
#       verdict, the last rate-limit event, and the unit of work the pickup
#       skill's stable lines named (see "Work" below). Tolerates a truncated or
#       empty stream (every field null/false).
#
#   fire_record_noop_ok <record-file> <ok-line>
#       True when the record proves a no-op Fire: exit 0, the plugin in the
#       init event's plugin list, the skill in its slash commands, and <ok-line>
#       as one whole line of the result text.
#
#   fire_record_dry_run_ok <record-file>
#       True when the record proves a dry-run pickup Fire: exit 0, plugin
#       loaded, skill listed, and the pickup skill ended on one of its dry-run
#       lines (`work.kind` is "none" or "dry-run") and printed the `picked:`
#       line of its report block, so it reached a verdict without a GitHub
#       write.
#
# Work: the pickup skill prints stable lines the wrapper scrapes (the same
# lines Smart-Smoker-V2's agent-run scraped), so the record can say what the
# Fire worked and the wrapper can clean a lock the Fire leaked on a crash:
#
#   picked:   #<N> <title>                        a Slice pick   -> kind "pick"
#   picked:   reconcile PR #<P> (issue #<N|null>) a reconcile    -> kind "reconcile"
#   resolve: #<N> <research|task> <slug>          a resolve Fire -> kind "resolve"
#   afk-pickup: no eligible issue | afk-pickup: skip …  nothing  -> kind "none"
#   afk-pickup: would-pick|would-resume|would-resolve|would-reconcile|would-fail …
#                                                 a dry-run      -> kind "dry-run"
#   resolve: DONE … | resolve: FAILED …           the resolve settled its own
#                                                 ticket: work.settled
#
# Record shape (the wrapper assembles it; keys are stable for the Dashboard):
#
#   {
#     "fireId", "kind": "dry-run" | "pickup" | "noop", "prompt", "startedAt", "endedAt",
#     "exit": <int>, "phase": "preflight" | "claude", "dryRun": <bool>,
#     "target": "<abs path>", "model": <requested model or null>,
#     "issue": <int> | null, "log": { "stream", "stderr" },
#     "plugin": { "name", "loaded": <bool>, "skill", "skillListed": <bool> },
#     "result": { "subtype", "isError", "totalCostUsd", "numTurns", "sessionId", "model", "text" },
#     "work": { "kind": "pick"|"reconcile"|"resolve"|"none"|"dry-run"|null,
#               "issue": <int>|null, "pr": <int>|null, "slug": <string>|null,
#               "line": "<the matched line>"|null,
#               "pickedLine": "<the picked: line of the report block>"|null,
#               "settled": "done"|"hitl"|"failed"|null },
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
        # Every line the Fire printed: the assistant text blocks, then the
        # result text (which repeats the last turn). Scraped in that order.
        | ([ ($ev | map(select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text)[]),
             ($res.result // "") ]
           | join("\n") | split("\n") | map(sub("^[[:space:]]+"; "") | sub("[[:space:]]+$"; ""))) as $lines
        | def first_match(re): ($lines | map(select(test(re))) | first);
          def cap(re): (first_match(re) | if . == null then null else capture(re) end);
          (cap("^resolve:[[:space:]]+#(?<issue>[0-9]+)[[:space:]]+(?<type>research|task)[[:space:]]+(?<slug>[A-Za-z0-9._-]+)")) as $resolve
        | (cap("^picked:[[:space:]]+reconcile PR #(?<pr>[0-9]+) \\(issue #(?<issue>[0-9]+|null)\\)")) as $reconcile
        | (cap("^picked:[[:space:]]+#(?<issue>[0-9]+)")) as $pick
        | (first_match("^afk-pickup: would-(pick|resume|resolve|reconcile|fail)")) as $would
        | (first_match("^afk-pickup: (no eligible issue|skip)")) as $none
        | (if first_match("^resolve:[[:space:]]+DONE.*relabelled HITL") != null then "hitl"
           elif first_match("^resolve:[[:space:]]+DONE") != null then "done"
           elif first_match("^resolve:[[:space:]]+FAILED") != null then "failed"
           else null end) as $settled
        | (first_match("^picked:[[:space:]]")) as $picked_line
        | (if $would != null then
             { kind: "dry-run", issue: (($would | capture("(would-(pick|resume|resolve|fail) |issue )#(?<n>[0-9]+)")? // {n: null}).n | if . == null then null else tonumber end),
               pr: (($would | capture("PR #(?<p>[0-9]+)")? // {p: null}).p | if . == null then null else tonumber end),
               slug: null, line: $would, settled: null }
           elif $resolve != null then
             { kind: "resolve", issue: ($resolve.issue | tonumber), pr: null, slug: $resolve.slug,
               line: first_match("^resolve:[[:space:]]+#[0-9]+"), settled: $settled }
           elif $reconcile != null then
             { kind: "reconcile", issue: (if $reconcile.issue == "null" then null else ($reconcile.issue | tonumber) end),
               pr: ($reconcile.pr | tonumber), slug: null, line: first_match("^picked:[[:space:]]+reconcile"), settled: null }
           elif $pick != null then
             { kind: "pick", issue: ($pick.issue | tonumber), pr: null, slug: null,
               line: first_match("^picked:[[:space:]]+#[0-9]+"), settled: null }
           elif $none != null then
             { kind: "none", issue: null, pr: null, slug: null, line: $none, settled: null }
           else
             { kind: null, issue: null, pr: null, slug: null, line: null, settled: null }
           end | . + { pickedLine: $picked_line }) as $work
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
            work: $work,
            issue: $work.issue
          }' "${input}"
}

# fire_record_noop_ok <record-file> <ok-line>
fire_record_noop_ok() {
    local file="${1:?record file required}" line="${2:?ok line required}"
    jq -e --arg l "${line}" '
        .exit == 0 and .plugin.loaded == true and .plugin.skillListed == true
        and ((.result.text // "") | split("\n") | index($l) != null)' "${file}" >/dev/null 2>&1
}

# fire_record_dry_run_ok <record-file>
fire_record_dry_run_ok() {
    local file="${1:?record file required}"
    jq -e '
        .exit == 0 and .plugin.loaded == true and .plugin.skillListed == true
        and (.work.kind == "none" or .work.kind == "dry-run")
        and (.work.pickedLine != null)' "${file}" >/dev/null 2>&1
}
