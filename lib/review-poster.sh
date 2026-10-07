#!/usr/bin/env bash
# review-poster.sh: the Review Poster: render, post, and track the automated
# one-time code review's footprint on an Agent PR (the Fire's review round).
#
# Sourceable library owning the two machine markers that distinguish the
# automated review from human activity, behind stable functions so the skill
# never hand-rolls REST calls or marker strings. thread-reconciler.sh stays the
# only owner of thread enumeration/reply/resolve; this lib composes with it
# (`tr_unresolved_threads ... | rp_filter_agent_threads`).
#
# The repo every call posts to is the Target Project's slug from the Harness
# config (ADR 0002); no function takes or carries a repo argument. A call
# that cannot resolve the config returns non-zero with a stderr line and
# makes no gh call.
#
#   rp_apply_bar  (stdin: JSON array of findings)
#       -> the posting bar: settles every finding's `kind` (defect |
#         review-note | product-ambiguity). A Standards category or
#         scope-creep is a review-note; test-coverage is a review-note unless
#         the finding quotes the requirement that asked for the test; a defect
#         is demoted to a review-note (`demoted: true`) when BOTH hold: its
#         failure_scenario is empty (or is nothing but a bare "none" / "n/a" /
#         "no failure" phrase) AND its quoted_requirement is empty. An empty
#         scenario with a quote stays a defect (a requirement contradicted).
#         A product-ambiguity is never reclassified. Pure.
#
#   rp_split_findings  (stdin: a barred array)
#       -> {"defects","notes","ambiguities"}: only defects open threads and
#         only a non-empty defects list earns AFK:revise. Pure.
#
#   rp_render_finding <kind> <axis> <category> <severity> <summary> <failure_scenario>
#       -> stdout: the uniform inline-comment body: RP_MARKER first line, then
#         the visible 🤖 header (defect · axis · category · severity), summary,
#         failure scenario, dispute footer. Only kind `defect` renders; any
#         other kind returns 2 with a stderr line (the posting bar).
#
#   rp_render_notes <notes_json>
#       -> the one collapsed "Review notes, no action taken (N)" list for the
#         done-marker; nothing for []. The reconciler ignores it.
#
#   rp_render_ambiguities <ambiguities_json> / rp_parse_ambiguities (stdin)
#       -> the product ambiguities as a human list plus a structured
#         `<!-- pr-review-ambiguities … -->` block, and the parse back out of
#         a done-marker body (the Ruling request lane's seam).
#
#   rp_first_changed_line <commit_sha> <path>
#       -> "<line> <side>": the first line GitHub can anchor to in the file's
#         diff against origin/<default_branch>: the right-hand start of the
#         first hunk that adds a line (side RIGHT), else the left-hand start of
#         the first hunk that deletes one (side LEFT: a deleted or
#         deletion-only file). 2 with a stderr line when the PR did not change
#         the file; 1 with a stderr line when git itself failed (base not
#         fetched, GIT_BIN missing) — never silently.
#
#   rp_post_inline_fallback <pr> <commit_sha> <path> <intended_line> <body>
#       -> the anchoring fallback: posts the body on the file's first changed
#         line with the intended location named up front. A defect is never
#         folded into the summary.
#
#   rp_post_defect <pr> <commit_sha> <path> <line> <body>
#       -> the one call the skill makes per defect: rp_post_inline, and ONLY
#         on an HTTP 422 (GitHub refused the anchor) rp_post_inline_fallback.
#         Any other failure (auth, rate limit, 5xx, network) returns as-is so a
#         perfectly anchorable defect is never re-posted elsewhere.
#
#   rp_post_inline <pr> <commit_sha> <path> <line> <body> [side]
#       -> posts one inline review comment anchored to a diff line (side
#         RIGHT by default); on failure gh's stderr is kept in RP_LAST_ERROR
#         and echoed to stderr
#         (REST pulls/comments with commit_id/path/line/side). Non-zero
#         on API failure; the skill's 422 fallback keys off this.
#
#   rp_filter_agent_threads
#       -> pure stdin filter over tr_unresolved_threads' JSON array: keeps only
#         elements whose first-comment body contains RP_MARKER. The ONLY thread
#         set the fix loop may touch; human threads never pass.
#
#   rp_done_marker_present <pr>
#       -> exit 0 iff any top-level PR comment carries RP_DONE_MARKER. The
#         once-per-PR idempotency gate the review round checks before it
#         starts and the skill's pre-flight re-checks.
#
#   rp_post_done_marker <pr> <n_findings> <m_fixed> <reviewed_sha> <fix_sha> [notes_json] [ambiguities_json] [extra_markdown]
#       -> posts the top-level completion comment: machine marker with both
#         SHAs (`fixes=none` when nothing was pushed), the tally (threads,
#         notes and ambiguities counted apart), the collapsed Review notes
#         list, the ambiguities block, and any extra markdown the skill adds.
#         rp_render_done_marker_body renders the same body without posting.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        or AUTO_AGENT_TARGET_DIR): repo.slug
#   GH_BIN               gh CLI (default: gh), so tests stub the network
#                        away; behavior under test is the arguments passed
#                        and the parse of the responses, never a live API.
#   GIT_BIN              git (default: git), for the first-changed-line read.
#   AUTO_AGENT_TARGET_DIR  the Target Project checkout git runs in (-C);
#                        default: the current directory.

_review_poster_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_review_poster_lib_dir}/harness-config.sh"


RP_MARKER='<!-- pr-review-bot -->'
RP_DONE_MARKER='<!-- pr-review-done'

# _rp_slug -> the Target Project's owner/repo, or return 2 with a stderr line.
_rp_slug() { harness_config_slug review-poster; }

# The posting bar (Spec #74, "The posting bar"): only a defect opens a thread.
# Standards categories and scope-creep are Review notes whatever the reviewer
# called them. This list is the one source of truth; the category lists in
# plugin/skills/pr-review/SKILL.md and plugin/skills/correctness-review/SKILL.md
# name the same slugs. `test-coverage` is deliberately NOT here: a coverage gap
# is a note unless the issue or Spec asked for the test in words, which the
# finding shows by quoting that sentence (RP_QUOTE_GATED_CATEGORIES).
RP_NOTE_CATEGORIES='duplication|naming|test-structure|speculative-generality|page-object-bypass|style|note|scope-creep'
RP_QUOTE_GATED_CATEGORIES='test-coverage'
# A failure scenario that is nothing but a bare "nothing fails" phrase. The
# WHOLE scenario must be the phrase (plus trailing punctuation): this is the
# mechanical check only. "Nonexistent config crashes the boot" or "No failure
# is visible until the cache expires, then every request 500s" are real
# scenarios and are not matched; the reviewer's sorting is trusted.
RP_NO_FAILURE_RE='^(none|n/?a|nothing( breaks| fails)?|no (concrete |runtime |observable |user-visible )?(failure|breakage|defect|bug)( scenario)?)[[:space:].!]*$'

# rp_apply_bar  (stdin: JSON array of findings in the pr-review contract)
#   -> stdout: the same array with every element's `kind` settled:
#      - a product-ambiguity is passed through untouched, whatever its category;
#      - a finding with a Standards category or `scope-creep` is a `review-note`;
#      - a `test-coverage` finding is a `review-note` unless it quotes the
#        requirement that asked for the test;
#      - a `defect` (or a finding with no kind) is demoted to a `review-note`
#        with `demoted: true` when its failure_scenario is empty or a bare
#        no-failure phrase AND its quoted_requirement is empty; an empty
#        scenario with a quote stays a defect;
#      - everything else keeps its kind.
#      Pure; never calls gh.
rp_apply_bar() {
    jq -c --arg notes "${RP_NOTE_CATEGORIES}" --arg gated "${RP_QUOTE_GATED_CATEGORIES}" \
          --arg nofail "${RP_NO_FAILURE_RE}" '
        def no_failure: (.failure_scenario // "" | gsub("^\\s+|\\s+$"; "")) as $s
            | ($s == "") or ($s | test($nofail; "i"));
        def unquoted: ((.quoted_requirement // "") | gsub("^\\s+|\\s+$"; "")) == "";
        def cat_in($list): (.category // "" | test("^(" + $list + ")$"; "i"));
        [ .[]
          | .kind = (.kind // "defect")
          | if .kind == "product-ambiguity" then .
            elif cat_in($notes) then .kind = "review-note"
            elif cat_in($gated) and unquoted then .kind = "review-note"
            elif .kind == "defect" and no_failure and unquoted then .kind = "review-note" | .demoted = true
            else . end ]'
}

# rp_split_findings  (stdin: a barred JSON array, rp_apply_bar's output)
#   -> stdout: {"defects":[...],"notes":[...],"ambiguities":[...]} — the three
#      routes. Only `.defects` open threads and only a non-empty `.defects`
#      earns AFK:revise; a notes-only review applies no label.
rp_split_findings() {
    jq -c '{
        defects:     [ .[] | select(.kind == "defect") ],
        notes:       [ .[] | select(.kind == "review-note") ],
        ambiguities: [ .[] | select(.kind == "product-ambiguity") ] }'
}

# rp_render_finding <kind> <axis> <category> <severity> <summary> <failure_scenario>
# Only kind `defect` renders: a review-note or a product-ambiguity never opens
# a thread (return 2, stderr, nothing on stdout), so the bar cannot be bypassed
# by a caller that forgot rp_apply_bar.
rp_render_finding() {
    local kind="$1" axis="$2" category="$3" severity="$4" summary="$5" scenario="$6"
    if [ "${kind}" != "defect" ]; then
        echo "review-poster: ${kind} is not a thread kind; only a defect opens a thread" >&2
        return 2
    fi
    printf '%s\n' \
        "${RP_MARKER}" \
        "🤖 **pr-review** · defect · ${axis} · ${category} · ${severity}" \
        "" \
        "${summary}" \
        "" \
        "**Failure scenario:** ${scenario}" \
        "" \
        "_Automated one-time review (the Fire's review round). A fix round follows; reply to dispute._"
}

# rp_post_inline <pr> <commit_sha> <path> <line> <body> [side]
#   side defaults to RIGHT; LEFT anchors to a deleted line (a deleted or
#   deletion-only file). On failure gh's stderr is kept in RP_LAST_ERROR (so a
#   caller can tell a 422 from a 5xx) and echoed to stderr.
RP_LAST_ERROR=""
rp_post_inline() {
    local pr="$1" commit="$2" path="$3" line="$4" body="$5" side="${6:-RIGHT}" repo err rc
    repo="$(_rp_slug)" || return $?
    err="$(mktemp)"
    "${GH_BIN:-gh}" api "repos/${repo}/pulls/${pr}/comments" \
        -f body="${body}" \
        -f commit_id="${commit}" \
        -f path="${path}" \
        -F line="${line}" \
        -f side="${side}" >/dev/null 2>"${err}"
    rc=$?
    RP_LAST_ERROR="$(cat "${err}")"; rm -f "${err}"
    if [ "${rc}" -ne 0 ]; then
        echo "review-poster: inline post ${path}:${line} (${side}) failed: ${RP_LAST_ERROR:-exit ${rc}}" >&2
    fi
    return "${rc}"
}

# _rp_base -> the Target Project's default branch from the resolved config.
_rp_base() {
    local cfg
    cfg="$(harness_config_resolve 2>/dev/null)" || { echo "review-poster: no Harness config to read the default branch from" >&2; return 2; }
    printf '%s' "${cfg}" | jq -r '.repo.default_branch // empty'
}

# rp_first_changed_line <commit_sha> <path>
#   -> stdout: "<line> <side>", the first line GitHub can anchor a review
#      comment to in `git diff origin/<default_branch>...<sha> -- <path>`:
#      the right-hand start of the first hunk that adds at least one line
#      (side RIGHT); when no hunk adds a line (the PR deleted the file, or
#      every hunk is deletion-only) the left-hand start of the first hunk
#      that deletes one (side LEFT). A deletion-only first hunk's right-hand
#      start is not in the diff, so it is never returned.
#      Return 2 with a stderr line when the file has no hunk (not changed by
#      the PR); return 1 with a stderr line when git failed (origin/<base> not
#      fetched, GIT_BIN missing) — never a silent "not changed".
rp_first_changed_line() {
    local sha="$1" path="$2" base diff err rc hunks line
    base="$(_rp_base)" || return $?
    [ -n "${base}" ] || { echo "review-poster: the Harness config carries no default branch" >&2; return 2; }
    err="$(mktemp)"
    diff="$("${GIT_BIN:-git}" -C "${AUTO_AGENT_TARGET_DIR:-.}" diff --unified=0 "origin/${base}...${sha}" -- "${path}" 2>"${err}")"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        echo "review-poster: git diff origin/${base}...${sha} -- ${path} failed (exit ${rc}): $(tr '\n' ' ' <"${err}")" >&2
        rm -f "${err}"; return 1
    fi
    rm -f "${err}"
    # one "<old_start> <old_count> <new_start> <new_count>" per hunk
    hunks="$(printf '%s\n' "${diff}" \
        | sed -nE 's/^@@ -([0-9]+)(,([0-9]+))? \+([0-9]+)(,([0-9]+))? @@.*/\1|\3|\4|\6/p' \
        | awk -F'|' '{ oc = ($2 == "" ? 1 : $2); nc = ($4 == "" ? 1 : $4); print $1, oc, $3, nc }')"
    if [ -z "${hunks}" ]; then
        echo "review-poster: ${path} has no hunk against origin/${base}; the PR did not change it" >&2
        return 2
    fi
    line="$(printf '%s\n' "${hunks}" | awk '$4 > 0 { print $3, "RIGHT"; exit }')"
    [ -n "${line}" ] || line="$(printf '%s\n' "${hunks}" | awk '$2 > 0 { print $1, "LEFT"; exit }')"
    if [ -z "${line}" ]; then
        echo "review-poster: ${path} has no anchorable line in its diff against origin/${base}" >&2
        return 2
    fi
    printf '%s\n' "${line}"
}

# rp_post_inline_fallback <pr> <commit_sha> <path> <intended_line> <body>
#   -> the anchoring fallback: GitHub refused <intended_line>, so post the same
#      body on the file's first changed line (right-hand when the file has an
#      added line, left-hand when it is deleted or deletion-only) with the
#      intended location named up front. A defect is never folded into the
#      summary. Returns rp_first_changed_line's code (no post) when the file
#      has no changed line at all or git failed.
rp_post_inline_fallback() {
    local pr="$1" commit="$2" path="$3" intended="$4" body="$5" anchor first side fallback_body
    anchor="$(rp_first_changed_line "${commit}" "${path}")" || return $?
    first="${anchor%% *}"; side="${anchor##* }"
    # The preamble goes after the marker + header (lines 1-2); a body shorter
    # than that gets it appended.
    fallback_body="$(printf '%s\n' "${body}" | awk -v note="**Intended location:** \`${path}:${intended}\` (GitHub refused to anchor a comment there; posted on the file's first changed line)" \
        '{ print } NR == 2 { print ""; print note } END { if (NR < 2) { print ""; print note } }')"
    rp_post_inline "${pr}" "${commit}" "${path}" "${first}" "${fallback_body}" "${side:-RIGHT}"
}

# rp_post_defect <pr> <commit_sha> <path> <line> <body>
#   -> the one call the skill makes per defect. rp_post_inline at the intended
#      line; when, and only when, GitHub answered HTTP 422 (the line is not in
#      the diff / not commentable) fall back to rp_post_inline_fallback. Any
#      other failure (auth, rate limit, 5xx, network) is returned as-is so a
#      perfectly anchorable defect is never re-posted on another line with a
#      misleading "intended location" preamble.
rp_post_defect() {
    local pr="$1" commit="$2" path="$3" line="$4" body="$5" rc
    rp_post_inline "${pr}" "${commit}" "${path}" "${line}" "${body}" && return 0
    rc=$?
    if printf '%s' "${RP_LAST_ERROR}" | grep -qE 'HTTP 422|Unprocessable Entity|"status": ?"422"'; then
        echo "review-poster: GitHub refused to anchor ${path}:${line}; falling back to the file's first changed line" >&2
        rp_post_inline_fallback "${pr}" "${commit}" "${path}" "${line}" "${body}"
        return $?
    fi
    return "${rc}"
}

# rp_filter_agent_threads  (stdin: tr_unresolved_threads JSON array)
rp_filter_agent_threads() {
    jq -c --arg marker "${RP_MARKER}" \
        '[ .[] | select(.body | contains($marker)) ]'
}

# rp_done_marker_present <pr>
rp_done_marker_present() {
    local pr="$1" repo resp
    repo="$(_rp_slug)" || return $?
    resp="$("${GH_BIN:-gh}" api "repos/${repo}/issues/${pr}/comments" --paginate)" || return 1
    printf '%s' "${resp}" | jq -e --arg marker "${RP_DONE_MARKER}" \
        '[ .[] | select(.body | contains($marker)) ] | length > 0' >/dev/null
}

RP_AMBIGUITIES_MARKER='<!-- pr-review-ambiguities'

# rp_render_notes <notes_json>
#   -> stdout: one collapsed "Review notes, no action taken (N)" list, one line
#      per note with its location, axis, category and summary; a demoted
#      defect says so. Nothing at all for an empty array. The reconciler
#      ignores this list and the implementer never acts on it.
rp_render_notes() {
    local notes="${1:-[]}" n
    n="$(printf '%s' "${notes}" | jq -r 'length')" || return 2
    [ "${n}" -gt 0 ] || return 0
    printf '%s\n' "<details>" "<summary>Review notes, no action taken (${n})</summary>" ""
    printf '%s' "${notes}" | jq -r '.[] |
        "- `\(.path // "?"):\(.line // "?")` · \(.axis // "?") · \(.category // "?") — \(.summary // "")"
        + (if .demoted == true then " _(demoted: no concrete failure scenario, no quoted requirement)_" else "" end)'
    printf '%s\n' "" "</details>"
}

# rp_render_ambiguities <ambiguities_json>
#   -> stdout: the human list plus the structured block the Ruling request
#      lane reads back with rp_parse_ambiguities. Nothing for an empty array.
rp_render_ambiguities() {
    local ambs="${1:-[]}" n
    n="$(printf '%s' "${ambs}" | jq -r 'length')" || return 2
    [ "${n}" -gt 0 ] || return 0
    printf '%s\n' "**Product ambiguities (${n})** — no thread; routed to the Ruling request:" ""
    printf '%s' "${ambs}" | jq -r '.[] | "- `\(.path // "?"):\(.line // "?")` — \(.summary // "")"'
    printf '%s\n' "" "${RP_AMBIGUITIES_MARKER}"
    # `-->` inside any string would close the HTML comment early in GitHub's
    # renderer; `\u003e` is the same `>` in JSON, so the parse is unchanged.
    printf '%s' "${ambs}" | jq -c '.' | sed 's/-->/--\\u003e/g'
    printf '%s\n' "-->"
}

# rp_parse_ambiguities  (stdin: a done-marker comment body)
#   -> stdout: the JSON array from the structured block, or [] when absent.
rp_parse_ambiguities() {
    local body json
    body="$(cat)"
    json="$(printf '%s\n' "${body}" | sed -n "/^${RP_AMBIGUITIES_MARKER}\$/,/^-->\$/p" | sed '1d;$d')"
    if [ -z "${json}" ]; then printf '[]\n'; return 0; fi
    printf '%s' "${json}" | jq -c '.' 2>/dev/null || printf '[]\n'
}

# rp_render_done_marker_body <n_findings> <m_fixed> <reviewed_sha> <fix_sha> [notes_json] [ambiguities_json] [extra_markdown]
#   -> stdout: the done-marker comment body: machine marker first, the tally
#      (threads, notes, ambiguities counted apart), then the notes list, the
#      ambiguities, and any extra markdown the skill appends (the Harness
#      config flag, say). n_findings counts every Finding the review produced;
#      the defect threads are n minus the notes and ambiguities.
rp_render_done_marker_body() {
    local n="$1" m="$2" reviewed="$3" fix="$4" notes="${5:-[]}" ambs="${6:-[]}" extra="${7:-}"
    local nn na nd section
    nn="$(printf '%s' "${notes}" | jq -r 'length')" || return 2
    na="$(printf '%s' "${ambs}" | jq -r 'length')" || return 2
    nd=$((n - nn - na)); [ "${nd}" -ge 0 ] || nd=0
    printf '%s\n' \
        "${RP_DONE_MARKER} reviewed=${reviewed} fixes=${fix} -->" \
        "🤖 pr-review: ${n} findings, ${m} fixed (reviewed ${reviewed}) — ${nd} defect thread(s), ${nn} review note(s), ${na} product ambiguity(ies)"
    section="$(rp_render_notes "${notes}")"
    [ -n "${section}" ] && printf '\n%s\n' "${section}"
    section="$(rp_render_ambiguities "${ambs}")"
    [ -n "${section}" ] && printf '\n%s\n' "${section}"
    [ -n "${extra}" ] && printf '\n%s\n' "${extra}"
    return 0
}

# rp_post_done_marker <pr> <n_findings> <m_fixed> <reviewed_sha> <fix_sha> [notes_json] [ambiguities_json] [extra_markdown]
rp_post_done_marker() {
    local pr="$1" repo body
    repo="$(_rp_slug)" || return $?
    body="$(rp_render_done_marker_body "$2" "$3" "$4" "$5" "${6:-[]}" "${7:-[]}" "${8:-}")" || return $?
    "${GH_BIN:-gh}" api "repos/${repo}/issues/${pr}/comments" \
        -f body="${body}" >/dev/null
}
