#!/usr/bin/env bash
# Tests for lib/deps-gate.sh
#
# Run: bash lib/deps-gate.test.sh
#
# Strategy: the gate is a runnable CLI whose only outside contact is `gh pr
# view` and `gh pr checks`, both served by one stub binary (GH_BIN) in a temp
# dir; no test touches the network or real PR state. The Harness config arrives
# resolved through HARNESS_CONFIG_JSON. Every fixture starts from the one
# APPROVING fixture and changes exactly one field, so each test names the
# single condition that flips the verdict; that is what keeps the refusal
# reasons from drifting into each other. Assertions cover the stdout JSON
# verdict (approved / sha / mergeCmd / reason) and the two-value exit code,
# the contract the lane's single merge call site consumes.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="${SCRIPT_DIR}/deps-gate.sh"

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

# The mergeable head sha every fixture is built around.
SHA='dead0beef'

# The resolved config every test runs under, as harness_config_load prints it.
# cfg [required_checks JSON array]
cfg() {
    printf '{"config_dir":"/srv/target/.auto-agent","repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"labels","project":null,"labels":{}},"docs_research_prefix":"docs/research/","required_checks":%s,"rounds":{"deps_fix":3}}' "${1:-[]}"
}
export HARNESS_CONFIG_JSON
HARNESS_CONFIG_JSON="$(cfg)"
# No Host env file on this box may leak into the tests.
export AUTO_AGENT_HOST_ENV=/nonexistent/host-env

# make_env: temp dir holding a gh stub that records every call's argv in
# gh-calls, serves view.json for `pr view` and checks.json for `pr checks`, and
# fails on anything else (so an unexpected subcommand shows up as a refusal
# rather than as silence). The canned files are the APPROVING fixture: an open,
# non-draft Dependabot PR whose comments carry both markers for SHA, with a
# green check list.
make_env() {
    local dir; dir="$(mktemp -d)"
    # The stub mimics the real `gh pr checks` exit-code contract: it prints the
    # JSON and THEN exits with whatever checks-rc says (gh exits 1 when a check
    # failed, 8 when one is pending, 0 only when all are green). Fixtures that
    # write a red or pending list write the matching rc, so a gate that reads
    # the exit code instead of the payload is caught here.
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "${dir}/gh-calls"
case "\$*" in
    *"pr view"*)   cat "${dir}/view.json" 2>/dev/null || exit 1 ;;
    *"pr checks"*)
        cat "${dir}/checks.json" 2>/dev/null || exit 1
        exit "\$(cat "${dir}/checks-rc" 2>/dev/null || echo 0)" ;;
    *) exit 1 ;;
esac
STUB
    chmod +x "${dir}/gh-stub"
    : > "${dir}/gh-calls"
    write_view "${dir}"
    write_checks "${dir}" '[{"name":"Conventional PR title","bucket":"pass"},
                            {"name":"test-apps / test (backend)","bucket":"pass"},
                            {"name":"e2e / e2e (device-service)","bucket":"skipping"}]'
    echo "${dir}"
}

# write_view <dir> [jq-filter applied to the approving fixture]
#
# One place builds the approving `gh pr view --json ...` payload; a fixture that
# needs a refusal passes the single-field jq edit that causes it.
write_view() {
    local dir="$1" edit="${2:-.}"
    jq -c --arg sha "${SHA}" \
        '{state: "OPEN", isDraft: false,
          author: {login: "app/dependabot"},
          headRefName: "dependabot/npm_and_yarn/axios-1.12.0",
          headRefOid: $sha,
          title: "chore(deps): bump axios from 1.11.0 to 1.12.0",
          reviewDecision: "",
          comments: [{body: "Bumps axios.\n<!-- deps-lane tierA=green sha=\($sha) -->"},
                     {body: "<!-- deps-lane tierB=PASS sha=\($sha) -->\nTier B 6/6."}]}
         | '"${edit}" -n > "${dir}/view.json"
}

write_checks() { # write_checks <dir> <json array> [gh exit code, default 0]
    printf '%s\n' "$2" | jq -c '.' > "$1/checks.json"
    printf '%s\n' "${3:-0}" > "$1/checks-rc"
}

run_gate() { # run_gate <dir> <args...>
    local dir="$1"; shift
    GH_BIN="${dir}/gh-stub" "${GATE}" "$@"
}

# refusal_is: run the gate on <dir> and assert exit 1 + the expected reason,
# with `approved` false. Every refusal test shares this shape.
refusal_is() { # refusal_is <dir> <expected reason> <test name> [extra gate args...]
    local dir="$1" want="$2" name="$3"; shift 3
    local out rc
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}" "$@" 2>/dev/null)"
    rc=$?

    if [ "${rc}" -ne 1 ]; then
        fail "${name}" "expected exit 1, got rc=${rc} out=${out}"
        return 1
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.reason')" != "${want}" ]; then
        fail "${name}" "expected reason ${want}, out=${out}"
        return 1
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.approved')" != "false" ]; then
        fail "${name}" "a refusal must carry approved false, out=${out}"
        return 1
    fi
    pass "${name}"
    return 0
}

#-------------------------------------------------------------------------------
# Test 1: the approving case: an open, non-draft Dependabot PR whose markers
# vouch for the head sha, with green checks and a deps title. Exit 0, no reason,
# and the merge command is the admin squash pinned to that exact sha, carrying
# the config's repo so the caller can run it from any cwd. The gate hands the
# command over; it never runs it.
#-------------------------------------------------------------------------------
test_approving_verdict() {
    echo "TEST: a green, marker-vouched Dependabot PR is approved with a pinned merge command"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    local out rc
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}")"
    rc=$?

    if [ "${rc}" -ne 0 ]; then
        fail "an approvable PR must exit 0" "rc=${rc} out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.approved')" != "true" ] \
        || [ "$(printf '%s' "${out}" | jq -r '.sha')" != "${SHA}" ]; then
        fail "verdict must carry approved true and the head sha" "out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.reason // "none"')" != "none" ]; then
        fail "an approved verdict must carry no reason" "out=${out}"
        return
    fi
    if [ "$(printf '%s' "${out}" | jq -r '.mergeCmd')" \
        != "gh pr merge 636 --repo acme/widgets --squash --admin --match-head-commit ${SHA}" ]; then
        fail "mergeCmd must be the admin squash pinned to sha, carrying the config's repo" "out=${out}"
        return
    fi
    if grep -q "pr merge" "${dir}/gh-calls"; then
        fail "the gate must never merge; the caller runs mergeCmd" \
            "calls: $(cat "${dir}/gh-calls")"
        return
    fi

    pass "a green, marker-vouched Dependabot PR is approved with a pinned merge command"
}

#-------------------------------------------------------------------------------
# Test 1b: the merge recipe is the one harness_merge_recipe prints, byte for
# byte, and so identical to the docs gate's for the same PR and sha. The two
# gates can never drift apart.
#-------------------------------------------------------------------------------
test_merge_cmd_is_the_shared_recipe() {
    echo "TEST: mergeCmd is harness_merge_recipe verbatim, shared with the docs gate"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    local out want docs
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}" | jq -r '.mergeCmd')"
    # shellcheck source=harness-config.sh
    want="$(. "${SCRIPT_DIR}/harness-config.sh"; harness_merge_recipe acme/widgets 636 "${SHA}")"
    if [ "${out}" != "${want}" ]; then
        fail "mergeCmd must equal the shared recipe" "out=${out} want=${want}"
        return
    fi

    # The docs gate, asked about the same PR and sha, prints the same command.
    cat > "${dir}/git-stub" <<STUB
#!/usr/bin/env bash
case "\$*" in "cat-file"*) exit 0 ;; esac
echo docs/research/one.md
STUB
    chmod +x "${dir}/git-stub"
    docs="$(GIT_BIN="${dir}/git-stub" GH_BIN="${dir}/gh-stub" "${SCRIPT_DIR}/docs-only-gate.sh" \
        --pr 636 --head "${SHA}" | jq -r '.mergeCmd')"
    if [ "${docs}" != "${out}" ]; then
        fail "the docs gate and the deps gate must print one recipe" "deps=${out} docs=${docs}"
        return
    fi

    pass "mergeCmd is harness_merge_recipe verbatim, shared with the docs gate"
}

#-------------------------------------------------------------------------------
# Test 2: a closed PR, and a draft one, are refused draft-or-closed. A draft
# Dependabot PR is one a human deliberately parked; merging it would undo that.
#-------------------------------------------------------------------------------
test_draft_or_closed_refused() {
    echo "TEST: a closed or draft PR is refused"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    write_view "${dir}" '.state = "CLOSED"'
    refusal_is "${dir}" draft-or-closed "a closed PR is refused" || return

    write_view "${dir}" '.isDraft = true'
    refusal_is "${dir}" draft-or-closed "a draft PR is refused" || return
}

#-------------------------------------------------------------------------------
# Test 3: the Dependabot test is BOTH the app login and the branch prefix, and
# it must agree with pr-triage.sh: login `app/dependabot`, branch
# `dependabot/...`. A human account renamed `dependabot`, or the app's login on
# somebody else's branch, must not inherit the lane's admin merge.
#-------------------------------------------------------------------------------
test_not_dependabot_refused() {
    echo "TEST: a non-Dependabot author or branch is refused"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    write_view "${dir}" '.author.login = "a-human"'
    refusal_is "${dir}" not-dependabot "a human author is refused" || return

    write_view "${dir}" '.headRefName = "feat/issue-656"'
    refusal_is "${dir}" not-dependabot "a non-dependabot branch is refused" || return

    # The login test is case-insensitive but exact: `dependabot[bot]`, the login
    # the commit author carries, is NOT the app's listing login and buys nothing.
    write_view "${dir}" '.author.login = "dependabot[bot]"'
    refusal_is "${dir}" not-dependabot "a lookalike login is refused" || return

    write_view "${dir}" '.author.login = "APP/Dependabot"'
    local out rc
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}")"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "the app login is matched case-insensitively" "rc=${rc} out=${out}"
        return
    fi
    pass "the app login is matched case-insensitively"
}

#-------------------------------------------------------------------------------
# Test 4: markers-stale. Three ways the markers stop vouching for what would
# merge: a missing tier A marker, a missing tier B marker, and markers that
# carry a different sha than --head. Plus the PR moving under the gate
# (headRefOid != --head): the verification was earned by code that is no longer
# the head, which is the exact failure --match-head-commit exists to prevent.
#-------------------------------------------------------------------------------
test_markers_stale_refused() {
    echo "TEST: absent, foreign or moved-past markers are refused"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    write_view "${dir}" '.comments = [.comments[1]]'
    refusal_is "${dir}" markers-stale "a missing tierA marker is refused" || return

    write_view "${dir}" '.comments = [.comments[0]]'
    refusal_is "${dir}" markers-stale "a missing tierB marker is refused" || return

    write_view "${dir}" '.comments |= map(.body |= gsub("sha=[a-z0-9]+"; "sha=00old00"))'
    refusal_is "${dir}" markers-stale "markers for another sha are refused" || return

    write_view "${dir}" '.headRefOid = "newpush99"'
    refusal_is "${dir}" markers-stale "a PR that moved past --head is refused" || return
}

#-------------------------------------------------------------------------------
# Test 5: check state, fail SAFE: the admin merge bypasses branch protection's
# required-check list, so green CI is the only thing vouching for the bump. A
# red check, a pending one, an EMPTY list (nothing ran) and an unreadable list
# all refuse, with the docs gate's exact bucket vocabulary and reason names.
#-------------------------------------------------------------------------------
test_check_state_refusals() {
    echo "TEST: red, pending, missing and unreadable checks are refused"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    # gh exits 1 on a red list and 8 on a pending one, while still printing the
    # JSON. Both must land on checks-not-green ("fix loop, or wait for CI"), NOT
    # on checks-unreadable, which tells the lane to file a harness bug.
    write_checks "${dir}" '[{"name":"Conventional PR title","bucket":"pass"},
                            {"name":"test-apps / test (backend)","bucket":"fail"}]' 1
    refusal_is "${dir}" checks-not-green "a red check is refused despite gh exit 1" || return

    write_checks "${dir}" '[{"name":"Conventional PR title","bucket":"pass"},
                            {"name":"test-apps / test (backend)","bucket":"pending"}]' 8
    refusal_is "${dir}" checks-not-green "a pending check is refused despite gh exit 8" || return

    write_checks "${dir}" '[]'
    refusal_is "${dir}" checks-missing "an empty check list is refused" || return

    rm -f "${dir}/checks.json"
    refusal_is "${dir}" checks-unreadable "an unreadable check list is refused" || return
}

#-------------------------------------------------------------------------------
# Test 6: title-not-deps. The title decides whether a release tool cuts a patch
# release for a security bump, so it must be a deps title. Both accepted
# prefixes approve.
#-------------------------------------------------------------------------------
test_title_not_deps_refused() {
    echo "TEST: a non-deps title is refused"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    write_view "${dir}" '.title = "chore(deps-dev): bump jest from 29.0.0 to 29.1.0"'
    refusal_is "${dir}" title-not-deps "a chore(deps-dev) title is refused" || return

    write_view "${dir}" '.title = "Bump axios from 1.11.0 to 1.12.0"'
    refusal_is "${dir}" title-not-deps "a non-conventional title is refused" || return

    # A security bump retitled to fix(deps): is the other accepted prefix.
    write_view "${dir}" '.title = "fix(deps): bump axios from 1.11.0 to 1.12.0"'
    local out rc
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}" --security true)"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "a fix(deps): title is accepted" "rc=${rc} out=${out}"
        return
    fi
    pass "a fix(deps): title is accepted"
}

#-------------------------------------------------------------------------------
# Test 6b: required_checks. A Target Project whose PR-title lint vouches for
# release notes names it in the config, and then the check must have actually
# run AND PASSED. A green list with no such check is refused (nothing vouches
# for the title); a SKIPPED one vouches for exactly as much as an absent one:
# the check loop counts `skipping` as green for the list as a whole (a
# path-filtered job that had nothing to do is benign), so presence alone would
# let a title nothing linted through. A RED one is refused too, as
# checks-not-green, since the whole-list green test runs first. With an empty
# list no named check is demanded.
#-------------------------------------------------------------------------------
test_required_checks_from_config() {
    echo "TEST: required_checks must be present and pass"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    local required; required="$(cfg '["Conventional PR title"]')"

    local out rc
    out="$(HARNESS_CONFIG_JSON="${required}" run_gate "${dir}" --pr 636 --head "${SHA}")"
    rc=$?
    if [ "${rc}" -ne 0 ] || [ "$(printf '%s' "${out}" | jq -r '.approved')" != "true" ]; then
        fail "a passed required check approves" "rc=${rc} out=${out}"
        return
    fi
    pass "a passed required check approves"

    write_checks "${dir}" '[{"name":"test-apps / test (backend)","bucket":"pass"}]'
    HARNESS_CONFIG_JSON="${required}" \
        refusal_is "${dir}" checks-missing "an absent required check is refused" || return

    write_checks "${dir}" '[{"name":"Conventional PR title","bucket":"skipping"},
                            {"name":"test-apps / test (backend)","bucket":"pass"}]'
    HARNESS_CONFIG_JSON="${required}" \
        refusal_is "${dir}" checks-missing "a skipped required check is refused" || return

    write_checks "${dir}" '[{"name":"Conventional PR title","bucket":"fail"},
                            {"name":"test-apps / test (backend)","bucket":"pass"}]' 1
    HARNESS_CONFIG_JSON="${required}" \
        refusal_is "${dir}" checks-not-green "a red required check is refused" || return

    # Empty list (the default config): the same absent-lint list approves.
    write_checks "${dir}" '[{"name":"test-apps / test (backend)","bucket":"pass"}]'
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}")"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "an empty required_checks demands no named check" "rc=${rc} out=${out}"
        return
    fi
    pass "an empty required_checks demands no named check"
}

#-------------------------------------------------------------------------------
# Test 7: major-unapproved. A major bump carries breaking changes by definition,
# so it needs a human review decision; a minor/patch bump does not. --major
# defaults to false, and --security never changes the verdict (it only feeds the
# caller's report line).
#-------------------------------------------------------------------------------
test_major_unapproved_refused() {
    echo "TEST: a major bump needs an approving review"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    refusal_is "${dir}" major-unapproved "an unreviewed major bump is refused" \
        --major true || return

    write_view "${dir}" '.reviewDecision = "CHANGES_REQUESTED"'
    refusal_is "${dir}" changes-requested "a changes-requested major bump is refused" \
        --major true || return

    write_view "${dir}" '.reviewDecision = "APPROVED"'
    local out rc
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}" --major true --security true)"
    rc=$?
    if [ "${rc}" -ne 0 ] || [ "$(printf '%s' "${out}" | jq -r '.approved')" != "true" ]; then
        fail "an APPROVED major bump is approved" "rc=${rc} out=${out}"
        return
    fi
    pass "an APPROVED major bump is approved"

    # An unreviewed NON-major bump is fine: the review requirement is scoped to
    # major, and --security alone must not change any verdict.
    write_view "${dir}"
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}" --major false --security true)"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "an unreviewed non-major bump is approved" "rc=${rc} out=${out}"
        return
    fi
    pass "an unreviewed non-major bump is approved"
}

#-------------------------------------------------------------------------------
# Test 7b: changes-requested. A human who reviewed the bump and asked for
# changes has rejected it; such a PR is left alone by the Daemon, and that is
# not scoped to major bumps. The review decision is consulted at every bump
# size, so a rejected minor/patch bump is refused rather than admin-squashed
# over the reviewer's objection.
#-------------------------------------------------------------------------------
test_changes_requested_refused() {
    echo "TEST: a rejected bump is refused at any bump size"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    write_view "${dir}" '.reviewDecision = "CHANGES_REQUESTED"'
    refusal_is "${dir}" changes-requested "a changes-requested non-major bump is refused" \
        --major false || return

    # ...and with --major omitted entirely (it defaults to false).
    refusal_is "${dir}" changes-requested "a changes-requested bump is refused by default" \
        || return

    # A REVIEW_REQUIRED / COMMENTED decision is not a rejection: only an explicit
    # CHANGES_REQUESTED blocks a non-major bump.
    write_view "${dir}" '.reviewDecision = "REVIEW_REQUIRED"'
    local out rc
    out="$(run_gate "${dir}" --pr 636 --head "${SHA}")"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "a REVIEW_REQUIRED non-major bump is still approved" "rc=${rc} out=${out}"
        return
    fi
    pass "a REVIEW_REQUIRED non-major bump is still approved"
}

#-------------------------------------------------------------------------------
# Test 8: bad or missing args are a gate ERROR: exit 1 like every refusal, but
# reason `usage`, which the caller must report as a harness bug rather than as a
# verdict about the PR. A --major flag that is not exactly true|false is usage
# too: reading `True` as "not major" would merge an unreviewed breaking bump.
# So is `--repo`, which is gone (the slug comes from the config), and an
# unresolvable Harness config.
#-------------------------------------------------------------------------------
test_usage_errors() {
    echo "TEST: bad or missing args report reason usage"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    local out rc
    for args in "--pr 636" "--head ${SHA}" "--bogus x" "--pr 636 --head ${SHA} --repo acme/widgets"; do
        # shellcheck disable=SC2086  # deliberate word-splitting of the arg case
        out="$(run_gate "${dir}" ${args} 2>/dev/null)"
        rc=$?
        if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "usage" ]; then
            fail "\`${args}\` must exit 1 with reason usage" "rc=${rc} out=${out}"
            return
        fi
    done

    out="$(run_gate "${dir}" --pr 636 --head "${SHA}" --major True 2>/dev/null)"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "usage" ]; then
        fail "a non true|false --major must exit 1 with reason usage" "rc=${rc} out=${out}"
        return
    fi

    out="$(HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= run_gate "${dir}" --pr 636 --head "${SHA}" 2>"${dir}/err")"
    rc=$?
    if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "usage" ] \
        || ! grep -q 'no Harness config' "${dir}/err"; then
        fail "no resolvable config must exit 1 with reason usage" "rc=${rc} out=${out} err=$(cat "${dir}/err")"
        return
    fi
    if [ -s "${dir}/gh-calls" ]; then
        fail "no config must stop the gate before any gh call" "calls: $(cat "${dir}/gh-calls")"
        return
    fi

    pass "bad or missing args report reason usage"

    # A trailing value-less flag must REFUSE, not hang. With `shift 2` on $#=1
    # and no `set -e` the arg loop used to spin forever, so a Fire whose
    # head-sha lookup came back empty blocked instead of reporting a harness
    # error. `timeout` is the assertion: rc 124 is the regression.
    for args in "--pr" "--head" "--major" "--security"; do
        out="$(GH_BIN="${dir}/gh-stub" timeout 5 "${GATE}" --pr 636 --head "${SHA}" ${args} 2>/dev/null)"
        rc=$?
        if [ "${rc}" -ne 1 ] || [ "$(printf '%s' "${out}" | jq -r '.reason')" != "usage" ]; then
            fail "a trailing \`${args}\` must refuse promptly with reason usage" \
                "rc=${rc} out=${out}"
            return
        fi
    done
    pass "a trailing value-less flag refuses with usage instead of hanging"
}

#-------------------------------------------------------------------------------
# Test 8b: --security is verdict-neutral, so its VALUE is not policed. The gate
# header promises the flag never changes the verdict; refusing `--security yes`
# with reason usage would block a mergeable bump with a harness error about a
# flag the gate only echoes to the caller's report line. --major is the one flag
# whose spelling is enforced, because misreading it auto-merges a breaking bump.
#-------------------------------------------------------------------------------
test_security_flag_is_verdict_neutral() {
    echo "TEST: --security accepts any value and never changes the verdict"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    local out rc
    for val in true false yes 1 ''; do
        out="$(run_gate "${dir}" --pr 636 --head "${SHA}" --security "${val}" 2>/dev/null)"
        rc=$?
        if [ "${rc}" -ne 0 ] || [ "$(printf '%s' "${out}" | jq -r '.approved')" != "true" ]; then
            fail "--security '${val}' must not change the verdict" "rc=${rc} out=${out}"
            return
        fi
    done

    pass "--security accepts any value and never changes the verdict"
}

#-------------------------------------------------------------------------------
# Test 9: an unreadable PR view is a harness error, not a verdict:
# checks-unreadable, the same reason an unreadable check list gets, because both
# mean "GitHub state could not be read" and neither says anything about the PR.
#-------------------------------------------------------------------------------
test_unreadable_pr_view() {
    echo "TEST: an unreadable PR view reports checks-unreadable"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN
    rm -f "${dir}/view.json"

    refusal_is "${dir}" checks-unreadable "an unreadable PR view is a harness error" || return
}

#-------------------------------------------------------------------------------
# Test 10: the gate is a pure decision. Across an approval and several refusals,
# every recorded gh call is a READ (`pr view` / `pr checks`) carrying the
# config's repo; never a merge, an edit, a comment or a mutating API call. This
# is the test that would catch the day somebody "helpfully" makes the gate merge
# what it approves.
#-------------------------------------------------------------------------------
test_gate_only_reads() {
    echo "TEST: the gate makes only read calls, whatever the verdict"

    local dir; dir="$(make_env)"
    trap "rm -rf '${dir}'" RETURN

    run_gate "${dir}" --pr 636 --head "${SHA}" >/dev/null 2>&1
    run_gate "${dir}" --pr 636 --head "${SHA}" --major true >/dev/null 2>&1
    write_view "${dir}" '.state = "CLOSED"'
    run_gate "${dir}" --pr 636 --head "${SHA}" >/dev/null 2>&1

    if grep -qE "pr merge|pr edit|pr comment|pr close|pr review|api .* -X|--method" \
        "${dir}/gh-calls"; then
        fail "the gate must never mutate" "calls: $(cat "${dir}/gh-calls")"
        return
    fi
    if grep -qvE "^pr (view|checks) " "${dir}/gh-calls"; then
        fail "every gh call must be pr view or pr checks" "calls: $(cat "${dir}/gh-calls")"
        return
    fi
    if ! grep -q "^pr view 636 --repo acme/widgets --json state,isDraft,author,headRefName,headRefOid,title,reviewDecision,comments$" \
        "${dir}/gh-calls"; then
        fail "PR state must be read in ONE pr view call carrying the config's repo" \
            "calls: $(cat "${dir}/gh-calls")"
        return
    fi
    if ! grep -q "^pr checks 636 --repo acme/widgets --json name,bucket$" \
        "${dir}/gh-calls"; then
        fail "check state must be read for the config's repo" \
            "calls: $(cat "${dir}/gh-calls")"
        return
    fi

    pass "the gate makes only read calls, whatever the verdict"
}

#-------------------------------------------------------------------------------
# Run suite
#-------------------------------------------------------------------------------
echo "deps-gate.sh tests"

test_approving_verdict
test_merge_cmd_is_the_shared_recipe
test_draft_or_closed_refused
test_not_dependabot_refused
test_markers_stale_refused
test_check_state_refusals
test_title_not_deps_refused
test_required_checks_from_config
test_major_unapproved_refused
test_changes_requested_refused
test_usage_errors
test_security_flag_is_verdict_neutral
test_unreadable_pr_view
test_gate_only_reads

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
