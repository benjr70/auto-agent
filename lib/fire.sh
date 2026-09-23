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
# lock cleanup when the Fire crashes, the pause when the Fire ran out of
# budget or its credential died, and the stable lines the Daemon and Setup
# read.
#
# After claude exits the exhaustion classifier (lib/exhaustion-classifier.sh)
# reads the exit code, the result text, the stream's system events, stderr
# and this Fire's last tapped rate-limit record, and the record carries its
# verdict as `outcome`. A non-zero pickup Fire then ends one of three ways:
#   EXHAUSTED  the unit of work is paused, not failed: a pick freezes partial
#              work in a `wip:` commit and moves its lock AFK:in-progress ->
#              AFK:paused (the branch stays for resume); a reconcile restores
#              AFK:done; a resolve drops its lock and research branch so the
#              next Fire restarts it. The wrapper prints
#              `AGENT_RUN_RESET_AT=<iso|empty>` and exits 0.
#   AUTH_DEAD  the same pause (the work is not the ticket's fault), then the
#              Daemon is parked (lib/daemon-park.sh: parked.json plus the
#              reused AFK:needs-human issue) and `AGENT_RUN_AUTH_DEAD=1` is
#              printed; the exit code is claude's.
#   FAILED     the lock is cleared as a crash (below).
# The outcome seeds the next Gate verdict on a setup-token Host
# (lib/usage-sensor.sh).
#
# A Fire also carries `--mcp-config`: the MCP servers the verification round
# drives the Target Project's `browser` and `electron` Surfaces through,
# rendered per Fire from the Harness config (lib/surface-launch.sh). A project
# that declares no UI Surface renders none, and the flag is left off.
#
# The Fire runs `--permission-mode bypassPermissions`, carried over from
# `agent-run`: a Daemon has no human to answer prompts. The `--settings`
# baseline's deny list still binds in that mode (verified live: a forced push
# is refused), and the Target Project's own `.claude/settings.json` merges on
# top of the baseline (verified live: its hooks run).
#
# Source this file, then:
#
#   fire_run [--dry-run | --resolve-dry-run <N> | --noop] [<target-dir>]
#       Runs one Fire against the Target Project at <target-dir> (default:
#       AUTO_AGENT_TARGET_DIR from the Host env; for the three dry kinds, the
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
#       --resolve-dry-run <N> (kind "resolve-dry-run") prompts
#       `/auto-agent:afk-resolve --issue <N> --dry-run`: the resolve lane
#       reads Decision ticket <N> and its Map, runs the research and writes
#       the findings file under the config's research prefix in the checkout,
#       with no GitHub or git write (no claim, branch, PR, comment or close),
#       and ends on `afk-resolve: would-open PR research/<slug> (<path>)`.
#       Returns 0 only under the same conditions as --dry-run. This is the
#       Fire seam the resolve lane demos on (issue #29 AC 1); the findings
#       file is left untracked for the human to read and the next plain
#       Fire's checkout hygiene removes it.
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
#   fires/<fire-id>.json          the Fire record (lib/fire-record.sh), whose
#                                 `bootstrap` field says whether the Target
#                                 Project was in the Bootstrap state for this
#                                 Fire — read from the DEFAULT-branch config the
#                                 preflight resolved, never from a PR head
#
# Prints stable lines on stdout for the Daemon and Setup's verify stage:
#   fire: id=<id> kind=<kind> exit=<n> record=<path>
#   fire: plugin auto-agent loaded=<yes|no> skill=<name> listed=<yes|no>
#   fire: work=<none | pick #N | reconcile PR #P | resolve #N | dry-run <line> | unknown>
#                                        (pickup, dry-run and resolve-dry-run kinds)
#   AGENT_RUN_NO_WORK=1                  (the skill found nothing to do: the
#                                        Daemon sleeps out the window)
#   AGENT_RUN_RESET_AT=<iso|empty>       (pickup kind, EXHAUSTED: sleep to it;
#                                        empty when unknown, or when the limit
#                                        was per-model and the Daemon should
#                                        re-gate at once instead)
#   AGENT_RUN_MODEL_LIMIT=<scope>        (pickup kind, EXHAUSTED on a per-model
#                                        limit: the next Gate verdict switches
#                                        the Fire model)
#   AGENT_RUN_AUTH_DEAD=1                (pickup kind, AUTH_DEAD: the Daemon parks)
#   fire: lock <what was restored>       (pickup kind, after a crash or a pause)
#   fire: dry-run ok=<yes|no>            (dry-run and resolve-dry-run)
#   fire: noop ok=<yes|no>               (noop only)
#
# Environment:
#   CLAUDE_BIN, GH_BIN, GIT_BIN   injected for tests (default: claude, gh, git)
#   AUTO_AGENT_ROOT               the Harness install (default: the parent of lib/)
#   AUTO_AGENT_STATE_DIR          the State dir (Host env; see lib/host-env.sh)
#   AUTO_AGENT_TARGET_DIR         the Target Project checkout (Host env)
#   AUTO_AGENT_FIRE_MODEL         pins --model for the whole Fire; when unset the
#                                 Gate verdict's `fireModel` (the model policy's
#                                 switch) is used, else claude's default
#   AUTO_AGENT_GATE_VERDICT_FILE  the Gate verdict JSON to embed (the usage
#                                 sensor's output); without one the record
#                                 carries a `sensor: none` verdict
#   DAEMON_GH_LOGIN               the machine login (Host env); the pick libs
#                                 read it, the wrapper only passes it through
# Exported into the Fire for the skills and libs it runs:
#   AUTO_AGENT_ROOT, AUTO_AGENT_TARGET_DIR, AUTO_AGENT_STATE_DIR,
#   HARNESS_CONFIG_JSON (every kind but noop)

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
# shellcheck source=exhaustion-classifier.sh
. "${_fire_lib_dir}/exhaustion-classifier.sh"
# shellcheck source=daemon-park.sh
. "${_fire_lib_dir}/daemon-park.sh"
# shellcheck source=bootstrap-state.sh
. "${_fire_lib_dir}/bootstrap-state.sh"

FIRE_PLUGIN_NAME="auto-agent"
FIRE_PLUGIN_DIR="${AUTO_AGENT_ROOT}/plugin"
FIRE_SETTINGS_BASELINE="${FIRE_PLUGIN_DIR}/settings/baseline.json"
FIRE_FIXTURE_TARGET="${FIRE_PLUGIN_DIR}/fixtures/target-project"
FIRE_SKILL_NOOP="${FIRE_PLUGIN_NAME}:dry-run"
FIRE_SKILL_PICKUP="${FIRE_PLUGIN_NAME}:afk-pickup"
FIRE_SKILL_RESOLVE="${FIRE_PLUGIN_NAME}:afk-resolve"
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

# _fire_outcome <exit> <stream> <stderr>
# Reads state and id from the caller's scope, like _fire_write_record.
# The exhaustion classifier over what claude left behind: the result text and
# the system events of the stream (not the whole transcript, whose tool
# output may mention limits in passing), stderr, and this Fire's last tapped
# rate-limit record when the tap wrote one.
_fire_outcome() {
    local rc="$1" stream="$2" stderr="$3" rec=""
    if [ -f "${state}/${RATE_LIMITS_JSON}" ] \
       && [ "$(jq -r '.fireId // ""' "${state}/${RATE_LIMITS_JSON}" 2>/dev/null)" = "${id}" ]; then
        rec="$(jq -c . "${state}/${RATE_LIMITS_JSON}" 2>/dev/null)" || rec=""
    fi
    {
        [ -f "${stream}" ] && jq -R -r '(fromjson? // empty) | if .type == "result" then (.result // "") elif .type == "system" then tojson else empty end' "${stream}" 2>/dev/null
        [ -f "${stderr}" ] && cat "${stderr}"
    } | exhaustion_classify "${rc}" "${rec}"
}

# _fire_write_record <exit> <phase>
# Reads the Fire context from the caller's scope (bash dynamic scoping):
# state id kind prompt skill dry target started stream stderr effective_model
# gate (the Gate verdict, read once per Fire) bootstrap (the Bootstrap state,
# null before the config resolved) outcome (null before claude ran).
_fire_write_record() {
    local rc="$1" phase="$2" summary record
    summary="$(fire_record_summarize_stream "${stream}" "${FIRE_PLUGIN_NAME}" "${skill}")"
    record="$(jq -n -c \
        --arg id "${id}" --arg kind "${kind}" --arg prompt "${prompt}" --argjson dry "${dry}" \
        --arg target "${target}" --arg started "${started}" --arg ended "$(_fire_now)" \
        --argjson rc "${rc}" --arg phase "${phase}" --arg model "${effective_model:-${AUTO_AGENT_FIRE_MODEL:-}}" \
        --arg stream "${stream}" --arg stderr "${stderr}" \
        --argjson summary "${summary}" --argjson gate "${gate}" \
        --argjson bootstrap "${bootstrap:-null}" \
        --argjson outcome "${outcome:-null}" '
        {
          fireId: $id, kind: $kind, prompt: $prompt, startedAt: $started, endedAt: $ended,
          exit: $rc, phase: $phase, dryRun: $dry, target: $target,
          model: (if $model == "" then null else $model end),
          issue: $summary.issue,
          log: { stream: $stream, stderr: $stderr },
          plugin: $summary.plugin, result: $summary.result, work: $summary.work,
          rateLimit: $summary.rateLimit,
          bootstrap: $bootstrap,
          outcome: $outcome,
          gate: $gate
        }')"
    fire_record_write "${state}" "${id}" "${record}"
}

# _fire_pause_inflight <record> <repo-slug> <target> <reset-at> <why>
# After an EXHAUSTED or AUTH_DEAD pickup Fire: pause the unit of work THIS
# Fire named, never fail it. Carried over from agent-run's pause_inflight,
# scoped like _fire_clear_lock. Every step is best-effort (|| true): a pause
# must never itself fail the Fire and re-wedge the pipeline.
_fire_pause_inflight() {
    local record="$1" slug="$2" target="$3" reset="$4" why="$5"
    local gh="${GH_BIN:-gh}" git="${GIT_BIN:-git}"
    local kind issue pr rslug ts note
    kind="$(jq -r '.work.kind // "none"' "${record}")"
    issue="$(jq -r '.work.issue // empty' "${record}")"
    pr="$(jq -r '.work.pr // empty' "${record}")"
    rslug="$(jq -r '.work.slug // empty' "${record}")"
    ts="$(_fire_now)"
    case "${kind}" in
        pick)
            # Freeze partial edits so the next checkout hygiene cannot lose them.
            "${git}" -C "${target}" add -A >/dev/null 2>&1 || true
            if ! "${git}" -C "${target}" diff --cached --quiet 2>/dev/null; then
                "${git}" -C "${target}" commit --quiet -m "wip: freeze partial work on #${issue} (${why})" >/dev/null 2>&1 || true
            fi
            _fire_relabel "${gh}" "${slug}" "${issue}" "${HARNESS_LABEL_PAUSED}"
            note="Fire paused at ${ts} — ${why} mid-Fire. Branch kept for resume."
            [ -n "${reset}" ] && note="${note} Budget resets at ${reset}."
            "${gh}" issue comment "${issue}" --repo "${slug}" --body "${note}" >/dev/null 2>&1 || true
            echo "fire: lock pick #${issue} ${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_PAUSED} (${why})" ;;
        reconcile)
            if [ -z "${issue}" ]; then
                echo "fire: lock reconcile PR #${pr} has no backing issue, nothing to pause"
                return 0
            fi
            _fire_relabel "${gh}" "${slug}" "${issue}" "${HARNESS_LABEL_DONE}"
            "${gh}" pr comment "${pr}" --repo "${slug}" --body "Reconcile Fire stopped at ${ts} (${why}): lock restored (${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_DONE} on issue #${issue}). The next Fire reconciles again." >/dev/null 2>&1 || true
            echo "fire: lock reconcile PR #${pr} restored #${issue} ${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_DONE} (${why})" ;;
        resolve)
            # A resolve is never paused: drop the lock and the half-written
            # research branch so the next Fire restarts it from the ticket.
            "${gh}" issue edit "${issue}" --repo "${slug}" --remove-label "${HARNESS_LABEL_IN_PROGRESS}" >/dev/null 2>&1 || true
            if [ -n "${rslug}" ]; then
                "${git}" -C "${target}" push --quiet origin --delete "${HARNESS_BRANCH_RESEARCH_PREFIX}${rslug}" >/dev/null 2>&1 || true
            fi
            "${gh}" issue comment "${issue}" --repo "${slug}" --body "Resolve Fire stopped at ${ts} (${why}): lock dropped, the next Fire restarts the resolve." >/dev/null 2>&1 || true
            echo "fire: lock resolve #${issue} ${HARNESS_LABEL_IN_PROGRESS} dropped, restarts next Fire (${why})" ;;
        *)
            echo "fire: lock nothing picked, nothing to pause" ;;
    esac
}

# _fire_preflight_fail <exit> <message>
# Writes the record (through _fire_write_record, so it reads the same Fire
# context from the caller's scope: id kind record and the rest), prints the
# stable line and the stderr reason, returns <exit>.
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

# _fire_relabel <gh> <slug> <issue> <to-label>
# Moves an issue's lock label: AFK:in-progress off, <to-label> on. Best-effort.
_fire_relabel() {
    "$1" issue edit "$3" --repo "$2" --remove-label "${HARNESS_LABEL_IN_PROGRESS}" --add-label "$4" >/dev/null 2>&1 || true
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
                    _fire_relabel "${gh}" "${slug}" "${issue}" "${HARNESS_LABEL_DONE}"
                    echo "fire: lock resolve #${issue} crashed after DONE, left ${HARNESS_LABEL_DONE}" ;;
                failed)
                    _fire_relabel "${gh}" "${slug}" "${issue}" "${HARNESS_LABEL_FAILED}"
                    echo "fire: lock resolve #${issue} already reported its own failure, no second comment" ;;
                *)
                    _fire_relabel "${gh}" "${slug}" "${issue}" "${HARNESS_LABEL_FAILED}"
                    "${gh}" issue comment "${issue}" --repo "${slug}" --body "Resolve Fire crashed at ${ts}: the resolve skill exited non-zero. Lock cleared (${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_FAILED}); the ticket stays open for human triage." >/dev/null 2>&1 || true
                    echo "fire: lock resolve #${issue} ${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_FAILED}" ;;
            esac ;;
        reconcile)
            if [ -z "${issue}" ]; then
                echo "fire: lock reconcile PR #${pr} has no backing issue, nothing to clear"
                return 0
            fi
            _fire_relabel "${gh}" "${slug}" "${issue}" "${HARNESS_LABEL_DONE}"
            "${gh}" pr comment "${pr}" --repo "${slug}" --body "Reconcile Fire crashed at ${ts}: lock restored (${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_DONE} on issue #${issue}). PR left as-is for the next Fire or human triage." >/dev/null 2>&1 || true
            echo "fire: lock reconcile PR #${pr} restored #${issue} ${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_DONE}" ;;
        pick)
            _fire_relabel "${gh}" "${slug}" "${issue}" "${HARNESS_LABEL_FAILED}"
            "${gh}" issue comment "${issue}" --repo "${slug}" --body "Fire failed at ${ts}: the pickup skill exited non-zero. Lock cleared (${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_FAILED}) for human triage." >/dev/null 2>&1 || true
            echo "fire: lock pick #${issue} ${HARNESS_LABEL_IN_PROGRESS} -> ${HARNESS_LABEL_FAILED}" ;;
        *)
            echo "fire: lock nothing picked, nothing to clear" ;;
    esac
}

# fire_run [--dry-run | --resolve-dry-run <N> | --noop] [<target-dir>]
fire_run() {
    local kind="pickup" dry=false target="" resolve_issue=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --dry-run) kind="dry-run"; dry=true ;;
            --resolve-dry-run)
                kind="resolve-dry-run"; dry=true; resolve_issue="${2:-}"
                if ! printf '%s' "${resolve_issue}" | grep -Eq '^[0-9]+$'; then
                    _fire_err "--resolve-dry-run needs an issue number"; return 2
                fi
                shift ;;
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
        resolve-dry-run) skill="${FIRE_SKILL_RESOLVE}"; prompt="/${skill} --issue ${resolve_issue} --dry-run" ;;
        *)       skill="${FIRE_SKILL_PICKUP}"; prompt="/${skill}" ;;
    esac
    local stream="${state}/logs/${id}.stream.jsonl" stderr="${state}/logs/${id}.stderr.log"
    local record; record="$(fire_record_path "${state}" "${id}")"
    local gate; gate="$(_fire_gate_verdict)"
    # The Bootstrap state as the DEFAULT branch declares it (ADR 0007: the
    # preflight validates the default-branch copy; only a verification round
    # reads the PR head). null until the config resolves, and for a noop Fire
    # that never resolves one.
    local bootstrap=null

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
        # The fact as bootstrap-state.sh reads it, never re-spelled here.
        if bootstrap_in_state "${cfg}"; then bootstrap=true; else bootstrap=false; fi
    fi
    if [ "${kind}" = "pickup" ]; then
        _fire_checkout_hygiene "${target}" "${base}" || {
            _fire_preflight_fail 2 "cannot check out ${base} in ${target}"; return $?
        }
    fi

    # The MCP servers the verification round drives the UI Surfaces through
    # (Slice #33). The Surface names come from the Harness config, so the
    # registry is rendered per Fire; a Target Project with no `browser` or
    # `electron` Surface renders none and the flag is left off. A render that
    # fails is a warning, never a failed Fire: every other lane works without
    # it.
    local -a mcp_args=()
    if [ -n "${cfg}" ]; then
        local mcp_file="${state}/mcp/${id}.json"
        local mcp_err; mcp_err="$(bash "${AUTO_AGENT_ROOT}/lib/surface-launch.sh" mcp-config "${target}" --out "${mcp_file}" 2>&1 >/dev/null)"
        if [ -n "${mcp_err}" ]; then
            _fire_err "the UI Surfaces' MCP registry could not be rendered (the round has no Surface tools): ${mcp_err}"
        fi
        if [ "$(jq -r '(.mcpServers // {}) | length' "${mcp_file}" 2>/dev/null)" != "0" ]; then
            mcp_args=(--mcp-config "${mcp_file}")
        else
            rm -f "${mcp_file}" 2>/dev/null
        fi
    fi

    # The model: the Host env pin, else the Gate verdict's switch (model policy).
    local effective_model="${AUTO_AGENT_FIRE_MODEL:-}"
    [ -n "${effective_model}" ] || effective_model="$(printf '%s' "${gate}" | jq -r '.fireModel // empty')"
    local -a model_args=()
    [ -n "${effective_model}" ] && model_args=(--model "${effective_model}")

    local rc
    ( cd "${target}" && "${CLAUDE_BIN:-claude}" \
        --print \
        --permission-mode bypassPermissions \
        --plugin-dir "${FIRE_PLUGIN_DIR}" \
        --settings "${FIRE_SETTINGS_BASELINE}" \
        "${mcp_args[@]}" \
        --output-format stream-json \
        --verbose \
        "${model_args[@]}" \
        "${prompt}" < /dev/null 2> "${stderr}" ) \
      | rate_limits_tap "${state}" "${id}" > "${stream}"
    rc=${PIPESTATUS[0]}

    local outcome; outcome="$(_fire_outcome "${rc}" "${stream}" "${stderr}")"
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
        dry-run|resolve-dry-run)
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
            local slug reset; slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
            reset="$(printf '%s' "${outcome}" | jq -r '.resetAt // ""')"
            case "$(printf '%s' "${outcome}" | jq -r '.status')" in
                EXHAUSTED)
                    _fire_pause_inflight "${record}" "${slug}" "${target}" "${reset}" "usage exhausted"
                    local limit_type; limit_type="$(printf '%s' "${outcome}" | jq -r '.limitType // ""')"
                    echo "fire: outcome EXHAUSTED source=$(printf '%s' "${outcome}" | jq -r '.source // "none"') limit=${limit_type:-unknown} resetAt=${reset:-unknown}"
                    case "${limit_type}" in
                        ''|session|weekly|five_hour|seven_day)
                            echo "AGENT_RUN_RESET_AT=${reset}" ;;
                        *)
                            # A per-model limit stops nothing but that model: the
                            # usage sensor's next verdict switches the Fire model
                            # (story 20), so the Daemon re-gates instead of sleeping.
                            echo "AGENT_RUN_MODEL_LIMIT=${limit_type}"
                            echo "AGENT_RUN_RESET_AT=" ;;
                    esac
                    return 0 ;;
                AUTH_DEAD)
                    _fire_pause_inflight "${record}" "${slug}" "${target}" "" "credential dead"
                    park_enter "authentication_failed in Fire ${id}" || true
                    echo "fire: outcome AUTH_DEAD"
                    echo "AGENT_RUN_AUTH_DEAD=1" ;;
                FAILED)
                    _fire_clear_lock "${record}" "${slug}" ;;
            esac ;;
    esac
    return "${rc}"
}
