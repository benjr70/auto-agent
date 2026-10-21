#!/usr/bin/env bash
# Tests for lib/pick.sh
#
# Run: bash lib/pick.test.sh
#
# Strategy: drive the CLI under both pick shapes with a GH_BIN stub that
# serves every harness label, canned `gh project` JSON, and records every
# call. The assertions are the output line, the exit code, and the order of
# the gh writes: the AFK label goes on last, after the board write landed,
# and comes off first.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/pick.sh"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env

cfg() {
    case "$1" in
        project) printf '{"config_dir":"/srv/t/.auto-agent","repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"project","project":{"number":7,"priority_field":"Priority","order":["P0","P1","P2"]},"labels":null}}' ;;
        labels)  printf '{"config_dir":"/srv/t/.auto-agent","repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"labels","project":null,"labels":{}}}' ;;
    esac
}

# make_env: the stub; item-edit exits with edit-rc (default 0).
make_env() {
    local dir; dir="$(mktemp -d)"
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "${dir}/gh-calls"
case "\$*" in
    "label list "*) cat "${dir}/labels.txt" ;;
    "label create "*) exit 0 ;;
    "issue edit "*) exit 0 ;;
    "issue view "*) echo '{"url":"https://github.com/acme/widgets/issues/41"}' ;;
    "project view "*) echo '{"id":"PVT_proj7","number":7}' ;;
    "project field-list "*) echo '{"fields":[{"id":"F_prio","name":"Priority","options":[{"id":"O_p0","name":"P0"},{"id":"O_p1","name":"P1"},{"id":"O_p2","name":"P2"}]}]}' ;;
    "project item-add "*) echo '{"id":"PVTI_item41"}' ;;
    "project item-edit "*) exit "\$(cat "${dir}/edit-rc" 2>/dev/null || echo 0)" ;;
    "project item-list "*) echo '{"items":[{"id":"PVTI_item41","content":{"number":41,"repository":"acme/widgets"}}]}' ;;
    "project item-delete "*) exit 0 ;;
    *) exit 1 ;;
esac
STUB
    chmod +x "${dir}/gh-stub"; : > "${dir}/gh-calls"
    bash "${SCRIPT_DIR}/labels-ensure.sh" --list | cut -f1 > "${dir}/labels.txt"
    echo "${dir}"
}

run_lib() { # run_lib <dir> <shape> <args...>
    local dir="$1" shape="$2"; shift 2
    HARNESS_CONFIG_JSON="$(cfg "${shape}")" GH_BIN="${dir}/gh-stub" bash "${LIB}" "$@"
}

writes() { grep -E '^(label create|issue edit|project item-(add|edit|delete))' "$1/gh-calls"; }

echo "TEST: add under a label-only pick: labels ensured, then AFK added"
dir="$(make_env)"
out="$(run_lib "${dir}" labels add 41)"; rc=$?
t="exit 0 and the Go command's line"
if [ "${rc}" -eq 0 ] && [ "${out}" = "pick: #41 is on the pick signal: AFK" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
t="the labels were read, then AFK put on the issue, and no project call"
if [ "$(head -1 "${dir}/gh-calls" | cut -d' ' -f1-2)" = "label list" ] && [ "$(writes "${dir}")" = "issue edit 41 --repo acme/widgets --add-label AFK" ] && ! grep -q '^project' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh-calls")"; fi
rm -rf "${dir}"

echo "TEST: add under a Project pick: the item added with the Priority, then AFK"
dir="$(make_env)"
out="$(run_lib "${dir}" project add 41 --priority P1)"; rc=$?
t="exit 0 and the line names the project and Priority"
if [ "${rc}" -eq 0 ] && [ "${out}" = "pick: #41 is on the pick signal: AFK, on project 7 with Priority P1" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
t="item-add, item-edit with the P1 option, then the AFK label, in that order"
if [ "$(writes "${dir}" | cut -d' ' -f1-2 | paste -sd,)" = "project item-add,project item-edit,issue edit" ] && grep -q -- '--single-select-option-id O_p1$' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh-calls")"; fi
rm -rf "${dir}"

echo "TEST: add under a Project pick defaults to the first of the order"
dir="$(make_env)"
out="$(run_lib "${dir}" project add 41)"; rc=$?
t="P0 set"
if [ "${rc}" -eq 0 ] && grep -q -- '--single-select-option-id O_p0$' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} $(cat "${dir}/gh-calls")"; fi
rm -rf "${dir}"

echo "TEST: an unknown priority, or a Project write that fails, exits non-zero with the reason"
dir="$(make_env)"
err="$(run_lib "${dir}" project add 41 --priority P9 2>&1 >/dev/null)"; rc=$?
t="unknown priority: exit 2, reason unknown-priority, no gh call"
if [ "${rc}" -eq 2 ] && printf '%s' "${err}" | grep -q 'unknown-priority' && [ ! -s "${dir}/gh-calls" ]; then pass "$t"; else fail "$t" "rc=${rc} ${err} $(cat "${dir}/gh-calls")"; fi
echo 1 > "${dir}/edit-rc"; : > "${dir}/gh-calls"
out="$(run_lib "${dir}" project add 41 2>"${dir}/err")"; rc=$?
t="failed item-edit: exit 1, reason priority-edit-failed, nothing on stdout, no AFK label"
if [ "${rc}" -eq 1 ] && grep -q 'priority-edit-failed' "${dir}/err" && [ -z "${out}" ] && ! grep -q '^issue edit' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} out=${out} $(cat "${dir}/err")"; fi
rm -rf "${dir}"

echo "TEST: remove undoes both"
dir="$(make_env)"
out="$(run_lib "${dir}" project remove 41)"; rc=$?
t="exit 0, AFK off first, then the item deleted"
if [ "${rc}" -eq 0 ] && [ "${out}" = "pick: #41 is off the pick signal: no AFK, taken off project 7" ] && [ "$(writes "${dir}" | paste -sd,)" = "issue edit 41 --repo acme/widgets --remove-label AFK,project item-delete 7 --owner acme --id PVTI_item41" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out} $(cat "${dir}/gh-calls")"; fi
: > "${dir}/gh-calls"
out="$(run_lib "${dir}" labels remove 41)"; rc=$?
t="label-only: AFK off, no project call"
if [ "${rc}" -eq 0 ] && [ "${out}" = "pick: #41 is off the pick signal: no AFK" ] && ! grep -q '^project' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
rm -rf "${dir}"

echo "TEST: labels ensures the harness labels and touches no ticket"
dir="$(make_env)"; printf 'AFK\n' > "${dir}/labels.txt"
out="$(run_lib "${dir}" labels labels)"; rc=$?
t="exit 0, 19 created, the summary last, no issue or project call"
if [ "${rc}" -eq 0 ] && [ "$(printf '%s\n' "${out}" | tail -1)" = "pick: labels created 19, present 1" ] && [ "$(printf '%s\n' "${out}" | grep -c '^pick: created the label ')" -eq 19 ] && ! grep -Eq '^(issue|project)' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"

echo "TEST: usage"
dir="$(make_env)"
for args in "add" "frobnicate 1" "add #41" "remove 41 --priority P0" "labels 41"; do
    # shellcheck disable=SC2086
    run_lib "${dir}" labels ${args} >/dev/null 2>&1; rc=$?
    t="'${args}' exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
done
HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= GH_BIN="${dir}/gh-stub" bash "${LIB}" add 1 >/dev/null 2>&1; rc=$?
t="no Harness config exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${dir}"

echo ""; echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
[ "${TESTS_FAILED}" -eq 0 ] || { for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done; exit 1; }
exit 0
