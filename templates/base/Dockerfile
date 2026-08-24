# PandaStack template · base
#
# The universal, language-agnostic sandbox runtime. This is the production
# parity of the local `seed_base_template` in scripts/mac-local-e2e.sh — keep
# the two in sync.
#
# Philosophy: do NOT bake a template per language. Bake ONE good base with:
#   - build essentials (git curl ca-certificates build-essential xz-utils
#     unzip pkg-config) so most native build steps work out of the box;
#   - mise (https://mise.jdx.dev) — a single static binary that manages runtime
#     versions for node/python/go/ruby/bun/deno/java/etc. It reads idiomatic
#     version files (.nvmrc / .python-version / .tool-versions / mise.toml), so
#     a project declares exactly what it needs and `mise install` fetches on
#     demand anything that is not pre-warmed;
#   - pre-warmed Node 24 LTS + Python 3.12 + Go (latest) + Bun via `mise use -g`
#     plus the pnpm and yarn package managers, so the common JS/TS/Python/Go
#     cases run instantly (no runtime download on first use).
#
# Deterministic, root-owned mise layout (resolvable from the non-login `sh -c`
# exec sessions the platform uses):
#   MISE_DATA_DIR=/opt/mise  MISE_CONFIG_DIR=/opt/mise  shims=/opt/mise/shims
#
# Memory note: Firecracker snapshot restore freezes RAM at bake time, so the
# guest's usable memory is governed by THIS base (see MEMORY_MB[base] in
# scripts/bake-templates.sh — 4096 MB). Size it for BUILDS: Next/Vite/tsc prod
# builds routinely spike 2.5-4 GiB and OOM at 2 GiB. NODE_OPTIONS below raises
# Node's old-space heap so builds actually use the baked RAM (Node under-detects
# available memory in a small guest).
#
# Bake:
#   pandastack template build -f Dockerfile -n base --size-mb 12288

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive \
    MISE_DATA_DIR=/opt/mise \
    MISE_CONFIG_DIR=/opt/mise \
    MISE_YES=1 \
    NODE_OPTIONS=--max-old-space-size=3072

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

# ── Resolve "localhost" ───────────────────────────────────────────────────────
# A Docker image's /etc/hosts is populated by the Docker RUNTIME (bind-mounted
# read-only during build — `RUN > /etc/hosts` fails with EROFS), and it is NOT
# baked into the image. So when we flatten this rootfs and boot it as a
# Firecracker VM, /etc/hosts is EMPTY and `localhost` does not resolve (getent
# hosts localhost returns nothing). Many tools assume localhost resolves
# (build tools, dev servers, anything binding or probing 127.0.0.1) and fail
# with a confusing "getaddrinfo ENOTFOUND localhost".
#
# Since we can't write /etc/hosts at build time, bake the content to a side file
# and seed /etc/hosts from it on boot via a oneshot systemd unit (the microVM
# boots under systemd, not Docker, so /etc/hosts is writable there). The unit
# only writes when /etc/hosts is empty/absent, so it never clobbers a hosts file
# the agent or a user set up.
RUN printf '127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\tpandastack\n' \
      > /etc/hosts.pandastack-default
RUN printf '%s\n' \
      '[Unit]' \
      'Description=Seed /etc/hosts with loopback names if empty (Firecracker rootfs has no Docker-managed hosts file)' \
      'DefaultDependencies=no' \
      'Before=network-pre.target sysinit.target' \
      'ConditionFileNotEmpty=!/etc/hosts' \
      '[Service]' \
      'Type=oneshot' \
      'ExecStart=/bin/cp /etc/hosts.pandastack-default /etc/hosts' \
      'RemainAfterExit=yes' \
      '[Install]' \
      'WantedBy=sysinit.target' \
      > /etc/systemd/system/pandastack-hosts.service \
    && systemctl enable pandastack-hosts.service

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

# Make the mise env + shims resolvable for EVERY session, not just login shells.
#
# Detached work runs as non-login `setsid sh -c ...` sessions. Those source
# neither /etc/profile.d nor an ENV baked at Docker build time (Docker ENV is
# not injected into the Firecracker microVM). A mise shim is inert without
# MISE_DATA_DIR/MISE_CONFIG_DIR in the env, so `node`/`python3`/`pnpm` fail to
# resolve for detached processes.
#
# Fix: bake the resolution into /etc/environment (read by PAM for every session,
# login or not) AND drop a login-shell profile fallback. Belt-and-suspenders
# below adds real-binary symlinks so even a bare PATH resolves the runtimes.
# NODE_OPTIONS is included here (not just as a Docker ENV) because Docker ENV is
# NOT injected into the Firecracker microVM — /etc/environment IS read by PAM
# for every guest session, so this is what actually reaches processes at
# runtime. Raises Node's old-space heap to ~3 GiB so JS builds use the 4 GiB the
# guest is baked with instead of OOMing at Node's small-guest default.
RUN printf 'MISE_DATA_DIR=/opt/mise\nMISE_CONFIG_DIR=/opt/mise\nPATH=/opt/mise/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\nNODE_OPTIONS=--max-old-space-size=3072\n' \
      > /etc/environment
RUN printf 'export MISE_DATA_DIR=/opt/mise\nexport MISE_CONFIG_DIR=/opt/mise\nexport PATH=/opt/mise/shims:$PATH\nexport NODE_OPTIONS=--max-old-space-size=3072\n' \
      > /etc/profile.d/mise.sh

# Put the mise shims on PATH for the remaining build steps below.
ENV PATH=/opt/mise/shims:$PATH

# ── Pre-warm the common runtimes globally (precompiled, no source build) ──────
# Node 24 LTS, Python 3.12, Go (latest stable) and Bun, then the pnpm + yarn
# package managers (installed against the pre-warmed Node so their shims exist).
RUN mise use -g node@24 \
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

# ── Real-binary symlinks into /usr/local/bin (belt-and-suspenders) ────────────
# The /etc/environment fix above makes the mise SHIMS resolve for non-login
# sessions. But a shim is a wrapper that still needs the mise env; a process
# with a clobbered PATH, or a tool that hardcodes /usr/local/bin/node, can still
# miss it. Symlinking the REAL resolved binaries (mise's global installs) into
# /usr/local/bin — which is on every default PATH — makes the core runtimes
# resolve unconditionally, with zero dependence on shim machinery. `mise which`
# resolves the active global version's absolute path at bake time.
RUN set -eux; \
    for bin in node npm npx python python3 pip pip3 go bun pnpm yarn; do \
      target="$(mise which "$bin" 2>/dev/null || true)"; \
      if [ -n "$target" ] && [ -x "$target" ]; then \
        ln -sf "$target" "/usr/local/bin/$bin"; \
      fi; \
    done; \
    # Verify the critical runtimes resolve with an EMPTY environment (the
    # worst case a detached setsid child could inherit). If this fails the bake
    # fails — we never ship a base that can't run node/python. Verify `python`
    # (mise's canonical tool name — always symlinked by the loop); the
    # `python3` alias is best-effort (falls back to python) since `mise which
    # python3` is not guaranteed to resolve on every mise version.
    env -i /usr/local/bin/node -v; \
    env -i /usr/local/bin/python --version; \
    env -i /usr/local/bin/python3 --version || ln -sf /usr/local/bin/python /usr/local/bin/python3

WORKDIR /workspace
CMD ["/bin/bash"]
