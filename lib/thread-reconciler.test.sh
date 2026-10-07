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

# Canned response for the authorship/ruling tests: every thread carries its
# whole comment list. Same login everywhere (the machine user replies to its
# own review), so only the markers can tell the two voices apart.
threads_fixture() {
    cat <<'EOS'
{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
  {"id":"RT_BOT","isResolved":false,"path":"src/a.ts","line":12,
   "comments":{"nodes":[
     {"databaseId":9101,"body":"<!-- pr-review-bot -->\n🤖 **pr-review** · correctness · logic-error · high\n\noff-by-one"}
   ]}},
  {"id":"RT_HUMAN","isResolved":false,"path":"src/b.ts","line":3,
   "comments":{"nodes":[
     {"databaseId":9102,"body":"please rename this"}
   ]}},
  {"id":"RT_RULED","isResolved":false,"path":"src/c.ts","line":7,
   "comments":{"nodes":[
     {"databaseId":9103,"body":"<!-- pr-review-bot -->\n🤖 missing null check"},
     {"databaseId":9104,"body":"<!-- auto-agent:fix -->\nfixed in abc123: guarded the call"},
     {"databaseId":9105,"body":"I agree with the fix, please resolve"}
   ]}},
  {"id":"RT_LOOPLAST","isResolved":false,"path":"src/d.ts","line":1,
   "comments":{"nodes":[
     {"databaseId":9106,"body":"<!-- pr-review-bot -->\n🤖 wrong default"},
     {"databaseId":9107,"body":"keep the default as is"},
     {"databaseId":9108,"body":"<!-- auto-agent:ruling -->\nruling applied: no change"}
   ]}}
]}}}}}
EOS
}

echo "TEST: authorship by marker on a same-login thread (AC 1)"
dir="$(make_stub)"; threads_fixture > "${dir}/response.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_unresolved_threads 310")"
t="first comment carrying the pr-review marker => authored: bot"
if [ "$(printf '%s' "${out}" | jq -r '.[] | select(.threadId == "RT_BOT") | .authored')" = "bot" ]; then pass "$t"; else fail "$t" "out=${out}"; fi
t="first comment lacking the marker => authored: human, whatever the login"
if [ "$(printf '%s' "${out}" | jq -r '.[] | select(.threadId == "RT_HUMAN") | .authored')" = "human" ]; then pass "$t"; else fail "$t" "out=${out}"; fi
t="the query asks for the whole thread, not only its first comment"
if grep -q 'comments(first: 100)' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
rm -rf "${dir}"

echo "TEST: ruling selection across interleaved replies (AC 2)"
dir="$(make_stub)"; threads_fixture > "${dir}/response.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_unresolved_threads 310")"
t="replies carry every later comment, each flagged agent by marker"
if [ "$(printf '%s' "${out}" | jq -c '.[] | select(.threadId == "RT_RULED") | [(.replies | length), .replies[0].agent, .replies[0].databaseId, .replies[1].agent, .replies[1].body]')" = '[2,true,9104,false,"I agree with the fix, please resolve"]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
t="a human reply after the loop's marked reply is the thread's ruling"
if [ "$(printf '%s' "${out}" | jq -r '.[] | select(.threadId == "RT_RULED") | .ruling')" = "I agree with the fix, please resolve" ]; then pass "$t"; else fail "$t" "out=${out}"; fi
t="a loop reply after the human's leaves ruling null"
if [ "$(printf '%s' "${out}" | jq -c '.[] | select(.threadId == "RT_LOOPLAST") | [.ruling, .replies[0].agent, .replies[1].agent]')" = '[null,false,true]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
t="a thread with no replies has an empty replies list and a null ruling"
if [ "$(printf '%s' "${out}" | jq -c '.[] | select(.threadId == "RT_HUMAN") | [.replies, .ruling]')" = '[[],null]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
t="the first comment is never a reply, and body stays the first comment's"
if [ "$(printf '%s' "${out}" | jq -r '.[] | select(.threadId == "RT_RULED") | .body')" = "$(printf '<!-- pr-review-bot -->\n🤖 missing null check')" ]; then pass "$t"; else fail "$t" "out=${out}"; fi
rm -rf "${dir}"

echo "TEST: tr_reply prefixes the marker it is given (AC 3)"
dir="$(make_stub)"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_reply 310 9001 'fixed in abc123: guarded the call' \"\${TR_MARKER_FIX}\""
t="the posted body is the marker, a newline, then the text"
if grep -q "body=<!-- auto-agent:fix -->"\$'\\n'"fixed in abc123: guarded the call" "${dir}/gh.log" || grep -qz 'body=<!-- auto-agent:fix -->.fixed in abc123: guarded the call' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
t="without a marker the body is posted bare (the pre-marker call shape still works)"
: > "${dir}/gh.log"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_reply 310 9001 'plain text'"
if grep -q 'body=plain text' "${dir}/gh.log" && ! grep -q 'auto-agent:' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
rm -rf "${dir}"

echo "TEST: tr_resolve_with_reply posts the marked reply and resolves in one call (AC 3)"
dir="$(make_stub)"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_resolve_with_reply 310 9103 RT_RULED \"\${TR_MARKER_ARBITER}\" 'arbiter: dismissed — the guard is already upstream'"; rc=$?
t="exit 0; a marked in-thread reply then the resolveReviewThread mutation, in that order"
# The stub logs a multi-line body across lines, so count `api` calls, not lines.
reply_at="$(grep -n 'repos/acme/widgets/pulls/310/comments' "${dir}/gh.log" | cut -d: -f1 | head -1)"
resolve_at="$(grep -n 'resolveReviewThread' "${dir}/gh.log" | cut -d: -f1 | head -1)"
if [ "${rc}" -eq 0 ] && [ "$(grep -c '^api ' "${dir}/gh.log")" -eq 2 ] \
   && [ -n "${reply_at}" ] && [ -n "${resolve_at}" ] && [ "${reply_at}" -lt "${resolve_at}" ] \
   && grep -q 'in_reply_to=9103' "${dir}/gh.log" \
   && grep -q 'body=<!-- auto-agent:arbiter -->' "${dir}/gh.log" \
   && grep -q 'arbiter: dismissed — the guard is already upstream' "${dir}/gh.log" \
   && grep -q 'threadId=RT_RULED' "${dir}/gh.log"; then pass "$t"; else fail "$t" "rc=${rc} log=$(cat "${dir}/gh.log")"; fi
t="the Ruling marker is the third voice the lib owns"
if [ "$(bash -c ". '${LIB}'; printf '%s' \"\${TR_MARKER_RULING}\"")" = '<!-- auto-agent:ruling -->' ]; then pass "$t"; else fail "$t"; fi
rm -rf "${dir}"

echo "TEST: tr_resolve_with_reply never resolves when the reply failed"
dir="$(make_stub)"; echo 1 > "${dir}/exit-code"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; tr_resolve_with_reply 310 9103 RT_RULED \"\${TR_MARKER_RULING}\" 'ruling applied: no change'" 2>/dev/null; rc=$?
t="a failed reply exits non-zero and the mutation is never sent"
if [ "${rc}" -ne 0 ] && ! grep -q 'resolveReviewThread' "${dir}/gh.log"; then pass "$t"; else fail "$t" "rc=${rc} log=$(cat "${dir}/gh.log")"; fi
rm -rf "${dir}"

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
