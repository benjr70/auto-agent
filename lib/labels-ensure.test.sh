#!/usr/bin/env bash
# Tests for lib/labels-ensure.sh
#
# Run: bash lib/labels-ensure.test.sh
#
# Strategy: drive the CLI with a GH_BIN stub that serves a canned label list
# and records every call. Assert the summary line, the exit code, and that
# only absent labels are created, never with --force, always with an explicit
# colour (the curated-metadata guarantee the skills used to spell by hand).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/labels-ensure.sh"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env
export HARNESS_CONFIG_JSON='{"config_dir":"/srv/t/.auto-agent","repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"labels","project":null,"labels":{}}}'

make_env() {
    local dir; dir="$(mktemp -d)"
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "${dir}/gh-calls"
case "\$*" in
    "label list "*) [ -f "${dir}/list-fails" ] && exit 1; cat "${dir}/labels.txt" ;;
    "label create "*) [ -f "${dir}/create-fails" ] && exit 1; exit 0 ;;
    *) exit 1 ;;
esac
STUB
    chmod +x "${dir}/gh-stub"; : > "${dir}/gh-calls"
    printf 'AFK\nHITL\nbug\nwayfinder:map\n' > "${dir}/labels.txt"
    echo "${dir}"
}

echo "TEST: --list prints the table without gh"
table="$(bash "${LIB}" --list)"; rc=$?
n="$(printf '%s\n' "${table}" | wc -l)"
t="exit 0, 19 rows of name/color/description, AFK and wayfinder:research among them"
if [ "${rc}" -eq 0 ] && [ "${n}" -eq 19 ] && printf '%s\n' "${table}" | grep -q $'^AFK\t1D76DB\t' && printf '%s\n' "${table}" | grep -q $'^wayfinder:research\t'; then pass "$t"; else fail "$t" "rc=${rc} rows=${n}"; fi
bad="$(printf '%s\n' "${table}" | grep -Evc $'^[A-Za-z:-]+\t[0-9A-F]{6}\t.+$')"
t="every row has a name, a 6-hex colour and a description"
if [ "${bad}" -eq 0 ]; then pass "$t"; else fail "$t" "${bad} malformed rows"; fi

echo "TEST: only absent labels are created, each with an explicit colour and never --force"
dir="$(make_env)"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}")"; rc=$?
t="exit 0 and the summary counts 16 created, 3 present"
if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^labels-ensure: created 16, present 3$'; then pass "$t"; else fail "$t" "rc=${rc} $(printf '%s\n' "${out}" | tail -1)"; fi
t="AFK, HITL and wayfinder:map were not re-created"
if ! grep -Eq '^label create (AFK|HITL|wayfinder:map) ' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "$(grep 'label create' "${dir}/gh-calls" | head -3)"; fi
t="spec and AFK:in-progress were created against the config's repo with a colour"
if grep -q '^label create spec --repo acme/widgets --color 0052CC --description ' "${dir}/gh-calls" && grep -q '^label create AFK:in-progress --repo acme/widgets --color FBCA04 ' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "$(grep 'label create' "${dir}/gh-calls" | head -3)"; fi
t="no call carries --force"
if ! grep -q -- '--force' "${dir}/gh-calls"; then pass "$t"; else fail "$t"; fi
t="one 'created <name>' line per created label"
if [ "$(printf '%s\n' "${out}" | grep -c '^labels-ensure: created [A-Za-z:-]*$')" -eq 16 ]; then pass "$t"; else fail "$t" "$(printf '%s\n' "${out}" | grep -c created)"; fi
rm -rf "${dir}"

echo "TEST: everything already present is a no-op"
dir="$(make_env)"; bash "${LIB}" --list | cut -f1 > "${dir}/labels.txt"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}")"; rc=$?
t="exit 0, created 0, present 19, no create call"
if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^labels-ensure: created 0, present 19$' && ! grep -q 'label create' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"

echo "TEST: failures"
dir="$(make_env)"; touch "${dir}/list-fails"
GH_BIN="${dir}/gh-stub" bash "${LIB}" >/dev/null 2>&1; rc=$?
t="unreadable label list exits 1"; if [ "${rc}" -eq 1 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -f "${dir}/list-fails"; touch "${dir}/create-fails"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}" 2>/dev/null)"; rc=$?
t="a failed create exits 1 after trying every label"; if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q 'created 0, present 3' && [ "$(grep -c 'label create' "${dir}/gh-calls")" -eq 16 ]; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= GH_BIN="${dir}/gh-stub" bash "${LIB}" >/dev/null 2>&1; rc=$?
t="no Harness config exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${dir}"

echo ""; echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
[ "${TESTS_FAILED}" -eq 0 ] || { for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done; exit 1; }
exit 0
