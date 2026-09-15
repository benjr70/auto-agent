#!/usr/bin/env bash
# Aggregate test runner for the harness's bash and python suites.
#
# Run: bash run-tests.sh [ROOT_DIR]
#
# Discovers every *.test.sh and *.test.py under ROOT_DIR (default: this repo)
# and runs each one in its own process. Every suite runs even if an earlier one
# fails; the aggregate exit code is non-zero if ANY suite failed. This is the
# single entry point CI calls, so new suites are picked up with no workflow
# edit. Carried over from Smart-Smoker-V2's scripts/claude-agent/run-tests.sh.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="${1:-${SCRIPT_DIR}}"
PYTHON_BIN="${PYTHON_BIN:-python3}"

if [ ! -d "${ROOT_DIR}" ]; then
    echo "FATAL: root directory not found: ${ROOT_DIR}" >&2
    exit 2
fi

suites=()
while IFS= read -r suite; do
    suites+=("${suite}")
done < <(find "${ROOT_DIR}" \( -path '*/.git' -o -path '*/node_modules' \) -prune -o \
              -type f \( -name '*.test.sh' -o -name '*.test.py' \) -print | sort)

if [ "${#suites[@]}" -eq 0 ]; then
    echo "No *.test.sh / *.test.py suites found under ${ROOT_DIR}"
    exit 0
fi

total=0
failed=0
failed_suites=()

for suite in "${suites[@]}"; do
    total=$((total + 1))
    echo ""
    echo ">>> RUN ${suite}"
    runner=(bash)
    case "${suite}" in
        *.test.py) runner=("${PYTHON_BIN}") ;;
    esac
    if "${runner[@]}" "${suite}"; then
        echo "<<< PASS ${suite}"
    else
        rc=$?
        echo "<<< FAIL ${suite} (exit ${rc})"
        failed=$((failed + 1))
        failed_suites+=("${suite}")
    fi
done

echo ""
echo "=========================================="
echo "Suites: ${total} | Failed: ${failed}"
echo "=========================================="

if [ "${failed}" -gt 0 ]; then
    echo "Failed suites:"
    for name in "${failed_suites[@]}"; do
        echo "  - ${name}"
    done
    exit 1
fi
exit 0
