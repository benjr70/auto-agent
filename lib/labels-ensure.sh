#!/usr/bin/env bash
# labels-ensure.sh: create the harness's fixed label vocabulary in the Target
# Project, create-if-missing only.
#
# Why this exists: `gh issue create --label X` fails when X does not exist,
# so every skill that publishes tickets (to-spec, to-tickets, wayfinder) used
# to carry its own bootstrap block, and each block spelled the colours and
# descriptions slightly differently. Worse, a `gh label create --force` in any
# of them rewrites the colour and description of a label that already exists,
# so two blocks flip-flop curated metadata on every run. The one label table
# lives here (the names are the constants in harness-config.sh, ADR 0002: the
# harness labels are fixed vocabulary, never Target Project config), and it
# only ever creates what is absent. Setup's config-scaffold stage runs the
# same call.
#
# Usage:
#   lib/labels-ensure.sh [--list]
#
# `--list` prints the table (name<TAB>color<TAB>description) and exits 0
# without touching gh.
#
# Output (stdout): one line, `labels-ensure: created <n>, present <m>`, after
# one `labels-ensure: created <name>` line per label created.
#
# Exit codes:
#   0  every label present (created or already there)
#   1  the label list could not be read, or a create failed (named on stderr)
#   2  no Harness config could be resolved
#
# The repo slug comes from the resolved Harness config through
# harness_config_resolve (HARNESS_CONFIG_JSON, or AUTO_AGENT_TARGET_DIR from
# the Host env).
#
# Env:
#   GH_BIN   gh CLI (default: gh), injectable for tests

set -uo pipefail

_labels_ensure_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_labels_ensure_lib_dir}/harness-config.sh"
# shellcheck source=host-env.sh
. "${_labels_ensure_lib_dir}/host-env.sh"

# The table: name<TAB>color<TAB>description, the vocabulary the core-loop
# skills shipped (issue #28) plus `spec` and the wayfinder types. Names come
# from the constants so a renamed label cannot drift between the picker and
# the bootstrap.
labels_table() {
    printf '%s\t%s\t%s\n' \
        "${HARNESS_LABEL_AFK}"           "1D76DB" "Agent-grabbable: the Daemon may pick it up" \
        "${HARNESS_LABEL_HITL}"          "5319E7" "Resolves only through live exchange with a human; never picked by the Daemon" \
        "${HARNESS_LABEL_SPEC}"          "0052CC" "Spec issue produced by to-spec; parent of Slices" \
        "${HARNESS_LABEL_IN_PROGRESS}"   "FBCA04" "Single-flight lock: a Fire is working it" \
        "${HARNESS_LABEL_DONE}"          "0E8A16" "Completed by the Daemon" \
        "${HARNESS_LABEL_FAILED}"        "B60205" "Daemon attempt failed; needs human triage" \
        "${HARNESS_LABEL_CHECKS_FAILED}" "D93F0B" "Agent PR: CI or verification failed after the fix loop was exhausted" \
        "${HARNESS_LABEL_REVISE}"        "0052CC" "Hand-back: the Daemon must address this PR's unresolved review comments" \
        "${HARNESS_LABEL_REVISE_FAILED}" "B60205" "Agent PR: review comments could not be auto-resolved (revise loop exhausted)" \
        "${HARNESS_LABEL_REBASE_FAILED}" "B60205" "Agent PR: automatic rebase onto the default branch failed; human rebase required" \
        "${HARNESS_LABEL_PAUSED}"        "FBCA04" "Fire cut off by usage exhaustion; awaiting resume next window" \
        "${HARNESS_LABEL_DEPS_FAILED}"   "B60205" "Dependabot PR: verify/fix loop exhausted; human triage required" \
        "${HARNESS_LABEL_VERIFY_HUMAN}"  "C5DEF5" "Agent PR opened in Bootstrap state: no Environment provider, a human verifies" \
        "${HARNESS_LABEL_NEEDS_HUMAN}"   "E99695" "The Daemon is parked: a human must act (dead credential, hand-off)" \
        "${HARNESS_LABEL_MAP}"           "0E8A16" "Wayfinder map issue" \
        "${HARNESS_LABEL_WAYFINDER_PREFIX}grilling"  "FBCA04" "Wayfinder grilling ticket (HITL)" \
        "${HARNESS_LABEL_WAYFINDER_PREFIX}prototype" "D4C5F9" "Wayfinder prototype ticket (HITL)" \
        "${HARNESS_LABEL_WAYFINDER_PREFIX}research"  "C5DEF5" "Wayfinder research ticket (AFK)" \
        "${HARNESS_LABEL_WAYFINDER_PREFIX}task"      "BFD4F2" "Wayfinder task ticket"
}

# labels_ensure : the whole bootstrap against the resolved config's repo
labels_ensure() {
    local gh="${GH_BIN:-gh}" cfg slug existing created=0 present=0 rc=0
    cfg="$(harness_config_resolve)" || return 2
    slug="$(printf '%s' "${cfg}" | jq -r '.repo.slug')"
    existing="$("${gh}" label list --repo "${slug}" --limit 200 --json name --jq '.[].name' 2>/dev/null)" || {
        echo "labels-ensure: cannot read the labels of ${slug}" >&2
        return 1
    }
    local name color desc
    while IFS=$'\t' read -r name color desc; do
        [ -n "${name}" ] || continue
        if printf '%s\n' "${existing}" | grep -qxF -- "${name}"; then
            present=$((present + 1))
            continue
        fi
        if "${gh}" label create "${name}" --repo "${slug}" --color "${color}" --description "${desc}" >/dev/null 2>&1; then
            created=$((created + 1))
            echo "labels-ensure: created ${name}"
        else
            echo "labels-ensure: could not create ${name}" >&2
            rc=1
        fi
    done < <(labels_table)
    echo "labels-ensure: created ${created}, present ${present}"
    return "${rc}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    case "${1:-}" in
        --list) labels_table; exit 0 ;;
        "") host_env_load; labels_ensure ;;
        *) echo "labels-ensure: unknown argument '$1'" >&2; exit 2 ;;
    esac
fi
