#!/usr/bin/env bash
# bootstrap-state.sh: the Bootstrap state — a Target Project whose Harness
# config declares no hermetic tier yet (ADR 0007, Spec #23 Bootstrap state).
#
# Why this exists: "no Environment provider yet" is not an error and not a
# mode to configure. It is one fact — `verification.hermetic` is absent — that
# four places have to read the same way: the Fire record (so the Dashboard can
# warn), the pickup Fire (which opens the one AFK ticket that asks the Daemon
# to write the provider), the verification round (which labels the PR
# `AFK:verify-human` instead of booting nothing), and the review round (which
# flags any Agent PR that edits `.auto-agent/`, because under ADR 0007 a PR can
# change its own verification). Each of those used to be a sentence in a
# prompt. Here the fact, the issue's identity and the config-diff question are
# one lib, so a skill asks instead of deciding.
#
# The state ends the ordinary way: the bootstrap ticket is an ordinary AFK
# ticket, the Daemon picks it, and the PR that adds the provider closes it —
# verified by the provider it adds (ADR 0007), which is the evidence that
# closes the state. Nothing here closes anything.
#
# Usage:
#   lib/bootstrap-state.sh state          [--head] [<target-dir>]
#   lib/bootstrap-state.sh issue          [--dry-run] [--head] [<target-dir>]
#   lib/bootstrap-state.sh config-touched [--pr <N>] [--head] [<target-dir>] [< changed-paths]
#
# Every subcommand takes `--head`, which reads the Harness config from the
# checkout rather than from an inherited HARNESS_CONFIG_JSON: a round that
# stands in a PR head obeys the config that PR carries (ADR 0007).
#
# `state` prints one compact JSON verdict and always exits 0 — branch on
# `.bootstrap`, never on the exit code:
#
#   { "bootstrap": <bool>, "reason": "<why>", "configDir": "<abs path>",
#     "issue": <N> | null }
#
# `.issue` is the open bootstrap ticket when one is already there, and null
# when none was found or gh could not be asked; it is informational (finding
# it costs one `gh issue list`, skipped entirely outside the state).
#
# `issue` ensures the bootstrap ticket exists, once. It is idempotent by the
# body marker, never by title, so a human may retitle or rewrite it and the
# next Fire still finds it. A fresh ticket is put on the Target Project's pick
# signal through `pick-publish` (a no-op under a label-only pick), so the
# Daemon can actually pick what it was just asked to do. Prints one line:
#
#   bootstrap: issue #<N> created | reused
#   bootstrap: would-open the bootstrap issue        (--dry-run, none open yet)
#   bootstrap: would-reuse issue #<N>                (--dry-run, one is open)
#
# `config-touched` answers the ADR 0007 review question: which paths under the
# Harness config directory does this diff change? It reads repo-relative paths
# on stdin, or fetches them with `gh pr diff --name-only` when `--pr` is given,
# and prints the matching paths, one per line, sorted. Empty output is the
# answer "none", not a failure.
#
# Exit codes:
#   0  printed (empty output is a legal answer for config-touched)
#   1  gh failed: the ticket could not be opened or the diff could not be read
#   2  usage error, or no Harness config could be resolved
#   3  (`issue`) the Target Project is NOT in the Bootstrap state, so no
#      bootstrap ticket is owed. Nothing was read and nothing was written —
#      this is the cheap answer a caller outside the state gets
#
# Env:
#   GH_BIN                    the gh CLI (default gh)
#   BOOTSTRAP_PICK_PUBLISH    the pick-publish CLI (default: beside this lib)

set -uo pipefail

_bootstrap_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_bootstrap_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_bootstrap_lib_dir}/host-env.sh"

# The one identity of the bootstrap ticket. Matched in the body, so the title
# is a human's to change.
BOOTSTRAP_MARKER="<!-- auto-agent:bootstrap -->"
BOOTSTRAP_TITLE="Write the Environment provider for the hermetic verification tier"

_bs_err() { echo "bootstrap: $*" >&2; }

# bootstrap_is_state <cfg> : 0 when the config declares no hermetic tier
bootstrap_is_state() {
    ! printf '%s' "${1:?bootstrap_is_state: config required}" \
        | jq -e '.verification.hermetic != null' >/dev/null 2>&1
}

# bootstrap_find_issue <cfg> : the open bootstrap ticket's number, or ''.
# Matched by the marker alone; the lowest number wins so two of them (a human
# opened one by hand) can never flip between Fires.
bootstrap_find_issue() {
    local cfg="${1:?bootstrap_find_issue: config required}" slug
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug // empty')"
    [ -n "${slug}" ] || return 0
    "${GH_BIN:-gh}" issue list --repo "${slug}" --label "${HARNESS_LABEL_AFK}" --state open \
        --json number,body \
        --jq "[.[] | select(.body | contains(\"${BOOTSTRAP_MARKER}\"))] | sort_by(.number) | .[0].number // empty" \
        2>/dev/null || true
}

# bootstrap_state <cfg> : the verdict JSON (always exit 0)
bootstrap_state() {
    local cfg="${1:?bootstrap_state: config required}" boot=false reason issue=''
    if bootstrap_is_state "${cfg}"; then
        boot=true
        reason="the Harness config declares no hermetic tier: no Environment provider yet"
        issue="$(bootstrap_find_issue "${cfg}")"
    else
        reason="the Harness config declares a hermetic tier"
    fi
    jq -cn --argjson b "${boot}" --arg r "${reason}" \
        --arg d "$(printf '%s' "${cfg}" | jq -r '.config_dir // ""')" \
        --arg n "${issue}" \
        '{bootstrap: $b, reason: $r, configDir: $d,
          issue: (if $n == "" then null else ($n | tonumber) end)}'
}

# _bs_issue_body <cfg> : the ticket the Daemon reads. It is a wayfinder-format
# Slice, because that is what the implementer knows how to work.
_bs_issue_body() {
    local cfg="$1" surfaces
    surfaces="$(printf '%s' "${cfg}" | jq -r '
        (.surfaces // {}) | to_entries
        | if length == 0 then "The config declares no Surface yet: declare the ones this project has."
          else map("- `\(.key)` (\(.value.kind)): its url_key is `\(.value.url_key)`") | join("\n") end')"
    cat <<BODY
${BOOTSTRAP_MARKER}

## What to build

This Target Project is in the **Bootstrap state**: its Harness config declares
no \`verification.hermetic\` block, so every Agent PR the Daemon opens is
labelled \`AFK:verify-human\` and waits for a human to verify it by hand. Write
the Environment provider that ends that.

Read the contract first — it is the whole specification, and the harness
ships two reference implementations beside it:

- \`plugin/providers/CONTRACT.md\` in the Harness install: \`up --pr N\`,
  \`down --pr N\` and the optional \`smoke\`, the \`KEY=value\` block \`up\`
  prints on stdout, and the exit codes each subcommand owes.
- \`plugin/providers/compose/provider\` — the reference for a project that
  boots with \`docker compose\`; \`plugin/providers/provider-lib.sh\` is the
  shared helper both references source.

The provider is one executable in THIS repository (the references are a
starting point to copy and adapt, never a file to depend on at runtime), plus
the \`verification.hermetic\` block in \`.auto-agent/harness.json\` that names
it and the \`surfaces\` its block must carry a URL for.

Surfaces this config declares today:

${surfaces}

## Acceptance criteria

- [ ] The provider is executable, committed in this repository, and named by
      \`verification.hermetic.command\` in \`.auto-agent/harness.json\`.
- [ ] \`bin/auto-agent provider-check\` against this checkout prints a
      conforming verdict (exit 0).
- [ ] \`up\` prints a \`KEY=value\` block carrying the \`url_key\` of every
      declared Surface, and \`down\` is idempotent.
- [ ] Two environments for different PR numbers can be up at once without
      colliding.

## Behaviors to test

1. \`up\` / \`down\` / \`up\` again leaves one healthy environment (the
   contract's own sequence).
2. \`provider-check\` conforms.

## Notes

The PR that adds this provider is verified by the provider it adds (ADR 0007:
the verification round reads the Harness config from the PR head), so its
first green round is the evidence that closes the Bootstrap state. That PR
touches \`.auto-agent/\`, so the review round flags it for a human — expected,
not a defect.
BODY
}

# bootstrap_issue_ensure <cfg> [--dry-run] : open or reuse the ticket
bootstrap_issue_ensure() {
    local cfg="${1:?bootstrap_issue_ensure: config required}" dry="${2:-}"
    local gh="${GH_BIN:-gh}" slug n url

    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug // empty')"
    if [ -z "${slug}" ]; then
        _bs_err "the Harness config names no repo slug"
        return 2
    fi

    n="$(bootstrap_find_issue "${cfg}")"
    if [ -n "${n}" ]; then
        if [ "${dry}" = "--dry-run" ]; then
            echo "bootstrap: would-reuse issue #${n}"
        else
            echo "bootstrap: issue #${n} reused"
        fi
        return 0
    fi
    if [ "${dry}" = "--dry-run" ]; then
        echo "bootstrap: would-open the bootstrap issue"
        return 0
    fi

    url="$("${gh}" issue create --repo "${slug}" --label "${HARNESS_LABEL_AFK}" \
        --title "${BOOTSTRAP_TITLE}" --body "$(_bs_issue_body "${cfg}")" 2>&1)" || {
        _bs_err "could not open the bootstrap issue in ${slug}: ${url}"
        return 1
    }
    n="$(printf '%s' "${url}" | grep -oE '[0-9]+$' | tail -1)"
    if [ -z "${n}" ]; then
        _bs_err "opened the bootstrap issue but could not read its number from: ${url}"
        return 1
    fi

    # The AFK label is not always the pick signal: under a Project pick a
    # ticket that is not on the board is silently never picked, so the one
    # ticket that asks for the provider would sit there for ever.
    local pp="${BOOTSTRAP_PICK_PUBLISH:-${_bootstrap_lib_dir}/pick-publish.sh}"
    if [ -x "${pp}" ] || [ -f "${pp}" ]; then
        HARNESS_CONFIG_JSON="${cfg}" bash "${pp}" publish --issue "${n}" >/dev/null 2>&1 \
            || _bs_err "issue #${n} is open but could not be put on the pick signal; publish it by hand"
    fi

    echo "bootstrap: issue #${n} created"
}

# bootstrap_config_touched <cfg> < changed-paths : the changed paths that are
# inside the Harness config directory, sorted. The directory comes from the
# resolved config (its basename, falling back to the fixed harness vocabulary
# in harness-config.sh), never from a Target Project literal.
bootstrap_config_touched() {
    local cfg="${1:?bootstrap_config_touched: config required}" line dir
    dir="$(printf '%s' "${cfg}" | jq -r '.config_dir // ""')"
    dir="${dir##*/}"
    dir="${dir:-${HARNESS_CONFIG_DIRNAME}}/"
    local -a hits=()
    while IFS= read -r line; do
        line="${line#./}"
        [ -n "${line}" ] || continue
        case "${line}" in
            "${dir}"*|*"/${dir}"*) hits+=("${line}") ;;
        esac
    done
    [ "${#hits[@]}" -gt 0 ] || return 0
    printf '%s\n' "${hits[@]}" | sort -u
}

_bs_usage() { sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed -n 's/^#\( \|$\)//p'; }

# _bs_changed_paths <pr> : the PR's changed paths, or stdin when no PR
_bs_changed_paths() {
    local pr="$1"
    if [ -n "${pr}" ]; then
        "${GH_BIN:-gh}" pr diff "${pr}" --name-only || return 1
    else
        cat
    fi
}

bootstrap_main() {
    local sub="${1:-}"; shift || true
    case "${sub}" in
        -h|--help|help) _bs_usage; return 0 ;;
        state|issue|config-touched) ;;
        '') _bs_usage >&2; return 2 ;;
        *) echo "bootstrap: unknown subcommand '${sub}'" >&2; _bs_usage >&2; return 2 ;;
    esac

    local pr='' target_arg='' head=0 dry=''
    while [ $# -gt 0 ]; do
        case "$1" in
            --pr) [ $# -ge 2 ] || { echo "bootstrap: --pr needs a number" >&2; return 2; }
                  pr="$2"; shift 2 ;;
            --pr=*) pr="${1#--pr=}"; shift ;;
            --dry-run) dry=--dry-run; shift ;;
            --head) head=1; shift ;;
            -*) echo "bootstrap: unknown option '$1'" >&2; return 2 ;;
            *) target_arg="$1"; shift ;;
        esac
    done
    if [ -n "${pr}" ]; then
        case "${pr}" in *[!0-9]*) echo "bootstrap: --pr needs a number" >&2; return 2 ;; esac
    fi

    local cfg
    if [ "${head}" -eq 1 ]; then
        cfg="$(harness_config_resolve_head "${target_arg}")" || return 2
    else
        cfg="$(harness_config_resolve "${target_arg}")" || return 2
    fi

    case "${sub}" in
        state) bootstrap_state "${cfg}" ;;
        issue)
            if ! bootstrap_is_state "${cfg}"; then
                echo "bootstrap: no bootstrap issue is owed — the Harness config declares a hermetic tier" >&2
                return 3
            fi
            bootstrap_issue_ensure "${cfg}" "${dry}" ;;
        config-touched)
            local paths
            paths="$(_bs_changed_paths "${pr}")" || {
                _bs_err "could not read the changed paths of PR #${pr}"
                return 1
            }
            printf '%s\n' "${paths}" | bootstrap_config_touched "${cfg}" ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    host_env_load
    bootstrap_main "$@"
fi
