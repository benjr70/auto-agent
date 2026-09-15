# PROTOTYPE: stand-in for the ADR 0004 Host image (Ubuntu 24.04 Server + Xvfb). Throwaway.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      xvfb x11-utils xauth curl ca-certificates python3 nodejs npm fonts-ubuntu fonts-liberation fonts-noto-core fonts-noto-color-emoji fontconfig \
      libgtk-3-0t64 libnss3 libasound2t64 libxss1 libgbm1 libatk-bridge2.0-0t64 libdrm2 libxkbcommon0 libnotify4 libxtst6 \
    && curl -fsSL -o /tmp/chrome.deb https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb \
    && apt-get install -y --no-install-recommends /tmp/chrome.deb && rm /tmp/chrome.deb \
    && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /work && cd /work && npm init -y >/dev/null && npm i playwright-core@1.61.1 ws@8 >/dev/null 2>&1
RUN useradd -m -s /bin/bash agent
COPY app /work/app
COPY electron-app /work/electron-app
COPY tour.js run-in-container.sh /work/
RUN chmod +x /work/run-in-container.sh && chown -R agent:agent /work
WORKDIR /work
ENTRYPOINT ["/work/run-in-container.sh"]
