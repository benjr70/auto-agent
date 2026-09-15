#!/usr/bin/env bash
# Tests for lib/runbook-check.sh
#
# Run: bash lib/runbook-check.test.sh
#
# Strategy: the checker's public interface is (a) its exit code + report over a
# plugin dir and (b) `--list`, the machine-readable tables of the rules it
# enforces. Tests drive it against the REAL plugin (the shipped prose must
# satisfy every rule and carry no literal) and against mutated temp copies:
# one rule phrase deleted at a time, and one forbidden literal injected at a
# time, proving a future edit to any skill cannot silently drop a load-bearing
# sentence or paste a Target Project fact back in (issue #28 behaviour 1,
# AC 2). No network, no gh, no writes outside mktemp.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHECKER="${SCRIPT_DIR}/runbook-check.sh"
PLUGIN="${ROOT_DIR}/plugin"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# copy_plugin -> a temp copy of skills/, agents/, hooks/ only
copy_plugin() {
    local dir; dir="$(mktemp -d)"
    cp -r "${PLUGIN}/skills" "${PLUGIN}/agents" "${PLUGIN}/hooks" "${dir}/"
    echo "${dir}"
}

# mutate_without <file> <pattern> : rewrite <file> whitespace-normalized with
# every occurrence of the regex deleted (the checker normalizes the same way).
mutate_without() {
    local file="$1" pattern="$2" d out
    d=$'\001'
    out="$(tr '\n' ' ' < "${file}" | tr -s '[:space:]' ' ' | sed -E "s${d}${pattern}${d}${d}gI")"
    printf '%s\n' "${out}" > "${file}"
}

echo "TEST: the shipped plugin satisfies every rule and carries no forbidden literal (issue #28 AC 2)"
out="$(bash "${CHECKER}" 2>&1)"; rc=$?
t="checker exits 0 on the shipped plugin (default path)"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "exit ${rc}; $(printf '%s\n' "${out}" | grep -E '^(MISSING|FORBIDDEN)' | head -8)"; fi
out="$(bash "${CHECKER}" "${PLUGIN}" 2>&1)"; rc=$?
t="checker exits 0 on the shipped plugin (explicit path)"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "exit ${rc}"; fi

echo "TEST: --list publishes both tables"
listing="$(bash "${CHECKER}" --list)"; rc=$?
t="--list exits 0 with rule and literal lines"
if [ "${rc}" -eq 0 ] && printf '%s\n' "${listing}" | grep -q $'^rule\t' && printf '%s\n' "${listing}" | grep -q $'^literal\t'; then pass "$t"; else fail "$t" "exit ${rc}"; fi
malformed="$(printf '%s\n' "${listing}" | grep -Evc $'^rule\t[a-zA-Z/._-]+: [a-z-]+\t.+$|^literal\t[a-z-]+\t.+\t.+$')"
t="every --list line is well formed"
if [ "${malformed}" -eq 0 ]; then pass "$t"; else fail "$t" "${malformed} malformed line(s)"; fi
t="rules cover every core-loop skill, every agent and both hooks"
missing_files=()
for f in skills/afk-pickup/SKILL.md skills/afk-dispatch/SKILL.md skills/pr-watch/SKILL.md skills/pr-review/SKILL.md skills/pr-reconcile/SKILL.md agents/implementer.md agents/reviewer.md agents/verifier.md hooks/smoke-trailer.sh hooks/review-gate.sh; do
    printf '%s\n' "${listing}" | grep -q $'^rule\t'"${f}: " || missing_files+=("${f}")
done
if [ "${#missing_files[@]}" -eq 0 ]; then pass "$t"; else fail "$t" "no rules for: ${missing_files[*]}"; fi
t="the literal table names the repo slug, the default branch, app names, ports, Smart Smoker paths, Agent Teams and unnamespaced chaining"
ids="$(printf '%s\n' "${listing}" | awk -F'\t' '$1=="literal"{print $2}' | sort | tr '\n' ' ')"
if [ "${ids}" = "agent-teams app-name default-branch-literal lockfile-literal port-or-host repo-slug research-path-literal smart-smoker-path unnamespaced-chain " ]; then pass "$t"; else fail "$t" "${ids}"; fi

echo "TEST: deleting any single rule phrase fails the check by name (issue #28 behaviour 1)"
undetected=()
while IFS=$'\t' read -r kind spec pattern; do
    [ "${kind}" = "rule" ] || continue
    file="${spec%%: *}"; rule="${spec#*: }"
    copy="$(copy_plugin)"
    mutate_without "${copy}/${file}" "${pattern}"
    out="$(bash "${CHECKER}" "${copy}" 2>&1)"; rc=$?
    rm -rf "${copy}"
    if [ "${rc}" -ne 1 ] || ! printf '%s' "${out}" | grep -q "MISSING rule=${rule} file=${file}"; then
        undetected+=("${spec} (exit ${rc})")
    fi
done < <(bash "${CHECKER}" --list)
t="every rule deletion is detected and named"
if [ "${#undetected[@]}" -eq 0 ]; then pass "$t"; else fail "$t" "undetected: ${undetected[*]}"; fi

echo "TEST: injecting any forbidden literal into any skill, agent or hook fails the check by name (issue #28 AC 2)"
undetected=()
while IFS=$'\t' read -r kind id pattern sample; do
    [ "${kind}" = "literal" ] || continue
    for file in skills/afk-pickup/SKILL.md agents/reviewer.md hooks/review-gate.sh; do
        copy="$(copy_plugin)"
        printf '\n%s\n' "${sample}" >> "${copy}/${file}"
        out="$(bash "${CHECKER}" "${copy}" 2>&1)"; rc=$?
        rm -rf "${copy}"
        if [ "${rc}" -ne 1 ] || ! printf '%s' "${out}" | grep -q "FORBIDDEN literal=${id} file=${file}"; then
            undetected+=("${id} in ${file} (exit ${rc})")
        fi
    done
done < <(bash "${CHECKER}" --list)
t="every literal injection is detected and named"
if [ "${#undetected[@]}" -eq 0 ]; then pass "$t"; else fail "$t" "undetected: ${undetected[*]}"; fi

echo "TEST: the allowed vocabulary is not a literal"
copy="$(copy_plugin)"
printf '\n%s\n' 'Labels: AFK:in-progress, AFK:deps-failed, HITL, wayfinder:map. Branches feat/issue-<N>, research/<slug>, dependabot/npm_and_yarn. Skills /auto-agent:pr-watch and Skill(auto-agent:afk-dispatch). The main session. plugin/skills/pr-watch/SKILL.md. git diff "origin/$BASE...HEAD". <!-- pr-review-done -->' >> "${copy}/skills/afk-pickup/SKILL.md"
out="$(bash "${CHECKER}" "${copy}" 2>&1)"; rc=$?
rm -rf "${copy}"
t="labels, branch shapes, namespaced chaining, a plugin path, \$BASE and markers pass"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "$(printf '%s\n' "${out}" | grep FORBIDDEN)"; fi

echo "TEST: a missing plugin dir is a usage error (exit 2), distinct from a missing rule"
out="$(bash "${CHECKER}" /nonexistent/plugin 2>&1)"; rc=$?
t="missing plugin dir exits 2 naming the path"
if [ "${rc}" -eq 2 ] && printf '%s' "${out}" | grep -q '/nonexistent/plugin'; then pass "$t"; else fail "$t" "exit ${rc}; ${out}"; fi
copy="$(copy_plugin)"; rm "${copy}/skills/pr-review/SKILL.md"
out="$(bash "${CHECKER}" "${copy}" 2>&1)"; rc=$?
rm -rf "${copy}"
t="a ruled file that is gone exits 2 naming it"
if [ "${rc}" -eq 2 ] && printf '%s' "${out}" | grep -q 'skills/pr-review/SKILL.md'; then pass "$t"; else fail "$t" "exit ${rc}; ${out}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
