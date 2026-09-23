#!/usr/bin/env bash
# shellcheck disable=SC2034
# Harness config loader: the only reader of a Target Project's
# .auto-agent/harness.json (ADR 0002).
#
# Source this file, then:
#
#   harness_config_validate <harness.json>
#       Schema-validates one file with jq alone. Prints one error per line on
#       stderr and returns 1 on any error, 2 when the file is missing or not
#       JSON. Makes no network call.
#
#   harness_config_load <target-dir> [<harness.json>]
#       Validates, then prints ONE resolved JSON object on stdout for every
#       other lib to read (see "Resolved shape" below). Derives the repo from
#       the target's `origin` remote and detects the default branch from GitHub
#       through GH_BIN; never reads a branch from the file. Returns non-zero
#       before any network call when the file is invalid.
#
#       The optional second argument feeds a file from elsewhere (e.g. one
#       extracted from a PR head, ADR 0007) while still resolving the target
#       directory's repo and sibling prose files.
#
#   harness_config_check <target-dir>
#       What `bin/auto-agent check-config` runs: validate plus the static
#       cross-checks that the schema cannot express. No network.
#
#   harness_config_slug [<caller-name>]
#       The resolved config's repo slug, or 2 with a stderr line; what every
#       lib whose gh calls name the repo uses.
#
#   harness_config_resolve [<target-dir>]
#       What every other lib calls to get the resolved JSON: prints
#       HARNESS_CONFIG_JSON when the caller (the Fire, a test) already resolved
#       the config once, else loads it from <target-dir>, else from
#       AUTO_AGENT_TARGET_DIR. Returns 2 with a stderr line when none of the
#       three names a Target Project.
#
# Resolved shape (every key present, absent optionals are null or defaulted):
#
#   {
#     "config_dir": "<abs path of .auto-agent>",
#     "repo": { "owner", "name", "slug", "default_branch" },
#     "pick": { "shape": "project" | "labels",
#               "project": { "number", "priority_field", "order" } | null,
#               "labels":  {} | null },
#     "commit_scopes": [...],
#     "commands": { "install", "test", "lint", "lockfile_refresh", "plan_gated_paths": [] },
#     "docs_research_prefix": "docs/research/",
#     "required_checks": [],
#     "rounds": { "pr_watch", "manual_verify", "revise", "deps_fix", "pause_resume" },
#     "verification": { "hermetic": { "command", "smoke" } | null,
#                       "deployed": { "command", "enabled" } | null },
#     "surfaces": { "<name>": { "kind", "url_key", "paths", "viewport", "launcher" } },
#     "lanes": { "deps_land": { "present", "enabled" }, "deployed": { "present", "enabled" } },
#     "host": { "docker": false, "extension": "<abs path>" | null },
#     "prose": { "verifier_runbook", "bot_pr_checklist", "deployed_checks" }   (abs path or null)
#   }
#
# Fixed harness vocabulary (labels, branch shapes, sibling file names, the
# Host extension name) is exported as constants below so no lib re-spells it.
#
# Environment:
#   GH_BIN   (default: gh)   injected for tests; `gh repo view` detects the default branch
#   GIT_BIN  (default: git)  injected for tests; reads the origin remote
#   HARNESS_SCHEMA_DIR       overrides where harness.schema.json / validate.jq live
#   HARNESS_CONFIG_JSON      an already-resolved config; harness_config_resolve prints it
#   AUTO_AGENT_TARGET_DIR    the Target Project checkout (Host env); the resolve fallback

HARNESS_CONFIG_DIRNAME=".auto-agent"
HARNESS_CONFIG_FILENAME="harness.json"
HARNESS_HOST_EXTENSION_FILENAME="host-extension"
HARNESS_PROSE_VERIFIER_RUNBOOK="verifier-runbook.md"
HARNESS_PROSE_BOT_PR_CHECKLIST="bot-pr-checklist.md"
HARNESS_PROSE_DEPLOYED_CHECKS="deployed-checks.md"

# Read by other libs, unused here:
# shellcheck disable=SC2034
HARNESS_LABEL_AFK="AFK"
HARNESS_LABEL_IN_PROGRESS="AFK:in-progress"
HARNESS_LABEL_PAUSED="AFK:paused"
HARNESS_LABEL_NEEDS_HUMAN="AFK:needs-human"
HARNESS_LABEL_VERIFY_HUMAN="AFK:verify-human"
HARNESS_LABEL_DEPS_FAILED="AFK:deps-failed"
HARNESS_LABEL_DONE="AFK:done"
HARNESS_LABEL_FAILED="AFK:failed"
HARNESS_LABEL_REVISE="AFK:revise"
HARNESS_LABEL_REVISE_FAILED="AFK:revise-failed"
HARNESS_LABEL_REBASE_FAILED="AFK:rebase-failed"
HARNESS_LABEL_CHECKS_FAILED="AFK:checks-failed"
HARNESS_LABEL_HITL="HITL"
HARNESS_LABEL_SPEC="spec"
HARNESS_LABEL_WAYFINDER_PREFIX="wayfinder:"
HARNESS_LABEL_MAP="wayfinder:map"
# The state labels: an AFK ticket carrying any of them is not a pick candidate.
HARNESS_LABELS_STATE_JSON='["AFK:in-progress","AFK:done","AFK:failed","AFK:paused"]'
HARNESS_BRANCH_FEATURE_PREFIX="feat/issue-"
HARNESS_BRANCH_RESEARCH_PREFIX="research/"
# Dependabot's own branch shape: a GitHub fact, not a Target Project one.
HARNESS_BRANCH_DEPENDABOT_PREFIX="dependabot/"

# harness_merge_recipe <slug> <pr> <sha>
# THE admin-squash merge recipe (ADR 0002), printed as the one command a gate
# hands its caller. Every lane that lands a PR without a human (the docs-only
# gate, the deps gate) prints this and nothing else, so the single shape a
# skill may run is defined once. `--admin` is what lets the machine user past
# a required review; `--match-head-commit` pins the merge to the sha the gate
# inspected, so anything pushed between gate and merge fails the merge instead
# of landing unreviewed.
harness_merge_recipe() {
    local slug="${1:?harness_merge_recipe: repo slug required}"
    local pr="${2:?harness_merge_recipe: pr number required}"
    local sha="${3:?harness_merge_recipe: head sha required}"
    printf 'gh pr merge %s --repo %s --squash --admin --match-head-commit %s\n' "${pr}" "${slug}" "${sha}"
}

# harness_changed_paths [<pr>] : the repo-relative paths a diff changed — the
# PR's, through `gh pr diff --name-only`, or whatever the caller piped in when
# no PR number is given. Every lib that asks "which Surfaces / which config
# files did this PR touch" opens with the same two lines; they are here so the
# stdin seam every one of those libs is tested through stays one seam.
# Returns 1 when gh could not be asked.
#   GH_BIN   (default: gh)   injected for tests
harness_changed_paths() {
    local pr="${1:-}"
    if [ -n "${pr}" ]; then
        "${GH_BIN:-gh}" pr diff "${pr}" --name-only || return 1
    else
        cat
    fi
}

# harness_re_escape <text>
# Makes a literal (a branch prefix, a sha) safe inside an ERE or jq regex, so
# every lib that splices fixed vocabulary into a pattern escapes it one way.
harness_re_escape() { printf '%s' "$1" | sed 's/[][\\.^$*+?(){}|\/-]/\\&/g'; }

# harness_config_slug [<caller-name>]
# The Target Project's owner/repo from the resolved config (harness_config_resolve),
# for every lib whose gh calls name the repo. Returns 2 with a stderr line
# prefixed by <caller-name> (default harness-config) when no config resolves or
# it carries no slug, so a call site never runs gh against a guessed repo.
harness_config_slug() {
    local who="${1:-harness-config}" cfg slug
    cfg="$(harness_config_resolve 2>/dev/null)" || {
        echo "${who}: no Harness config to read the repo from" >&2
        return 2
    }
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug // empty' 2>/dev/null)"
    if [ -z "${slug}" ]; then
        echo "${who}: the Harness config carries no repo slug" >&2
        return 2
    fi
    printf '%s\n' "${slug}"
}

# The line a Deployed tier declared with `enabled: false` is reported by: what
# `deployed lane` prints (lib/deployed-tier.sh) and the Fire record's note.
HARNESS_DEPLOYED_LANE_DISABLED="deployed-lane: off — verification.deployed.enabled is false"

# harness_lane_notes <resolved-json>
# The Fire record's `notes`: a JSON array holding the disabled-lane line when
# the config declares the Deployed tier but switches it off, so a lane that is
# off by choice says so on every Fire; an undeclared lane is simply absent.
harness_lane_notes() {
    printf '%s' "${1:?harness_lane_notes: resolved config required}" | jq -c --arg off "${HARNESS_DEPLOYED_LANE_DISABLED}" '
        [ if (.lanes.deployed.present == true) and (.lanes.deployed.enabled != true) then $off else empty end ]'
}

# harness_config_target_dir <resolved-json>
# The Target Project checkout the resolved config came from: the parent of
# config_dir. What a lib needs when it must look beside `.auto-agent/` (a
# `.github/dependabot.yml`, the transcripts a checkout produced).
harness_config_target_dir() {
    local cfg="${1:?harness_config_target_dir: resolved config required}" dir
    dir="$(printf '%s' "${cfg}" | jq -r '.config_dir // empty')" || return 1
    [ -n "${dir}" ] || return 1
    printf '%s\n' "$(dirname "${dir}")"
}

_harness_config_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_SCHEMA_DIR="${HARNESS_SCHEMA_DIR:-${_harness_config_lib_dir}/../plugin/schema}"

_hc_err() { echo "harness-config: $*" >&2; }

# harness_config_validate <file>
harness_config_validate() {
    local file="${1:?harness_config_validate: file required}"
    local schema="${HARNESS_SCHEMA_DIR}/harness.schema.json"
    local program="${HARNESS_SCHEMA_DIR}/validate.jq"

    if [ ! -f "${file}" ]; then
        _hc_err "missing ${file}"
        return 2
    fi
    if ! jq . "${file}" >/dev/null 2>&1; then
        _hc_err "${file} is not valid JSON"
        return 2
    fi
    if [ ! -f "${schema}" ] || [ ! -f "${program}" ]; then
        _hc_err "schema not found under ${HARNESS_SCHEMA_DIR}"
        return 2
    fi

    local errors
    errors="$(jq -r --slurpfile schema "${schema}" -f "${program}" "${file}")" || {
        _hc_err "validator failed on ${file}"
        return 2
    }
    if [ -n "${errors}" ]; then
        _hc_err "${file} does not match the Harness config schema:"
        printf '%s\n' "${errors}" | sed 's/^/  /' >&2
        return 1
    fi
    return 0
}

# _harness_config_static_checks <file>
# Cross-field rules the schema cannot express. Prints errors, returns 1 on any.
_harness_config_static_checks() {
    local file="$1"
    local errors
    errors="$(jq -r '
        [
          (.surfaces // {} | to_entries[]
            | select(.value.kind == "electron" and (.value.launcher | not))
            | "/surfaces/\(.key): an electron surface needs a launcher"),
          (.surfaces // {} | [to_entries[] | .value.url_key] | group_by(.) | map(select(length > 1) | .[0])[]
            | "/surfaces: url_key \"\(.)\" is declared by more than one surface"),
          (if (.verification.deployed? and (.verification.hermetic? | not)) then
             "/verification/deployed: a deployed tier needs a hermetic tier to defer from"
           else empty end)
        ][]' "${file}")"
    if [ -n "${errors}" ]; then
        _hc_err "${file} fails the Harness config cross-checks:"
        printf '%s\n' "${errors}" | sed 's/^/  /' >&2
        return 1
    fi
    return 0
}

# _harness_config_precheck <target-dir> <file>
# Everything that must pass before any network call: target exists, schema,
# cross-checks, Host extension executable. Shared by check and load.
_harness_config_precheck() {
    local target="$1" file="$2"
    if [ ! -d "${target}" ]; then
        _hc_err "target dir not found: ${target}"
        return 2
    fi
    harness_config_validate "${file}" || return $?
    _harness_config_static_checks "${file}" || return $?
    local ext="${target}/${HARNESS_CONFIG_DIRNAME}/${HARNESS_HOST_EXTENSION_FILENAME}"
    if [ -e "${ext}" ] && [ ! -x "${ext}" ]; then
        _hc_err "${ext} exists but is not executable"
        return 1
    fi
    return 0
}

# harness_config_check <target-dir>
harness_config_check() {
    local target="${1:?harness_config_check: target dir required}"
    _harness_config_precheck "${target}" "${target}/${HARNESS_CONFIG_DIRNAME}/${HARNESS_CONFIG_FILENAME}"
}

# _harness_config_prose_path <config-dir> <filename> -> JSON string or null
_harness_config_prose_path() {
    if [ -f "$1/$2" ]; then jq -n --arg p "$1/$2" '$p'; else echo null; fi
}

# _harness_config_repo_slug <target-dir> -> "owner/name" from the origin remote
_harness_config_repo_slug() {
    local target="$1" url
    url="$("${GIT_BIN:-git}" -C "${target}" remote get-url origin 2>/dev/null)" || return 1
    # https://github.com/o/n(.git) | git@github.com:o/n(.git) | ssh://git@github.com/o/n(.git)
    url="${url%/}"
    url="${url%.git}"
    case "${url}" in
        *:*/*) url="${url##*:}" ;;
    esac
    url="${url#*//}"
    url="${url#*github.com/}"
    case "${url}" in
        */*/*) url="$(printf '%s' "${url}" | awk -F/ '{print $(NF-1)"/"$NF}')" ;;
    esac
    case "${url}" in
        */*) printf '%s\n' "${url}" ;;
        *) return 1 ;;
    esac
}

# _harness_config_default_branch <slug> -> branch name via gh
_harness_config_default_branch() {
    local slug="$1" branch
    branch="$("${GH_BIN:-gh}" repo view "${slug}" --json defaultBranchRef --jq .defaultBranchRef.name 2>/dev/null)" || return 1
    [ -n "${branch}" ] || return 1
    printf '%s\n' "${branch}"
}

# harness_config_load <target-dir> [<harness.json>]
harness_config_load() {
    local target="${1:?harness_config_load: target dir required}"
    local dir="${target}/${HARNESS_CONFIG_DIRNAME}"
    local file="${2:-${dir}/${HARNESS_CONFIG_FILENAME}}"

    # Fail closed before any network call.
    _harness_config_precheck "${target}" "${file}" || return $?

    local slug
    slug="$(_harness_config_repo_slug "${target}")" || {
        _hc_err "cannot derive the repo from the origin remote of ${target}"
        return 3
    }
    local branch
    branch="$(_harness_config_default_branch "${slug}")" || {
        _hc_err "cannot detect the default branch of ${slug} (is gh authenticated?)"
        return 3
    }

    local abs_dir
    abs_dir="$(cd "${dir}" 2>/dev/null && pwd)" || abs_dir="${dir}"
    local ext="${abs_dir}/${HARNESS_HOST_EXTENSION_FILENAME}"
    local ext_json='null'
    [ -x "${ext}" ] && ext_json="$(jq -n --arg p "${ext}" '$p')"
    local schema="${HARNESS_SCHEMA_DIR}/harness.schema.json"

    jq --slurpfile schema "${schema}" \
        --arg config_dir "${abs_dir}" \
        --arg owner "${slug%%/*}" \
        --arg name "${slug##*/}" \
        --arg slug "${slug}" \
        --arg branch "${branch}" \
        --argjson ext "${ext_json}" \
        --argjson runbook "$(_harness_config_prose_path "${abs_dir}" "${HARNESS_PROSE_VERIFIER_RUNBOOK}")" \
        --argjson checklist "$(_harness_config_prose_path "${abs_dir}" "${HARNESS_PROSE_BOT_PR_CHECKLIST}")" \
        --argjson deployed_checks "$(_harness_config_prose_path "${abs_dir}" "${HARNESS_PROSE_DEPLOYED_CHECKS}")" \
        '
        # Defaults live in the schema alone; the loader reads them from there.
        def P: $schema[0].properties;
        def d(p): getpath(p | split("/") | map(select(length > 0)) | map(., "properties")[:-1] + ["default"]);
        # `//` would read an explicit false as absent; opt keeps a declared false.
        def opt($k; $d): if has($k) then .[$k] else $d end;
        def opt($k; $d; $p): opt($k; P | d($p));
        def surface: {
            kind, url_key, paths,
            viewport: (.viewport // null),
            launcher: (.launcher // null)
        };
        {
          config_dir: $config_dir,
          repo: { owner: $owner, name: $name, slug: $slug, default_branch: $branch },
          pick: (
            if .pick.project then
              { shape: "project",
                project: { number: .pick.project.number,
                           priority_field: (.pick.project | opt("priority_field"; null; "pick/project/priority_field")),
                           order: (.pick.project | opt("order"; null; "pick/project/order")) },
                labels: null }
            else
              { shape: "labels",
                project: null,
                labels: {} }
            end),
          commit_scopes,
          commands: {
            install: .commands.install,
            test: .commands.test,
            lint: (.commands.lint // null),
            lockfile_refresh: (.commands.lockfile_refresh // null),
            plan_gated_paths: (.commands.plan_gated_paths // [])
          },
          docs_research_prefix: opt("docs_research_prefix"; null; "docs_research_prefix"),
          required_checks: opt("required_checks"; null; "required_checks"),
          rounds: ((.rounds // {}) | {
            pr_watch: opt("pr_watch"; null; "rounds/pr_watch"),
            manual_verify: opt("manual_verify"; null; "rounds/manual_verify"),
            revise: opt("revise"; null; "rounds/revise"),
            deps_fix: opt("deps_fix"; null; "rounds/deps_fix"),
            pause_resume: opt("pause_resume"; null; "rounds/pause_resume")
          }),
          verification: {
            hermetic: (if .verification.hermetic then
                         { command: .verification.hermetic.command, smoke: (.verification.hermetic | opt("smoke"; null; "verification/hermetic/smoke")) }
                       else null end),
            deployed: (if .verification.deployed then
                         { command: .verification.deployed.command, enabled: (.verification.deployed | opt("enabled"; null; "verification/deployed/enabled")) }
                       else null end)
          },
          surfaces: ((.surfaces // {}) | with_entries(.value |= surface)),
          lanes: {
            deps_land: { present: (.dependabot != null),
                         enabled: ((.dependabot != null) and (.dependabot | opt("enabled"; null; "dependabot/enabled"))) },
            deployed:  { present: (.verification.deployed != null),
                         enabled: ((.verification.deployed != null) and (.verification.deployed | opt("enabled"; null; "verification/deployed/enabled"))) }
          },
          host: { docker: ((.host // {}) | opt("docker"; null; "host/docker")), extension: $ext },
          prose: { verifier_runbook: $runbook, bot_pr_checklist: $checklist, deployed_checks: $deployed_checks }
        }' "${file}"
}

# harness_config_resolve_head [<target-dir>]
# The config as the CHECKOUT has it, ignoring an inherited HARNESS_CONFIG_JSON.
# A Fire resolves the config once, from the default branch, and exports it; a
# verification round runs in the PR head and must obey the config the PR
# carries (ADR 0007), so it resolves again from the checkout it is standing in.
harness_config_resolve_head() {
    local target="${1:-}"
    [ -n "${target}" ] || target="${AUTO_AGENT_TARGET_DIR:-}"
    if [ -z "${target}" ]; then
        _hc_err "no Target Project: pass <target-dir> or set AUTO_AGENT_TARGET_DIR"
        return 2
    fi
    harness_config_load "${target}"
}

# harness_config_resolve [<target-dir>]
harness_config_resolve() {
    local target="${1:-}"
    if [ -n "${HARNESS_CONFIG_JSON:-}" ]; then
        if printf '%s' "${HARNESS_CONFIG_JSON}" | jq -e 'type == "object" and has("repo") and has("pick")' >/dev/null 2>&1; then
            printf '%s\n' "${HARNESS_CONFIG_JSON}"
            return 0
        fi
        _hc_err "HARNESS_CONFIG_JSON is set but is not a resolved Harness config"
        return 2
    fi
    [ -n "${target}" ] || target="${AUTO_AGENT_TARGET_DIR:-}"
    if [ -z "${target}" ]; then
        _hc_err "no Target Project: pass <target-dir>, or set AUTO_AGENT_TARGET_DIR or HARNESS_CONFIG_JSON"
        return 2
    fi
    harness_config_load "${target}"
}
