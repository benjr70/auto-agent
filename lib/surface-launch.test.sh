#!/usr/bin/env bash
# Tests for lib/surface-launch.sh
#
# Run: bash lib/surface-launch.test.sh
#
# Strategy: the MCP server, the CDP probe and the Target Project's app are all
# stubs, so a round's wiring is observable without a browser, a display server
# or an Electron build: the stub MCP command records the arguments the kind
# chose, and the stub app records that it was launched with a debugging port
# and the Surface's URL in its environment. The cases are the ones a round
# cannot recover from by itself — a kind with no launcher, a Surface whose app
# the project never declared, a URL the provider's block did not carry, and the
# degraded sandbox report (AC 4).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/surface-launch.sh"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# A Target Project whose `verify/app` records its launch and then idles.
TARGET="${WORK}/target"
mkdir -p "${TARGET}/.auto-agent" "${TARGET}/verify"
cat > "${TARGET}/verify/app" <<'APP'
#!/usr/bin/env bash
{ echo "args: $*"; echo "PANEL_URL=${PANEL_URL:-}"; echo "DISPLAY=${DISPLAY:-}"; echo "SANDBOX_FLAG=${ELECTRON_DISABLE_SANDBOX:-unset}"; } > "${APP_RECORD}"
[ "${APP_EXITS:-0}" = 1 ] && exit 1
sleep 30
APP
chmod +x "${TARGET}/verify/app"

# A stub MCP server that records its arguments instead of exec'ing npx.
cat > "${WORK}/mcp" <<'MCP'
#!/usr/bin/env bash
printf '%s\n' "$*" > "${MCP_RECORD}"
MCP
chmod +x "${WORK}/mcp"

SURFACES='{
  "web":   {"kind": "browser",  "url_key": "WEB_URL",   "paths": ["web/**"],   "viewport": "427x952", "launcher": null},
  "panel": {"kind": "electron", "url_key": "PANEL_URL", "paths": ["panel/**"], "viewport": null, "launcher": "verify/app"},
  "bare":  {"kind": "electron", "url_key": "BARE_URL",  "paths": ["bare/**"],  "viewport": null, "launcher": null},
  "api":   {"kind": "api",      "url_key": "API_URL",   "paths": ["api/**"],   "viewport": null, "launcher": null}
}'

cfg() {
    jq -cn --arg dir "${TARGET}/.auto-agent" --argjson s "${SURFACES}" \
        '{config_dir: $dir,
          repo: {owner: "acme", name: "widgets", slug: "acme/widgets", default_branch: "trunk"},
          pick: {shape: "labels", project: null, labels: {}},
          surfaces: $s}'
}

# run <args...> : the lib as the CLI, against the stub Target Project. The
# display is declared and its probe stubbed, so the suite needs no display
# server; the cases that test the no-display path override it back.
run() {
    HARNESS_CONFIG_JSON="$(cfg)" \
    SURFACE_LAUNCH_MCP_CMD="${WORK}/mcp" \
    SURFACE_LAUNCH_RUN_DIR="${WORK}/run" \
    SURFACE_LAUNCH_INTERVAL=0 \
    DISPLAY="${DISPLAY:-:99}" DISPLAY_PROBE_CMD="${DISPLAY_PROBE_CMD:-true}" \
    bash "${LIB}" "$@"
}

echo "TEST: a browser Surface's MCP server is headful on the Host display, on a fresh profile, at the declared viewport"
MCP_RECORD="${WORK}/mcp-web" run mcp web >/dev/null 2>"${WORK}/err"
rec="$(cat "${WORK}/mcp-web" 2>/dev/null)"
if printf '%s' "${rec}" | grep -q -- '--browser chrome' &&
   printf '%s' "${rec}" | grep -q -- '--viewport-size 427,952' &&
   printf '%s' "${rec}" | grep -q -- "--user-data-dir ${WORK}/run/profiles/web-"; then
    pass "mcp browser: the kind chose the launcher"
else fail "mcp browser: the kind chose the launcher" "rec=${rec} err=$(cat "${WORK}/err")"; fi

echo "TEST: each run gets its own browser profile"
MCP_RECORD="${WORK}/mcp-web2" run mcp web >/dev/null 2>&1
a="$(grep -o -- '--user-data-dir [^ ]*' "${WORK}/mcp-web")"
b="$(grep -o -- '--user-data-dir [^ ]*' "${WORK}/mcp-web2")"
if [ -n "${a}" ] && [ "${a}" != "${b}" ]; then pass "mcp browser: a fresh profile per run"; else fail "mcp browser: a fresh profile per run" "a=${a} b=${b}"; fi

echo "TEST: a browser Surface with no display on the Host is exit 3, not a headless round"
HARNESS_CONFIG_JSON="$(cfg)" SURFACE_LAUNCH_MCP_CMD="${WORK}/mcp" SURFACE_LAUNCH_RUN_DIR="${WORK}/run" \
    env -u DISPLAY -u AUTO_AGENT_DISPLAY bash "${LIB}" mcp web >/dev/null 2>&1
[ $? -eq 3 ] && pass "mcp browser: no display" || fail "mcp browser: no display"

echo "TEST: an electron Surface's MCP server registers even before the app exists"
MCP_RECORD="${WORK}/mcp-panel" SURFACE_LAUNCH_PROBE_CMD=false SURFACE_LAUNCH_RETRIES=1 run mcp panel >/dev/null 2>"${WORK}/err"; rc=$?
rec="$(cat "${WORK}/mcp-panel" 2>/dev/null)"
if [ "${rc}" -eq 0 ] && printf '%s' "${rec}" | grep -q -- '--cdp-endpoint' && grep -q 'did not answer yet' "${WORK}/err"; then
    pass "mcp electron: registers anyway, warns"
else fail "mcp electron: registers anyway, warns" "rc=${rc} rec=${rec} err=$(cat "${WORK}/err")"; fi

echo "TEST: an evidence-only kind has no launcher, and says so"
err="$(run mcp api 2>&1 >/dev/null)"; rc=$?
if [ "${rc}" -eq 2 ] && printf '%s' "${err}" | grep -q 'evidence-only'; then pass "mcp: api is evidence-only"; else fail "mcp: api is evidence-only" "rc=${rc} err=${err}"; fi

echo "TEST: an undeclared Surface is a usage error, never a guess"
run mcp nope >/dev/null 2>&1; [ $? -eq 2 ] && pass "mcp: unknown Surface" || fail "mcp: unknown Surface"
run mcp >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: a Surface name is required" || fail "usage: a Surface name is required"
run bogus web >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: unknown subcommand" || fail "usage: unknown subcommand"

echo "TEST: mcp-config renders one server per UI Surface, and none for the evidence-only kinds"
out="$(run mcp-config 2>/dev/null)"
names="$(printf '%s' "${out}" | jq -r '.mcpServers | keys | join(",")')"
if [ "${names}" = "surface-bare,surface-panel,surface-web" ]; then pass "mcp-config: UI Surfaces only"; else fail "mcp-config: UI Surfaces only" "got: ${names}"; fi
if [ "$(printf '%s' "${out}" | jq -r '.mcpServers["surface-web"].args | join(" ")')" = "surface-launch mcp web ${TARGET}" ] &&
   [ "$(printf '%s' "${out}" | jq -r '.mcpServers["surface-web"].command')" = "$(cd "${SCRIPT_DIR}/.." && pwd)/bin/auto-agent" ]; then
    pass "mcp-config: each entry runs this CLI for that Surface"
else fail "mcp-config: each entry runs this CLI for that Surface" "got: ${out}"; fi

echo "TEST: mcp-config writes the registry to --out and prints the path"
path="$(run mcp-config --out "${WORK}/mcp.json" 2>/dev/null)"
if [ "${path}" = "${WORK}/mcp.json" ] && jq -e '.mcpServers | length == 3' "${WORK}/mcp.json" >/dev/null; then pass "mcp-config: --out"; else fail "mcp-config: --out" "path=${path}"; fi

echo "TEST: a Target Project with no UI Surface renders an empty registry"
only_api='{"api": {"kind": "api", "url_key": "API_URL", "paths": ["api/**"], "viewport": null, "launcher": null}}'
out="$(HARNESS_CONFIG_JSON="$(jq -cn --arg dir "${TARGET}/.auto-agent" --argjson s "${only_api}" \
    '{config_dir: $dir, repo: {owner: "acme", name: "widgets", slug: "acme/widgets", default_branch: "trunk"}, pick: {shape: "labels", project: null, labels: {}}, surfaces: $s}')" \
    bash "${LIB}" mcp-config 2>/dev/null)"
if [ "$(printf '%s' "${out}" | jq -r '.mcpServers | length')" = "0" ]; then pass "mcp-config: no UI Surface"; else fail "mcp-config: no UI Surface" "got: ${out}"; fi

echo "TEST: start launches the app with a debugging port, the Surface's URL and the Host display"
out="$(APP_RECORD="${WORK}/app-1" PANEL_URL=http://panel.test DISPLAY=:99 DISPLAY_PROBE_CMD=true \
    SURFACE_LAUNCH_PROBE_CMD=true DISPLAY_USERNS_RESTRICT_FILE="${WORK}/missing" \
    run start panel --pr 77 2>"${WORK}/err")"; rc=$?
rec="$(cat "${WORK}/app-1" 2>/dev/null)"
if [ "${rc}" -eq 0 ] &&
   printf '%s' "${rec}" | grep -q -- '--remote-debugging-port=' &&
   printf '%s' "${rec}" | grep -q 'PANEL_URL=http://panel.test' &&
   printf '%s' "${rec}" | grep -q 'DISPLAY=:99'; then
    pass "start: the app is launched wired to the environment"
else fail "start: the app is launched wired to the environment" "rc=${rc} rec=${rec} err=$(cat "${WORK}/err")"; fi
if printf '%s' "${out}" | grep -q '^sandbox: OK'; then pass "start: the sandbox line reports OK"; else fail "start: the sandbox line reports OK" "out=${out}"; fi

echo "TEST: stop is idempotent, on every exit path"
run stop panel >/dev/null 2>&1; rc1=$?
run stop panel >/dev/null 2>&1; rc2=$?
if [ "${rc1}" -eq 0 ] && [ "${rc2}" -eq 0 ] && [ ! -f "${WORK}/run/panel.pid" ]; then pass "stop: idempotent"; else fail "stop: idempotent" "rc1=${rc1} rc2=${rc2}"; fi

echo "TEST: a Host with no AppArmor profile for the app reports DEGRADED, and the round goes on (AC 4)"
printf '1\n' > "${WORK}/userns-on"
printf '/usr/bin/other (enforce)\n' > "${WORK}/profiles-other"
out="$(APP_RECORD="${WORK}/app-2" PANEL_URL=http://panel.test DISPLAY=:99 DISPLAY_PROBE_CMD=true \
    SURFACE_LAUNCH_PROBE_CMD=true DISPLAY_USERNS_RESTRICT_FILE="${WORK}/userns-on" \
    DISPLAY_APPARMOR_PROFILES_FILE="${WORK}/profiles-other" \
    run start panel --pr 77 2>/dev/null)"; rc=$?
rec="$(cat "${WORK}/app-2" 2>/dev/null)"
if [ "${rc}" -eq 0 ] && printf '%s' "${out}" | grep -q '^sandbox: DEGRADED — panel started with ELECTRON_DISABLE_SANDBOX=1' &&
   printf '%s' "${rec}" | grep -q 'SANDBOX_FLAG=1'; then
    pass "start: DEGRADED is reported, never silent"
else fail "start: DEGRADED is reported, never silent" "rc=${rc} out=${out} rec=${rec}"; fi
run stop panel >/dev/null 2>&1

echo "TEST: an app whose debugging endpoint never answers is exit 5, and leaves no process behind"
out="$(APP_RECORD="${WORK}/app-3" PANEL_URL=http://panel.test DISPLAY=:99 DISPLAY_PROBE_CMD=true \
    SURFACE_LAUNCH_PROBE_CMD=false SURFACE_LAUNCH_RETRIES=2 DISPLAY_USERNS_RESTRICT_FILE="${WORK}/missing" \
    run start panel --pr 77 2>/dev/null)"; rc=$?
if [ "${rc}" -eq 5 ] && [ ! -f "${WORK}/run/panel.pid" ]; then pass "start: the endpoint never answered"; else fail "start: the endpoint never answered" "rc=${rc}"; fi

echo "TEST: an electron Surface that declares no launcher is exit 4 with the reason"
err="$(BARE_URL=http://bare.test DISPLAY=:99 DISPLAY_PROBE_CMD=true run start bare --pr 77 2>&1 >/dev/null)"; rc=$?
if [ "${rc}" -eq 4 ] && printf '%s' "${err}" | grep -q 'declares no launcher'; then pass "start: no launcher declared"; else fail "start: no launcher declared" "rc=${rc} err=${err}"; fi

echo "TEST: a Surface whose url_key the provider's block did not carry is exit 4"
err="$(PANEL_URL='' run start panel --pr 77 2>&1 >/dev/null)"; rc=$?
if [ "${rc}" -eq 4 ] && printf '%s' "${err}" | grep -q 'PANEL_URL is not in the environment'; then pass "start: the url_key is missing"; else fail "start: the url_key is missing" "rc=${rc} err=${err}"; fi

echo "TEST: only an electron Surface has an app to launch"
err="$(run start web --pr 77 2>&1 >/dev/null)"; rc=$?
if [ "${rc}" -eq 2 ] && printf '%s' "${err}" | grep -q 'has no app to launch'; then pass "start: browser Surfaces are not launched"; else fail "start: browser Surfaces are not launched" "rc=${rc} err=${err}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
