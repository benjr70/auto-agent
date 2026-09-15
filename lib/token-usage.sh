#!/usr/bin/env bash
# token-usage.sh: per-ticket Claude token accounting from session transcripts.
#
# Every assistant message in a Claude Code transcript line carries
# `message.usage` (output/cache-read/cache-write/input tokens) and the
# `gitBranch` the session was on when the message landed. The harness's
# branch shapes are fixed (ADR 0002): a Slice Fire works `feat/issue-<N>` and
# a resolve Fire works `research/<slug>`, so summing usage by branch
# attributes the whole Fire's spend (main session AND subagent transcripts
# under <session>/subagents/) to the ticket it was working.
#
# Lines on other branches (the default branch, human sessions) are aggregated
# under "overhead" so totals reconcile, but only ticket rows are ever posted
# to GitHub.
#
# Usage:
#   token-usage.sh scan                              # every row, JSON on stdout
#   token-usage.sh scan --issue 417                  # one Slice row
#   token-usage.sh scan --branch research/<slug>     # one research row
#   token-usage.sh markdown --issue 417 [--branch research/<slug>]
#                                                    # the GitHub comment body
#   token-usage.sh post --issue 417 [--branch research/<slug>]
#                                                    # create-or-update the issue's
#                                                    #   token-usage comment (marker
#                                                    #   <!-- token-usage -->)
#
# With `--branch research/<slug>` the row is the research branch's and the
# comment still lands on issue N: a resolve Fire works a Decision ticket whose
# branch carries the ticket's slug, not its number.
#
# scan JSON shape:
#   { "issues":   { "<N>":    { "outputTokens": i, "cacheReadTokens": i,
#                               "cacheWriteTokens": i, "inputTokens": i,
#                               "turns": i, "sessions": i } , ... },
#     "research": { "<slug>": { same fields }, ... },
#     "overhead": { same fields } }
#
# post is idempotent: one marked comment per issue, PATCHed in place on
# re-runs, so re-posting after a resume or reconcile updates the same comment.
#
# Sourced functions:
#   tu_scan [key]                 key is an issue number or a research branch
#   tu_markdown <issue> [scanJson] [branch]
#   tu_post <issue> [scanJson] [branch]
#
# Exit codes: 0 ok; 2 usage error (including no Harness config where one is
# needed); 3 transcripts dir missing; 4 gh post failed.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        or AUTO_AGENT_TARGET_DIR): repo slug for post, the
#                        Target Project checkout for the transcripts dir
#   GH_BIN               gh CLI (default: gh), injectable for tests
#   TOKEN_USAGE_DIR      transcripts dir override (default: derived from the
#                        Target Project checkout, ~/.claude/projects/<encoded-cwd>)

set -uo pipefail

_token_usage_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_token_usage_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_token_usage_lib_dir}/host-env.sh"

# _tu_config -> the resolved config, or exit-2 semantics with a stderr line.
_tu_config() {
    local cfg
    cfg="$(harness_config_resolve 2>/dev/null)" || {
        echo "token-usage: no Harness config (set HARNESS_CONFIG_JSON or AUTO_AGENT_TARGET_DIR)" >&2
        return 2
    }
    printf '%s' "${cfg}"
}

# _tu_default_dir -> the Target Project checkout's transcripts dir.
_tu_default_dir() {
    local cfg top
    cfg="$(_tu_config)" || return $?
    top="$(harness_config_target_dir "${cfg}")" || {
        echo "token-usage: the Harness config names no Target Project checkout" >&2
        return 2
    }
    # Claude Code encodes the project cwd by replacing '/' and '.' with '-'.
    printf '%s/.claude/projects/%s' "${HOME}" "$(printf '%s' "${top}" | sed 's|[/.]|-|g')"
}

# _tu_dir -> the transcripts dir to scan (override, else derived).
_tu_dir() {
    if [ -n "${TOKEN_USAGE_DIR:-}" ]; then
        printf '%s' "${TOKEN_USAGE_DIR}"
    else
        _tu_default_dir
    fi
}

# _tu_research_slug <branch> -> the slug when the branch is a research branch.
_tu_research_slug() {
    case "$1" in
        "${HARNESS_BRANCH_RESEARCH_PREFIX}"?*) printf '%s' "${1#"${HARNESS_BRANCH_RESEARCH_PREFIX}"}" ;;
        *) return 1 ;;
    esac
}

# tu_scan [key]: aggregate usage by ticket; prints the scan JSON. The key is
# an issue number (filters .issues) or a research branch (filters .research).
tu_scan() {
    local only="${1:-}" only_issue='' only_slug=''
    local dir
    dir="$(_tu_dir)" || return $?
    [ -d "${dir}" ] || { echo "token-usage: transcripts dir not found: ${dir}" >&2; return 3; }

    if [ -n "${only}" ]; then
        only_slug="$(_tu_research_slug "${only}")" || only_issue="${only}"
    fi

    # One pass: per file (main sessions + subagents), emit one compact record
    # per usage-bearing line, tagged with the source file for session counting.
    # The branch shapes are the harness's fixed prefixes, regex-escaped.
    local feat_re research_re
    feat_re="^$(harness_re_escape "${HARNESS_BRANCH_FEATURE_PREFIX}")(?<n>[0-9]+)\$"
    research_re="^$(harness_re_escape "${HARNESS_BRANCH_RESEARCH_PREFIX}")(?<s>.+)\$"
    find "${dir}" -name '*.jsonl' -type f -print0 \
    | while IFS= read -r -d '' f; do
        jq -c --arg file "${f}" --arg feat "${feat_re}" --arg research "${research_re}" '
            select(.message.usage? != null)
            | (.gitBranch // "") as $b
            | (if ($b | test($feat)) then { bucket: "issues", key: ($b | capture($feat) | .n) }
               elif ($b | test($research)) then { bucket: "research", key: ($b | capture($research) | .s) }
               else { bucket: "overhead", key: "overhead" } end) as $k
            | { bucket: $k.bucket, key: $k.key, file: $file,
                out: (.message.usage.output_tokens // 0),
                cr:  (.message.usage.cache_read_input_tokens // 0),
                cw:  (.message.usage.cache_creation_input_tokens // 0),
                inp: (.message.usage.input_tokens // 0) }' "${f}" 2>/dev/null \
            || true   # noise lines (non-JSON) fail this jq; with pipefail the
                      # LAST file's status would sink the whole scan; tolerate
    done \
    | jq -s --arg only_issue "${only_issue}" --arg only_slug "${only_slug}" '
        def rows(b): [ .[] | select(.bucket == b) ]
            | group_by(.key)
            | map({ key: .[0].key,
                    value: { outputTokens: (map(.out) | add),
                             cacheReadTokens: (map(.cr) | add),
                             cacheWriteTokens: (map(.cw) | add),
                             inputTokens: (map(.inp) | add),
                             turns: length,
                             sessions: (map(.file) | unique | length) } })
            | from_entries;
        def empty_row: { outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0,
                         inputTokens: 0, turns: 0, sessions: 0 };
        { issues:   (rows("issues")   | with_entries(select($only_slug == "" and ($only_issue == "" or .key == $only_issue)))),
          research: (rows("research") | with_entries(select($only_issue == "" and ($only_slug == "" or .key == $only_slug)))),
          overhead: (rows("overhead").overhead // empty_row) }'
}

# tu_fmt <int>: humanize a token count (1234 -> "1.2k", 45012345 -> "45.0M").
tu_fmt() {
    awk -v n="$1" 'BEGIN {
        if (n >= 1000000)      printf "%.1fM", n / 1000000
        else if (n >= 1000)    printf "%.1fk", n / 1000
        else                   printf "%d", n
    }'
}

# tu_markdown <issue> [scanJson] [branch]: the comment body for one ticket.
tu_markdown() {
    local issue="$1" scan="${2:-}" branch="${3:-}" slug='' row label
    if [ -n "${branch}" ]; then
        slug="$(_tu_research_slug "${branch}")" || {
            echo "token-usage: --branch must be a ${HARNESS_BRANCH_RESEARCH_PREFIX}<slug> branch, got ${branch}" >&2
            return 2
        }
    fi
    [ -n "${scan}" ] || scan="$(tu_scan "${branch:-${issue}}")" || return $?
    if [ -n "${slug}" ]; then
        row="$(printf '%s' "${scan}" | jq -c --arg s "${slug}" '.research[$s] // empty')"
        label="${branch}"
    else
        row="$(printf '%s' "${scan}" | jq -c --arg n "${issue}" '.issues[$n] // empty')"
        label="${HARNESS_BRANCH_FEATURE_PREFIX}${issue}"
    fi
    if [ -z "${row}" ]; then
        echo "token-usage: no transcript data for issue #${issue}${branch:+ on ${branch}}" >&2
        return 2
    fi
    local out cr cw turns sessions
    out="$(printf '%s' "${row}" | jq -r '.outputTokens')"
    cr="$(printf '%s' "${row}" | jq -r '.cacheReadTokens')"
    cw="$(printf '%s' "${row}" | jq -r '.cacheWriteTokens')"
    turns="$(printf '%s' "${row}" | jq -r '.turns')"
    sessions="$(printf '%s' "${row}" | jq -r '.sessions')"
    cat <<MD
<!-- token-usage -->
### 🔢 Token usage (agent transcripts)

| metric       | value |
| ------------ | ----- |
| output       | $(tu_fmt "${out}") |
| cache reads  | $(tu_fmt "${cr}") |
| cache writes | $(tu_fmt "${cw}") |
| API turns    | ${turns} |
| sessions     | ${sessions} |

_Summed over every \`${label}\` transcript line (the Fire session +
subagents) on the Host. Updated $(date -u +%Y-%m-%dT%H:%M:%SZ)._
MD
}

# tu_post <issue> [scanJson] [branch]: create-or-update the marked comment.
tu_post() {
    local issue="$1" scan="${2:-}" branch="${3:-}"
    local gh="${GH_BIN:-gh}" cfg repo body existing_id
    cfg="$(_tu_config)" || return $?
    repo="$(printf '%s' "${cfg}" | jq -r '.repo.slug // empty')"
    if [ -z "${repo}" ]; then
        echo "token-usage: the Harness config names no repo slug" >&2
        return 2
    fi
    body="$(tu_markdown "${issue}" "${scan}" "${branch}")" || return $?

    existing_id="$("${gh}" api "repos/${repo}/issues/${issue}/comments" --paginate \
        --jq '[.[] | select(.body | contains("<!-- token-usage -->"))][0].id // empty' \
        2>/dev/null || echo '')"

    if [ -n "${existing_id}" ]; then
        "${gh}" api --method PATCH "repos/${repo}/issues/comments/${existing_id}" \
            -f body="${body}" >/dev/null \
            || { echo "token-usage: PATCH failed for issue #${issue}" >&2; return 4; }
        echo "token-usage: updated comment on #${issue}"
    else
        "${gh}" api --method POST "repos/${repo}/issues/${issue}/comments" \
            -f body="${body}" >/dev/null \
            || { echo "token-usage: POST failed for issue #${issue}" >&2; return 4; }
        echo "token-usage: posted comment on #${issue}"
    fi
}

_tu_main() {
    local cmd="${1:-}"; shift || true
    local issue='' branch=''
    # Every flag carries a value; a trailing value-less flag is a usage error,
    # not a reason to spin (with $#=1 a `shift 2` fails and the loop never
    # advances).
    while [ $# -gt 0 ]; do
        if [ $# -lt 2 ]; then
            echo "token-usage: $1 requires a value" >&2; exit 2
        fi
        case "$1" in
            --issue)  issue="${2:-}"; shift 2 ;;
            --branch) branch="${2:-}"; shift 2 ;;
            *) echo "token-usage: unknown arg $1" >&2; exit 2 ;;
        esac
    done
    host_env_load
    case "${cmd}" in
        scan)     if [ -n "${issue}" ] && [ -n "${branch}" ]; then
                      echo "token-usage: scan takes --issue or --branch, not both" >&2; exit 2
                  fi
                  tu_scan "${branch:-${issue}}" ;;
        markdown) [ -n "${issue}" ] || { echo "token-usage: markdown needs --issue" >&2; exit 2; }
                  tu_markdown "${issue}" '' "${branch}" ;;
        post)     [ -n "${issue}" ] || { echo "token-usage: post needs --issue" >&2; exit 2; }
                  tu_post "${issue}" '' "${branch}" ;;
        *) echo "usage: token-usage.sh scan|markdown|post [--issue N] [--branch research/<slug>]" >&2; exit 2 ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    _tu_main "$@"
    exit $?
fi
