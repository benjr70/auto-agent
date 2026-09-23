#!/usr/bin/env bash
# Tests for lib/bootstrap-state.sh
#
# Run: bash lib/bootstrap-state.test.sh
#
# Strategy, two layers:
#   1. the bootstrap path end to end over the real fixture Target Project with
#      its hermetic block removed (behaviour 1): the state verdict, a pickup
#      that proceeds anyway, and the one AFK ticket opened once and reused on
#      every re-run. gh is a recording stub, so every write the lib would make
#      is visible and none of them leaves this box;
#   2. the self-verifying provider PR (behaviour 2): a default-branch config in
#      the Bootstrap state plus a checkout that ADDS the provider, proving the
#      round booted from the head is verified by the provider the head carries
#      while the inherited config still says there is nothing to boot.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/bootstrap-state.sh"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLI="${ROOT}/bin/auto-agent"
FIX="${ROOT}/plugin/fixtures/target-project"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# cfg <hermetic-json> [<config-dir>] : a resolved config, the only input the lib reads
cfg() {
    jq -cn --argjson h "$1" --arg dir "${2:-/nowhere/.auto-agent}" \
        '{config_dir: $dir,
          repo: {owner: "acme", name: "widgets", slug: "acme/widgets", default_branch: "trunk"},
          pick: {shape: "labels", project: null, labels: {}},
          verification: {hermetic: $h, deployed: null},
          surfaces: {web: {kind: "browser", url_key: "WEB_URL", paths: ["app/**"], viewport: null, launcher: null}}}'
}

# gh_stub <dir> : a gh that records every call and answers `issue list` from
# <dir>/issues.json (the open issues of the Target Project)
gh_stub() {
    local dir="$1"
    echo '[]' > "${dir}/issues.json"
    echo 0 > "${dir}/create.code"
    cat > "${dir}/gh" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/gh.log"
case "\$1 \$2" in
  "issue list")
      # The lib passes --jq; apply it to the canned issue list.
      prev=''; prog='.'
      for a in "\$@"; do [ "\$prev" = "--jq" ] && prog="\$a"; prev="\$a"; done
      jq -r "\$prog" "${dir}/issues.json"
      ;;
  "issue create")
      rc=\$(cat "${dir}/create.code")
      [ "\$rc" -ne 0 ] && { echo "gh: boom" >&2; exit "\$rc"; }
      prev=''; body=''
      for a in "\$@"; do [ "\$prev" = "--body" ] && body="\$a"; prev="\$a"; done
      n=\$(( \$(jq 'length' "${dir}/issues.json") + 41 ))
      jq --argjson n "\$n" --arg b "\$body" '. + [{number: \$n, body: \$b}]' \\
          "${dir}/issues.json" > "${dir}/issues.tmp" && mv "${dir}/issues.tmp" "${dir}/issues.json"
      printf '%s\n' "\$body" > "${dir}/last-body.md"
      echo "https://github.com/acme/widgets/issues/\${n}"
      ;;
  *) exit 0 ;;
esac
STUB
    chmod +x "${dir}/gh"
}

#-------------------------------------------------------------------------------
echo "TEST: the state verdict is the one reading of 'no hermetic tier' (AC 1)"
out="$(HARNESS_CONFIG_JSON="$(cfg '{"command":"verify/provider","smoke":true}')" bash "${LIB}" state)"
if [ "$(printf '%s' "${out}" | jq -r .bootstrap)" = "false" ]; then pass "a declared hermetic tier is not the Bootstrap state"
else fail "a declared hermetic tier is not the Bootstrap state" "${out}"; fi

D="$(mktemp -d "${WORK}/gh-XXXXXX")"; gh_stub "${D}"
out="$(GH_BIN="${D}/gh" HARNESS_CONFIG_JSON="$(cfg null)" bash "${LIB}" state)"
if [ "$(printf '%s' "${out}" | jq -r .bootstrap)" = "true" ] \
   && [ "$(printf '%s' "${out}" | jq -r .issue)" = "null" ]; then pass "no hermetic tier is the Bootstrap state, with no ticket yet"
else fail "no hermetic tier is the Bootstrap state, with no ticket yet" "${out}"; fi

echo "TEST: the bootstrap ticket is opened once and reused on every re-run (AC 3)"
D="$(mktemp -d "${WORK}/issue-XXXXXX")"; gh_stub "${D}"
run_issue() { GH_BIN="${D}/gh" BOOTSTRAP_PICK_PUBLISH=/nonexistent \
    HARNESS_CONFIG_JSON="$(cfg null)" bash "${LIB}" issue "$@"; }
first="$(run_issue)"; rc=$?
second="$(run_issue)"
third="$(run_issue)"
creates="$(grep -c '^issue create' "${D}/gh.log" || true)"
if [ "${rc}" -eq 0 ] && [ "${first}" = "bootstrap: issue #41 created" ] \
   && [ "${second}" = "bootstrap: issue #41 reused" ] && [ "${third}" = "${second}" ] \
   && [ "${creates}" = "1" ]; then pass "created once, reused twice, one gh issue create"
else fail "created once, reused twice, one gh issue create" "1=${first} 2=${second} 3=${third} creates=${creates}"; fi

echo "TEST: the ticket is found by its body marker, not its title"
jq '.[0].body = "Someone rewrote this.\n<!-- auto-agent:bootstrap -->\nstill the one."' \
    "${D}/issues.json" > "${D}/t" && mv "${D}/t" "${D}/issues.json"
out="$(run_issue)"
if [ "${out}" = "bootstrap: issue #41 reused" ]; then pass "a retitled, rewritten ticket is still the one"
else fail "a retitled, rewritten ticket is still the one" "${out}"; fi

echo "TEST: the ticket names the contract and the reference providers"
if grep -q 'plugin/providers/CONTRACT.md' "${D}/last-body.md" \
   && grep -q 'plugin/providers/compose/provider' "${D}/last-body.md" \
   && grep -q '^## Acceptance criteria' "${D}/last-body.md" \
   && grep -q 'provider-check' "${D}/last-body.md"; then pass "the body sends the Daemon to the contract and the references"
else fail "the body sends the Daemon to the contract and the references" "$(cat "${D}/last-body.md")"; fi

echo "TEST: --dry-run reaches the same verdict and writes nothing"
D="$(mktemp -d "${WORK}/dry-XXXXXX")"; gh_stub "${D}"
out="$(run_issue --dry-run)"
if [ "${out}" = "bootstrap: would-open the bootstrap issue" ] && ! grep -q '^issue create' "${D}/gh.log"; then
    pass "dry-run: would-open, no write"
else fail "dry-run: would-open, no write" "${out} log=$(cat "${D}/gh.log")"; fi
run_issue >/dev/null
out="$(run_issue --dry-run)"
if [ "${out}" = "bootstrap: would-reuse issue #41" ]; then pass "dry-run: would-reuse the open one"
else fail "dry-run: would-reuse the open one" "${out}"; fi

echo "TEST: outside the Bootstrap state no ticket is owed and gh is never asked"
D="$(mktemp -d "${WORK}/owed-XXXXXX")"; gh_stub "${D}"
GH_BIN="${D}/gh" HARNESS_CONFIG_JSON="$(cfg '{"command":"verify/provider","smoke":true}')" \
    bash "${LIB}" issue >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 3 ] && [ ! -s "${D}/gh.log" ]; then pass "exit 3, no gh call"
else fail "exit 3, no gh call" "rc=${rc} log=$(cat "${D}/gh.log" 2>/dev/null)"; fi

echo "TEST: a gh failure is reported, never swallowed"
D="$(mktemp -d "${WORK}/ghfail-XXXXXX")"; gh_stub "${D}"; echo 1 > "${D}/create.code"
err="$(run_issue 2>&1 >/dev/null)"; rc=$?
if [ "${rc}" -eq 1 ] && printf '%s' "${err}" | grep -q 'could not open the bootstrap issue'; then pass "exit 1 with a reason"
else fail "exit 1 with a reason" "rc=${rc} err=${err}"; fi

echo "TEST: config-touched names the paths that change this PR's own verification (AC 4)"
run_touched() { HARNESS_CONFIG_JSON="$(cfg null "/repo/.auto-agent")" bash "${LIB}" config-touched; }
out="$(printf '%s\n' '.auto-agent/harness.json' 'app/server.py' './.auto-agent/verifier-runbook.md' | run_touched)"
want="$(printf '.auto-agent/harness.json\n.auto-agent/verifier-runbook.md')"
if [ "${out}" = "${want}" ]; then pass "every config path, sorted, nothing else"
else fail "every config path, sorted, nothing else" "got: ${out}"; fi
out="$(printf '%s\n' 'app/server.py' 'docs/adr/0003.md' 'notauto-agent/x' | run_touched)"
if [ -z "${out}" ]; then pass "a PR that touches no config path answers nothing"
else fail "a PR that touches no config path answers nothing" "got: ${out}"; fi

echo "TEST: no Harness config is a usage error, never a silent 'not bootstrap'"
( unset HARNESS_CONFIG_JSON AUTO_AGENT_TARGET_DIR; bash "${LIB}" state >/dev/null 2>&1 ); rc=$?
if [ "${rc}" -eq 2 ]; then pass "state without a config: exit 2"; else fail "state without a config: exit 2" "rc=${rc}"; fi
bash "${LIB}" bogus >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 2 ]; then pass "unknown subcommand: exit 2"; else fail "unknown subcommand: exit 2" "rc=${rc}"; fi

#-------------------------------------------------------------------------------
# Behaviour 1, over the real fixture: the hermetic block removed.
echo "TEST: the fixture with its hermetic block removed is the Bootstrap state (AC 1)"
BOOT="${WORK}/bootstrap-target"
cp -r "${FIX}" "${BOOT}"
jq 'del(.verification.hermetic) | del(.verification.deployed)' "${FIX}/.auto-agent/harness.json" \
    > "${BOOT}/.auto-agent/harness.json"
D="$(mktemp -d "${WORK}/fix-XXXXXX")"; gh_stub "${D}"
cat > "${D}/git" <<'STUB'
#!/usr/bin/env bash
case "$*" in *"remote get-url origin"*) echo 'https://github.com/acme/widgets.git'; exit 0 ;; esac
exit 0
STUB
cat > "${D}/gh-repo" <<STUB
#!/usr/bin/env bash
case "\$1 \$2" in "repo view") echo trunk; exit 0 ;; esac
exec "${D}/gh" "\$@"
STUB
chmod +x "${D}/git" "${D}/gh-repo"
if bash "${CLI}" check-config "${BOOT}" >/dev/null 2>&1; then pass "the config still validates without a hermetic tier"
else fail "the config still validates without a hermetic tier" "check-config refused it"; fi
out="$(GH_BIN="${D}/gh-repo" GIT_BIN="${D}/git" HARNESS_CONFIG_JSON= bash "${CLI}" bootstrap state "${BOOT}")"
if [ "$(printf '%s' "${out}" | jq -r .bootstrap)" = "true" ]; then pass "the fixture without a provider reads as the Bootstrap state"
else fail "the fixture without a provider reads as the Bootstrap state" "${out}"; fi

echo "TEST: the provider check and the round both say 'nothing to boot', not 'failed' (AC 1)"
GH_BIN="${D}/gh-repo" GIT_BIN="${D}/git" HARNESS_CONFIG_JSON= bash "${CLI}" provider-check "${BOOT}" >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 3 ]; then pass "provider-check: BOOTSTRAP (exit 3)"; else fail "provider-check: BOOTSTRAP (exit 3)" "rc=${rc}"; fi
GH_BIN="${D}/gh-repo" GIT_BIN="${D}/git" HARNESS_CONFIG_JSON= bash "${CLI}" verify-boot up --pr 7 "${BOOT}" >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 3 ]; then pass "verify-boot: Bootstrap state (exit 3)"; else fail "verify-boot: Bootstrap state (exit 3)" "rc=${rc}"; fi

echo "TEST: the pickup Fire still picks a ticket in the Bootstrap state (AC 1)"
# Nothing about a missing provider may stop the queue being worked: that is the
# whole point of the state. The triage is the pickup Fire's whole decision.
cat > "${D}/gh-pick" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${D}/pick.log"
case "\$1 \$2" in
  "auth status") exit 0 ;;
  "api user") echo daemon-user; exit 0 ;;
  "repo view") echo trunk; exit 0 ;;
  "issue list")
      prev=''; prog='.'
      for a in "\$@"; do [ "\$prev" = "--jq" ] && prog="\$a"; prev="\$a"; done
      echo '[]' | jq -r "\$prog"
      ;;
  "api graphql")
      cat <<'JSON'
{"data":{"repository":{"issues":{"nodes":[{"number":77,"title":"A slice to work","createdAt":"2026-01-01T00:00:00Z","labels":{"nodes":[{"name":"AFK"}]},"blockedBy":{"nodes":[],"pageInfo":{"hasNextPage":false}},"assignees":{"nodes":[]}}]}}}}
JSON
      ;;
  *) exit 0 ;;
esac
STUB
chmod +x "${D}/gh-pick"
out="$(GH_BIN="${D}/gh-pick" GIT_BIN="${D}/git" HARNESS_CONFIG_JSON= bash "${CLI}" pickup-triage "${BOOT}")"
if [ "$(printf '%s' "${out}" | jq -r .verdict)" = "pick" ] \
   && [ "$(printf '%s' "${out}" | jq -r .pick.issue)" = "77" ]; then pass "pickup proceeds: verdict pick #77"
else fail "pickup proceeds: verdict pick #77" "${out}"; fi

#-------------------------------------------------------------------------------
# Behaviour 2: the PR that adds the provider is verified by the provider it adds.
echo "TEST: a PR that adds the hermetic block is verified by the provider it adds (AC 2)"
# The inherited config is what the Fire resolved from the default branch: the
# Bootstrap state. The checkout is the PR head, which ADDS the provider.
HEAD_CFG="$(HARNESS_CONFIG_JSON= GH_BIN="${D}/gh-repo" GIT_BIN="${D}/git" bash "${CLI}" show-config "${FIX}" | jq -c .)"
BOOT_CFG="$(printf '%s' "${HEAD_CFG}" | jq -c '.verification = {hermetic: null, deployed: null}')"

block="$(HARNESS_CONFIG_JSON="${BOOT_CFG}" GH_BIN="${D}/gh-repo" GIT_BIN="${D}/git" \
    bash "${CLI}" verify-boot up --pr 94 --head "${FIX}" 2>/dev/null)"; rc=$?
HARNESS_CONFIG_JSON="${BOOT_CFG}" GH_BIN="${D}/gh-repo" GIT_BIN="${D}/git" \
    bash "${CLI}" verify-boot down --pr 94 --head "${FIX}" >/dev/null 2>&1
if [ "${rc}" -eq 0 ] && printf '%s' "${block}" | grep -q '^FIXTURE_WEB_URL='; then
    pass "--head boots the provider the PR adds while the default branch has none"
else fail "--head boots the provider the PR adds while the default branch has none" "rc=${rc} block=${block}"; fi

HARNESS_CONFIG_JSON="${BOOT_CFG}" bash "${CLI}" verify-boot up --pr 94 "${FIX}" >/dev/null 2>&1; rc=$?
if [ "${rc}" -eq 3 ]; then pass "without --head the inherited Bootstrap config still wins"
else fail "without --head the inherited Bootstrap config still wins" "rc=${rc}"; fi

echo "TEST: that same PR is flagged for a human, because it changes its own verification (AC 4)"
out="$(printf '%s\n' '.auto-agent/harness.json' 'verify/provider' \
    | HARNESS_CONFIG_JSON="${HEAD_CFG}" bash "${CLI}" bootstrap config-touched "${FIX}")"
if [ "${out}" = ".auto-agent/harness.json" ]; then pass "the provider PR's config change is named"
else fail "the provider PR's config change is named" "got: ${out}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
