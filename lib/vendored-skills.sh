#!/usr/bin/env bash
# vendored-skills.sh: the upstream skills the plugin vendors, pinned to one
# upstream commit (ADR 0001: a fresh Host needs nothing beyond the install).
#
# Why this exists: `research`, `grilling` and `domain-modeling` are third-party
# skills (mattpocock/skills) that afk-resolve and the planning skills call by
# name. Installing them per Host with a skills manager would be a second,
# separately versioned pin next to the Harness install, so they are copied
# into plugin/skills/ instead, and this lib is the only thing that copies
# them: it records WHICH upstream commit the copies came from in the pin file,
# can prove the copies still match that commit, and moves the pin. A hand
# edit to a vendored skill is therefore visible (check --upstream fails), and
# an upgrade is `sync --commit <ref>` plus a review of the diff.
#
# The pin file, plugin/vendored-skills.json:
#   { "source": { "repo": "<owner>/<name>", "commit": "<40-hex>", "committed_at": "<ISO>" },
#     "skills": { "<plugin skill name>": "<path of the skill dir in the upstream repo>", ... } }
#
# Usage:
#   lib/vendored-skills.sh check [--upstream] [<plugin-dir>]
#   lib/vendored-skills.sh sync  [--commit <sha|ref>] [<plugin-dir>]
#   lib/vendored-skills.sh list  [<plugin-dir>]
#
# check      Offline: the pin file parses, the commit is a full sha, every
#            pinned skill has plugin/skills/<name>/SKILL.md whose frontmatter
#            `name:` is <name>, and no vendored dir carries a file the pin's
#            upstream path would not (it cannot know that offline; it checks
#            the dir exists and is non-empty). Prints one line per skill and
#            `vendored-skills: <n> skills pinned at <commit> ok`.
#            --upstream additionally fetches the upstream tree at the pinned
#            commit (one gh api call) and compares every file's git blob sha
#            with the local copy's: extra, missing or changed files fail.
# sync       Network: resolves --commit (default: the pinned commit; a ref such
#            as `main` is resolved to its sha), replaces each vendored dir with
#            the upstream files at that commit, and rewrites the pin. Prints
#            `vendored-skills: synced <n> skills at <commit>`.
# list       Prints `<name><TAB><upstream path><TAB><commit>` per skill.
#
# Exit codes:
#   0  ok
#   1  a check failed (each is reported), or sync could not fetch
#   2  usage, or the pin file is missing/unreadable
#
# Env:
#   GH_BIN   gh CLI (default: gh), injectable for tests; only `gh api` is used
#   GIT_BIN  git CLI (default: git); `git hash-object` computes the blob shas

set -uo pipefail

_vs_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENDORED_SKILLS_PIN_FILENAME="vendored-skills.json"
_VS_DEFAULT_PLUGIN="$(cd "${_vs_lib_dir}/.." && pwd)/plugin"

_vs_err() { echo "vendored-skills: $*" >&2; }

# _vs_pin <plugin-dir> -> the pin JSON, or 2
_vs_pin() {
    local file="$1/${VENDORED_SKILLS_PIN_FILENAME}"
    if [ ! -f "${file}" ]; then _vs_err "pin file not found: ${file}"; return 2; fi
    jq -e -c 'type == "object" and (.source.repo | type == "string") and (.source.commit | type == "string") and (.skills | type == "object")' "${file}" >/dev/null 2>&1 || {
        _vs_err "pin file is not a vendored-skills pin: ${file}"; return 2; }
    jq -c . "${file}"
}

# _vs_tree <repo> <commit> -> "path<TAB>sha" per blob, or 1
_vs_tree() {
    "${GH_BIN:-gh}" api "repos/$1/git/trees/$2?recursive=1" --jq '.tree[] | select(.type == "blob") | "\(.path)\t\(.sha)"' 2>/dev/null
}

vendored_skills_list() {
    local plugin="${1:-${_VS_DEFAULT_PLUGIN}}" pin
    pin="$(_vs_pin "${plugin}")" || return $?
    printf '%s' "${pin}" | jq -r '.source.commit as $c | .skills | to_entries[] | "\(.key)\t\(.value)\t\($c)"'
}

vendored_skills_check() {
    local upstream=false plugin=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --upstream) upstream=true ;;
            -*) _vs_err "unknown option '$1'"; return 2 ;;
            *) plugin="$1" ;;
        esac
        shift
    done
    plugin="${plugin:-${_VS_DEFAULT_PLUGIN}}"
    local pin commit repo failed=0 count=0 name path dir fm
    pin="$(_vs_pin "${plugin}")" || return $?
    commit="$(printf '%s' "${pin}" | jq -r '.source.commit')"
    repo="$(printf '%s' "${pin}" | jq -r '.source.repo')"
    if ! printf '%s' "${commit}" | grep -Eq '^[0-9a-f]{40}$'; then
        echo "FAIL pin: commit is not a full sha: ${commit}"; failed=1
    fi
    local tree=""
    if [ "${upstream}" = true ]; then
        tree="$(_vs_tree "${repo}" "${commit}")" || { _vs_err "cannot fetch the upstream tree of ${repo}@${commit}"; return 1; }
    fi
    while IFS=$'\t' read -r name path _; do
        [ -n "${name}" ] || continue
        count=$((count + 1))
        dir="${plugin}/skills/${name}"
        if [ ! -f "${dir}/SKILL.md" ]; then
            echo "FAIL ${name}: ${dir}/SKILL.md missing"; failed=1; continue
        fi
        fm="$(sed -n '1,/^---$/{/^name:/p}' "${dir}/SKILL.md" | head -1 | sed -E 's/^name:[[:space:]]*//; s/[[:space:]]+$//')"
        if [ "${fm}" != "${name}" ]; then
            echo "FAIL ${name}: SKILL.md frontmatter name is '${fm}'"; failed=1; continue
        fi
        if [ "${upstream}" = true ]; then
            local want got rel drift=0
            # every upstream file under <path>/ must exist locally with the same blob sha
            while IFS=$'\t' read -r upath usha; do
                [ -n "${upath}" ] || continue
                case "${upath}" in "${path}"/*) ;; *) continue ;; esac
                rel="${upath#"${path}"/}"
                if [ ! -f "${dir}/${rel}" ]; then echo "FAIL ${name}: ${rel} missing locally"; drift=1; continue; fi
                got="$("${GIT_BIN:-git}" hash-object "${dir}/${rel}")"
                if [ "${got}" != "${usha}" ]; then echo "FAIL ${name}: ${rel} differs from ${repo}@${commit:0:7}"; drift=1; fi
            done <<<"${tree}"
            # and no local file may be absent upstream
            while IFS= read -r local_file; do
                rel="${local_file#"${dir}"/}"
                want="$(printf '%s\n' "${tree}" | grep -F -- "${path}/${rel}"$'\t' | head -1)"
                if [ -z "${want}" ]; then echo "FAIL ${name}: ${rel} is not in the upstream skill"; drift=1; fi
            done < <(find "${dir}" -type f | sort)
            [ "${drift}" -eq 0 ] || { failed=1; continue; }
        fi
        echo "ok ${name}: ${path}@${commit:0:7}"
    done < <(printf '%s' "${pin}" | jq -r '.skills | to_entries[] | "\(.key)\t\(.value)\t"')
    if [ "${failed}" -ne 0 ]; then
        echo "vendored-skills: check FAILED (${count} skills pinned at ${commit:0:7})"
        return 1
    fi
    echo "vendored-skills: ${count} skills pinned at ${commit:0:7} ok"
}

vendored_skills_sync() {
    local ref="" plugin=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --commit) ref="${2:-}"; shift ;;
            -*) _vs_err "unknown option '$1'"; return 2 ;;
            *) plugin="$1" ;;
        esac
        shift
    done
    plugin="${plugin:-${_VS_DEFAULT_PLUGIN}}"
    local gh="${GH_BIN:-gh}" pin repo commit when tree name path dir count=0
    pin="$(_vs_pin "${plugin}")" || return $?
    repo="$(printf '%s' "${pin}" | jq -r '.source.repo')"
    [ -n "${ref}" ] || ref="$(printf '%s' "${pin}" | jq -r '.source.commit')"
    local meta
    meta="$("${gh}" api "repos/${repo}/commits/${ref}" --jq '"\(.sha)\t\(.commit.committer.date)"' 2>/dev/null)" || {
        _vs_err "cannot resolve ${repo}@${ref}"; return 1; }
    IFS=$'\t' read -r commit when <<<"${meta}"
    tree="$(_vs_tree "${repo}" "${commit}")" || { _vs_err "cannot fetch the upstream tree of ${repo}@${commit}"; return 1; }
    while IFS=$'\t' read -r name path _; do
        [ -n "${name}" ] || continue
        dir="${plugin}/skills/${name}"
        local staged; staged="$(mktemp -d)"
        local n=0 upath usha rel
        while IFS=$'\t' read -r upath usha; do
            [ -n "${upath}" ] || continue
            case "${upath}" in "${path}"/*) ;; *) continue ;; esac
            rel="${upath#"${path}"/}"
            mkdir -p "${staged}/$(dirname "${rel}")"
            "${gh}" api "repos/${repo}/contents/${upath}?ref=${commit}" --jq '.content' 2>/dev/null | base64 -d > "${staged}/${rel}" || {
                _vs_err "cannot fetch ${upath}@${commit:0:7}"; rm -rf "${staged}"; return 1; }
            n=$((n + 1))
        done <<<"${tree}"
        if [ "${n}" -eq 0 ]; then
            _vs_err "no files under ${path} in ${repo}@${commit:0:7}"; rm -rf "${staged}"; return 1
        fi
        rm -rf "${dir}"; mkdir -p "$(dirname "${dir}")"; mv "${staged}" "${dir}"
        echo "synced ${name}: ${n} files from ${path}@${commit:0:7}"
        count=$((count + 1))
    done < <(printf '%s' "${pin}" | jq -r '.skills | to_entries[] | "\(.key)\t\(.value)\t"')
    printf '%s' "${pin}" | jq --arg c "${commit}" --arg w "${when}" '.source.commit = $c | .source.committed_at = $w' > "${plugin}/${VENDORED_SKILLS_PIN_FILENAME}.tmp" \
        && mv "${plugin}/${VENDORED_SKILLS_PIN_FILENAME}.tmp" "${plugin}/${VENDORED_SKILLS_PIN_FILENAME}"
    echo "vendored-skills: synced ${count} skills at ${commit}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    cmd="${1:-}"; shift || true
    case "${cmd}" in
        check) vendored_skills_check "$@" ;;
        sync)  vendored_skills_sync "$@" ;;
        list)  vendored_skills_list "$@" ;;
        *) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
    esac
fi
