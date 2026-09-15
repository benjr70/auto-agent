#!/usr/bin/env bash
# pr-checks.sh: the one reading of a PR's check list that stands in for a
# human review. Both merge gates (lib/docs-only-gate.sh, lib/deps-gate.sh)
# land a PR without a reviewer, so they share this verdict rather than each
# re-deriving the bucket vocabulary; lib/ci-wait.sh reads the same GitHub
# buckets from the other side (it waits on `pending`, reports `fail`).
#
# Source this file, then:
#
#   pr_checks_verdict <required-checks-json> < <gh pr checks --json name,bucket>
#       Reads the check list on stdin and prints ONE line, `<reason>\t<detail>`,
#       when the list does not vouch for the PR, or nothing when it does.
#       Exit 0 when it vouches, 1 when it does not. Reasons, in the order they
#       are tested:
#         checks-unreadable  the list is not a JSON array
#         checks-missing     the list is EMPTY (nothing ran, so nothing vouches;
#                            an admin merge also bypasses branch protection's
#                            required-check list), or a name in
#                            <required-checks-json> is absent or `skipping`
#         checks-not-green   a check is failing or still pending
#       `pass` and `skipping` are green; everything else is not. A required
#       check (the config's `required_checks`) must be present with bucket
#       exactly `pass`: the workflow behind it may be path-filtered, and a
#       skipped check vouches for nothing. An empty list demands no named
#       check.
#
# Callers must feed the PAYLOAD regardless of gh's exit code: `gh pr checks`
# exits non-zero whenever the news is bad (1 when a check failed, 8 when one is
# pending) while still printing the full JSON. Discarding the payload on a
# non-zero exit turns every genuinely red or pending PR into checks-unreadable.

pr_checks_verdict() {
    local required="${1:-[]}" checks
    checks="$(cat)"

    if ! printf '%s' "${checks}" | jq -e 'type == "array"' >/dev/null 2>&1; then
        printf 'checks-unreadable\tthe check list is unreadable\n'
        return 1
    fi
    if [ "$(printf '%s' "${checks}" | jq 'length')" = "0" ]; then
        printf 'checks-missing\tno checks at all; nothing vouches for the PR\n'
        return 1
    fi

    local not_green
    not_green="$(printf '%s' "${checks}" | jq \
        '[.[] | select((.bucket // "") != "pass" and (.bucket // "") != "skipping")] | length')"
    if [ "${not_green}" != "0" ]; then
        printf 'checks-not-green\t%s check(s) are failing or pending\n' "${not_green}"
        return 1
    fi

    local missing
    missing="$(printf '%s' "${checks}" | jq -r --argjson req "${required}" '
        . as $checks
        | [ $req[] as $n | $n
            | select(any($checks[]; (.name // "") == $n and (.bucket // "") == "pass") | not) ]
        | first // empty' 2>/dev/null)"
    if [ -n "${missing}" ]; then
        printf 'checks-missing\ta required check did not run and pass: %s\n' "${missing}"
        return 1
    fi
    return 0
}
