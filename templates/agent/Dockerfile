# PandaStack template · agent
#
# Unified coding-agent CLI box. Replaces the former per-brand templates
# (claude-code, codex, opencode) with ONE image that ships the terminal coding
# agents that have a real, installable CLI:
#   - claude   (@anthropic-ai/claude-code) → set ANTHROPIC_API_KEY
#   - codex    (@openai/codex)             → set OPENAI_API_KEY
#   - opencode (opencode-ai, BYO model)    → set the provider key it expects
#   - amp      (@ampcode/cli)              → set AMP_API_KEY
#   - grok     (@xai-official/grok)        → set XAI_API_KEY
#   - gemini   (@google/gemini-cli)        → set GEMINI_API_KEY
#   - copilot  (@github/copilot)           → GitHub auth
#
# Pick which agent to run by providing the matching API key in the sandbox env
# and invoking its command.
#
# Bake:
#   pandastack template build -f Dockerfile -n agent --size-mb 6144
# The seven CLIs measure ~1.56 GiB of npm globals on their own (measured, not
# estimated), which no longer fits the old 3072 MiB rootfs alongside Ubuntu +
# build-essential + Node — hence 6144, which leaves ~3 GiB for the user's repo
# and its own node_modules.

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# systemd-sysv provides /sbin/init; openssh-server provides sshd. Both are
# required by the microVM contract: the guest boots under systemd and the host
# agent's readiness probe is a TCP connect to the guest's sshd, not an ICMP
# probe. The agent injects its authorized_keys + sshd overrides at create time
# (see agent/internal/guest/keys.go InjectInto), so the image only needs the
# init system + the sshd binary baked in.
# build-essential + pkg-config: coding agents routinely `npm install` packages
# with native addons (node-gyp needs gcc/make), and `pip install` C-extension
# wheels. Without a compiler those installs fail with confusing gyp errors.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git ripgrep fd-find bat tmux \
      build-essential pkg-config \
      python3 python3-pip python3-venv \
      systemd systemd-sysv openssh-server sudo \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install each agent CLI on its OWN line so a single registry blip only drops
# THAT one (the old single `... || true` line dropped all three on any failure).
# Each is verified so a silently-missing CLI fails the bake rather than shipping
# a box where one of the agent commands is absent.
# NOTE on amp: the package is @ampcode/cli. @sourcegraph/amp is the OLD name and
# now publishes only a deprecation stub — installing it yields no `amp` binary.
RUN npm i -g @anthropic-ai/claude-code && command -v claude
RUN npm i -g @openai/codex             && command -v codex
RUN npm i -g opencode-ai               && command -v opencode
RUN npm i -g @ampcode/cli              && command -v amp
RUN npm i -g @xai-official/grok        && command -v grok
RUN npm i -g @google/gemini-cli        && command -v gemini
RUN npm i -g @github/copilot           && command -v copilot

WORKDIR /workspace
CMD ["/bin/bash"]
