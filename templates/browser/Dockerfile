# PandaStack template · browser
#
# Headless Chromium + Playwright + xvfb for scraping, RPA, and screenshot tasks,
# PLUS the Python crawl4ai / readability / trafilatura extraction stack (merged
# from the former 'crawler' template) for LLM-friendly web extraction pipelines.
# Use the Node Playwright API or the Python crawl4ai API in the same sandbox.
#
# Bake:
#   pandastack template build -f Dockerfile -n browser --size-mb 4096

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# systemd-sysv provides /sbin/init (without it the Firecracker kernel falls
# through to /bin/sh and nothing boots); openssh-server provides sshd, the
# host↔guest bridge the agent uses for exec/fs and the snapshot-bake readiness
# probe. The agent injects its authorized_keys + sshd overrides at create time
# (see agent/internal/guest/keys.go InjectInto), so the image only needs the
# init system + the sshd binary baked in.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git xvfb ffmpeg chromium-browser \
      python3 python3-pip python3-venv \
      libnss3 libatk-bridge2.0-0 libgbm1 libasound2t64 \
      systemd systemd-sysv openssh-server sudo \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Node Playwright + Python extraction stack (crawl4ai et al.)
RUN npm i -g playwright \
    && npx --yes playwright install chromium

RUN pip install --no-cache-dir --break-system-packages \
      playwright crawl4ai readability-lxml trafilatura \
      httpx beautifulsoup4 lxml \
    && python3 -m playwright install chromium

WORKDIR /workspace
ENV DISPLAY=:99
CMD ["/bin/bash"]
