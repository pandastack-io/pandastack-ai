# PandaStack template · agent
#
# Unified coding-agent CLI box. Replaces the former per-brand templates
# (claude-code, codex, opencode, amp, devin) with ONE image that ships every
# real terminal coding agent pre-installed:
#   - claude  (@anthropic-ai/claude-code)  → set ANTHROPIC_API_KEY
#   - codex   (@openai/codex)              → set OPENAI_API_KEY
#   - opencode (opencode-ai, BYO model)    → set the provider key it expects
#
# Pick which agent to run by providing the matching API key in the sandbox env
# and invoking its command (`claude`, `codex`, or `opencode`).
#
# Bake:
#   pandastack template build -f Dockerfile -n agent --size-mb 3072

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# systemd-sysv provides /sbin/init (without it the Firecracker kernel falls
# through to /bin/sh and nothing boots); openssh-server provides sshd, the
# host↔guest bridge the agent uses for exec/fs and the snapshot-bake readiness
# probe. The agent injects its authorized_keys + sshd overrides at create time
# (see agent/internal/guest/keys.go InjectInto), so the image only needs the
# init system + the sshd binary baked in.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git ripgrep fd-find bat tmux \
      python3 python3-pip python3-venv \
      systemd systemd-sysv openssh-server sudo \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# All three CLIs in one layer. `|| true` keeps the bake resilient if any single
# registry publish lags; the others still land.
RUN npm i -g @anthropic-ai/claude-code @openai/codex opencode-ai || true

WORKDIR /workspace
CMD ["/bin/bash"]
