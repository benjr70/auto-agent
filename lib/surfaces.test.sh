#!/usr/bin/env bash
# Tests for lib/surfaces.sh
#
# Run: bash lib/surfaces.test.sh
#
# Strategy: the Surface declaration is the only input, so every case is a
# synthetic config (HARNESS_CONFIG_JSON — no gh, no git remote) plus a list of
# changed paths. The cases that matter are the ones a round bets on: a glob
# that must not over-match, the kinds that always earn a tour (AC 2), and the
# viewport coming from the declaration rather than from the verifier (AC 3).
# One case runs against the real fixture Target Project.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/surfaces.sh"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env

# cfg <surfaces-json> : the resolved config the lib reads
cfg() {
    jq -cn --argjson s "$1" \
        '{config_dir: "/nowhere/.auto-agent",
          repo: {owner: "acme", name: "widgets", slug: "acme/widgets", default_branch: "trunk"},
          pick: {shape: "labels", project: null, labels: {}},
          surfaces: $s}'
}

SURFACES='{
  "web":   {"kind": "browser",  "url_key": "WEB_URL",  "paths": ["apps/web/src/**", "apps/web/public/"], "viewport": "427x952", "launcher": null},
  "panel": {"kind": "electron", "url_key": "PANEL_URL","paths": ["apps/panel/**"], "viewport": null, "launcher": "verify/panel"},
  "cli":   {"kind": "cli",      "url_key": "CLI_BIN",  "paths": ["cmd/*.go"], "viewport": null, "launcher": null},
  "api":   {"kind": "api",      "url_key": "API_URL",  "paths": ["services/api/**"], "viewport": null, "launcher": null}
}'

# run <sub> [args...] < paths : the lib as the CLI, stdout captured
run() {
    HARNESS_CONFIG_JSON="$(cfg "${SURFACES}")" bash "${LIB}" "$@"
}

echo "TEST: list prints every Surface with its resolved viewport and launcher"
out="$(run list </dev/null)"
want='api	api	API_URL	1280x800	-
cli	cli	CLI_BIN	1280x800	-
panel	electron	PANEL_URL	1280x800	verify/panel
web	browser	WEB_URL	427x952	-'
if [ "${out}" = "${want}" ]; then pass "list"; else fail "list" "got: ${out}"; fi

echo "TEST: touched names every Surface whose globs the diff matches (AC 3)"
out="$(printf '%s\n' 'apps/web/src/app.tsx' 'services/api/main.go' | run touched)"
if [ "${out}" = "$(printf 'api\nweb')" ]; then pass "touched: two Surfaces"; else fail "touched: two Surfaces" "got: ${out}"; fi

echo "TEST: a diff touching nothing declared names no Surface"
out="$(printf '%s\n' 'README.md' 'docs/adr/0001.md' | run touched)"
if [ -z "${out}" ]; then pass "touched: empty is an answer"; else fail "touched: empty is an answer" "got: ${out}"; fi

echo "TEST: * stops at a slash, ** crosses directories"
out="$(printf '%s\n' 'cmd/sub/deep.go' | run touched)"
if [ -z "${out}" ]; then pass "glob: cmd/*.go does not match cmd/sub/deep.go"; else fail "glob: cmd/*.go does not match cmd/sub/deep.go" "got: ${out}"; fi
out="$(printf '%s\n' 'cmd/main.go' | run touched)"
if [ "${out}" = "cli" ]; then pass "glob: cmd/*.go matches cmd/main.go"; else fail "glob: cmd/*.go matches cmd/main.go" "got: ${out}"; fi
out="$(printf '%s\n' 'apps/panel/src/deep/main.ts' | run touched)"
if [ "${out}" = "panel" ]; then pass "glob: ** crosses directories"; else fail "glob: ** crosses directories" "got: ${out}"; fi

echo "TEST: a trailing slash means everything beneath the directory"
out="$(printf '%s\n' 'apps/web/public/logo.svg' | run touched)"
if [ "${out}" = "web" ]; then pass "glob: trailing slash"; else fail "glob: trailing slash" "got: ${out}"; fi

echo "TEST: a leading ./ on a changed path is ignored"
out="$(printf '%s\n' './apps/web/src/app.tsx' | run touched)"
if [ "${out}" = "web" ]; then pass "glob: leading ./"; else fail "glob: leading ./" "got: ${out}"; fi

echo "TEST: a dot in a glob is a dot, not 'any character'"
dotted='{"one": {"kind": "browser", "url_key": "U", "paths": ["app/v1.2/**"], "viewport": null, "launcher": null}}'
out="$(printf '%s\n' 'app/v1X2/index.tsx' | HARNESS_CONFIG_JSON="$(cfg "${dotted}")" bash "${LIB}" touched)"
if [ -z "${out}" ]; then pass "glob: . is literal"; else fail "glob: . is literal" "got: ${out}"; fi

echo "TEST: browser and electron Surfaces earn a tour when touched; cli and api never do (AC 2)"
out="$(printf '%s\n' 'apps/web/src/app.tsx' 'apps/panel/main.ts' 'cmd/main.go' 'services/api/main.go' | run tour)"
if [ "${out}" = "$(printf 'panel\nweb')" ]; then pass "tour: UI kinds only"; else fail "tour: UI kinds only" "got: ${out}"; fi

echo "TEST: a touched cli or api Surface alone earns no tour, and that is not an error"
out="$(printf '%s\n' 'cmd/main.go' | run tour)"; rc=$?
if [ "${rc}" -eq 0 ] && [ -z "${out}" ]; then pass "tour: evidence-only kinds"; else fail "tour: evidence-only kinds" "rc=${rc} got: ${out}"; fi

echo "TEST: the viewport comes from the declaration, with one default for a Surface that declares none (AC 3)"
if [ "$(run viewport web)" = "427x952" ]; then pass "viewport: declared"; else fail "viewport: declared" "got: $(run viewport web)"; fi
if [ "$(run viewport panel)" = "1280x800" ]; then pass "viewport: default"; else fail "viewport: default" "got: $(run viewport panel)"; fi
out="$(SURFACES_DEFAULT_VIEWPORT=800x480 HARNESS_CONFIG_JSON="$(cfg "${SURFACES}")" bash "${LIB}" viewport panel)"
if [ "${out}" = "800x480" ]; then pass "viewport: the default is overridable"; else fail "viewport: the default is overridable" "got: ${out}"; fi

echo "TEST: an undeclared Surface is exit 1 with a message, never a made-up shape"
err="$(run viewport nope 2>&1 >/dev/null)"; rc=$?
if [ "${rc}" -eq 1 ] && printf '%s' "${err}" | grep -q "no Surface 'nope'"; then pass "viewport: unknown Surface"; else fail "viewport: unknown Surface" "rc=${rc} err=${err}"; fi

echo "TEST: an unknown subcommand and a missing name are usage errors"
bash "${LIB}" bogus >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: unknown subcommand" || fail "usage: unknown subcommand"
HARNESS_CONFIG_JSON="$(cfg "${SURFACES}")" bash "${LIB}" viewport >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: viewport needs a name" || fail "usage: viewport needs a name"

echo "TEST: --pr reads the changed paths from the PR itself"
stub="$(mktemp -d)"
cat > "${stub}/gh" <<'GH'
#!/usr/bin/env bash
[ "$1" = "pr" ] && [ "$2" = "diff" ] && [ "$3" = "31" ] && [ "$4" = "--name-only" ] || exit 9
printf 'apps/web/src/app.tsx\n'
GH
chmod +x "${stub}/gh"
out="$(GH_BIN="${stub}/gh" HARNESS_CONFIG_JSON="$(cfg "${SURFACES}")" bash "${LIB}" tour --pr 31 </dev/null)"
if [ "${out}" = "web" ]; then pass "--pr: paths from gh pr diff"; else fail "--pr: paths from gh pr diff" "got: ${out}"; fi
rm -rf "${stub}"

echo "TEST: the fixture Target Project's own Surfaces answer (AC 1's Surface half)"
out="$(printf 'app/server.py\n' | bash "${LIB}" tour "${ROOT}/plugin/fixtures/target-project")"
if [ "${out}" = "web" ]; then pass "fixture: the browser Surface earns the tour"; else fail "fixture: the browser Surface earns the tour" "got: ${out}"; fi
out="$(bash "${LIB}" viewport web "${ROOT}/plugin/fixtures/target-project")"
if [ "${out}" = "1024x768" ]; then pass "fixture: the declared viewport"; else fail "fixture: the declared viewport" "got: ${out}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
