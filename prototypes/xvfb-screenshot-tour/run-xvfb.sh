#!/usr/bin/env bash
# PROTOTYPE: build the stand-in Host image and run the tour under Xvfb. Output: out/xvfb/ (elevated run:
# seccomp=unconfined + SYS_ADMIN so a SUID sandbox helper can make namespaces the way it can on a VM) and
# out/xvfb-default/ (docker default confinement, probes only). Neither is the VM: see README.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
ELECTRON_DIST="${ELECTRON_DIST:-$HOME/Smart-Smoker-V2/node_modules/electron/dist}"
docker build -q -t prototype-xvfb-tour "$HERE"
rm -rf "$HERE/out/xvfb" "$HERE/out/xvfb-default"; mkdir -p "$HERE/out/xvfb" "$HERE/out/xvfb-default"
docker run --rm --name prototype-xvfb-tour --security-opt seccomp=unconfined --cap-add SYS_ADMIN \
  -v "$ELECTRON_DIST:/opt/electron-dist:ro" -v "$HERE/out/xvfb:/out" prototype-xvfb-tour full
docker run --rm --name prototype-xvfb-tour-default \
  -v "$ELECTRON_DIST:/opt/electron-dist:ro" -v "$HERE/out/xvfb-default:/out" prototype-xvfb-tour probes
