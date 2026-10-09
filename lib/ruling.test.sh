#!/usr/bin/env bash
# Tests for lib/ruling.sh
#
# Run: bash lib/ruling.test.sh
#
# Strategy: the Ruling request lib has four pure halves (compose, parse,
# compose-applied, the thread reply text) and three gh-touching ones (post,
# post-applied / nudge, pending). The pure halves are asserted on their text:
# the rendered request and applied comment are matched section for section
# against docs/prototypes/ruling-request-pr722.md (issue #78 AC 1), and the
# grammar on the exact replies the issue names (AC 2). The gh halves run
# against a GH_BIN stub that logs every call and plays back canned comment
# lists, so label state (AC 4, 5) and the partial / invalid paths (AC 6) are
# asserted on the arguments passed, never a live API.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
LIB="${SCRIPT_DIR}/ruling.sh"
PROTO="${ROOT_DIR}/docs/prototypes/ruling-request-pr722.md"
DECISIONS="$(cat "${SCRIPT_DIR}/testdata/ruling-pr722.json")"

CFG='{"repo":{"owner":"acme","name":"widgets","slug":"acme/widgets","default_branch":"trunk"},"pick":{"shape":"labels","project":null,"labels":{}}}'
export HARNESS_CONFIG_JSON="${CFG}"
# The Host env may carry the machine login; the login tests below set it
# explicitly per case, so start without one.
unset DAEMON_GH_LOGIN RULING_AGENT_LOGIN

TESTS_RUN=0
TESTS_FAILED=0
FAILED_NAMES=()

pass() { TESTS_RUN=$((TESTS_RUN + 1)); echo "  PASS: $1"; }
fail() {
    TESTS_RUN=$((TESTS_RUN + 1)); TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$1")
    echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "    $2"
}

# parse <reply> -> the parse JSON for the PR 722 decisions
parse() { bash -c ". '${LIB}'; ruling_parse \"\$1\" \"\$2\"" _ "${DECISIONS}" "$1"; }

# The prototype's three sections, split on its `<!-- ===== ... ===== -->`
# separators, with the leading PROTOTYPE banner dropped and blank edges trimmed.
proto_section() {
    awk -v want="$1" '
        /^<!-- =====/ { n++; next }
        n == want { print }
    ' "${PROTO}" | awk 'NR == 1 && /^<!-- PROTOTYPE/ {skip=1} skip {if (/-->$/) skip=0; next} {print}' \
      | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}' | awk 'NF || started {started=1; print}'
}

# ---------------------------------------------------------------------------
echo "TEST: the grammar (AC 2): full, partial and invalid replies"
for r in '1a 2B' '1A2B' '1A 2B' '  1A   2b  ' $'1A\n2B'; do
    out="$(parse "$r")"
    t="'${r//$'\n'/\\n}' parses to the full Ruling 1A 2B"
    if [ "$(printf '%s' "${out}" | jq -c '[.status, .answers, .ruling]')" = '["full",{"1":"A","2":"B"},"1A 2B"]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
done
out="$(parse '1A')"
t="'1A' alone is partial, naming decision 2 as missing"
if [ "$(printf '%s' "${out}" | jq -c '[.status, .answers, .missing]')" = '["partial",{"1":"A"},[2]]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
for r in 'I agree, resolve it' '1Z' '' '3A' '1A 2B done' '1A 1B' 'A1'; do
    out="$(parse "$r")"
    t="'${r}' is invalid and applies nothing"
    if [ "$(printf '%s' "${out}" | jq -c '[.status, .answers]')" = '["invalid",{}]' ] && [ -n "$(printf '%s' "${out}" | jq -r '.reason')" ]; then pass "$t"; else fail "$t" "out=${out}"; fi
done
out="$(parse '1A 1A 2B')"
t="a repeated pair with the same letter is not a contradiction"
if [ "$(printf '%s' "${out}" | jq -r '.status')" = "full" ]; then pass "$t"; else fail "$t" "out=${out}"; fi

# ---------------------------------------------------------------------------
echo "TEST: the rendered request matches the prototype section for section (AC 1)"
rendered="$(bash -c ". '${LIB}'; ruling_compose \"\$1\" 2e204022 'CI green, manual verification 6/6'" _ "${DECISIONS}")"
visible="$(printf '%s\n' "${rendered}" | grep -v '^<!-- auto-agent:ruling-decisions ')"
expected="$(proto_section 0)"
t="the request body (marker, heading, instructions, both decisions, footer) is byte-identical"
if [ "${visible}" = "${expected}" ]; then pass "$t"; else fail "$t" "$(diff <(printf '%s\n' "${expected}") <(printf '%s\n' "${visible}") | head -20)"; fi
t="the decisions ride inside the comment as a hidden block the lib can read back"
if printf '%s\n' "${rendered}" | grep -q '^<!-- auto-agent:ruling-decisions ' && [ "$(bash -c ". '${LIB}'; ruling_decisions_from_body \"\$1\"" _ "${rendered}" | jq -c 'map(.title)')" = "$(printf '%s' "${DECISIONS}" | jq -c 'map(.title)')" ]; then pass "$t"; else fail "$t"; fi
one="$(printf '%s' "${DECISIONS}" | jq -c '.[1:2]')"
rendered1="$(bash -c ". '${LIB}'; ruling_compose \"\$1\" abc1234 'CI green'" _ "${one}")"
t="a one-decision request says '1 decision' and shows the single all-recommended reply"
if printf '%s\n' "${rendered1}" | grep -q '^## 🧑‍⚖️ Ruling request · 1 decision$' && printf '%s\n' "${rendered1}" | grep -q 'A reply of `1A` leaves the PR ready to merge' && printf '%s\n' "${rendered1}" | grep -q '^<!-- auto-agent:ruling-request head=abc1234 decisions=1 -->$'; then pass "$t"; else fail "$t" "${rendered1}"; fi
rendered3="$(bash -c ". '${LIB}'; ruling_compose \"\$1\" abc1234 'CI green' 4242" _ "${DECISIONS}")"
t="a request that supersedes an earlier open one says so, names it, and asks for the reply here"
if printf '%s\n' "${rendered3}" | grep -q '^This request supersedes the earlier one (comment 4242)' && printf '%s\n' "${rendered3}" | grep -q '^<!-- auto-agent:ruling-request head=abc1234 decisions=2 -->$'; then pass "$t"; else fail "$t" "$(printf '%s\n' "${rendered3}" | head -6)"; fi
t="without a superseded request the line is absent"
if ! printf '%s\n' "${rendered}" | grep -q 'supersedes'; then pass "$t"; else fail "$t"; fi
fixrec="$(printf '%s' "${DECISIONS}" | jq -c '.[0].options[0].recommended = false | .[0].options[2].recommended = true')"
rendered2="$(bash -c ". '${LIB}'; ruling_compose \"\$1\" abc1234 'CI green'" _ "${fixrec}")"
t="when a recommended option is a fix, the footer says the all-recommended reply needs one more Fire"
if printf '%s\n' "${rendered2}" | grep -q 'A reply of `1C 2A` applies the fix and re-runs verification in one more Fire' ; then pass "$t"; else fail "$t" "$(printf '%s\n' "${rendered2}" | tail -2)"; fi

# ---------------------------------------------------------------------------
echo "TEST: the applied comment and the thread replies match the prototype (AC 1, 5)"
results='[{"n":1,"letter":"A","summary":"Fill time stays.","sha":null,"thread":"useSmokeScreenBinding.ts:88"},
          {"n":2,"letter":"B","summary":"Serve Plan written with the card hidden.","sha":"9f3c1a0","thread":"extractionContract.ts:912"}]'
applied="$(bash -c ". '${LIB}'; ruling_compose_applied \"\$1\" 9f3c1a0 '1A 2B' 'CI green, manual 6/6. Ready to merge.'" _ "${results}")"
t="the applied comment is byte-identical to the prototype's"
if [ "${applied}" = "$(proto_section 1)" ]; then pass "$t"; else fail "$t" "$(diff <(proto_section 1) <(printf '%s\n' "${applied}") | head)"; fi
reply="$(bash -c ". '${LIB}'; ruling_thread_reply 1 A 'Fill time stays.'")"
t="a no-change thread reply reads as the prototype's in-thread line"
if [ "${reply}" = "Ruling 1A: Fill time stays. Resolving, no change." ]; then pass "$t"; else fail "$t" "${reply}"; fi
reply="$(bash -c ". '${LIB}'; ruling_thread_reply 2 B 'Serve Plan written with the card hidden.' 9f3c1a0")"
t="a fixed thread reply names the commit"
if [ "${reply}" = 'Ruling 2B: Serve Plan written with the card hidden. Fixed in `9f3c1a0`; resolving.' ]; then pass "$t"; else fail "$t" "${reply}"; fi
t="the thread reply marker is the Thread Reconciler's ruling marker, not a lib-private one"
if [ "$(bash -c ". '${LIB}'; printf '%s' \"\${RULING_THREAD_MARKER}\"")" = '<!-- auto-agent:ruling -->' ]; then pass "$t"; else fail "$t"; fi
t="the prototype's in-thread section carries exactly that marker and the lib's reply line"
if [ "$(proto_section 2)" = "$(printf '%s\n%s' '<!-- auto-agent:ruling -->' 'Ruling 1A: Fill time stays. Resolving, no change.')" ]; then pass "$t"; else fail "$t" "$(proto_section 2)"; fi

# ---------------------------------------------------------------------------
# A gh stub: logs every call; `api .../issues/<pr>/comments` GET plays back
# comments.json, POST echoes an id; `pr edit` is logged for the label asserts.
make_stub() {
    local dir; dir="$(mktemp -d)"
    : > "${dir}/gh.log"; echo '[]' > "${dir}/comments.json"
    cat > "${dir}/gh-stub" <<EOS
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${dir}/gh.log"
[ -f "${dir}/exit-code" ] && exit "\$(cat "${dir}/exit-code")"
case "\$*" in
  "api user -q .login") echo acme-bot ;;
  "api repos/"*"/comments --paginate"*) cat "${dir}/comments.json" ;;
  "api repos/"*"/comments -f body="*) echo '{"id":777,"html_url":"https://example.invalid/c/777"}' ;;
esac
exit 0
EOS
    chmod +x "${dir}/gh-stub"
    printf '%s' "${dir}"
}
# Build a comments.json from (id, body) pairs, in order, every one under the
# human's login; set_login re-attributes one to another account.
comments_json() {
    local json='[]' id body
    while [ $# -gt 0 ]; do
        id="$1"; body="$2"; shift 2
        json="$(printf '%s' "${json}" | jq -c --argjson id "${id}" --arg body "${body}" '. + [{id: $id, body: $body, user: {login: "the-human"}, created_at: ("2026-10-0" + ($id | tostring) + "T00:00:00Z")}]')"
    done
    printf '%s' "${json}"
}
set_login() { jq -c --argjson id "$2" --arg l "$3" 'map(if .id == $id then .user.login = $l else . end)' "$1" > "$1.tmp" && mv "$1.tmp" "$1"; }

echo "TEST: posting a request applies AFK:ruling and never AFK:revise-failed (AC 4)"
dir="$(make_stub)"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_post 310 \"\$1\" 2e204022 'CI green, manual verification 6/6'" _ "${DECISIONS}")"; rc=$?
t="the comment is posted to the configured repo's PR and its id is printed"
if [ "${rc}" -eq 0 ] && grep -q '^api repos/acme/widgets/issues/310/comments -f body=<!-- auto-agent:ruling-request head=2e204022 decisions=2 -->' "${dir}/gh.log" && [ "${out}" = "777" ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out} $(head -3 "${dir}/gh.log")"; fi
t="AFK:ruling is added on the PR, AFK:revise-failed is not touched"
if grep -q '^pr edit 310 --repo acme/widgets --add-label AFK:ruling$' "${dir}/gh.log" && ! grep -q 'revise-failed' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
t="the label follows the comment, never precedes it"
if [ "$(grep -n 'add-label' "${dir}/gh.log" | cut -d: -f1)" -gt "$(grep -n 'ruling-request' "${dir}/gh.log" | cut -d: -f1)" ]; then pass "$t"; else fail "$t"; fi
rm -rf "${dir}"
dir="$(make_stub)"; echo 1 > "${dir}/exit-code"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_post 310 \"\$1\" 2e204022 'x'" _ "${DECISIONS}" >/dev/null 2>&1; rc=$?
t="a failed post exits non-zero and applies no label"
if [ "${rc}" -ne 0 ] && ! grep -q 'add-label' "${dir}/gh.log"; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${dir}"

echo "TEST: the applied comment removes AFK:ruling (AC 5)"
dir="$(make_stub)"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_post_applied 310 \"\$1\"" _ "${applied}" >/dev/null; rc=$?
t="posted, then the label dropped"
if [ "${rc}" -eq 0 ] && grep -q '^api repos/acme/widgets/issues/310/comments -f body=<!-- auto-agent:ruling-applied head=9f3c1a0 ruling="1A 2B" -->' "${dir}/gh.log" && grep -q '^pr edit 310 --repo acme/widgets --remove-label AFK:ruling$' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
rm -rf "${dir}"

echo "TEST: ruling_pending reads the open request, its decisions and the human's reply"
dir="$(make_stub)"
req="$(bash -c ". '${LIB}'; ruling_compose \"\$1\" 2e204022 'CI green'" _ "${DECISIONS}")"
comments_json 1 "$(printf '<!-- pr-review-done reviewed=x fixes=none -->\n🤖 pr-review: 0 findings')" 2 "${req}" > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"; rc=$?
t="an unanswered request: request id/head/decisions back, reply null"
if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -c '[.request.id, .request.head, (.request.decisions | length), .reply]')" = '[2,"2e204022",2,null]' ]; then pass "$t"; else fail "$t" "rc=${rc} out=${out}"; fi
t="the call lists the configured repo's PR comments, paginated"
if grep -q '^api repos/acme/widgets/issues/310/comments --paginate' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
comments_json 1 "${req}" 2 '1a 2B' > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="a full reply after the request is parsed against the request's own decisions"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.id, .reply.status, .reply.ruling, .reply.nudged]')" = '[2,"full","1A 2B",false]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 'I agree, resolve it' 3 "$(printf '<!-- auto-agent:ruling-nudge -->\nnot a Ruling')" > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="an invalid reply already nudged: status invalid, nudged true (no second nudge is owed)"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.status, .reply.nudged]')" = '["invalid",true]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 'I agree, resolve it' 3 "$(printf '<!-- auto-agent:ruling-nudge -->\nnot a Ruling')" 4 'still not reading the instructions' > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="a second non-Ruling after the nudge: the latest reply is parsed, and nudged stays true — one nudge per request"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.id, .reply.status, .reply.nudged]')" = '[4,"invalid",true]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 'I agree, resolve it' 3 "$(printf '<!-- auto-agent:ruling-nudge -->\nnot a Ruling')" 4 '1A 2B' > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="a Ruling after the nudge is read as the reply"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.status, .reply.ruling]')" = '["full","1A 2B"]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 '1A' 3 "$(printf '<!-- auto-agent:ruling-applied head=abc ruling="1A" -->\n## ✅ Ruling applied')" > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="an applied comment after the request closes it: nothing pending"
if [ "$(printf '%s' "${out}" | jq -c '[.request, .reply]')" = '[null,null]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 "$(printf '### Manual verification — round 2/3\n\n- [x] item')" 3 "$(printf 'pr-reconcile: automatic rebase failed at now. Human rebase required.')" 4 'a plain-text notice from a skill nobody listed' > "${dir}/comments.json"
set_login "${dir}/comments.json" 2 acme-bot; set_login "${dir}/comments.json" 3 acme-bot; set_login "${dir}/comments.json" 4 acme-bot
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="the machine user's own unmarked comments (a round heading, a pr-reconcile notice, any plain text) are told by login — never a reply, with the login from gh api user when the env has none"
if [ "$(printf '%s' "${out}" | jq -c '[.request.id, .reply]')" = '[1,null]' ] && grep -q '^api user -q .login$' "${dir}/gh.log"; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 'looks fine to me' > "${dir}/comments.json"
set_login "${dir}/comments.json" 2 acme-bot
out="$(RULING_AGENT_LOGIN=acme-bot GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="free text by the machine user's login (RULING_AGENT_LOGIN) is never a reply"
if [ "$(printf '%s' "${out}" | jq -c '.reply')" = 'null' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
out="$(RULING_AGENT_LOGIN=someone-else GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="the same comment by another login is the human's (invalid) reply"
if [ "$(printf '%s' "${out}" | jq -c '.reply.status')" = '"invalid"' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 "$(printf '### Manual verification — round 2/3\n\n- [x] item')" 3 ' 1a 2B ' 4 'pr-reconcile: rebased, CI green' > "${dir}/comments.json"
for i in 2 3 4; do set_login "${dir}/comments.json" "$i" acme-bot; done
out="$(RULING_AGENT_LOGIN=acme-bot GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="a shared account (the human answers under the machine user's login): a comment that is nothing but a valid Ruling is the human's, the loop's own plain text around it is still not"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.id, .reply.status, .reply.ruling, .reply.nudged]')" = '[3,"full","1A 2B",false]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 '1A' 3 '1Z' 4 '9A' 5 '2B and ship it' > "${dir}/comments.json"
for i in 2 3 4 5; do set_login "${dir}/comments.json" "$i" acme-bot; done
out="$(RULING_AGENT_LOGIN=acme-bot GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="under the machine user's login only a VALID Ruling counts: an unknown letter, an unknown decision and pairs with free text stay the loop's own (partial 1A, reply id 2)"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.id, .reply.status, .reply.answers]')" = '[2,"partial",{"1":"A"}]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 '1A' 3 'lets go with B for the second' > "${dir}/comments.json"
set_login "${dir}/comments.json" 2 acme-bot
out="$(RULING_AGENT_LOGIN=acme-bot GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="a Ruling under the machine user's login and free text under another login read together, in order"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.id, .reply.status, .reply.answers]')" = '[3,"partial",{"1":"A"}]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 'looks fine to me' > "${dir}/comments.json"
set_login "${dir}/comments.json" 2 acme-bot
: > "${dir}/gh.log"
out="$(DAEMON_GH_LOGIN=acme-bot GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="DAEMON_GH_LOGIN from the Host env is the login, and gh api user is not asked"
if [ "$(printf '%s' "${out}" | jq -c '.reply')" = 'null' ] && ! grep -q '^api user' "${dir}/gh.log"; then pass "$t"; else fail "$t" "out=${out} $(cat "${dir}/gh.log")"; fi
mkdir -p "${dir}/nologin"; printf '#!/usr/bin/env bash\ncase "$*" in "api user"*) exit 1 ;; esac\nexec "%s/gh-stub" "$@"\n' "${dir}" > "${dir}/nologin/gh"; chmod +x "${dir}/nologin/gh"
GH_BIN="${dir}/nologin/gh" bash -c ". '${LIB}'; ruling_pending 310" >/dev/null 2>"${dir}/err"; rc=$?
t="no login anywhere (no env, gh api user fails): exit 1 with a reason, never a guess by first line"
if [ "${rc}" -eq 1 ] && grep -q 'DAEMON_GH_LOGIN' "${dir}/err"; then pass "$t"; else fail "$t" "rc=${rc} $(cat "${dir}/err")"; fi
comments_json 1 "${req}" 2 '1A' 3 '2B' > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="a partial answer split over two comments (1A, then 2B) is read as the one full Ruling 1A 2B"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.id, .reply.status, .reply.answers, .reply.missing, .reply.ruling]')" = '[3,"full",{"1":"A","2":"B"},[],"1A 2B"]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 '1A' 3 'actually, 1C' > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="an earlier Ruling stands when a later comment is not one: partial {1:A}, missing [2], not invalid"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.status, .reply.answers, .reply.missing]')" = '["partial",{"1":"A"},[2]]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 "${req}" 2 '1A' 3 '1C' > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="a later comment's letter for the same decision replaces the earlier one"
if [ "$(printf '%s' "${out}" | jq -c '[.reply.status, .reply.answers]')" = '["partial",{"1":"C"}]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
comments_json 1 '1A 2B' 2 "${req}" > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="a human comment BEFORE the request is not a reply to it"
if [ "$(printf '%s' "${out}" | jq -c '[.request.id, .reply]')" = '[2,null]' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
echo '[]' > "${dir}/comments.json"
out="$(GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_pending 310")"
t="no request at all: both null, exit 0"
if [ "$(printf '%s' "${out}" | jq -c '.')" = '{"request":null,"reply":null}' ]; then pass "$t"; else fail "$t" "out=${out}"; fi
rm -rf "${dir}"

echo "TEST: a partial reply re-posts a request holding only the rest; an invalid one gets one marked nudge (AC 6)"
rest="$(bash -c ". '${LIB}'; ruling_remaining \"\$1\" '{\"1\":\"A\"}'" _ "${DECISIONS}")"
t="ruling_remaining keeps only the unanswered decisions, renumbered from 1"
if [ "$(printf '%s' "${rest}" | jq -c 'map(.title)')" = '["Serve time or rest spoken with no Serve Plan card"]' ]; then pass "$t"; else fail "$t" "${rest}"; fi
dir="$(make_stub)"
GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_nudge 310 \"\$1\" 'free text is not read'" _ "${DECISIONS}" >/dev/null; rc=$?
t="the nudge carries its marker, says what was expected per decision, and touches no label"
if [ "${rc}" -eq 0 ] && grep -q '^api repos/acme/widgets/issues/310/comments -f body=<!-- auto-agent:ruling-nudge -->' "${dir}/gh.log" && grep -q 'decision 1: A, B or C' "${dir}/gh.log" && grep -q 'decision 2: A or B' "${dir}/gh.log" && grep -q 'free text is not read' "${dir}/gh.log" && ! grep -q 'label' "${dir}/gh.log"; then pass "$t"; else fail "$t" "$(cat "${dir}/gh.log")"; fi
rm -rf "${dir}"

echo "TEST: no Harness config: exit 2, no gh call"
dir="$(make_stub)"
HARNESS_CONFIG_JSON= AUTO_AGENT_TARGET_DIR= GH_BIN="${dir}/gh-stub" bash -c ". '${LIB}'; ruling_post 310 '[]' x y" >/dev/null 2>&1; rc=$?
t="ruling_post without a config exits 2 and calls nothing"
if [ "${rc}" -eq 2 ] && [ ! -s "${dir}/gh.log" ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
rm -rf "${dir}"

echo "TEST: the CLI face (bin/auto-agent ruling)"
out="$(bash "${LIB}" parse "${SCRIPT_DIR}/testdata/ruling-pr722.json" '1A 2B')"; rc=$?
t="ruling parse <decisions-file> <reply> prints the parse JSON"
if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq -r .status)" = "full" ]; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
out="$(bash "${LIB}" compose "${SCRIPT_DIR}/testdata/ruling-pr722.json" --head 2e204022 --evidence 'CI green, manual verification 6/6' | grep -v '^<!-- auto-agent:ruling-decisions ')"
t="ruling compose renders the same request as the function"
if [ "${out}" = "${expected}" ]; then pass "$t"; else fail "$t"; fi
out="$(bash -c "bash '${LIB}' remaining <(printf '%s' \"\$1\") '{\"1\":\"A\"}'" _ "${DECISIONS}")"; rc=$?
t="ruling remaining reads its decisions from a process substitution, as the runbook calls it"
if [ "${rc}" -eq 0 ] && [ "$(printf '%s' "${out}" | jq 'length')" -eq 1 ]; then pass "$t"; else fail "$t" "rc=${rc} ${out}"; fi
dir="$(make_stub)"
GH_BIN="${dir}/gh-stub" bash -c "bash '${LIB}' nudge --pr 310 --reason 'free text is not read' <(printf '%s' \"\$1\")" _ "${DECISIONS}" >/dev/null 2>"${dir}/err"; rc=$?
t="ruling nudge reads its decisions from a process substitution and posts"
if [ "${rc}" -eq 0 ] && grep -q 'ruling-nudge' "${dir}/gh.log"; then pass "$t"; else fail "$t" "rc=${rc} $(cat "${dir}/err")"; fi
rm -rf "${dir}"
bash "${LIB}" parse /nonexistent/decisions.json '1A' >/dev/null 2>&1; rc=$?
t="a missing decisions path still exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
bash "${LIB}" bogus >/dev/null 2>&1; rc=$?
t="an unknown subcommand exits 2"; if [ "${rc}" -eq 2 ]; then pass "$t"; else fail "$t" "rc=${rc}"; fi
t="bin/auto-agent routes 'ruling' to the lib"
if grep -q 'ruling)' "${ROOT_DIR}/bin/auto-agent"; then pass "$t"; else fail "$t"; fi

echo ""; echo "Tests run: ${TESTS_RUN}, failed: ${TESTS_FAILED}"
[ "${TESTS_FAILED}" -eq 0 ] || { for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done; exit 1; }
exit 0
