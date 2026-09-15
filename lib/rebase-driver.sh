#!/usr/bin/env bash
# rebase-driver.sh: the Rebase Driver: rebase a PR branch onto the default
# branch and push it back, safely.
#
# Sourceable library wrapping the git mechanics of the reconcile round's
# conflict-fix, behind four functions that each emit a compact JSON verdict on
# stdout. The driver is the ONLY sanctioned force-push site in the Daemon, and
# even here it is `--force-with-lease` exclusively: the lease guarantees the
# push is refused if anyone moved the remote branch after our fetch, so a
# concurrent human push can never be clobbered.
#
#   rebase_onto <branch> [base]
#       base defaults to origin/<default branch>, the branch the Harness
#       config loader detected from GitHub (ADR 0002); no branch name is
#       assumed here. Fetches, checks the branch out, rebases onto the base.
#       -> { "status": "CLEAN" }                       rebase completed
#       -> { "status": "CONFLICT", "files": [...] }    stopped; conflicted paths
#         On CONFLICT the rebase is left IN PROGRESS so the caller's
#         implementer can resolve in place, then drive rebase_continue.
#       -> { "status": "ERROR", "detail": "..." }      fetch/checkout failed,
#         or no base was given and no Harness config resolves.
#
#   rebase_continue
#       After resolutions are staged (`git add`). A multi-commit rebase may
#       stop more than once: call again per stop.
#       -> { "status": "CLEAN" } | { "status": "CONFLICT", "files": [...] }
#
#   rebase_abort
#       Restores the branch to its pre-rebase state.
#       -> { "status": "ABORTED" }
#
#   rebase_push <branch>
#       -> { "status": "PUSHED" } | { "status": "REJECTED" }
#       REJECTED = the lease failed (remote moved since fetch) or the push was
#       otherwise refused; the caller escalates instead of retrying harder.
#
# Inputs:
#   the Harness config   through harness_config_resolve (HARNESS_CONFIG_JSON,
#                        or AUTO_AGENT_TARGET_DIR): repo.default_branch
#   GIT_BIN              git CLI (default: git), injectable for composition;
#                        the test suite drives the driver against real
#                        throwaway repositories, because rebase/lease
#                        semantics are exactly the behavior under test.

_rebase_driver_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=harness-config.sh
. "${_rebase_driver_lib_dir}/harness-config.sh"

GIT_BIN="${GIT_BIN:-git}"

# List currently conflicted paths as a JSON array.
_rd_conflicted_files() {
    "${GIT_BIN}" diff --name-only --diff-filter=U 2>/dev/null \
        | jq -R -s -c 'split("\n") | map(select(length > 0))'
}

# _rd_default_base -> "origin/<default branch>" from the config, or return 1.
_rd_default_base() {
    local cfg branch
    cfg="$(harness_config_resolve 2>/dev/null)" || return 1
    branch="$(printf '%s' "${cfg}" | jq -r '.repo.default_branch // empty' 2>/dev/null)" || return 1
    [ -n "${branch}" ] || return 1
    printf 'origin/%s\n' "${branch}"
}

# rebase_onto <branch> [base]
rebase_onto() {
    local branch="$1" base="${2:-}"

    if [ -z "${base}" ]; then
        base="$(_rd_default_base)" || {
            printf '{"status":"ERROR","detail":"no Harness config"}\n'
            return 1
        }
    fi

    "${GIT_BIN}" fetch --quiet origin 2>/dev/null || {
        printf '{"status":"ERROR","detail":"fetch failed"}\n'
        return 1
    }
    "${GIT_BIN}" checkout --quiet "${branch}" 2>/dev/null || {
        printf '{"status":"ERROR","detail":"cannot checkout %s"}\n' "${branch}"
        return 1
    }

    if "${GIT_BIN}" rebase "${base}" >/dev/null 2>&1; then
        printf '{"status":"CLEAN"}\n'
        return 0
    fi

    # Rebase stopped. Distinguish a conflict stop (resolvable, left in
    # progress) from any other failure (abort and report).
    local files
    files="$(_rd_conflicted_files)"
    if [ "${files}" != "[]" ] && [ -n "${files}" ]; then
        printf '{"status":"CONFLICT","files":%s}\n' "${files}"
        return 0
    fi

    "${GIT_BIN}" rebase --abort >/dev/null 2>&1 || true
    printf '{"status":"ERROR","detail":"rebase failed without conflicts"}\n'
    return 1
}

# rebase_continue: resolutions must already be staged.
rebase_continue() {
    if GIT_EDITOR=true "${GIT_BIN}" rebase --continue >/dev/null 2>&1; then
        printf '{"status":"CLEAN"}\n'
        return 0
    fi

    local files
    files="$(_rd_conflicted_files)"
    if [ "${files}" != "[]" ] && [ -n "${files}" ]; then
        printf '{"status":"CONFLICT","files":%s}\n' "${files}"
        return 0
    fi

    printf '{"status":"ERROR","detail":"rebase --continue failed without conflicts"}\n'
    return 1
}

# rebase_abort
rebase_abort() {
    "${GIT_BIN}" rebase --abort >/dev/null 2>&1 || true
    printf '{"status":"ABORTED"}\n'
    return 0
}

# rebase_push <branch>: force-with-lease ONLY; a plain --force is never used.
rebase_push() {
    local branch="$1"
    if "${GIT_BIN}" push --force-with-lease origin "${branch}" >/dev/null 2>&1; then
        printf '{"status":"PUSHED"}\n'
        return 0
    fi
    printf '{"status":"REJECTED"}\n'
    return 1
}
