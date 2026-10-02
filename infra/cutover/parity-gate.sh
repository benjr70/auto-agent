#!/usr/bin/env bash
# parity-gate.sh: the gate a Host passes before its old, repo-owned daemon is
# stopped and a Harness install takes over (the cut-over, ticket #17).
#
# Why this exists: the cut-over is one switch, not a hot hand-off. The moment
# the old units stop, the Target Project's queue is nobody's until Setup has
# finished. So everything that can be proven beforehand is proven here, from
# the Harness install, while the old daemon is still the one that works:
# the suites are green from this install, the Target Project's Harness config
# is valid, the harness reads the same queue the old daemon reads, the
# Environment provider conforms, and one dry-run Fire loads the plugin and
# reaches a pick verdict. Nothing here writes to GitHub, to git or to the Host.
#
# Usage:
#   infra/cutover/parity-gate.sh [options] <target-dir>
#
#   --skip-suites       do not run run-tests.sh (about eight minutes)
#   --skip-provider     do not run the Provider check (it boots a real environment)
#   --skip-fire         do not run the dry-run Fire (it spends a little budget)
#   --old-triage <cmd>  the old daemon's read-only triage, run from <target-dir>
#                       (default: bash scripts/claude-agent/lib/pickup-triage.sh,
#                       when that file exists; else the step is skipped)
#
# Steps, each one line `parity: <step>: ok|FAIL|skipped — <detail>`:
#   install   this is a Harness install on a clean checkout (its commit is named)
#   lock      no issue holds the single-flight lock: the old daemon is between
#             Fires, so nothing races it in the checkout or in Docker
#   suites    bash run-tests.sh
#   config    bin/auto-agent check-config <target-dir>
#   queue     the old triage and the harness's agree on the verdict and on
#             which issue or PR it names
#   provider  bin/auto-agent provider-check <target-dir>; `skipped` when the
#             config declares no hermetic tier (the Bootstrap state)
#   fire      bin/auto-agent fire --dry-run <target-dir>, into a scratch State
#             dir: exit 0, the plugin loaded, a pick verdict printed, and that
#             verdict naming the issue the triage picked (or saying there is
#             none, when the triage reads an idle queue)
# The lock is read again before `provider` and before `fire`: the suites take
# minutes, and a Fire the old daemon started meanwhile fails that step.
# and a last line `parity: PASS — ...` or `parity: FAIL — <the failed steps>`.
#
# Exit codes: 0 every step ok or skipped, 1 a step failed, 2 usage.
#
# Env:
#   PARITY_CLI          the engine (default: this install's bin/auto-agent)
#   PARITY_INSTALL_DIR  the checkout the install step inspects (default: this one)
#   PARITY_SUITES_CMD   the suites (default: bash run-tests.sh in this install)
#   PARITY_STATE_DIR    the dry-run Fire's State dir (default: a temp dir, kept
#                       and named so its Fire record can be read afterwards)
#   GH_BIN              the gh CLI (default: gh)
#   AUTO_AGENT_FIRE_MODEL  pins the dry-run Fire's model, as for any Fire

set -uo pipefail

_pg_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PARITY_ROOT="$(cd "${_pg_dir}/../.." && pwd)"
# shellcheck source=../../lib/harness-config.sh
. "${PARITY_ROOT}/lib/harness-config.sh"
CLI="${PARITY_CLI:-${PARITY_ROOT}/bin/auto-agent}"
LOCK="${HARNESS_LABEL_IN_PROGRESS}"
GH="${GH_BIN:-gh}"

FAILED=()
line() {
    printf 'parity: %s: %s — %s\n' "$1" "$2" "$3"
    [ "$2" = "FAIL" ] && FAILED+=("$1")
    return 0
}
usage() { sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

skip_suites=0; skip_provider=0; skip_fire=0; old_triage=''; target=''
while [ $# -gt 0 ]; do
    case "$1" in
        --skip-suites) skip_suites=1 ;;
        --skip-provider) skip_provider=1 ;;
        --skip-fire) skip_fire=1 ;;
        --old-triage)
            [ -n "${2:-}" ] || { echo "parity-gate: --old-triage needs a command" >&2; exit 2; }
            old_triage="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        -*) echo "parity-gate: unknown option '$1'" >&2; exit 2 ;;
        *) target="$1" ;;
    esac
    shift
done
if [ -z "${target}" ] || [ ! -d "${target}" ]; then
    echo "parity-gate: name the Target Project checkout (parity-gate.sh <target-dir>)" >&2
    exit 2
fi
target="$(cd "${target}" && pwd)"

# The Daemon's own identity is not on the Host yet (Setup writes it), so every
# step runs as whoever gh is logged in as, with no Host env to mislead it.
export AUTO_AGENT_HOST_ENV="${AUTO_AGENT_HOST_ENV:-/nonexistent}"
unset HARNESS_CONFIG_JSON

# -- install
install="${PARITY_INSTALL_DIR:-${PARITY_ROOT}}"
commit="$(git -C "${install}" rev-parse --short HEAD 2>/dev/null)" || commit=''
branch="$(git -C "${install}" rev-parse --abbrev-ref HEAD 2>/dev/null)" || branch=''
if [ -z "${commit}" ]; then
    line install FAIL "${install} is not a git checkout of the harness"
elif [ -n "$(git -C "${install}" status --porcelain 2>/dev/null)" ]; then
    line install FAIL "${install} has uncommitted changes: a Harness install is a clean checkout"
else
    line install ok "${install} at ${commit} (${branch})"
fi

# -- config (before the lock: the lock needs the repo the config resolves)
cfg=''
if out="$("${CLI}" check-config "${target}" 2>&1)"; then
    line config ok "${target}/.auto-agent/harness.json matches the schema"
    cfg="$("${CLI}" show-config "${target}" 2>/dev/null)" || cfg=''
else
    line config FAIL "$(printf '%s' "${out}" | head -2 | tr '\n' ' ')"
fi
slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug // empty' 2>/dev/null)"

# lock_held : the issues holding the single-flight lock ("" none, "?" unknown)
lock_held() {
    "${GH}" issue list --repo "${slug}" --label "${LOCK}" --state open --json number --jq '[.[].number] | join(", #")' 2>/dev/null || echo '?'
}
# lock_taken_since <step> : 0 (and the step fails) when a Fire started after
# the lock step passed: the suites take minutes, and the steps after them
# boot an environment and run a session in the checkout the old daemon uses.
lock_taken_since() {
    local now; now="$(lock_held)"
    [ -z "${now}" ] && return 1
    line "$1" FAIL "#${now} took ${LOCK} while the gate was running: the old daemon started a Fire; run the gate again when it is done"
    return 0
}

# -- lock
if [ -z "${slug}" ]; then
    line lock FAIL "no resolved Harness config to read the repo from"
else
    held="$(lock_held)"
    if [ "${held}" = "?" ]; then
        line lock FAIL "gh could not list ${slug}'s issues"
    elif [ -n "${held}" ]; then
        line lock FAIL "#${held} holds ${LOCK}: a Fire is in flight; run the gate when the old daemon is between Fires"
    else
        line lock ok "no issue holds ${LOCK} in ${slug}"
    fi
fi

# -- suites
if [ "${skip_suites}" -eq 1 ]; then
    line suites skipped "--skip-suites"
else
    log="$(mktemp "${TMPDIR:-/tmp}/parity-suites-XXXXXX.log")"
    if ( cd "${PARITY_ROOT}" && bash -c "${PARITY_SUITES_CMD:-bash run-tests.sh}" ) > "${log}" 2>&1; then
        line suites ok "$(grep -E '^(Ran|Suites):' "${log}" | tail -1 | tr -s ' ') (log: ${log})"
    else
        line suites FAIL "run-tests.sh failed (log: ${log})"
    fi
fi

# -- queue
essence='{verdict, issue: (.pick.issue // .paused.issue // null), pr: (.reconcile.pr // null)}'
new_json="$("${CLI}" pickup-triage "${target}" 2>/dev/null)" || true
new="$(printf '%s' "${new_json}" | jq -c "${essence}" 2>/dev/null)" || new=''
if [ -z "${old_triage}" ] && [ -f "${target}/scripts/claude-agent/lib/pickup-triage.sh" ]; then
    old_triage='bash scripts/claude-agent/lib/pickup-triage.sh'
fi
if [ -z "${new}" ]; then
    line queue FAIL "the harness's pickup-triage printed no verdict"
elif [ -z "${old_triage}" ]; then
    line queue skipped "no old triage to compare with; the harness reads ${new}"
else
    old="$( cd "${target}" && bash -c "${old_triage}" 2>/dev/null | jq -c "${essence}" 2>/dev/null )" || old=''
    if [ -z "${old}" ]; then
        line queue FAIL "the old triage (${old_triage}) printed no verdict"
    elif [ "${old}" = "${new}" ]; then
        line queue ok "both read ${new}"
    else
        line queue FAIL "the old daemon reads ${old}, the harness reads ${new}"
    fi
fi

# -- provider
hermetic="$(printf '%s' "${cfg}" | jq -c '.verification.hermetic // null' 2>/dev/null)"
if [ "${skip_provider}" -eq 1 ]; then
    line provider skipped "--skip-provider"
elif [ -z "${cfg}" ]; then
    line provider FAIL "no resolved Harness config"
elif [ "${hermetic}" = "null" ]; then
    line provider skipped "the config declares no hermetic tier (the Bootstrap state)"
elif lock_taken_since provider; then
    :
else
    perr="$(mktemp "${TMPDIR:-/tmp}/parity-provider-XXXXXX.err")"
    out="$("${CLI}" provider-check "${target}" 2>"${perr}")"; rc=$?
    verdict="$(printf '%s\n' "${out}" | grep -E '^provider-check:' | tail -1)"
    if [ "${rc}" -eq 0 ]; then line provider ok "${verdict#provider-check: }"
    else line provider FAIL "exit ${rc}: ${verdict:-no verdict} (stderr: ${perr})"; fi
fi

# -- fire
if [ "${skip_fire}" -eq 1 ]; then
    line fire skipped "--skip-fire"
elif [ -n "${slug}" ] && lock_taken_since fire; then
    :
else
    state="${PARITY_STATE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/parity-state-XXXXXX")}"
    out="$(AUTO_AGENT_STATE_DIR="${state}" "${CLI}" fire --dry-run "${target}" 2>&1)"; rc=$?
    verdict="$(printf '%s\n' "${out}" | grep -Eo 'afk-pickup: (would-[a-z-]+|no eligible issue).*' | tail -1)"
    want="$(printf '%s' "${new}" | jq -r '.issue // .pr // empty' 2>/dev/null)"
    if [ "${rc}" -ne 0 ]; then
        printf '%s\n' "${out}" | tail -8 >&2
        line fire FAIL "the dry-run Fire exited ${rc} (State dir: ${state})"
    elif [ -z "${verdict}" ]; then
        line fire FAIL "the dry-run Fire printed no pick verdict (State dir: ${state})"
    elif [ -n "${want}" ] && ! printf '%s' "${verdict}" | grep -q "#${want}\b"; then
        line fire FAIL "the triage names #${want}, the Fire says: ${verdict} (State dir: ${state})"
    elif [ "$(printf '%s' "${new}" | jq -r '.verdict // empty' 2>/dev/null)" = "idle" ] && [ "${verdict#afk-pickup: no eligible issue}" = "${verdict}" ]; then
        line fire FAIL "the triage reads an idle queue, the Fire says: ${verdict} (State dir: ${state})"
    else
        line fire ok "${verdict} (State dir: ${state})"
    fi
fi

if [ "${#FAILED[@]}" -gt 0 ]; then
    echo "parity: FAIL — ${FAILED[*]}"
    exit 1
fi
echo "parity: PASS — the old daemon can be stopped"
