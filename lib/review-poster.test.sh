#!/usr/bin/env bash
# Tests for lib/review-poster.sh
#
# Run: bash lib/review-poster.test.sh
#
# Strategy: the poster's job is (a) render a finding into the uniform
# marker-bearing comment body, (b) post it as an inline review comment anchored
# to a diff line, (c) filter the thread-reconciler's enumeration down to
# agent-authored threads via the marker, (d) detect and post the once-per-PR
# done-marker comment. A GH_BIN stub logs every call and plays back a canned
# response, so the tests assert observable behavior (what was asked of gh and
# how responses were distilled) with no network. The repo comes only from
# HARNESS_CONFIG_JSON, so every REST path is asserted to carry the configured
# slug.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/review-poster.sh"

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

if [ ! -f "${LIB}" ]; then
    echo "FATAL: ${LIB} not found"
    exit 2
fi

# Build a stub gh that logs args and prints the canned response file (if any).
make_stub() {
    local dir; dir="$(mktemp -d)"
    : > "${dir}/gh.log"
    mkdir -p "${dir}/target"
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

# Canned tr_unresolved_threads output: 1 human thread + 2 agent-marked threads.
threads_fixture() {
    cat <<'EOS'
[{"threadId":"RT_1","path":"apps/backend/src/a.ts","line":12,"commentDatabaseId":9001,"body":"rename this variable"},
 {"threadId":"RT_2","path":"apps/backend/src/b.ts","line":3,"commentDatabaseId":9002,"body":"<!-- pr-review-bot -->\n🤖 **pr-review** · correctness · logic-error · high\n\ninverted null check"},
 {"threadId":"RT_3","path":"apps/frontend/src/c.tsx","line":40,"commentDatabaseId":9003,"body":"<!-- pr-review-bot -->\n🤖 **pr-review** · spec · missing-requirement · medium\n\nAC 3 not implemented"}]
EOS
}

#-------------------------------------------------------------------------------
# Test 1: rp_render_finding output carries the machine marker, the visible 🤖
# header with axis/category/severity, the summary, and the failure scenario.
#-------------------------------------------------------------------------------
test_render_finding_body() {
    echo "TEST: rendered finding carries marker + template fields"

    local out
    out="$(bash -c ". '${LIB}'; rp_render_finding defect correctness logic-error high 'inverted null check on save' 'a null profile crashes the save endpoint'")"

    if ! printf '%s' "${out}" | grep -qF '<!-- pr-review-bot -->'; then
        fail "body must contain the machine marker" "out=${out}"
        return
    fi
    if ! printf '%s' "${out}" | grep -q '🤖 \*\*pr-review\*\* · defect · correctness · logic-error · high'; then
        fail "body must contain the visible 🤖 axis/category/severity header" "out=${out}"
        return
    fi
    if ! printf '%s' "${out}" | grep -qF 'inverted null check on save'; then
        fail "body must contain the summary" "out=${out}"
        return
    fi
    if ! printf '%s' "${out}" | grep -qF '**Failure scenario:** a null profile crashes the save endpoint'; then
        fail "body must contain the failure scenario" "out=${out}"
        return
    fi

    pass "rendered finding carries marker + template fields"
}

#-------------------------------------------------------------------------------
# Test 2: rp_post_inline posts to the configured repo's pulls comments endpoint
# with path, line, side=RIGHT, and the anchoring commit_id: an inline thread,
# not a detached comment.
#-------------------------------------------------------------------------------
test_post_inline_anchors() {
    echo "TEST: inline post anchors path/line/side/commit on the configured repo"

    local dir gh_log
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN

    GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_inline 310 abc1234 apps/backend/src/a.ts 12 'the body'"

    gh_log="$(cat "${dir}/gh.log")"
    if ! printf '%s' "${gh_log}" | grep -q 'repos/acme/widgets/pulls/310/comments'; then
        fail "must post to the configured repo's PR review-comments endpoint" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -q 'commit_id=abc1234' \
        || ! printf '%s' "${gh_log}" | grep -q 'path=apps/backend/src/a.ts' \
        || ! printf '%s' "${gh_log}" | grep -q 'line=12' \
        || ! printf '%s' "${gh_log}" | grep -q 'side=RIGHT'; then
        fail "must anchor with commit_id, path, line, side=RIGHT" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -q 'body=the body'; then
        fail "must carry the rendered body" "gh.log=${gh_log}"
        return
    fi

    pass "inline post anchors path/line/side/commit on the configured repo"
}

#-------------------------------------------------------------------------------
# Test 3: rp_filter_agent_threads keeps only marker-bearing threads from the
# tr_unresolved_threads array; human threads never reach the fix loop.
#-------------------------------------------------------------------------------
test_filter_keeps_only_agent_threads() {
    echo "TEST: filter keeps only agent-marked threads"

    local out count
    out="$(threads_fixture | bash -c ". '${LIB}'; rp_filter_agent_threads")"

    count="$(printf '%s' "${out}" | jq 'length')"
    if [ "${count}" != "2" ]; then
        fail "must keep exactly the 2 marker-bearing threads" "out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.[0].threadId')" != "RT_2" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.[1].threadId')" != "RT_3" ]; then
        fail "human RT_1 must be dropped, RT_2/RT_3 kept with ids intact" "out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.[0].commentDatabaseId')" != "9002" ]; then
        fail "thread fields must survive the filter" "out=${out}"
        return
    fi

    pass "filter keeps only agent-marked threads"
}

#-------------------------------------------------------------------------------
# Test 4: rp_done_marker_present exits 0 when a top-level comment carries the
# done-marker, 1 when none does; the comments are read from the configured repo.
#-------------------------------------------------------------------------------
test_done_marker_detection() {
    echo "TEST: done-marker detection"

    local dir rc gh_log
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN

    cat > "${dir}/response.json" <<'EOS'
[{"body":"manual-verify: 3/3 PASS"},
 {"body":"<!-- pr-review-done reviewed=abc1234 fixes=def5678 -->\n🤖 pr-review: 3 findings, 2 fixed (reviewed abc1234)"}]
EOS
    GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; rp_done_marker_present 310"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "marker present must exit 0" "rc=${rc}"
        return
    fi
    gh_log="$(cat "${dir}/gh.log")"
    if ! printf '%s' "${gh_log}" | grep -q 'repos/acme/widgets/issues/310/comments'; then
        fail "must read the configured repo's PR comments" "gh.log=${gh_log}"
        return
    fi

    cat > "${dir}/response.json" <<'EOS'
[{"body":"manual-verify: 3/3 PASS"},{"body":"just a human comment"}]
EOS
    GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; rp_done_marker_present 310"
    rc=$?
    if [ "${rc}" -eq 0 ]; then
        fail "marker absent must exit non-zero" "rc=${rc}"
        return
    fi

    pass "done-marker detection"
}

#-------------------------------------------------------------------------------
# Test 5: rp_post_done_marker posts one top-level comment carrying the machine
# marker with both SHAs and the human-readable tally, on the configured repo.
#-------------------------------------------------------------------------------
test_post_done_marker() {
    echo "TEST: done-marker comment body"

    local dir gh_log
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN

    GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_done_marker 310 3 2 abc1234 def5678"

    gh_log="$(cat "${dir}/gh.log")"
    if ! printf '%s' "${gh_log}" | grep -q 'repos/acme/widgets/issues/310/comments'; then
        fail "must post a top-level issue comment on the configured repo's PR" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -qF '<!-- pr-review-done reviewed=abc1234 fixes=def5678 -->'; then
        fail "must carry the machine marker with both SHAs" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -q '🤖 pr-review: 3 findings, 2 fixed (reviewed abc1234)'; then
        fail "must carry the human-readable tally" "gh.log=${gh_log}"
        return
    fi

    pass "done-marker comment body"
}

#-------------------------------------------------------------------------------
# Test 6: an API failure surfaces as a non-zero exit from rp_post_inline (the
# skill's 422 fallback keys off this), never a silent success.
#-------------------------------------------------------------------------------
test_api_failure_surfaces() {
    echo "TEST: API failure exits non-zero"

    local dir rc
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN
    echo 1 > "${dir}/exit-code"

    GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_inline 310 abc1234 apps/backend/src/a.ts 12 'body'" \
        >/dev/null 2>&1
    rc=$?

    if [ "${rc}" -eq 0 ]; then
        fail "a failed API call must exit non-zero" "rc=${rc}"
        return
    fi

    pass "API failure exits non-zero"
}

#-------------------------------------------------------------------------------
# Test 7: with no Harness config, every posting/reading function refuses
# non-zero with a stderr line and gh is never called.
#-------------------------------------------------------------------------------
test_no_config_refuses_without_gh_call() {
    echo "TEST: no Harness config refuses before any gh call"

    local dir rc fn
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN

    for fn in "rp_post_inline 310 abc1234 a.ts 12 body" \
              "rp_done_marker_present 310" \
              "rp_post_done_marker 310 0 0 abc1234 none" \
              "rp_first_changed_line abc1234 a.ts" \
              "rp_post_inline_fallback 310 abc1234 a.ts 12 body" \
              "rp_post_defect 310 abc1234 a.ts 12 body"; do
        : > "${dir}/err"
        HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= GH_BIN="${dir}/gh-stub" \
            bash -c ". '${LIB}'; ${fn}" 2>"${dir}/err"
        rc=$?
        if [ "${rc}" -eq 0 ]; then
            fail "${fn%% *} must fail without a config" "rc=${rc}"
            return
        fi
        if ! grep -q 'no Harness config' "${dir}/err"; then
            fail "${fn%% *} must say why on stderr" "err=$(cat "${dir}/err")"
            return
        fi
    done
    if [ -s "${dir}/gh.log" ]; then
        fail "gh must not be called without a config" "gh.log=$(cat "${dir}/gh.log")"
        return
    fi

    pass "no Harness config refuses before any gh call"
}

#-------------------------------------------------------------------------------
# Test 8: rp_apply_bar — the posting bar (Spec #74, the posting bar). A defect
# whose failure scenario is empty, or says no failure, and that quotes no
# requirement is demoted to a Review note; a Standards category or scope-creep
# is a Review note whatever the reviewer said; a defect with a concrete
# scenario, or one that quotes a contradicted requirement, keeps its kind; a
# product ambiguity passes through untouched.
#-------------------------------------------------------------------------------
test_apply_bar_demotes_scenarioless_defects() {
    echo "TEST: the bar demotes scenario-less defects and Standards findings to Review notes"

    local out kinds
    out="$(bash -c ". '${LIB}'; rp_apply_bar" <<'EOS'
[{"kind":"defect","axis":"correctness","category":"logic-error","path":"a.sh","line":3,"severity":"low","summary":"empty scenario","failure_scenario":""},
 {"kind":"defect","axis":"correctness","category":"bug","path":"a.sh","line":4,"severity":"low","summary":"says no failure","failure_scenario":"No runtime failure."},
 {"kind":"defect","axis":"correctness","category":"duplication","path":"a.sh","line":5,"severity":"high","summary":"standards","failure_scenario":"the helper is copied twice and one copy drifts"},
 {"kind":"defect","axis":"spec","category":"scope-creep","path":"a.sh","line":6,"severity":"medium","summary":"creep","failure_scenario":"an endpoint nobody asked for is reachable"},
 {"kind":"defect","axis":"correctness","category":"logic-error","path":"a.sh","line":7,"severity":"low","summary":"real","failure_scenario":"a null profile crashes the save endpoint"},
 {"kind":"defect","axis":"spec","category":"spec-mismatch","path":"a.sh","line":8,"severity":"low","summary":"contradicts AC","failure_scenario":"","quoted_requirement":"AC 2: the label is applied only when a thread was opened"},
 {"kind":"product-ambiguity","axis":"spec","category":"ambiguity","path":"a.sh","line":9,"severity":"medium","summary":"spec silent on retries","failure_scenario":""},
 {"axis":"correctness","category":"error-handling","path":"a.sh","line":10,"severity":"medium","summary":"no kind at all","failure_scenario":"a 500 on an empty body is swallowed and the caller sees 200"}]
EOS
)"
    kinds="$(printf '%s' "${out}" | jq -r '[.[].kind] | join(",")')"
    if [ "${kinds}" != "review-note,review-note,review-note,review-note,defect,defect,product-ambiguity,defect" ]; then
        fail "kinds after the bar" "kinds=${kinds} out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.[0].demoted')" != "true" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.[4].demoted // "absent"')" != "absent" ]; then
        fail "a demoted finding is flagged demoted:true; an untouched one is not" "out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.[4].path')" != "a.sh" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.[4].line')" != "7" ]; then
        fail "every other field survives the bar" "out=${out}"
        return
    fi

    # The no-failure match is the WHOLE scenario, not a prefix: a real scenario
    # that happens to start with "Non…", "Na…" or "No failure is visible until…"
    # is a defect and must not be demoted (the reviewer's sorting is trusted).
    out="$(bash -c ". '${LIB}'; rp_apply_bar" <<'EOS'
[{"kind":"defect","axis":"correctness","category":"bug","path":"a.sh","line":1,"severity":"high","summary":"boot","failure_scenario":"Nonexistent config file makes the daemon crash at boot"},
 {"kind":"defect","axis":"correctness","category":"data-loss","path":"a.sh","line":2,"severity":"high","summary":"truncate","failure_scenario":"Naming collision truncates the wrong table"},
 {"kind":"defect","axis":"correctness","category":"bug","path":"a.sh","line":3,"severity":"medium","summary":"cache","failure_scenario":"No failure is visible until the cache expires, then every request 500s"},
 {"kind":"defect","axis":"correctness","category":"bug","path":"a.sh","line":4,"severity":"low","summary":"bare none","failure_scenario":"  None. "},
 {"kind":"defect","axis":"correctness","category":"bug","path":"a.sh","line":5,"severity":"low","summary":"bare n/a","failure_scenario":"n/a"},
 {"kind":"defect","axis":"correctness","category":"bug","path":"a.sh","line":6,"severity":"low","summary":"nothing breaks","failure_scenario":"Nothing breaks"}]
EOS
)"
    kinds="$(printf '%s' "${out}" | jq -r '[.[].kind] | join(",")')"
    if [ "${kinds}" != "defect,defect,defect,review-note,review-note,review-note" ]; then
        fail "only a bare no-failure phrase demotes; a scenario starting with Non/Na/No failure is visible… is a defect" "kinds=${kinds}"
        return
    fi

    # test-coverage is a note unless the finding quotes the requirement that
    # asked for the test (Spec #74: "unless the issue or Spec asks for the test
    # in words"); a product ambiguity is never reclassified by its category.
    out="$(bash -c ". '${LIB}'; rp_apply_bar" <<'EOS'
[{"kind":"defect","axis":"spec","category":"test-coverage","path":"lib/a.sh","line":1,"severity":"medium","summary":"no a.test.sh","failure_scenario":"","quoted_requirement":"Every touched lib/*.sh gains its *.test.sh"},
 {"kind":"defect","axis":"correctness","category":"test-coverage","path":"lib/a.sh","line":2,"severity":"medium","summary":"no test for the empty case","failure_scenario":"the empty case is untested and a regression there goes unnoticed"},
 {"kind":"product-ambiguity","axis":"spec","category":"test-coverage","path":"lib/a.sh","line":3,"severity":"medium","summary":"spec silent on which suite","failure_scenario":"reading A: lib suite; reading B: e2e"},
 {"kind":"product-ambiguity","axis":"spec","category":"duplication","path":"lib/a.sh","line":4,"severity":"medium","summary":"silent","failure_scenario":"A or B"}]
EOS
)"
    kinds="$(printf '%s' "${out}" | jq -r '[.[].kind] | join(",")')"
    if [ "${kinds}" != "defect,review-note,product-ambiguity,product-ambiguity" ]; then
        fail "test-coverage with a quoted requirement is a defect, unquoted a note; a product ambiguity keeps its kind whatever its category" "kinds=${kinds}"
        return
    fi

    pass "the bar demotes scenario-less defects and Standards findings to Review notes"
}

#-------------------------------------------------------------------------------
# Test 9: only a defect renders as a thread body. A review-note or a
# product-ambiguity kind is refused (exit 2, stderr, empty stdout): the bar is
# enforced at the one function every thread body passes through.
#-------------------------------------------------------------------------------
test_render_refuses_non_defect_kinds() {
    echo "TEST: rp_render_finding renders only a defect"

    local kind out rc err
    for kind in review-note product-ambiguity; do
        err="$(mktemp)"
        out="$(bash -c ". '${LIB}'; rp_render_finding ${kind} spec ambiguity medium 'spec silent on retries' ''" 2>"${err}")"
        rc=$?
        if [ "${rc}" -ne 2 ] || [ -n "${out}" ] || ! grep -q 'not a thread kind' "${err}"; then
            fail "${kind} must be refused with exit 2, no body, a stderr line" "rc=${rc} out=${out} err=$(cat "${err}")"
            rm -f "${err}"
            return
        fi
        rm -f "${err}"
    done

    pass "rp_render_finding renders only a defect"
}

#-------------------------------------------------------------------------------
# Test 10: the done-marker carries the Review notes as one collapsed list and
# the product ambiguities as a structured block (for the Ruling request lane
# to pick up); neither opens a thread. With no notes and no ambiguities the
# comment is the bare marker + tally, so a PR that earned neither carries no
# empty sections.
#-------------------------------------------------------------------------------
test_done_marker_carries_notes_and_ambiguities() {
    echo "TEST: done-marker carries Review notes (collapsed) and ambiguities (structured)"

    local dir gh_log notes ambs rendered
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN

    notes='[{"kind":"review-note","axis":"correctness","category":"duplication","path":"a.sh","line":5,"severity":"low","summary":"helper copied twice","failure_scenario":"","demoted":true},
            {"kind":"review-note","axis":"spec","category":"test-coverage","path":"b.sh","line":9,"severity":"low","summary":"no test for the empty case","failure_scenario":""}]'
    ambs='[{"kind":"product-ambiguity","axis":"spec","category":"ambiguity","path":"a.sh","line":9,"severity":"medium","summary":"Spec silent on retries","failure_scenario":"","quoted_requirement":""}]'

    rendered="$(bash -c ". '${LIB}'; rp_render_notes '${notes}'")"
    if ! printf '%s' "${rendered}" | grep -q '<details>' \
        || ! printf '%s' "${rendered}" | grep -q '<summary>Review notes, no action taken (2)</summary>' \
        || ! printf '%s' "${rendered}" | grep -qF '`a.sh:5` · correctness · duplication — helper copied twice' \
        || ! printf '%s' "${rendered}" | grep -qF '`b.sh:9` · spec · test-coverage — no test for the empty case'; then
        fail "rp_render_notes must render one collapsed list, one line per note with its location" "rendered=${rendered}"
        return
    fi
    if ! printf '%s' "${rendered}" | grep -q 'demoted'; then
        fail "a demoted defect is marked as such in its note line" "rendered=${rendered}"
        return
    fi
    if [ -n "$(bash -c ". '${LIB}'; rp_render_notes '[]'")" ]; then
        fail "no notes renders nothing"
        return
    fi

    GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_done_marker 310 4 0 abc1234 none '${notes}' '${ambs}'"
    gh_log="$(cat "${dir}/gh.log")"
    if ! printf '%s' "${gh_log}" | grep -qF '<!-- pr-review-done reviewed=abc1234 fixes=none -->'; then
        fail "the machine marker stays first" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -q '🤖 pr-review: 4 findings, 0 fixed (reviewed abc1234) — 1 defect thread(s), 2 review note(s), 1 product ambiguit'; then
        fail "the tally counts threads, notes and ambiguities apart" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -q '<summary>Review notes, no action taken (2)</summary>'; then
        fail "the notes list is in the done-marker body" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -q '<!-- pr-review-ambiguities' \
        || ! printf '%s' "${gh_log}" | grep -qF '"summary":"Spec silent on retries"'; then
        fail "the ambiguities ride in a structured block the Ruling request lane can parse" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -qF 'Product ambiguities (1)'; then
        fail "the ambiguities are also listed for the human" "gh.log=${gh_log}"
        return
    fi
    if printf '%s' "${gh_log}" | grep -q 'repos/acme/widgets/pulls/'; then
        fail "the done-marker never posts an inline (thread) comment" "gh.log=${gh_log}"
        return
    fi

    : > "${dir}/gh.log"
    GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_done_marker 310 0 0 abc1234 none"
    gh_log="$(cat "${dir}/gh.log")"
    if printf '%s' "${gh_log}" | grep -q 'Review notes\|pr-review-ambiguities'; then
        fail "no notes / no ambiguities → no empty sections" "gh.log=${gh_log}"
        return
    fi

    pass "done-marker carries Review notes (collapsed) and ambiguities (structured)"
}

#-------------------------------------------------------------------------------
# Test 11: rp_parse_ambiguities reads the structured block back out of a
# done-marker body — the seam the Ruling request Slice consumes.
#-------------------------------------------------------------------------------
test_parse_ambiguities_round_trip() {
    echo "TEST: ambiguities round-trip through the done-marker body"

    local ambs body out
    ambs='[{"kind":"product-ambiguity","axis":"spec","category":"ambiguity","path":"a.sh","line":9,"severity":"medium","summary":"Spec silent on retries","failure_scenario":""}]'
    body="$(bash -c ". '${LIB}'; rp_render_done_marker_body 1 0 abc1234 none '[]' '${ambs}'")"
    out="$(printf '%s' "${body}" | bash -c ". '${LIB}'; rp_parse_ambiguities")"
    if [ "$(printf '%s' "${out}" | jq -r 'length')" != "1" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.[0].summary')" != "Spec silent on retries" ]; then
        fail "the block parses back to the same array" "out=${out} body=${body}"
        return
    fi
    out="$(printf '%s' "no block here" | bash -c ". '${LIB}'; rp_parse_ambiguities")"
    if [ "${out}" != "[]" ]; then
        fail "a body with no block parses to an empty array" "out=${out}"
        return
    fi
    # A `-->` inside a string field must not close the HTML comment early: the
    # block carries it escaped, and it parses back to the same text.
    ambs='[{"kind":"product-ambiguity","axis":"spec","category":"ambiguity","path":"a.sh","line":9,"severity":"medium","summary":"state A --> B is undefined","failure_scenario":"reading 1 --> stay; reading 2 --> reset"}]'
    body="$(bash -c ". '${LIB}'; rp_render_done_marker_body 1 0 abc1234 none '[]' '${ambs}'")"
    block="$(printf '%s\n' "${body}" | sed -n '/^<!-- pr-review-ambiguities$/,/^-->$/p' | sed '1d;$d')"
    if printf '%s' "${block}" | grep -qF -- '-->'; then
        fail "the structured block must not contain a literal --> (it would end the HTML comment)" "block=${block}"
        return
    fi
    out="$(printf '%s' "${body}" | bash -c ". '${LIB}'; rp_parse_ambiguities")"
    if [ "$(printf '%s' "${out}" | jq -r '.[0].summary')" != "state A --> B is undefined" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.[0].failure_scenario')" != "reading 1 --> stay; reading 2 --> reset" ]; then
        fail "an escaped --> parses back to the original text" "out=${out}"
        return
    fi

    pass "ambiguities round-trip through the done-marker body"
}

#-------------------------------------------------------------------------------
# Test 12: the anchoring fallback. When GitHub refuses the intended line,
# rp_post_inline_fallback posts the same defect on the file's FIRST changed
# line (read from `git diff origin/<default_branch>...<sha> -- <path>`) with
# the intended location named in the body, so no defect is ever folded into
# the summary. The first changed line is the first hunk that ADDS a line
# (right-hand side); a deletion-only first hunk is skipped because its
# right-hand start is not in the diff; a deleted file anchors LEFT. No changed
# line at all → return 2, no post, a stderr line. git failing → return 1 with
# git's stderr in the line, never a silent "not changed".
#-------------------------------------------------------------------------------
test_inline_fallback_first_changed_line() {
    echo "TEST: unanchorable defect is posted on the file's first changed line"

    local dir gh_log git_log first rc
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN
    : > "${dir}/git.log"
    cat > "${dir}/git-stub" <<EOS
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/git.log"
if [ -f "${dir}/git-fail" ]; then echo "fatal: bad revision 'origin/trunk...abc1234'" >&2; exit 128; fi
cat "${dir}/diff.txt"
EOS
    chmod +x "${dir}/git-stub"
    cat > "${dir}/diff.txt" <<'EOS'
diff --git a/lib/x.sh b/lib/x.sh
index 1111111..2222222 100644
--- a/lib/x.sh
+++ b/lib/x.sh
@@ -3,0 +4,2 @@ set -u
+first_added() { :; }
+second_added() { :; }
@@ -40,2 +42,3 @@ main() {
+    extra
EOS

    first="$(AUTO_AGENT_TARGET_DIR="${dir}/target" GIT_BIN="${dir}/git-stub" bash -c ". '${LIB}'; rp_first_changed_line abc1234 lib/x.sh")"
    if [ "${first}" != "4 RIGHT" ]; then
        fail "first changed line is the first hunk's right-hand start, side RIGHT" "first=${first}"
        return
    fi
    git_log="$(cat "${dir}/git.log")"
    if ! printf '%s' "${git_log}" | grep -q 'origin/trunk\.\.\.abc1234 -- lib/x.sh'; then
        fail "the diff is read against the configured default branch for the one path" "git.log=${git_log}"
        return
    fi
    if ! printf '%s' "${git_log}" | grep -q -- "-C ${dir}/target "; then
        fail "git runs in the Target Project checkout (-C AUTO_AGENT_TARGET_DIR)" "git.log=${git_log}"
        return
    fi

    GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_inline_fallback 310 abc1234 lib/x.sh 77 \"\$(rp_render_finding defect correctness bug high 'off by one' 'the last row is dropped')\""
    gh_log="$(cat "${dir}/gh.log")"
    if ! printf '%s' "${gh_log}" | grep -q 'repos/acme/widgets/pulls/310/comments' \
        || ! printf '%s' "${gh_log}" | grep -q 'path=lib/x.sh' \
        || ! printf '%s' "${gh_log}" | grep -q 'line=4' \
        || ! printf '%s' "${gh_log}" | grep -q 'side=RIGHT'; then
        fail "the fallback posts an inline comment on the first changed line" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -qF '**Intended location:** `lib/x.sh:77`'; then
        fail "the body names the intended location" "gh.log=${gh_log}"
        return
    fi
    if ! printf '%s' "${gh_log}" | grep -qF '<!-- pr-review-bot -->' \
        || ! printf '%s' "${gh_log}" | grep -qF 'the last row is dropped'; then
        fail "the original rendered body (marker included) is kept" "gh.log=${gh_log}"
        return
    fi

    # Deletion-only first hunk: `+2,0` has no right-hand line, so the anchor
    # is the next hunk that adds one.
    cat > "${dir}/diff.txt" <<'EOS'
@@ -3,2 +2,0 @@ set -u
-gone_a
-gone_b
@@ -10,0 +9,1 @@ main() {
+kept
EOS
    first="$(GIT_BIN="${dir}/git-stub" bash -c ". '${LIB}'; rp_first_changed_line abc1234 lib/x.sh")"
    if [ "${first}" != "9 RIGHT" ]; then
        fail "a deletion-only first hunk is skipped for the first hunk that adds a line" "first=${first}"
        return
    fi
    # A hunk header with the counts omitted (`@@ -3 +3 @@`) means one line each.
    printf '@@ -3 +3 @@\n-a\n+b\n' > "${dir}/diff.txt"
    first="$(GIT_BIN="${dir}/git-stub" bash -c ". '${LIB}'; rp_first_changed_line abc1234 lib/x.sh")"
    if [ "${first}" != "3 RIGHT" ]; then
        fail "omitted hunk counts mean one line" "first=${first}"
        return
    fi

    # Deleted file (`+0,0`): no right-hand line exists; anchor to the first
    # deleted line on the LEFT side so the defect still gets a thread.
    cat > "${dir}/diff.txt" <<'EOS'
diff --git a/lib/old.sh b/lib/old.sh
deleted file mode 100644
--- a/lib/old.sh
+++ /dev/null
@@ -1,5 +0,0 @@
-a
EOS
    first="$(GIT_BIN="${dir}/git-stub" bash -c ". '${LIB}'; rp_first_changed_line abc1234 lib/old.sh")"
    if [ "${first}" != "1 LEFT" ]; then
        fail "a deleted file anchors to its first deleted line on the LEFT side" "first=${first}"
        return
    fi
    : > "${dir}/gh.log"
    GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_inline_fallback 310 abc1234 lib/old.sh 3 body"
    gh_log="$(cat "${dir}/gh.log")"
    if ! printf '%s' "${gh_log}" | grep -q 'pulls/310/comments' \
        || ! printf '%s' "${gh_log}" | grep -q 'line=1' \
        || ! printf '%s' "${gh_log}" | grep -q 'side=LEFT'; then
        fail "the fallback on a deleted file posts LEFT on line 1" "gh.log=${gh_log}"
        return
    fi

    : > "${dir}/diff.txt"; : > "${dir}/gh.log"; : > "${dir}/err"
    GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_inline_fallback 310 abc1234 lib/x.sh 77 body" 2>"${dir}/err"
    rc=$?
    if [ "${rc}" -ne 2 ] || [ -s "${dir}/gh.log" ] || ! grep -q 'did not change it' "${dir}/err"; then
        fail "no changed line in the file → exit 2, no post, a stderr line" "rc=${rc} err=$(cat "${dir}/err") gh.log=$(cat "${dir}/gh.log")"
        return
    fi

    touch "${dir}/git-fail"; : > "${dir}/gh.log"; : > "${dir}/err"
    GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-stub" bash -c \
        ". '${LIB}'; rp_post_inline_fallback 310 abc1234 lib/x.sh 77 body" 2>"${dir}/err"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ -s "${dir}/gh.log" ] || ! grep -q 'git diff.*failed.*bad revision' "${dir}/err"; then
        fail "git failing → exit 1, no post, git's stderr in the line (never a silent 'not changed')" "rc=${rc} err=$(cat "${dir}/err")"
        return
    fi
    rm -f "${dir}/git-fail"

    pass "unanchorable defect is posted on the file's first changed line"
}

#-------------------------------------------------------------------------------
# Test 12b: rp_post_defect falls back ONLY on an HTTP 422 (GitHub refused the
# anchor). A transient failure (5xx, rate limit, auth) on the primary post is
# returned as-is: the defect is not re-posted on another line with a
# misleading "intended location" preamble.
#-------------------------------------------------------------------------------
test_post_defect_falls_back_only_on_422() {
    echo "TEST: rp_post_defect falls back only on a 422"

    local dir gh_log rc
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN
    printf '@@ -3,0 +4,2 @@\n+a\n+b\n' > "${dir}/diff.txt"
    printf '#!/usr/bin/env bash\ncat "%s/diff.txt"\n' "${dir}" > "${dir}/git-stub"
    chmod +x "${dir}/git-stub"
    # gh: the intended line 77 is refused with the given HTTP status; any
    # other line is accepted.
    cat > "${dir}/gh-422" <<EOS
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/gh.log"
case " \$* " in *" line=77 "*) echo "gh: Unprocessable Entity (HTTP 422)" >&2; exit 1;; esac
exit 0
EOS
    cat > "${dir}/gh-502" <<EOS
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/gh.log"
case " \$* " in *" line=77 "*) echo "gh: Bad Gateway (HTTP 502)" >&2; exit 1;; esac
exit 0
EOS
    chmod +x "${dir}/gh-422" "${dir}/gh-502"

    GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-422" bash -c \
        ". '${LIB}'; rp_post_defect 310 abc1234 lib/x.sh 77 body" 2>/dev/null
    rc=$?
    gh_log="$(cat "${dir}/gh.log")"
    if [ "${rc}" -ne 0 ] || [ "$(grep -c 'pulls/310/comments' "${dir}/gh.log")" -ne 2 ] \
        || ! printf '%s' "${gh_log}" | grep -q 'line=4' \
        || ! printf '%s' "${gh_log}" | grep -qF 'Intended location:** `lib/x.sh:77`'; then
        fail "a 422 on the intended line → one fallback post on the first changed line" "rc=${rc} gh.log=${gh_log}"
        return
    fi

    : > "${dir}/gh.log"
    GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-502" bash -c \
        ". '${LIB}'; rp_post_defect 310 abc1234 lib/x.sh 77 body" 2>/dev/null
    rc=$?
    gh_log="$(cat "${dir}/gh.log")"
    if [ "${rc}" -eq 0 ] || [ "$(grep -c 'pulls/310/comments' "${dir}/gh.log")" -ne 1 ] \
        || printf '%s' "${gh_log}" | grep -q 'Intended location'; then
        fail "a non-422 failure is returned as-is: no fallback post" "rc=${rc} gh.log=${gh_log}"
        return
    fi

    : > "${dir}/gh.log"
    GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-422" bash -c \
        ". '${LIB}'; rp_post_defect 310 abc1234 lib/x.sh 5 body"
    if [ "$(grep -c 'pulls/310/comments' "${dir}/gh.log")" -ne 1 ]; then
        fail "a post that succeeds first time is posted once" "gh.log=$(cat "${dir}/gh.log")"
        return
    fi

    pass "rp_post_defect falls back only on a 422"
}

#-------------------------------------------------------------------------------
# Test 12c: there is no thread cap. Twelve defects produce twelve inline
# posts (AC 5), each anchored where the reviewer said.
#-------------------------------------------------------------------------------
test_twelve_defects_twelve_threads() {
    echo "TEST: twelve defects produce twelve threads (no cap)"

    local dir i n
    dir="$(make_stub)"
    trap "rm -rf '${dir}'" RETURN
    GH_BIN="${dir}/gh-stub" bash -c "
        . '${LIB}'
        findings=\$(jq -nc '[range(1;13) | {kind:\"defect\",axis:\"correctness\",category:\"bug\",path:\"lib/x.sh\",line:(.*3),severity:\"low\",summary:(\"defect \\(.)\"),failure_scenario:(\"input \\(.) crashes the save\")}]')
        split=\$(printf '%s' \"\$findings\" | rp_apply_bar | rp_split_findings)
        [ \"\$(jq '.defects | length' <<<\"\$split\")\" -eq 12 ] || exit 9
        jq -c '.defects[]' <<<\"\$split\" | while read -r f; do
            body=\$(rp_render_finding defect \"\$(jq -r .axis <<<\"\$f\")\" \"\$(jq -r .category <<<\"\$f\")\" \"\$(jq -r .severity <<<\"\$f\")\" \"\$(jq -r .summary <<<\"\$f\")\" \"\$(jq -r .failure_scenario <<<\"\$f\")\")
            rp_post_defect 310 abc1234 \"\$(jq -r .path <<<\"\$f\")\" \"\$(jq -r .line <<<\"\$f\")\" \"\$body\" || exit 8
        done"
    if [ $? -ne 0 ]; then
        fail "the bar keeps twelve concrete defects and every post succeeds" "gh.log=$(cat "${dir}/gh.log")"
        return
    fi
    n="$(grep -c 'repos/acme/widgets/pulls/310/comments' "${dir}/gh.log")"
    if [ "${n}" -ne 12 ]; then
        fail "twelve defects → twelve inline posts" "n=${n}"
        return
    fi
    for i in 3 18 36; do
        if ! grep -q "line=${i} " "${dir}/gh.log" && ! grep -q "line=${i}\$" "${dir}/gh.log"; then
            fail "each defect is anchored at its own line" "missing line=${i}; gh.log=$(cat "${dir}/gh.log")"
            return
        fi
    done

    pass "twelve defects produce twelve threads (no cap)"
}

#-------------------------------------------------------------------------------
# Test 13: rp_split_findings sorts a barred array into the three routes: the
# defects (threads, and the only thing that earns AFK:revise), the Review
# notes (the done-marker list) and the product ambiguities (the structured
# block). A notes-only review therefore has zero defects → no label.
#-------------------------------------------------------------------------------
test_split_findings_routes_three_kinds() {
    echo "TEST: findings split into defects / notes / ambiguities; notes-only has no defects"

    local out
    out="$(bash -c ". '${LIB}'; rp_apply_bar | rp_split_findings" <<'EOS'
[{"kind":"defect","axis":"correctness","category":"logic-error","path":"a.sh","line":3,"severity":"low","summary":"empty scenario","failure_scenario":""},
 {"kind":"defect","axis":"correctness","category":"naming","path":"a.sh","line":4,"severity":"low","summary":"standards","failure_scenario":"x"},
 {"kind":"product-ambiguity","axis":"spec","category":"ambiguity","path":"a.sh","line":9,"severity":"medium","summary":"silent","failure_scenario":""}]
EOS
)"
    if [ "$(printf '%s' "${out}" | jq -r '.defects | length')" != "0" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.notes | length')" != "2" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.ambiguities | length')" != "1" ]; then
        fail "notes-only review: 0 defects, 2 notes, 1 ambiguity" "out=${out}"
        return
    fi
    out="$(bash -c ". '${LIB}'; rp_apply_bar | rp_split_findings" <<'EOS'
[{"kind":"defect","axis":"correctness","category":"bug","path":"a.sh","line":3,"severity":"low","summary":"real","failure_scenario":"the last row is dropped on save"}]
EOS
)"
    if [ "$(printf '%s' "${out}" | jq -r '.defects | length')" != "1" ]; then
        fail "a real defect is routed to defects" "out=${out}"
        return
    fi

    pass "findings split into defects / notes / ambiguities; notes-only has no defects"
}

#-------------------------------------------------------------------------------
# Run suite
#-------------------------------------------------------------------------------
test_render_finding_body
test_post_inline_anchors
test_filter_keeps_only_agent_threads
test_done_marker_detection
test_post_done_marker
test_api_failure_surfaces
test_no_config_refuses_without_gh_call
test_apply_bar_demotes_scenarioless_defects
test_render_refuses_non_defect_kinds
test_done_marker_carries_notes_and_ambiguities
test_parse_ambiguities_round_trip
test_inline_fallback_first_changed_line
test_post_defect_falls_back_only_on_422
test_twelve_defects_twelve_threads
test_split_findings_routes_three_kinds

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
