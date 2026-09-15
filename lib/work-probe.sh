#!/usr/bin/env bash
# work-probe.sh: the Work Probe, "did work appear mid-window?" for zero Claude
# cost. A Fire that found an empty queue would otherwise sleep until the window
# reset, blind to work arriving on human time. The probe lets the Daemon sleep
# in chunks and peek between them using only `gh`. Carried over from
# the Smart Smoker harness; the repo now comes from the Harness config (ADR 0002) and the
# machine login from the Host env (ADR 0005).
#
# Two functions:
#
#   wp_scan [<target-dir>]
#                       one `gh` sweep of the Target Project's work signals;
#                       emits
#                         { "locked":    <bool>,        # AFK:in-progress held
#                           "reconcile": <pr# | null>,  # pr-triage's verdict
#                           "paused":    <issue# | null>,
#                           "pickSig":   "<csv of candidate issue numbers>",
#                           "prSig":     "<csv of open PR numbers>" | null,
#                           "slices":    <n eligible implementation Slices>,
#                           "wayfinder": <n eligible wayfinder:* tickets>,
#                           "openMaps":  <n open wayfinder:map issues> }
#                       The last three are read-only signals for the
#                       Dashboard; wp_decide ignores them. prSig is derived
#                       from the same `pr list` fetch that feeds the reconcile
#                       triage. A failed or malformed fetch emits prSig=null
#                       (set UNKNOWN), never "" (a readable EMPTY set).
#
#   wp_decide <baseline-pickSig> <baseline-prSig>
#                       pure: reads a scan JSON on stdin, prints a one-line
#                       wake reason and exits 0, or exits 1 (keep sleeping).
#
# Wake rules (the pickup Fire's priority order):
#   - lock held: never wake, every Fire would skip. The lock read fails SAFE;
#     a gh error reads as "locked" so a flake can never start a wake-fire-skip
#     loop against a genuinely held lock.
#   - reconcile candidate: wake unconditionally. The pickup Fire runs the very
#     same pr-triage over the same inputs, so it WILL act on it.
#   - AFK:paused issue: wake unconditionally. The Fire always acts (resume, or
#     cap -> AFK:failed; either way the signal clears itself).
#   - open-PR set shrink: wake when a PR present in the baseline is absent from
#     the current scan (merged OR closed). A blocker PR's merge changes no
#     issue-side signal but unblocks the queue. Set GROWTH never wakes here.
#     Fails SAFE: a null (unreadable) current prSig is never read as a shrink.
#   - pick-class candidates (open `AFK` issues with no state label): wake ONLY
#     when the signature differs from the baseline captured when the Fire
#     reported no work. The probe cannot cheaply check Project membership or
#     blocker closure, so an issue the Fire already declined must not re-wake
#     the Daemon every chunk; a genuinely new issue changes the signature and
#     wakes once.
#
# The reconcile signal is lib/pr-triage.sh's seam: sourced when the file
# exists beside this one, its `pr_triage_enrich | pr_triage_pick` classify the
# fetched PR list and its `pr_triage_bot_verdict_unworkable` suppresses a Bot
# PR verdict when the deps-land lane is off in the Harness config. Without
# the lib, reconcile is always null and prSig still carries the shrink signal.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        <target-dir>, or AUTO_AGENT_TARGET_DIR): repo slug
#   DAEMON_GH_LOGIN      the machine login from the Host env, for pr-triage's
#                        ours-filter; else WP_AUTHOR; else `gh api user`
#   GH_BIN               gh CLI (default: gh), injected for tests

_work_probe_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_work_probe_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_work_probe_lib_dir}/host-env.sh"
# shellcheck source=single-flight-lock.sh
. "${_work_probe_lib_dir}/single-flight-lock.sh"
if [ -f "${_work_probe_lib_dir}/pr-triage.sh" ]; then
    # shellcheck source=/dev/null
    . "${_work_probe_lib_dir}/pr-triage.sh"
fi

# _wp_reconcile <author> : reads the PR list JSON on stdin, prints the PR
# number pr-triage picks or "null". Owns the seam described in the header.
_wp_reconcile() {
    local author="$1" pick_json reconcile
    if ! declare -F pr_triage_enrich >/dev/null 2>&1 || ! declare -F pr_triage_pick >/dev/null 2>&1; then
        cat >/dev/null
        echo null
        return 0
    fi
    pick_json="$(PR_TRIAGE_AUTHOR="${author}" pr_triage_enrich \
        | PR_TRIAGE_AUTHOR="${author}" pr_triage_pick)" || true
    if ! printf '%s' "${pick_json}" | jq -e 'type == "object"' >/dev/null 2>&1; then
        echo null
        return 0
    fi
    reconcile="$(printf '%s' "${pick_json}" | jq -r '.pr // "null"' 2>/dev/null || echo 'null')"
    # The same suppression predicate pickup-triage.sh asks: if the two ever
    # disagreed the probe would wake the Daemon every chunk for a PR the Fire
    # then skips.
    if declare -F pr_triage_bot_verdict_unworkable >/dev/null 2>&1 \
        && pr_triage_bot_verdict_unworkable "${pick_json}"; then
        reconcile='null'
    fi
    printf '%s\n' "${reconcile}"
}

# wp_scan [<target-dir>]: sweep the work signals, print the scan JSON. Exits 0
# with valid JSON whenever the config resolves; individual gh failures degrade
# field-by-field (lock -> locked, everything else -> "nothing there"), never
# crash the Daemon's sleep loop. An unresolvable config is the one hard error
# (exit 2, no JSON): the Daemon cannot probe a repo it cannot name.
wp_scan() {
    local gh="${GH_BIN:-gh}" author locked_raw locked prs reconcile paused pick_sig
    local pr_sig pr_sig_arg

    host_env_load
    local cfg slug
    cfg="$(harness_config_resolve "${1:-}")" || return 2
    export HARNESS_CONFIG_JSON="${cfg}"
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"

    author="${DAEMON_GH_LOGIN:-${WP_AUTHOR:-$("${gh}" api user -q .login 2>/dev/null || echo '')}}"

    locked_raw="$(single_flight_inflight "${slug}")"
    if [ "${locked_raw}" = "0" ]; then
        locked=false
    else
        locked=true
    fi

    # One `pr list` fetch feeds BOTH the reconcile triage and the open-PR
    # signature. A failed/malformed fetch fails SAFE: the set is UNKNOWN
    # (prSig -> JSON null), never mistaken for an empty set. The field list is
    # pr-triage's: a listing missing headRefOid/reviewDecision cannot support
    # its Dependabot verdict.
    prs="$("${gh}" pr list --repo "${slug}" --state open \
        --json number,headRefName,isDraft,mergeable,labels,createdAt,author,headRefOid,reviewDecision \
        2>/dev/null)" || prs=''
    if [ -n "${prs}" ] && printf '%s' "${prs}" | jq -e 'type == "array"' >/dev/null 2>&1; then
        pr_sig="$(printf '%s' "${prs}" | jq -r '[.[].number] | sort | map(tostring) | join(",")')"
        pr_sig_arg="$(jq -cn --arg s "${pr_sig}" '$s')"
    else
        prs='[]'          # the reconcile triage still needs a valid empty array
        pr_sig_arg='null' # set unreadable: the signature is null, not ""
    fi

    reconcile="$(printf '%s' "${prs}" | _wp_reconcile "${author}")"

    paused="$("${gh}" issue list --repo "${slug}" --label "${HARNESS_LABEL_PAUSED}" --state open \
        --json number --jq '(sort_by(.number) | first | .number) // "null"' \
        2>/dev/null || echo 'null')"
    case "${paused}" in ''|*[!0-9]*) paused='null' ;; esac
    case "${reconcile}" in ''|*[!0-9]*) reconcile='null' ;; esac

    # One raw fetch of the AFK queue feeds BOTH the pick signature and the kind
    # split (Slices vs wayfinder tickets). A failed/malformed fetch reads as an
    # empty queue.
    local queue eligible slices wayfinder open_maps
    queue="$("${gh}" issue list --repo "${slug}" --label "${HARNESS_LABEL_AFK}" --state open --json number,labels \
        2>/dev/null)" || queue=''
    if [ -z "${queue}" ] || ! printf '%s' "${queue}" | jq -e 'type == "array"' >/dev/null 2>&1; then
        queue='[]'
    fi
    eligible="$(printf '%s' "${queue}" | jq -c --argjson state "${HARNESS_LABELS_STATE_JSON}" '
        [ .[] | . as $i | [$i.labels[].name] as $l
          | select((($l - $state) | length) == ($l | length))
          | {number: $i.number, labels: $l} ]' 2>/dev/null || echo '[]')"
    pick_sig="$(printf '%s' "${eligible}" \
        | jq -r '[.[].number] | sort | map(tostring) | join(",")' 2>/dev/null || echo '')"
    # Wayfinder tickets carry a `wayfinder:<type>` label and go to the resolve
    # lane; everything else in the queue is an implementation Slice.
    wayfinder="$(printf '%s' "${eligible}" \
        | jq --arg p "${HARNESS_LABEL_WAYFINDER_PREFIX}" '[.[] | select(.labels | any(startswith($p)))] | length' \
        2>/dev/null || echo 0)"
    slices="$(printf '%s' "${eligible}" \
        | jq --arg p "${HARNESS_LABEL_WAYFINDER_PREFIX}" '[.[] | select(.labels | any(startswith($p)) | not)] | length' \
        2>/dev/null || echo 0)"

    # --limit is explicit: gh defaults to 30, which would silently under-count
    # the Dashboard's map total once the repo passes 30 open maps.
    open_maps="$("${gh}" issue list --repo "${slug}" --label "${HARNESS_LABEL_MAP}" --state open --limit 200 \
        --json number --jq 'length' 2>/dev/null || echo 0)"
    case "${open_maps}" in
        ''|*[!0-9]*) open_maps=0 ;;
    esac

    jq -cn \
        --argjson locked "${locked}" \
        --argjson reconcile "${reconcile}" \
        --argjson paused "${paused}" \
        --arg pickSig "${pick_sig}" \
        --argjson prSig "${pr_sig_arg}" \
        --argjson slices "${slices:-0}" \
        --argjson wayfinder "${wayfinder:-0}" \
        --argjson openMaps "${open_maps}" \
        '{locked: $locked, reconcile: $reconcile, paused: $paused, pickSig: $pickSig,
          prSig: $prSig, slices: $slices, wayfinder: $wayfinder, openMaps: $openMaps}'
}

# wp_decide: read a scan JSON on stdin; wake (print reason, exit 0) or keep
# sleeping (exit 1). $1 is the baseline pickSig and $2 the baseline prSig, both
# captured at no-work time. Anything malformed keeps sleeping: a broken sensor
# must never wake-loop.
wp_decide() {
    local baseline="${1:-}" pr_baseline="${2:-}" scan locked reconcile paused pick_sig
    local pr_readable pr_sig gone
    scan="$(cat)"

    printf '%s' "${scan}" | jq -e 'type == "object"' >/dev/null 2>&1 || return 1

    # Anything other than an explicit false counts as locked (fail safe).
    locked="$(printf '%s' "${scan}" | jq -r '.locked' 2>/dev/null || echo 'true')"
    if [ "${locked}" != "false" ]; then
        return 1
    fi

    reconcile="$(printf '%s' "${scan}" | jq -r '.reconcile // "null"')"
    if [ "${reconcile}" != "null" ]; then
        printf 'reconcile PR #%s\n' "${reconcile}"
        return 0
    fi

    paused="$(printf '%s' "${scan}" | jq -r '.paused // "null"')"
    if [ "${paused}" != "null" ]; then
        printf 'resume issue #%s\n' "${paused}"
        return 0
    fi

    # Shrink-wake: only when the current set is READABLE (prSig not null); a
    # `gh pr list` flake scans as null and must never look like a whole-set
    # shrink. Growth is ignored here.
    if [ -n "${pr_baseline}" ]; then
        pr_readable="$(printf '%s' "${scan}" | jq -r '.prSig != null' 2>/dev/null || echo 'false')"
        if [ "${pr_readable}" = "true" ]; then
            pr_sig="$(printf '%s' "${scan}" | jq -r '.prSig')"
            gone="$(jq -rn --arg b "${pr_baseline}" --arg c "${pr_sig}" \
                'def toset($s): ($s | if length > 0 then split(",") else [] end);
                 (toset($b) - toset($c)) | join(",")')"
            if [ -n "${gone}" ]; then
                printf 'PR(s) left the open set #%s\n' "${gone}"
                return 0
            fi
        fi
    fi

    pick_sig="$(printf '%s' "${scan}" | jq -r '.pickSig // ""')"
    if [ -n "${pick_sig}" ] && [ "${pick_sig}" != "${baseline}" ]; then
        printf 'new pick candidate(s) #%s\n' "${pick_sig}"
        return 0
    fi

    return 1
}
