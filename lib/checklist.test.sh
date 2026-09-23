#!/usr/bin/env bash
# Tests for lib/checklist.sh
#
# Run: bash lib/checklist.test.sh
#
# Strategy: both halves are pure text transforms, so every case is a PR body in
# a temp file. The cases are the ones that protect a human's PR: a `- [ ]` in a
# section that is not a verification section is invisible to both halves, a
# ticked box is never read back or un-ticked, and only the items handed in as
# passed are flipped.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/checklist.sh"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

BODY="${WORK}/body.md"
cat > "${BODY}" <<'MD'
Closes #12

## Summary

What this does.

## Acceptance criteria

- [ ] an acceptance box nobody verifies
- [x] a ticked acceptance box

## Manual verification

- [ ] the item list shows every posted item
- [ ] the health route answers while the process is up
- [x] already signed off by a human

## Human verification required

- [ ] the panel shows the reading on real hardware

## Notes

Nothing to see.
MD

echo "TEST: parse reads both verification sections, unchecked items only"
out="$(bash "${LIB}" parse "${BODY}")"
want='manual	the item list shows every posted item
manual	the health route answers while the process is up
human	the panel shows the reading on real hardware'
if [ "${out}" = "${want}" ]; then pass "parse: sections and tags"; else fail "parse: sections and tags" "got: ${out}"; fi

echo "TEST: parse reads stdin when no file is given"
out="$(bash "${LIB}" parse < "${BODY}" | wc -l | tr -d ' ')"
if [ "${out}" = "3" ]; then pass "parse: stdin"; else fail "parse: stdin" "got: ${out}"; fi

echo "TEST: a body with no verification section parses to nothing, and that is not an error"
printf '## Summary\n\n- [ ] not a verification item\n' > "${WORK}/plain.md"
out="$(bash "${LIB}" parse "${WORK}/plain.md")"; rc=$?
if [ "${rc}" -eq 0 ] && [ -z "${out}" ]; then pass "parse: nothing to verify"; else fail "parse: nothing to verify" "rc=${rc} got: ${out}"; fi

echo "TEST: tick flips exactly the items handed in as passed"
new="$(printf '%s\n' 'the item list shows every posted item' | bash "${LIB}" tick "${BODY}")"
if printf '%s' "${new}" | grep -q -- '- \[x\] the item list shows every posted item'; then pass "tick: the passing box"; else fail "tick: the passing box" "got: ${new}"; fi
if printf '%s' "${new}" | grep -q -- '- \[ \] the health route answers while the process is up'; then pass "tick: an item that did not pass keeps its box"; else fail "tick: an item that did not pass keeps its box"; fi
if printf '%s' "${new}" | grep -q -- '- \[ \] the panel shows the reading on real hardware'; then pass "tick: a deferred item keeps its box"; else fail "tick: a deferred item keeps its box"; fi

echo "TEST: tick never touches a box outside the verification sections"
if printf '%s' "${new}" | grep -q -- '- \[ \] an acceptance box nobody verifies'; then pass "tick: other sections are untouched"; else fail "tick: other sections are untouched"; fi

echo "TEST: tick never un-ticks a box"
new2="$(printf '%s\n' 'nothing that appears anywhere' | bash "${LIB}" tick "${BODY}")"
if printf '%s' "${new2}" | grep -q -- '- \[x\] already signed off by a human'; then pass "tick: a ticked box stays ticked"; else fail "tick: a ticked box stays ticked"; fi

echo "TEST: an empty pass list leaves the body unchanged"
printf '' | bash "${LIB}" tick "${BODY}" > "${WORK}/same.md"
if diff -q "${BODY}" "${WORK}/same.md" >/dev/null; then pass "tick: empty pass list"; else fail "tick: empty pass list" "$(diff "${BODY}" "${WORK}/same.md")"; fi

echo "TEST: a coincidentally identical box elsewhere is not flipped"
cat > "${WORK}/twins.md" <<'MD'
## Acceptance criteria

- [ ] the same words

## Manual verification

- [ ] the same words
MD
new3="$(printf '%s\n' 'the same words' | bash "${LIB}" tick "${WORK}/twins.md")"
if [ "$(printf '%s' "${new3}" | grep -c -- '- \[x\] the same words')" = "1" ]; then pass "tick: only the verification copy"; else fail "tick: only the verification copy" "got: ${new3}"; fi

echo "TEST: indentation and the list marker are preserved verbatim"
cat > "${WORK}/indent.md" <<'MD'
## Manual verification

  * [ ] an indented star item
MD
new4="$(printf '%s\n' 'an indented star item' | bash "${LIB}" tick "${WORK}/indent.md")"
if printf '%s' "${new4}" | grep -q -- '^  \* \[x\] an indented star item$'; then pass "tick: marker and indent"; else fail "tick: marker and indent" "got: ${new4}"; fi

echo "TEST: a missing body file and an unknown subcommand are usage errors"
bash "${LIB}" tick "${WORK}/nope.md" >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: missing body file" || fail "usage: missing body file"
bash "${LIB}" tick >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: tick needs a file" || fail "usage: tick needs a file"
bash "${LIB}" bogus >/dev/null 2>&1; [ $? -eq 2 ] && pass "usage: unknown subcommand" || fail "usage: unknown subcommand"

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then printf '  - %s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
