#!/usr/bin/env bash
# PROTOTYPE: runs as root inside the container. Copies the Host's Electron dist (mounted ro at
# /opt/electron-dist), starts Xvfb on :99, then runs the tour and the sandbox probes as user `agent`.
set -u
MODE="${1:-full}"
OUT=/out; mkdir -p "$OUT"; chown agent:agent "$OUT"
cp -r /opt/electron-dist /opt/electron && chown -R root:root /opt/electron && chmod 4755 /opt/electron/chrome-sandbox
Xvfb :99 -screen 0 1600x1200x24 -nolisten tcp >/tmp/xvfb.log 2>&1 &
for _ in $(seq 1 50); do xdpyinfo -display :99 >/dev/null 2>&1 && break; sleep 0.2; done
xdpyinfo -display :99 | grep -E 'dimensions|resolution' | tr -s ' ' > "$OUT/xvfb-display.txt"
{ echo "fc-match sans-serif: $(fc-match sans-serif)"; echo "unprivileged_userns_clone: $(cat /proc/sys/kernel/unprivileged_userns_clone 2>/dev/null)"; echo "apparmor_restrict_unprivileged_userns: $(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null)"; } > "$OUT/env.txt"
PORT=21990
run_agent() { su agent -s /bin/bash -c "export DISPLAY=:99 HOME=/home/agent FIXTURE_WEB_URL=http://127.0.0.1:$PORT; cd /work; $*"; }
run_agent "python3 app/server.py $PORT" &
for _ in $(seq 1 50); do curl -fsS "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 && break; sleep 0.2; done

# --- Electron sandbox probes (each: does the shell start and answer CDP within 20s?)
probe_electron() { # $1 label, $2 env prefix, $3 extra args
  local label="$1" envp="$2" args="$3" rc=1
  run_agent "$envp /opt/electron/electron $args --remote-debugging-port=9444 electron-app >/tmp/el-$label.log 2>&1" &
  local sh_pid=$!
  for _ in $(seq 1 40); do curl -fsS http://127.0.0.1:9444/json/version >/dev/null 2>&1 && { rc=0; break; }; sleep 0.5; done
  echo "ELECTRON_PROBE_$label=$([ $rc -eq 0 ] && echo STARTED || echo FAILED)" >> "$OUT/sandbox.txt"
  [ $rc -ne 0 ] && { echo "--- $label stderr tail:"; tail -3 /tmp/el-$label.log; } >> "$OUT/sandbox.txt"
  pkill -f 'remote-debugging-port=9444' >/dev/null 2>&1; wait $sh_pid 2>/dev/null; sleep 1
}
: > "$OUT/sandbox.txt"
probe_electron "suid_helper_4755" "" ""
chmod 0755 /opt/electron/chrome-sandbox
probe_electron "no_helper_no_flag" "" ""
probe_electron "disable_sandbox_env" "ELECTRON_DISABLE_SANDBOX=1" ""
chmod 4755 /opt/electron/chrome-sandbox

[ "$MODE" = probes ] && { chown -R agent:agent "$OUT"; cat "$OUT/sandbox.txt"; exit 0; }
# --- Chrome sandbox probe: playwright with chromiumSandbox=true (no --no-sandbox) as non-root
run_agent "OUT_DIR=$OUT/chrome-sandbox-on CHROME_BIN=/usr/bin/google-chrome CHROME_SANDBOX=1 node tour.js" >/dev/null 2>&1 \
  && echo "CHROME_PROBE_sandbox_on=STARTED" >> "$OUT/sandbox.txt" \
  || { echo "CHROME_PROBE_sandbox_on=FAILED"; cat "$OUT/chrome-sandbox-on/tour.txt" 2>/dev/null; } >> "$OUT/sandbox.txt"

# --- The tour proper: Electron up (SUID helper), then browser + electron shots
run_agent "/opt/electron/electron --remote-debugging-port=9444 electron-app >/tmp/el-tour.log 2>&1" &
for _ in $(seq 1 40); do curl -fsS http://127.0.0.1:9444/json/version >/dev/null 2>&1 && break; sleep 0.5; done
run_agent "OUT_DIR=$OUT CHROME_BIN=/usr/bin/google-chrome CDP_URL=http://127.0.0.1:9444 node tour.js" > "$OUT/tour.log" 2>&1
echo "TOUR_RC=$?" >> "$OUT/tour.txt"
pkill -f 'remote-debugging-port=9444' >/dev/null 2>&1
chown -R agent:agent "$OUT"; ls -la "$OUT"
