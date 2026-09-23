#!/usr/bin/env bash
# checklist.sh: the verification checklist protocol — what a round acts on, and
# the one mutation it is allowed to make to a PR body.
#
# Why this exists: the round reads a human's PR body and writes it back. Both
# halves are one-way doors — a parse that reads too much verifies boxes nobody
# asked for, and a tick that writes too much destroys a human's text or signs
# off work that was never done. So the two halves live here, as pure text
# transforms with a test suite, and the skill never hand-edits a body in prose.
#
# The protocol (carried over from the harness this came from, unchanged):
#   - only two sections hold verification items: `## Manual verification` and
#     `## Human verification required`. A `- [ ]` anywhere else (acceptance
#     criteria, a summary) is not a verification item and is never touched;
#   - only UNCHECKED items are acted on, so a re-run never re-verifies a box a
#     human already signed off;
#   - only items that PASSED are ticked; deferred and failed items keep their
#     empty box, and a ticked box is never un-ticked.
#
# Usage:
#   lib/checklist.sh parse [<body-file>]           # stdin when no file
#   lib/checklist.sh tick  <body-file> < passed-items
#
# `parse` prints one item per line as `<section><TAB><item text>`, where
# section is `manual` or `human`. Empty output means there is nothing to
# verify. `tick` reads the verbatim text of every passing item on stdin (one
# per line) and writes the new body to stdout; every other line of the body is
# emitted byte for byte.
#
# Exit codes:
#   0  printed
#   2  usage error / body file not found

set -uo pipefail

# _cl_heading_text <line> : the lower-cased heading text, or 1 when the line is
# not an ATX heading.
_cl_heading_text() {
    local line="$1"
    if [[ "${line}" =~ ^[[:space:]]*#+[[:space:]]+(.*)$ ]]; then
        printf '%s' "${BASH_REMATCH[1]}" \
            | tr '[:upper:]' '[:lower:]' \
            | sed -E 's/[[:space:]]+$//'
        return 0
    fi
    return 1
}

# _cl_section_tag <heading-text> : `manual`, `human`, or empty for a heading
# that does not open a verification section.
_cl_section_tag() {
    case "$1" in
        *"manual verification"*) printf 'manual' ;;
        *"human verification"*) printf 'human' ;;
        *) printf '' ;;
    esac
}

# checklist_parse < body : `<section><TAB><item>` for every unchecked item
checklist_parse() {
    local section='' line htext item
    while IFS= read -r line || [ -n "${line}" ]; do
        # A heading either opens a verification section or closes the current one.
        if htext="$(_cl_heading_text "${line}")"; then
            section="$(_cl_section_tag "${htext}")"
            continue
        fi
        [ -n "${section}" ] || continue
        if [[ "${line}" =~ ^[[:space:]]*[-*][[:space:]]+\[[[:space:]]\][[:space:]]+(.*)$ ]]; then
            item="$(printf '%s' "${BASH_REMATCH[1]}" | sed -E 's/[[:space:]]+$//')"
            [ -n "${item}" ] && printf '%s\t%s\n' "${section}" "${item}"
        fi
    done
}

# checklist_tick <body-file> < passed-items : the body with exactly the passing
# boxes flipped to `- [x]`
checklist_tick() {
    local body_file="${1:?checklist_tick: body-file required}"
    [ -f "${body_file}" ] || { echo "checklist: body file not found: ${body_file}" >&2; return 2; }

    declare -A passed=()
    local p
    while IFS= read -r p || [ -n "${p}" ]; do
        p="$(printf '%s' "${p}" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
        [ -n "${p}" ] && passed["${p}"]=1
    done

    local in_section=0 line htext trimmed prefix gap rest
    while IFS= read -r line || [ -n "${line}" ]; do
        if htext="$(_cl_heading_text "${line}")"; then
            if [ -n "$(_cl_section_tag "${htext}")" ]; then in_section=1; else in_section=0; fi
            printf '%s\n' "${line}"
            continue
        fi
        if [ "${in_section}" -eq 1 ] &&
            [[ "${line}" =~ ^([[:space:]]*[-*][[:space:]]+)\[[[:space:]]\]([[:space:]]+)(.*)$ ]]; then
            prefix="${BASH_REMATCH[1]}"; gap="${BASH_REMATCH[2]}"; rest="${BASH_REMATCH[3]}"
            trimmed="$(printf '%s' "${rest}" | sed -E 's/[[:space:]]+$//')"
            if [ -n "${passed[${trimmed}]:-}" ]; then
                printf '%s[x]%s%s\n' "${prefix}" "${gap}" "${rest}"
                continue
            fi
        fi
        printf '%s\n' "${line}"
    done < "${body_file}"
}

_cl_usage() { sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; }

checklist_main() {
    local sub="${1:-}"; shift || true
    case "${sub}" in
        parse)
            if [ -n "${1:-}" ] && [ "$1" != "-" ]; then
                [ -f "$1" ] || { echo "checklist: body file not found: $1" >&2; return 2; }
                checklist_parse < "$1"
            else
                checklist_parse
            fi ;;
        tick)
            [ -n "${1:-}" ] || { echo "checklist tick: a body file is required" >&2; return 2; }
            checklist_tick "$1" ;;
        -h|--help|help) _cl_usage ;;
        *) echo "checklist: unknown subcommand '${sub}'" >&2; _cl_usage >&2; return 2 ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    checklist_main "$@"
fi
