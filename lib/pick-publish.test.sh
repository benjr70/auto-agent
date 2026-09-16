#!/usr/bin/env bash
# Tests for lib/pick-publish.sh
#
# Run: bash lib/pick-publish.test.sh
#
# Strategy: the lib is the one place a skill puts an AFK issue on (or takes it
# off) the Target Project's pick signal, so the tests drive its CLI under both
# pick shapes from the resolved Harness config (HARNESS_CONFIG_JSON) with a
# GH_BIN stub that serves canned `gh project` JSON and records every call. The
# assertions are the stdout JSON verdict, the exit code, and which gh calls
# were (or were not) made: a label-only pick must never touch a project, and a
# project pick must set the Priority and fail loudly when it cannot (issue #29
# AC 2, behaviour 2).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/pick-publish.sh"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env

# cfg <shape> : the resolved config, project pick #7 with field "Priority" and
# order P0/P1/P2, or the label-only pick.
cfg() {
    case "$1" in
        project) printf '{"config_dir":"/srv/t/.auto-agent","repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"project","project":{"number":7,"priority_field":"Priority","order":["P0","P1","P2"]},"labels":null}}' ;;
        labels)  printf '{"config_dir":"/srv/t/.auto-agent","repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"labels","project":null,"labels":{}}}' ;;
    esac
}

# make_env: a gh stub answering the project commands from canned files and
# logging every argv line to gh-calls. `item-edit` exits with the code in
# edit-rc (default 0).
make_env() {
    local dir; dir="$(mktemp -d)"
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "${dir}/gh-calls"
case "\$*" in
    "issue view "*) echo '{"url":"https://github.com/acme/widgets/issues/41"}' ;;
    "project view "*) echo '{"id":"PVT_proj7","number":7}' ;;
    "project field-list "*) cat "${dir}/fields.json" ;;
    "project item-add "*) echo '{"id":"PVTI_item41","title":"x"}' ;;
    "project item-edit "*) exit "\$(cat "${dir}/edit-rc" 2>/dev/null || echo 0)" ;;
    "project item-list "*) cat "${dir}/items.json" ;;
    "project item-delete "*) exit 0 ;;
    *) exit 1 ;;
esac
STUB
    chmod +x "${dir}/gh-stub"
    : > "${dir}/gh-calls"
    printf '{"fields":[{"id":"F_status","name":"Status","type":"ProjectV2SingleSelectField","options":[{"id":"O_todo","name":"Todo"}]},{"id":"F_prio","name":"Priority","type":"ProjectV2SingleSelectField","options":[{"id":"O_p0","name":"P0"},{"id":"O_p1","name":"P1"},{"id":"O_p2","name":"P2"}]}]}\n' > "${dir}/fields.json"
    printf '{"items":[{"id":"PVTI_other","content":{"number":9,"repository":"acme/widgets"}},{"id":"PVTI_item41","content":{"number":41,"repository":"acme/widgets"}}]}\n' > "${dir}/items.json"
    echo "${dir}"
}

run_lib() { # run_lib <dir> <shape> <args...>
    local dir="$1" shape="$2"; shift 2
    HARNESS_CONFIG_JSON="$(cfg "${shape}")" GH_BIN="${dir}/gh-stub" bash "${LIB}" "$@"
}

test_project_publish_sets_priority() {
    echo "TEST: project pick: publish adds the item and sets the asked Priority (AC 2)"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    local out rc; out="$(run_lib "${dir}" project publish --issue 41 --priority P1)"; rc=$?
    local t="exit 0 with a projected verdict"
    if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -c '{shape, projected, issue, priority, itemId}')" = '{"shape":"project","projected":true,"issue":41,"priority":"P1","itemId":"PVTI_item41"}' ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
    t="item-add ran against the configured project, owner and issue url"
    if grep -q '^project item-add 7 --owner acme --url https://github.com/acme/widgets/issues/41 --format json$' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh-calls")"; fi
    t="item-edit ran with the project, item, Priority field and P1 option ids"
    if grep -q '^project item-edit --project-id PVT_proj7 --id PVTI_item41 --field-id F_prio --single-select-option-id O_p1$' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh-calls")"; fi
}

test_project_publish_defaults_priority_to_last_of_order() {
    echo "TEST: project pick: no --priority means the last value of the configured order"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    local out rc; out="$(run_lib "${dir}" project publish --issue 41)"; rc=$?
    local t="verdict priority is P2 and the P2 option was set"
    if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -r .priority)" = "P2" ] && grep -q -- '--single-select-option-id O_p2$' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} out=${out} $(cat "${dir}/gh-calls")"; fi
}

test_project_publish_fails_when_edit_fails() {
    echo "TEST: project pick: a failed item-edit is a failure, never a silent downgrade"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    echo 1 > "${dir}/edit-rc"
    local out rc; out="$(run_lib "${dir}" project publish --issue 41 --priority P0 2>/dev/null)"; rc=$?
    local t="exit 1 and the verdict names the priority-edit failure"
    if [ "${rc}" -eq 1 ] && [ "$(printf '%s' "${out}" | jq -r '.projected, .reason' | paste -sd' ')" = "true priority-edit-failed" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
}

test_project_publish_rejects_unknown_priority() {
    echo "TEST: project pick: a Priority outside the configured order is refused before any write"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    local out rc; out="$(run_lib "${dir}" project publish --issue 41 --priority P9 2>/dev/null)"; rc=$?
    local t="exit 2 with reason unknown-priority and no item-add"
    if [ "${rc}" -eq 2 ] && [ "$(printf '%s' "${out}" | jq -r .reason)" = "unknown-priority" ] && ! grep -q 'item-add' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} out=${out} $(cat "${dir}/gh-calls")"; fi
}

test_project_publish_fails_when_option_missing() {
    echo "TEST: project pick: a Priority the board has no option for fails with option-missing"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    printf '{"fields":[{"id":"F_prio","name":"Priority","type":"ProjectV2SingleSelectField","options":[{"id":"O_p2","name":"P2"}]}]}\n' > "${dir}/fields.json"
    local out rc; out="$(run_lib "${dir}" project publish --issue 41 --priority P1 2>/dev/null)"; rc=$?
    local t="exit 1 with reason option-missing"
    if [ "${rc}" -eq 1 ] && [ "$(printf '%s' "${out}" | jq -r .reason)" = "option-missing" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
}

test_labels_publish_is_a_noop() {
    echo "TEST: label-only pick: publish projects nothing and touches no gh project command (AC 2)"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    local out rc; out="$(run_lib "${dir}" labels publish --issue 41 --priority P1)"; rc=$?
    local t="exit 0, shape labels, projected false, priority null"
    if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -c '{shape, projected, issue, priority}')" = '{"shape":"labels","projected":false,"issue":41,"priority":null}' ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
    t="no gh call at all"
    if [ ! -s "${dir}/gh-calls" ]; then pass "$t"; else fail "$t" "$(cat "${dir}/gh-calls")"; fi
}

test_project_unpublish_deletes_the_item() {
    echo "TEST: project pick: unpublish finds the issue's item and deletes it"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    local out rc; out="$(run_lib "${dir}" project unpublish --issue 41)"; rc=$?
    local t="exit 0 with removed true and the item id"
    if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -c '{shape, removed, itemId}')" = '{"shape":"project","removed":true,"itemId":"PVTI_item41"}' ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
    t="item-delete ran on the right item and never on the other"
    if grep -q '^project item-delete 7 --owner acme --id PVTI_item41$' "${dir}/gh-calls" && ! grep -q 'PVTI_other' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh-calls")"; fi
}

test_project_unpublish_absent_item_is_fine() {
    echo "TEST: project pick: unpublish of an issue not on the board is a no-op success"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    local out rc; out="$(run_lib "${dir}" project unpublish --issue 99)"; rc=$?
    local t="exit 0, removed false, no item-delete"
    if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -r .removed)" = "false" ] && ! grep -q 'item-delete' "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
}

test_labels_unpublish_is_a_noop() {
    echo "TEST: label-only pick: unpublish is a no-op"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    local out rc; out="$(run_lib "${dir}" labels unpublish --issue 41)"; rc=$?
    local t="exit 0, removed false, no gh call"
    if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -r .removed)" = "false" ] && [ ! -s "${dir}/gh-calls" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
}

test_usage_errors() {
    echo "TEST: usage: a missing --issue or an unknown subcommand exits 2; no config exits 2"
    local dir; dir="$(make_env)"; trap "rm -rf '${dir}'" RETURN
    local rc
    run_lib "${dir}" project publish >/dev/null 2>&1; rc=$?
    local t="publish without --issue exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
    run_lib "${dir}" project frobnicate --issue 1 >/dev/null 2>&1; rc=$?
    t="unknown subcommand exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
    HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= GH_BIN="${dir}/gh-stub" bash "${LIB}" publish --issue 1 >/dev/null 2>&1; rc=$?
    t="no Harness config exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
}

test_project_publish_sets_priority
test_project_publish_defaults_priority_to_last_of_order
test_project_publish_fails_when_edit_fails
test_project_publish_rejects_unknown_priority
test_project_publish_fails_when_option_missing
test_labels_publish_is_a_noop
test_project_unpublish_deletes_the_item
test_project_unpublish_absent_item_is_fine
test_labels_unpublish_is_a_noop
test_usage_errors

echo ""
echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
    exit 1
fi
exit 0
