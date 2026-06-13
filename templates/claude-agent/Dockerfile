# PandaStack template · claude-agent
#
# Self-hosted sandbox runtime for Claude Managed Agents
# (https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes).
#
# Anthropic runs the agent loop (the model, reasoning, session state); this
# template is where the agent's tool calls actually execute. The orchestrator
# (examples/claude-managed-agents) claims a work item from Anthropic's queue,
# creates one sandbox per session from this template, and launches the
# pre-built in-guest runner:
#
#   ant beta:worker run --workdir /workspace
#
# The runner reads ANTHROPIC_SESSION_ID / ANTHROPIC_WORK_ID /
# ANTHROPIC_ENVIRONMENT_ID / ANTHROPIC_ENVIRONMENT_KEY from its environment,
# downloads the agent's skills to /workspace/skills/<name>/, executes tool
# calls (bash, read, write, edit, glob, grep), heartbeats its work lease, and
# exits when the session goes idle (--max-idle, default 60s).
#
# Contract with Anthropic's worker harness:
#   - /bin/bash must exist at that exact path (ubuntu: yes)
#   - /workspace is the working directory for tools + skills
#   - /mnt/session/outputs is where the agent is told to write deliverables;
#     the orchestrator pulls it via the fs API before deleting the sandbox
#     (no volume mount: volume-attached creates force a cold boot and would
#     forfeit the snapshot-restore fast path)
#
# Runtimes: mise with pre-warmed Node 22 + Python 3.12 so agent-authored code
# runs instantly — same philosophy and layout as the `base` template.
#
# Bake:
#   pandastack template build -f Dockerfile -n claude-agent --size-mb 8192

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive \
    MISE_DATA_DIR=/opt/mise \
    MISE_CONFIG_DIR=/opt/mise \
    MISE_YES=1

# ── Essentials + microVM init/sshd contract ───────────────────────────────────
# systemd-sysv provides /sbin/init (without it the Firecracker kernel falls
# through to /bin/sh and nothing boots); openssh-server provides sshd, the
# host↔guest bridge the agent uses for exec/fs and the snapshot-bake readiness
# probe. The agent injects its authorized_keys + sshd overrides at create time.
# jq + ripgrep are quality-of-life for agent bash sessions.
RUN apt-get update && apt-get install -y --no-install-recommends \
      git curl ca-certificates build-essential xz-utils unzip zip pkg-config \
      jq ripgrep tar \
      systemd systemd-sysv openssh-server sudo \
    && rm -rf /var/lib/apt/lists/*

# ── ant CLI: Anthropic's pre-built environment worker ─────────────────────────
# Single static Go binary; its only runtime dependency is /bin/bash. Release
# assets use amd64/arm64 directly (no x64 mapping needed, unlike mise).
ARG ANT_VERSION=1.12.1
ARG TARGETARCH
RUN case "${TARGETARCH:-amd64}" in \
      amd64|arm64) ANT_ARCH="${TARGETARCH:-amd64}" ;; \
      *) echo "unsupported arch: ${TARGETARCH}" && exit 1 ;; \
    esac \
    && curl -fsSL "https://github.com/anthropics/anthropic-cli/releases/download/v${ANT_VERSION}/ant_${ANT_VERSION}_linux_${ANT_ARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin ant \
    && chmod 0755 /usr/local/bin/ant \
    && ant --version

# ── mise: pre-warmed Node 22 + Python 3.12 for agent-authored code ────────────
RUN mkdir -p /opt/mise \
    && case "${TARGETARCH:-amd64}" in \
         amd64) MISE_ARCH=x64 ;; \
         arm64) MISE_ARCH=arm64 ;; \
         *) echo "unsupported arch: ${TARGETARCH}" && exit 1 ;; \
       esac \
    && curl -fsSL "https://mise.jdx.dev/mise-latest-linux-${MISE_ARCH}" -o /usr/local/bin/mise \
    && chmod 0755 /usr/local/bin/mise \
    && mise --version

RUN printf 'export MISE_DATA_DIR=/opt/mise\nexport MISE_CONFIG_DIR=/opt/mise\nexport PATH=/opt/mise/shims:$PATH\n' \
      > /etc/profile.d/mise.sh

# Anthropic's `ant beta:worker run` executes agent bash tool calls as non-login
# `sh -c` sessions, which do NOT source /etc/profile.d. mise shims are inert
# without MISE_DATA_DIR/MISE_CONFIG_DIR, so node/python would fail to resolve.
# Bake the resolution into /etc/environment (read by PAM for every session) and
# drop a profile fallback so both login and non-login shells find the runtimes.
RUN printf 'MISE_DATA_DIR=/opt/mise\nMISE_CONFIG_DIR=/opt/mise\nPATH=/opt/mise/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n' \
      > /etc/environment

ENV PATH=/opt/mise/shims:$PATH

RUN mise use -g node@22 \
    && mise use -g python@3.12 \
    && mise reshim \
    && node -v \
    && python --version

# ── Session filesystem contract ───────────────────────────────────────────────
# /workspace: tool working directory + skills download target.
# /mnt/session/outputs: the worker harness instructs Claude to write final
# deliverables here; the orchestrator retrieves it before sandbox deletion.
RUN mkdir -p /workspace /mnt/session/outputs

WORKDIR /workspace
CMD ["/bin/bash"]
