#!/usr/bin/env bash
# pr-triage.sh: the PR Triage. Which open Agent PR (if any) needs reconciling.
#
# Sourceable library. Four functions:
#
#   pr_triage_scan        owns the `gh pr list` call (rides out GitHub's async
#                         mergeability), then enrich | pick. What the pickup
#                         Fire and `bin/auto-agent pr-triage` run.
#   pr_triage_enrich      stdin filter: merges the per-PR comment and file
#                         signals (review/verify done, docs-only, Dependabot
#                         classification) into a `gh pr list --json` payload.
#   pr_triage_pick        pure: reads the (enriched) payload on stdin and
#                         emits the verdict below.
#   pr_triage_bot_verdict_unworkable <verdict>
#                         exit 0 when the verdict names a Bot PR the deps-land
#                         lane is not on for (Harness config); the pick libs
#                         then fall through instead of reconciling it.
#
# The verdict, on stdout, is either the single PR the reconcile step should
# work this Fire, or a no-pick:
#
#     { "pr": <number>, "branch": "feat/issue-<M>", "issue": <M>,
#       "reason": "revise|conflict|docs-merge|incomplete" }
#     { "pr": null }
#
# A Dependabot PR (see below) is verdicted in the same call but a wider shape,
# because the deps lane needs its whole classification up front:
#
#     { "pr": N, "branch": "dependabot/…", "issue": null, "reason": "dependabot",
#       "security": <bool>, "major": <bool>, "sha": "<headRefOid>",
#       "tierA": <bool>, "tierB": <bool>, "attempts": <int> }
#
# and a CONFLICTING one keeps reason "conflict" with one extra key,
# "agentCommits": true when the branch carries a commit Dependabot did not
# author, which is what decides between an `@dependabot rebase` nudge and the
# agent rebasing the branch itself.
#
# `issue` is null when no ticket number can be derived from the head branch or
# the PR title (possible on a hand-named research branch); the caller must
# handle that: no issue lock, no ticket comment, the PR is still worked.
#
# Carried over from the Smart Smoker harness. Every Target Project fact now
# comes from the Harness config (ADR 0002): the repo slug every gh call names,
# the research docs prefix, where a `.github/dependabot.yml` would live, and
# whether the deps-land lane is on. The branch shapes and labels are the
# harness's fixed vocabulary (lib/harness-config.sh constants), not config.
#
# "Ours" filter: a PR is only ever considered when ALL hold:
#   - state OPEN and not a draft (drafts are the escalation parking state and
#     must never be auto-picked);
#   - head branch matches one of the shapes the harness creates:
#     `feat/issue-<M>` (Slices) or `research/<ticket-slug>` (resolve-lane
#     research PRs, which are exactly the docs-only PRs reason "docs-merge"
#     exists for). Defends against reconciling a human's hand-made PR. The
#     shape lives in one place, PR_TRIAGE_OURS_RE, built from the fixed branch
#     prefixes, because three jq programs below must agree on it;
#   - the author matches. Which author depends on the branch: a `dependabot/`
#     branch must be authored by the Dependabot app (login `app/dependabot` on
#     a listing), every other shape by PR_TRIAGE_AUTHOR when that env is
#     non-empty. The two tests are exclusive, so a human-named `dependabot/…`
#     branch and a fork PR reusing the shape both fail (the branch prefix is a
#     shape, never a licence), while a Dependabot PR is never rejected for not
#     being the agent's login.
#
# Dependabot PRs: a PR is one only when its author is the Dependabot app AND
# its head branch starts with `dependabot/`. They are the deps-land lane's
# input and are triaged apart from Agent PRs:
#   - they only ever earn reason "conflict" or "dependabot"; the tail signals
#     (revise / docs-merge / incomplete) describe an Agent PR's review rounds
#     and say nothing about a bot branch, so an `AFK:revise` label on one is
#     ignored;
#   - BOTH bot reasons rank LAST, below every Agent-PR reason: a human waiting
#     on their own PR is never queued behind a bot, including behind a
#     CONFLICTING one, whose reason "conflict" is the Agent rank-1 name but
#     ranks with the bot block. Inside the block a conflicting bot PR comes
#     before a "dependabot" one (nothing can be verified on a branch that has
#     to be rebased first); within reason "dependabot", security bumps come
#     before version bumps, then oldest createdAt, one per Fire;
#   - a bot PR carrying `HITL` (a major bump handed to the maintainer) is
#     invisible until GitHub's reviewDecision is APPROVED; the native approval
#     is how a human re-admits it;
#   - `AFK:deps-failed` (and the draft state that accompanies it) parks a bot
#     PR for good, exactly as the other escalation labels park an Agent PR.
# The lane that acts on a "dependabot" verdict (retitle, tier A/B, fix loop,
# gate, merge) is the deps-land skill; this module only classifies. Whether
# that lane is ON is the Harness config's call: pr_triage_bot_verdict_unworkable
# (below) is the ONE predicate both callers ask.
#
# Needs-attention: a filtered PR is picked when EITHER holds:
#   - it carries the `AFK:revise` label (a human reviewed and explicitly handed
#     it back to the agent): reason "revise";
#   - its mergeable state is CONFLICTING (the default branch moved under it):
#     reason "conflict". MERGEABLE and UNKNOWN both skip: UNKNOWN means GitHub
#     is still computing mergeability async; the next Fire re-checks rather
#     than guessing;
#   - it is otherwise clean but every file it changes lives under the research
#     docs prefix: reason "docs-merge". A research PR carries no code risk and
#     never earns review/verify rounds, so it is squash-merged by
#     lib/docs-only-gate.sh instead of being reconciled. The file list comes
#     from pr_triage_enrich too; an absent docsOnly reads as false, so a broken
#     sensor can never auto-merge anything;
#   - it is otherwise clean but the Fire's review and verify tail never finished: the one-time
#     review marker (<!-- pr-review-done -->) and/or any manual-verification
#     round comment is missing (a prior Fire died mid-tail): reason
#     "incomplete". These signals live in PR comments, so pr_triage_enrich
#     (below) merges them into the payload first; an un-enriched payload reads
#     every PR as complete (only an explicit false flags incomplete; jq's //
#     would swallow false, so the pick tests != false).
#   PRs already escalated (AFK:revise-failed / AFK:rebase-failed) are skipped:
#   they are parked for a human; re-picking them would loop on a known-stuck PR.
#
# Pick order: `AFK:revise` beats plain CONFLICTING (a human is actively waiting
# on their own review), which beats "docs-merge" (a docs PR cannot be merged
# while it conflicts anyway), which beats "incomplete" (nothing blocks a merge
# yet; the tail just needs finishing). "docs-merge" is tested BEFORE the
# incomplete markers precisely because a docs PR never gets those rounds and
# would otherwise be reconciled forever. Within the same reason rank, oldest
# createdAt wins.
#
# The pick is pure: it reads only stdin + env. pr_triage_scan owns the gh call:
#
#   gh pr list --repo <slug> --state open --json \
#     number,headRefName,title,isDraft,mergeable,labels,createdAt,author,\
#     headRefOid,reviewDecision
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        <target-dir>, or AUTO_AGENT_TARGET_DIR): repo slug,
#                        docs_research_prefix, config_dir, lanes.deps_land.
#                        When it cannot resolve, every function fails SAFE the
#                        way malformed input does (no pick, payload passed
#                        through) with one stderr line.
#   PR_TRIAGE_AUTHOR     agent's GitHub login; empty (default) disables the
#                        check for Agent PRs (never for Dependabot PRs)
#   PR_TRIAGE_DEPENDABOT_YML
#                        tests only: a path whose ABSENCE means "every
#                        Dependabot PR is a security update". Defaults to
#                        <target-dir>/.github/dependabot.yml, the Target
#                        Project's own config.
#   GH_BIN               gh CLI (default: gh), injected for tests
# Exit codes:
#   0  a PR was picked (verdict has a number)
#   1  nothing needs attention (verdict {"pr":null}); also for empty/malformed
#      input or an unresolvable config: a broken sensor must fall through to
#      the normal pick, not crash.

_pr_triage_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_pr_triage_lib_dir}/harness-config.sh"
# shellcheck source=docs-research-paths.sh
. "${_pr_triage_lib_dir}/docs-research-paths.sh"
# The Dependabot lane owns its marker vocabulary (deps_lane_marker_parse); the
# triage only reads markers, never writes them.
# shellcheck source=deps-lane.sh
. "${_pr_triage_lib_dir}/deps-lane.sh"

# The one definition of an "ours"-shaped head branch (see the header), built
# from the fixed branch prefixes so nothing a human hand-names should match.
PR_TRIAGE_OURS_RE="^($(harness_re_escape "${HARNESS_BRANCH_FEATURE_PREFIX}")[0-9]+\
|$(harness_re_escape "${HARNESS_BRANCH_RESEARCH_PREFIX}")[A-Za-z0-9._-]+\
|$(harness_re_escape "${HARNESS_BRANCH_DEPENDABOT_PREFIX}")[A-Za-z0-9._/-]+)$"

# jq prelude shared by the three programs below: the ours-shaped test, the
# Dependabot test and the tolerant ticket-number extraction. A research branch
# may or may not carry the ticket number; `capture` raises on no-match, so
# every branch is guarded with `?` and the whole chain falls back to null
# rather than collapsing the pick. Every branch prefix and label arrives as a
# jq arg from the harness vocabulary (see _pt_jq_args), never spelled here.
# shellcheck disable=SC2016  # jq program text: $vars are jq vars.
PR_TRIAGE_JQ_DEFS='
    def ours: .headRefName // "" | test($ours);
    def deps_branch: (.headRefName // "") | startswith($deps);
    def deps_author:
      # ONE login: the Dependabot app as a listing names it, app/dependabot.
      # Nothing else is accepted: a user account can be renamed to dependabot
      # or dependabot[bot], and a dependabot/... head branch is a shape anyone
      # can push, so a wider test would hand a human account the auto-pick
      # licence the lane grants only the app.
      ((.author.login // "") | ascii_downcase) == "app/dependabot";
    def dependabot_pr: deps_branch and deps_author;
    def author_ok:
      if deps_branch then deps_author
      else ($author == "") or ((.author.login // "") == $author) end;
    def labels_of: [.labels[]?.name // empty];
    def issue_of:
      (.headRefName // "") as $b
      | (($b | select(startswith($feat)) | ltrimstr($feat) | select(test("^[0-9]+$")) | tonumber)?
         // ($b | select(startswith($research)) | ltrimstr($research) | capture("^(?<n>[0-9]+)") | .n | tonumber)?
         // ((.title // "") | capture("#(?<n>[0-9]+)") | .n | tonumber)?
         // null);
'

# _pt_jq_args: the jq arguments every program takes, printed one per line for
# `mapfile`. The author is per call and passed separately.
_pt_jq_args() {
    printf '%s\n' \
        --arg ours "${PR_TRIAGE_OURS_RE}" \
        --arg feat "${HARNESS_BRANCH_FEATURE_PREFIX}" \
        --arg research "${HARNESS_BRANCH_RESEARCH_PREFIX}" \
        --arg deps "${HARNESS_BRANCH_DEPENDABOT_PREFIX}" \
        --arg l_revise "${HARNESS_LABEL_REVISE}" \
        --arg l_revise_failed "${HARNESS_LABEL_REVISE_FAILED}" \
        --arg l_rebase_failed "${HARNESS_LABEL_REBASE_FAILED}" \
        --arg l_deps_failed "${HARNESS_LABEL_DEPS_FAILED}" \
        --arg l_hitl "${HARNESS_LABEL_HITL}"
}

# _pt_cfg: the resolved Harness config, or return 2 after one stderr line.
_pt_cfg() {
    local cfg
    cfg="$(harness_config_resolve 2>/dev/null)" || {
        echo "pr-triage: no Harness config to resolve the Target Project from" >&2
        return 2
    }
    printf '%s' "${cfg}"
}

# _pt_dependabot_yml <cfg>: where the Target Project's Dependabot config would
# live. Its ABSENCE is the security default (see pr_triage_enrich): with no
# config every Dependabot PR is a security update.
_pt_dependabot_yml() {
    if [ -n "${PR_TRIAGE_DEPENDABOT_YML:-}" ]; then
        printf '%s' "${PR_TRIAGE_DEPENDABOT_YML}"
        return 0
    fi
    local target
    target="$(harness_config_target_dir "$1" 2>/dev/null)" || target=''
    printf '%s' "${target:-/nonexistent}/.github/dependabot.yml"
}

# pr_triage_scan: own the gh call AND ride out GitHub's async mergeability.
#
# A push to the default branch queues a background recompute of every open
# PR's mergeable state; until it lands, the API says UNKNOWN and
# pr_triage_pick (correctly) refuses to guess. But "re-check next Fire"
# strands a conflicted PR for a whole no-work sleep when the Fire lands
# seconds after a merge. Querying mergeable is itself what triggers the
# recompute, so polling resolves it in seconds: re-list while any ours-shaped
# open non-draft PR is still UNKNOWN, up to PR_TRIAGE_UNKNOWN_RETRIES re-lists
# (default 6) every PR_TRIAGE_UNKNOWN_INTERVAL seconds (default 20, about two
# minutes worst case), then triage whatever the last listing said.
#
# Env (beyond pr_triage_pick's): GH_BIN, PR_TRIAGE_UNKNOWN_RETRIES,
# PR_TRIAGE_UNKNOWN_INTERVAL, PR_TRIAGE_SLEEP (injectable for tests).
# Exit codes: pr_triage_pick's.
pr_triage_scan() {
    local gh="${GH_BIN:-gh}" tries="${PR_TRIAGE_UNKNOWN_RETRIES:-6}"
    local interval="${PR_TRIAGE_UNKNOWN_INTERVAL:-20}" sleep_bin="${PR_TRIAGE_SLEEP:-sleep}"
    local i prs unknown fields cfg slug
    local -a jq_args

    cfg="$(_pt_cfg)" || { printf '{"pr":null}\n'; return 1; }
    export HARNESS_CONFIG_JSON="${cfg}"
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug // empty')"
    if [ -z "${slug}" ]; then
        echo "pr-triage: the Harness config names no repo slug" >&2
        printf '{"pr":null}\n'
        return 1
    fi
    mapfile -t jq_args < <(_pt_jq_args)

    # The listing's fields, in one place: the shapes/labels the triage filters
    # on, plus the two the Dependabot verdict is built from (headRefOid keys the
    # lane's markers, reviewDecision re-admits a HITL major).
    fields='number,headRefName,title,isDraft,mergeable,labels,createdAt,author'
    fields="${fields},headRefOid,reviewDecision"

    for ((i = 0; i <= tries; i++)); do
        prs="$("${gh}" pr list --repo "${slug}" --state open --json "${fields}" \
            2>/dev/null || echo '[]')"

        unknown="$(printf '%s' "${prs}" | jq "${jq_args[@]}" \
            "${PR_TRIAGE_JQ_DEFS}"'
            [ .[]
              | select((.isDraft // false) | not)
              | select(ours)
              | select((.mergeable // "UNKNOWN") == "UNKNOWN") ]
            | length' 2>/dev/null || echo '0')"

        if [ "${unknown:-0}" = "0" ] || [ "${i}" -ge "${tries}" ]; then
            printf '%s' "${prs}" | pr_triage_enrich | pr_triage_pick
            return $?
        fi
        "${sleep_bin}" "${interval}"
    done
}

# _pr_triage_bump_major <from> <to>: exit 0 when the version move is a MAJOR
# bump in the sense the deps lane cares about, i.e. "a human must look at
# this". That is a leading-component increase, plus the semver-0 rule: below
# 1.0.0 the minor is the breaking-change component (0.4.x to 0.5.0 may break
# everything), so a 0.x minor bump is promoted to major. Anything the parser
# cannot read as two numeric components is a major too: the lane must never
# call an unknown move safe.
_pr_triage_bump_major() {
    local from="$1" to="$2" f_maj f_min t_maj t_min
    f_maj="${from%%.*}"; f_maj="${f_maj%%[!0-9]*}"
    t_maj="${to%%.*}";   t_maj="${t_maj%%[!0-9]*}"
    [ -n "${f_maj}" ] && [ -n "${t_maj}" ] || return 0
    [ "${t_maj}" -gt "${f_maj}" ] && return 0
    if [ "${t_maj}" -eq "${f_maj}" ] && [ "${f_maj}" -eq 0 ]; then
        f_min="${from#*.}"; f_min="${f_min%%.*}"; f_min="${f_min%%[!0-9]*}"
        t_min="${to#*.}";   t_min="${t_min%%.*}"; t_min="${t_min%%[!0-9]*}"
        [ -n "${f_min}" ] && [ -n "${t_min}" ] || return 0
        [ "${t_min}" -gt "${f_min}" ] && return 0
    fi
    return 1
}

# _pr_triage_deps_major <commit-message>: print true|false, the highest bump
# level across every `from A to B` pair in the bot's first commit message.
#
# The commit message, not the PR title: a grouped/multi-dependency PR's title
# carries no versions at all, while the message body lists every dependency it
# moved. Any single major pair makes the whole PR major, and a message with no
# readable pair prints true: an unparseable bump is never landed unattended.
_pr_triage_deps_major() {
    local msg="${1:-}" pairs line from to
    pairs="$(printf '%s' "${msg}" \
        | grep -oE 'from [0-9][0-9A-Za-z.+_-]* to [0-9][0-9A-Za-z.+_-]*')" || pairs=''
    if [ -z "${pairs}" ]; then
        printf 'true'
        return 0
    fi
    while IFS= read -r line; do
        [ -n "${line}" ] || continue
        from="${line#from }"; from="${from%% to *}"
        to="${line##* to }"
        if _pr_triage_bump_major "${from}" "${to}"; then
            printf 'true'
            return 0
        fi
    done <<< "${pairs}"
    printf 'false'
    return 0
}

# _pr_triage_enrich_deps_one <pr-number> <slug> <dependabot-yml> < payload > payload
#
# The Dependabot half of pr_triage_enrich: ONE
# `gh pr view <N> --repo <slug> --json comments,files,commits,body` round trip
# per bot PR, merged into the payload as the fields the pick and the lane
# consume:
#
#   depsSecurity  Dependabot's security-update footer in the PR body. The body
#                 rides this round trip rather than the listing on purpose:
#                 the listing is shared with the issue picker, whose contract
#                 is that no gh call it makes ever pulls a body. While the
#                 Target Project carries no dependabot.yml every Dependabot PR
#                 IS a security update, so an absent footer defaults to true
#                 and only flips to false once that config exists.
#   depsMajor     highest bump level over the FIRST commit message's
#                 `from A to B` pairs (see _pr_triage_deps_major).
#   agentCommits  true when any commit on the branch was NOT authored by
#                 Dependabot. Dependabot refuses to rebase a branch carrying
#                 foreign commits, so a conflicting PR with agent commits must
#                 be rebased by the agent itself rather than nudged. Read for
#                 every bot PR, including CONFLICTING ones, which is exactly
#                 why they are probed at all. UNKNOWN reads as true: `false`
#                 sends the lane down the `@dependabot rebase` nudge, which a
#                 branch that already carries an agent commit silently ignores
#                 and the PR is then stranded forever. `true` costs at most an
#                 unnecessary agent-side rebase of a branch the bot could have
#                 rebased itself, so an unread probe defaults to true.
#   tierA/tierB/fixAttempts
#                 the lane's own sha-keyed markers, parsed out of the comment
#                 bodies by lib/deps-lane.sh. Markers are keyed to the CURRENT
#                 head sha, so a force-pushed PR reads as fresh and is fully
#                 re-verified.
#
# Fails SAFE: on any gh/jq error the fields stay absent, and pr_triage_pick
# only ever names reason "dependabot" for a PR that carries them; a broken
# sensor yields no bot pick at all rather than a guessed classification.
_pr_triage_enrich_deps_one() {
    local num="$1" slug="$2" dependabot_yml="$3" gh="${GH_BIN:-gh}" payload view
    local security major agent_commits sha markers fields merged

    payload="$(cat)"

    if ! view="$("${gh}" pr view "${num}" --repo "${slug}" --json comments,files,commits,body 2>/dev/null)"; then
        printf '%s' "${payload}"
        return 0
    fi
    if ! printf '%s' "${view}" | jq -e '(.commits | type) == "array"' >/dev/null 2>&1; then
        printf '%s' "${payload}"
        return 0
    fi

    local body first_msg
    body="$(printf '%s' "${view}" | jq -r '.body // ""' 2>/dev/null)" || body=''
    first_msg="$(printf '%s' "${view}" | jq -r \
        '(.commits[0].messageHeadline // "") + "\n" + (.commits[0].messageBody // "")' \
        2>/dev/null)" || first_msg=''

    security=true
    if ! printf '%s' "${body}" | grep -qiF "automated security fix"; then
        [ -f "${dependabot_yml}" ] && security=false
    fi

    major="$(_pr_triage_deps_major "${first_msg}")"

    # A commit is the bot's when any of its authors names dependabot; anything
    # else on the branch is an agent (or human) commit.
    agent_commits="$(printf '%s' "${view}" | jq -c '
        any(.commits[]?;
            (any(.authors[]?;
                 ((.login // "") + " " + (.name // "") + " " + (.email // ""))
                 | ascii_downcase | test("dependabot"))) | not)' 2>/dev/null)"
    # Unreadable means true (see the header): the safe direction is "assume the
    # branch is the agent's to rebase", never "nudge a bot that will refuse".
    [ "${agent_commits}" = "true" ] || [ "${agent_commits}" = "false" ] || agent_commits=true

    fields="$(jq -cn --argjson s "${security}" --argjson m "${major}" \
        --argjson a "${agent_commits}" \
        '{depsSecurity: $s, depsMajor: $m, agentCommits: $a}' 2>/dev/null)" || {
        printf '%s' "${payload}"
        return 0
    }

    # Marker state is meaningful only against the current head sha; with no sha
    # in the listing there is nothing to key on and the PR simply reads fresh.
    sha="$(printf '%s' "${payload}" | jq -r --argjson n "${num}" \
        '.[] | select(.number == $n) | .headRefOid // ""' 2>/dev/null)" || sha=''
    if [ -n "${sha}" ]; then
        markers="$(printf '%s' "${view}" | jq -r '.comments[]?.body // ""' 2>/dev/null \
            | deps_lane_marker_parse "${sha}" 2>/dev/null)" || markers=''
        if [ -n "${markers}" ]; then
            fields="$(printf '%s' "${fields}" | jq -c --argjson m "${markers}" \
                '. + {tierA: $m.tierA, tierB: $m.tierB, fixAttempts: $m.fixAttempts}' \
                2>/dev/null || printf '%s' "${fields}")"
        fi
    fi

    merged="$(printf '%s' "${payload}" | jq -c --argjson n "${num}" --argjson f "${fields}" \
        'map(if .number == $n then . + $f else . end)' 2>/dev/null)" || merged=''
    if [ -n "${merged}" ]; then
        printf '%s' "${merged}"
    else
        printf '%s' "${payload}"
    fi
    return 0
}

# pr_triage_enrich: merge the "did the review and verify tail finish?" comment signals and the
# "docs-only?" file signal into the PR-list payload so pr_triage_pick can
# triage reasons "incomplete" and "docs-merge".
#
# Reads the `gh pr list --json ...` array on stdin and, for every PR that is
# ours-shaped and otherwise attention-free (open, non-draft, ours branch shape,
# author match, no AFK:revise, not parked, not CONFLICTING), fetches its
# conversation comments AND its changed-file list in ONE
# `gh pr view --json comments,files` round trip (two calls per PR would double
# this sensor's API cost and its rate-limit exposure) and merges:
#   reviewDone  any comment contains the <!-- pr-review-done --> marker
#               (posted by the review skill via lib/review-poster.sh)
#   verifyDone  any comment matches "Manual verification — .*round"
#               (posted by the verify round, one per round)
#   docsOnly    the PR changes at least one file and every one of them is
#               under the research docs prefix (lib/docs-research-paths.sh,
#               the same rule lib/docs-only-gate.sh re-runs against the real
#               diff before merging)
# Everything else passes through untouched.
#
# Fails SAFE toward "complete": on any gh/jq error the fields stay absent and
# pr_triage_pick's `// true` defaults read the PR as complete, and an absent
# docsOnly is not true so nothing is auto-merged; a broken sensor must never
# start a pick/wake loop, nor land a merge. Known accepted gap: a Fire that
# crashed after posting round 1 leaves a FAIL-latest PR looking complete.
#
# Env: GH_BIN, PR_TRIAGE_AUTHOR (same semantics as pr_triage_pick).
# Exit: always 0; stdout is the (possibly enriched) payload.
pr_triage_enrich() {
    local gh="${GH_BIN:-gh}" payload nums deps_nums num view review_done verify_done
    local docs_only fields merged cfg slug prefix dependabot_yml
    local -a jq_args

    payload="$(cat)"

    if ! printf '%s' "${payload}" | jq -e 'type == "array"' >/dev/null 2>&1; then
        printf '%s' "${payload}"
        return 0
    fi

    cfg="$(_pt_cfg)" || { printf '%s\n' "${payload}"; return 0; }
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
    prefix="$(docs_research_prefix "${cfg}")" || prefix=''
    dependabot_yml="$(_pt_dependabot_yml "${cfg}")"
    mapfile -t jq_args < <(_pt_jq_args)

    nums="$(printf '%s' "${payload}" | jq -r --arg author "${PR_TRIAGE_AUTHOR:-}" \
        "${jq_args[@]}" "${PR_TRIAGE_JQ_DEFS}"'
        .[]
        | select((.state // "OPEN") == "OPEN")
        | select((.isDraft // false) | not)
        | select(ours)
        | select(author_ok)
        | select(dependabot_pr | not)
        | (labels_of) as $lbls
        | select(($lbls | index($l_revise) | not)
             and ($lbls | index($l_revise_failed) | not)
             and ($lbls | index($l_rebase_failed) | not))
        | select((.mergeable // "UNKNOWN") != "CONFLICTING")
        | .number' 2>/dev/null || echo '')"

    # Dependabot candidates are selected separately: they are ours by a
    # different author test, they are worth probing even when CONFLICTING (the
    # lane needs to know whether the branch carries foreign commits before it
    # can decide between an `@dependabot rebase` nudge and rebasing itself), and
    # their round trip asks for different json. A parked (AFK:deps-failed) PR
    # and a HITL PR the maintainer has not approved are both invisible to the
    # pick, so they are not probed either; an invisible PR costs no API call.
    #
    # A listing with no headRefOid cannot support a Dependabot verdict at all
    # (the markers are keyed to the head sha, and the lane merges that exact
    # sha), so such an item is not probed: paying a round trip for a
    # classification that can never be picked is pure API cost.
    #
    # EVERY visible candidate is probed, with no cap. A cap would truncate the
    # candidate list before the classification exists, and the ranking the
    # Spec mandates (security before version, then oldest) can only be
    # computed AFTER enrichment: with an oldest-first window a newer security
    # bump would stay unclassified, hence unpickable, and an older version
    # bump would be picked ahead of it. Cost is bounded instead by how few bot
    # PRs survive the filters above and by the lane landing one PR per Fire.
    deps_nums="$(printf '%s' "${payload}" | jq -r "${jq_args[@]}" \
        "${PR_TRIAGE_JQ_DEFS}"'
        [ .[]
          | select((.state // "OPEN") == "OPEN")
          | select((.isDraft // false) | not)
          | select(ours)
          | select(dependabot_pr)
          | select((.headRefOid // "") != "")
          | (labels_of) as $lbls
          | select($lbls | index($l_deps_failed) | not)
          | select(($lbls | index($l_hitl) | not)
               or ((.reviewDecision // "") == "APPROVED")) ]
        | sort_by(.createdAt // "")
        | .[].number' 2>/dev/null || echo '')"

    for num in ${deps_nums}; do
        payload="$(printf '%s' "${payload}" | _pr_triage_enrich_deps_one "${num}" "${slug}" "${dependabot_yml}")"
    done

    for num in ${nums}; do
        fields='{}'

        # ONE round trip for both signals: bot-tail comments (reason
        # "incomplete") and the changed-file list (reason "docs-merge").
        view="$("${gh}" pr view "${num}" --repo "${slug}" --json comments,files 2>/dev/null)" || continue

        review_done="$(printf '%s' "${view}" | jq \
            'any(.comments[]?; .body | contains("<!-- pr-review-done"))' 2>/dev/null)"
        verify_done="$(printf '%s' "${view}" | jq \
            'any(.comments[]?; .body | test("Manual verification — .*round"))' 2>/dev/null)"
        if [ -n "${review_done}" ] && [ -n "${verify_done}" ]; then
            fields="$(printf '%s' "${fields}" | jq -c \
                --argjson r "${review_done}" --argjson v "${verify_done}" \
                '. + {reviewDone: $r, verifyDone: $v}' 2>/dev/null || printf '%s' "${fields}")"
        fi

        # Docs-only signal (reason "docs-merge"): the shared rule over the
        # changed paths. Absent on any gh/jq error or a view without a file
        # list, so the pick reads it as false and a broken sensor never
        # auto-merges.
        docs_only=''
        if [ -n "${prefix}" ] \
            && printf '%s' "${view}" | jq -e '(.files | type) == "array"' >/dev/null 2>&1; then
            docs_only="$(printf '%s' "${view}" | jq -c '[.files[] | .path]' 2>/dev/null \
                | docs_research_only "${prefix}")"
        fi
        if [ "${docs_only}" = "true" ] || [ "${docs_only}" = "false" ]; then
            fields="$(printf '%s' "${fields}" | jq -c --argjson d "${docs_only}" \
                '. + {docsOnly: $d}' 2>/dev/null || printf '%s' "${fields}")"
        fi

        [ "${fields}" = "{}" ] && continue
        merged="$(printf '%s' "${payload}" | jq -c \
            --argjson n "${num}" --argjson f "${fields}" \
            'map(if .number == $n then . + $f else . end)' 2>/dev/null)" || continue
        [ -n "${merged}" ] && payload="${merged}"
    done

    printf '%s\n' "${payload}"
    return 0
}

# pr_triage_pick: read the PR-list JSON on stdin, print the verdict JSON.
pr_triage_pick() {
    local payload verdict
    local -a jq_args

    payload="$(cat)"

    if ! printf '%s' "${payload}" | jq -e 'type == "array"' >/dev/null 2>&1; then
        printf '{"pr":null}\n'
        return 1
    fi
    mapfile -t jq_args < <(_pt_jq_args)

    verdict="$(printf '%s' "${payload}" | jq -c --arg author "${PR_TRIAGE_AUTHOR:-}" \
        "${jq_args[@]}" "${PR_TRIAGE_JQ_DEFS}"'
        [ .[]
          | select((.state // "OPEN") == "OPEN")
          | select((.isDraft // false) | not)
          | select(ours)
          | select(author_ok)
          | (labels_of) as $lbls
          | select(($lbls | index($l_revise_failed) | not)
                and ($lbls | index($l_rebase_failed) | not)
                and ($lbls | index($l_deps_failed) | not))
          | select((dependabot_pr | not)
               or ($lbls | index($l_hitl) | not)
               or ((.reviewDecision // "") == "APPROVED"))
          | . + { reason:
                    (if dependabot_pr then
                       (if (.mergeable // "UNKNOWN") == "CONFLICTING" then "conflict"
                        elif ((.depsSecurity | type) == "boolean")
                         and ((.depsMajor | type) == "boolean") then "dependabot"
                        else null end)
                     elif ($lbls | index($l_revise)) then "revise"
                     elif (.mergeable // "UNKNOWN") == "CONFLICTING" then "conflict"
                     elif (.docsOnly == true) then "docs-merge"
                     elif (((.reviewDone != false) and (.verifyDone != false)) | not) then "incomplete"
                     else null end) }
          | select(.reason != null) ]
        | sort_by([(if dependabot_pr then
                      # EVERY Bot PR ranks below EVERY Agent PR, whichever
                      # reason it earned: a human waiting on their own review is
                      # never queued behind a bot, and a CONFLICTING Bot PR
                      # sharing the name "conflict" must not borrow the Agent
                      # rank-1 slot. Within the bot block a conflicting PR comes
                      # first: nothing else can be done with the branch until
                      # it is rebased.
                      (if .reason == "conflict" then 4 else 5 end)
                    elif .reason == "revise" then 0
                    elif .reason == "conflict" then 1
                    elif .reason == "docs-merge" then 2
                    elif .reason == "incomplete" then 3
                    else 6 end),
                   (if .reason == "dependabot" and (.depsSecurity != true)
                    then 1 else 0 end),
                   .createdAt])
        | first
        | if . == null then {pr: null}
          elif .reason == "dependabot" then
            # issue is pinned to null, never issue_of: a Bot PR has no backing
            # ticket, and a #123 appearing in bot text (a changelog entry, the
            # upstream PR that fixed the CVE) belongs to another repo or another
            # ticket entirely. A caller that took it would lock and comment on
            # an unrelated issue.
            { pr: .number,
              branch: .headRefName,
              issue: null,
              reason: .reason,
              security: (.depsSecurity == true),
              major: (.depsMajor == true),
              sha: (.headRefOid // null),
              tierA: (.tierA == true),
              tierB: (.tierB == true),
              attempts: (.fixAttempts // 0) }
          elif dependabot_pr then
            # Same rule as above: a Bot PR has no backing ticket, so issue is
            # null whichever reason it earned.
            { pr: .number,
              branch: .headRefName,
              issue: null,
              reason: .reason,
              # Absent (the probe failed, or the whole view was unreadable) is
              # NOT false: an unknown branch is treated as agent-owned so the
              # lane rebases it itself instead of posting a nudge Dependabot
              # would refuse. Only an explicit false says "bot commits only".
              agentCommits: (.agentCommits != false) }
          else { pr: .number,
                 branch: .headRefName,
                 issue: issue_of,
                 reason: .reason }
          end' 2>/dev/null)"

    if [ -z "${verdict}" ] || [ "$(printf '%s' "${verdict}" | jq -r '.pr')" = "null" ]; then
        printf '{"pr":null}\n'
        return 1
    fi

    printf '%s\n' "${verdict}"
    return 0
}

# pr_triage_bot_verdict_unworkable <verdict-json>: exit 0 when the verdict
# names a Bot PR the harness can classify but this Target Project cannot work.
#
# THE ONE PLACE the "is the deps lane on?" decision lives. Both callers
# (pickup-triage.sh, work-probe.sh) ask this predicate rather than re-deriving
# the test, so the Fire and the probe can never disagree about a bot PR (a
# disagreement would wake the Daemon every chunk for a PR the Fire then skips).
#
# Optional lanes are on when the Harness config declares them: the deps-land
# lane owns reason "dependabot" and, for a Bot PR, reason "conflict" too (it
# posts `@dependabot rebase` instead of force-pushing Dependabot's own branch).
# So a Bot PR verdict is unworkable exactly when `lanes.deps_land.enabled` is
# not true, and every Agent PR verdict is always workable. With no config the
# lane reads as off, so nothing bot-shaped is ever picked unconfigured.
pr_triage_bot_verdict_unworkable() {
    local verdict="${1:-}" cfg
    [ -n "${verdict}" ] || return 1
    printf '%s' "${verdict}" | jq -e --arg deps "${HARNESS_BRANCH_DEPENDABOT_PREFIX}" '
        type == "object"
        and ((.reason == "dependabot")
             or (.reason == "conflict" and ((.branch // "") | startswith($deps))))' \
        >/dev/null 2>&1 || return 1
    cfg="$(harness_config_resolve 2>/dev/null)" || return 0
    if printf '%s' "${cfg}" | jq -e '.lanes.deps_land.enabled == true' >/dev/null 2>&1; then
        return 1
    fi
    return 0
}
