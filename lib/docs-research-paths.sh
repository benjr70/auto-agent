#!/usr/bin/env bash
# docs-research-paths.sh: the one definition of "this PR is research docs only".
#
# Two independent sensors must agree on it: lib/pr-triage.sh (which routes a
# PR to reason `docs-merge` from the `gh pr view --json files` list) and
# lib/docs-only-gate.sh (which re-checks the same rule against the real diff
# before the PR is merged). If the two ever drift, triage routes PRs the gate
# then refuses on every Fire, or docs PRs stop being routed at all. So the
# rule lives here and both source this file.
#
# The prefix is the Target Project's `docs_research_prefix` from the Harness
# config (ADR 0002; the schema default is `docs/research/`). Nothing here
# carries a path literal.
#
# Source this file, then:
#
#   docs_research_prefix <resolved-config-json>
#       Prints the prefix. Returns 1 when the config carries none.
#
#   docs_research_only <prefix> < <JSON array of paths>
#       Prints `true` when the array is non-empty AND every path starts with
#       the prefix, else `false`. Exit 0 either way; an unreadable array prints
#       `false` (a broken sensor must never auto-merge anything).

docs_research_prefix() {
    local cfg="${1:?docs_research_prefix: resolved config required}" prefix
    prefix="$(printf '%s' "${cfg}" | jq -r '.docs_research_prefix // empty' 2>/dev/null)" || return 1
    [ -n "${prefix}" ] || return 1
    printf '%s\n' "${prefix}"
}

docs_research_only() {
    local prefix="${1:?docs_research_only: prefix required}" verdict
    verdict="$(jq -r --arg p "${prefix}" '
        if type == "array" then ((length > 0) and all(.[]; type == "string" and startswith($p)))
        else false end' 2>/dev/null)" || verdict=false
    case "${verdict}" in
        true) echo true ;;
        *) echo false ;;
    esac
    return 0
}
