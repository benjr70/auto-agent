#!/usr/bin/env bash
# Tests for lib/ci-wait.sh
#
# Run: bash lib/ci-wait.test.sh
#
# Strategy: the gh stub is STATEFUL: a counter file advances one canned
# `pr checks` fixture per poll (checks-1.out, checks-2.out, ...; the last one
# repeats), so tests drive pending-to-settled transitions. Interval 0 keeps the
# loop instant. Assertions cover the exit code + the line-1 JSON verdict + the
# failure log bundle, the exact contract the PR-watch round consumes. The repo
# comes only from HARNESS_CONFIG_JSON; every gh call is logged so the suite
# asserts the configured slug reached gh.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CI_WAIT="${SCRIPT_DIR}/ci-wait.sh"

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

make_env() {
    local dir; dir="$(mktemp -d)"
    echo 0 > "${dir}/poll-count"
    : > "${dir}/gh.log"
    cat > "${dir}/gh-stub" <<EOS
#!/usr/bin/env bash
args="\$*"
printf '%s\n' "\${args}" >> "${dir}/gh.log"
case "\${args}" in
    *"pr checks"*)
        n=\$(( \$(cat "${dir}/poll-count") + 1 ))
        echo "\${n}" > "${dir}/poll-count"
        while [ "\${n}" -gt 0 ]; do
            if [ -f "${dir}/checks-\${n}.out" ]; then cat "${dir}/checks-\${n}.out"; exit 0; fi
            n=\$(( n - 1 ))
        done
        exit 1 ;;
    *"run view"*)
        cat "${dir}/runlog.out" 2>/dev/null || exit 1 ;;
    *) exit 1 ;;
esac
EOS
    chmod +x "${dir}/gh-stub"
    echo 'line1
FAIL: expected 3 to be 4' > "${dir}/runlog.out"
    echo "${dir}"
}

checks() { # checks <bucket:name:link csv> -> JSON array fixture on stdout
    local out='[' sep='' item bucket name link
    for item in "$@"; do
        IFS=':' read -r bucket name link <<< "${item}"
        out="${out}${sep}{\"bucket\":\"${bucket}\",\"name\":\"${name}\",\"state\":\"X\",\"link\":\"${link:-}\"}"
        sep=','
    done
    printf '%s]' "${out}"
}

run_wait() { # run_wait <dir> [extra args...]: echoes output, returns code
    local dir="$1"; shift
    GH_BIN="${dir}/gh-stub" AUTO_AGENT_HOST_ENV="${dir}/no-host-env" bash "${CI_WAIT}" --pr 1 --interval 0 "$@"
}

# Test 1: pending -> green settles with exit 0 and poll count; every gh call
# carried the configured repo.
test_green_after_pending() {
    local dir out code
    dir="$(make_env)"
    checks "pending:build" > "${dir}/checks-1.out"
    checks "pass:build"    > "${dir}/checks-2.out"
    out="$(run_wait "${dir}")"; code=$?
    if [ "${code}" -eq 0 ] \
        && [ "$(printf '%s' "${out}" | head -1 | jq -r '.result')" = "green" ] \
        && [ "$(printf '%s' "${out}" | head -1 | jq -r '.polls')" = "2" ]; then
        pass "pending then green -> exit 0, result green, polls 2"
    else
        fail "pending then green -> exit 0, result green, polls 2" "code=${code} out=${out}"
    fi
    if grep -q '^pr checks 1 --repo acme/widgets ' "${dir}/gh.log" \
        && ! grep -v -- '--repo acme/widgets' "${dir}/gh.log" | grep -q .; then
        pass "every pr checks call carries the configured repo"
    else
        fail "every pr checks call carries the configured repo" "gh.log=$(cat "${dir}/gh.log")"
    fi
}

# Test 2: settled red -> exit 1, failed list + log bundle on stdout; the run
# log is read from the configured repo.
test_fail_with_log_bundle() {
    local dir out code
    dir="$(make_env)"
    checks "fail:unit-tests:https://x/actions/runs/1234/job/9" "pass:lint" > "${dir}/checks-1.out"
    out="$(run_wait "${dir}")"; code=$?
    if [ "${code}" -eq 1 ] \
        && [ "$(printf '%s' "${out}" | head -1 | jq -r '.result')" = "fail" ] \
        && [ "$(printf '%s' "${out}" | head -1 | jq -r '.failed[0].name')" = "unit-tests" ] \
        && printf '%s' "${out}" | grep -q '=== unit-tests ===' \
        && printf '%s' "${out}" | grep -q 'FAIL: expected 3 to be 4'; then
        pass "red -> exit 1, failed[] + '=== job ===' log tail"
    else
        fail "red -> exit 1, failed[] + '=== job ===' log tail" "code=${code} out=${out}"
    fi
    if grep -q '^run view 1234 --repo acme/widgets --log-failed$' "${dir}/gh.log"; then
        pass "the failed run log is read from the configured repo"
    else
        fail "the failed run log is read from the configured repo" "gh.log=$(cat "${dir}/gh.log")"
    fi
}

# Test 3: fail+pending keeps waiting (checks not settled yet).
test_fail_waits_for_pending() {
    local dir out code
    dir="$(make_env)"
    checks "fail:unit:https://x/actions/runs/1/job/2" "pending:e2e" > "${dir}/checks-1.out"
    checks "fail:unit:https://x/actions/runs/1/job/2" "pass:e2e"    > "${dir}/checks-2.out"
    out="$(run_wait "${dir}")"; code=$?
    if [ "${code}" -eq 1 ] \
        && [ "$(printf '%s' "${out}" | head -1 | jq -r '.polls')" = "2" ]; then
        pass "fail+pending waits for settle before verdict"
    else
        fail "fail+pending waits for settle before verdict" "code=${code} out=${out}"
    fi
}

# Test 4: skipping bucket is benign.
test_skipping_is_green() {
    local dir out code
    dir="$(make_env)"
    checks "skipping:docs" "pass:build" > "${dir}/checks-1.out"
    out="$(run_wait "${dir}")"; code=$?
    if [ "${code}" -eq 0 ] && [ "$(printf '%s' "${out}" | head -1 | jq -r '.result')" = "green" ]; then
        pass "skipping bucket ignored -> green"
    else
        fail "skipping bucket ignored -> green" "code=${code} out=${out}"
    fi
}

# Test 5: deadline with pending -> exit 2, pending count reported.
test_timeout() {
    local dir out code
    dir="$(make_env)"
    checks "pending:build" > "${dir}/checks-1.out"
    out="$(run_wait "${dir}" --timeout-mins 0)"; code=$?
    if [ "${code}" -eq 2 ] \
        && [ "$(printf '%s' "${out}" | head -1 | jq -r '.result')" = "timeout" ] \
        && [ "$(printf '%s' "${out}" | head -1 | jq -r '.pending')" = "1" ]; then
        pass "deadline with pending -> exit 2, result timeout"
    else
        fail "deadline with pending -> exit 2, result timeout" "code=${code} out=${out}"
    fi
}

# Test 6: three consecutive unreadable polls -> exit 3 error.
test_unreadable_errors_out() {
    local dir out code
    dir="$(make_env)"
    # No checks fixture at all -> stub exits 1 on every pr checks call.
    rm -f "${dir}"/checks-*.out
    out="$(run_wait "${dir}")"; code=$?
    if [ "${code}" -eq 3 ] && [ "$(printf '%s' "${out}" | head -1 | jq -r '.result')" = "error" ]; then
        pass "3 unreadable polls -> exit 3, result error"
    else
        fail "3 unreadable polls -> exit 3, result error" "code=${code} out=${out}"
    fi
}

# Test 7: missing --pr -> exit 3, no JSON; a --repo flag no longer exists.
test_missing_args() {
    local code out
    out="$(bash "${CI_WAIT}" 2>/dev/null)"; code=$?
    if [ "${code}" -eq 3 ] && [ -z "${out}" ]; then
        pass "missing --pr -> exit 3"
    else
        fail "missing --pr -> exit 3" "code=${code} out=${out}"
    fi
    bash "${CI_WAIT}" --pr 1 --repo o/r >/dev/null 2>&1; code=$?
    if [ "${code}" -eq 3 ]; then
        pass "--repo is not a flag any more -> exit 3"
    else
        fail "--repo is not a flag any more -> exit 3" "code=${code}"
    fi
}

# Test 8: no Harness config -> exit 3 with a stderr line, and gh is never called.
test_no_config() {
    local dir code
    dir="$(make_env)"
    checks "pass:build" > "${dir}/checks-1.out"
    HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= GH_BIN="${dir}/gh-stub" AUTO_AGENT_HOST_ENV="${dir}/no-host-env" \
        bash "${CI_WAIT}" --pr 1 --interval 0 >/dev/null 2>"${dir}/err"; code=$?
    if [ "${code}" -eq 3 ] && grep -q 'no Harness config' "${dir}/err" && [ ! -s "${dir}/gh.log" ]; then
        pass "no Harness config -> exit 3, no gh call"
    else
        fail "no Harness config -> exit 3, no gh call" "code=${code} err=$(cat "${dir}/err") gh.log=$(cat "${dir}/gh.log")"
    fi
}

test_green_after_pending
test_fail_with_log_bundle
test_fail_waits_for_pending
test_skipping_is_green
test_timeout
test_unreadable_errors_out
test_missing_args
test_no_config

echo "TEST: a value-less flag is a usage error, not a hang"
out="$(timeout 5 bash "${CI_WAIT}" --pr 2>/dev/null)"; rc=$?
if [ $rc -eq 3 ]; then pass "a value-less flag is a usage error"; else fail "a value-less flag is a usage error" "rc=$rc out=${out}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
