#!/usr/bin/env bash
# Tests for infra/cutover/parity-gate.sh and soak-check.sh (issue #43)
#
# Run: bash infra/cutover/cutover.test.sh
#
# Strategy: both scripts are driven through their command lines. The parity
# gate runs against a copy of the fixture Target Project with a stub engine
# for the steps that would boot an environment or spend budget, and the real
# engine for the Harness config; the soak check runs against a scratch Host
# env and State dir with stub gh, systemctl and curl. Asserts the machine
# lines and exit codes only.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
GATE="${SCRIPT_DIR}/parity-gate.sh"
SOAK="${SCRIPT_DIR}/soak-check.sh"
FIXTURE="${ROOT_DIR}/plugin/fixtures/target-project"

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && printf '    %s\n' "$2"
}
check() { if eval "$2"; then pass "$1"; else fail "$1" "${3:-}"; fi; }

W="$(mktemp -d)" || exit 1
trap 'rm -rf "${W}"' EXIT
out_has() { grep -qF -- "$1" "${W}/out"; }

# ---------------------------------------------------------------- parity gate

# A Target Project checkout with a GitHub origin, and the stubs around it.
T="${W}/widget"
cp -r "${FIXTURE}" "${T}"
git -C "${T}" init -q -b main
git -C "${T}" remote add origin https://github.com/acme/widget.git

# The engine: real for the Harness config, canned for everything else.
cat > "${W}/cli" <<STUB
#!/usr/bin/env bash
case "\$1" in
    check-config|show-config) GH_BIN="${W}/gh" exec bash "${ROOT_DIR}/bin/auto-agent" "\$@" ;;
    pickup-triage) cat "${W}/new-triage.json" ;;
    provider-check) echo "provider-check: \$(cat "${W}/provider-verdict")"; exit "\$(cat "${W}/provider-rc")" ;;
    fire) echo "fire \$*" >> "${W}/cli.calls"; echo "AUTO_AGENT_STATE_DIR=\${AUTO_AGENT_STATE_DIR}" >> "${W}/cli.calls"
          cat "${W}/fire-out"; exit "\$(cat "${W}/fire-rc")" ;;
esac
STUB
cat > "${W}/gh" <<STUB
#!/usr/bin/env bash
case "\$1 \$2" in
    "repo view") echo main ;;
    "issue list") cat "${W}/locked" ;;
    "api user") echo "\${STUB_LOGIN:-widget-bot}" ;;
    "pr list") cat "${W}/merged-pr" ;;
esac
STUB
chmod +x "${W}/cli" "${W}/gh"
# A clean checkout standing in for the Harness install the gate inspects.
I="${W}/install"
git init -q -b main "${I}"
git -C "${I}" -c user.name=t -c user.email=t@t commit -q --allow-empty -m install

gate_defaults() {
    : > "${W}/locked"; : > "${W}/cli.calls"
    echo '{"verdict":"pick","pick":{"issue":7},"reconcile":null,"paused":null}' > "${W}/new-triage.json"
    echo "cat '${W}/new-triage.json'" > "${W}/old-triage.cmd"
    echo "PASS — verify/provider conforms (6 checks, pr 0)" > "${W}/provider-verdict"; echo 0 > "${W}/provider-rc"
    printf 'fire: dry-run\nafk-pickup: would-pick #7 (label-only pick)\n' > "${W}/fire-out"; echo 0 > "${W}/fire-rc"
}
gate() {
    env PARITY_CLI="${W}/cli" GH_BIN="${W}/gh" PARITY_INSTALL_DIR="${I}" TMPDIR="${W}" PARITY_SUITES_CMD="${SUITES-echo 'Suites: 3 | Failed: 0'}" \
        PARITY_STATE_DIR="${W}/parity-state" bash "${GATE}" --old-triage "$(cat "${W}/old-triage.cmd")" "$@" "${T}" > "${W}/out" 2> "${W}/err"
    RC=$?
}

test_gate_passes() {
    echo "TEST: the parity gate passes when the install, the queue, the provider and a dry-run Fire agree"
    gate_defaults
    gate
    check "exit 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(cat "${W}/out" "${W}/err")"
    local s
    for s in install config lock suites queue provider fire; do
        check "step ${s} is ok" 'grep -Eq "^parity: ${s}: ok — " "${W}/out"' "$(grep "^parity: ${s}" "${W}/out")"
    done
    check "the queue step names what both sides read" 'out_has "parity: queue: ok — both read {\"verdict\":\"pick\",\"issue\":7,\"pr\":null}"' "$(grep 'parity: queue' "${W}/out")"
    check "the last line says the old daemon can be stopped" '[ "$(tail -1 "${W}/out")" = "parity: PASS — the old daemon can be stopped" ]' "$(tail -1 "${W}/out")"
    check "the Fire was a dry run into the scratch State dir, never the Host's" \
        'grep -q "^fire fire --dry-run ${T}$" "${W}/cli.calls" && grep -qx "AUTO_AGENT_STATE_DIR=${W}/parity-state" "${W}/cli.calls"' "$(cat "${W}/cli.calls")"
}

test_gate_failures() {
    echo "TEST: each step fails the gate by name, and a failed gate exits 1"
    gate_defaults; echo '{"verdict":"idle","pick":null,"reconcile":null,"paused":null}' > "${W}/other.json"
    echo "cat '${W}/other.json'" > "${W}/old-triage.cmd"
    gate
    check "a queue the two sides read differently fails" \
        '[ "${RC}" -eq 1 ] && out_has "parity: queue: FAIL — the old daemon reads {\"verdict\":\"idle\"" && [ "$(tail -1 "${W}/out")" = "parity: FAIL — queue" ]' "$(cat "${W}/out")"

    gate_defaults; echo "12" > "${W}/locked"
    gate
    check "a held single-flight lock fails: the old daemon is mid-Fire" '[ "${RC}" -eq 1 ] && out_has "parity: lock: FAIL — #12 holds AFK:in-progress"' "$(grep 'parity: lock' "${W}/out")"

    gate_defaults; echo "FAIL — up exited 4 (boot failed) on both attempts" > "${W}/provider-verdict"; echo 4 > "${W}/provider-rc"
    gate
    check "a provider that does not conform fails, with its verdict" '[ "${RC}" -eq 1 ] && out_has "parity: provider: FAIL — exit 4: provider-check: FAIL — up exited 4"' "$(grep 'parity: provider' "${W}/out")"

    gate_defaults; echo 1 > "${W}/fire-rc"
    gate
    check "a dry-run Fire that exits non-zero fails" '[ "${RC}" -eq 1 ] && out_has "parity: fire: FAIL — the dry-run Fire exited 1"' "$(grep 'parity: fire' "${W}/out")"
    gate_defaults; printf 'afk-pickup: would-pick #9 (label-only pick)\n' > "${W}/fire-out"
    gate
    check "a Fire that would pick a different issue than the triage fails" '[ "${RC}" -eq 1 ] && out_has "parity: fire: FAIL — the triage names #7, the Fire says: afk-pickup: would-pick #9"' "$(grep 'parity: fire' "${W}/out")"
    gate_defaults; printf 'no verdict here\n' > "${W}/fire-out"
    gate
    check "a Fire that prints no pick verdict fails" '[ "${RC}" -eq 1 ] && out_has "printed no pick verdict"' "$(grep 'parity: fire' "${W}/out")"

    gate_defaults
    echo '{"verdict":"idle","pick":null,"reconcile":null,"paused":null}' > "${W}/new-triage.json"
    gate
    check "an idle triage and a Fire that would pick something disagree" '[ "${RC}" -eq 1 ] && out_has "parity: fire: FAIL — the triage reads an idle queue"' "$(grep 'parity: fire' "${W}/out")"

    # The lock is free at the lock step and taken by the time the suites end.
    gate_defaults
    SUITES="echo 'Suites: 3 | Failed: 0'; echo 12 > '${W}/locked'" gate
    check "a Fire the old daemon starts while the suites run fails the steps that would race it" \
        '[ "${RC}" -eq 1 ] && out_has "parity: lock: ok" && out_has "parity: provider: FAIL — #12 took AFK:in-progress while the gate was running" && ! grep -q "^fire " "${W}/cli.calls"' "$(cat "${W}/out")"

    gate_defaults; touch "${I}/stray"
    gate
    check "an install with uncommitted changes fails: a Harness install is a clean checkout" '[ "${RC}" -eq 1 ] && out_has "parity: install: FAIL" && out_has "uncommitted changes"' "$(grep 'parity: install' "${W}/out")"
    rm -f "${I}/stray"

    gate_defaults
    SUITES='echo "host-env=${AUTO_AGENT_HOST_ENV-unset}" > "'"${W}"'/suites.env"; echo "Suites: 1 | Failed: 0"' gate
    check "the suites do not inherit the gate's own Host env override" '[ "${RC}" -eq 0 ] && grep -qx "host-env=unset" "${W}/suites.env"' "$(cat "${W}/suites.env" 2>/dev/null)"

    gate_defaults
    SUITES="echo boom; exit 1" gate
    check "red suites fail" '[ "${RC}" -eq 1 ] && out_has "parity: suites: FAIL"' "$(grep 'parity: suites' "${W}/out")"

    gate_defaults; mv "${T}/.auto-agent/harness.json" "${W}/harness.json.bak"
    gate
    check "no Harness config fails config, and the steps that need it" '[ "${RC}" -eq 1 ] && out_has "parity: config: FAIL" && out_has "parity: lock: FAIL"' "$(cat "${W}/out")"
    mv "${W}/harness.json.bak" "${T}/.auto-agent/harness.json"
}

test_gate_skips() {
    echo "TEST: the expensive steps can be skipped, and say so"
    gate_defaults
    gate --skip-suites --skip-provider --skip-fire
    check "exit 0 with three skipped lines" \
        '[ "${RC}" -eq 0 ] && [ "$(grep -c ": skipped — --skip-" "${W}/out")" -eq 3 ] && [ ! -s "${W}/cli.calls" ]' "$(cat "${W}/out")"
    gate_defaults
    echo '{"verdict":"idle","pick":null,"reconcile":null,"paused":null}' > "${W}/new-triage.json"
    printf 'afk-pickup: no eligible issue\n' > "${W}/fire-out"
    gate --skip-suites
    check "an empty queue on both sides passes on the Fire's no-eligible-issue line" '[ "${RC}" -eq 0 ] && out_has "parity: fire: ok — afk-pickup: no eligible issue"' "$(cat "${W}/out")"
    bash "${GATE}" > /dev/null 2>&1
    check "no target is a usage error: exit 2" '[ $? -eq 2 ]'
}

# ------------------------------------------------------------------ soak check

H="${W}/host"
soak_host() {
    rm -rf "${H}"; mkdir -p "${H}/state/fires"
    printf 'GH_TOKEN=ghp_stub\nDAEMON_GH_LOGIN=widget-bot\nAUTO_AGENT_TARGET_DIR=%s\nAUTO_AGENT_STATE_DIR=%s/state\n' "${T}" "${H}" > "${H}/env"
    cat > "${H}/systemctl" <<STUB
#!/usr/bin/env bash
case "\$1 \$2" in
    "is-active auto-agent-"*) echo active ;;
    "is-active "*) cat "${H}/old-active" 2>/dev/null || echo inactive ;;
    "is-enabled "*) cat "${H}/old-enabled" 2>/dev/null || echo disabled ;;
esac
STUB
    printf '#!/usr/bin/env bash\ncat "%s/status.json" 2>/dev/null || exit 7\n' "${H}" > "${H}/curl"
    chmod +x "${H}/systemctl" "${H}/curl"
    : > "${W}/merged-pr"
}
# record <id> <work-kind> <issue> [<settled>] [<exit>] [<status>] [<dryRun>]
record() {
    jq -n --arg id "$1" --arg kind "$2" --argjson issue "$3" --arg settled "${4:-}" --argjson exit "${5:-0}" --arg status "${6:-OK}" --argjson dry "${7:-false}" \
        '{fireId: $id, kind: "pickup", exit: $exit, dryRun: $dry, outcome: {status: $status},
          work: {kind: $kind, issue: $issue, pr: null, settled: (if $settled == "" then null else $settled end)}}' > "${H}/state/fires/$1.json"
}
dashboard() { jq -n --arg s "${1:-${H}/state}" --argjson n "${2:-2}" '{host: {stateDir: $s}, fires: {items: [range($n)]}}' > "${H}/status.json"; }
soak() {
    env AUTO_AGENT_HOST_ENV="${H}/env" GH_BIN="${W}/gh" SYSTEMCTL_BIN="${H}/systemctl" CURL_BIN="${H}/curl" \
        STUB_LOGIN="${LOGIN-widget-bot}" bash "${SOAK}" "$@" > "${W}/out" 2> "${W}/err"
    RC=$?
}

test_soak_passes() {
    echo "TEST: a soaked Host passes: machine user, units, one merged AFK ticket, one resolve, the Dashboard"
    soak_host
    record f1 pick 30; record f2 resolve 31 done
    echo "44 widget-bot" > "${W}/merged-pr"; dashboard
    soak
    check "exit 0" '[ "${RC}" -eq 0 ]' "rc=${RC} $(cat "${W}/out" "${W}/err")"
    check "every item is ok" '[ "$(grep -c "^soak: [a-z-]*: ok — " "${W}/out")" -eq 5 ]' "$(cat "${W}/out")"
    check "the AFK ticket names the merged Agent PR and who opened it" 'out_has "soak: afk-ticket: ok — #30 by merged PR #44 (opened by widget-bot)"' "$(grep afk-ticket "${W}/out")"
    check "the last line ends the rollback window" 'tail -1 "${W}/out" | grep -q "^soak: PASS — the deletion PR may merge"' "$(tail -1 "${W}/out")"
}

test_soak_waits() {
    echo "TEST: what has not happened yet is waiting, not a failure"
    soak_host; dashboard "${H}/state" 0
    soak
    check "a fresh install: exit 3, NOT YET" '[ "${RC}" -eq 3 ] && tail -1 "${W}/out" | grep -q "^soak: NOT YET — afk-ticket resolve dashboard$"' "$(cat "${W}/out")"
    record f1 pick 30; dashboard
    soak
    check "a picked ticket whose Agent PR has not merged is waiting" '[ "${RC}" -eq 3 ] && out_has "soak: afk-ticket: waiting — #30 was picked; its Agent PR has not merged"' "$(grep afk-ticket "${W}/out")"
    echo "44 acme" > "${W}/merged-pr"
    soak
    check "a merged PR someone else opened does not count as the Daemon's" '[ "${RC}" -eq 3 ] && out_has "was opened by acme, not by the machine user"' "$(grep afk-ticket "${W}/out")"
    : > "${W}/merged-pr"
    record f2 pick 40 "" 1 FAILED; record f3 resolve 41 done 0 OK true; record f4 resolve 42 "" 0 EXHAUSTED
    soak
    check "failed, dry-run and exhausted Fires do not count" '[ "${RC}" -eq 3 ] && out_has "soak: resolve: waiting" && ! out_has "#40"' "$(cat "${W}/out")"
}

test_soak_fails() {
    echo "TEST: a Host that is not cut over fails"
    soak_host; record f1 pick 30; record f2 resolve 31 done; echo "44 widget-bot" > "${W}/merged-pr"; dashboard
    LOGIN=acme soak
    check "a token that is not the machine user's fails identity" '[ "${RC}" -eq 1 ] && out_has "logs in as acme, not widget-bot"' "$(grep identity "${W}/out")"
    sed -i 's/^DAEMON_GH_LOGIN=.*/DAEMON_GH_LOGIN=acme/' "${H}/env"
    LOGIN=acme soak
    check "a Daemon acting as the repository's owner fails identity" '[ "${RC}" -eq 1 ] && out_has "acme owns acme/widget"' "$(grep identity "${W}/out")"
    soak_host; record f1 pick 30; record f2 resolve 31 done; echo "44 widget-bot" > "${W}/merged-pr"; dashboard
    echo active > "${H}/old-active"
    soak
    check "an old unit still active fails units" '[ "${RC}" -eq 1 ] && out_has "agent-daemon.service is still active"' "$(grep units "${W}/out")"
    rm -f "${H}/old-active"; echo enabled > "${H}/old-enabled"
    soak --old-units "legacy.service"
    check "--old-units names the old units; one still enabled fails" '[ "${RC}" -eq 1 ] && out_has "legacy.service is still enabled"' "$(grep units "${W}/out")"
    rm -f "${H}/old-enabled"; dashboard /somewhere/else
    soak
    check "a Dashboard reading another State dir fails" '[ "${RC}" -eq 1 ] && out_has "soak: dashboard: FAIL — the Dashboard reads /somewhere/else"' "$(grep dashboard "${W}/out")"
    rm -f "${H}/status.json"
    soak
    check "a Dashboard that does not answer fails" '[ "${RC}" -eq 1 ] && out_has "/api/status did not answer"' "$(grep dashboard "${W}/out")"
    env AUTO_AGENT_HOST_ENV="${W}/no-such-env" bash "${SOAK}" > /dev/null 2>&1
    check "no Host env is exit 2: Setup has not run" '[ $? -eq 2 ]'
}

test_gate_passes
test_gate_failures
test_gate_skips
test_soak_passes
test_soak_waits
test_soak_fails

echo
echo "cutover.test.sh: ${TESTS_RUN} run, ${TESTS_FAILED} failed"
if [ "${TESTS_FAILED}" -gt 0 ]; then
    printf '  - %s\n' "${FAILED_NAMES[@]}"
    exit 1
fi
