#!/usr/bin/env bash
# pickup-triage.sh: one-call, read-only triage for the pickup Fire.
#
# Why this exists: each Bash call the pickup skill makes is one API turn that
# re-reads the session's whole cached context. The pre-pick decision tree used
# to be 15-20 probe turns per Fire. This script runs the whole read-only tree
# in ONE call and emits a single JSON verdict the skill branches on. Carried
# over from the Smart Smoker harness; every repo fact now comes from the Harness config
# (ADR 0002) and the machine login from the Host env (ADR 0005).
#
# READ-ONLY by design: the script never edits labels, comments, branches or
# PRs. Mutations (lock flips, label moves) stay with the skill.
#
# Sourceable library exposing `pickup_triage [<target-dir>]`; also runnable
# directly and through `bin/auto-agent pickup-triage [<target-dir>]`.
#
# Verdict JSON (one line, jq-compact):
#
#   { "verdict": "no-config|no-gh|wrong-login|in-flight|reconcile|resume|
#                 resume-cap|pick|pick-wayfinder|pick-mcp|idle",
#     "agentLogin": "<login|''>",
#     "pickShape": "project|labels|null",
#     "useMcpForProject": <bool>,       # project pick and the token lacks `project`
#     "inflight": <int>,                # open AFK:in-progress count
#     "reconcile": { "pr": N, "branch": "...", "issue": M|null, "reason": "...",
#                    "hadDone": <bool>, ... } | null,   # pr-triage's verdict + hadDone
#     "paused":    { "issue": N, "pauseCount": <int>,
#                    "action": "resume|fail" } | null,
#     "pick":      { "issue": N, "title": "...", "priority": "P0"|null,
#                    "type": "research|task" } | null }
#
# `.pick.priority` is the Project priority value (missing = the last value of
# the configured order) and null under a label-only pick. `.pick.type` is
# present only on `pick-wayfinder`.
#
# Verdict semantics (priority order, first match wins):
#   no-config   the Harness config cannot be resolved -> exit 2
#   no-gh       gh unauthenticated -> exit 3
#   wrong-login gh is logged in as someone other than DAEMON_GH_LOGIN -> exit 4:
#               the Daemon never acts as a human account (ADR 0005)
#   in-flight   the single-flight lock (AFK:in-progress) is held -> skip silently
#   reconcile   a PR needs attention (lib/pr-triage.sh verdict)
#   resume      paused issue below the pause_resume cap
#   resume-cap  paused issue AT the cap -> the skill applies AFK:failed
#   pick        eligible Slice (blockers closed, no human assignee)
#   pick-wayfinder  the eligible issue is a wayfinder Decision ticket
#               (`wayfinder:research` / `wayfinder:task`) for the resolve lane
#   pick-mcp    project pick and the token lacks `project` scope: the skill
#               runs the pick via the GitHub MCP tools instead
#   idle        nothing to do
#
# The JSON is emitted on stdout in ALL cases: branch on .verdict, not just the
# exit code (0 for every verdict from in-flight down).
#
# The pick, one `gh api graphql` call; eligibility is decided entirely inside
# the jq filter so there are no per-candidate follow-up calls:
#
#   repository(owner, name).issues(first: 100, labels: ["AFK"], states: OPEN) {
#     nodes { number title createdAt
#       labels(first: 30) { nodes { name } }
#       blockedBy(first: 50) { nodes { number state } pageInfo { hasNextPage } }
#       assignees(first: 10) { nodes { login } }
#       projectItems(first: 10) { nodes { project { number }          # project pick only
#         fieldValueByName(name: <priority_field>) { ... on
#           ProjectV2ItemFieldSingleSelectValue { name } } } } } }
#
# Eligibility rules:
#   labels     none of AFK:in-progress / AFK:done / AFK:failed / AFK:paused.
#   blockers   GitHub's NATIVE issue dependencies only: every `blockedBy` node
#              must be CLOSED. Body prose ("Blocked by #123") has no effect.
#              The 50-node page is a cap, not a promise: when hasNextPage is
#              true the unseen blockers could be open, so the candidate fails
#              SAFE (treated as blocked).
#   assignee   never race a human: the issue must carry NO assignee other than
#              the Daemon's own login. A human co-assignee disqualifies. When
#              the login is unknown (empty), only unassigned issues qualify.
#   partials   GraphQL can return `data` with null fields alongside `errors`;
#              every list is defaulted to [] so one partial node never aborts
#              the filter and silently degrades the verdict to idle.
#   order      Project pick: must be an item of the configured project; the
#              configured priority field ranks by the configured order (a
#              missing or unknown value ranks last), then oldest createdAt.
#              Label-only pick: oldest createdAt.
#   wayfinder  `wayfinder:research` / `wayfinder:task` make the winner a
#              `pick-wayfinder` with that `.pick.type`. Any other
#              `wayfinder:*` type is HITL by definition and reached the AFK
#              queue by mislabelling: skipped with a stderr note, next
#              candidate considered. More than one `wayfinder:*` label is
#              equally mislabelled and skipped, so the verdict never depends
#              on GraphQL's label order.
#
# The reconcile step is lib/pr-triage.sh's seam: sourced when the file exists
# beside this one, its `pr_triage_scan` (with the same HARNESS_CONFIG_JSON
# exported) gives the reconcile verdict, and its
# `pr_triage_bot_verdict_unworkable` says whether a Bot PR verdict may be acted
# on (the deps-land lane is on only when the Harness config declares it).
# Without the lib the step reads as "no PR needs attention" and the verdict
# falls through to resume/pick.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        <target-dir>, or AUTO_AGENT_TARGET_DIR): repo slug,
#                        pick block, rounds.pause_resume
#   DAEMON_GH_LOGIN      the machine login from the Host env; when set, the gh
#                        login must match it
#   GH_BIN               gh CLI (default: gh), injected for tests

_pickup_triage_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_pickup_triage_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_pickup_triage_lib_dir}/host-env.sh"
# shellcheck source=pause-resume.sh
. "${_pickup_triage_lib_dir}/pause-resume.sh"
# shellcheck source=single-flight-lock.sh
. "${_pickup_triage_lib_dir}/single-flight-lock.sh"
if [ -f "${_pickup_triage_lib_dir}/pr-triage.sh" ]; then
    # shellcheck source=/dev/null
    . "${_pickup_triage_lib_dir}/pr-triage.sh"
fi

# _pt_emit <verdict> <inflight> <reconcileJson> <pausedJson> <pickJson>
# Reads the Fire's identity from the caller's scope (bash dynamic scoping):
# login shape use_mcp, which every verdict carries.
_pt_emit() {
    jq -cn \
        --arg verdict "$1" \
        --arg login "${login}" \
        --arg shape "${shape}" \
        --argjson useMcp "${use_mcp}" \
        --argjson inflight "$2" \
        --argjson reconcile "$3" \
        --argjson paused "$4" \
        --argjson pick "$5" \
        '{verdict: $verdict, agentLogin: $login,
          pickShape: (if $shape == "" then null else $shape end),
          useMcpForProject: $useMcp, inflight: $inflight,
          reconcile: $reconcile, paused: $paused, pick: $pick}'
}

# _pt_int <value> <fallback>: a non-negative integer or the fallback, so no
# gh output can ever reach --argjson unchecked (the verdict is JSON in ALL cases).
_pt_int() {
    case "$1" in
        ''|*[!0-9]*) printf '%s' "$2" ;;
        *) printf '%s' "$1" ;;
    esac
}

# _pt_reconcile <login> -> pr-triage's verdict JSON, or '' when nothing needs
# attention, the verdict is unreadable, or the lib is not present. Owns the
# seam described in the header.
_pt_reconcile() {
    local login="$1" verdict=''
    declare -F pr_triage_scan >/dev/null 2>&1 || return 0
    verdict="$(PR_TRIAGE_AUTHOR="${login}" pr_triage_scan)" || verdict=''
    [ -n "${verdict}" ] || return 0
    if ! printf '%s' "${verdict}" | jq -e '.pr | type == "number"' >/dev/null 2>&1; then
        return 0
    fi
    if declare -F pr_triage_bot_verdict_unworkable >/dev/null 2>&1 \
        && pr_triage_bot_verdict_unworkable "${verdict}"; then
        echo "pickup-triage: PR #$(printf '%s' "${verdict}" | jq -r '.pr') suppressed by" \
            "pr_triage_bot_verdict_unworkable, falling through" >&2
        return 0
    fi
    printf '%s' "${verdict}"
}

# _pt_pick_query <owner> <name> <shape> <priority_field>
# Every config value is JSON-encoded before it is spliced into the query, so a
# quote in a field name breaks nothing.
_pt_pick_query() {
    local owner name field shape="$3" project_fragment=''
    owner="$(jq -n --arg s "$1" '$s')"
    name="$(jq -n --arg s "$2" '$s')"
    field="$(jq -n --arg s "$4" '$s')"
    if [ "${shape}" = "project" ]; then
        project_fragment='
        projectItems(first: 10) {
          nodes {
            project { number }
            fieldValueByName(name: '"${field}"') {
              ... on ProjectV2ItemFieldSingleSelectValue { name }
            }
          }
        }'
    fi
    printf '%s' '
query {
  repository(owner: '"${owner}"', name: '"${name}"') {
    issues(first: 100, labels: ["'"${HARNESS_LABEL_AFK}"'"], states: OPEN) {
      nodes {
        number
        title
        createdAt
        labels(first: 30) { nodes { name } }
        blockedBy(first: 50) { nodes { number state } pageInfo { hasNextPage } }
        assignees(first: 10) { nodes { login } }'"${project_fragment}"'
      }
    }
  }
}'
}

# _pt_pick_filter: the jq program that turns the GraphQL response into the
# ordered candidate rows (one base64 JSON per line). $pick is the resolved
# pick block, $login the Daemon's login.
_pt_pick_filter() {
    cat <<'JQ'
def rank($p): ($pick.project.order | index($p)) // ($pick.project.order | length);
[(.data.repository.issues.nodes // [])[]
  | . as $i
  | (($i.labels.nodes // []) | map(.name)) as $lbls
  | select((($lbls - $state) | length) == ($lbls | length))
  | select([($i.blockedBy.nodes // [])[] | select(.state != "CLOSED")] | length == 0)
  | select(($i.blockedBy.pageInfo.hasNextPage // false) | not)
  | (($i.assignees.nodes // []) | map(.login)) as $asgn
  | select((($asgn - [$login]) | length) == 0)
  | (if $pick.shape == "project" then
       ((($i.projectItems.nodes // []) | map(select(.project.number == $pick.project.number)) | first) as $pi
        | select($pi != null)
        | { priority: ($pi.fieldValueByName.name // ($pick.project.order | last)),
            prio_rank: rank($pi.fieldValueByName.name // ($pick.project.order | last)) })
     else { priority: null, prio_rank: 0 } end) as $p
  | (($lbls | map(select(startswith($wf_prefix))) | sort | join(",")) // "") as $wf
  | {number, title, createdAt, priority: $p.priority, prio_rank: $p.prio_rank, wf: $wf}]
| sort_by([.prio_rank, .createdAt])
| .[] | @base64
JQ
}

# pickup_triage [<target-dir>]
pickup_triage() {
    local gh="${GH_BIN:-gh}"
    local login='' use_mcp=false inflight=0 shape=''

    host_env_load
    local cfg
    if ! cfg="$(harness_config_resolve "${1:-}")"; then
        _pt_emit no-config 0 null null null
        return 2
    fi
    export HARNESS_CONFIG_JSON="${cfg}"
    local owner name slug pick cap
    owner="$(printf '%s' "${cfg}" | jq -r '.repo.owner')"
    name="$(printf '%s' "${cfg}" | jq -r '.repo.name')"
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
    pick="$(printf '%s' "${cfg}" | jq -c '.pick')"
    shape="$(printf '%s' "${pick}" | jq -r '.shape')"
    cap="$(printf '%s' "${cfg}" | jq -r '.rounds.pause_resume // 3')"

    # gh auth. Unauthenticated: the skill's MCP fallback owns the Fire.
    if ! "${gh}" auth status >/dev/null 2>&1; then
        use_mcp=true
        _pt_emit no-gh 0 null null null
        return 3
    fi
    if [ "${shape}" = "project" ] && ! "${gh}" auth status 2>&1 | grep -q "'project'"; then
        use_mcp=true
    fi
    login="$("${gh}" api user -q .login 2>/dev/null || echo '')"

    # The machine-login check: when the Host env names the machine user, the
    # token must be that user's. A human's token never drives a Fire.
    if [ -n "${DAEMON_GH_LOGIN:-}" ] && [ "${login}" != "${DAEMON_GH_LOGIN}" ]; then
        echo "pickup-triage: gh is logged in as '${login}', the Host env expects '${DAEMON_GH_LOGIN}'" >&2
        _pt_emit wrong-login 0 null null null
        return 4
    fi

    # The single-flight lock (fails safe toward locked).
    inflight="$(single_flight_inflight "${slug}")"
    if [ "${inflight}" -gt 0 ]; then
        _pt_emit in-flight "${inflight}" null null null
        return 0
    fi

    # A PR needing attention (the pr-triage seam).
    local recon_json reconcile=null
    recon_json="$(_pt_reconcile "${login}")"
    if [ -n "${recon_json}" ]; then
        local recon_n had_done='false'
        # The ticket number can be null (a research branch that carries none):
        # then there is no issue to read a label off, and the caller skips the
        # issue lock entirely.
        recon_n="$(printf '%s' "${recon_json}" | jq -r '.issue')"
        if [ -n "${recon_n}" ] && [ "${recon_n}" != "null" ]; then
            had_done="$("${gh}" issue view "${recon_n}" --repo "${slug}" --json labels \
                --jq "[.labels[].name] | index(\"${HARNESS_LABEL_DONE}\") != null" 2>/dev/null || echo 'false')"
            [ "${had_done}" = "true" ] || had_done='false'
        fi
        reconcile="$(printf '%s' "${recon_json}" | jq -c --argjson hd "${had_done}" '. + {hadDone: $hd}')"
        _pt_emit reconcile 0 "${reconcile}" null null
        return 0
    fi

    # Paused work resumes before any new pick. The cap decision belongs to
    # pause-resume.sh; we gather its inputs and the config's cap.
    local paused_n pause_count action_json action paused=null
    paused_n="$(_pt_int "$("${gh}" issue list --repo "${slug}" --label "${HARNESS_LABEL_PAUSED}" --state open \
        --json number --jq '.[0].number // empty' 2>/dev/null || echo '')" '')"
    if [ -n "${paused_n}" ]; then
        pause_count="$(_pt_int "$("${gh}" issue view "${paused_n}" --repo "${slug}" --json comments \
            --jq '[.comments[] | select(.body | test("Run paused at .*usage exhausted"))] | length' \
            2>/dev/null || echo 0)" 0)"
        action_json="$(pause_resume_action "${paused_n}" "${pause_count}" "${cap}")"
        action="$(printf '%s' "${action_json}" | jq -r '.action')"
        # The count as pause-resume read it (an unreadable count is one pause).
        paused="$(printf '%s' "${action_json}" | jq -c --argjson n "${paused_n}" \
            '{issue: $n, pauseCount: .pauseCount, action: .action}')"
        if [ "${action}" = "resume" ]; then
            _pt_emit resume 0 null "${paused}" null
            return 0
        fi
        if [ "${action}" = "fail" ]; then
            _pt_emit resume-cap 0 null "${paused}" null
            return 0
        fi
        # pick-new falls through with the paused context attached.
    fi

    # Fresh pick. The Project priority field needs the `project` scope; without
    # it the skill runs the pick via the GitHub MCP tools instead.
    if [ "${use_mcp}" = "true" ]; then
        _pt_emit pick-mcp 0 null "${paused}" null
        return 0
    fi

    local field rows
    field="$(printf '%s' "${pick}" | jq -r '.project.priority_field // "Priority"')"
    rows="$("${gh}" api graphql -f query="$(_pt_pick_query "${owner}" "${name}" "${shape}" "${field}")" 2>/dev/null \
        | jq -r --argjson pick "${pick}" --arg login "${login}" \
            --argjson state "${HARNESS_LABELS_STATE_JSON}" \
            --arg wf_prefix "${HARNESS_LABEL_WAYFINDER_PREFIX}" \
            "$(_pt_pick_filter)" 2>/dev/null)" || rows=''

    # Eligibility is decided inside the jq filter, so the first surviving row
    # IS the pick unless it is a mislabelled wayfinder ticket.
    local row cand_json cand_n cand_title cand_prio cand_wf cand_type
    for row in ${rows}; do
        cand_json="$(printf '%s' "${row}" | base64 -d 2>/dev/null)" || continue
        cand_n="$(printf '%s' "${cand_json}" | jq -r '.number')"
        cand_title="$(printf '%s' "${cand_json}" | jq -r '.title')"
        cand_prio="$(printf '%s' "${cand_json}" | jq -c '.priority')"
        cand_wf="$(printf '%s' "${cand_json}" | jq -r '.wf // ""')"

        case "${cand_wf}" in
            '')                  cand_type='' ;;
            wayfinder:research)  cand_type='research' ;;
            wayfinder:task)      cand_type='task' ;;
            *,*)
                echo "pickup-triage: skipping #${cand_n}: ambiguous wayfinder labels (${cand_wf})" >&2
                continue
                ;;
            *)
                echo "pickup-triage: skipping #${cand_n}: ${cand_wf} is not an AFK ticket type" >&2
                continue
                ;;
        esac

        if [ -n "${cand_type}" ]; then
            _pt_emit pick-wayfinder 0 null "${paused}" \
                "$(jq -cn --argjson n "${cand_n}" --arg t "${cand_title}" \
                    --argjson p "${cand_prio}" --arg ty "${cand_type}" \
                    '{issue: $n, title: $t, priority: $p, type: $ty}')"
            return 0
        fi

        _pt_emit pick 0 null "${paused}" \
            "$(jq -cn --argjson n "${cand_n}" --arg t "${cand_title}" --argjson p "${cand_prio}" \
                '{issue: $n, title: $t, priority: $p}')"
        return 0
    done

    _pt_emit idle 0 null "${paused}" null
    return 0
}

# Runnable directly: print the verdict and propagate the return code.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    pickup_triage "$@"
    exit $?
fi
