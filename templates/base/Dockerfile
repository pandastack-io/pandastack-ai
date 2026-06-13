# PandaStack template · base
#
# The universal, language-agnostic app-runtime base for the git-driven app
# hosting feature (Vercel/Render/Railway-style deploys). This is the production
# parity of the local `seed_base_template` in scripts/mac-local-e2e.sh — keep
# the two in sync.
#
# Philosophy: do NOT bake a template per language. Bake ONE good base with:
#   - build essentials (git curl ca-certificates build-essential xz-utils
#     unzip pkg-config) so most native build steps work out of the box;
#   - mise (https://mise.jdx.dev) — a single static binary that manages runtime
#     versions for node/python/go/ruby/bun/deno/java/etc. It reads idiomatic
#     version files (.nvmrc / .python-version / .tool-versions / mise.toml), so
#     a repo declares exactly what it needs and the build pipeline runs
#     `mise install` on demand for anything not pre-warmed;
#   - pre-warmed Node 22 LTS + Python 3.12 + Go (latest) + Bun via `mise use -g`
#     plus the pnpm and yarn package managers, so the common JS/TS/Python/Go
#     cases deploy instantly (no runtime download at build time).
#
# Deterministic, root-owned mise layout (resolvable from the non-login `sh -c`
# exec sessions the build pipeline uses):
#   MISE_DATA_DIR=/opt/mise  MISE_CONFIG_DIR=/opt/mise  shims=/opt/mise/shims
#
# Memory note: Firecracker snapshot restore freezes RAM at bake time, so the
# app runtime memory is governed by THIS base, not the per-app memory_mb. Size
# it for builds.
#
# Bake:
#   pandastack template build -f Dockerfile -n base --size-mb 12288

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive \
    MISE_DATA_DIR=/opt/mise \
    MISE_CONFIG_DIR=/opt/mise \
    MISE_YES=1

# ── Build essentials + microVM init/sshd contract ────────────────────────────
# systemd-sysv provides /sbin/init (without it the Firecracker kernel falls
# through to /bin/sh and nothing boots); openssh-server provides sshd, the
# host↔guest bridge the agent uses for exec/fs and the snapshot-bake readiness
# probe. The agent injects its authorized_keys + sshd overrides at create time
# (see agent/internal/guest/keys.go InjectInto), so the image only needs the
# init system + the sshd binary baked in.
RUN apt-get update && apt-get install -y --no-install-recommends \
      git curl ca-certificates build-essential xz-utils unzip pkg-config \
      systemd systemd-sysv openssh-server sudo \
    && rm -rf /var/lib/apt/lists/*

# ── mise: single static binary, deterministic root-owned layout ───────────────
# TARGETARCH is auto-populated by BuildKit/buildx. mise uses "x64" (not "amd64")
# in its release URLs, so map it. This makes the image build for both the
# linux/amd64 CI/prod path and local arm64 (Apple Silicon) builds.
ARG TARGETARCH
RUN mkdir -p /opt/mise \
    && case "${TARGETARCH:-amd64}" in \
         amd64) MISE_ARCH=x64 ;; \
         arm64) MISE_ARCH=arm64 ;; \
         *) echo "unsupported arch: ${TARGETARCH}" && exit 1 ;; \
       esac \
    && curl -fsSL "https://mise.jdx.dev/mise-latest-linux-${MISE_ARCH}" -o /usr/local/bin/mise \
    && chmod 0755 /usr/local/bin/mise \
    && mise --version

# Make the mise env + shims resolvable for login shells too (the build pipeline
# also sets these explicitly for its non-login sh -c exec sessions).
RUN printf 'export MISE_DATA_DIR=/opt/mise\nexport MISE_CONFIG_DIR=/opt/mise\nexport PATH=/opt/mise/shims:$PATH\n' \
      > /etc/profile.d/mise.sh

# Put the mise shims on PATH for the remaining build steps below.
ENV PATH=/opt/mise/shims:$PATH

# ── Pre-warm the common runtimes globally (precompiled, no source build) ──────
# Node 22 LTS, Python 3.12, Go (latest stable) and Bun, then the pnpm + yarn
# package managers (installed against the pre-warmed Node so their shims exist).
RUN mise use -g node@22 \
    && mise use -g python@3.12 \
    && mise use -g go@latest \
    && mise use -g bun@latest \
    && mise reshim \
    && npm install -g pnpm yarn \
    && mise reshim \
    && node -v \
    && python --version \
    && go version \
    && bun --version \
    && pnpm --version \
    && yarn --version

WORKDIR /workspace
CMD ["/bin/bash"]
