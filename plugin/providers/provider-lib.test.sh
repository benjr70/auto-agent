#!/usr/bin/env bash
# Tests for plugin/providers/provider-lib.sh
#
# Run: bash plugin/providers/provider-lib.test.sh
#
# The lib is shipped to provider authors, so the tests are the promises they
# are allowed to rely on: the contract's exit codes come out of provider_need,
# ports never collide between PRs, and provider_key refuses anything the
# harness could not export.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=provider-lib.sh
. "${SCRIPT_DIR}/provider-lib.sh"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

echo "TEST: provider_need"
provider_need bash >/dev/null 2>&1; rc=$?
t="a present command is exit 0"; if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
err="$(provider_need bash definitely-not-a-command 2>&1 >/dev/null)"; rc=$?
t="a missing command returns 3, the contract's prerequisite code, and names it"
if [ "${rc}" -eq 3 ] && printf '%s' "${err}" | grep -q 'prerequisite missing: definitely-not-a-command'; then pass "$t"; else fail "$t" "rc=${rc} ${err}"; fi

echo "TEST: provider_pr_arg"
t="--pr N and --pr=N both read"
if [ "$(provider_pr_arg --pr 42)" = 42 ] && [ "$(provider_pr_arg --pr=42)" = 42 ]; then pass "$t"; else fail "$t"; fi
t="the number is found after other arguments"
if [ "$(provider_pr_arg --verbose --pr 7)" = 7 ]; then pass "$t"; else fail "$t"; fi
provider_pr_arg >/dev/null 2>&1; rc=$?
t="no --pr returns 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
provider_pr_arg --pr >/dev/null 2>&1; rc=$?
t="a --pr with nothing after it returns 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi

echo "TEST: provider_port_block"
t="the default block is 10 ports from 20000, stepped by the PR number"
if [ "$(provider_port_block 0)" = 20000 ] && [ "$(provider_port_block 1)" = 20010 ] && [ "$(provider_port_block 42)" = 20420 ]; then pass "$t"; else fail "$t" "$(provider_port_block 42)"; fi
t="the stride and base are overridable"
if [ "$(provider_port_block 3 100 30000)" = 30300 ]; then pass "$t"; else fail "$t" "$(provider_port_block 3 100 30000)"; fi
t="PR numbers wrap at 1000, so the block stays in the ephemeral range"
if [ "$(provider_port_block 1002)" = "$(provider_port_block 2)" ] && [ "$(provider_port_block 9999)" -lt 65536 ]; then pass "$t"; else fail "$t" "$(provider_port_block 9999)"; fi
t="two different PRs never share a port block"
if [ "$(provider_port_block 11)" != "$(provider_port_block 12)" ]; then pass "$t"; else fail "$t"; fi
provider_port_block main >/dev/null 2>&1; rc=$?
t="a non-numeric PR returns 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi

echo "TEST: provider_compose_project"
t="the name is <prefix>-pr-<N>"
if [ "$(provider_compose_project widgets 9)" = "widgets-pr-9" ]; then pass "$t"; else fail "$t" "$(provider_compose_project widgets 9)"; fi
t="a prefix compose would refuse is folded to [a-z0-9_-]"
if [ "$(provider_compose_project 'My Widgets!' 9)" = "my-widgets-pr-9" ]; then pass "$t"; else fail "$t" "$(provider_compose_project 'My Widgets!' 9)"; fi
provider_compose_project widgets head >/dev/null 2>&1; rc=$?
t="a non-numeric PR returns 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi

echo "TEST: provider_key"
t="a good pair is one KEY=value line on stdout"
if [ "$(provider_key WEB_URL 'http://127.0.0.1:1/x')" = 'WEB_URL=http://127.0.0.1:1/x' ]; then pass "$t"; else fail "$t"; fi
t="an empty value is still a line (the key is what the harness exports)"
if [ "$(provider_key WEB_URL '')" = 'WEB_URL=' ]; then pass "$t"; else fail "$t"; fi
t="a value carrying an = keeps it: values run to end of line"
if [ "$(provider_key TOKEN 'a=b=c')" = 'TOKEN=a=b=c' ]; then pass "$t"; else fail "$t"; fi
for bad in web_url 9WEB WEB-URL 'WEB URL'; do
    out="$(provider_key "${bad}" x 2>/dev/null)"; rc=$?
    t="the key '${bad}' is refused (2) and nothing is printed"
    if [ "${rc}" -eq 2 ] && [ -z "${out}" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
done
out="$(provider_key WEB_URL "$(printf 'a\nB_KEY=x')" 2>/dev/null)"; rc=$?
t="a value with a newline is refused: it would read as a second key"
if [ "${rc}" -eq 2 ] && [ -z "${out}" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi

echo "TEST: provider_wait_healthy"
dir="$(mktemp -d)"
cat > "${dir}/curl" <<'CURL'
#!/usr/bin/env bash
D="$(dirname "$0")"; echo x >> "$D/tries"
[ -f "$D/healthy" ] && exit 0
[ "$(wc -l < "$D/tries")" -ge 3 ] && [ -f "$D/healthy-on-3" ] && exit 0
exit 7
CURL
chmod +x "${dir}/curl"; : > "${dir}/tries"; touch "${dir}/healthy"
PROVIDER_CURL_BIN="${dir}/curl" provider_wait_healthy http://x 5 0.01 >/dev/null 2>&1; rc=$?
t="a URL that answers at once returns 0 after one poll"
if [ "${rc}" -eq 0 ] && [ "$(wc -l < "${dir}/tries")" -eq 1 ]; then pass "$t"; else fail "$t" "rc=${rc} tries=$(wc -l < "${dir}/tries")"; fi
rm -f "${dir}/healthy"; : > "${dir}/tries"; touch "${dir}/healthy-on-3"
PROVIDER_CURL_BIN="${dir}/curl" provider_wait_healthy http://x 5 0.01 >/dev/null 2>&1; rc=$?
t="a URL that comes up late returns 0, still inside the budget"
if [ "${rc}" -eq 0 ] && [ "$(wc -l < "${dir}/tries")" -eq 3 ]; then pass "$t"; else fail "$t" "rc=${rc} tries=$(wc -l < "${dir}/tries")"; fi
rm -f "${dir}/healthy-on-3"; : > "${dir}/tries"
err="$(PROVIDER_CURL_BIN="${dir}/curl" provider_wait_healthy http://x 1 0.01 2>&1 >/dev/null)"; rc=$?
t="a URL that never answers returns 1 within the budget, naming the timeout"
if [ "${rc}" -eq 1 ] && printf '%s' "${err}" | grep -q 'health wait timed out after 1s'; then pass "$t"; else fail "$t" "rc=${rc} ${err}"; fi
rm -rf "${dir}"

echo ""; echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
[ "${TESTS_FAILED}" -eq 0 ] || { for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done; exit 1; }
exit 0
