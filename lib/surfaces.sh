#!/usr/bin/env bash
# surfaces.sh: what a verification round may drive, read from the Harness
# config's `surfaces` block (ADR 0003, Spec #23 Surface).
#
# Why this exists: a round has to answer three questions before it boots
# anything — which Surfaces this PR touched, which of those earn a screenshot
# tour, and at what viewport each tour is captured. In the harness this came
# from, all three were app-specific scripts: one hard-coded two source roots,
# another two viewports. Here they are one lib over the declaration — the
# Target Project says which paths mark a Surface touched and what shape its
# users see, and the harness owns the judgement.
#
# The rule the harness keeps for itself (ADR 0003): `browser` and `electron`
# Surfaces ALWAYS earn a tour when touched; `cli` and `api` Surfaces are
# evidence-only. A project cannot opt out of screenshots by declaration.
#
# Usage:
#   lib/surfaces.sh list     [<target-dir>]
#   lib/surfaces.sh touched  [--pr <N>] [<target-dir>] [< changed-paths]
#   lib/surfaces.sh tour     [--pr <N>] [<target-dir>] [< changed-paths]
#   lib/surfaces.sh viewport <name> [<target-dir>]
#
# Every subcommand takes `--head`, which reads the Harness config from the
# checkout rather than from an inherited HARNESS_CONFIG_JSON: a verification
# round obeys the config the PR head carries (ADR 0007).
#
# `list` prints every declared Surface as
# `<name>  <kind>  <url_key>  <viewport>  <launcher>` (tab-separated, the
# resolved viewport, and `-` for an undeclared launcher). `touched` reads
# repo-relative changed paths on stdin — or fetches them with
# `gh pr diff --name-only` when `--pr` is given — and prints the names of the
# Surfaces whose globs match, one per line, sorted. `tour` is `touched`
# narrowed to the kinds that earn a tour. `viewport` prints the capture shape
# of one Surface.
#
# Path globs are matched as globs, not regexes: `*` and `?` stop at `/`, `**`
# crosses directories, and a pattern ending in `/` matches everything beneath
# it. A leading `./` on a changed path is ignored, so `git diff` and
# `gh pr diff` styles both work.
#
# Exit codes:
#   0  printed (empty output is a legal answer: nothing matched)
#   1  no such Surface (`viewport`)
#   2  usage error, or no Harness config could be resolved
#
# Env:
#   SURFACES_DEFAULT_VIEWPORT  the viewport for a Surface that declares none
#                              (default 1280x800). A Target Project whose users
#                              see a phone or a fixed panel declares the real
#                              shape per Surface; the default only keeps a tour
#                              from having no shape at all.
#   GH_BIN                     the gh CLI (default gh), for `--pr`

set -uo pipefail

_surfaces_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_surfaces_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_surfaces_lib_dir}/host-env.sh"

SURFACES_DEFAULT_VIEWPORT="${SURFACES_DEFAULT_VIEWPORT:-1280x800}"

# The kinds that always earn a screenshot tour when touched (ADR 0003).
SURFACES_TOUR_KINDS='browser electron'

# The kinds a launcher exists for, and the one kind that is an app the round
# starts itself. Every lib that asks "what can I do with this Surface" asks
# here; nothing else re-lists the kinds.
SURFACES_LAUNCHER_KINDS='browser electron'
SURFACES_APP_KIND='electron'

# surfaces_tour_kind <kind> : 0 when a touched Surface of this kind earns a tour
surfaces_tour_kind() {
    case " ${SURFACES_TOUR_KINDS} " in
        *" ${1:-} "*) return 0 ;;
        *) return 1 ;;
    esac
}

# surfaces_launcher_kind <kind> : 0 when this kind is driven through a launcher
# (a `cli` or `api` Surface is evidence-only and has none)
surfaces_launcher_kind() {
    case " ${SURFACES_LAUNCHER_KINDS} " in
        *" ${1:-} "*) return 0 ;;
        *) return 1 ;;
    esac
}

# surfaces_app_kind <kind> : 0 when this kind is an app the round launches
surfaces_app_kind() {
    [ "${1:-}" = "${SURFACES_APP_KIND}" ]
}

# _surfaces_glob_re <glob> : the glob as an anchored extended regular
# expression. Every regex metacharacter in the pattern is escaped, so a
# declaration like `app/v1.2/**` matches the directory it names and not
# `app/v1X2/`.
_surfaces_glob_re() {
    local glob="$1" out='' i ch
    # A pattern ending in `/` means "everything beneath this directory".
    case "${glob}" in */) glob="${glob}**" ;; esac
    for (( i = 0; i < ${#glob}; i++ )); do
        ch="${glob:i:1}"
        case "${ch}" in
            '*')
                if [ "${glob:i:2}" = '**' ]; then
                    out+='.*'; i=$((i + 1))
                else
                    out+='[^/]*'
                fi ;;
            '?') out+='[^/]' ;;
            [A-Za-z0-9_/-]) out+="${ch}" ;;
            *) out+="\\${ch}" ;;
        esac
    done
    printf '^%s$\n' "${out}"
}

# surfaces_match <path> <glob>... : 0 when the path matches any of the globs
surfaces_match() {
    local path="$1"; shift
    path="${path#./}"
    local glob re
    for glob in "$@"; do
        [ -n "${glob}" ] || continue
        re="$(_surfaces_glob_re "${glob}")"
        printf '%s' "${path}" | grep -Eq -- "${re}" && return 0
    done
    return 1
}

# surfaces_viewport <cfg> <name> : the declared viewport, else the default.
# Returns 1 when the Surface is not declared at all.
surfaces_viewport() {
    local cfg="${1:?surfaces_viewport: config required}" name="${2:?surfaces_viewport: name required}" v
    printf '%s' "${cfg}" | jq -e --arg n "${name}" '(.surfaces // {}) | has($n)' >/dev/null 2>&1 || return 1
    v="$(printf '%s' "${cfg}" | jq -r --arg n "${name}" '.surfaces[$n].viewport // ""')"
    [ -n "${v}" ] || v="${SURFACES_DEFAULT_VIEWPORT}"
    printf '%s\n' "${v}"
}

# surfaces_kind <cfg> <name> : the declared kind. Returns 1 when undeclared.
surfaces_kind() {
    local cfg="${1:?surfaces_kind: config required}" name="${2:?surfaces_kind: name required}" k
    k="$(printf '%s' "${cfg}" | jq -r --arg n "${name}" '(.surfaces // {})[$n].kind // ""')"
    [ -n "${k}" ] || return 1
    printf '%s\n' "${k}"
}

# surfaces_launcher <cfg> <name> : the declared launcher, empty when there is
# none. Returns 1 when the Surface is not declared at all.
surfaces_launcher() {
    local cfg="${1:?surfaces_launcher: config required}" name="${2:?surfaces_launcher: name required}"
    printf '%s' "${cfg}" | jq -e --arg n "${name}" '(.surfaces // {}) | has($n)' >/dev/null 2>&1 || return 1
    printf '%s' "${cfg}" | jq -r --arg n "${name}" '.surfaces[$n].launcher // ""'
}

# surfaces_url_key <cfg> <name> : the key the provider's block must carry for
# this Surface. Returns 1 when the Surface is not declared at all.
surfaces_url_key() {
    local cfg="${1:?surfaces_url_key: config required}" name="${2:?surfaces_url_key: name required}" k
    k="$(printf '%s' "${cfg}" | jq -r --arg n "${name}" '(.surfaces // {})[$n].url_key // ""')"
    [ -n "${k}" ] || return 1
    printf '%s\n' "${k}"
}

# surfaces_list <cfg> : one line per declared Surface, sorted by name
surfaces_list() {
    # Filled in jq, not in a read loop: a tab is IFS whitespace, so an empty
    # viewport field would collapse into the next one and shift the row.
    local cfg="${1:?surfaces_list: config required}"
    printf '%s' "${cfg}" | jq -r --arg d "${SURFACES_DEFAULT_VIEWPORT}" '
        (.surfaces // {}) | to_entries | sort_by(.key)[]
        | [.key, .value.kind, .value.url_key, (.value.viewport // $d), (.value.launcher // "-")] | @tsv'
}

# surfaces_touched <cfg> [--tour-only] < changed-paths : the Surfaces the diff
# touched, one name per line, sorted. With --tour-only, narrowed to the kinds
# that always earn a tour.
surfaces_touched() {
    local cfg="${1:?surfaces_touched: config required}" tour_only=0
    [ "${2:-}" = "--tour-only" ] && tour_only=1

    local -a paths=()
    local line
    while IFS= read -r line; do
        [ -n "${line}" ] || continue
        paths+=("${line}")
    done
    [ "${#paths[@]}" -gt 0 ] || return 0

    local name kind globs_json p
    local -a globs=() hits=()
    while IFS=$'\t' read -r name kind globs_json; do
        [ -n "${name}" ] || continue
        if [ "${tour_only}" -eq 1 ] && ! surfaces_tour_kind "${kind}"; then
            continue
        fi
        globs=()
        while IFS= read -r line; do
            [ -n "${line}" ] && globs+=("${line}")
        done < <(printf '%s' "${globs_json}" | jq -r '.[]' 2>/dev/null)
        [ "${#globs[@]}" -gt 0 ] || continue
        for p in "${paths[@]}"; do
            if surfaces_match "${p}" "${globs[@]}"; then
                hits+=("${name}")
                break
            fi
        done
    done < <(printf '%s' "${cfg}" | jq -r '(.surfaces // {}) | to_entries[]
        | [.key, .value.kind, (.value.paths | tostring)] | @tsv')

    [ "${#hits[@]}" -gt 0 ] || return 0
    printf '%s\n' "${hits[@]}" | sort -u
}

_surfaces_usage() { sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; }

surfaces_main() {
    local sub="${1:-}"; shift || true
    case "${sub}" in
        -h|--help|help) _surfaces_usage; return 0 ;;
        '') _surfaces_usage >&2; return 2 ;;
    esac

    local pr='' target_arg='' name='' head=0
    if [ "${sub}" = "viewport" ]; then
        name="${1:-}"; shift || true
        if [ -z "${name}" ]; then echo "surfaces viewport: a Surface name is required" >&2; return 2; fi
    fi
    while [ $# -gt 0 ]; do
        case "$1" in
            --pr) [ $# -ge 2 ] || { echo "surfaces: --pr needs a number" >&2; return 2; }
                  pr="$2"; shift 2 ;;
            --pr=*) pr="${1#--pr=}"; shift ;;
            --head) head=1; shift ;;
            -*) echo "surfaces: unknown option '$1'" >&2; return 2 ;;
            *) target_arg="$1"; shift ;;
        esac
    done
    if [ -n "${pr}" ]; then
        case "${pr}" in *[!0-9]*) echo "surfaces: --pr needs a number" >&2; return 2 ;; esac
    fi

    local cfg
    if [ "${head}" -eq 1 ]; then
        cfg="$(harness_config_resolve_head "${target_arg}")" || return 2
    else
        cfg="$(harness_config_resolve "${target_arg}")" || return 2
    fi

    case "${sub}" in
        list) surfaces_list "${cfg}" ;;
        touched) harness_changed_paths "${pr}" | surfaces_touched "${cfg}" ;;
        tour) harness_changed_paths "${pr}" | surfaces_touched "${cfg}" --tour-only ;;
        viewport)
            surfaces_viewport "${cfg}" "${name}" || {
                echo "surfaces viewport: no Surface '${name}' is declared in the Harness config" >&2
                return 1
            } ;;
        *) echo "surfaces: unknown subcommand '${sub}'" >&2; _surfaces_usage >&2; return 2 ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    surfaces_main "$@"
fi
