#!/usr/bin/env python3
"""PROTOTYPE: compare out/host vs out/xvfb captures. Prints size match and fraction of differing pixels."""
import os, sys
from PIL import Image, ImageChops
here = os.path.dirname(os.path.abspath(__file__)); a, b = f"{here}/out/host", f"{here}/out/xvfb"
for name in sorted(f for f in os.listdir(a) if f.endswith(".png")):
    if not os.path.exists(f"{b}/{name}"): print(f"{name}: missing on xvfb side"); continue
    ia, ib = Image.open(f"{a}/{name}").convert("RGB"), Image.open(f"{b}/{name}").convert("RGB")
    if ia.size != ib.size: print(f"{name}: SIZE host={ia.size} xvfb={ib.size}"); continue
    diff = ImageChops.difference(ia, ib).convert("L").point(lambda p: 255 if p > 24 else 0)
    frac = sum(1 for p in diff.getdata() if p) / (ia.size[0] * ia.size[1])
    diff.save(f"{here}/out/diff-{name}")
    print(f"{name}: size {ia.size} identical; {frac*100:.2f}% pixels differ (threshold 24/255) -> out/diff-{name}")
