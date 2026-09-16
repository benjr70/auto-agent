#!/usr/bin/env bash
# Tests for lib/harness-config.sh
#
# Run: bash lib/harness-config.test.sh
#
# Strategy: every test drives a public seam (harness_config_validate,
# harness_config_check, harness_config_load, bin/auto-agent check-config) and
# asserts the observable output: exit code, the resolved JSON, the error text.
# `gh` and `git` go through injected GH_BIN / GIT_BIN stubs so no test touches
# the network. The fixture Target Project in the plugin is the happy-path input.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
FIXTURE="${ROOT_DIR}/plugin/fixtures/target-project"
# shellcheck source=harness-config.sh
. "${SCRIPT_DIR}/harness-config.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# make_target [<harness.json content>] -> dir
# A throwaway Target Project: a copy of the fixture's .auto-agent with the
# given harness.json (default: the fixture's), plus git/gh stubs.
make_target() {
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/.auto-agent"
    cp "${FIXTURE}/.auto-agent/verifier-runbook.md" "${dir}/.auto-agent/"
    if [ $# -gt 0 ]; then printf '%s\n' "$1" > "${dir}/.auto-agent/harness.json"
    else cp "${FIXTURE}/.auto-agent/harness.json" "${dir}/.auto-agent/harness.json"; fi
    cat > "${dir}/git-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/calls.log"
case "\$*" in
    *"remote get-url origin"*) cat "${dir}/origin.out"; exit \$(cat "${dir}/origin.code") ;;
    *) echo "git-stub: unmatched: \$*" >&2; exit 1 ;;
esac
STUB
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/calls.log"
case "\$*" in
    *"repo view"*) cat "${dir}/branch.out"; exit \$(cat "${dir}/branch.code") ;;
    *) echo "gh-stub: unmatched: \$*" >&2; exit 1 ;;
esac
STUB
    chmod +x "${dir}/git-stub" "${dir}/gh-stub"
    echo 'https://github.com/acme/widgets.git' > "${dir}/origin.out"; echo 0 > "${dir}/origin.code"
    echo 'trunk' > "${dir}/branch.out"; echo 0 > "${dir}/branch.code"
    : > "${dir}/calls.log"
    echo "${dir}"
}

# load <dir> -> stdout JSON; stderr to <dir>/err; echoes exit code in LOAD_RC
load() {
    local dir="$1"
    LOAD_OUT="$(GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-stub" harness_config_load "${dir}" 2>"${dir}/err")"
    LOAD_RC=$?
}

VALID_MIN='{"commit_scopes":["core"],"commands":{"install":"npm ci","test":"npm test"},"pick":{"project":{"number":7}}}'

echo "harness_config_validate"

t="fixture harness.json validates"
if harness_config_validate "${FIXTURE}/.auto-agent/harness.json" 2>/dev/null; then pass "$t"; else fail "$t"; fi

t="missing file exits 2 with a message"
d="$(make_target)"; rm "${d}/.auto-agent/harness.json"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 2 ] && grep -q "missing" "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

t="non-JSON file exits 2"
d="$(make_target 'not json')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 2 ] && grep -q "not valid JSON" "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc"; fi

t="missing required block fails closed naming the key"
d="$(make_target '{"commit_scopes":["a"],"pick":{"labels":{}}}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q 'missing required key "commands"' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

t="unknown top-level key is rejected"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"model":"opus"}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q 'unknown key "model"' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

t="a declared default branch is rejected (detected, never read)"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"default_branch":"main"}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q 'unknown key "default_branch"' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc"; fi

t="pick with both shapes is rejected"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{},"project":{"number":1}}}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/pick: matches more than one shape' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

t="pick with neither shape is rejected"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{}}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/pick: matches none' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

t="a sixth round cap is rejected; a zero cap is rejected"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"poll":5,"revise":0}}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/rounds: unknown key "poll"' "${d}/err" && grep -q '/rounds/revise: must be >= 1' "${d}/err"; then pass "$t"; else fail "$t" "err=$(cat "${d}/err")"; fi

t="surface with a bad kind, lowercase url_key and no paths lists every error"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"surfaces":{"ui":{"kind":"tv","url_key":"url","paths":[]}}}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/surfaces/ui/kind: must be one of' "${d}/err" && grep -q '/surfaces/ui/url_key' "${d}/err" && grep -q '/surfaces/ui/paths: needs at least 1' "${d}/err"; then pass "$t"; else fail "$t" "err=$(cat "${d}/err")"; fi

t="hermetic block without a command is rejected; deployed enabled must be boolean"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"verification":{"hermetic":{"smoke":true},"deployed":{"command":"c","enabled":"yes"}}}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/verification/hermetic: missing required key "command"' "${d}/err" && grep -q '/verification/deployed/enabled: expected boolean' "${d}/err"; then pass "$t"; else fail "$t" "err=$(cat "${d}/err")"; fi

t="optional lane blocks accept enabled and nothing else"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"dependabot":{"enabled":false,"login":"x"}}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/dependabot: unknown key "login"' "${d}/err"; then pass "$t"; else fail "$t" "err=$(cat "${d}/err")"; fi

echo "harness_config_check"

t="electron surface without a launcher fails the cross-checks"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"surfaces":{"app":{"kind":"electron","url_key":"APP_URL","paths":["src/**"]}}}')"
harness_config_check "${d}" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/surfaces/app: an electron surface needs a launcher' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

t="deployed tier without a hermetic tier fails the cross-checks"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"verification":{"deployed":{"command":"c"}}}')"
harness_config_check "${d}" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/verification/deployed: a deployed tier needs a hermetic tier' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

t="non-executable host-extension fails the check"
d="$(make_target)"; : > "${d}/.auto-agent/host-extension"
harness_config_check "${d}" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q 'not executable' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

t="check makes no git or gh call"
d="$(make_target)"
GIT_BIN="${d}/git-stub" GH_BIN="${d}/gh-stub" harness_config_check "${d}" 2>/dev/null
if [ ! -s "${d}/calls.log" ]; then pass "$t"; else fail "$t" "calls: $(cat "${d}/calls.log")"; fi

echo "harness_config_load"

t="fixture config loads to the documented resolved shape"
d="$(make_target)"; load "${d}"
expected_keys='["commands","commit_scopes","config_dir","docs_research_prefix","host","lanes","pick","prose","repo","required_checks","rounds","surfaces","verification"]'
if [ $LOAD_RC -eq 0 ] && [ "$(printf '%s' "${LOAD_OUT}" | jq -c 'keys')" = "${expected_keys}" ]; then pass "$t"; else fail "$t" "rc=$LOAD_RC keys=$(printf '%s' "${LOAD_OUT}" | jq -c 'keys' 2>/dev/null) err=$(cat "${d}/err")"; fi

t="repo is derived from origin, default branch from gh"
if [ "$(printf '%s' "${LOAD_OUT}" | jq -c '.repo')" = '{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"}' ]; then pass "$t"; else fail "$t" "$(printf '%s' "${LOAD_OUT}" | jq -c '.repo')"; fi

t="gh was asked for the default branch of the derived slug"
if grep -q 'repo view acme/widgets --json defaultBranchRef' "${d}/calls.log"; then pass "$t"; else fail "$t" "$(cat "${d}/calls.log")"; fi

t="label-only pick resolves to the labels shape"
if [ "$(printf '%s' "${LOAD_OUT}" | jq -c '.pick')" = '{"shape":"labels","project":null,"labels":{}}' ]; then pass "$t"; else fail "$t" "$(printf '%s' "${LOAD_OUT}" | jq -c '.pick')"; fi

t="hermetic and deployed tiers resolve; deployed lane present but disabled; deps-land absent"
got="$(printf '%s' "${LOAD_OUT}" | jq -c '[.verification, .lanes]')"
want='[{"hermetic":{"command":"verify/provider","smoke":true},"deployed":{"command":"verify/provider","enabled":false}},{"deps_land":{"present":false,"enabled":false},"deployed":{"present":true,"enabled":false}}]'
if [ "${got}" = "${want}" ]; then pass "$t"; else fail "$t" "${got}"; fi

t="surfaces keep their viewport and get null launcher; api surface gets no viewport"
got="$(printf '%s' "${LOAD_OUT}" | jq -c '[.surfaces.web.viewport, .surfaces.web.launcher, .surfaces.api.viewport, (.surfaces|keys)]')"
if [ "${got}" = '["1024x768",null,null,["api","web"]]' ]; then pass "$t"; else fail "$t" "${got}"; fi

t="prose siblings resolve to absolute paths or null; host extension null when absent"
got="$(printf '%s' "${LOAD_OUT}" | jq -c '[(.prose.verifier_runbook|type), .prose.bot_pr_checklist, .prose.deployed_checks, .host]')"
if [ "${got}" = '["string",null,null,{"docker":false,"extension":null}]' ] && [ "$(printf '%s' "${LOAD_OUT}" | jq -r .prose.verifier_runbook)" = "${d}/.auto-agent/verifier-runbook.md" ]; then pass "$t"; else fail "$t" "${got}"; fi

t="fixture round caps and commands pass through"
got="$(printf '%s' "${LOAD_OUT}" | jq -c '[.rounds, .commands.lint, .commands.lockfile_refresh, .commands.plan_gated_paths, .docs_research_prefix, .required_checks]')"
want='[{"pr_watch":10,"manual_verify":3,"revise":3,"deps_fix":3,"pause_resume":3},"python3 -m compileall -q app",null,[],"docs/research/",[]]'
if [ "${got}" = "${want}" ]; then pass "$t"; else fail "$t" "${got}"; fi

t="Project pick shape resolves with defaults; minimal config gets every default"
d="$(make_target "${VALID_MIN}")"; load "${d}"
got="$(printf '%s' "${LOAD_OUT}" | jq -c '[.pick, .rounds, .verification, .surfaces, .lanes.deps_land]')"
want='[{"shape":"project","project":{"number":7,"priority_field":"Priority","order":["P0","P1","P2"]},"labels":null},{"pr_watch":10,"manual_verify":3,"revise":3,"deps_fix":3,"pause_resume":3},{"hermetic":null,"deployed":null},{},{"present":false,"enabled":false}]'
if [ $LOAD_RC -eq 0 ] && [ "${got}" = "${want}" ]; then pass "$t"; else fail "$t" "rc=$LOAD_RC ${got} err=$(cat "${d}/err")"; fi

t="Project pick keeps a declared priority field and order"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"project":{"number":3,"priority_field":"Urgency","order":["now","later"]}}}')"; load "${d}"
got="$(printf '%s' "${LOAD_OUT}" | jq -c '.pick.project')"
if [ "${got}" = '{"number":3,"priority_field":"Urgency","order":["now","later"]}' ]; then pass "$t"; else fail "$t" "${got}"; fi

t="labels pick rejects keys of its own (no configurable label vocabulary)"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{"priority":["P0"]}}}')"
harness_config_validate "${d}/.auto-agent/harness.json" 2>"${d}/err"; rc=$?
if [ $rc -eq 1 ] && grep -q '/pick/labels: unknown key "priority"' "${d}/err"; then pass "$t"; else fail "$t" "err=$(cat "${d}/err")"; fi

t="dependabot block present with default enabled turns the deps-land lane on"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"dependabot":{}}')"; load "${d}"
if [ "$(printf '%s' "${LOAD_OUT}" | jq -c '.lanes.deps_land')" = '{"present":true,"enabled":true}' ]; then pass "$t"; else fail "$t"; fi

t="viewport is null when not declared; launcher passes through"
d="$(make_target '{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"surfaces":{"web":{"kind":"browser","url_key":"W","paths":["a"]},"app":{"kind":"electron","url_key":"A","paths":["b"],"launcher":"bin/app"}}}')"; load "${d}"
got="$(printf '%s' "${LOAD_OUT}" | jq -c '[.surfaces.web.viewport, .surfaces.app.viewport, .surfaces.app.launcher]')"
if [ "${got}" = '[null,null,"bin/app"]' ]; then pass "$t"; else fail "$t" "${got}"; fi

t="executable host-extension resolves to its path"
d="$(make_target)"; printf '#!/bin/sh\ntrue\n' > "${d}/.auto-agent/host-extension"; chmod +x "${d}/.auto-agent/host-extension"; load "${d}"
if [ "$(printf '%s' "${LOAD_OUT}" | jq -r '.host.extension')" = "${d}/.auto-agent/host-extension" ]; then pass "$t"; else fail "$t"; fi

t="invalid file fails before any git or gh call"
d="$(make_target '{"pick":{"labels":{}}}')"; load "${d}"
if [ $LOAD_RC -eq 1 ] && [ ! -s "${d}/calls.log" ] && grep -q 'missing required key' "${d}/err"; then pass "$t"; else fail "$t" "rc=$LOAD_RC calls=$(cat "${d}/calls.log")"; fi

t="no origin remote exits 3 with a message"
d="$(make_target)"; echo 1 > "${d}/origin.code"; : > "${d}/origin.out"; load "${d}"
if [ $LOAD_RC -eq 3 ] && grep -q 'origin remote' "${d}/err"; then pass "$t"; else fail "$t" "rc=$LOAD_RC err=$(cat "${d}/err")"; fi

t="gh failure exits 3 without emitting a config"
d="$(make_target)"; echo 1 > "${d}/branch.code"; load "${d}"
if [ $LOAD_RC -eq 3 ] && [ -z "${LOAD_OUT}" ] && grep -q 'default branch' "${d}/err"; then pass "$t"; else fail "$t" "rc=$LOAD_RC"; fi

t="ssh and bare-https origin URLs derive the same slug"
for url in 'git@github.com:acme/widgets.git' 'ssh://git@github.com/acme/widgets' 'https://github.com/acme/widgets'; do
    d="$(make_target)"; echo "${url}" > "${d}/origin.out"; load "${d}"
    [ "$(printf '%s' "${LOAD_OUT}" | jq -r '.repo.slug')" = 'acme/widgets' ] || { fail "$t" "url=${url} slug=$(printf '%s' "${LOAD_OUT}" | jq -r '.repo.slug')"; continue 2; }
done; pass "$t"

t="a file from elsewhere loads against the target's repo (PR-head seam)"
d="$(make_target)"; printf '%s\n' "${VALID_MIN}" > "${d}/from-pr-head.json"
LOAD_OUT="$(GIT_BIN="${d}/git-stub" GH_BIN="${d}/gh-stub" harness_config_load "${d}" "${d}/from-pr-head.json" 2>"${d}/err")"; rc=$?
if [ $rc -eq 0 ] && [ "$(printf '%s' "${LOAD_OUT}" | jq -r '.pick.shape')" = 'project' ] && [ "$(printf '%s' "${LOAD_OUT}" | jq -r '.repo.slug')" = 'acme/widgets' ]; then pass "$t"; else fail "$t" "rc=$rc"; fi

echo "bin/auto-agent check-config"

t="check-config passes on the fixture"
out="$("${ROOT_DIR}/bin/auto-agent" check-config "${FIXTURE}" 2>&1)"; rc=$?
if [ $rc -eq 0 ] && printf '%s' "${out}" | grep -q '^ok:'; then pass "$t"; else fail "$t" "rc=$rc ${out}"; fi

t="check-config fails on a fixture with a broken block"
d="$(make_target)"; jq '.verification.hermetic = {"smoke": "yes"}' "${FIXTURE}/.auto-agent/harness.json" > "${d}/.auto-agent/harness.json"
out="$("${ROOT_DIR}/bin/auto-agent" check-config "${d}" 2>&1)"; rc=$?
if [ $rc -eq 1 ] && printf '%s' "${out}" | grep -q '/verification/hermetic: missing required key "command"' && printf '%s' "${out}" | grep -q '/verification/hermetic/smoke: expected boolean'; then pass "$t"; else fail "$t" "rc=$rc ${out}"; fi

t="check-config without a target exits 2"
"${ROOT_DIR}/bin/auto-agent" check-config >/dev/null 2>&1; rc=$?
if [ $rc -eq 2 ]; then pass "$t"; else fail "$t" "rc=$rc"; fi

t="unknown command exits 2"
"${ROOT_DIR}/bin/auto-agent" frobnicate >/dev/null 2>&1; rc=$?
if [ $rc -eq 2 ]; then pass "$t"; else fail "$t" "rc=$rc"; fi

t="show-config prints the resolved JSON"
d="$(make_target)"
out="$(GIT_BIN="${d}/git-stub" GH_BIN="${d}/gh-stub" "${ROOT_DIR}/bin/auto-agent" show-config "${d}" 2>/dev/null)"; rc=$?
if [ $rc -eq 0 ] && [ "$(printf '%s' "${out}" | jq -r '.repo.default_branch')" = 'trunk' ]; then pass "$t"; else fail "$t" "rc=$rc"; fi

echo "harness_config_resolve"

t="HARNESS_CONFIG_JSON wins and needs no target, no git, no gh"
d="$(make_target)"
out="$(HARNESS_CONFIG_JSON='{"repo":{"slug":"x/y"},"pick":{"shape":"labels"}}' GIT_BIN=/nonexistent GH_BIN=/nonexistent harness_config_resolve 2>/dev/null)"; rc=$?
if [ $rc -eq 0 ] && [ "${out}" = '{"repo":{"slug":"x/y"},"pick":{"shape":"labels"}}' ]; then pass "$t"; else fail "$t" "rc=$rc out=${out}"; fi

t="a HARNESS_CONFIG_JSON that is not a resolved config is refused (exit 2)"
out="$(HARNESS_CONFIG_JSON='{"commit_scopes":[]}' harness_config_resolve 2>/dev/null)"; rc=$?
if [ $rc -eq 2 ] && [ -z "${out}" ]; then pass "$t"; else fail "$t" "rc=$rc out=${out}"; fi

t="a target dir loads through the loader"
out="$(HARNESS_CONFIG_JSON= GIT_BIN="${d}/git-stub" GH_BIN="${d}/gh-stub" harness_config_resolve "${d}" 2>/dev/null)"; rc=$?
if [ $rc -eq 0 ] && [ "$(printf '%s' "${out}" | jq -r '.repo.default_branch')" = 'trunk' ]; then pass "$t"; else fail "$t" "rc=$rc"; fi

t="AUTO_AGENT_TARGET_DIR is the fallback target"
out="$(HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR="${d}" GIT_BIN="${d}/git-stub" GH_BIN="${d}/gh-stub" harness_config_resolve 2>/dev/null)"; rc=$?
if [ $rc -eq 0 ] && [ "$(printf '%s' "${out}" | jq -r '.repo.slug')" = 'acme/widgets' ]; then pass "$t"; else fail "$t" "rc=$rc"; fi

t="nothing set: exit 2 with a stderr line"
out="$(HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= harness_config_resolve 2>"${d}/err")"; rc=$?
if [ $rc -eq 2 ] && grep -q 'no Target Project' "${d}/err"; then pass "$t"; else fail "$t" "rc=$rc err=$(cat "${d}/err")"; fi

echo ""
echo "fixed vocabulary helpers"
t="harness_merge_recipe prints the one admin-squash command, pinned to the sha"
out="$(harness_merge_recipe acme/widgets 590 abc123)"
if [ "${out}" = "gh pr merge 590 --repo acme/widgets --squash --admin --match-head-commit abc123" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

t="harness_config_target_dir is the parent of config_dir"
out="$(harness_config_target_dir '{"config_dir":"/srv/target/.auto-agent"}')"
if [ "${out}" = "/srv/target" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

t="harness_re_escape neutralises every ERE metacharacter"
out="$(printf 'a.b' | grep -cE "^$(harness_re_escape 'a.b')$")"; out2="$(printf 'axb' | grep -cE "^$(harness_re_escape 'a.b')$")"
if [ "${out}" = "1" ] && [ "${out2}" = "0" ]; then pass "$t"; else fail "$t" "out=${out} out2=${out2}"; fi

t="harness_config_slug prints the resolved slug and names its caller on failure"
out="$(HARNESS_CONFIG_JSON='{"repo":{"slug":"x/y"},"pick":{"shape":"labels"}}' harness_config_slug 2>/dev/null)"; rc=$?
out2="$(HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= harness_config_slug my-lib 2>&1 >/dev/null)"; rc2=$?
if [ $rc -eq 0 ] && [ "${out}" = "x/y" ] && [ $rc2 -eq 2 ] && [ "${out2}" = "my-lib: no Harness config to read the repo from" ]; then pass "$t"; else fail "$t" "rc=$rc out=${out} rc2=$rc2 out2=${out2}"; fi

t="harness_config_target_dir fails on a config without config_dir"
if ! harness_config_target_dir '{"repo":{}}' >/dev/null 2>&1; then pass "$t"; else fail "$t"; fi

echo ""
echo "fixed vocabulary is the only source of repo facts"
t="no lib spells a repo, a default branch or a research path literal outside comments (runbook-check.sh names them on purpose)"
hits="$(grep -nE 'benjr70|Smart-Smoker|origin/master|(^|[^A-Za-z_/-])(master|main)($|[^A-Za-z_-])|docs/research/' "${SCRIPT_DIR}"/*.sh \
    | grep -v '\.test\.sh:' | grep -v 'runbook-check\.sh:' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' || true)"
if [ -z "${hits}" ]; then pass "$t"; else fail "$t" "$(printf '%s' "${hits}" | head -5)"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
