#!/usr/bin/env bash
# Tests for lib/token-usage.sh
#
# Run: bash lib/token-usage.test.sh
#
# Strategy: build a throwaway transcripts dir (main-session jsonl + a
# subagents/ subdir, usage-bearing and noise lines, multiple branches) and
# assert the aggregated scan JSON, the markdown body, and the create-vs-update
# post flow through a GH_BIN stub. The Harness config arrives resolved through
# HARNESS_CONFIG_JSON; the repo slug and the Target Project checkout come from
# it and nowhere else.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=token-usage.sh
. "${SCRIPT_DIR}/token-usage.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# cfg <target-dir>: the resolved config the libs read.
cfg() {
    printf '{"repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"labels","project":null,"labels":{}},"config_dir":"%s/.auto-agent"}' "$1"
}
export HARNESS_CONFIG_JSON
HARNESS_CONFIG_JSON="$(cfg /srv/target)"
export AUTO_AGENT_HOST_ENV=/nonexistent/host-env

# usage_line <branch> <out> <cacheRead> <cacheWrite>
usage_line() {
    jq -cn --arg b "$1" --argjson o "$2" --argjson cr "$3" --argjson cw "$4" \
        '{gitBranch: $b, message: {usage: {output_tokens: $o,
          cache_read_input_tokens: $cr, cache_creation_input_tokens: $cw,
          input_tokens: 5}}}'
}

make_transcripts() {
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/aaa/subagents"
    {   # main session: 2 usage turns on issue 42, 1 on the default branch,
        # 1 on a resolve Fire's research branch, plus noise
        usage_line "feat/issue-42" 100 1000 10
        usage_line "feat/issue-42" 200 2000 20
        usage_line "trunk"         50  500  5
        usage_line "research/which-merge-gate" 70 700 7
        echo '{"type":"mode","gitBranch":null}'
        echo 'not json at all'
    } > "${dir}/aaa.jsonl"
    {   # subagent of same session: issue 42, a different issue, the research
        # branch again. Noise line here too, so a non-JSON line sits in the
        # LAST file regardless of find order; guards the pipefail regression.
        usage_line "feat/issue-42" 300 3000 30
        usage_line "feat/issue-7"  40  400  4
        usage_line "research/which-merge-gate" 30 300 3
        echo 'not json either'
    } > "${dir}/aaa/subagents/agent-1.jsonl"
    echo "${dir}"
}

# Test 1: scan aggregates per issue across main + subagent files
test_scan_aggregation() {
    local dir out row
    dir="$(make_transcripts)"
    out="$(TOKEN_USAGE_DIR="${dir}" tu_scan)"
    row="$(printf '%s' "${out}" | jq -c '.issues["42"]')"
    if [ "$(printf '%s' "${row}" | jq -r '.outputTokens')" = "600" ] \
        && [ "$(printf '%s' "${row}" | jq -r '.cacheReadTokens')" = "6000" ] \
        && [ "$(printf '%s' "${row}" | jq -r '.turns')" = "3" ] \
        && [ "$(printf '%s' "${row}" | jq -r '.sessions')" = "2" ] \
        && [ "$(printf '%s' "${out}" | jq -r '.issues["7"].outputTokens')" = "40" ]; then
        pass "scan sums per issue across main + subagent files"
    else
        fail "scan sums per issue across main + subagent files" "out=${out}"
    fi
}

# Test 2: default-branch lines land in overhead, never in issues
test_scan_overhead() {
    local dir out
    dir="$(make_transcripts)"
    out="$(TOKEN_USAGE_DIR="${dir}" tu_scan)"
    if [ "$(printf '%s' "${out}" | jq -r '.overhead.outputTokens')" = "50" ] \
        && [ "$(printf '%s' "${out}" | jq -r '.issues | has("trunk")')" = "false" ] \
        && [ "$(printf '%s' "${out}" | jq -r '.research | has("trunk")')" = "false" ]; then
        pass "default-branch lines land in the overhead bucket"
    else
        fail "default-branch lines land in the overhead bucket" "out=${out}"
    fi
}

# Test 3: --issue filters the issues map (and drops research rows)
test_scan_issue_filter() {
    local dir out
    dir="$(make_transcripts)"
    out="$(TOKEN_USAGE_DIR="${dir}" tu_scan 7)"
    if [ "$(printf '%s' "${out}" | jq -r '.issues | keys | join(",")')" = "7" ] \
        && [ "$(printf '%s' "${out}" | jq -r '.research | length')" = "0" ]; then
        pass "scan --issue 7 filters to that issue only"
    else
        fail "scan --issue 7 filters to that issue only" "out=${out}"
    fi
}

# Test 4: a research branch is its own row, keyed by slug (AC 3)
test_scan_research_row() {
    local dir out row
    dir="$(make_transcripts)"
    out="$(TOKEN_USAGE_DIR="${dir}" tu_scan)"
    row="$(printf '%s' "${out}" | jq -c '.research["which-merge-gate"]')"
    if [ "$(printf '%s' "${row}" | jq -r '.outputTokens')" = "100" ] \
        && [ "$(printf '%s' "${row}" | jq -r '.turns')" = "2" ] \
        && [ "$(printf '%s' "${row}" | jq -r '.sessions')" = "2" ] \
        && [ "$(printf '%s' "${out}" | jq -r '.issues | has("which-merge-gate")')" = "false" ]; then
        pass "a research branch is grouped under .research by slug"
    else
        fail "a research branch is grouped under .research by slug" "out=${out}"
    fi
    out="$(TOKEN_USAGE_DIR="${dir}" tu_scan research/which-merge-gate)"
    if [ "$(printf '%s' "${out}" | jq -r '.research | keys | join(",")')" = "which-merge-gate" ] \
        && [ "$(printf '%s' "${out}" | jq -r '.issues | length')" = "0" ]; then
        pass "scan --branch filters to that research row only"
    else
        fail "scan --branch filters to that research row only" "out=${out}"
    fi
}

# Test 5: markdown body carries marker + humanized numbers
test_markdown_body() {
    local dir body
    dir="$(make_transcripts)"
    body="$(TOKEN_USAGE_DIR="${dir}" tu_markdown 42)"
    if printf '%s' "${body}" | grep -q '<!-- token-usage -->' \
        && printf '%s' "${body}" | grep -q '| output       | 600 |' \
        && printf '%s' "${body}" | grep -q '| cache reads  | 6.0k |' \
        && printf '%s' "${body}" | grep -q 'feat/issue-42'; then
        pass "markdown has marker, humanized counts, branch name"
    else
        fail "markdown has marker, humanized counts, branch name" "body=${body}"
    fi
    body="$(TOKEN_USAGE_DIR="${dir}" tu_markdown 9 '' research/which-merge-gate)"
    if printf '%s' "${body}" | grep -q '| output       | 100 |' \
        && printf '%s' "${body}" | grep -q 'research/which-merge-gate'; then
        pass "markdown with a research branch reads the research row"
    else
        fail "markdown with a research branch reads the research row" "body=${body}"
    fi
}

# Test 6: markdown for an unknown ticket errors (never posts empty)
test_markdown_unknown_issue() {
    local dir code
    dir="$(make_transcripts)"
    TOKEN_USAGE_DIR="${dir}" tu_markdown 999 >/dev/null 2>&1; code=$?
    if [ "${code}" -eq 2 ]; then
        pass "no data for issue: exit 2, nothing to post"
    else
        fail "no data for issue: exit 2, nothing to post" "code=${code}"
    fi
    TOKEN_USAGE_DIR="${dir}" tu_markdown 9 '' research/no-such-slug >/dev/null 2>&1; code=$?
    if [ "${code}" -eq 2 ]; then
        pass "no data for research branch: exit 2, nothing to post"
    else
        fail "no data for research branch: exit 2, nothing to post" "code=${code}"
    fi
    TOKEN_USAGE_DIR="${dir}" tu_markdown 9 '' feat/issue-9 >/dev/null 2>&1; code=$?
    if [ "${code}" -eq 2 ]; then
        pass "a --branch that is not a research branch is a usage error"
    else
        fail "a --branch that is not a research branch is a usage error" "code=${code}"
    fi
}

# gh stub for post tests: records method+path, serves the comment list.
make_gh_stub() {
    local dir="$1"
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
args="\$*"
echo "\${args%%-f body=*}" >> "${dir}/gh-calls"
case "\${args}" in
    *"--method PATCH"*|*"--method POST"*) exit 0 ;;
    *"/comments --paginate"*) cat "${dir}/comments.out" ;;
esac
STUB
    chmod +x "${dir}/gh-stub"
}

# Test 7: post creates when no marked comment exists, on the config's repo
test_post_creates() {
    local dir out
    dir="$(make_transcripts)"
    make_gh_stub "${dir}"
    echo '' > "${dir}/comments.out"
    out="$(TOKEN_USAGE_DIR="${dir}" GH_BIN="${dir}/gh-stub" tu_post 42)"
    if printf '%s' "${out}" | grep -q 'posted comment on #42' \
        && grep -q -- '--method POST repos/acme/widgets/issues/42/comments' "${dir}/gh-calls"; then
        pass "no marked comment: POST create on the config's repo"
    else
        fail "no marked comment: POST create on the config's repo" "out=${out} calls=$(cat "${dir}/gh-calls")"
    fi
}

# Test 8: post updates in place when the marked comment exists
test_post_updates() {
    local dir out
    dir="$(make_transcripts)"
    make_gh_stub "${dir}"
    echo '31337' > "${dir}/comments.out"   # stub emulates gh --jq output: the id
    out="$(TOKEN_USAGE_DIR="${dir}" GH_BIN="${dir}/gh-stub" tu_post 42)"
    if printf '%s' "${out}" | grep -q 'updated comment on #42' \
        && grep -q -- '--method PATCH repos/acme/widgets/issues/comments/31337' "${dir}/gh-calls"; then
        pass "marked comment exists: PATCH same comment"
    else
        fail "marked comment exists: PATCH same comment" "out=${out} calls=$(cat "${dir}/gh-calls")"
    fi
}

# Test 9: a resolve Fire posts the research row onto its Decision ticket
test_post_research() {
    local dir out
    dir="$(make_transcripts)"
    make_gh_stub "${dir}"
    echo '' > "${dir}/comments.out"
    out="$(TOKEN_USAGE_DIR="${dir}" GH_BIN="${dir}/gh-stub" tu_post 9 '' research/which-merge-gate)"
    if printf '%s' "${out}" | grep -q 'posted comment on #9' \
        && grep -q -- '--method POST repos/acme/widgets/issues/9/comments' "${dir}/gh-calls"; then
        pass "post --branch lands the research row on the given issue"
    else
        fail "post --branch lands the research row on the given issue" "out=${out} calls=$(cat "${dir}/gh-calls")"
    fi
}

# Test 10: no config where one is needed is a usage error (exit 2)
test_no_config() {
    local dir code
    dir="$(make_transcripts)"
    make_gh_stub "${dir}"
    TOKEN_USAGE_DIR="${dir}" GH_BIN="${dir}/gh-stub" HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= \
        tu_post 42 >/dev/null 2>"${dir}/err"; code=$?
    if [ "${code}" -eq 2 ] && grep -q 'no Harness config' "${dir}/err" && [ ! -f "${dir}/gh-calls" ]; then
        pass "post without a config: exit 2, nothing posted"
    else
        fail "post without a config: exit 2, nothing posted" "code=${code} err=$(cat "${dir}/err")"
    fi
    # scan with an explicit dir never needs the config
    if TOKEN_USAGE_DIR="${dir}" HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= tu_scan >/dev/null 2>&1; then
        pass "scan with TOKEN_USAGE_DIR set needs no config"
    else
        fail "scan with TOKEN_USAGE_DIR set needs no config"
    fi
}

# Test 11: the default transcripts dir is derived from the Target Project
#          checkout named by the config, with Claude Code's cwd encoding
test_default_dir_from_config() {
    local home target enc src out
    home="$(mktemp -d)"
    target="${home}/work/my.target"
    mkdir -p "${target}/.auto-agent"
    enc="$(printf '%s' "${target}" | sed 's|[/.]|-|g')"
    mkdir -p "${home}/.claude/projects/${enc}"
    src="$(make_transcripts)"
    cp "${src}/aaa.jsonl" "${home}/.claude/projects/${enc}/"
    out="$(HOME="${home}" TOKEN_USAGE_DIR= HARNESS_CONFIG_JSON="$(cfg "${target}")" tu_scan)"
    if [ "$(printf '%s' "${out}" | jq -r '.issues["42"].outputTokens')" = "300" ]; then
        pass "default transcripts dir derives from the config's Target Project checkout"
    else
        fail "default transcripts dir derives from the config's Target Project checkout" "out=${out}"
    fi
}

# Test 12: the CLI form dispatches scan/markdown/post with --issue/--branch
test_cli() {
    local dir out code
    dir="$(make_transcripts)"
    out="$(TOKEN_USAGE_DIR="${dir}" bash "${SCRIPT_DIR}/token-usage.sh" scan --branch research/which-merge-gate)"
    if [ "$(printf '%s' "${out}" | jq -r '.research["which-merge-gate"].outputTokens')" = "100" ]; then
        pass "CLI scan --branch"
    else
        fail "CLI scan --branch" "out=${out}"
    fi
    TOKEN_USAGE_DIR="${dir}" bash "${SCRIPT_DIR}/token-usage.sh" scan --issue 7 --branch research/x >/dev/null 2>&1; code=$?
    if [ "${code}" -eq 2 ]; then
        pass "CLI scan refuses --issue with --branch"
    else
        fail "CLI scan refuses --issue with --branch" "code=${code}"
    fi
    out="$(TOKEN_USAGE_DIR="${dir}" bash "${SCRIPT_DIR}/token-usage.sh" markdown --issue 9 --branch research/which-merge-gate)"
    if printf '%s' "${out}" | grep -q 'research/which-merge-gate'; then
        pass "CLI markdown --issue --branch"
    else
        fail "CLI markdown --issue --branch" "out=${out}"
    fi
}

echo "token-usage.sh tests:"
test_scan_aggregation
test_scan_overhead
test_scan_issue_filter
test_scan_research_row
test_markdown_body
test_markdown_unknown_issue
test_post_creates
test_post_updates
test_post_research
test_no_config
test_default_dir_from_config
test_cli

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
