#!/usr/bin/env bash
# Fire wrapper: one Daemon pass. Replaces Smart-Smoker-V2's `agent-run`
# (ADR 0001): every Fire loads the pinned Harness install as a plugin with the
# `--settings` baseline, runs `claude -p` in stream-json through the rate-limit
# tap, and ends by writing a Fire record. Text mode is gone.
#
# A Fire prompts `/auto-agent:afk-pickup`, the core lane's entry skill in the
# plugin, which does exactly one unit of work (reconcile an Agent PR, resume
# paused work, pick an AFK ticket, or route a Decision ticket). The wrapper
# owns what the skill cannot: the checkout hygiene before the skill runs, the
# lock cleanup when the Fire crashes, and the stable lines the Daemon and
# Setup read. The pause-on-exhaustion path (freeze a `wip:` commit, flip the
# lock to `AFK:paused`) arrives with the budget-gate Slice; until then an
# exhausted Fire is a failed Fire and its lock is cleared like any crash.
#
# The Fire runs `--permission-mode bypassPermissions`, carried over from
# `agent-run`: a Daemon has no human to answer prompts. The `--settings`
# baseline's deny list still binds in that mode (verified live: a forced push
# is refused), and the Target Project's own `.claude/settings.json` merges on
# top of the baseline (verified live: its hooks run).
#
# Source this file, then:
#
#   fire_run [--dry-run | --noop] [<target-dir>]
#       Runs one Fire against the Target Project at <target-dir> (default:
#       AUTO_AGENT_TARGET_DIR from the Host env; for --dry-run and --noop, the
#       fixture Target Project in the plugin).
#
#       Preflight fails closed, and a preflight failure is still a Fire with a
#       record (phase "preflight"): 1 for a bad Harness config, 2 for a missing
#       settings baseline or a checkout that cannot be reset, 3 when the repo
#       or its default branch cannot be resolved. The resolved Harness config
#       is exported as HARNESS_CONFIG_JSON so no lib or skill inside the Fire
#       resolves it again (one `gh repo view` per Fire).
#
#       A plain Fire (kind "pickup") first puts the checkout on the tip of the
#       detected default branch (fetch, reset, checkout, reset to origin): a
#       prior Fire may have left it on its feat/issue-<N> branch or with crash
#       debris, and anything worth keeping is already committed on its own
#       branch. Then `/auto-agent:afk-pickup` runs. When claude exits non-zero
#       the wrapper clears the single-flight lock the Fire took, scoped to the
#       unit of work the Fire's own `picked:` / `resolve:` line named (never a
#       lock another Fire holds): a pick goes AFK:in-progress -> AFK:failed
#       with a comment; a reconcile restores AFK:done and comments on the PR;
#       a resolve is failed the same way unless its terminal `resolve:` line
#       already settled the ticket.
#
#       --dry-run (kind "dry-run") prompts `/auto-agent:afk-pickup --dry-run`:
#       the skill reaches its pick verdict and reports it without a single
#       GitHub or git write. No checkout hygiene, no lock cleanup. Returns 0
#       only when the stream's init event lists the plugin and the skill and
#       the skill ended on one of its dry-run lines. This is the Fire seam
#       Setup's verify stage and the harness's own tests demo on.
#
#       --noop (kind "noop") prompts the no-op `/auto-agent:dry-run` skill: it
#       proves the plugin loads and a Fire runs end to end with no GitHub at
#       all. Returns 0 only when the skill printed `dry-run: ok`.
#
#       Returns the claude exit code otherwise.
#
# Writes into the State dir (host_env_state_dir):
#   logs/<fire-id>.stream.jsonl   the raw stream-json the Fire produced
#   logs/<fire-id>.stderr.log     claude's stderr
#   rate-limits.json / .jsonl     from the tap (lib/rate-limits-tap.sh)
#   fires/<fire-id>.json          the Fire record (lib/fire-record.sh)
#
# Prints stable lines on stdout for the Daemon and Setup's verify stage:
#   fire: id=<id> kind=<kind> exit=<n> record=<path>
#   fire: plugin auto-agent loaded=<yes|no> skill=<name> listed=<yes|no>
#   fire: work=<none | pick #N | reconcile PR #P | resolve #N | dry-run <line> | unknown>
#                                        (pickup and dry-run kinds)
#   AGENT_RUN_NO_WORK=1                  (the skill found nothing to do: the
#                                        Daemon sleeps out the window)
#   fire: lock <what was restored>       (pickup kind, after a crash)
#   fire: dry-run ok=<yes|no>            (dry-run only)
#   fire: noop ok=<yes|no>               (noop only)
#
# Environment:
#   CLAUDE_BIN, GH_BIN, GIT_BIN   injected for tests (default: claude, gh, git)
#   AUTO_AGENT_ROOT               the Harness install (default: the parent of lib/)
#   AUTO_AGENT_STATE_DIR          the State dir (Host env; see lib/host-env.sh)
#   AUTO_AGENT_TARGET_DIR         the Target Project checkout (Host env)
#   AUTO_AGENT_FIRE_MODEL         pins --model for the whole Fire (model policy)
#   AUTO_AGENT_GATE_VERDICT_FILE  the Gate verdict JSON to embed; without one the
#                                 record carries a `sensor: none` verdict
#   DAEMON_GH_LOGIN               the machine login (Host env); the pick libs
#                                 read it, the wrapper only passes it through
# Exported into the Fire for the skills and libs it runs:
#   AUTO_AGENT_ROOT, AUTO_AGENT_TARGET_DIR, AUTO_AGENT_STATE_DIR,
#   HARNESS_CONFIG_JSON (pickup and dry-run kinds)

_fire_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTO_AGENT_ROOT="${AUTO_AGENT_ROOT:-$(cd "${_fire_lib_dir}/.." && pwd)}"
# shellcheck source=harness-config.sh
. "${_fire_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_fire_lib_dir}/host-env.sh"
# shellcheck source=rate-limits-tap.sh
. "${_fire_lib_dir}/rate-limits-tap.sh"
# shellcheck source=fire-record.sh
. "${_fire_lib_dir}/fire-record.sh"

FIRE_PLUGIN_NAME="auto-agent"
FIRE_PLUGIN_DIR="${AUTO_AGENT_ROOT}/plugin"
FIRE_SETTINGS_BASELINE="${FIRE_PLUGIN_DIR}/settings/baseline.json"
FIRE_FIXTURE_TARGET="${FIRE_PLUGIN_DIR}/fixtures/target-project"
FIRE_SKILL_NOOP="${FIRE_PLUGIN_NAME}:dry-run"
FIRE_SKILL_PICKUP="${FIRE_PLUGIN_NAME}:afk-pickup"
FIRE_NOOP_OK_LINE="dry-run: ok"

_fire_err() { echo "fire: $*" >&2; }
_fire_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# _fire_gate_verdict -> JSON: the file the Daemon handed over, else a
# `sensor: none` verdict in the ADR 0008 shape so the record's gate block is
# never missing.
_fire_gate_verdict() {
    local file="${AUTO_AGENT_GATE_VERDICT_FILE:-}"
    if [ -n "${file}" ] && [ -f "${file}" ] && jq -e 'type == "object"' "${file}" >/dev/null 2>&1; then
        jq -c . "${file}"
        return 0
    fi
    jq -n -c --arg mode "${CLAUDE_AUTH_MODE:-}" --arg now "$(_fire_now)" '{
        authMode: (if $mode == "" then null else $mode end),
        sensor: "none", state: "unavailable", remainPct: null, resetAt: null,
        shouldFire: true, observedAt: $now, limits: [],
        warnings: ["no Gate verdict was supplied to this Fire"]
    }'
}

# _fire_write_record <exit> <phase>
# Reads the Fire context from the caller's scope (bash dynamic scoping):
# state id kind prompt skill dry target started stream stderr.
_fire_write_record() {
    local rc="$1" phase="$2" summary record
    summary="$(fire_record_summarize_stream "${stream}" "${FIRE_PLUGIN_NAME}" "${skill}")"
    record="$(jq -n -c \
        --arg id "${id}" --arg kind "${kind}" --arg prompt "${prompt}" --argjson dry "${dry}" \
        --arg target "${target}" --arg started "${started}" --arg ended "$(_fire_now)" \
        --argjson rc "${rc}" --arg phase "${phase}" --arg model "${AUTO_AGENT_FIRE_MODEL:-}" \
        --arg stream "${stream}" --arg stderr "${stderr}" \
        --argjson summary "${summary}" --argjson gate "$(_fire_gate_verdict)" '
        {
          fireId: $id, kind: $kind, prompt: $prompt, startedAt: $started, endedAt: $ended,
          exit: $rc, phase: $phase, dryRun: $dry, target: $target,
          model: (if $model == "" then null else $model end),
          issue: $summary.issue,
          log: { stream: $stream, stderr: $stderr },
          plugin: $summary.plugin, result: $summary.result, work: $summary.work,
          rateLimit: $summary.rateLimit,
          gate: $gate
        }')"
    fire_record_write "${state}" "${id}" "${record}"
}

# _fire_preflight_fail <exit> <message> : record, stable line, stderr; echoes nothing else.
_fire_preflight_fail() {
    _fire_write_record "$1" preflight
    echo "fire: id=${id} kind=${kind} exit=$1 record=${record}"
    _fire_err "preflight failed: $2"
    return "$1"
}

# _fire_checkout_hygiene <target> <default-branch>
# Every plain Fire starts from the tip of the default branch. Returns 1 only
# when the branch cannot be checked out at all; fetch and reset failures are
# warnings (a stale but valid checkout still lets the skill work).
_fire_checkout_hygiene() {
    local target="$1" base="$2" git="${GIT_BIN:-git}"
    "${git}" -C "${target}" fetch --quiet origin "${base}" \
        || _fire_err "warn: git fetch failed, continuing with cached refs"
    "${git}" -C "${target}" reset --hard --quiet || true
    "${git}" -C "${target}" checkout --quiet "${base}" || return 1
    "${git}" -C "${target}" reset --hard --quiet "origin/${base}" \
        || _fire_err "warn: reset to origin/${base} failed, continuing on local ${base}"
    echo "fire: checkout reset to ${base}"
}

# _fire_work_line <record> -> the `fire: work=` value
_fire_work_line() {
    jq -r '.work | if .kind == "pick" then "pick #\(.issue)"
                   elif .kind == "reconcile" then "reconcile PR #\(.pr)"
                   elif .kind == "resolve" then "resolve #\(.issue)"
                   elif .kind == "dry-run" then "dry-run \(.line)"
                   elif .kind == "none" then "none"
                   else "unknown" end' "$1"
}

# _fire_clear_lock <record> <repo-slug>
# After a crashed pickup Fire: clear the single-flight lock THIS Fire took, from
# the unit of work its own output named. Every call is best-effort (|| true): a
# cleanup must never itself fail the Fire. Never touches a lock the Fire did not
# name. Carried over from agent-run's fail_inflight.
_fire_clear_lock() {
    local record="$1" slug="$2" gh="${GH_BIN:-gh}"
    local kind issue pr settled ts
    kind="$(jq -r '.work.kind // "none"' "${record}")"
    issue="$(jq -r '.work.issue // empty' "${record}")"
    pr="$(jq -r '.work.pr // empty' "${record}")"
    settled="$(jq -r '.work.settled // empty' "${record}")"
    ts="$(_fire_now)"
    case "${kind}" in
        resolve)
            case "${settled}" in
                hitl)
                    echo "fire: lock resolve #${issue} relabelled HITL before the crash, nothing restored" ;;
                done)
                    "${gh}" issue edit "${issue}" --repo "${slug}" --remove-label "${HARNESS_LABEL_IN_PROGRESS}" --add-label "${HARNESS_LABEL_DONE}" >/dev/null 2>&1 || true
                    echo "fire: lock resolve #${issue} crashed after DONE, left ${HARNESS_LABEL_DONE}" ;;
                failed)
                    "${gh}" issue edit "${issue}" --repo "${slug}" --remove-label "${HARNESS_LABEL_IN_PROGRESS}" --add-label "${HARNESS_LABEL_FAILED}" >/dev/null 2>&1 || true
                    echo "fire: lock resolve #${issue} already reported its own failure, no second comment" ;;
                *)
                    "${gh}" issue edit "${issue}" --repo "${slug}" --remove-label "${HARNESS_LABEL_IN_PROGRESS}" --add-label "${HARNESS_LABEL_FAILED}" >/dev/null 2>&1 || true
                    "${gh}" issue comment "${issue}" --repo "${slug}" --body "Resolve Fire crashed at ${ts}: the resolve skill exited non-zero. Lock cleared (${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_FAILED}); the ticket stays open for human triage." >/dev/null 2>&1 || true
                    echo "fire: lock resolve #${issue} ${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_FAILED}" ;;
            esac ;;
        reconcile)
            if [ -z "${issue}" ]; then
                echo "fire: lock reconcile PR #${pr} has no backing issue, nothing to clear"
                return 0
            fi
            "${gh}" issue edit "${issue}" --repo "${slug}" --remove-label "${HARNESS_LABEL_IN_PROGRESS}" --add-label "${HARNESS_LABEL_DONE}" >/dev/null 2>&1 || true
            "${gh}" pr comment "${pr}" --repo "${slug}" --body "Reconcile Fire crashed at ${ts}: lock restored (${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_DONE} on issue #${issue}). PR left as-is for the next Fire or human triage." >/dev/null 2>&1 || true
            echo "fire: lock reconcile PR #${pr} restored #${issue} ${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_DONE}" ;;
        pick)
            "${gh}" issue edit "${issue}" --repo "${slug}" --remove-label "${HARNESS_LABEL_IN_PROGRESS}" --add-label "${HARNESS_LABEL_FAILED}" >/dev/null 2>&1 || true
            "${gh}" issue comment "${issue}" --repo "${slug}" --body "Fire failed at ${ts}: the pickup skill exited non-zero. Lock cleared (${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_FAILED}) for human triage." >/dev/null 2>&1 || true
            echo "fire: lock pick #${issue} ${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_FAILED}" ;;
        *)
            echo "fire: lock nothing picked, nothing to clear" ;;
    esac
}

# fire_run [--dry-run | --noop] [<target-dir>]
fire_run() {
    local kind="pickup" dry=false target=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --dry-run) kind="dry-run"; dry=true ;;
            --noop) kind="noop"; dry=true ;;
            --) shift; break ;;
            -*) _fire_err "unknown option '$1'"; return 2 ;;
            *) target="$1" ;;
        esac
        shift
    done
    [ -n "${target}" ] || target="${1:-}"

    host_env_load
    if [ -z "${target}" ]; then
        target="${AUTO_AGENT_TARGET_DIR:-}"
        [ -n "${target}" ] || { [ "${dry}" = "true" ] && target="${FIRE_FIXTURE_TARGET}"; }
    fi
    if [ -z "${target}" ]; then
        _fire_err "no Target Project: pass <target-dir> or set AUTO_AGENT_TARGET_DIR in the Host env"
        return 2
    fi
    if [ ! -d "${target}" ]; then
        _fire_err "target dir not found: ${target}"
        return 2
    fi
    target="$(cd "${target}" && pwd)"

    local state; state="$(host_env_state_dir)"
    mkdir -p "${state}/logs" "${state}/${FIRE_RECORD_DIRNAME}" || {
        _fire_err "cannot create the State dir ${state}"
        return 2
    }

    # What the skills and libs inside the Fire read.
    export AUTO_AGENT_ROOT AUTO_AGENT_TARGET_DIR="${target}" AUTO_AGENT_STATE_DIR="${state}"

    # The Fire context every helper reads.
    local id started skill prompt
    id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
    started="$(_fire_now)"
    case "${kind}" in
        noop)    skill="${FIRE_SKILL_NOOP}";   prompt="/${skill}" ;;
        dry-run) skill="${FIRE_SKILL_PICKUP}"; prompt="/${skill} --dry-run" ;;
        *)       skill="${FIRE_SKILL_PICKUP}"; prompt="/${skill}" ;;
    esac
    local stream="${state}/logs/${id}.stream.jsonl" stderr="${state}/logs/${id}.stderr.log"
    local record; record="$(fire_record_path "${state}" "${id}")"

    # Preflight: fail closed before anything else runs.
    if ! harness_config_check "${target}"; then
        _fire_preflight_fail 1 "the Harness config of ${target} is invalid"; return $?
    fi
    if [ ! -f "${FIRE_SETTINGS_BASELINE}" ]; then
        _fire_preflight_fail 2 "settings baseline missing: ${FIRE_SETTINGS_BASELINE}"; return $?
    fi
    local cfg="" base=""
    if [ "${kind}" != "noop" ]; then
        cfg="$(harness_config_load "${target}" | jq -c .)" || {
            _fire_preflight_fail 3 "cannot resolve the repo or default branch of ${target}"; return $?
        }
        export HARNESS_CONFIG_JSON="${cfg}"
        base="$(printf '%s' "${cfg}" | jq -r '.repo.default_branch')"
    fi
    if [ "${kind}" = "pickup" ]; then
        _fire_checkout_hygiene "${target}" "${base}" || {
            _fire_preflight_fail 2 "cannot check out ${base} in ${target}"; return $?
        }
    fi

    local -a model_args=()
    [ -n "${AUTO_AGENT_FIRE_MODEL:-}" ] && model_args=(--model "${AUTO_AGENT_FIRE_MODEL}")

    local rc
    ( cd "${target}" && "${CLAUDE_BIN:-claude}" \
        --print \
        --permission-mode bypassPermissions \
        --plugin-dir "${FIRE_PLUGIN_DIR}" \
        --settings "${FIRE_SETTINGS_BASELINE}" \
        --output-format stream-json \
        --verbose \
        "${model_args[@]}" \
        "${prompt}" < /dev/null 2> "${stderr}" ) \
      | rate_limits_tap "${state}" "${id}" > "${stream}"
    rc=${PIPESTATUS[0]}

    _fire_write_record "${rc}" claude

    local loaded_listed
    loaded_listed="$(jq -r '(if .plugin.loaded then "yes" else "no" end) + " " + (if .plugin.skillListed then "yes" else "no" end)' "${record}")"
    echo "fire: id=${id} kind=${kind} exit=${rc} record=${record}"
    echo "fire: plugin ${FIRE_PLUGIN_NAME} loaded=${loaded_listed% *} skill=${skill} listed=${loaded_listed#* }"

    case "${kind}" in
        noop)
            if fire_record_noop_ok "${record}" "${FIRE_NOOP_OK_LINE}"; then
                echo "fire: noop ok=yes"
            else
                echo "fire: noop ok=no"
                [ "${rc}" -ne 0 ] && return "${rc}"
                return 1
            fi ;;
        dry-run)
            echo "fire: work=$(_fire_work_line "${record}")"
            [ "$(jq -r '.work.kind' "${record}")" = "none" ] && echo "AGENT_RUN_NO_WORK=1"
            if fire_record_dry_run_ok "${record}"; then
                echo "fire: dry-run ok=yes"
            else
                echo "fire: dry-run ok=no"
                [ "${rc}" -ne 0 ] && return "${rc}"
                return 1
            fi ;;
        *)
            echo "fire: work=$(_fire_work_line "${record}")"
            [ "$(jq -r '.work.kind' "${record}")" = "none" ] && echo "AGENT_RUN_NO_WORK=1"
            if [ "${rc}" -ne 0 ]; then
                _fire_clear_lock "${record}" "$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
            fi ;;
    esac
    return "${rc}"
}
