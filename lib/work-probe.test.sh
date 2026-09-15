#!/usr/bin/env bash
# Tests for lib/work-probe.sh
#
# Run: bash lib/work-probe.test.sh
#
# Strategy: wp_scan's gh calls go through an injected GH_BIN stub serving
# canned per-query fixtures (the stub emulates gh's own --jq post-processing,
# so fixtures hold post-jq output for the issue queries and a raw JSON array
# for `pr list`); the Harness config arrives resolved through
# HARNESS_CONFIG_JSON. wp_decide is pure, driven with literal scan JSON on
# stdin. The reconcile signal comes through lib/pr-triage.sh's seam, stubbed
# here as shell functions.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT_DIR}/bin/auto-agent"
# shellcheck source=work-probe.sh
. "${SCRIPT_DIR}/work-probe.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

CFG='{"repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"main"},
  "pick":{"shape":"labels","project":null,"labels":{}},
  "rounds":{"pr_watch":10,"manual_verify":3,"revise":3,"deps_fix":3,"pause_resume":3}}'

# Build a workspace with the gh stub + default "nothing happening" fixtures.
# Echoes the dir. Individual tests overwrite fixtures as needed.
make_env() {
    local dir; dir="$(mktemp -d)"
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
args="\$*"
printf '%s\n' "\${args}" >> "${dir}/calls.log"
case "\${args}" in
    *"api user"*)                cat "${dir}/login.out" ;;
    *"--label AFK:in-progress"*) cat "${dir}/locked.out" ;;
    *"--label AFK:paused"*)      cat "${dir}/paused.out" ;;
    *"--label wayfinder:map"*)   cat "${dir}/maps.out" ;;
    *"issue list --repo acme/widgets --label AFK "*) cat "${dir}/picks.out" ;;
    *"pr list"*)                 cat "${dir}/prs.out" ;;
    *)                           exit 1 ;;
esac
STUB
    chmod +x "${dir}/gh-stub"
    echo "agent-bot" > "${dir}/login.out"
    echo "0"         > "${dir}/locked.out"
    echo "null"      > "${dir}/paused.out"
    echo "[]"        > "${dir}/picks.out"   # raw `issue list --json number,labels`
    echo "0"         > "${dir}/maps.out"
    echo "[]"        > "${dir}/prs.out"
    : > "${dir}/calls.log"
    printf '%s' "${dir}"
}

# scan <dir>: wp_scan with the stub and the resolved config
scan() {
    GH_BIN="$1/gh-stub" HARNESS_CONFIG_JSON="${CFG}" AUTO_AGENT_HOST_ENV="$1/none" DAEMON_GH_LOGIN= WP_AUTHOR= wp_scan
}

# A pr-triage stand-in: picks the first CONFLICTING PR by the configured author.
stub_pr_triage() {
    pr_triage_enrich() { cat; }
    pr_triage_pick() {
        jq -c --arg a "${PR_TRIAGE_AUTHOR:-}" '
            [.[] | select(.mergeable == "CONFLICTING" and ($a == "" or .author.login == $a))]
            | if length == 0 then {pr: null} else {pr: .[0].number, reason: "conflict"} end'
    }
}
unstub_pr_triage() { unset -f pr_triage_enrich pr_triage_pick pr_triage_bot_verdict_unworkable 2>/dev/null; }

#-------------------------------------------------------------------------------
echo "work-probe.sh tests:"

echo "TEST: wp_scan emits the full scan shape and reads the configured repo"
dir="$(make_env)"
cat > "${dir}/prs.out" <<'EOF'
[{"number":305,"headRefName":"feat/issue-281","isDraft":false,
  "mergeable":"CONFLICTING","labels":[],"createdAt":"2026-07-09T00:00:00Z",
  "author":{"login":"agent-bot"}}]
EOF
echo '[{"number":290,"labels":[{"name":"AFK"}]}]' > "${dir}/picks.out"
stub_pr_triage
got="$(scan "${dir}")"
want='{"locked":false,"reconcile":305,"paused":null,"pickSig":"290","prSig":"305","slices":1,"wayfinder":0,"openMaps":0}'
if [ "${got}" = "${want}" ]; then pass "full scan shape with a reconcile candidate"
else fail "full scan shape with a reconcile candidate" "got:  ${got}
want: ${want}"; fi
if [ "$(grep -c -- '--repo acme/widgets' "${dir}/calls.log")" -eq 5 ] \
   && ! grep -E '(issue|pr) list' "${dir}/calls.log" | grep -v -- '--repo acme/widgets' | grep -q .; then
    pass "every issue/pr list call carries --repo acme/widgets (AC 3)"
else fail "every issue/pr list call carries --repo acme/widgets (AC 3)" "$(cat "${dir}/calls.log")"; fi
sed -i 's/agent-bot/somebody-else/' "${dir}/prs.out"
got="$(scan "${dir}" | jq -r .reconcile)"
if [ "${got}" = "null" ]; then pass "the ours-filter is wired to the scanned login"
else fail "the ours-filter is wired to the scanned login" "reconcile=${got}"; fi
got="$(GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON="${CFG}" AUTO_AGENT_HOST_ENV="${dir}/none" DAEMON_GH_LOGIN=somebody-else wp_scan | jq -r .reconcile)"
if [ "${got}" = "305" ]; then pass "DAEMON_GH_LOGIN from the Host env is the ours-filter author"
else fail "DAEMON_GH_LOGIN from the Host env is the ours-filter author" "reconcile=${got}"; fi
pr_triage_bot_verdict_unworkable() { return 0; }
got="$(GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON="${CFG}" AUTO_AGENT_HOST_ENV="${dir}/none" DAEMON_GH_LOGIN=somebody-else wp_scan | jq -r .reconcile)"
if [ "${got}" = "null" ]; then pass "the suppression predicate is honoured"
else fail "the suppression predicate is honoured" "reconcile=${got}"; fi
unstub_pr_triage
got="$(scan "${dir}")"
want='{"locked":false,"reconcile":null,"paused":null,"pickSig":"290","prSig":"305","slices":1,"wayfinder":0,"openMaps":0}'
if [ "${got}" = "${want}" ]; then pass "without lib/pr-triage.sh reconcile is null and prSig still carries the shrink signal"
else fail "without lib/pr-triage.sh reconcile is null and prSig still carries the shrink signal" "got: ${got}"; fi
rm -rf "${dir}"

echo "TEST: wp_scan needs a resolvable config"
dir="$(make_env)"
out="$(GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= AUTO_AGENT_HOST_ENV="${dir}/none" wp_scan 2>/dev/null)"; code=$?
if [ "${code}" -eq 2 ] && [ -z "${out}" ] && [ ! -s "${dir}/calls.log" ]; then pass "no config: exit 2, no JSON, no gh call"
else fail "no config: exit 2, no JSON, no gh call" "code=${code} out=${out}"; fi
rm -rf "${dir}"

echo "TEST: the lock fails safe (behaviour 3)"
dir="$(make_env)"
rm "${dir}/locked.out"
if [ "$(scan "${dir}" | jq -r .locked)" = "true" ]; then pass "lock-query error scans as locked=true"
else fail "lock-query error scans as locked=true"; fi
echo 1 > "${dir}/locked.out"
if [ "$(scan "${dir}" | jq -r .locked)" = "true" ]; then pass "a held lock scans as locked=true"
else fail "a held lock scans as locked=true"; fi
rm -rf "${dir}"

echo "TEST: wp_decide wake rules"
decide() { printf '%s' "$1" | wp_decide "${2:-}" "${3:-}"; }
if r="$(decide '{"locked":false,"reconcile":305,"paused":null,"pickSig":""}')" && [ "${r}" = "reconcile PR #305" ]; then pass "wakes on reconcile naming the PR"
else fail "wakes on reconcile naming the PR" "${r:-}"; fi
if ! decide '{"locked":true,"reconcile":305,"paused":281,"pickSig":"290"}' >/dev/null; then pass "never wakes while locked"
else fail "never wakes while locked"; fi
if r="$(decide '{"locked":false,"reconcile":null,"paused":281,"pickSig":""}')" && [ "${r}" = "resume issue #281" ]; then pass "wakes on a paused issue"
else fail "wakes on a paused issue" "${r:-}"; fi
s='{"locked":false,"reconcile":null,"paused":null,"pickSig":"290,291"}'
if ! decide "${s}" "290,291" >/dev/null; then pass "unchanged pick signature does not wake"
else fail "unchanged pick signature does not wake"; fi
if r="$(decide "${s}" "290")" && [ "${r}" = "new pick candidate(s) #290,291" ]; then pass "changed pick signature wakes"
else fail "changed pick signature wakes" "${r:-}"; fi
if ! decide 'not json at all' >/dev/null 2>&1 && ! decide '' >/dev/null 2>&1; then pass "malformed or empty scan keeps sleeping"
else fail "malformed or empty scan keeps sleeping"; fi
if r="$(decide '{"locked":false,"reconcile":null,"paused":null,"pickSig":"","prSig":""}' "" "354,367")" && [ "${r}" = "PR(s) left the open set #354,367" ]; then
    pass "wakes when baseline PRs leave the open set"
else fail "wakes when baseline PRs leave the open set" "${r:-}"; fi
r="$(decide '{"locked":false,"reconcile":null,"paused":null,"pickSig":"","prSig":"367"}' "" "354,367")"
if [ "${r}" = "PR(s) left the open set #354" ]; then pass "names only the PRs that left"
else fail "names only the PRs that left" "${r}"; fi
if ! decide '{"locked":false,"reconcile":null,"paused":null,"pickSig":"","prSig":"354,367,401"}' "" "354,367" >/dev/null; then pass "PR-set growth alone does not wake"
else fail "PR-set growth alone does not wake"; fi
if ! decide '{"locked":false,"reconcile":null,"paused":null,"pickSig":"","prSig":"354,367"}' "" "354,367" >/dev/null; then pass "unchanged PR set keeps sleeping"
else fail "unchanged PR set keeps sleeping"; fi
if ! decide '{"locked":false,"reconcile":null,"paused":null,"pickSig":"","prSig":null}' "" "354,367" >/dev/null; then pass "an unreadable PR set never shrink-wakes"
else fail "an unreadable PR set never shrink-wakes"; fi
if ! decide '{"locked":false,"reconcile":null,"paused":null,"pickSig":"","prSig":""}' "" "" >/dev/null; then pass "an empty PR baseline never shrink-wakes"
else fail "an empty PR baseline never shrink-wakes"; fi
if ! decide '{"locked":true,"reconcile":null,"paused":null,"pickSig":"","prSig":""}' "" "354,367" >/dev/null; then pass "a held lock suppresses shrink-wake"
else fail "a held lock suppresses shrink-wake"; fi

echo "TEST: the open-PR signature"
dir="$(make_env)"
cat > "${dir}/prs.out" <<'EOF'
[{"number":367,"headRefName":"feat/a","isDraft":false,"mergeable":"MERGEABLE","labels":[],"createdAt":"2026-07-15T00:00:00Z","author":{"login":"agent-bot"}},
 {"number":354,"headRefName":"feat/b","isDraft":false,"mergeable":"MERGEABLE","labels":[],"createdAt":"2026-07-15T00:00:00Z","author":{"login":"agent-bot"}}]
EOF
if [ "$(scan "${dir}" | jq -r .prSig)" = "354,367" ]; then pass "prSig is the sorted CSV of open PR numbers"
else fail "prSig is the sorted CSV of open PR numbers"; fi
if grep -q 'headRefOid' "${dir}/calls.log" && grep -q 'reviewDecision' "${dir}/calls.log"; then pass "the listing requests pr-triage's fields"
else fail "the listing requests pr-triage's fields" "$(grep 'pr list' "${dir}/calls.log")"; fi
echo '[]' > "${dir}/prs.out"
if [ "$(scan "${dir}" | jq -c .prSig)" = '""' ]; then pass "an empty PR set is a readable empty signature"
else fail "an empty PR set is a readable empty signature"; fi
rm "${dir}/prs.out"
if [ "$(scan "${dir}" | jq -c .prSig)" = 'null' ]; then pass "a pr-list error scans prSig as null"
else fail "a pr-list error scans prSig as null"; fi
rm -rf "${dir}"

echo "TEST: the queue split and the map count"
dir="$(make_env)"
cat > "${dir}/picks.out" <<'EOF'
[{"number":589,"labels":[{"name":"AFK"}]},
 {"number":590,"labels":[{"name":"AFK"},{"name":"wayfinder:research"}]},
 {"number":591,"labels":[{"name":"AFK"},{"name":"wayfinder:task"}]},
 {"number":592,"labels":[{"name":"AFK"},{"name":"AFK:done"}]},
 {"number":593,"labels":[{"name":"AFK"},{"name":"AFK:in-progress"}]}]
EOF
echo 2 > "${dir}/maps.out"
echo 281 > "${dir}/paused.out"
got="$(scan "${dir}" | jq -c '{paused, pickSig, slices, wayfinder, openMaps}')"
want='{"paused":281,"pickSig":"589,590,591","slices":1,"wayfinder":2,"openMaps":2}'
if [ "${got}" = "${want}" ]; then pass "eligible queue split into Slices and wayfinder tickets, maps counted, paused named"
else fail "eligible queue split into Slices and wayfinder tickets, maps counted, paused named" "got: ${got}"; fi
if grep -- 'wayfinder:map' "${dir}/calls.log" | grep -q -- '--limit'; then pass "the map count passes an explicit --limit"
else fail "the map count passes an explicit --limit"; fi
rm "${dir}/picks.out" "${dir}/maps.out" "${dir}/paused.out"
got="$(scan "${dir}" | jq -c '{paused, pickSig, slices, wayfinder, openMaps}')"
want='{"paused":null,"pickSig":"","slices":0,"wayfinder":0,"openMaps":0}'
if [ "${got}" = "${want}" ]; then pass "queue, map and paused query errors fail safe as empty"
else fail "queue, map and paused query errors fail safe as empty" "got: ${got}"; fi
rm -rf "${dir}"

echo "TEST: bin/auto-agent work-probe"
dir="$(make_env)"
out="$(GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON="${CFG}" AUTO_AGENT_HOST_ENV="${dir}/none" DAEMON_GH_LOGIN= bash "${CLI}" work-probe)"; code=$?
if [ "${code}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -r .locked)" = "false" ]; then pass "the CLI prints one scan JSON"
else fail "the CLI prints one scan JSON" "code=${code} out=${out}"; fi
rm -rf "${dir}"

echo ""
echo "${TESTS_RUN} tests, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  failed: %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
