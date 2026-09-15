#!/usr/bin/env bash
# Tests for lib/host-env.sh
#
# Run: bash lib/host-env.test.sh
#
# Strategy: point AUTO_AGENT_HOST_ENV at throwaway files and assert what a
# caller sees after host_env_load: exported keys, precedence, the State dir.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=host-env.sh
. "${SCRIPT_DIR}/host-env.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# load_and_print <file-content> <var...> : run host_env_load in a clean subshell, print the vars
load_and_print() {
    local content="$1"; shift
    local f; f="$(mktemp)"; printf '%b' "${content}" > "${f}"
    ( AUTO_AGENT_HOST_ENV="${f}" host_env_load; for v in "$@"; do printf '%s=%s\n' "${v}" "${!v-<unset>}"; done )
    rm -f "${f}"
}

test_environmentfile_shapes() {
    echo "TEST: the systemd EnvironmentFile shapes are read"
    local got
    got="$(load_and_print '# comment\nA=1\nB="two words"\nC='"'"'three'"'"'\n  D=indented\nexport E=exported\nF=win\r\n\nG=a=b\n' A B C D E F G)"
    local want='A=1
B=two words
C=three
D=indented
E=exported
F=win
G=a=b'
    if [ "${got}" = "${want}" ]; then pass "plain, quoted, indented, exported, CRLF, embedded '=' all read"
    else fail "plain, quoted, indented, exported, CRLF, embedded '=' all read" "${got}"; fi
}

test_bad_lines_named_and_skipped() {
    echo "TEST: unreadable lines are named on stderr and skipped, the rest still loads"
    local f; f="$(mktemp)"; printf 'GOOD=1\nnot a pair\n1BAD=x\nALSO_GOOD=2\n' > "${f}"
    local err rc
    err="$( (AUTO_AGENT_HOST_ENV="${f}" host_env_load; echo "rc=$? GOOD=${GOOD-} ALSO_GOOD=${ALSO_GOOD-}" >&2) 2>&1 >/dev/null )"
    if printf '%s\n' "${err}" | grep -q "rc=0 GOOD=1 ALSO_GOOD=2"; then pass "returns 0 and loads the good keys"
    else fail "returns 0 and loads the good keys" "${err}"; fi
    if printf '%s\n' "${err}" | grep -q "ignoring line without '=': not a pair" && printf '%s\n' "${err}" | grep -q "invalid key: 1BAD"; then
        pass "both bad lines are named"
    else fail "both bad lines are named" "${err}"; fi
    rm -f "${f}"
}

test_environment_wins_and_missing_file_is_noop() {
    echo "TEST: an exported key wins over the file; a missing file is a no-op"
    local f; f="$(mktemp)"; echo 'AUTO_AGENT_STATE_DIR=/from/file' > "${f}"
    local got; got="$( AUTO_AGENT_HOST_ENV="${f}" AUTO_AGENT_STATE_DIR=/from/env bash -c '. "'"${SCRIPT_DIR}"'/host-env.sh"; host_env_load; host_env_state_dir' )"
    if [ "${got}" = "/from/env" ]; then pass "environment wins"; else fail "environment wins" "${got}"; fi
    got="$( AUTO_AGENT_HOST_ENV="${f}" bash -c '. "'"${SCRIPT_DIR}"'/host-env.sh"; host_env_load; host_env_state_dir' )"
    if [ "${got}" = "/from/file" ]; then pass "file sets it when the environment does not"; else fail "file sets it when the environment does not" "${got}"; fi
    rm -f "${f}"
    local rc; ( AUTO_AGENT_HOST_ENV="${f}" host_env_load ); rc=$?
    if [ "${rc}" -eq 0 ]; then pass "missing file returns 0"; else fail "missing file returns 0" "rc=${rc}"; fi
}

test_state_dir_default() {
    echo "TEST: the State dir defaults outside any checkout"
    local got
    got="$( unset AUTO_AGENT_STATE_DIR XDG_STATE_HOME; HOME=/h host_env_state_dir )"
    if [ "${got}" = "/h/.local/state/auto-agent" ]; then pass "\$HOME/.local/state/auto-agent"; else fail "\$HOME/.local/state/auto-agent" "${got}"; fi
    got="$( unset AUTO_AGENT_STATE_DIR; XDG_STATE_HOME=/x host_env_state_dir )"
    if [ "${got}" = "/x/auto-agent" ]; then pass "XDG_STATE_HOME honoured"; else fail "XDG_STATE_HOME honoured" "${got}"; fi
    got="$( host_env_file )"
    if [ "${got}" = "${HOME}/.config/auto-agent/env" ]; then pass "Host env file defaults to ~/.config/auto-agent/env"; else fail "Host env file defaults to ~/.config/auto-agent/env" "${got}"; fi
}

test_environmentfile_shapes
test_bad_lines_named_and_skipped
test_environment_wins_and_missing_file_is_noop
test_state_dir_default

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
