#!/usr/bin/env bash
# PROTOTYPE: the Daemon's side of the Verification Harness contract.
# Drives a Target Project's provider the way /verify-pr would, and prints every
# decision it makes so a reader can react to the contract.
set -u
cd "$(dirname "$0")"
PR="${1:?usage: harness-stub.sh <pr-number>}"
CFG=.auto-agent/harness.json
say() { echo "[harness] $*"; }

# 0. config (schema stands in for jq assertions here)
jq -e '.verification.hermetic.command | strings' "$CFG" >/dev/null || { say "no hermetic provider: would label AFK:verify-human and stop"; exit 0; }
PROVIDER=$(jq -r .verification.hermetic.command "$CFG")
SMOKE=$(jq -r '.verification.hermetic.smoke // false' "$CFG")
say "provider=$PROVIDER smoke=$SMOKE"

# teardown on every exit path
trap '"$PROVIDER" down --pr "$PR"; say "down ran (trap)"' EXIT

# 1. up with one retry; stdout is the contract, stderr is progress
boot() { "$PROVIDER" up --pr "$PR" 2>"/tmp/fixture-up-$PR.err"; }
CONTRACT=$(boot); rc=$?
if [ $rc -eq 4 ]; then say "boot failed (rc=4), retrying once"; "$PROVIDER" down --pr "$PR"; CONTRACT=$(boot); rc=$?; fi
case $rc in
  0) say "up healthy";;
  3) say "manual-verify: infra-error — prerequisite missing: $(tail -1 /tmp/fixture-up-$PR.err)"; exit 0;;
  4) say "manual-verify: infra-error — stack boot failed twice: $(tail -1 /tmp/fixture-up-$PR.err)"; exit 0;;
  *) say "manual-verify: infra-error — provider exit $rc (not in contract)"; exit 0;;
esac
say "contract:"; echo "$CONTRACT" | sed 's/^/    /'
eval "$CONTRACT"; export $(echo "$CONTRACT" | cut -d= -f1 | xargs)

# 2. surfaces: resolve url_key against the contract, decide tour vs evidence-only
CHANGED="${CHANGED_FILES:-app/server.py}"
say "changed files (simulated): $CHANGED"
jq -r '.surfaces | to_entries[] | "\(.key) \(.value.kind) \(.value.url_key) \(.value.paths|join(","))"' "$CFG" | while read -r name kind key paths; do
  url="${!key:-<MISSING $key in contract>}"
  touched=no; IFS=, read -ra globs <<<"$paths"; for g in "${globs[@]}"; do case "$CHANGED" in *${g%%\**}*) touched=yes;; esac; done
  case "$kind" in browser|electron) mode="screenshot tour (mandatory when touched)";; *) mode="evidence-only";; esac
  say "surface $name kind=$kind url=$url touched=$touched -> $mode"
done

# 3. smoke sub-hook, last stdout line is the verdict
if [ "$SMOKE" = true ]; then
  out=$("$PROVIDER" smoke); rc=$?; line=$(echo "$out" | tail -1)
  say "smoke rc=$rc line='$line'"
fi

# 4. a checklist item an api surface would verify
say "api evidence: POST item -> $(curl -s -X POST -d hello "$FIXTURE_API_URL/items")  GET -> $(curl -s "$FIXTURE_API_URL/items")"
say "manual-verify: 1/1 PASS, 0 deferred, 0 FAIL"
