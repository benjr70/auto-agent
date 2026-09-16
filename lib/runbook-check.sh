#!/usr/bin/env bash
# runbook-check.sh: assert the plugin's skills, agents and hooks still carry
# their load-bearing rules, and carry no Target Project literal (issue #28;
# the resolve lane and planning skills' rules, issue #29).
#
# Why this exists: a SKILL.md is not documentation, it is the program an agent
# executes. Nothing compiles it, so a well-meant rewrite can delete the
# sentence that says a bot PR is labelled `AFK:deps-failed`, or paste a repo
# slug, a `master`, a port or an app name back into a prompt that must serve
# any Target Project (ADR 0002: every repo fact comes from the Harness
# config). This check makes both kinds of edit loud. Carried over from
# Smart-Smoker-V2's pr-watch-runbook-check.sh and check-harness-runbook.sh,
# generalised to one rule table for the whole plugin plus a forbidden-literal
# table (AC 2 of issue #28).
#
# Usage:
#   runbook-check.sh [PLUGIN_DIR]
#   runbook-check.sh --list
#
# Default PLUGIN_DIR: the plugin beside this lib (<install>/plugin), so it
# works from any cwd. `--list` prints both tables, one entry per line:
#   rule<TAB><file>: <rule-id><TAB><pattern>        a phrase that must be present
#   literal<TAB><literal-id><TAB><pattern><TAB><sample>   a pattern that must be absent
# The machine-readable interface tests (and humans) use to enumerate them.
#
# Matching is done over a whitespace-normalized copy of each file (leading
# blockquote markers stripped, newlines joined), so a prose re-wrap can never
# break a multi-word rule phrase. Patterns are case-insensitive extended
# regular expressions. A rule with several patterns must satisfy ALL of them.
#
# Exit codes:
#   0  every rule present, no forbidden literal
#   1  at least one rule missing or literal found (each is reported)
#   2  usage error / plugin dir or a ruled file not found

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_PLUGIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)/plugin"

# The load-bearing rules: `<file>: <rule-id><TAB><pattern>`, file relative to
# the plugin dir. Each pattern is a phrase from the runbook, not a lone
# keyword, so the check fails when the RULE goes away rather than when a word
# is reused elsewhere.
rule_table() {
    printf '%s\n' \
        "skills/afk-pickup/SKILL.md: machine-no-work	afk-pickup: no eligible issue" \
        "skills/afk-pickup/SKILL.md: machine-skip	afk-pickup: skip" \
        "skills/afk-pickup/SKILL.md: picked-line	picked: #<N> <title>" \
        "skills/afk-pickup/SKILL.md: reconcile-line	picked: reconcile PR #<P> \(issue #<N" \
        "skills/afk-pickup/SKILL.md: resolve-marker	resolve: #<N> <(research|task)" \
        "skills/afk-pickup/SKILL.md: dry-run-lines	afk-pickup: would-pick #<N>" \
        "skills/afk-pickup/SKILL.md: dry-run-lines	afk-pickup: would-resume #<N>" \
        "skills/afk-pickup/SKILL.md: dry-run-lines	afk-pickup: would-resolve #<N>" \
        "skills/afk-pickup/SKILL.md: dry-run-lines	afk-pickup: would-reconcile PR #<P>" \
        "skills/afk-pickup/SKILL.md: dry-run-lines	afk-pickup: would-fail #<N>" \
        "skills/afk-pickup/SKILL.md: output-discipline	reads ONLY the text you write in your own assistant messages" \
        "skills/afk-pickup/SKILL.md: lock-label	--add-label AFK:in-progress" \
        "skills/afk-pickup/SKILL.md: one-unit-of-work	one Fire = (at most )?one" \
        "skills/afk-pickup/SKILL.md: chain-dispatch	/auto-agent:afk-dispatch --issue" \
        "skills/afk-pickup/SKILL.md: chain-pr-watch	/auto-agent:pr-watch" \
        "skills/afk-pickup/SKILL.md: chain-pr-review	/auto-agent:pr-review" \
        "skills/afk-pickup/SKILL.md: chain-pr-reconcile	/auto-agent:pr-reconcile" \
        "skills/afk-pickup/SKILL.md: chain-verify-pr	/auto-agent:verify-pr" \
        "skills/afk-pickup/SKILL.md: smoke-gate	smoke: FAIL" \
        "skills/afk-pickup/SKILL.md: bootstrap-state	AFK:verify-human" \
        "skills/afk-pickup/SKILL.md: merge-recipe	--squash --admin --match-head-commit" \
        "skills/afk-pickup/SKILL.md: base-from-config	origin/\\\$BASE" \
        "skills/afk-pickup/SKILL.md: pr-base-from-config	--base \"?\\\$BASE" \
        "skills/afk-pickup/SKILL.md: never-merges-agent-pr	(never merge[sd]? (an |the |a )?(agent )?PR|merge (is|stays|remains) human-gated|never merged by the machine)" \
        "skills/afk-pickup/SKILL.md: triage-call	pickup-triage" \
        "skills/afk-pickup/SKILL.md: token-accounting	token-usage post --issue" \
        "skills/afk-dispatch/SKILL.md: smoke-trailer	smoke: (PASS|FAIL|SKIPPED)" \
        "skills/afk-dispatch/SKILL.md: reviewer-subagent	auto-agent:reviewer" \
        "skills/afk-dispatch/SKILL.md: verifier-subagent	auto-agent:verifier" \
        "skills/afk-dispatch/SKILL.md: review-state	review-state\.json" \
        "skills/afk-dispatch/SKILL.md: review-verdicts	change-request" \
        "skills/afk-dispatch/SKILL.md: review-verdicts	approved" \
        "skills/afk-dispatch/SKILL.md: plan-gate	plan_gated_paths" \
        "skills/afk-dispatch/SKILL.md: done-label	--add-label AFK:done" \
        "skills/afk-dispatch/SKILL.md: failed-label	--add-label AFK:failed" \
        "skills/afk-dispatch/SKILL.md: tdd	red-green" \
        "skills/afk-dispatch/SKILL.md: closes-line	Closes #" \
        "skills/afk-dispatch/SKILL.md: test-from-config	commands\.test" \
        "skills/afk-dispatch/SKILL.md: no-lock-sweep	(never|do not|never).{0,40}sweep" \
        "skills/pr-watch/SKILL.md: bot-flag	--bot" \
        "skills/pr-watch/SKILL.md: bot-flag	dependabot/" \
        "skills/pr-watch/SKILL.md: bot-verdict-pass	pr-watch: PASS — all checks green at attempt <K> \(bot\)" \
        "skills/pr-watch/SKILL.md: bot-verdict-draft	pr-watch: DRAFT — exhausted \\\$DEPS_CAP attempts, marked draft, AFK:deps-failed" \
        "skills/pr-watch/SKILL.md: bot-label	--add-label AFK:deps-failed" \
        "skills/pr-watch/SKILL.md: bot-label	never .{0,60}AFK:checks-failed" \
        "skills/pr-watch/SKILL.md: default-label	--add-label AFK:checks-failed" \
        'skills/pr-watch/SKILL.md: default-verdict	pr-watch: PASS — all checks green at attempt <K>`' \
        "skills/pr-watch/SKILL.md: default-verdict	pr-watch: DRAFT — exhausted \\\$MAX_ROUNDS rounds, marked draft, AFK:checks-failed" \
        "skills/pr-watch/SKILL.md: head-sha-key	Markers are keyed to the .{0,4}PR head sha" \
        "skills/pr-watch/SKILL.md: no-fallback-budget	never a full budget" \
        "skills/pr-watch/SKILL.md: bot-cap	deps_lane_marker_parse" \
        "skills/pr-watch/SKILL.md: bot-cap	deps_lane_rounds_left" \
        "skills/pr-watch/SKILL.md: bot-cap	MAX_ROUNDS.{0,2} is 0" \
        "skills/pr-watch/SKILL.md: fix-marker	deps_lane_marker_emit fix-attempt" \
        "skills/pr-watch/SKILL.md: fix-marker	one marker comment per fix round" \
        "skills/pr-watch/SKILL.md: skip-trailer	\[dependabot skip\]" \
        "skills/pr-watch/SKILL.md: lockfile-from-config	lockfile_refresh" \
        "skills/pr-watch/SKILL.md: cap-from-config	rounds\.pr_watch" \
        "skills/pr-watch/SKILL.md: ci-wait	ci-wait --pr" \
        "skills/pr-watch/SKILL.md: never-merges	never merges the PR" \
        "skills/pr-watch/SKILL.md: never-force	never force-push" \
        "skills/pr-review/SKILL.md: verdict-pass	pr-review: PASS — 0 findings" \
        "skills/pr-review/SKILL.md: verdict-done	pr-review: DONE — <N> findings posted, AFK:revise applied" \
        "skills/pr-review/SKILL.md: verdict-skipped	pr-review: SKIPPED — already reviewed" \
        "skills/pr-review/SKILL.md: verdict-error	pr-review: ERROR" \
        "skills/pr-review/SKILL.md: done-marker	rp_done_marker_present" \
        "skills/pr-review/SKILL.md: done-marker	rp_post_done_marker" \
        "skills/pr-review/SKILL.md: revise-label	--add-label AFK:revise" \
        "skills/pr-review/SKILL.md: findings-contract	PR_REVIEW_FINDINGS_BEGIN" \
        "skills/pr-review/SKILL.md: never-merges	never merges the PR" \
        "skills/pr-review/SKILL.md: never-fixes	never fixes its own findings" \
        "skills/pr-review/SKILL.md: correctness-axis	/code-review" \
        "skills/pr-reconcile/SKILL.md: rebase-driver	rebase_onto" \
        "skills/pr-reconcile/SKILL.md: rebase-driver	rebase_push" \
        "skills/pr-reconcile/SKILL.md: lease-only	--force-with-lease" \
        "skills/pr-reconcile/SKILL.md: parked-labels	AFK:rebase-failed" \
        "skills/pr-reconcile/SKILL.md: parked-labels	AFK:revise-failed" \
        "skills/pr-reconcile/SKILL.md: thread-reconciler	tr_unresolved_threads" \
        "skills/pr-reconcile/SKILL.md: thread-reconciler	tr_reply" \
        "skills/pr-reconcile/SKILL.md: thread-reconciler	tr_resolve" \
        "skills/pr-reconcile/SKILL.md: verdicts	pr-reconcile: PASS" \
        "skills/pr-reconcile/SKILL.md: verdicts	pr-reconcile: REBASE-FAILED" \
        "skills/pr-reconcile/SKILL.md: verdicts	pr-reconcile: REVISE-FAILED" \
        "skills/pr-reconcile/SKILL.md: verdicts	pr-reconcile: DRAFT" \
        "skills/pr-reconcile/SKILL.md: verdicts	pr-reconcile: ERROR" \
        "skills/pr-reconcile/SKILL.md: label-drop	--remove-label AFK:revise" \
        "skills/pr-reconcile/SKILL.md: cap-from-config	rounds\.revise" \
        "skills/pr-reconcile/SKILL.md: missing-round	verify: MISSING" \
        "skills/pr-reconcile/SKILL.md: never-merges	never merges the PR" \
        "skills/afk-resolve/SKILL.md: marker-line	resolve: #<N> <research\|task> <slug>" \
        "skills/afk-resolve/SKILL.md: docs-merge-marker	docs-merge: PR #<P> <sha>" \
        "skills/afk-resolve/SKILL.md: terminal-research	resolve: DONE — #<N> closed, PR #<P> merged <sha>" \
        "skills/afk-resolve/SKILL.md: terminal-task	resolve: DONE — #<N> closed \(task\)" \
        "skills/afk-resolve/SKILL.md: terminal-hitl	resolve: DONE — #<N> relabelled HITL \(needs code\)" \
        "skills/afk-resolve/SKILL.md: terminal-failed	resolve: FAILED — #<N> <reason>" \
        "skills/afk-resolve/SKILL.md: warn-line	resolve: WARN — map #<MAP_N> append failed" \
        "skills/afk-resolve/SKILL.md: pr-marker	<!-- afk-resolve ticket:#<N> map:#<MAP_N> slug:<SLUG> -->" \
        "skills/afk-resolve/SKILL.md: finish-merged	--finish-merged --pr" \
        "skills/afk-resolve/SKILL.md: dry-run-open	afk-resolve: would-open PR research/<slug>" \
        "skills/afk-resolve/SKILL.md: dry-run-skip	afk-resolve: would-skip #<N>" \
        "skills/afk-resolve/SKILL.md: dry-run-fail	afk-resolve: would-fail #<N>" \
        "skills/afk-resolve/SKILL.md: output-discipline	reads ONLY the text you write in your own assistant messages" \
        "skills/afk-resolve/SKILL.md: findings-path	RESEARCH_PREFIX\}\\\$\{MAP_SLUG\}/\\\$\{SLUG\}\.md" \
        "skills/afk-resolve/SKILL.md: branch-from-base	research/\\\$SLUG\"? \"?origin/\\\$BASE" \
        "skills/afk-resolve/SKILL.md: pr-base-from-config	--base \"?\\\$BASE" \
        "skills/afk-resolve/SKILL.md: research-skill	auto-agent:research" \
        "skills/afk-resolve/SKILL.md: chain-pr-watch	/auto-agent:pr-watch" \
        "skills/afk-resolve/SKILL.md: gate-decides	docs-only-gate --head \"?\\\$HEAD_SHA\"? --pr \"?\\\$PR\"? --check-state" \
        "skills/afk-resolve/SKILL.md: gate-merges	\.mergeCmd" \
        "skills/afk-resolve/SKILL.md: never-hand-merge	never hand-roll a .{0,4}gh pr merge" \
        "skills/afk-resolve/SKILL.md: lock-label	--add-label AFK:in-progress" \
        "skills/afk-resolve/SKILL.md: failed-label	--add-label AFK:failed" \
        "skills/afk-resolve/SKILL.md: done-label	--add-label AFK:done" \
        "skills/afk-resolve/SKILL.md: never-paused	never apply .{0,4}AFK:paused" \
        "skills/afk-resolve/SKILL.md: never-map-scope	never edit the Map.{0,4}s Destination or Out of scope" \
        "skills/afk-resolve/SKILL.md: fog-cap	at most .{0,4}3.{0,4} new tickets per resolve" \
        "skills/afk-resolve/SKILL.md: fog-parentage	sub_issues" \
        "skills/afk-resolve/SKILL.md: fog-blocking	dependencies/blocked_by" \
        "skills/afk-resolve/SKILL.md: fog-provenance	Spawned by #<N>" \
        "skills/afk-resolve/SKILL.md: fog-publish	pick-publish publish --issue" \
        "skills/afk-resolve/SKILL.md: hitl-unpublish	pick-publish unpublish --issue" \
        "skills/afk-resolve/SKILL.md: hitl-relabel	--remove-label AFK --remove-label AFK:in-progress --add-label HITL" \
        "skills/afk-resolve/SKILL.md: no-recursion	never resolve a ticket you just created" \
        "skills/afk-resolve/SKILL.md: refuse-hitl-types	wayfinder:grilling.{0,40}wayfinder:prototype.{0,120}never" \
        "skills/wayfinder/SKILL.md: map-label	wayfinder:map" \
        "skills/wayfinder/SKILL.md: map-body	## Decisions so far" \
        "skills/wayfinder/SKILL.md: map-body	## Not yet specified" \
        "skills/wayfinder/SKILL.md: map-body	## Out of scope" \
        "skills/wayfinder/SKILL.md: claim-first	--add-assignee @me" \
        "skills/wayfinder/SKILL.md: sub-issues	sub_issues" \
        "skills/wayfinder/SKILL.md: native-blocking	dependencies/blocked_by" \
        "skills/wayfinder/SKILL.md: database-id	--jq \.id" \
        "skills/wayfinder/SKILL.md: frontier	issue_dependencies_summary\.blocked_by > 0" \
        "skills/wayfinder/SKILL.md: labels-ensure	labels-ensure" \
        "skills/wayfinder/SKILL.md: no-force	never[^.]{0,40}--force" \
        "skills/wayfinder/SKILL.md: publish	pick-publish publish --issue" \
        "skills/wayfinder/SKILL.md: hitl-never-published	HITL.{0,80}never published" \
        "skills/wayfinder/SKILL.md: quiz-by-shape	PICK_SHAPE.{0,40}project" \
        "skills/wayfinder/SKILL.md: chain-resolve	/auto-agent:afk-resolve" \
        "skills/wayfinder/SKILL.md: chain-to-spec	/auto-agent:to-spec" \
        "skills/wayfinder/SKILL.md: chain-to-tickets	/auto-agent:to-tickets" \
        "skills/wayfinder/SKILL.md: grilling-skill	auto-agent:grilling" \
        "skills/wayfinder/SKILL.md: domain-skill	auto-agent:domain-modeling" \
        "skills/wayfinder/SKILL.md: one-per-session	never resolve more than one ticket per session" \
        "skills/wayfinder/SKILL.md: plan-dont-do	produce decisions, not deliverables" \
        "skills/wayfinder/SKILL.md: refer-by-name	never by a bare id, number, or slug" \
        "skills/to-spec/SKILL.md: spec-label	--label spec" \
        "skills/to-spec/SKILL.md: labels-ensure	labels-ensure" \
        "skills/to-spec/SKILL.md: no-force	never[^.]{0,40}--force" \
        "skills/to-spec/SKILL.md: never-afk	never.{0,20}labelled .{0,4}AFK" \
        "skills/to-spec/SKILL.md: never-published	never (put on the pick signal|published)" \
        "skills/to-spec/SKILL.md: sub-issue-of-map	sub_issues" \
        "skills/to-spec/SKILL.md: autonomous	Running autonomously" \
        "skills/to-spec/SKILL.md: template	## Module design" \
        "skills/to-spec/SKILL.md: template	## User Stories" \
        "skills/to-spec/SKILL.md: chain-to-tickets	/auto-agent:to-tickets" \
        "skills/to-tickets/SKILL.md: dry-run	--dry-run" \
        "skills/to-tickets/SKILL.md: labels-ensure	labels-ensure" \
        "skills/to-tickets/SKILL.md: no-force	never[^.]{0,40}--force" \
        "skills/to-tickets/SKILL.md: afk-label	--label AFK" \
        "skills/to-tickets/SKILL.md: hitl-label	--label HITL" \
        "skills/to-tickets/SKILL.md: template	## Acceptance criteria" \
        "skills/to-tickets/SKILL.md: template	## Behaviors to test" \
        "skills/to-tickets/SKILL.md: template	## Blocked by" \
        "skills/to-tickets/SKILL.md: no-spawned-by	Never .{0,4}Spawned by.{0,4} on a Slice" \
        "skills/to-tickets/SKILL.md: native-blocking	dependencies/blocked_by" \
        "skills/to-tickets/SKILL.md: sub-issues	sub_issues" \
        "skills/to-tickets/SKILL.md: database-id	--jq \.id" \
        "skills/to-tickets/SKILL.md: publish-by-shape	pick-publish publish --issue <N> --priority <P>" \
        "skills/to-tickets/SKILL.md: label-only-noop	label-only.{0,120}no-op" \
        "skills/to-tickets/SKILL.md: priority-edit-failed	priority-edit-failed" \
        "skills/to-tickets/SKILL.md: hitl-never-published	HITL.{0,80}never published" \
        "skills/to-tickets/SKILL.md: quiz-by-shape	PICK_SHAPE.{0,40}project" \
        "agents/implementer.md: tools	tools: Read, Edit, Write, Bash, Glob, Grep" \
        "agents/implementer.md: never-pushes	never push" \
        "agents/reviewer.md: tools	tools: Read, Grep, Glob, Bash" \
        "agents/reviewer.md: verdicts	change-request" \
        "agents/reviewer.md: verdicts	approved" \
        "agents/verifier.md: tools	tools: Read, Bash" \
        "agents/verifier.md: trailer	smoke: (PASS|FAIL|SKIPPED)" \
        "agents/verifier.md: never-guesses	never .{0,40}PASS" \
        "hooks/smoke-trailer.sh: trailer	smoke: \(PASS\|FAIL\|SKIPPED\)" \
        "hooks/smoke-trailer.sh: dispatch-scoped	review-state\.json" \
        "hooks/review-gate.sh: state-file	review-state\.json"
}

# The forbidden literals (issue #28 AC 2): `<literal-id><TAB><pattern><TAB><sample>`.
# Applied to every skill, agent and hook in the plugin. The sample is a string
# the pattern matches, for the mutation test. The harness's own fixed
# vocabulary (AFK labels, branch shapes, markers) is not a literal.
literal_table() {
    printf '%s\n' \
        "repo-slug	benjr70|smart-smoker	benjr70/Smart-Smoker-V2" \
        "default-branch-literal	(^|[^A-Za-z_/.-])master($|[^A-Za-z_-])|origin/main	git checkout master" \
        "app-name	(^|[^A-Za-z-])smoker($|[^A-Za-z-])|device-service|(^|[^A-Za-z])apps/	cd apps/backend" \
        "port-or-host	localhost:[0-9]+|127\.0\.0\.1:[0-9]+|tail[0-9a-f]+\.ts\.net|:[0-9]{4,5}([^0-9:]|$)|(^|[^A-Za-z_])PORT=[0-9]+	http://localhost:3001/api/health" \
        "smart-smoker-path	scripts/(claude-agent|smoke|ralph|verify-pr|pr-images|validate-pr-title|deployment)	scripts/claude-agent/lib/x.sh" \
        "agent-teams	agent teams?[^a-z]|teammate|CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS|(^|[^a-z])ralph	spawn a teammate" \
        "research-path-literal	docs/research/	every file under docs/research/" \
        "unnamespaced-chain	(^|[^:A-Za-z0-9_/.-])/(afk-pickup|afk-dispatch|afk-resolve|pr-watch|pr-review|pr-reconcile|verify-pr|deps-land|wayfinder|to-spec|to-tickets)([^A-Za-z0-9_/-]|$)	invoke /pr-watch now" \
        "lockfile-literal	npm install --legacy-peer-deps	npm install --legacy-peer-deps --package-lock-only" \
        "project-number-literal	Project #[0-9]|--owner [a-z0-9-]+ --format json	added to Project #1 at P1" \
        "project-hand-recipe	gh project (item-add|item-edit|field-list|view) |gh label create [^\\-]	pid=\$(gh project view 1 --owner me --format json)" \
        "skills-manager-path	~/\\.agents/skills|\\.skill-lock\\.json	installed at ~/.agents/skills/research/SKILL.md" \
        "smart-smoker-lint	validate-pr-title|release-please	bash scripts/validate-pr-title.sh"
}

# Collapse a file to a single whitespace-normalized line so wrapped prose still
# matches a multi-word phrase. Leading blockquote markers are stripped first.
normalize_file() {
    sed -E 's/^[[:space:]]*>[[:space:]]?//' "$1" | tr '\n' ' ' | tr -s '[:space:]' ' '
}

# checked_files <plugin-dir> -> every file the literal table applies to
checked_files() {
    find "$1/skills" "$1/agents" "$1/hooks" -type f \( -name 'SKILL.md' -o -name '*.md' -o -name '*.sh' -o -name '*.json' \) 2>/dev/null | sort
}

main() {
    if [ "${1:-}" = "--list" ]; then
        while IFS=$'\t' read -r spec pattern; do
            [ -n "${spec}" ] || continue
            printf 'rule\t%s\t%s\n' "${spec}" "${pattern}"
        done < <(rule_table)
        while IFS=$'\t' read -r id pattern sample; do
            [ -n "${id}" ] || continue
            printf 'literal\t%s\t%s\t%s\n' "${id}" "${pattern}" "${sample}"
        done < <(literal_table)
        return 0
    fi

    local plugin="${1:-${DEFAULT_PLUGIN_DIR}}"
    if [ ! -d "${plugin}" ]; then
        echo "runbook-check: plugin dir not found: ${plugin}" >&2
        return 2
    fi

    local checks=0 missing=0 found=0 spec pattern file rule text
    local -A cache=()
    while IFS=$'\t' read -r spec pattern; do
        [ -n "${spec}" ] || continue
        file="${spec%%: *}"; rule="${spec#*: }"
        if [ ! -f "${plugin}/${file}" ]; then
            echo "runbook-check: ruled file not found: ${plugin}/${file}" >&2
            return 2
        fi
        [ -n "${cache[${file}]+x}" ] || cache[${file}]="$(normalize_file "${plugin}/${file}")"
        text="${cache[${file}]}"
        checks=$((checks + 1))
        if ! printf '%s' "${text}" | grep -Eqi -- "${pattern}"; then
            missing=$((missing + 1))
            echo "MISSING rule=${rule} file=${file} pattern=${pattern}"
        fi
    done < <(rule_table)

    local id sample match f rel
    while IFS= read -r f; do
        rel="${f#"${plugin}"/}"
        [ -n "${cache[${rel}]+x}" ] || cache[${rel}]="$(normalize_file "${f}")"
        text="${cache[${rel}]}"
        while IFS=$'\t' read -r id pattern sample; do
            [ -n "${id}" ] || continue
            checks=$((checks + 1))
            match="$(printf '%s' "${text}" | grep -Eio -- "${pattern}" | head -1)"
            if [ -n "${match}" ]; then
                found=$((found + 1))
                echo "FORBIDDEN literal=${id} file=${rel} match=${match}"
            fi
        done < <(literal_table)
    done < <(checked_files "${plugin}")

    echo "runbook-check: ${checks} assertions, ${missing} missing, ${found} forbidden"
    if [ "${missing}" -gt 0 ] || [ "${found}" -gt 0 ]; then
        echo "The rules above are load-bearing (Spec #23, issue #28): restore a missing"
        echo "phrase in the skill text, and replace a forbidden literal with the Harness"
        echo "config value, rather than relaxing this check."
        return 1
    fi
    return 0
}

main "$@"
