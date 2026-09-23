#!/usr/bin/env bash
# evidence.sh: the evidence sink of a verification round — where the round's
# artifacts live, what a tour screenshot is called, and how the tour lands in
# the PR description.
#
# Why this exists: the round produces files (screenshots, logs) and one body
# edit, and both are harness-owned. If naming were left to the verifier, every
# round would invent its own and the evidence comment could not cite anything;
# if the body edit were done in skill prose, a re-verified PR would grow a wall
# of stale images under the old ones. So the sink is this lib: one directory
# per round under the State dir (never inside a checkout, which the Daemon
# resets), one filename grammar, and an idempotent `## Screenshots` section
# that a later round REPLACES rather than appends to.
#
# Usage:
#   lib/evidence.sh dir    --pr <N> [--round <M>]
#   lib/evidence.sh name   <surface> <index> <slug>
#   lib/evidence.sh shots  <dir>
#   lib/evidence.sh inject <body-file> < caption-TAB-ref-lines
#
# `dir` creates and prints the round's artifact directory,
# `<state>/verify/pr-<N>/<UTC stamp>[-r<M>]/`. `name` prints the one legal
# screenshot filename, `<surface>-NN-<slug>.png`, refusing anything that would
# not sort into reading order. `shots` lists the tour files in a directory in
# reading order as `<caption><TAB><path>` lines, ready to pipe into `inject`.
#
# `inject` reads `<caption><TAB><ref>` lines and rewrites the body's
# `## Screenshots` section. A ref that is a URL is embedded as an image; a ref
# that is a path is listed as the path it is — a Host with no image uploader
# still gets a reviewer-readable tour that says where the pixels are. Empty
# input leaves the body byte-identical, so a round that captured nothing never
# destroys what an earlier round posted.
#
# Exit codes:
#   0  printed
#   1  a name that does not fit the grammar (`name`)
#   2  usage error / body file or directory not found
#
# Env:
#   AUTO_AGENT_STATE_DIR  the State dir (host-env.sh resolves the default)
#   EVIDENCE_NOW          the UTC stamp to use instead of `date` (tests)

set -uo pipefail

_evidence_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=host-env.sh
. "${_evidence_lib_dir}/host-env.sh"

EVIDENCE_MARKER='<!-- auto-agent-screenshots -->'

# evidence_dir <pr> [round] : create and print the round's artifact directory
evidence_dir() {
    local pr="${1:?evidence_dir: pr required}" round="${2:-}" stamp dir
    stamp="${EVIDENCE_NOW:-$(date -u +%Y%m%dT%H%M%SZ)}"
    dir="$(host_env_state_dir)/verify/pr-${pr}/${stamp}"
    [ -n "${round}" ] && dir="${dir}-r${round}"
    mkdir -p "${dir}" || return 2
    printf '%s\n' "${dir}"
}

# evidence_name <surface> <index> <slug> : the one legal screenshot filename.
# The index is zero-padded so a directory listing is the reading order, and the
# slug is what a reviewer reads as the caption, so it has to be a slug.
evidence_name() {
    local surface="${1:-}" index="${2:-}" slug="${3:-}"
    case "${surface}" in
        ''|*[!a-z0-9_-]*) echo "evidence name: surface '${surface}' is not a Surface name" >&2; return 1 ;;
    esac
    case "${index}" in
        ''|*[!0-9]*) echo "evidence name: index '${index}' is not a number" >&2; return 1 ;;
    esac
    case "${slug}" in
        ''|*[!a-z0-9-]*) echo "evidence name: slug '${slug}' is not a lower-case slug" >&2; return 1 ;;
    esac
    printf '%s-%02d-%s.png\n' "${surface}" "$((10#${index}))" "${slug}"
}

# evidence_shots <dir> : the tour files in reading order, `<caption><TAB><path>`
evidence_shots() {
    local dir="${1:?evidence_shots: dir required}" f base surface rest caption
    [ -d "${dir}" ] || { echo "evidence shots: no such directory: ${dir}" >&2; return 2; }
    while IFS= read -r f; do
        [ -n "${f}" ] || continue
        base="$(basename "${f}")"
        # `<surface>-NN-<slug>.png` reads back as `<surface> — <slug in words>`.
        if [[ "${base}" =~ ^([a-z0-9_-]+)-([0-9]+)-(.+)\.png$ ]]; then
            surface="${BASH_REMATCH[1]}"; rest="${BASH_REMATCH[3]}"
            caption="${surface} — ${rest//-/ }"
        else
            caption="${base%.png}"
        fi
        printf '%s\t%s\n' "${caption}" "${f}"
    done < <(find "${dir}" -maxdepth 1 -type f -name '*.png' | sort)
}

# _evidence_render <count> <captions...> <refs...> : the section text
_evidence_render() {
    local count="$1"; shift
    local -a captions=("${@:1:count}") refs=("${@:count+1:count}")
    printf '## Screenshots\n'
    printf '%s\n' "${EVIDENCE_MARKER}"
    printf '\n'
    printf '_Captured live by the verification round on the Host, one shot per screen the diff touches._\n'
    local i ref
    for (( i = 0; i < count; i++ )); do
        ref="${refs[i]}"
        printf '\n'
        printf '**%s**\n' "${captions[i]:-Screenshot $((i + 1))}"
        printf '\n'
        case "${ref}" in
            http://*|https://*) printf '![%s](%s)\n' "${captions[i]:-screenshot}" "${ref}" ;;
            *) printf '`%s` — on the Host, in this round'"'"'s evidence directory\n' "${ref}" ;;
        esac
    done
}

# _evidence_strip : drop a previously injected section (ours only — a hand
# written `## Screenshots` with no marker under it is left alone).
_evidence_strip() {
    awk -v marker="${EVIDENCE_MARKER}" '
        BEGIN { in_section = 0; pending = 0 }
        pending {
            pending = 0
            if ($0 == marker) { in_section = 1; next }
            print held
        }
        in_section {
            if ($0 ~ /^## / || $0 ~ /^---[[:space:]]*$/) { in_section = 0 }
            else { next }
        }
        /^## Screenshots[[:space:]]*$/ && !in_section { held = $0; pending = 1; next }
        { print }
        END { if (pending) print held }
    '
}

# _evidence_trim_trailing : no trailing blank lines
_evidence_trim_trailing() {
    awk '{ lines[NR] = $0 } END {
        last = NR
        while (last > 0 && lines[last] ~ /^[[:space:]]*$/) { last-- }
        for (i = 1; i <= last; i++) { print lines[i] }
    }'
}

# _evidence_insert <section> : body on stdin. The pixels go before the
# checklist they belong to, else before the trailing rule, else at the end.
_evidence_insert() {
    awk -v section="$1" '
        { lines[NR] = $0 }
        /^## Manual verification[[:space:]]*$/ || /^## Human verification required[[:space:]]*$/ {
            if (!checklist) { checklist = NR }
        }
        /^---[[:space:]]*$/ { rule = NR }
        END {
            at = checklist ? checklist : (rule ? rule : 0)
            if (at == 0) {
                for (i = 1; i <= NR; i++) { print lines[i] }
                print ""
                print section
            } else {
                for (i = 1; i < at; i++) {
                    if (i == at - 1 && lines[i] ~ /^[[:space:]]*$/) { continue }
                    print lines[i]
                }
                print ""
                print section
                print ""
                for (i = at; i <= NR; i++) { print lines[i] }
            }
        }
    ' | _evidence_trim_trailing
}

# evidence_inject <body-file> < caption-TAB-ref : the new body on stdout
evidence_inject() {
    local body_file="${1:?evidence_inject: body-file required}"
    [ -f "${body_file}" ] || { echo "evidence inject: body file not found: ${body_file}" >&2; return 2; }

    local -a captions=() refs=()
    local caption ref
    while IFS=$'\t' read -r caption ref || [ -n "${caption:-}" ]; do
        [ -n "${ref:-}" ] || continue
        captions+=("${caption}")
        refs+=("${ref}")
    done

    if [ "${#refs[@]}" -eq 0 ]; then
        cat "${body_file}"
        return 0
    fi

    local section stripped
    section="$(_evidence_render "${#refs[@]}" "${captions[@]}" "${refs[@]}")"
    stripped="$(_evidence_strip < "${body_file}" | cat -s)"
    printf '%s\n' "${stripped}" | _evidence_insert "${section}"
}

_evidence_usage() { sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; }

evidence_main() {
    local sub="${1:-}"; shift || true
    case "${sub}" in
        dir)
            local pr='' round=''
            while [ $# -gt 0 ]; do
                case "$1" in
                    --pr) pr="${2:-}"; shift 2 ;;
                    --pr=*) pr="${1#--pr=}"; shift ;;
                    --round) round="${2:-}"; shift 2 ;;
                    --round=*) round="${1#--round=}"; shift ;;
                    *) echo "evidence dir: unexpected argument '$1'" >&2; return 2 ;;
                esac
            done
            case "${pr}" in ''|*[!0-9]*) echo "evidence dir: --pr <N> is required" >&2; return 2 ;; esac
            if [ -n "${round}" ]; then
                case "${round}" in *[!0-9]*) echo "evidence dir: --round needs a number" >&2; return 2 ;; esac
            fi
            evidence_dir "${pr}" "${round}" ;;
        name) evidence_name "${1:-}" "${2:-}" "${3:-}" ;;
        shots) [ -n "${1:-}" ] || { echo "evidence shots: a directory is required" >&2; return 2; }
               evidence_shots "$1" ;;
        inject) [ -n "${1:-}" ] || { echo "evidence inject: a body file is required" >&2; return 2; }
                evidence_inject "$1" ;;
        -h|--help|help) _evidence_usage ;;
        *) echo "evidence: unknown subcommand '${sub}'" >&2; _evidence_usage >&2; return 2 ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    evidence_main "$@"
fi
