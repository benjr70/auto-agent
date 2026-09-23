#!/usr/bin/env bash
# Tests for lib/deployed-tier.sh
#
# Run: bash lib/deployed-tier.test.sh
#
# Strategy, three layers:
#   1. lane gating (behaviour 1, critical): real `.auto-agent/harness.json`
#      files through the real loader (gh and git stubbed for the repo facts),
#      so "the block exists and `enabled` is not false" is proven on the file a
#      maintainer writes — including an omitted `enabled`, which is on;
#   2. the read-only round (behaviour 2): a recording stub provider proves the
#      tier calls `status` and never `up` or `down`, on the healthy path and
#      on every failing one, and maps each `status` exit to the tier's own;
#   3. the fixture Target Project end to end (AC 1): a live fixture service
#      stands in for the deployed environment, `status` resolves it, and the
#      PR's deferred items are ticked from what that run exported — while the
#      same fixture without the block never gets as far as asking GitHub.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/deployed-tier.sh"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT}/bin/auto-agent"
FIX="${ROOT}/plugin/fixtures/target-project"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env
unset HARNESS_CONFIG_JSON AUTO_AGENT_TARGET_DIR
WORK="$(mktemp -d)"
FIXTURE_RUN_DIR="${WORK}/fixture-run"; export FIXTURE_RUN_DIR
E2E_PR=917
trap '"${FIX}/verify/provider" down --pr ${E2E_PR} >/dev/null 2>&1; rm -rf "${WORK}"' EXIT

# stubs <dir> : gh and git that answer the loader's repo questions and serve
# `pr list` from <dir>/merged.json, logging every call to <dir>/gh.log
stubs() {
    local dir="$1"
    echo '[]' > "${dir}/merged.json"
    cat > "${dir}/gh" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/gh.log"
case "\$*" in
  *"repo view"*) echo trunk ;;
  "pr list"*) cat "${dir}/merged.json" ;;
  *) exit 0 ;;
esac
STUB
    cat > "${dir}/git" <<'STUB'
#!/usr/bin/env bash
case "$*" in *"remote get-url origin"*) echo 'https://github.com/acme/widgets.git' ;; *) exit 0 ;; esac
STUB
    chmod +x "${dir}/gh" "${dir}/git"
}

# target <name> <jq-edit-of-the-fixture-config> : a copy of the fixture Target
# Project whose harness.json is the fixture's with the edit applied. The copy
# sits two levels below a link to the plugin's providers, where the fixture's
# provider finds provider-lib.sh by its relative path.
target() {
    local dir="${WORK}/$1/fixtures/target-project"
    mkdir -p "${WORK}/$1/fixtures"
    ln -s "${ROOT}/plugin/providers" "${WORK}/$1/providers"
    cp -r "${FIX}" "${dir}"
    jq "$2" "${FIX}/.auto-agent/harness.json" > "${dir}/.auto-agent/harness.json"
    stubs "${dir}"
    printf '%s\n' "${dir}"
}

# dt <target-dir> <args...> : the CLI over <target-dir>, its stubs injected.
# "Now" is pinned an hour after the last canned timestamp.
NOW="$(date -u -d 2026-09-20T01:00:00Z +%s)"
dt() {
    local dir="$1"; shift
    GH_BIN="${dir}/gh" GIT_BIN="${dir}/git" DEPLOYED_TIER_NOW="${DEPLOYED_TIER_NOW:-${NOW}}" \
        bash "${CLI}" deployed "$@" "${dir}"
}

# stub_provider <target-dir> <status-exit> [<status-stdout>] : replace the
# deployed command with one that records every call and answers `status`
stub_provider() {
    local dir="$1" rc="$2" block="${3:-}"
    printf '%s' "${block}" > "${dir}/status.out"
    cat > "${dir}/verify/provider" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "${dir}/calls"
case "\$1" in
  status) echo "status: probing the live environment" >&2; cat "${dir}/status.out"; exit ${rc} ;;
  *) exit 0 ;;
esac
STUB
    chmod +x "${dir}/verify/provider"
    rm -f "${dir}/calls"
}

#-------------------------------------------------------------------------------
echo "TEST: the lane is on only when the block exists and enabled is not false (behaviour 1)"

on="$(target on '.verification.deployed.enabled = true')"
out="$(dt "${on}" lane 2>&1)"; rc=$?
if [ "${rc}" -eq 0 ] && [ "${out}" = "deployed-lane: on — verify/provider" ]; then pass "enabled true: on, naming the command"
else fail "enabled true: on, naming the command" "rc=${rc} out=${out}"; fi

omitted="$(target omitted 'del(.verification.deployed.enabled)')"
out="$(dt "${omitted}" lane 2>&1)"; rc=$?
if [ "${rc}" -eq 0 ] && [ "${out}" = "deployed-lane: on — verify/provider" ]; then pass "enabled omitted: on (the schema default)"
else fail "enabled omitted: on (the schema default)" "rc=${rc} out=${out}"; fi

off="$(target off '.verification.deployed.enabled = false')"
out="$(dt "${off}" lane 2>&1)"; rc=$?
if [ "${rc}" -eq 3 ] && [ "${out}" = "deployed-lane: off — verification.deployed.enabled is false" ]; then pass "enabled false: off, saying why"
else fail "enabled false: off, saying why" "rc=${rc} out=${out}"; fi

absent="$(target absent 'del(.verification.deployed)')"
out="$(dt "${absent}" lane 2>&1)"; rc=$?
if [ "${rc}" -eq 3 ] && [ "${out}" = "deployed-lane: off — the Harness config declares no verification.deployed block" ]; then pass "no block: off, saying why"
else fail "no block: off, saying why" "rc=${rc} out=${out}"; fi

for d in "${off}" "${absent}"; do
    stub_provider "${d}" 0 "FIXTURE_WEB_URL=http://x"
    : > "${d}/gh.log"
    dt "${d}" pick >/dev/null 2>&1; prc=$?
    dt "${d}" status >/dev/null 2>&1; src=$?
    if [ "${prc}" -eq 3 ] && [ "${src}" -eq 3 ] && ! grep -q '^pr list' "${d}/gh.log" && [ ! -e "${d}/calls" ]; then
        pass "lane off (${d#"${WORK}/"}): pick and status exit 3, no PR is listed, the command never runs"
    else
        fail "lane off (${d#"${WORK}/"}): pick and status exit 3, no PR is listed, the command never runs" \
            "pick=${prc} status=${src} gh=$(cat "${d}/gh.log") calls=$(cat "${d}/calls" 2>/dev/null)"
    fi
done

#-------------------------------------------------------------------------------
echo "TEST: status is read-only and maps the contract's exits (behaviour 2)"

ro="$(target readonly '.verification.deployed.enabled = true')"
BLOCK=$'FIXTURE_WEB_URL=https://live.example\nFIXTURE_API_URL=https://live.example/api?a=b'

stub_provider "${ro}" 0 "${BLOCK}"
out="$(dt "${ro}" status 2>"${WORK}/err")"; rc=$?
if [ "${rc}" -eq 0 ] && [ "${out}" = "${BLOCK}" ]; then pass "healthy: exit 0, stdout is exactly the block"
else fail "healthy: exit 0, stdout is exactly the block" "rc=${rc} out=${out}"; fi
if grep -q 'status: probing the live environment' "${WORK}/err"; then pass "the command's own progress reaches stderr"
else fail "the command's own progress reaches stderr" "$(cat "${WORK}/err")"; fi
if [ "$(cat "${ro}/calls")" = "status" ]; then pass "healthy: the command was called with status, and nothing else"
else fail "healthy: the command was called with status, and nothing else" "$(cat "${ro}/calls")"; fi

check_status() { # <name> <status-rc> <block> <want-rc> <want-stderr>
    stub_provider "${ro}" "$2" "$3"
    local o r
    o="$(dt "${ro}" status 2>"${WORK}/err")"; r=$?
    if [ "${r}" -eq "$4" ] && grep -qF -- "$5" "${WORK}/err" && [ -z "${o}" ]; then pass "$1: exit $4, no block on stdout"
    else fail "$1: exit $4, no block on stdout" "rc=${r} out=${o} err=$(cat "${WORK}/err")"; fi
    if [ "$(cat "${ro}/calls")" = "status" ]; then pass "$1: never up, never down"
    else fail "$1: never up, never down" "calls=$(cat "${ro}/calls")"; fi
}
check_status "unhealthy (status exit 1)" 1 "" 4 "status exited 1 (the deployed environment is unhealthy)"
check_status "prerequisite missing (status exit 3)" 3 "" 4 "status exited 3 (prerequisite missing)"
check_status "an exit outside the contract" 7 "" 2 "status exited 7, want 0 healthy, 1 unhealthy or 3 prerequisite missing"
check_status "progress on stdout" 0 "probing..." 2 "status printed a line that is not KEY=value: probing..."
check_status "a lower-case key" 0 "web_url=http://x" 2 "status printed a key that is not an uppercase shell identifier: web_url"
check_status "an empty block" 0 "" 2 "status printed no keys"

check_status "a declared Surface's url_key missing (ADR 0003: an infra-error)" 0 "FIXTURE_WEB_URL=https://live.example" 2 \
    "the status block does not carry the url_key of Surface api (FIXTURE_API_URL)"

chmod -x "${ro}/verify/provider"
dt "${ro}" status >/dev/null 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 2 ] && grep -q 'the deployed command verify/provider is not an executable file' "${WORK}/err"; then pass "a command that is not executable: exit 2"
else fail "a command that is not executable: exit 2" "rc=${rc} err=$(cat "${WORK}/err")"; fi

#-------------------------------------------------------------------------------
echo "TEST: the deferred items are the unchecked post-deploy items of the verification sections"

cat > "${WORK}/body.md" <<'MD'
## Summary

- [ ] a stray box <!-- post-deploy: not a verification item -->

## Manual verification

- [x] already signed off <!-- post-deploy: GET /api/health -->
- [ ] the page renders the list
- [ ] the live health endpoint answers ok <!-- post-deploy: GET $FIXTURE_API_URL/health -->

## Human verification required

- [ ] the item survives a restart <!-- post-deploy: GET $FIXTURE_API_URL/items -->
MD
out="$(bash "${CLI}" deployed items "${WORK}/body.md")"
want=$'manual\tthe live health endpoint answers ok <!-- post-deploy: GET $FIXTURE_API_URL/health -->\nhuman\tthe item survives a restart <!-- post-deploy: GET $FIXTURE_API_URL/items -->'
if [ "${out}" = "${want}" ]; then pass "only unchecked, tagged items inside the two sections"
else fail "only unchecked, tagged items inside the two sections" "${out}"; fi
out2="$(bash "${CLI}" deployed items < "${WORK}/body.md")"
if [ "${out2}" = "${want}" ]; then pass "the body can come on stdin"; else fail "the body can come on stdin" "${out2}"; fi

#-------------------------------------------------------------------------------
echo "TEST: the pick is the oldest merged Agent PR with deferred items and rounds left"

pk="$(target pick '.verification.deployed.enabled = true')"
TAGGED=$'## Manual verification\n\n- [ ] live check <!-- post-deploy: GET /api/health -->\n'
ROUND='### Deployed verification — round 1/3'
jq -n --arg t "${TAGGED}" --arg r "${ROUND}" '[
  {number: 60, title: "newest", headRefName: "feat/issue-12", mergedAt: "2026-09-20T00:00:00Z", body: $t, comments: [{body: $r}]},
  {number: 49, title: "merged minutes ago", headRefName: "feat/issue-3", mergedAt: "2026-09-20T00:50:00Z", body: $t, comments: []},
  {number: 48, title: "round minutes ago", headRefName: "feat/issue-2", mergedAt: "2026-08-01T00:00:00Z", body: $t,
   comments: [{body: $r, createdAt: "2026-09-20T00:45:00.123Z"}]},
  {number: 51, title: "human branch", headRefName: "fix-typo", mergedAt: "2026-09-01T00:00:00Z", body: $t, comments: []},
  {number: 52, title: "exhausted", headRefName: "feat/issue-7", mergedAt: "2026-09-02T00:00:00Z", body: $t,
   comments: [{body: $r}, {body: $r}, {body: $r}]},
  {number: 53, title: "nothing deferred", headRefName: "feat/issue-8", mergedAt: "2026-09-03T00:00:00Z",
   body: "## Manual verification\n\n- [ ] local only\n", comments: []},
  {number: 55, title: "the one", headRefName: "feat/issue-9", mergedAt: "2026-09-05T00:00:00Z", body: $t,
   comments: [{body: "unrelated"}, {body: $r}]}
]' > "${pk}/merged.json"
out="$(dt "${pk}" pick 2>/dev/null)"; rc=$?
want='{"pr":55,"issue":9,"title":"the one","branch":"feat/issue-9","round":2,"max":3,"items":[{"section":"manual","text":"live check <!-- post-deploy: GET /api/health -->"}]}'
if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -c 'del(.mergedAt)')" = "${want}" ]; then
    pass "skips a non-Agent branch, an exhausted PR, one with nothing deferred and two inside the wait window; the round is the next one"
else fail "skips a non-Agent branch, an exhausted PR, one with nothing deferred and two inside the wait window; the round is the next one" "rc=${rc} out=${out}"; fi
out="$(DEPLOYED_TIER_WAIT_MINS=5 dt "${pk}" pick 2>/dev/null)"
if [ "$(printf '%s' "${out}" | jq -c '[.pr, .round]')" = '[48,2]' ]; then pass "a shorter wait window lets the PR whose last round was 15 minutes ago back in"
else fail "a shorter wait window lets the PR whose last round was 15 minutes ago back in" "${out}"; fi
if grep -q '^pr list --repo acme/widgets --state merged' "${pk}/gh.log"; then pass "merged PRs of the configured repo are listed"
else fail "merged PRs of the configured repo are listed" "$(cat "${pk}/gh.log")"; fi

echo '[]' > "${pk}/merged.json"
dt "${pk}" pick >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 1 ]; then pass "nothing deferred anywhere: exit 1"; else fail "nothing deferred anywhere: exit 1" "rc=${rc}"; fi

printf 'not json' > "${pk}/merged.json"
dt "${pk}" pick >/dev/null 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 1 ] && grep -q 'could not list' "${WORK}/err"; then pass "an unreadable PR list is no pick, and says so"
else fail "an unreadable PR list is no pick, and says so" "rc=${rc} err=$(cat "${WORK}/err")"; fi

#-------------------------------------------------------------------------------
echo "TEST: the fixture with a deployed block ticks its deferred items from a status run (AC 1)"

e2e="$(target e2e '.verification.deployed.enabled = true')"
# The live environment: the fixture service, booted by the test (never by the tier).
up_block="$("${FIX}/verify/provider" up --pr "${E2E_PR}" 2>/dev/null)" || true
FIXTURE_DEPLOYED_URL="$(printf '%s\n' "${up_block}" | sed -n 's/^FIXTURE_WEB_URL=//p')"
export FIXTURE_DEPLOYED_URL
if [ -z "${FIXTURE_DEPLOYED_URL}" ]; then
    fail "the fixture service stands up as the live environment" "up printed: ${up_block}"
else
    BLOCK="$(dt "${e2e}" status 2>/dev/null)"; rc=$?
    if [ "${rc}" -eq 0 ] && printf '%s\n' "${BLOCK}" | grep -qx "FIXTURE_API_URL=${FIXTURE_DEPLOYED_URL}/api"; then
        pass "status resolves the live environment's targets itself"
    else fail "status resolves the live environment's targets itself" "rc=${rc} block=${BLOCK}"; fi

    # What the round does with it: export the block, exercise each deferred
    # item read-only, tick the ones that passed.
    passed="$(
        while IFS='=' read -r k v; do [ -n "$k" ] && export "$k=$v"; done <<<"${BLOCK}"
        while IFS=$'\t' read -r _ item; do
            path="$(printf '%s' "${item}" | sed -n 's/.*post-deploy: GET \$FIXTURE_API_URL\([^ ]*\).*/\1/p')"
            curl -fsS "${FIXTURE_API_URL}${path}" >/dev/null 2>&1 && printf '%s\n' "${item}"
        done < <(bash "${CLI}" deployed items "${WORK}/body.md")
    )"
    printf '%s\n' "${passed}" | bash "${CLI}" checklist tick "${WORK}/body.md" > "${WORK}/body.new.md"
    if grep -qF -- '- [x] the live health endpoint answers ok <!-- post-deploy:' "${WORK}/body.new.md" \
       && grep -qF -- '- [x] the item survives a restart <!-- post-deploy:' "${WORK}/body.new.md" \
       && grep -qF -- '- [ ] the page renders the list' "${WORK}/body.new.md" \
       && grep -qF -- '- [ ] a stray box' "${WORK}/body.new.md"; then
        pass "both deferred items are ticked; the untagged item and the stray box are not"
    else fail "both deferred items are ticked; the untagged item and the stray box are not" "$(cat "${WORK}/body.new.md")"; fi
    if [ -z "$(bash "${CLI}" deployed items "${WORK}/body.new.md")" ]; then pass "a ticked body has nothing left for the lane"
    else fail "a ticked body has nothing left for the lane"; fi
fi

#-------------------------------------------------------------------------------
echo "TEST: usage"
bash "${CLI}" deployed bogus >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 2 ]; then pass "unknown subcommand: exit 2"; else fail "unknown subcommand: exit 2" "rc=${rc}"; fi
bash "${CLI}" deployed lane "${WORK}/no-such-dir" >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 2 ]; then pass "no Harness config: exit 2"; else fail "no Harness config: exit 2" "rc=${rc}"; fi

echo ""
echo "deployed-tier: ${TESTS_RUN} run, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
