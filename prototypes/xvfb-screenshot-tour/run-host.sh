#!/usr/bin/env bash
# PROTOTYPE: the same tour on the live Host's GNOME/XWayland display (:0), the way verify-pr runs it.
# Uses the checkout's Electron shim (sandbox disabled, as provisioning measured) and playwright-core.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; SS="${SS:-$HOME/Smart-Smoker-V2}"
OUT="$HERE/out/host"; rm -rf "$OUT"; mkdir -p "$OUT"
source "$SS/scripts/verify-pr/lib/resolve-display-env.sh"; resolve_display_env || exit 3
PORT=21990; export FIXTURE_WEB_URL="http://127.0.0.1:$PORT"
python3 "$HERE/app/server.py" $PORT & APP=$!
for _ in $(seq 1 50); do curl -fsS "$FIXTURE_WEB_URL/api/health" >/dev/null 2>&1 && break; sleep 0.2; done
{ echo "fc-match sans-serif: $(fc-match sans-serif)"; echo "DISPLAY=$DISPLAY"; xdpyinfo | grep -E 'dimensions|resolution' | tr -s ' '; } > "$OUT/env.txt"
electron --remote-debugging-port=9444 "$HERE/electron-app" >"$OUT/electron.log" 2>&1 & EL=$!
for _ in $(seq 1 40); do curl -fsS http://127.0.0.1:9444/json/version >/dev/null 2>&1 && break; sleep 0.5; done
OUT_DIR="$OUT" CHROME_BIN=/usr/bin/google-chrome CDP_URL=http://127.0.0.1:9444 NODE_PATH="$SS/node_modules" node "$HERE/tour.js" > "$OUT/tour.log" 2>&1
echo "TOUR_RC=$?" >> "$OUT/tour.txt"
kill $EL $APP 2>/dev/null; wait 2>/dev/null; ls -la "$OUT"
