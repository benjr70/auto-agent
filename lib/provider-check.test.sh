#!/usr/bin/env bash
# Tests for lib/provider-check.sh
#
# Run: bash lib/provider-check.test.sh
#
# Strategy, two layers:
#   1. the two reference providers really run (the single-process fixture, and
#      the compose reference against a stub compose CLI that starts the same
#      stdlib server): AC 1's "passes on both reference providers";
#   2. a recording stub provider, driven into each contract violation, proves
#      the named reason, the exit code and — what no real provider can show —
#      the CALL ORDER: down before the first up, down between the one retry,
#      down on every exit path, and smoke never before a healthy up.
#
# Only the config the check reads is injected (HARNESS_CONFIG_JSON), so no gh
# and no git remote are needed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="${SCRIPT_DIR}/provider-check.sh"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

export AUTO_AGENT_HOST_ENV=/nonexistent/host-env

# cfg <target-dir> <hermetic-json> <surfaces-json> : the resolved config the
# check reads, with everything else it never touches left minimal.
cfg() {
    jq -cn --arg dir "$1/.auto-agent" --argjson h "$2" --argjson s "$3" \
        '{config_dir: $dir,
          repo: {owner: "acme", name: "widgets", slug: "acme/widgets", default_branch: "trunk"},
          pick: {shape: "labels", project: null, labels: {}},
          verification: {hermetic: $h, deployed: null},
          surfaces: $s}'
}

# stub_target : a Target Project dir whose `provider` records every call and
# behaves as the STUB_* files in it say.
stub_target() {
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/.auto-agent"
    cat > "${dir}/provider" <<'STUB'
#!/usr/bin/env bash
D="$(cd "$(dirname "$0")" && pwd)"
echo "$*" >> "$D/calls"
sub="$1"
case "$sub" in
  down)
    [ -f "$D/down-fails" ] && { echo "down: broken" >&2; exit 1; }
    if [ -f "$D/down-post-fails" ] && [ -s "$D/up-ok" ]; then echo "down: broken after up" >&2; exit 1; fi
    exit 0;;
  up)
    echo attempt >> "$D/attempts"
    if [ -f "$D/up-rc" ]; then
        rc="$(cat "$D/up-rc")"
        # up-recovers: boot fails once, then the retry succeeds.
        if ! { [ "$rc" = 4 ] && [ -f "$D/up-recovers" ] && [ "$(wc -l < "$D/attempts")" -ge 2 ]; }; then
            echo "up: exiting $rc" >&2; exit "$rc"
        fi
    fi
    echo ok > "$D/up-ok"
    cat "$D/block"
    exit 0;;
  smoke)
    env | grep -E '^(WEB_URL|API_URL)=' | sort > "$D/smoke-env"
    [ -f "$D/smoke-out" ] && cat "$D/smoke-out"
    exit "$(cat "$D/smoke-rc" 2>/dev/null || echo 0)";;
esac
exit 2
STUB
    chmod +x "${dir}/provider"
    : > "${dir}/calls"; : > "${dir}/attempts"
    printf 'WEB_URL=http://127.0.0.1:9/\nAPI_URL=http://127.0.0.1:9/api\n' > "${dir}/block"
    printf 'smoke: PASS (2/2)\n' > "${dir}/smoke-out"
    echo "${dir}"
}

SURFACES='{"web":{"kind":"browser","url_key":"WEB_URL","paths":["app/**"]},"api":{"kind":"api","url_key":"API_URL","paths":["app/**"]}}'
HERMETIC_SMOKE='{"command":"provider","smoke":true}'

# run_stub <dir> [args...] : the check against a stub target, stdout captured
run_stub() {
    local dir="$1"; shift
    HARNESS_CONFIG_JSON="$(cfg "${dir}" "${HERMETIC_SMOKE}" "${SURFACES}")" \
        bash "${LIB}" "$@" 2>/dev/null
}

echo "TEST: the single-process reference provider (the fixture) conforms"
run="$(mktemp -d)"
out="$(FIXTURE_RUN_DIR="${run}" HARNESS_CONFIG_JSON='' AUTO_AGENT_TARGET_DIR='' \
    bash "${LIB}" --pr 991 "${ROOT}/plugin/fixtures/target-project" 2>/dev/null)"; rc=$?
t="exit 0 and one PASS verdict naming the command and the check count"
if [ "${rc}" -eq 0 ] && [ "$(printf '%s\n' "${out}" | wc -l)" -eq 1 ] \
   && printf '%s\n' "${out}" | grep -q '^provider-check: PASS — verify/provider conforms (6 checks, pr 991)$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="the fixture environment is gone afterwards (the final down ran)"
if [ -z "$(ls "${run}" 2>/dev/null | grep -F 'pr-991.pid')" ]; then pass "$t"; else fail "$t" "$(ls "${run}")"; fi
rm -rf "${run}"

echo "TEST: the compose reference provider conforms (stub compose CLI)"
stubdir="$(mktemp -d)"
cat > "${stubdir}/compose" <<'CSTUB'
#!/usr/bin/env bash
# Enough of `docker compose` for the reference provider: -p <project> -f <file>
# up -d --wait starts the stdlib server on $WEB_PORT, down stops it.
proj=""; while [ $# -gt 0 ]; do case "$1" in -p) proj="$2"; shift 2;; -f) shift 2;; *) break;; esac; done
D="${COMPOSE_STUB_DIR:?}"; pidf="$D/$proj.pid"
case "${1:-}" in
  up) python3 -m http.server "${WEB_PORT:?}" --bind 127.0.0.1 >"$D/$proj.log" 2>&1 & echo $! > "$pidf"; exit 0;;
  down) [ -f "$pidf" ] && { kill "$(cat "$pidf")" 2>/dev/null; rm -f "$pidf"; }; exit 0;;
  logs) exit 0;;
esac
exit 1
CSTUB
chmod +x "${stubdir}/compose"
out="$(COMPOSE_STUB_DIR="${stubdir}" PROVIDER_COMPOSE_BIN="${stubdir}/compose" \
    PROVIDER_HEALTH_SECS=15 HARNESS_CONFIG_JSON='' AUTO_AGENT_TARGET_DIR='' \
    bash "${LIB}" --pr 992 "${ROOT}/plugin/providers/compose" 2>/dev/null)"; rc=$?
t="exit 0 and a PASS verdict"
if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^provider-check: PASS — provider conforms (6 checks, pr 992)$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="the compose project was torn down"
if [ -z "$(ls "${stubdir}"/*.pid 2>/dev/null)" ]; then pass "$t"; else fail "$t" "$(ls "${stubdir}")"; fi
rm -rf "${stubdir}"

echo "TEST: the call order is down, up, smoke, down"
dir="$(stub_target)"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="exit 0 with a PASS verdict"
if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^provider-check: PASS — provider conforms (6 checks, pr 5)$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="down ran before the first up, and again after the run"
want=$'down --pr 5\nup --pr 5\nsmoke\ndown --pr 5'
if [ "$(cat "${dir}/calls")" = "${want}" ]; then pass "$t"; else fail "$t" "$(tr '\n' '|' < "${dir}/calls")"; fi
t="the up block was exported into smoke's environment"
if [ "$(cat "${dir}/smoke-env")" = "$(printf 'API_URL=http://127.0.0.1:9/api\nWEB_URL=http://127.0.0.1:9/')" ]; then pass "$t"; else fail "$t" "$(cat "${dir}/smoke-env")"; fi
rm -rf "${dir}"

echo "TEST: a boot failure is retried once, with a down between the attempts"
dir="$(stub_target)"; echo 4 > "${dir}/up-rc"; touch "${dir}/up-recovers"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="the second attempt succeeds: exit 0"
if [ "${rc}" -eq 0 ]; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="down, up, down, up, smoke, down"
want=$'down --pr 5\nup --pr 5\ndown --pr 5\nup --pr 5\nsmoke\ndown --pr 5'
if [ "$(cat "${dir}/calls")" = "${want}" ]; then pass "$t"; else fail "$t" "$(tr '\n' '|' < "${dir}/calls")"; fi
rm -rf "${dir}"

echo "TEST: up exiting 4 twice is exit 4, and up exiting 3 is not retried"
dir="$(stub_target)"; echo 4 > "${dir}/up-rc"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="exit 4 and a verdict naming boot failure on both attempts"
if [ "${rc}" -eq 4 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — up exited 4 (boot failed) on both attempts: up: exiting 4$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="exactly two up attempts, and the environment is torn down afterwards"
if [ "$(grep -c '^up ' "${dir}/calls")" -eq 2 ] && [ "$(tail -1 "${dir}/calls")" = "down --pr 5" ] \
   && [ "$(grep -c '^down ' "${dir}/calls")" -eq 3 ]; then pass "$t"; else fail "$t" "$(tr '\n' '|' < "${dir}/calls")"; fi
rm -rf "${dir}"
dir="$(stub_target)"; echo 3 > "${dir}/up-rc"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="a missing prerequisite is exit 4, named, tried once, and torn down after"
if [ "${rc}" -eq 4 ] && [ "$(grep -c '^up ' "${dir}/calls")" -eq 1 ] \
   && [ "$(tail -1 "${dir}/calls")" = "down --pr 5" ] \
   && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — up exited 3 (prerequisite missing): up: exiting 3$'; then pass "$t"; else fail "$t" "rc=${rc} $(tr '\n' '|' < "${dir}/calls") ${out}"; fi
rm -rf "${dir}"
dir="$(stub_target)"; echo 7 > "${dir}/up-rc"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="an exit code outside the contract is a contract violation (exit 1), torn down after"
if [ "${rc}" -eq 1 ] && [ "$(tail -1 "${dir}/calls")" = "down --pr 5" ] \
   && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — up exited 7, want 0 healthy, 3 prerequisite missing or 4 boot failed'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"

echo "TEST: a Surface whose url_key the block omits is named"
dir="$(stub_target)"; printf 'WEB_URL=http://127.0.0.1:9/\n' > "${dir}/block"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="exit 1 naming the surface and the key"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — surface api declares url_key API_URL, which the up block does not carry$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="the environment was still torn down, and smoke never ran"
if [ "$(tail -1 "${dir}/calls")" = "down --pr 5" ] && ! grep -q '^smoke' "${dir}/calls"; then pass "$t"; else fail "$t" "$(tr '\n' '|' < "${dir}/calls")"; fi
rm -rf "${dir}"

echo "TEST: the block grammar"
dir="$(stub_target)"; printf 'WEB_URL=http://127.0.0.1:9/\nstarting api...\nAPI_URL=x\n' > "${dir}/block"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="a line that is not KEY=value is named, exit 1"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — up printed a line that is not KEY=value: starting api...$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"
dir="$(stub_target)"; printf 'WEB_URL=http://127.0.0.1:9/\napi_url=x\n' > "${dir}/block"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="a key that is not an uppercase shell identifier is named, exit 1"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — up printed a key that is not an uppercase shell identifier: api_url$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"
dir="$(stub_target)"; printf 'WEB-URL=x\n' > "${dir}/block"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="a key with a dash in it is named, exit 1"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q 'not an uppercase shell identifier: WEB-URL$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"
dir="$(stub_target)"; : > "${dir}/block"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="a healthy up that printed nothing is named, exit 1"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — up printed no keys$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"

echo "TEST: smoke"
dir="$(stub_target)"; echo 1 > "${dir}/smoke-rc"; printf 'smoke: FAIL (1/2 failed)\n' > "${dir}/smoke-out"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="smoke exit 1 is exit 1, quoting the provider's own line"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — smoke exited 1: smoke: FAIL (1/2 failed)$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"
dir="$(stub_target)"; echo 2 > "${dir}/smoke-rc"; : > "${dir}/smoke-out"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="smoke exit 2 (could not run) is exit 4, not a contract violation"
if [ "${rc}" -eq 4 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — smoke exited 2 (could not run)'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"
dir="$(stub_target)"; printf 'smoke: PASS (2/2)\nall done\n' > "${dir}/smoke-out"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="a PASS line that is not the LAST stdout line is exit 1"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q "^provider-check: FAIL — smoke exited 0 but its last stdout line is not 'smoke: PASS (…)': all done$"; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"
dir="$(stub_target)"
out="$(HARNESS_CONFIG_JSON="$(cfg "${dir}" '{"command":"provider","smoke":false}' "${SURFACES}")" bash "${LIB}" --pr 5 2>/dev/null)"; rc=$?
t="smoke off: it is never called, and the verdict counts 5 checks"
if [ "${rc}" -eq 0 ] && ! grep -q '^smoke' "${dir}/calls" && printf '%s\n' "${out}" | grep -q 'conforms (5 checks, pr 5)$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"

echo "TEST: down's own exit code"
dir="$(stub_target)"; touch "${dir}/down-fails"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="a down before the first up that exits non-zero is exit 1, named"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — down before the first up exited 1, want 0 (down is idempotent): down: broken$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="up was never attempted"
if ! grep -q '^up ' "${dir}/calls"; then pass "$t"; else fail "$t" "$(tr '\n' '|' < "${dir}/calls")"; fi
rm -rf "${dir}"
dir="$(stub_target)"; touch "${dir}/down-post-fails"
out="$(run_stub "${dir}" --pr 5)"; rc=$?
t="a final down that exits non-zero fails an otherwise clean run, exit 1"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — down after up exited 1, want 0 (down is idempotent)$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"

echo "TEST: config-shaped outcomes"
dir="$(stub_target)"
out="$(HARNESS_CONFIG_JSON="$(cfg "${dir}" 'null' '{}')" bash "${LIB}" 2>/dev/null)"; rc=$?
t="no hermetic tier is exit 3 with a BOOTSTRAP verdict, and nothing driven"
if [ "${rc}" -eq 3 ] && [ ! -s "${dir}/calls" ] \
   && printf '%s\n' "${out}" | grep -q '^provider-check: BOOTSTRAP — the Harness config declares no hermetic tier (Bootstrap state)'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -f "${dir}/provider"
out="$(HARNESS_CONFIG_JSON="$(cfg "${dir}" "${HERMETIC_SMOKE}" '{}')" bash "${LIB}" 2>/dev/null)"; rc=$?
t="a hermetic command that is not executable is exit 1, named"
if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^provider-check: FAIL — the hermetic command provider is not an executable file under '; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"
HARNESS_CONFIG_JSON='' AUTO_AGENT_TARGET_DIR='' bash "${LIB}" >/dev/null 2>&1; rc=$?
t="no Harness config is exit 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
HARNESS_CONFIG_JSON='' bash "${LIB}" --pr nine /nowhere >/dev/null 2>&1; rc=$?
t="a --pr that is not a number is exit 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
HARNESS_CONFIG_JSON='' timeout 10 bash "${LIB}" --pr >/dev/null 2>&1; rc=$?
t="a trailing --pr with nothing after it is exit 2, not a spin"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
HARNESS_CONFIG_JSON='' bash "${LIB}" --wat >/dev/null 2>&1; rc=$?
t="an unknown option is exit 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi

echo "TEST: the contract doc and the check say the same thing"
DOC="${ROOT}/plugin/providers/CONTRACT.md"
# Every verdict the check can print, as the stable part of its string. The doc
# is a transcription of these, and nothing but this test keeps the two honest.
# Some reasons are worded by the shared contract driver the check calls
# (lib/provider-contract.sh), so both files count as "the code".
VERDICTS=(
    "the Harness config declares no hermetic tier (Bootstrap state)"
    "is not an executable file under"
    "down before the first up exited"
    "down between the one retry exited"
    "down after up exited"
    "up exited 3 (prerequisite missing)"
    "up exited 4 (boot failed) on both attempts"
    "want 0 healthy, 3 prerequisite missing or 4 boot failed"
    "up printed no keys"
    "up printed a line that is not KEY=value"
    "up printed a key that is not an uppercase shell identifier"
    "which the up block does not carry"
    "smoke exited 0 but its last stdout line is not"
    "smoke exited 1 but its last stdout line is not"
    "smoke exited 2 (could not run)"
    "want 0 pass, 1 fail or 2 could not run"
)
missing_doc=(); missing_code=()
for v in "${VERDICTS[@]}"; do
    grep -qF -- "${v}" "${DOC}" || missing_doc+=("${v}")
    grep -qF -- "${v}" "${LIB}" "${SCRIPT_DIR}/provider-contract.sh" || missing_code+=("${v}")
done
t="every verdict the check prints is in CONTRACT.md"
if [ "${#missing_doc[@]}" -eq 0 ]; then pass "$t"; else fail "$t" "undocumented: ${missing_doc[*]}"; fi
t="every verdict CONTRACT.md documents is still in the check"
if [ "${#missing_code[@]}" -eq 0 ]; then pass "$t"; else fail "$t" "gone from the code: ${missing_code[*]}"; fi
t="the doc's exit-code table is the one the check's header documents"
if grep -q '^#   0  the provider conforms$' "${LIB}" && grep -q '| 0 | the provider conforms |' "${DOC}" \
   && grep -q '| 3 | no hermetic tier declared' "${DOC}" && grep -q '| 4 | this machine could not boot' "${DOC}"; then pass "$t"; else fail "$t"; fi

echo ""; echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
[ "${TESTS_FAILED}" -eq 0 ] || { for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done; exit 1; }
exit 0
