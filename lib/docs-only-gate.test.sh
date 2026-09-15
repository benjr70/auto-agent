#!/usr/bin/env bash
# Tests for lib/docs-only-gate.sh
#
# Run: bash lib/docs-only-gate.test.sh
#
# Strategy: the gate is a runnable CLI whose only outside contact is `git diff`
# and `gh pr checks`, both injected as stub binaries (GIT_BIN / GH_BIN) written
# into a temp dir, so no test ever touches the network or real repo state. The
# Harness config arrives resolved through HARNESS_CONFIG_JSON with `trunk` as
# the default branch, so a gate that assumed `master` would fail here.
# Assertions cover the stdout JSON verdict (docsOnly / changed / mergeCmd /
# reason) and the two-value exit code, the contract the pickup skill consumes.
# The gate never merges: every test also asserts gh is never asked to.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="${SCRIPT_DIR}/docs-only-gate.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

if [ ! -f "${GATE}" ]; then
    echo "FATAL: ${GATE} not found"
    exit 2
fi

# The resolved config every test runs under, as harness_config_load prints it.
# cfg [required_checks JSON array]
cfg() {
    printf '{"config_dir":"/srv/target/.auto-agent","repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"labels","project":null,"labels":{}},"docs_research_prefix":"docs/research/","required_checks":%s,"rounds":{"deps_fix":3}}' "${1:-[]}"
}
export HARNESS_CONFIG_JSON
HARNESS_CONFIG_JSON="$(cfg)"
# No Host env file on this box may leak into the tests.
export AUTO_AGENT_HOST_ENV=/nonexistent/host-env

# make_env: temp dir with a git stub that echoes the canned diff in diff.out
# (and records its argv in git-calls) and a gh stub that serves checks.json for
# `pr checks`, records every call in gh-calls, and fails on anything else.
make_env() {
    local dir; dir="$(mktemp -d)"
    cat > "${dir}/git-stub" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "${dir}/git-calls"
case "\$*" in
    "cat-file"*) [ -f "${dir}/head-missing" ] && exit 1; exit 0 ;;
esac
cat "${dir}/diff.out" 2>/dev/null || exit 1
STUB
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "${dir}/gh-calls"
case "\$*" in
    *"pr checks"*) cat "${dir}/checks.json" 2>/dev/null || exit 1 ;;
    *) exit 1 ;;
esac
STUB
    chmod +x "${dir}/git-stub" "${dir}/gh-stub"
    : > "${dir}/git-calls"
    : > "${dir}/gh-calls"
    printf '[{"name":"test","bucket":"pass"},{"name":"skipped-one","bucket":"skipping"}]\n' \
        > "${dir}/checks.json"
    echo "${dir}"
}

run_gate() { # run_gate <dir> <args...>
    local dir="$1"; shift
    GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-stub" "${GATE}" "$@"
}

#-------------------------------------------------------------------------------
# Test 1: a diff touching only the research prefix is docs-only: exit 0, the
# verdict carries the changed paths and the admin squash command pinned to the
# head sha, --repo (the config's slug) included so the caller can run it from
# any cwd.
#-------------------------------------------------------------------------------
test_docs_only_verdict() {
    echo "TEST: docs-only diff yields docsOnly true + pinned merge command"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\ndocs/research/583-spec.md\n' > "${dir}/diff.out"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590)"
    rc=$?

    if [ "${rc}" -ne 0 ]; then
        fail "docs-only diff must exit 0" "rc=${rc} out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "true" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.sha')" != "abc123" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.changed | length')" != "2" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.changed[0]')" != "docs/research/577-merge-recipe.md" ]; then
        fail "verdict must carry docsOnly true, sha and changed paths" "out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.reason // "none"')" != "none" ]; then
        fail "an approved verdict must carry no reason" "out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.mergeCmd')" \
        != "gh pr merge 590 --repo acme/widgets --squash --admin --match-head-commit abc123" ]; then
        fail "mergeCmd must be the admin squash pinned to sha, carrying the config's repo" "out=${out}"
        return
    fi
    if ! grep -q -- "--no-renames origin/trunk...abc123" "${dir}/git-calls"; then
        fail "gate must use a three-dot diff with --no-renames" \
            "calls: $(cat "${dir}/git-calls")"
        return
    fi
    if [ -s "${dir}/gh-calls" ]; then
        fail "without --check-state the gate must not call gh" "calls: $(cat "${dir}/gh-calls")"
        return
    fi

    pass "docs-only diff yields docsOnly true + pinned merge command"
}

#-------------------------------------------------------------------------------
# Test 1b: --base is optional and defaults to origin/<detected default branch>
# from the config, never a hard-coded name.
#-------------------------------------------------------------------------------
test_base_defaults_to_default_branch() {
    echo "TEST: --base defaults to origin/<default branch> from the config"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"

    local out rc
    out="$(run_gate "${dir}" --head abc123 --pr 590)"
    rc=$?

    if [ "${rc}" -ne 0 ] || [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "true" ]; then
        fail "the gate must run without --base" "rc=${rc} out=${out}"
        return
    fi
    if ! grep -q -- "^diff --name-only --no-renames origin/trunk...abc123$" "${dir}/git-calls"; then
        fail "the diff base must be origin/<default branch>" \
            "calls: $(cat "${dir}/git-calls")"
        return
    fi

    pass "--base defaults to origin/<default branch> from the config"
}

#-------------------------------------------------------------------------------
# Test 1c: the merge recipe is the one harness_merge_recipe prints, byte for
# byte, so the docs gate and the deps gate can never drift apart.
#-------------------------------------------------------------------------------
test_merge_cmd_is_the_shared_recipe() {
    echo "TEST: mergeCmd is harness_merge_recipe verbatim"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"

    local out want
    out="$(run_gate "${dir}" --head abc123 --pr 590 | jq -r '.mergeCmd')"
    # shellcheck source=harness-config.sh
    want="$(. "${SCRIPT_DIR}/harness-config.sh"; harness_merge_recipe acme/widgets 590 abc123)"

    if [ "${out}" != "${want}" ]; then
        fail "mergeCmd must equal the shared recipe" "out=${out} want=${want}"
        return
    fi

    pass "mergeCmd is harness_merge_recipe verbatim"
}

#-------------------------------------------------------------------------------
# Test 2: any path outside the research prefix poisons the whole PR: docsOnly
# false, exit 1, reason not-docs-only.
#-------------------------------------------------------------------------------
test_non_docs_path_refused() {
    echo "TEST: a path outside the research prefix is refused"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577.md\napps/backend/src/app.service.ts\n' > "${dir}/diff.out"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 --check-state 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ]; then
        fail "a non-docs path must exit 1" "rc=${rc} out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "false" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "not-docs-only" ]; then
        fail "verdict must be docsOnly false with reason not-docs-only" "out=${out}"
        return
    fi
    if [ -s "${dir}/gh-calls" ]; then
        fail "a refused PR must not even cost a check read" "calls: $(cat "${dir}/gh-calls")"
        return
    fi

    pass "a path outside the research prefix is refused"
}

#-------------------------------------------------------------------------------
# Test 2b: the prefix is the config's docs_research_prefix, not a literal. A
# Target Project keeping research under notes/ gets the same rule there.
#-------------------------------------------------------------------------------
test_prefix_comes_from_config() {
    echo "TEST: the research prefix comes from the config"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'notes/one.md\nnotes/two.md\n' > "${dir}/diff.out"

    local out rc
    out="$(run_gate "${dir}" --head abc123 --pr 590 2>/dev/null)"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "not-docs-only" ]; then
        fail "notes/ is outside the default prefix" "rc=${rc} out=${out}"
        return
    fi

    out="$(HARNESS_CONFIG_JSON="$(cfg | jq -c '.docs_research_prefix = "notes/"')" \
        run_gate "${dir}" --head abc123 --pr 590)"
    rc=$?
    if [ "${rc}" -ne 0 ] || [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "true" ]; then
        fail "with docs_research_prefix notes/ the same diff is docs-only" "rc=${rc} out=${out}"
        return
    fi

    pass "the research prefix comes from the config"
}

#-------------------------------------------------------------------------------
# Test 3: --no-renames means a file MOVED into the research prefix still shows
# its original path, outside the prefix, so the PR is refused. (With rename
# detection on, the same diff would read as one research path and slip a code
# deletion past the gate.)
#-------------------------------------------------------------------------------
test_rename_into_docs_research_refused() {
    echo "TEST: a rename into the research prefix is refused"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/moved-spec.md\nscripts/ralph/old-spec.md\n' > "${dir}/diff.out"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "false" ]; then
        fail "a renamed-in file must not make the PR docs-only" "rc=${rc} out=${out}"
        return
    fi

    pass "a rename into the research prefix is refused"
}

#-------------------------------------------------------------------------------
# Test 4: an empty diff is not docs-only: nothing to merge, and an empty
# `all()` would otherwise read as vacuously true.
#-------------------------------------------------------------------------------
test_empty_diff_refused() {
    echo "TEST: an empty diff is refused"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    : > "${dir}/diff.out"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "false" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.changed | length')" != "0" ]; then
        fail "an empty diff must be refused" "rc=${rc} out=${out}"
        return
    fi

    pass "an empty diff is refused"
}

#-------------------------------------------------------------------------------
# Test 5: --check-state on a docs-only PR with all checks green approves the
# merge (exit 0, no reason) and hands the caller the merge command; the gate
# itself never runs it. The check read carries the config's repo.
#-------------------------------------------------------------------------------
test_approved_when_checks_green() {
    echo "TEST: --check-state approves when checks are green"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 --check-state)"
    rc=$?

    if [ "${rc}" -ne 0 ] || [ "$(printf '%s' "${out}" | jq -r '.reason // "none"')" != "none" ]; then
        fail "a green docs-only PR must be approved (exit 0, no reason)" "rc=${rc} out=${out}"
        return
    fi
    if ! grep -q "^pr checks 590 --repo acme/widgets --json name,bucket$" \
        "${dir}/gh-calls"; then
        fail "check state must be read for the config's repo" "calls: $(cat "${dir}/gh-calls")"
        return
    fi
    if grep -q "pr merge" "${dir}/gh-calls"; then
        fail "the gate must never merge; the caller runs mergeCmd" \
            "calls: $(cat "${dir}/gh-calls")"
        return
    fi

    pass "--check-state approves when checks are green"
}

#-------------------------------------------------------------------------------
# Test 6: a failing check refuses: exit 1 with reason checks-not-green, and the
# verdict still says docsOnly true (the refusal is about CI, not the paths).
#-------------------------------------------------------------------------------
test_refused_when_checks_red() {
    echo "TEST: --check-state refuses when a check is not green"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"
    printf '[{"name":"test","bucket":"pass"},{"name":"lint","bucket":"fail"}]\n' \
        > "${dir}/checks.json"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 --check-state 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ]; then
        fail "a red check must refuse with exit 1" "rc=${rc} out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "true" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "checks-not-green" ]; then
        fail "verdict must stay docsOnly true with reason checks-not-green" "out=${out}"
        return
    fi

    pass "--check-state refuses when a check is not green"
}

#-------------------------------------------------------------------------------
# Test 7: a still-pending check is not green either; the Daemon re-runs the
# gate next Fire rather than approving mid-CI.
#-------------------------------------------------------------------------------
test_refused_when_checks_pending() {
    echo "TEST: --check-state refuses while checks are pending"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"
    printf '[{"name":"test","bucket":"pending"}]\n' > "${dir}/checks.json"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 --check-state 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "checks-not-green" ]; then
        fail "pending checks must refuse the merge" "rc=${rc} out=${out}"
        return
    fi

    pass "--check-state refuses while checks are pending"
}

#-------------------------------------------------------------------------------
# Test 8: unreadable check state fails SAFE: no check list, no approval.
#-------------------------------------------------------------------------------
test_refused_when_checks_unreadable() {
    echo "TEST: --check-state refuses when check state is unreadable"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"
    rm -f "${dir}/checks.json"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 --check-state 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "checks-unreadable" ]; then
        fail "unreadable checks must refuse the merge" "rc=${rc} out=${out}"
        return
    fi

    pass "--check-state refuses when check state is unreadable"
}

#-------------------------------------------------------------------------------
# Test 8b: an EMPTY check array is not green: it means nothing ran (the gate
# fired seconds after the push, or a required workflow never queued). An admin
# merge also bypasses branch protection's required-check list, so approving here
# would land a docs PR with zero CI signal.
#-------------------------------------------------------------------------------
test_refused_when_checks_empty() {
    echo "TEST: --check-state refuses when no checks ran at all"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"
    printf '[]\n' > "${dir}/checks.json"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 --check-state 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "checks-missing" ]; then
        fail "an empty check list must refuse with reason checks-missing" "rc=${rc} out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "true" ]; then
        fail "verdict must stay docsOnly true; the refusal is about CI" "out=${out}"
        return
    fi

    pass "--check-state refuses when no checks ran at all"
}

#-------------------------------------------------------------------------------
# Test 8c: the config's required_checks must each have run and PASSED. An
# absent one, or one that merely skipped, refuses with checks-missing; a passed
# one approves; an empty list demands no named check. Without --check-state
# the list is not consulted at all.
#-------------------------------------------------------------------------------
test_required_checks_from_config() {
    echo "TEST: required_checks must be present and pass"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"
    local required; required="$(cfg '["Conventional PR title"]')"

    local out rc
    printf '[{"name":"test","bucket":"pass"}]\n' > "${dir}/checks.json"
    out="$(HARNESS_CONFIG_JSON="${required}" run_gate "${dir}" --head abc123 --pr 590 --check-state 2>/dev/null)"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "checks-missing" ]; then
        fail "an absent required check must refuse with checks-missing" "rc=${rc} out=${out}"
        return
    fi

    printf '[{"name":"Conventional PR title","bucket":"skipping"},{"name":"test","bucket":"pass"}]\n' > "${dir}/checks.json"
    out="$(HARNESS_CONFIG_JSON="${required}" run_gate "${dir}" --head abc123 --pr 590 --check-state 2>/dev/null)"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "checks-missing" ]; then
        fail "a skipped required check must refuse with checks-missing" "rc=${rc} out=${out}"
        return
    fi

    printf '[{"name":"Conventional PR title","bucket":"pass"},{"name":"test","bucket":"pass"}]\n' > "${dir}/checks.json"
    out="$(HARNESS_CONFIG_JSON="${required}" run_gate "${dir}" --head abc123 --pr 590 --check-state)"
    rc=$?
    if [ "${rc}" -ne 0 ] || [ "$(printf '%s' "${out}" | jq -r '.reason // "none"')" != "none" ]; then
        fail "a passed required check approves" "rc=${rc} out=${out}"
        return
    fi

    # Empty list: the every-check-green rule alone decides.
    printf '[{"name":"test","bucket":"pass"}]\n' > "${dir}/checks.json"
    out="$(run_gate "${dir}" --head abc123 --pr 590 --check-state)"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "an empty required_checks demands no named check" "rc=${rc} out=${out}"
        return
    fi

    # Without --check-state a missing required check is not consulted.
    out="$(HARNESS_CONFIG_JSON="${required}" run_gate "${dir}" --head abc123 --pr 590)"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "required_checks apply only under --check-state" "rc=${rc} out=${out}"
        return
    fi

    pass "required_checks must be present and pass"
}

#-------------------------------------------------------------------------------
# Test 9: the gate is a pure decision: no flag, no check state and no verdict
# makes it mutate anything. It only ever reads.
#-------------------------------------------------------------------------------
test_gate_never_mutates() {
    echo "TEST: the gate never merges, whatever the verdict"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"

    run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 >/dev/null 2>&1
    run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 --check-state >/dev/null 2>&1
    run_gate "${dir}" --head abc123 --pr 590 >/dev/null 2>&1

    if grep -qE "pr merge|push|api .* -X" "${dir}/gh-calls" \
        || grep -qE "(^|[[:space:]])(push|commit|merge)([[:space:]]|$)" "${dir}/git-calls"; then
        fail "the gate must never mutate" \
            "gh: $(cat "${dir}/gh-calls") git: $(cat "${dir}/git-calls")"
        return
    fi

    pass "the gate never merges, whatever the verdict"
}

#-------------------------------------------------------------------------------
# Test 10: missing/unknown args are a gate ERROR: exit 1 like every refusal,
# but the JSON reason is "usage", which the pickup skill must report as a
# harness bug rather than as a verdict about the PR. So is an unresolvable
# Harness config: the gate cannot name the repo it would merge into.
#-------------------------------------------------------------------------------
test_missing_args_usage_error() {
    echo "TEST: missing args and a missing config report reason usage"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 2>/dev/null)"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "usage" ]; then
        fail "a missing --pr must exit 1 with reason usage" "rc=${rc} out=${out}"
        return
    fi

    out="$(run_gate "${dir}" --bogus x 2>/dev/null)"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "usage" ]; then
        fail "an unknown arg must exit 1 with reason usage" "rc=${rc} out=${out}"
        return
    fi

    out="$(run_gate "${dir}" --head abc123 --pr 590 --repo acme/widgets 2>/dev/null)"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "usage" ]; then
        fail "--repo is gone: the slug comes from the config" "rc=${rc} out=${out}"
        return
    fi

    out="$(HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= run_gate "${dir}" --head abc123 --pr 590 2>"${dir}/err")"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "usage" ] \
        || ! grep -q 'no Harness config' "${dir}/err"; then
        fail "no resolvable config must exit 1 with reason usage" "rc=${rc} out=${out} err=$(cat "${dir}/err")"
        return
    fi
    if [ -s "${dir}/git-calls" ]; then
        fail "no config must stop the gate before any git call" "calls: $(cat "${dir}/git-calls")"
        return
    fi

    pass "missing args and a missing config report reason usage"
}

#-------------------------------------------------------------------------------
# Test 11: the head object is not in the local clone (a Fire fetches only the
# default branch; the PR branch may be pruned or pushed from another checkout).
# A three-dot diff against a missing sha would fail, or worse, diff nothing, so
# the gate says so explicitly: reason head-missing, no diff attempted. The
# caller must NOT read this as a "not docs-only" refusal.
#-------------------------------------------------------------------------------
test_missing_head_object_is_gate_error() {
    echo "TEST: a missing head object reports reason head-missing"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    printf 'docs/research/577-merge-recipe.md\n' > "${dir}/diff.out"
    : > "${dir}/head-missing"

    local out rc
    out="$(run_gate "${dir}" --base origin/trunk --head abc123 --pr 590 --check-state 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ]; then
        fail "a missing head object must exit 1" "rc=${rc} out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.docsOnly')" != "false" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "head-missing" ]; then
        fail "verdict must be docsOnly false with reason head-missing" "out=${out}"
        return
    fi
    if ! grep -q -- "^cat-file -e abc123\^{commit}$" "${dir}/git-calls"; then
        fail "gate must probe the head object before diffing" \
            "calls: $(cat "${dir}/git-calls")"
        return
    fi
    if grep -q -- "diff" "${dir}/git-calls" || [ -s "${dir}/gh-calls" ]; then
        fail "an absent head must stop the gate before diff/check read" \
            "git: $(cat "${dir}/git-calls") gh: $(cat "${dir}/gh-calls")"
        return
    fi

    pass "a missing head object reports reason head-missing"
}

#-------------------------------------------------------------------------------
# Run suite
#-------------------------------------------------------------------------------
echo "docs-only-gate.sh tests"

test_docs_only_verdict
test_base_defaults_to_default_branch
test_merge_cmd_is_the_shared_recipe
test_non_docs_path_refused
test_prefix_comes_from_config
test_rename_into_docs_research_refused
test_empty_diff_refused
test_approved_when_checks_green
test_refused_when_checks_red
test_refused_when_checks_pending
test_refused_when_checks_unreadable
test_refused_when_checks_empty
test_required_checks_from_config
test_gate_never_mutates
test_missing_args_usage_error
test_missing_head_object_is_gate_error

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
