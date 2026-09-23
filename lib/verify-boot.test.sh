#!/usr/bin/env bash
# Tests for lib/verify-boot.sh
#
# Run: bash lib/verify-boot.test.sh
#
# Strategy, two layers:
#   1. the fixture Target Project's real Environment provider is booted, its
#      block is read back, and the round is torn down — the boot half of AC 1;
#   2. a recording stub provider proves what no real provider can show: the
#      call ORDER (down before the first up, down between the one retry, down
#      on every failing path), and that a round which never booted returns an
#      infra-error rather than anything that could be read as a verdict.
#
# Only the config is injected (HARNESS_CONFIG_JSON), so no gh and no git
# remote are needed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/verify-boot.sh"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
FIX="${ROOT}/plugin/fixtures/target-project"

# cfg <target-dir> <hermetic-json> <surfaces-json>
cfg() {
    jq -cn --arg dir "$1/.auto-agent" --argjson h "$2" --argjson s "$3" \
        '{config_dir: $dir,
          repo: {owner: "acme", name: "widgets", slug: "acme/widgets", default_branch: "trunk"},
          pick: {shape: "labels", project: null, labels: {}},
          verification: {hermetic: $h, deployed: null},
          surfaces: $s}'
}

# stub_target : a Target Project whose provider records every call
stub_target() {
    local dir; dir="$(mktemp -d "${WORK}/stub-XXXXXX")"
    mkdir -p "${dir}/.auto-agent"
    cat > "${dir}/provider" <<'STUB'
#!/usr/bin/env bash
D="$(cd "$(dirname "$0")" && pwd)"
echo "$1" >> "$D/calls"
case "$1" in
  down) exit 0;;
  up)
    echo attempt >> "$D/attempts"
    if [ -f "$D/up-rc" ]; then
        rc="$(cat "$D/up-rc")"
        if ! { [ "$rc" = 4 ] && [ -f "$D/up-recovers" ] && [ "$(wc -l < "$D/attempts")" -ge 2 ]; }; then
            echo "up: exiting $rc" >&2; exit "$rc"
        fi
    fi
    cat "$D/block"; exit 0;;
esac
exit 2
STUB
    chmod +x "${dir}/provider"
    printf 'WEB_URL=http://web.test\n' > "${dir}/block"
    printf '%s\n' "${dir}"
}

WEB_SURFACE='{"web": {"kind": "browser", "url_key": "WEB_URL", "paths": ["web/**"], "viewport": null, "launcher": null}}'
HERMETIC='{"command": "provider", "smoke": true}'

echo "TEST: a healthy boot prints the provider's block and nothing else on stdout"
d="$(stub_target)"
out="$(HARNESS_CONFIG_JSON="$(cfg "${d}" "${HERMETIC}" "${WEB_SURFACE}")" bash "${LIB}" up --pr 5 2>"${WORK}/err")"; rc=$?
if [ "${rc}" -eq 0 ] && [ "${out}" = "WEB_URL=http://web.test" ]; then pass "up: the block is the contract"; else fail "up: the block is the contract" "rc=${rc} out=${out} err=$(cat "${WORK}/err")"; fi

echo "TEST: down runs before the first up (a killed Fire can leave an environment behind)"
if [ "$(head -1 "${d}/calls")" = "down" ]; then pass "up: down first"; else fail "up: down first" "$(cat "${d}/calls")"; fi

echo "TEST: a boot that fails once is retried once, with a down between"
d="$(stub_target)"; printf '4\n' > "${d}/up-rc"; : > "${d}/up-recovers"
out="$(HARNESS_CONFIG_JSON="$(cfg "${d}" "${HERMETIC}" "${WEB_SURFACE}")" bash "${LIB}" up --pr 5 2>/dev/null)"; rc=$?
if [ "${rc}" -eq 0 ] && [ "$(tr '\n' ' ' < "${d}/calls")" = "down up down up " ]; then pass "up: one retry, down between"; else fail "up: one retry, down between" "rc=${rc} calls=$(tr '\n' ' ' < "${d}/calls")"; fi

echo "TEST: a boot that fails twice is exit 4 — an infra-error, with the environment torn down"
d="$(stub_target)"; printf '4\n' > "${d}/up-rc"
HARNESS_CONFIG_JSON="$(cfg "${d}" "${HERMETIC}" "${WEB_SURFACE}")" bash "${LIB}" up --pr 5 >"${WORK}/out" 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 4 ] && [ ! -s "${WORK}/out" ] && [ "$(grep -c '^up$' "${d}/calls")" = "2" ] && [ "$(tail -1 "${d}/calls")" = "down" ]; then
    pass "up: boot failed twice"
else fail "up: boot failed twice" "rc=${rc} calls=$(tr '\n' ' ' < "${d}/calls")"; fi
if grep -q 'boot failed' "${WORK}/err"; then pass "up: the reason is on stderr"; else fail "up: the reason is on stderr" "$(cat "${WORK}/err")"; fi

echo "TEST: a missing prerequisite is not retried"
d="$(stub_target)"; printf '3\n' > "${d}/up-rc"
HARNESS_CONFIG_JSON="$(cfg "${d}" "${HERMETIC}" "${WEB_SURFACE}")" bash "${LIB}" up --pr 5 >/dev/null 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 4 ] && [ "$(grep -c '^up$' "${d}/calls")" = "1" ] && grep -q 'prerequisite missing' "${WORK}/err"; then
    pass "up: exit 3 is not retried"
else fail "up: exit 3 is not retried" "rc=${rc} calls=$(tr '\n' ' ' < "${d}/calls")"; fi

echo "TEST: progress written to stdout instead of stderr fails the boot rather than being exported"
d="$(stub_target)"; printf 'booting the thing...\nWEB_URL=http://web.test\n' > "${d}/block"
HARNESS_CONFIG_JSON="$(cfg "${d}" "${HERMETIC}" "${WEB_SURFACE}")" bash "${LIB}" up --pr 5 >/dev/null 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 4 ] && grep -q 'not a KEY=value block' "${WORK}/err"; then pass "up: the block grammar"; else fail "up: the block grammar" "rc=${rc} err=$(cat "${WORK}/err")"; fi

echo "TEST: a Surface the block cannot reach is an infra-error naming it"
d="$(stub_target)"
two='{"web": {"kind": "browser", "url_key": "WEB_URL", "paths": ["web/**"], "viewport": null, "launcher": null},
      "panel": {"kind": "electron", "url_key": "PANEL_URL", "paths": ["panel/**"], "viewport": null, "launcher": "verify/app"}}'
HARNESS_CONFIG_JSON="$(cfg "${d}" "${HERMETIC}" "${two}")" bash "${LIB}" up --pr 5 >/dev/null 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 4 ] && grep -q 'panel (PANEL_URL)' "${WORK}/err"; then pass "up: a missing url_key"; else fail "up: a missing url_key" "rc=${rc} err=$(cat "${WORK}/err")"; fi

echo "TEST: the Bootstrap state is exit 3 — nothing to boot, and not a failure"
d="$(stub_target)"
HARNESS_CONFIG_JSON="$(cfg "${d}" null "${WEB_SURFACE}")" bash "${LIB}" up --pr 5 >/dev/null 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 3 ] && grep -q 'Bootstrap state' "${WORK}/err"; then pass "up: Bootstrap state"; else fail "up: Bootstrap state" "rc=${rc} err=$(cat "${WORK}/err")"; fi

echo "TEST: a hermetic command that is not executable is a usage error, before anything is driven"
d="$(stub_target)"
HARNESS_CONFIG_JSON="$(cfg "${d}" '{"command": "nope", "smoke": false}' "${WEB_SURFACE}")" bash "${LIB}" up --pr 5 >/dev/null 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 2 ] && grep -q 'not an executable file' "${WORK}/err"; then pass "up: no provider"; else fail "up: no provider" "rc=${rc} err=$(cat "${WORK}/err")"; fi

echo "TEST: an app launch that fails leaves the environment UP and is exit 5"
d="$(stub_target)"
printf 'WEB_URL=http://web.test\nPANEL_URL=http://panel.test\n' > "${d}/block"
HARNESS_CONFIG_JSON="$(cfg "${d}" "${HERMETIC}" "${two}")" \
    SURFACE_LAUNCH_RUN_DIR="${WORK}/run" DISPLAY=:99 DISPLAY_PROBE_CMD=true \
    bash "${LIB}" up --pr 5 --surface panel >"${WORK}/out" 2>"${WORK}/err"; rc=$?
if [ "${rc}" -eq 5 ] && [ "$(tail -1 "${d}/calls")" = "up" ] && grep -q 'WEB_URL=' "${WORK}/out"; then
    pass "up: a failed launch leaves the environment up"
else fail "up: a failed launch leaves the environment up" "rc=${rc} calls=$(tr '\n' ' ' < "${d}/calls")"; fi

echo "TEST: naming a Surface with no app to launch is a no-op, not an error"
d="$(stub_target)"
out="$(HARNESS_CONFIG_JSON="$(cfg "${d}" "${HERMETIC}" "${WEB_SURFACE}")" bash "${LIB}" up --pr 5 --surface web 2>/dev/null)"; rc=$?
if [ "${rc}" -eq 0 ] && [ "${out}" = "WEB_URL=http://web.test" ]; then pass "up: a browser Surface is not launched here"; else fail "up: a browser Surface is not launched here" "rc=${rc} out=${out}"; fi

echo "TEST: a launched app puts its sandbox verdict in the block, for the round's result line (AC 4)"
d="$(stub_target)"
printf 'WEB_URL=http://web.test\nPANEL_URL=http://panel.test\n' > "${d}/block"
app_target="${WORK}/app-target"; mkdir -p "${app_target}/.auto-agent" "${app_target}/verify"
cp "${d}/provider" "${app_target}/provider"; cp "${d}/block" "${app_target}/block"
cat > "${app_target}/verify/app" <<'APP'
#!/usr/bin/env bash
sleep 30
APP
chmod +x "${app_target}/verify/app"
printf '1\n' > "${WORK}/userns-on"; printf '/usr/bin/other (enforce)\n' > "${WORK}/profiles-other"
out="$(HARNESS_CONFIG_JSON="$(cfg "${app_target}" "${HERMETIC}" "${two}")" \
    SURFACE_LAUNCH_RUN_DIR="${WORK}/run2" SURFACE_LAUNCH_PROBE_CMD=true SURFACE_LAUNCH_INTERVAL=0 \
    DISPLAY=:99 DISPLAY_PROBE_CMD=true \
    DISPLAY_USERNS_RESTRICT_FILE="${WORK}/userns-on" DISPLAY_APPARMOR_PROFILES_FILE="${WORK}/profiles-other" \
    bash "${LIB}" up --pr 5 --surface panel 2>/dev/null)"; rc=$?
if [ "${rc}" -eq 0 ] && printf '%s' "${out}" | grep -q '^AUTO_AGENT_SANDBOX=DEGRADED$' &&
   printf '%s' "${out}" | grep -q '^AUTO_AGENT_SANDBOX_DETAIL=panel started with ELECTRON_DISABLE_SANDBOX=1'; then
    pass "up: the sandbox verdict is in the block"
else fail "up: the sandbox verdict is in the block" "rc=${rc} out=${out}"; fi

echo "TEST: down stops every launched app and then the environment, idempotently"
HARNESS_CONFIG_JSON="$(cfg "${app_target}" "${HERMETIC}" "${two}")" SURFACE_LAUNCH_RUN_DIR="${WORK}/run2" \
    bash "${LIB}" down --pr 5 >/dev/null 2>&1; rc1=$?
HARNESS_CONFIG_JSON="$(cfg "${app_target}" "${HERMETIC}" "${two}")" SURFACE_LAUNCH_RUN_DIR="${WORK}/run2" \
    bash "${LIB}" down --pr 5 >/dev/null 2>&1; rc2=$?
if [ "${rc1}" -eq 0 ] && [ "${rc2}" -eq 0 ] && [ ! -f "${WORK}/run2/panel.pid" ] && [ "$(tail -1 "${app_target}/calls")" = "down" ]; then
    pass "down: apps then environment, twice"
else fail "down: apps then environment, twice" "rc1=${rc1} rc2=${rc2}"; fi

echo "TEST: down in the Bootstrap state has nothing to tear down and says nothing else"
HARNESS_CONFIG_JSON="$(cfg "${app_target}" null "${WEB_SURFACE}")" SURFACE_LAUNCH_RUN_DIR="${WORK}/run3" \
    bash "${LIB}" down --pr 5 >/dev/null 2>&1
[ $? -eq 0 ] && pass "down: Bootstrap state" || fail "down: Bootstrap state"

echo "TEST: usage errors"
bash "${LIB}" up >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: --pr is required" || fail "usage: --pr is required"
bash "${LIB}" bogus --pr 1 >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: unknown subcommand" || fail "usage: unknown subcommand"

echo "TEST: the fixture Target Project's own environment boots, answers and tears down (AC 1)"
export FIXTURE_RUN_DIR="${WORK}/fixture-run"
block="$(bash "${LIB}" up --pr 91 "${FIX}" 2>"${WORK}/err")"; rc=$?
url="$(printf '%s\n' "${block}" | sed -n 's/^FIXTURE_WEB_URL=//p')"
if [ "${rc}" -eq 0 ] && [ -n "${url}" ] && curl -fsS "${url}/api/health" >/dev/null 2>&1; then
    pass "fixture: the browser Surface is reachable at the key the block carried"
else fail "fixture: the browser Surface is reachable at the key the block carried" "rc=${rc} block=${block} err=$(cat "${WORK}/err")"; fi
bash "${LIB}" down --pr 91 "${FIX}" >/dev/null 2>&1
if [ -n "${url}" ] && ! curl -fsS "${url}/api/health" >/dev/null 2>&1; then pass "fixture: torn down"; else fail "fixture: torn down"; fi

echo "TEST: --head reads the checkout's config, not an inherited one (ADR 0007)"
# The inherited config (what a Fire exports, resolved from the default branch)
# says Bootstrap state; the checkout declares a provider, and the round obeys
# the checkout — this is how a PR that ADDS a provider is verified by it.
block="$(HARNESS_CONFIG_JSON="$(cfg "${FIX}" null '{}')" bash "${LIB}" up --pr 92 --head "${FIX}" 2>/dev/null)"; rc=$?
if [ "${rc}" -eq 0 ] && printf '%s' "${block}" | grep -q '^FIXTURE_WEB_URL='; then pass "--head: the checkout's provider is booted"; else fail "--head: the checkout's provider is booted" "rc=${rc} block=${block}"; fi
HARNESS_CONFIG_JSON="$(cfg "${FIX}" null '{}')" bash "${LIB}" up --pr 92 "${FIX}" >/dev/null 2>&1
if [ $? -eq 3 ]; then pass "without --head: the inherited config still wins (Bootstrap state)"; else fail "without --head: the inherited config still wins (Bootstrap state)"; fi
bash "${LIB}" down --pr 92 "${FIX}" >/dev/null 2>&1

echo "TEST: a whole round's seams run over the fixture's browser Surface (AC 1)"
# The capture itself is the verifier's job, through the Surface's MCP browser;
# here it is a stub that writes the file the round named, so the chain the
# round is made of — boot, tour Surfaces, evidence naming, shots, injection,
# result lines, teardown — is exercised end to end with no browser.
AA="${ROOT}/bin/auto-agent"
export AUTO_AGENT_STATE_DIR="${WORK}/round-state"
block="$(bash "${LIB}" up --pr 93 "${FIX}" 2>/dev/null)"
while IFS='=' read -r k v; do [ -n "${k}" ] && export "${k}=${v}"; done <<<"${block}"
tour="$(printf 'app/server.py\n' | bash "${AA}" surfaces tour "${FIX}")"
view="$(bash "${AA}" surfaces viewport "${tour}" "${FIX}")"
adir="$(bash "${AA}" evidence dir --pr 93 --round 1)"
shot="$(bash "${AA}" evidence name "${tour}" 1 item-list)"
# "capture": put the Surface in the state a reviewer would want to see, prove
# it really answers at the key the block carried, then write the round's
# artifact under the name the sink gave it.
curl -fsS -X POST -d brisket "${FIXTURE_API_URL}/items" >/dev/null
curl -fsS "${FIXTURE_WEB_URL}/" > "${adir}/page.html" && printf 'PNG-stub %s\n' "${view}" > "${adir}/${shot}"
printf '## Manual verification\n\n- [ ] the item list shows every posted item\n' > "${WORK}/round-body.md"
printf 'the item list shows every posted item\n' | bash "${AA}" checklist tick "${WORK}/round-body.md" > "${WORK}/round-ticked.md"
bash "${AA}" evidence shots "${adir}" | bash "${AA}" evidence inject "${WORK}/round-ticked.md" > "${WORK}/round-final.md"
bash "${LIB}" down --pr 93 "${FIX}" >/dev/null 2>&1
if [ "${tour}" = "web" ] && [ "${view}" = "1024x768" ] && [ "${shot}" = "web-01-item-list.png" ] &&
   grep -q '<li>' "${adir}/page.html" &&
   grep -q -- '- \[x\] the item list shows every posted item' "${WORK}/round-final.md" &&
   grep -q '^## Screenshots$' "${WORK}/round-final.md" &&
   grep -q "web-01-item-list.png" "${WORK}/round-final.md"; then
    pass "round: tour Surface, viewport, named shot, ticked box and the tour in the body"
else
    fail "round: tour Surface, viewport, named shot, ticked box and the tour in the body" "tour=${tour} view=${view} shot=${shot}"
fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
