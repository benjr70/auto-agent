#!/usr/bin/env bash
# Tests for lib/pickup-triage.sh
#
# Run: bash lib/pickup-triage.test.sh
#
# Strategy: pickup_triage's gh calls go through an injected GH_BIN stub serving
# canned per-query fixtures from a temp workspace; the Harness config arrives
# resolved through HARNESS_CONFIG_JSON (one test goes through the loader with a
# target dir instead). Each test builds the GitHub-side state and asserts the
# single JSON verdict the pickup skill branches on, never the internals. The
# project-pick cases are Smart-Smoker-V2's own canned GraphQL, so AC 1 is "the
# same fixtures, the same verdicts".

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"
FIXTURE="${ROOT_DIR}/plugin/fixtures/target-project"
# shellcheck source=pickup-triage.sh
. "${SCRIPT_DIR}/pickup-triage.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# The two resolved pick blocks, as harness_config_load prints them. The
# deps-land lane is on, so a Bot PR verdict from the pr-triage seam is acted on.
PROJECT_CFG='{"repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"main"},
  "pick":{"shape":"project","project":{"number":1,"priority_field":"Priority","order":["P0","P1","P2"]},"labels":null},
  "rounds":{"pr_watch":10,"manual_verify":3,"revise":3,"deps_fix":3,"pause_resume":3},
  "lanes":{"deps_land":{"present":true,"enabled":true},"deployed":{"present":false,"enabled":false}}}'
LABELS_CFG='{"repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"main"},
  "pick":{"shape":"labels","project":null,"labels":{}},
  "rounds":{"pr_watch":10,"manual_verify":3,"revise":3,"deps_fix":3,"pause_resume":3},
  "lanes":{"deps_land":{"present":true,"enabled":true},"deployed":{"present":false,"enabled":false}}}'

# Build a workspace: gh stub + default "authed, scoped, nothing happening"
# fixtures. Echoes the dir. Every call is logged so tests can assert the
# --repo the stub was asked for.
make_env() {
    local dir; dir="$(mktemp -d)"
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
args="\$*"
printf '%s\n' "\${args}" >> "${dir}/calls.log"
case "\${args}" in
    "auth status")               cat "${dir}/auth.out"; exit \$(cat "${dir}/auth.code") ;;
    *"api user"*)                cat "${dir}/login.out" ;;
    *"--label AFK:in-progress"*) cat "${dir}/locked.out" ;;
    *"--label AFK:paused"*)      cat "${dir}/paused.out" ;;
    *"--json labels"*)           cat "${dir}/haddone.out" ;;
    *"--json comments"*)         cat "${dir}/pausecomments.out" ;;
    *"api graphql"*)             printf '%s\n' "\${args}" > "${dir}/graphql.query"; cat "${dir}/graphql.out" ;;
    "pr list"*"--state merged"*) cat "${dir}/merged.out" ;;
    *"repo view"*)               echo trunk ;;
    *) echo "gh-stub: unmatched: \${args}" >&2; exit 1 ;;
esac
STUB
    chmod +x "${dir}/gh-stub"
    echo "Token scopes: 'project', 'repo'" > "${dir}/auth.out"
    echo 0 > "${dir}/auth.code"
    echo 'agent-bot' > "${dir}/login.out"
    echo 0 > "${dir}/locked.out"
    echo '' > "${dir}/paused.out"
    echo 'false' > "${dir}/haddone.out"
    echo 0 > "${dir}/pausecomments.out"
    echo '[]' > "${dir}/merged.out"
    : > "${dir}/calls.log"
    graphql_fixture "${dir}"   # no candidates by default
    echo "${dir}"
}

# graphql_fixture <dir> [issueJson ...]: wrap issue nodes in the query shape.
graphql_fixture() {
    local dir="$1"; shift
    local nodes='' sep='' n
    for n in "$@"; do nodes="${nodes}${sep}${n}"; sep=','; done
    printf '{"data":{"repository":{"issues":{"nodes":[%s]}}}}\n' "${nodes}" > "${dir}/graphql.out"
}

# issue_node <number> <title> <priority|null> <inProject:true/false> <createdAt>
#           [labels-csv] [blockedBy-nodes-json|null] [assignee-logins-csv|null]
#           [blockedByHasNextPage:true/false]
# The literal string `null` for blockedBy/assignees emits `{"nodes": null}`,
# the partial GraphQL response shape. `body` is never emitted: the pick query
# does not select it.
issue_node() {
    local number="$1" title="$2" prio="$3" in_project="$4" created="$5" labels_csv="${6:-AFK}"
    local blocked_by="${7:-[]}" assignees_csv="${8:-}" has_next="${9:-false}"
    local labels prio_json project_items assignees
    labels="$(printf '%s' "${labels_csv}" | jq -R 'split(",") | map({name: .})')"
    if [ "${assignees_csv}" = "null" ]; then assignees='null'
    elif [ -n "${assignees_csv}" ]; then assignees="$(printf '%s' "${assignees_csv}" | jq -R 'split(",") | map({login: .})')"
    else assignees='[]'; fi
    if [ "${prio}" = "null" ]; then prio_json='null'; else prio_json="{\"name\": \"${prio}\"}"; fi
    if [ "${in_project}" = "true" ]; then
        project_items="[{\"project\": {\"number\": 1}, \"fieldValueByName\": ${prio_json}}]"
    else
        project_items="[{\"project\": {\"number\": 9}, \"fieldValueByName\": ${prio_json}}]"
    fi
    jq -cn --argjson n "${number}" --arg t "${title}" --arg c "${created}" \
        --argjson l "${labels}" --argjson pi "${project_items}" \
        --argjson bb "${blocked_by}" --argjson as "${assignees}" --argjson hn "${has_next}" \
        '{number: $n, title: $t, createdAt: $c,
          labels: {nodes: $l}, projectItems: {nodes: $pi},
          blockedBy: {nodes: $bb, pageInfo: {hasNextPage: $hn}},
          assignees: {nodes: $as}}'
}

# label_node <number> <title> <createdAt> [labels-csv] [blockedBy-json] [assignees-csv]
# The label-only shape: what the query returns when projectItems is not selected.
label_node() {
    issue_node "$1" "$2" null true "$3" "${4:-AFK}" "${5:-[]}" "${6:-}" | jq -c 'del(.projectItems)'
}

# run_triage <dir> <cfg-json>: echoes the verdict JSON, returns pickup_triage's code
run_triage() {
    local dir="$1" cfg="$2"
    GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON="${cfg}" AUTO_AGENT_HOST_ENV="${dir}/no-host-env" \
        DAEMON_GH_LOGIN= pickup_triage
}

verdict() { printf '%s' "$1" | jq -r '.verdict'; }
field()   { printf '%s' "$1" | jq -r "$2"; }

#-------------------------------------------------------------------------------
echo "pickup-triage.sh tests:"

echo "TEST: verdict order and exit codes"
dir="$(make_env)"
out="$(GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= AUTO_AGENT_HOST_ENV="${dir}/none" pickup_triage 2>/dev/null)"; code=$?
if [ "${code}" -eq 2 ] && [ "$(verdict "${out}")" = "no-config" ] && [ ! -s "${dir}/calls.log" ]; then
    pass "no config anywhere: verdict no-config, exit 2, no gh call"
else fail "no config anywhere: verdict no-config, exit 2, no gh call" "code=${code} out=${out}"; fi

echo 1 > "${dir}/auth.code"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"; code=$?
if [ "${code}" -eq 3 ] && [ "$(verdict "${out}")" = "no-gh" ] && [ "$(field "${out}" .useMcpForProject)" = "true" ]; then
    pass "gh unauthenticated: verdict no-gh, exit 3"
else fail "gh unauthenticated: verdict no-gh, exit 3" "code=${code} out=${out}"; fi
rm -rf "${dir}"

dir="$(make_env)"
out="$(GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON="${LABELS_CFG}" AUTO_AGENT_HOST_ENV="${dir}/none" DAEMON_GH_LOGIN=smoker-bot pickup_triage 2>"${dir}/err")"; code=$?
if [ "${code}" -eq 4 ] && [ "$(verdict "${out}")" = "wrong-login" ] && [ "$(field "${out}" .agentLogin)" = "agent-bot" ] \
   && grep -q "smoker-bot" "${dir}/err" && ! grep -q 'in-progress' "${dir}/calls.log"; then
    pass "gh logged in as someone other than DAEMON_GH_LOGIN: verdict wrong-login, exit 4, before the lock read"
else fail "gh logged in as someone other than DAEMON_GH_LOGIN: verdict wrong-login, exit 4, before the lock read" "code=${code} out=${out}"; fi
out="$(GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON="${LABELS_CFG}" AUTO_AGENT_HOST_ENV="${dir}/none" DAEMON_GH_LOGIN=agent-bot pickup_triage)"; code=$?
if [ "${code}" -eq 0 ] && [ "$(verdict "${out}")" = "idle" ]; then pass "gh logged in as DAEMON_GH_LOGIN: proceeds"
else fail "gh logged in as DAEMON_GH_LOGIN: proceeds" "code=${code} out=${out}"; fi
printf 'DAEMON_GH_LOGIN=host-bot\n' > "${dir}/host.env"
out="$(unset DAEMON_GH_LOGIN; GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON="${LABELS_CFG}" AUTO_AGENT_HOST_ENV="${dir}/host.env" pickup_triage 2>/dev/null)"; code=$?
if [ "${code}" -eq 4 ] && [ "$(verdict "${out}")" = "wrong-login" ]; then pass "the login is read from the Host env file"
else fail "the login is read from the Host env file" "code=${code} out=${out}"; fi
rm -rf "${dir}"

echo "TEST: the single-flight lock (behaviour 3)"
for cfg in "${PROJECT_CFG}" "${LABELS_CFG}"; do
    dir="$(make_env)"
    echo 2 > "${dir}/locked.out"
    out="$(run_triage "${dir}" "${cfg}")"; code=$?
    shape="$(printf '%s' "${cfg}" | jq -r .pick.shape)"
    if [ "${code}" -eq 0 ] && [ "$(verdict "${out}")" = "in-flight" ] && [ "$(field "${out}" .inflight)" = "2" ] \
       && [ "$(field "${out}" .pickShape)" = "${shape}" ] && ! grep -q graphql "${dir}/calls.log"; then
        pass "${shape} pick: AFK:in-progress held: verdict in-flight, no pick query"
    else fail "${shape} pick: AFK:in-progress held: verdict in-flight, no pick query" "code=${code} out=${out}"; fi
    rm -rf "${dir}"
done
dir="$(make_env)"
echo 'ERR' > "${dir}/locked.out"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "in-flight" ]; then pass "unreadable lock: fails safe to in-flight"
else fail "unreadable lock: fails safe to in-flight" "out=${out}"; fi
rm -rf "${dir}"

echo "TEST: every gh read names the configured repo (AC 3)"
dir="$(make_env)"
echo 77 > "${dir}/paused.out"; echo 1 > "${dir}/pausecomments.out"
run_triage "${dir}" "${PROJECT_CFG}" >/dev/null
# Counted over the issue reads alone: the pr-triage seam's own `pr list` read
# (asserted by lib/pr-triage.test.sh) also carries the slug.
if [ "$(grep -E 'issue (list|view)' "${dir}/calls.log" | grep -c -- '--repo acme/widgets')" -eq 3 ] \
   && ! grep -E 'issue (list|view)' "${dir}/calls.log" | grep -v -- '--repo acme/widgets' | grep -q .; then
    pass "issue list/view calls carry --repo acme/widgets"
else fail "issue list/view calls carry --repo acme/widgets" "$(cat "${dir}/calls.log")"; fi
rm -rf "${dir}"
dir="$(make_env)"
run_triage "${dir}" "${PROJECT_CFG}" >/dev/null
if grep -q 'repository(owner: "acme", name: "widgets")' "${dir}/graphql.query" \
   && grep -q 'fieldValueByName(name: "Priority")' "${dir}/graphql.query"; then
    pass "the pick query names the configured owner, name and priority field"
else fail "the pick query names the configured owner, name and priority field" "$(cat "${dir}/graphql.query")"; fi
rm -rf "${dir}"
if grep -qE 'benjr70|Smart-Smoker' "${SCRIPT_DIR}/pickup-triage.sh" "${SCRIPT_DIR}/work-probe.sh" "${SCRIPT_DIR}/pause-resume.sh"; then
    fail "no benjr70 / Smart-Smoker-V2 literal in the pick libs"
else pass "no benjr70 / Smart-Smoker-V2 literal in the pick libs"; fi

echo "TEST: reconcile through the pr-triage seam"
dir="$(make_env)"
echo 'true' > "${dir}/haddone.out"
pr_triage_scan() { printf '%s' '{"pr":501,"branch":"feat/issue-441","issue":441,"reason":"revise"}'; }
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "reconcile" ] && [ "$(field "${out}" .reconcile.pr)" = "501" ] \
   && [ "$(field "${out}" .reconcile.issue)" = "441" ] && [ "$(field "${out}" .reconcile.hadDone)" = "true" ] \
   && grep -q 'issue view 441 --repo acme/widgets' "${dir}/calls.log"; then
    pass "pr_triage_scan verdict: reconcile with hadDone merged in, read from the configured repo"
else fail "pr_triage_scan verdict: reconcile with hadDone merged in, read from the configured repo" "out=${out}"; fi
pr_triage_scan() { printf '%s' '{"pr":635,"branch":"dependabot/npm/axios","issue":null,"reason":"dependabot","sha":"s"}'; }
: > "${dir}/calls.log"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "reconcile" ] && [ "$(field "${out}" .reconcile.issue)" = "null" ] \
   && [ "$(field "${out}" .reconcile.hadDone)" = "false" ] && [ "$(field "${out}" .reconcile.sha)" = "s" ] \
   && ! grep -q 'issue view' "${dir}/calls.log"; then
    pass "a verdict with no issue: reconcile, hadDone false, no issue view"
else fail "a verdict with no issue: reconcile, hadDone false, no issue view" "out=${out}"; fi
pr_triage_scan() { printf '%s' '{"pr":null}'; }
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "idle" ]; then pass "a no-pick verdict falls through"
else fail "a no-pick verdict falls through" "out=${out}"; fi
pr_triage_scan() { printf '%s' '{"pr":9,"issue":null,"reason":"dependabot"}'; }
pr_triage_bot_verdict_unworkable() { return 0; }
out="$(run_triage "${dir}" "${PROJECT_CFG}" 2>"${dir}/err")"
if [ "$(verdict "${out}")" = "idle" ] && grep -q 'suppressed' "${dir}/err"; then pass "the suppression predicate is honoured"
else fail "the suppression predicate is honoured" "out=${out}"; fi
unset -f pr_triage_scan pr_triage_bot_verdict_unworkable
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "idle" ] && [ "$(field "${out}" .reconcile)" = "null" ]; then
    pass "without lib/pr-triage.sh the reconcile step reads as nothing to do"
else fail "without lib/pr-triage.sh the reconcile step reads as nothing to do" "out=${out}"; fi
rm -rf "${dir}"

echo "TEST: paused work and the config's pause_resume cap"
dir="$(make_env)"
echo 77 > "${dir}/paused.out"; echo 1 > "${dir}/pausecomments.out"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "resume" ] && [ "$(field "${out}" .paused.issue)" = "77" ] && [ "$(field "${out}" .paused.pauseCount)" = "1" ]; then
    pass "paused below the cap: verdict resume"
else fail "paused below the cap: verdict resume" "out=${out}"; fi
echo 3 > "${dir}/pausecomments.out"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "resume-cap" ] && [ "$(field "${out}" .paused.action)" = "fail" ]; then
    pass "paused at the cap: verdict resume-cap"
else fail "paused at the cap: verdict resume-cap" "out=${out}"; fi
out="$(run_triage "${dir}" "$(printf '%s' "${PROJECT_CFG}" | jq -c '.rounds.pause_resume = 5')")"
if [ "$(verdict "${out}")" = "resume" ]; then pass "rounds.pause_resume from the config raises the cap"
else fail "rounds.pause_resume from the config raises the cap" "out=${out}"; fi
rm -rf "${dir}"

echo "TEST: project pick order and blocking (behaviour 1, AC 1)"
dir="$(make_env)"
# #10 is P0 but has an OPEN native blocker (#99); #20 is P1: the pick. #30 is P0 but not in project 1.
graphql_fixture "${dir}" \
    "$(issue_node 10 'blocked-p0' P0 true '2026-01-01T00:00:00Z' AFK '[{"number": 99, "state": "OPEN"}]')" \
    "$(issue_node 20 'clean-p1' P1 true '2026-02-01T00:00:00Z')" \
    "$(issue_node 30 'orphan-p0' P0 false '2026-01-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "pick" ] && [ "$(field "${out}" .pick.issue)" = "20" ] && [ "$(field "${out}" .pick.priority)" = "P1" ]; then
    pass "open native blocker skips P0; off-project skipped; P1 picked"
else fail "open native blocker skips P0; off-project skipped; P1 picked" "out=${out}"; fi
graphql_fixture "${dir}" \
    "$(issue_node 10 'unblocked-p0' P0 true '2026-01-01T00:00:00Z' AFK '[{"number": 98, "state": "CLOSED"}, {"number": 99, "state": "CLOSED"}]')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "pick" ] && [ "$(field "${out}" .pick.issue)" = "10" ]; then pass "all native blockers CLOSED: candidate picked"
else fail "all native blockers CLOSED: candidate picked" "out=${out}"; fi
graphql_fixture "${dir}" "$(issue_node 10 'prose-blocker-only' P0 true '2026-01-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.issue)" = "10" ] && ! grep -q 'body' "${dir}/graphql.query" && ! grep -q 'issue view' "${dir}/calls.log"; then
    pass "the pick reads no body: the query omits it and no issue-view follow-up"
else fail "the pick reads no body: the query omits it and no issue-view follow-up" "out=${out}"; fi
graphql_fixture "${dir}" \
    "$(issue_node 40 'no-prio' null true '2025-01-01T00:00:00Z')" \
    "$(issue_node 50 'p1-newer' P1 true '2026-03-01T00:00:00Z')" \
    "$(issue_node 60 'p1-older' P1 true '2026-02-01T00:00:00Z')" \
    "$(issue_node 70 'p2' P2 true '2026-01-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.issue)" = "60" ] && [ "$(field "${out}" .pick.priority)" = "P1" ]; then pass "P0>P1>P2 (missing = P2) then oldest wins"
else fail "P0>P1>P2 (missing = P2) then oldest wins" "out=${out}"; fi
graphql_fixture "${dir}" "$(issue_node 40 'no-prio' null true '2025-01-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.priority)" = "P2" ]; then pass "a missing priority reports the last value of the order"
else fail "a missing priority reports the last value of the order" "out=${out}"; fi
graphql_fixture "${dir}" \
    "$(issue_node 40 'urgent' Urgent true '2026-03-01T00:00:00Z')" \
    "$(issue_node 50 'later' Later true '2026-01-01T00:00:00Z')" \
    "$(issue_node 60 'unknown' Whatever true '2025-01-01T00:00:00Z')"
out="$(run_triage "${dir}" "$(printf '%s' "${PROJECT_CFG}" | jq -c '.pick.project.order = ["Urgent","Later"]')")"
if [ "$(field "${out}" .pick.issue)" = "40" ] && [ "$(field "${out}" .pick.priority)" = "Urgent" ]; then
    pass "the configured order ranks; a value outside it ranks last"
else fail "the configured order ranks; a value outside it ranks last" "out=${out}"; fi
run_triage "${dir}" "$(printf '%s' "${PROJECT_CFG}" | jq -c '.pick.project.priority_field = "Urgency"')" >/dev/null
if grep -q 'fieldValueByName(name: "Urgency")' "${dir}/graphql.query"; then pass "the configured priority field is queried"
else fail "the configured priority field is queried" "$(cat "${dir}/graphql.query")"; fi
graphql_fixture "${dir}" "$(issue_node 30 'in-project-9' P0 false '2026-01-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "idle" ]; then pass "an issue outside the configured project is invisible"
else fail "an issue outside the configured project is invisible" "out=${out}"; fi
out="$(run_triage "${dir}" "$(printf '%s' "${PROJECT_CFG}" | jq -c '.pick.project.number = 9')")"
if [ "$(field "${out}" .pick.issue)" = "30" ]; then pass "project membership is the configured project number"
else fail "project membership is the configured project number" "out=${out}"; fi
rm -rf "${dir}"

echo "TEST: assignees and partial responses (AC 1)"
dir="$(make_env)"
graphql_fixture "${dir}" \
    "$(issue_node 10 'human-claimed' P0 true '2026-01-01T00:00:00Z' AFK '[]' 'a-human')" \
    "$(issue_node 20 'daemon-claimed' P0 true '2026-02-01T00:00:00Z' AFK '[]' 'agent-bot')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.issue)" = "20" ]; then pass "foreign assignee skipped; daemon-assigned issue eligible"
else fail "foreign assignee skipped; daemon-assigned issue eligible" "out=${out}"; fi
graphql_fixture "${dir}" "$(issue_node 10 'human-claimed' P0 true '2026-01-01T00:00:00Z' AFK '[]' 'a-human,other')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "idle" ]; then pass "only human-claimed candidates: verdict idle"
else fail "only human-claimed candidates: verdict idle" "out=${out}"; fi
printf '' > "${dir}/login.out"
graphql_fixture "${dir}" \
    "$(issue_node 10 'assigned' P0 true '2026-01-01T00:00:00Z' AFK '[]' 'agent-bot')" \
    "$(issue_node 20 'unassigned' P1 true '2026-02-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.issue)" = "20" ]; then pass "empty daemon login: assigned issues skipped, unassigned picked"
else fail "empty daemon login: assigned issues skipped, unassigned picked" "out=${out}"; fi
echo 'agent-bot' > "${dir}/login.out"
graphql_fixture "${dir}" \
    "$(issue_node 10 'human-and-daemon' P0 true '2026-01-01T00:00:00Z' AFK '[]' 'a-human,agent-bot')" \
    "$(issue_node 20 'unassigned' P1 true '2026-02-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.issue)" = "20" ]; then pass "human + daemon co-assignees: skipped"
else fail "human + daemon co-assignees: skipped" "out=${out}"; fi
graphql_fixture "${dir}" \
    "$(issue_node 10 'partial-node' P1 true '2026-01-01T00:00:00Z' AFK 'null' 'null')" \
    "$(issue_node 20 'healthy' P1 true '2026-02-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.issue)" = "10" ]; then pass "null blockedBy/assignees: treated as empty, filter survives"
else fail "null blockedBy/assignees: treated as empty, filter survives" "out=${out}"; fi
graphql_fixture "${dir}" \
    "$(issue_node 10 'over-50-blockers' P0 true '2026-01-01T00:00:00Z' AFK '[{"number": 98, "state": "CLOSED"}]' '' true)" \
    "$(issue_node 20 'clean-p1' P1 true '2026-02-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.issue)" = "20" ]; then pass "blockedBy hasNextPage: candidate treated as blocked"
else fail "blockedBy hasNextPage: candidate treated as blocked" "out=${out}"; fi
graphql_fixture "${dir}" \
    "$(issue_node 10 'in-progress' P0 true '2026-01-01T00:00:00Z' 'AFK,AFK:in-progress')" \
    "$(issue_node 11 'done' P0 true '2026-01-01T00:00:00Z' 'AFK,AFK:done')" \
    "$(issue_node 12 'failed' P0 true '2026-01-01T00:00:00Z' 'AFK,AFK:failed')" \
    "$(issue_node 13 'paused' P0 true '2026-01-01T00:00:00Z' 'AFK,AFK:paused')" \
    "$(issue_node 20 'clean' P1 true '2026-02-01T00:00:00Z')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(field "${out}" .pick.issue)" = "20" ]; then pass "state labels exclude a candidate"
else fail "state labels exclude a candidate" "out=${out}"; fi
rm -rf "${dir}"

echo "TEST: wayfinder routing"
dir="$(make_env)"
graphql_fixture "${dir}" "$(issue_node 10 'Which merge gate?' P1 true '2026-01-01T00:00:00Z' 'AFK,wayfinder:research')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "pick-wayfinder" ] && [ "$(field "${out}" .pick.type)" = "research" ] && [ "$(field "${out}" .pick.priority)" = "P1" ]; then
    pass "wayfinder:research pick: verdict pick-wayfinder, type research"
else fail "wayfinder:research pick: verdict pick-wayfinder, type research" "out=${out}"; fi
graphql_fixture "${dir}" "$(issue_node 11 'Provision the key' P1 true '2026-01-01T00:00:00Z' 'AFK,wayfinder:task')"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "pick-wayfinder" ] && [ "$(field "${out}" .pick.type)" = "task" ]; then pass "wayfinder:task pick: type task"
else fail "wayfinder:task pick: type task" "out=${out}"; fi
graphql_fixture "${dir}" "$(issue_node 20 'Tracer' P1 true '2026-02-01T00:00:00Z' AFK)"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "pick" ] && [ "$(field "${out}" .pick.type)" = "null" ]; then pass "plain AFK Slice: verdict pick, no type"
else fail "plain AFK Slice: verdict pick, no type" "out=${out}"; fi
graphql_fixture "${dir}" \
    "$(issue_node 10 'grill the shape' P0 true '2026-01-01T00:00:00Z' 'AFK,wayfinder:grilling')" \
    "$(issue_node 20 'clean slice' P1 true '2026-02-01T00:00:00Z' AFK)"
out="$(run_triage "${dir}" "${PROJECT_CFG}" 2>"${dir}/err")"
if [ "$(field "${out}" .pick.issue)" = "20" ] && grep -q '10' "${dir}/err"; then pass "wayfinder:grilling candidate skipped with a stderr note"
else fail "wayfinder:grilling candidate skipped with a stderr note" "out=${out} err=$(cat "${dir}/err")"; fi
for order in 'wayfinder:research,wayfinder:grilling' 'wayfinder:grilling,wayfinder:research'; do
    graphql_fixture "${dir}" \
        "$(issue_node 10 'both types' P0 true '2026-01-01T00:00:00Z' "AFK,${order}")" \
        "$(issue_node 20 'clean slice' P1 true '2026-02-01T00:00:00Z' AFK)"
    out="$(run_triage "${dir}" "${PROJECT_CFG}" 2>"${dir}/err")"
    if [ "$(field "${out}" .pick.issue)" = "20" ] && grep -q 'ambiguous wayfinder labels' "${dir}/err"; then
        pass "multi-type wayfinder candidate skipped as ambiguous (${order})"
    else fail "multi-type wayfinder candidate skipped as ambiguous (${order})" "out=${out}"; fi
done
rm -rf "${dir}"

echo "TEST: the project scope"
dir="$(make_env)"
echo "Token scopes: 'repo'" > "${dir}/auth.out"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "pick-mcp" ] && [ "$(field "${out}" .useMcpForProject)" = "true" ]; then pass "project pick without the project scope: verdict pick-mcp"
else fail "project pick without the project scope: verdict pick-mcp" "out=${out}"; fi
graphql_fixture "${dir}" "$(label_node 20 'clean' '2026-02-01T00:00:00Z')"
out="$(run_triage "${dir}" "${LABELS_CFG}")"
if [ "$(verdict "${out}")" = "pick" ] && [ "$(field "${out}" .useMcpForProject)" = "false" ]; then pass "label-only pick needs no project scope"
else fail "label-only pick needs no project scope" "out=${out}"; fi
rm -rf "${dir}"

echo "TEST: label-only pick (behaviour 2, AC 2)"
dir="$(make_env)"
graphql_fixture "${dir}" \
    "$(label_node 10 'newer' '2026-03-01T00:00:00Z')" \
    "$(label_node 20 'oldest-but-blocked' '2026-01-01T00:00:00Z' AFK '[{"number": 99, "state": "OPEN"}]')" \
    "$(label_node 30 'oldest-but-human' '2026-01-02T00:00:00Z' AFK '[]' 'a-human')" \
    "$(label_node 40 'oldest-eligible' '2026-02-01T00:00:00Z' AFK '[{"number": 98, "state": "CLOSED"}]' 'agent-bot')"
out="$(run_triage "${dir}" "${LABELS_CFG}")"
if [ "$(verdict "${out}")" = "pick" ] && [ "$(field "${out}" .pick.issue)" = "40" ] && [ "$(field "${out}" .pick.priority)" = "null" ] \
   && [ "$(field "${out}" .pickShape)" = "labels" ]; then
    pass "oldest open unblocked AFK issue with no human assignee is picked, priority null"
else fail "oldest open unblocked AFK issue with no human assignee is picked, priority null" "out=${out}"; fi
if ! grep -q 'projectItems' "${dir}/graphql.query" && grep -q 'blockedBy' "${dir}/graphql.query"; then
    pass "the label-only query selects no projectItems"
else fail "the label-only query selects no projectItems" "$(cat "${dir}/graphql.query")"; fi
graphql_fixture "${dir}" \
    "$(label_node 10 'done' '2026-01-01T00:00:00Z' 'AFK,AFK:done')" \
    "$(label_node 11 'wf' '2026-01-02T00:00:00Z' 'AFK,wayfinder:research')" \
    "$(label_node 12 'slice' '2026-01-03T00:00:00Z')"
out="$(run_triage "${dir}" "${LABELS_CFG}")"
if [ "$(verdict "${out}")" = "pick-wayfinder" ] && [ "$(field "${out}" .pick.issue)" = "11" ] && [ "$(field "${out}" .pick.priority)" = "null" ]; then
    pass "label-only: state labels exclude, wayfinder routing applies, oldest first"
else fail "label-only: state labels exclude, wayfinder routing applies, oldest first" "out=${out}"; fi
graphql_fixture "${dir}"
out="$(run_triage "${dir}" "${LABELS_CFG}")"; code=$?
if [ "${code}" -eq 0 ] && [ "$(verdict "${out}")" = "idle" ]; then pass "empty queue: verdict idle, exit 0"
else fail "empty queue: verdict idle, exit 0" "code=${code} out=${out}"; fi
echo 'garbage' > "${dir}/graphql.out"
out="$(run_triage "${dir}" "${LABELS_CFG}")"; code=$?
if [ "${code}" -eq 0 ] && [ "$(verdict "${out}")" = "idle" ]; then pass "a broken GraphQL response degrades to idle"
else fail "a broken GraphQL response degrades to idle" "code=${code} out=${out}"; fi
rm -rf "${dir}"

echo "TEST: the deployed lane fills an otherwise idle Fire, and only when it is on"
dir="$(make_env)"
DEPLOYED_CFG="$(printf '%s' "${LABELS_CFG}" | jq -c '.verification = {hermetic: {command: "verify/provider", smoke: true},
    deployed: {command: "verify/provider", enabled: true}} | .lanes.deployed = {present: true, enabled: true}')"
jq -n '[{number: 55, title: "live thing", headRefName: "feat/issue-9", mergedAt: "2026-09-05T00:00:00Z", comments: [],
         body: "## Manual verification\n\n- [ ] live check <!-- post-deploy: GET /api/health -->\n"}]' > "${dir}/merged.out"
out="$(run_triage "${dir}" "${DEPLOYED_CFG}")"; code=$?
if [ "${code}" -eq 0 ] && [ "$(verdict "${out}")" = "deployed" ] \
   && [ "$(field "${out}" '[.deployed.pr, .deployed.issue, .deployed.round, .deployed.max, (.deployed.items | length)] | map(tostring) | join(" ")')" = "55 9 1 3 1" ]; then
    pass "lane on, empty queue, a merged Agent PR with a deferred item: verdict deployed"
else fail "lane on, empty queue, a merged Agent PR with a deferred item: verdict deployed" "code=${code} out=${out}"; fi
graphql_fixture "${dir}" "$(label_node 12 'slice' '2026-01-03T00:00:00Z')"
out="$(run_triage "${dir}" "${DEPLOYED_CFG}")"
if [ "$(verdict "${out}")" = "pick" ] && [ "$(field "${out}" .deployed)" = "null" ]; then pass "a pickable Slice comes first: the lane only fills an idle Fire"
else fail "a pickable Slice comes first: the lane only fills an idle Fire" "out=${out}"; fi
graphql_fixture "${dir}"
: > "${dir}/calls.log"
for off in '.lanes.deployed.enabled = false' '.lanes.deployed = {present: false, enabled: false} | .verification.deployed = null'; do
    out="$(run_triage "${dir}" "$(printf '%s' "${DEPLOYED_CFG}" | jq -c "${off}")")"
    if [ "$(verdict "${out}")" = "idle" ] && ! grep -q -- '--state merged' "${dir}/calls.log"; then pass "lane off (${off}): idle, and no merged PR is ever listed"
    else fail "lane off (${off}): idle, and no merged PR is ever listed" "out=${out} calls=$(cat "${dir}/calls.log")"; fi
done
rm -rf "${dir}"

echo "TEST: the verdict is JSON in ALL cases"
dir="$(make_env)"
echo 77 > "${dir}/paused.out"; echo 'not a number' > "${dir}/pausecomments.out"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "resume" ] && [ "$(field "${out}" .paused.pauseCount)" = "1" ]; then pass "an unreadable pause count reads as one pause, JSON still emitted"
else fail "an unreadable pause count reads as one pause, JSON still emitted" "out=${out}"; fi
echo 'weird' > "${dir}/paused.out"
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "idle" ]; then pass "an unreadable paused number reads as no paused issue"
else fail "an unreadable paused number reads as no paused issue" "out=${out}"; fi
echo '' > "${dir}/paused.out"; echo 'garbage' > "${dir}/haddone.out"
pr_triage_scan() { printf '%s' '{"pr":501,"issue":441,"reason":"revise"}'; }
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "reconcile" ] && [ "$(field "${out}" .reconcile.hadDone)" = "false" ]; then pass "an unreadable hadDone reads as false"
else fail "an unreadable hadDone reads as false" "out=${out}"; fi
pr_triage_scan() { printf '%s' 'not json'; }
out="$(run_triage "${dir}" "${PROJECT_CFG}")"
if [ "$(verdict "${out}")" = "idle" ]; then pass "an unreadable pr-triage verdict falls through"
else fail "an unreadable pr-triage verdict falls through" "out=${out}"; fi
unset -f pr_triage_scan
graphql_fixture "${dir}" "$(issue_node 40 'x' P0 true '2026-01-01T00:00:00Z')"
run_triage "${dir}" "$(printf '%s' "${PROJECT_CFG}" | jq -c '.pick.project.priority_field = "Pri\"ority" | .repo.name = "wid\"gets"')" >/dev/null
if grep -q 'fieldValueByName(name: "Pri\\"ority")' "${dir}/graphql.query" && grep -q 'name: "wid\\"gets"' "${dir}/graphql.query"; then
    pass "config strings are JSON-escaped into the query"
else fail "config strings are JSON-escaped into the query" "$(cat "${dir}/graphql.query")"; fi
rm -rf "${dir}"

echo "TEST: the config resolves from a target dir and the CLI"
dir="$(make_env)"
mkdir -p "${dir}/target/.auto-agent"
cp "${FIXTURE}/.auto-agent/harness.json" "${dir}/target/.auto-agent/"
cat > "${dir}/git-stub" <<STUB
#!/usr/bin/env bash
case "\$*" in *"remote get-url origin"*) echo 'git@github.com:acme/widgets.git' ;; *) exit 1 ;; esac
STUB
chmod +x "${dir}/git-stub"
graphql_fixture "${dir}" "$(label_node 20 'clean' '2026-02-01T00:00:00Z')"
out="$(GH_BIN="${dir}/gh-stub" GIT_BIN="${dir}/git-stub" HARNESS_CONFIG_JSON= AUTO_AGENT_HOST_ENV="${dir}/none" DAEMON_GH_LOGIN= \
    bash "${CLI}" pickup-triage "${dir}/target")"; code=$?
if [ "${code}" -eq 0 ] && [ "$(verdict "${out}")" = "pick" ] && [ "$(field "${out}" .pick.issue)" = "20" ] \
   && [ "$(field "${out}" .pickShape)" = "labels" ] && grep -q -- '--repo acme/widgets' "${dir}/calls.log" \
   && grep -q 'owner: "acme", name: "widgets"' "${dir}/graphql.query"; then
    pass "bin/auto-agent pickup-triage <target-dir>: the fixture's label-only config drives the pick against the origin repo"
else fail "bin/auto-agent pickup-triage <target-dir>: the fixture's label-only config drives the pick against the origin repo" "code=${code} out=${out}"; fi
echo '{"broken": true}' > "${dir}/target/.auto-agent/harness.json"
out="$(GH_BIN="${dir}/gh-stub" GIT_BIN="${dir}/git-stub" HARNESS_CONFIG_JSON= AUTO_AGENT_HOST_ENV="${dir}/none" \
    bash "${CLI}" pickup-triage "${dir}/target" 2>/dev/null)"; code=$?
if [ "${code}" -eq 2 ] && [ "$(verdict "${out}")" = "no-config" ]; then pass "an invalid Harness config fails closed: no-config, exit 2"
else fail "an invalid Harness config fails closed: no-config, exit 2" "code=${code} out=${out}"; fi
rm -rf "${dir}"

echo ""
echo "${TESTS_RUN} tests, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  failed: %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
