#!/usr/bin/env bash
# Tests for lib/thread-reconciler.sh
#
# Run: bash lib/thread-reconciler.test.sh
#
# Strategy: the reconciler's job is (a) enumerate only UNRESOLVED review
# threads from the GraphQL response shape, (b) reply in-thread via the REST
# in_reply_to parameter, (c) resolve via the resolveReviewThread mutation. A
# GH_BIN stub logs every call and plays back a canned GraphQL response, so the
# tests assert observable behavior (what was asked of gh and how the response
# was distilled) with no network. The repo comes only from HARNESS_CONFIG_JSON,
# so every call is asserted to carry the configured slug.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/thread-reconciler.sh"

CFG='{"repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"labels","project":null,"labels":{}}}'
export HARNESS_CONFIG_JSON="${CFG}"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

make_stub() {
    local dir; dir="$(mktemp -d)"
    : > "${dir}/gh.log"
    cat > "${dir}/gh-stub" <<EOS
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/gh.log"
if [ -f "${dir}/response.json" ]; then
  cat "${dir}/response.json"
fi
exit "\$(cat "${dir}/exit-code" 2>/dev/null || echo 0)"
EOS
    chmod +x "${dir}/gh-stub"
    printf '%s' "${dir}"
}

# Canned GraphQL response: 2 unresolved + 1 resolved thread.
graphql_fixture() {
    cat <<'EOS'
{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
  {"id":"RT_1","isResolved":false,"path":"src/a.ts","line":12,
   "comments":{"nodes":[{"databaseId":9001,"body":"rename this variable"}]}},
  {"id":"RT_2","isResolved":true,"path":"src/b.ts","line":3,
   "comments":{"nodes":[{"databaseId":9002,"body":"already handled"}]}},
  {"id":"RT_3","isResolved":false,"path":null,"line":null,
   "comments":{"nodes":[{"databaseId":9003,"body":"missing test for the sad path"}]}}
]}}}}}
EOS
}

echo "TEST: enumerates only unresolved threads, distilled for the fix loop"
dir="$(make_stub)"; graphql_fixture > "${dir}/response.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_unresolved_threads 310")"
t="exactly the 2 unresolved threads, fields distilled, RT_2 skipped"
if [ "$(printf '%s' "${out}" | jq -c '[length, .[0].threadId, .[0].commentDatabaseId, .[0].body, .[1].threadId]')" = '[2,"RT_1",9001,"rename this variable","RT_3"]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
t="the query targets the configured owner/name and the PR"
if grep -q 'owner=acme' "${dir}/gh.log" && grep -q 'name=widgets' "${dir}/gh.log" && grep -q 'pr=310' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
rm -rf "${dir}"

echo "TEST: reply lands in-thread via in_reply_to on the configured repo"
dir="$(make_stub)"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_reply 310 9001 'fixed in abc123: renamed the variable'"
t="posts to repos/acme/widgets/pulls/310/comments with in_reply_to and the body"
if grep -q 'repos/acme/widgets/pulls/310/comments' "${dir}/gh.log" && grep -q 'in_reply_to=9001' "${dir}/gh.log" && grep -q 'body=fixed in abc123: renamed the variable' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
rm -rf "${dir}"

echo "TEST: resolve fires the resolveReviewThread mutation with the thread id"
dir="$(make_stub)"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_resolve RT_1"
t="mutation named, threadId passed"
if grep -q 'resolveReviewThread' "${dir}/gh.log" && grep -q 'threadId=RT_1' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
rm -rf "${dir}"

echo "TEST: an API failure exits non-zero, never bogus JSON"
dir="$(make_stub)"; echo 1 > "${dir}/exit-code"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_unresolved_threads 310" >/dev/null 2>&1; rc=$?
t="failed API call exits non-zero"
if [ "${rc}" -ne 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${dir}"

echo "TEST: no Harness config: exit 2, no gh call"
dir="$(make_stub)"
out="$(HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_unresolved_threads 310" 2>"${dir}/err")"; rc=$?
t="tr_unresolved_threads without a config exits 2 with a stderr line and calls nothing"
if [ "${rc}" -eq 2 ] && [ -z "${out}" ] && grep -q 'no Harness config' "${dir}/err" && [ ! -s "${dir}/gh.log" ]; then pass "$t"; else fail "$t" "rc=${rc} err=$(cat "${dir}/err") log=$(cat "${dir}/gh.log")"; fi
HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_reply 310 9001 x" 2>/dev/null; rc=$?
t="tr_reply without a config exits 2 and calls nothing"
if [ "${rc}" -eq 2 ] && [ ! -s "${dir}/gh.log" ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${dir}"

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
