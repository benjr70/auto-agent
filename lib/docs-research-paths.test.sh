#!/usr/bin/env bash
# Tests for lib/docs-research-paths.sh
#
# Run: bash lib/docs-research-paths.test.sh
#
# Strategy: both helpers are pure (a config JSON in, a path array in), so each
# test drives them directly and asserts the printed answer.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=docs-research-paths.sh
. "${SCRIPT_DIR}/docs-research-paths.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

t="the prefix comes from the resolved config"
out="$(docs_research_prefix '{"docs_research_prefix":"notes/research/"}')"
if [ "${out}" = "notes/research/" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

t="a config without a prefix fails instead of guessing one"
if ! docs_research_prefix '{"repo":{}}' >/dev/null 2>&1; then pass "$t"; else fail "$t"; fi

t="every path under the prefix, at least one: true"
out="$(printf '["docs/research/a.md","docs/research/b/c.md"]' | docs_research_only docs/research/)"
if [ "${out}" = "true" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

t="one path outside the prefix poisons the whole set"
out="$(printf '["docs/research/a.md","apps/backend/x.ts"]' | docs_research_only docs/research/)"
if [ "${out}" = "false" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

t="an empty set is not docs-only"
out="$(printf '[]' | docs_research_only docs/research/)"
if [ "${out}" = "false" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

t="the prefix is a prefix, not a substring"
out="$(printf '["x/docs/research/a.md"]' | docs_research_only docs/research/)"
if [ "${out}" = "false" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

t="unreadable input reads as false, exit 0"
out="$(printf 'not json' | docs_research_only docs/research/)"; rc=$?
if [ "${out}" = "false" ] && [ $rc -eq 0 ]; then pass "$t"; else fail "$t" "out=${out} rc=${rc}"; fi

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
exit 0
