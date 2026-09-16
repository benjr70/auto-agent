#!/usr/bin/env bash
# Tests for lib/vendored-skills.sh
#
# Run: bash lib/vendored-skills.test.sh
#
# Strategy: the offline `check` runs against the REAL plugin (the shipped pin
# and copies must agree) and against mutated temp copies (a missing dir, a
# renamed frontmatter, a short sha). `check --upstream` and `sync` are driven
# with a GH_BIN stub serving a canned upstream tree and file contents, so the
# drift detection and the copy are tested without the network (issue #29
# AC 3: vendored skills carry their upstream commit in a pin file).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
LIB="${SCRIPT_DIR}/vendored-skills.sh"
PLUGIN="${ROOT_DIR}/plugin"

TESTS_RUN=0; TESTS_FAILED=0; FAILED_NAMES=()
pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() { TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1"); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"; }

SHA="1111111111111111111111111111111111111111"

# make_plugin: a temp plugin with one pinned skill `alpha` (SKILL.md + agents/openai.yaml)
make_plugin() {
    local dir; dir="$(mktemp -d)"
    mkdir -p "${dir}/skills/alpha/agents"
    printf -- '---\nname: alpha\ndescription: x\n---\n\nbody\n' > "${dir}/skills/alpha/SKILL.md"
    printf 'interface:\n  display_name: "Alpha"\n' > "${dir}/skills/alpha/agents/openai.yaml"
    printf '{"source":{"repo":"acme/skills","commit":"%s","committed_at":"2026-01-01T00:00:00Z"},"skills":{"alpha":"skills/x/alpha"}}\n' "${SHA}" > "${dir}/vendored-skills.json"
    echo "${dir}"
}

# make_gh <dir> <upstream-root>: a gh stub serving the tree and contents of
# <upstream-root> (files under skills/x/alpha/...) as acme/skills@<any sha>
make_gh() {
    local dir="$1" up="$2"
    cat > "${dir}/gh-stub" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "${dir}/gh-calls"
[ "\$1" = api ] || exit 1
case "\$2" in
    repos/acme/skills/commits/*)
        ref="\${2##*/}"
        case "\$ref" in main) sha="2222222222222222222222222222222222222222" ;; *) sha="\$ref" ;; esac
        echo "\$sha	2026-02-02T00:00:00Z" ;;
    repos/acme/skills/git/trees/*)
        (cd "${up}" && find . -type f | sort | while read -r f; do rel="\${f#./}"; printf '{"path":"%s","type":"blob","sha":"%s"}\n' "\$rel" "\$(git hash-object "\$f")"; done) \
            | jq -s --argjson t "\$(cat "${dir}/truncated" 2>/dev/null || echo false)" '{sha: "x", truncated: \$t, tree: .}' ;;
    repos/acme/skills/contents/*)
        p="\${2#repos/acme/skills/contents/}"; p="\${p%%\\?*}"
        base64 -w0 < "${up}/\$p" ;;
    *) exit 1 ;;
esac
STUB
    chmod +x "${dir}/gh-stub"
}

echo "TEST: the shipped plugin's pin and vendored copies agree (offline check)"
out="$(bash "${LIB}" check 2>&1)"; rc=$?
t="check exits 0 on the shipped plugin and names the three skills"
if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^ok research:' && printf '%s\n' "${out}" | grep -q '^ok grilling:' && printf '%s\n' "${out}" | grep -q '^ok domain-modeling:' && printf '%s\n' "${out}" | grep -Eq '^vendored-skills: 3 skills pinned at [0-9a-f]{7} ok$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="the pin names a full upstream commit and the source repo"
if jq -e '.source.commit | test("^[0-9a-f]{40}$")' "${PLUGIN}/vendored-skills.json" >/dev/null && [ "$(jq -r .source.repo "${PLUGIN}/vendored-skills.json")" = "mattpocock/skills" ]; then pass "$t"; else fail "$t"; fi
t="list prints name, upstream path and commit per skill"
if [ "$(bash "${LIB}" list | grep -Ec $'^[a-z-]+\tskills/[a-z]+/[a-z-]+\t[0-9a-f]{40}$')" -eq 3 ]; then pass "$t"; else fail "$t" "$(bash "${LIB}" list)"; fi

echo "TEST: offline check failures"
dir="$(make_plugin)"
t="a healthy temp plugin passes"; bash "${LIB}" check "${dir}" >/dev/null 2>&1 && pass "$t" || fail "$t"
rm -rf "${dir}/skills/alpha"
out="$(bash "${LIB}" check "${dir}" 2>&1)"; rc=$?
t="a missing vendored dir fails with its name"; if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^FAIL alpha: .*SKILL.md missing'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"; dir="$(make_plugin)"
sed -i 's/^name: alpha$/name: beta/' "${dir}/skills/alpha/SKILL.md"
out="$(bash "${LIB}" check "${dir}" 2>&1)"; rc=$?
t="a frontmatter name that is not the pinned name fails"; if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q "^FAIL alpha: SKILL.md frontmatter name is 'beta'"; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}"; dir="$(make_plugin)"
jq '.source.commit = "abc123"' "${dir}/vendored-skills.json" > "${dir}/p.json" && mv "${dir}/p.json" "${dir}/vendored-skills.json"
out="$(bash "${LIB}" check "${dir}" 2>&1)"; rc=$?
t="a short sha in the pin fails"; if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^FAIL pin: commit is not a full sha'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -f "${dir}/vendored-skills.json"
bash "${LIB}" check "${dir}" >/dev/null 2>&1; rc=$?
t="a missing pin file exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${dir}"

echo "TEST: check --upstream compares blob shas with the pinned upstream tree"
dir="$(make_plugin)"; up="$(mktemp -d)"
mkdir -p "${up}/skills/x/alpha/agents"; cp "${dir}/skills/alpha/SKILL.md" "${up}/skills/x/alpha/SKILL.md"; cp "${dir}/skills/alpha/agents/openai.yaml" "${up}/skills/x/alpha/agents/openai.yaml"
make_gh "${dir}" "${up}"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}" check --upstream "${dir}" 2>&1)"; rc=$?
t="identical copies pass and the tree was fetched at the pinned commit"
if [ "${rc}" -eq 0 ] && grep -q "^api repos/acme/skills/git/trees/${SHA}?recursive=1" "${dir}/gh-calls"; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
echo "edited" >> "${dir}/skills/alpha/SKILL.md"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}" check --upstream "${dir}" 2>&1)"; rc=$?
t="a locally edited file is reported as differing"; if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^FAIL alpha: SKILL.md differs from acme/skills@1111111'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
cp "${up}/skills/x/alpha/SKILL.md" "${dir}/skills/alpha/SKILL.md"; echo x > "${dir}/skills/alpha/EXTRA.md"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}" check --upstream "${dir}" 2>&1)"; rc=$?
t="a local file absent upstream is reported"; if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^FAIL alpha: EXTRA.md is not in the upstream skill'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -f "${dir}/skills/alpha/EXTRA.md" "${dir}/skills/alpha/agents/openai.yaml"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}" check --upstream "${dir}" 2>&1)"; rc=$?
t="an upstream file missing locally is reported"; if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q '^FAIL alpha: agents/openai.yaml missing locally'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
echo true > "${dir}/truncated"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}" check --upstream "${dir}" 2>&1)"; rc=$?
t="a truncated upstream tree cannot verify: exit 1, not a silent pass"; if [ "${rc}" -eq 1 ] && printf '%s\n' "${out}" | grep -q 'truncated'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}" "${up}"

echo "TEST: sync replaces the vendored dirs from upstream and moves the pin"
dir="$(make_plugin)"; up="$(mktemp -d)"
mkdir -p "${up}/skills/x/alpha/agents" "${up}/skills/x/other"
printf -- '---\nname: alpha\ndescription: new\n---\n\nnew body\n' > "${up}/skills/x/alpha/SKILL.md"
printf 'interface: {}\n' > "${up}/skills/x/alpha/agents/openai.yaml"
printf 'ref\n' > "${up}/skills/x/alpha/REF.md"
printf 'not vendored\n' > "${up}/skills/x/other/SKILL.md"
echo stale > "${dir}/skills/alpha/STALE.md"
make_gh "${dir}" "${up}"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}" sync --commit main "${dir}" 2>&1)"; rc=$?
t="sync resolves the ref, copies 3 files, reports the new commit"
if [ "${rc}" -eq 0 ] && printf '%s\n' "${out}" | grep -q '^synced alpha: 3 files from skills/x/alpha@2222222$' && printf '%s\n' "${out}" | grep -q '^vendored-skills: synced 1 skills at 2222222222222222222222222222222222222222$'; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
t="the vendored dir now mirrors upstream: new files in, stale file gone, other skill untouched"
if [ -f "${dir}/skills/alpha/REF.md" ] && [ ! -f "${dir}/skills/alpha/STALE.md" ] && grep -q 'new body' "${dir}/skills/alpha/SKILL.md" && [ ! -d "${dir}/skills/other" ]; then pass "$t"; else fail "$t" "$(find "${dir}/skills" -type f)"; fi
t="the pin carries the resolved commit and its date"
if [ "$(jq -r '.source.commit + " " + .source.committed_at' "${dir}/vendored-skills.json")" = "2222222222222222222222222222222222222222 2026-02-02T00:00:00Z" ]; then pass "$t"; else fail "$t" "$(jq -c .source "${dir}/vendored-skills.json")"; fi
t="check --upstream passes right after a sync"
GH_BIN="${dir}/gh-stub" bash "${LIB}" check --upstream "${dir}" >/dev/null 2>&1 && pass "$t" || fail "$t"
t="the synced skill dir is world-readable, not a 0700 temp dir"
mode="$(stat -c %a "${dir}/skills/alpha")"
if [ $(( 0${mode} & 05 )) -eq 5 ]; then pass "$t"; else fail "$t" "mode ${mode}"; fi
# a second pinned skill whose upstream files are missing: nothing may change
jq '.skills.beta = "skills/x/beta"' "${dir}/vendored-skills.json" > "${dir}/p.json" && mv "${dir}/p.json" "${dir}/vendored-skills.json"
before="$(find "${dir}/skills" -type f | sort | xargs md5sum | md5sum)"
out="$(GH_BIN="${dir}/gh-stub" bash "${LIB}" sync --commit main "${dir}" 2>&1)"; rc=$?
after="$(find "${dir}/skills" -type f | sort | xargs md5sum | md5sum)"
t="a sync that cannot fetch one pinned skill exits 1 and leaves every vendored dir and the pin untouched"
if [ "${rc}" -eq 1 ] && [ "${before}" = "${after}" ] && [ "$(jq -r .source.commit "${dir}/vendored-skills.json")" = "2222222222222222222222222222222222222222" ]; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
rm -rf "${dir}" "${up}"

echo "TEST: usage"
bash "${LIB}" >/dev/null 2>&1; rc=$?; t="no subcommand exits 2"; [ "${rc}" -eq 2 ] && pass "$t" || fail "$t" "rc=${rc}"
bash "${LIB}" check --bogus >/dev/null 2>&1; rc=$?; t="unknown option exits 2"; [ "${rc}" -eq 2 ] && pass "$t" || fail "$t" "rc=${rc}"

echo ""; echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
[ "${TESTS_FAILED}" -eq 0 ] || { for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done; exit 1; }
exit 0
