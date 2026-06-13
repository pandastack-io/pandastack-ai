#!/usr/bin/env bash
# Bake the first-party PandaStack templates on a Firecracker host.
#
# Usage (run as root on the agent host):
#   sudo bake-templates.sh                 # bake all
#   sudo bake-templates.sh code-interpreter browser   # bake specific
#   sudo FORCE=1 bake-templates.sh         # rebake even if already present
#
# Each template is created by cloning the base rootfs, chroot-installing the
# template-specific packages, then registering it at $TEMPLATES/<name>/.

set -euo pipefail

DATA_DIR="${DATA_DIR:-/var/lib/pandastack}"
BASE_NAME="${BASE_NAME:-ubuntu-24.04-net}"
BASE_ROOTFS="$DATA_DIR/templates/$BASE_NAME/rootfs.ext4"
TEMPLATES="$DATA_DIR/templates"
WORK="${WORK:-/var/lib/pandastack-work/bake}"
FORCE="${FORCE:-0}"
KERNEL="${KERNEL:-vmlinux-5.10}"

if [[ $EUID -ne 0 ]]; then
  echo "must be root" >&2; exit 1
fi
if [[ ! -f "$BASE_ROOTFS" ]]; then
  echo "base rootfs missing: $BASE_ROOTFS" >&2; exit 1
fi

mkdir -p "$WORK"

log() { printf '\e[36m[%(%H:%M:%S)T]\e[0m %s\n' -1 "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

# ---- template definitions ----------------------------------------------------
# Each template is a function `tpl::<name>` that runs inside the chroot.
# The wrapper below sets up DEBIAN_FRONTEND, resolv.conf, etc.

tpl::code-interpreter() {
  # Enterprise-grade sandbox: Python 3.11 + Node.js 22 LTS + full DS/AI/Playwright stack.
  # Must match templates/code-interpreter/Dockerfile.
  export DEBIAN_FRONTEND=noninteractive
  export PLAYWRIGHT_BROWSERS_PATH=/opt/playwright

  # ── System packages ──────────────────────────────────────────────────────
  apt-get update -qq
  apt-get install -y --no-install-recommends \
    ca-certificates curl wget git \
    procps net-tools iproute2 build-essential \
    libpq-dev gcc xz-utils \
    libgl1 libglib2.0-0 libsndfile1 libsm6 libxext6 libxrender-dev ffmpeg \
    busybox e2fsprogs \
    systemd systemd-sysv openssh-server sudo \
    chrony socat fuse3 iptables nfs-common \
    software-properties-common

  # ── Python 3.11 ──────────────────────────────────────────────────────────
  add-apt-repository -y ppa:deadsnakes/ppa || true
  apt-get update -qq
  apt-get install -y --no-install-recommends \
    python3.11 python3.11-dev python3.11-venv python3.11-distutils libpython3.11-dev
  curl -sS https://bootstrap.pypa.io/get-pip.py | python3.11
  ln -sf /usr/bin/python3.11 /usr/bin/python3
  ln -sf /usr/bin/python3.11 /usr/bin/python
  ln -sf /usr/local/bin/pip /usr/bin/pip3
  ln -sf /usr/local/bin/pip /usr/bin/pip

  # ── Node.js 22 LTS ───────────────────────────────────────────────────────
  curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
  apt-get install -y --no-install-recommends nodejs
  npm install -g yarn typescript ts-node

  # ── System-wide PATH ─────────────────────────────────────────────────────
  printf 'PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"\n' \
    > /etc/environment
  printf 'export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"\n' \
    > /etc/profile.d/pandastack-path.sh
  chmod +x /etc/profile.d/pandastack-path.sh

  # ── Full DS + AI stack ───────────────────────────────────────────────────
  pip install --no-cache-dir \
    jupyter-server==2.16.0 ipykernel==6.29.5 ipython==9.2.0 notebook jupyterlab \
    numpy==2.3.5 pandas==2.2.3 scipy==1.17.1 scikit-learn==1.6.1 scikit-image==0.25.2 \
    xarray==2025.4.0 statsmodels sympy==1.14.0 numba==0.63.1 \
    matplotlib==3.10.8 seaborn==0.13.2 plotly==6.0.1 bokeh==3.9.0 kaleido==1.0.0 \
    pillow==12.1.1 imageio==2.37.3 opencv-python==4.11.0.86 \
    librosa==0.11.0 soundfile==0.13.1 \
    nltk==3.9.3 spacy==3.8.11 textblob==0.19.0 gensim==4.4.0 \
    openpyxl==3.1.5 xlrd==2.0.2 python-docx==1.1.2 \
    aiohttp==3.13.3 requests==2.32.5 httpx==0.28.1 urllib3==2.6.3 \
    beautifulsoup4==4.14.3 orjson==3.11.7 \
    yfinance joblib==1.5.3 \
    psycopg2-binary dill==0.3.9 sqlalchemy==2.0.49 alembic \
    fastapi "uvicorn[standard]" pydantic==2.11.10 python-dotenv==1.1.1 \
    openai==2.31.0 anthropic==0.94.0 \
    pyarrow==23.0.1 \
    pytest==8.3.5 \
    playwright \
    'python-lsp-server>=1.14' pylsp-rope pyflakes pycodestyle

  python3.11 -m ipykernel install --sys-prefix
  python3.11 -m playwright install chromium --with-deps

  # ── Persist PLAYWRIGHT_BROWSERS_PATH across all Python processes ──────────
  SITEDIR=$(python3.11 -c "import site; print(site.getsitepackages()[0])")
  printf 'import os\nif not os.environ.get("PLAYWRIGHT_BROWSERS_PATH"):\n    os.environ["PLAYWRIGHT_BROWSERS_PATH"] = "/opt/playwright"\n' \
    > "$SITEDIR/sitecustomize.py"

  mkdir -p /workspace
}

# Shared helper: install the pandastack-autostart systemd service into the
# *current* chroot rootfs. Safe to call from any tpl function; idempotent.
# Lets dev-server templates ship an /etc/pandastack/autostart.sh that runs
# on every boot, without requiring the base rootfs to be rebaked.
_install_autostart_unit() {
  mkdir -p /etc/pandastack
  cat > /usr/local/bin/pandastack-autostart <<'AUTO'
#!/bin/sh
set -e
SCRIPT=/etc/pandastack/autostart.sh
[ -x "$SCRIPT" ] || { echo "pandastack-autostart: no $SCRIPT, nothing to do"; exit 0; }
exec /bin/bash -lc "$SCRIPT"
AUTO
  chmod 0755 /usr/local/bin/pandastack-autostart

  cat > /etc/systemd/system/pandastack-autostart.service <<'UNIT'
[Unit]
Description=PandaStack template autostart hook
After=pandastack-init.service network-online.target sshd.service
Wants=network-online.target
ConditionPathExists=/etc/pandastack/autostart.sh

[Service]
Type=simple
ExecStartPre=/bin/mkdir -p /workspace
ExecStart=/usr/local/bin/pandastack-autostart
Restart=on-failure
RestartSec=3s
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
UNIT
  systemctl enable pandastack-autostart.service
}

tpl::browser() {
  # Headless Chromium + Playwright (Node) PLUS the Python crawl4ai extraction
  # stack (merged from the former 'crawler' template). One browser-automation
  # box for scraping, RPA, screenshots, and LLM-friendly web extraction.
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates curl git xvfb ffmpeg chromium-browser \
    python3 python3-pip python3-venv \
    libnss3 libatk-bridge2.0-0 libgbm1 libasound2t64
  curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nodejs
  npm i -g playwright --silent || true
  python3 -m pip install --break-system-packages --no-cache-dir \
    'playwright' 'crawl4ai' 'readability-lxml' 'trafilatura' 'httpx' \
    'beautifulsoup4' 'lxml' || true
  npx --yes playwright install chromium || true
  python3 -m playwright install chromium || true
}

tpl::agent() {
  # Unified coding-agent CLI box (merged from claude-code, codex, opencode,
  # amp, devin). Ships every real terminal coding agent pre-installed; the user
  # picks which to run by providing the matching API key in the sandbox env
  # (ANTHROPIC_API_KEY -> claude, OPENAI_API_KEY -> codex, BYO model -> opencode).
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates curl git ripgrep fd-find bat tmux \
    python3 python3-pip python3-venv
  curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nodejs
  npm i -g @anthropic-ai/claude-code @openai/codex opencode-ai --silent || true
}

tpl::claude-agent() {
  # Self-hosted sandbox runtime for Claude Managed Agents
  # (https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes).
  #
  # Anthropic runs the agent loop; this template is where the agent's tool
  # calls execute. The reference orchestrator (cookbook/claude-managed-agents)
  # creates one sandbox per session from this template and launches the
  # pre-built in-guest runner `ant beta:worker run --workdir /workspace`, which
  # downloads the agent's skills, executes tool calls (bash/read/write/edit/
  # glob/grep), heartbeats its work lease, and exits when the session goes idle.
  #
  # Keep this in sync with templates/claude-agent/Dockerfile (the CLI/API build
  # path used for local dev). This chroot path is the production bake.
  local ANT_VERSION=1.12.1
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates curl git build-essential xz-utils unzip zip pkg-config \
    jq ripgrep tar

  # ── ant CLI: Anthropic's pre-built environment worker ───────────────────────
  # Single static Go binary; only runtime dependency is /bin/bash. Release
  # assets use amd64/arm64 directly (no x64 mapping, unlike mise).
  local arch
  arch="$(dpkg --print-architecture)"  # amd64 | arm64
  curl -fsSL "https://github.com/anthropics/anthropic-cli/releases/download/v${ANT_VERSION}/ant_${ANT_VERSION}_linux_${arch}.tar.gz" \
    | tar -xz -C /usr/local/bin ant
  chmod 0755 /usr/local/bin/ant
  ant --version

  # ── mise: pre-warmed Node 22 + Python 3.12 for agent-authored code ──────────
  local mise_arch
  case "$arch" in
    amd64) mise_arch=x64 ;;
    arm64) mise_arch=arm64 ;;
    *) echo "unsupported arch: $arch" && exit 1 ;;
  esac
  mkdir -p /opt/mise
  curl -fsSL "https://mise.jdx.dev/mise-latest-linux-${mise_arch}" -o /usr/local/bin/mise
  chmod 0755 /usr/local/bin/mise
  export MISE_DATA_DIR=/opt/mise MISE_CONFIG_DIR=/opt/mise MISE_YES=1
  mise use -g node@22
  mise use -g python@3.12
  mise reshim
  PATH=/opt/mise/shims:$PATH node -v
  PATH=/opt/mise/shims:$PATH python --version

  printf 'export MISE_DATA_DIR=/opt/mise\nexport MISE_CONFIG_DIR=/opt/mise\nexport PATH=/opt/mise/shims:$PATH\n' \
    > /etc/profile.d/mise.sh

  # `ant beta:worker run` executes agent bash tool calls as non-login `sh -c`
  # sessions, which do NOT source /etc/profile.d. mise shims are inert without
  # MISE_DATA_DIR/MISE_CONFIG_DIR, so node/python would fail to resolve. Bake
  # the resolution into /etc/environment (read by PAM for every session).
  printf 'MISE_DATA_DIR=/opt/mise\nMISE_CONFIG_DIR=/opt/mise\nPATH=/opt/mise/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n' \
    > /etc/environment

  # ── Session filesystem contract ─────────────────────────────────────────────
  # /workspace: tool working directory + skills download target.
  # /mnt/session/outputs: where the agent is told to write deliverables; the
  # orchestrator retrieves it before sandbox deletion.
  mkdir -p /workspace /mnt/session/outputs
}

# ---- size + ordering -------------------------------------------------------

# Rootfs image sizes (megabytes on disk).
declare -A SIZE_MB=(
  # 'base' is the universal app-runtime substrate for the git-driven apps
  # feature (mise + Node 22/Python 3.12/Go/Bun + pnpm/yarn). It is NOT a
  # user-facing sandbox template, but the CI bake-templates.yml workflow reads
  # this array to size its rootfs. 12 GiB matches templates/base/Dockerfile.
  [base]=12288
  [code-interpreter]=12288
  # 'browser' merges the former 'crawler' Python stack — sized up from 3072.
  [browser]=4096
  # 'agent' is the unified coding-agent CLI box (replaces claude-code, codex,
  # opencode, amp, devin): Node 22 + git + three real agent CLIs.
  [agent]=3072
  # 'claude-agent' is the Claude Managed Agents self-hosted sandbox runtime:
  # ant CLI + mise (Node 22 / Python 3.12). 8 GiB fits the runtimes + the
  # agent's downloaded skills + working files.
  [claude-agent]=8192
  [postgres-16]=12288
)

# Guest vCPU count — must match marketing page (pandastack.ai/templates/).
tpl::postgres-16() {
  # Enterprise-grade PostgreSQL 16 sandbox.
  # Installs: PostgreSQL 16 (PGDG), PgBouncer, pgvector, and pds-query-broker
  # (the REST JSON-over-SQL bridge compiled in templates/postgres-16/query-broker/).
  #
  # Bake-time init (this function): pds-pg-init creates the cluster, role,
  # database, and extensions inside the chroot so the template snapshot starts
  # with a fully initialised PostgreSQL cluster.
  #
  # Every sandbox boot (autostart.sh): PostgreSQL starts in ~1s (cluster
  # already exists), credentials are rotated with a single ALTER USER, and
  # ready.json is written — total ready time < 5s.
  export DEBIAN_FRONTEND=noninteractive

  # ── Base dependencies ───────────────────────────────────────────────────────
  apt-get update -qq
  apt-get install -y --no-install-recommends \
    ca-certificates curl gnupg lsb-release locales tzdata \
    openssl procps net-tools iproute2 \
    sudo

  # ── Locale ─────────────────────────────────────────────────────────────────
  locale-gen en_US.UTF-8
  update-locale LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8

  # ── PostgreSQL 16 from PGDG ─────────────────────────────────────────────────
  curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
    | gpg --dearmor -o /etc/apt/trusted.gpg.d/postgresql.gpg
  echo "deb [signed-by=/etc/apt/trusted.gpg.d/postgresql.gpg] \
    http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
    > /etc/apt/sources.list.d/pgdg.list
  apt-get update -qq
  apt-get install -y --no-install-recommends \
    postgresql-16 \
    postgresql-client-16 \
    postgresql-16-pgvector \
    postgresql-contrib \
    pgbouncer

  # ── Compile and install pds-query-broker ────────────────────────────────────
  # Go is fetched transiently; removed after compilation to keep rootfs lean.
  GO_VERSION=1.23.4
  GO_ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" \
    | tar -C /usr/local -xz
  export PATH="/usr/local/go/bin:$PATH"

  BROKER_SRC="/opt/pds-query-broker-src"
  mkdir -p "${BROKER_SRC}"
  # Source + verified go.sum copied into chroot by bake_one before entering chroot.
  cp /tmp/pds-broker-src/main.go  "${BROKER_SRC}/"
  cp /tmp/pds-broker-src/go.mod   "${BROKER_SRC}/"
  cp /tmp/pds-broker-src/go.sum   "${BROKER_SRC}/"

  (
    cd "${BROKER_SRC}"
    # Use go.sum for verified download; skip tidy (sum already committed).
    GOPATH="/tmp/gopath" GOMODCACHE="/tmp/gomodcache" \
      go mod download
    GOPATH="/tmp/gopath" GOMODCACHE="/tmp/gomodcache" \
      CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath \
        -o /usr/local/bin/pds-query-broker .
  )

  # Remove Go toolchain + source to save space.
  rm -rf /usr/local/go "${BROKER_SRC}" /tmp/gopath /tmp/gomodcache

  # ── Install scripts ─────────────────────────────────────────────────────────
  mkdir -p /etc/pandastack
  cp /tmp/pds-broker-src/autostart.sh    /etc/pandastack/autostart.sh
  cp /tmp/pds-broker-src/healthcheck.sh  /usr/local/bin/pds-pg-health
  cp /tmp/pds-broker-src/pgbouncer.ini   /etc/pandastack/pgbouncer.ini
  chmod +x \
    /usr/local/bin/pds-query-broker \
    /usr/local/bin/pds-pg-health \
    /etc/pandastack/autostart.sh

  # ── Install pandastack-autostart systemd unit ───────────────────────────────
  _install_autostart_unit

  # ── Configure the auto-created cluster (Sandflare pattern) ──────────────────
  # apt-get install postgresql-16 auto-creates a cluster with a standard conf.
  # We patch it in-place (pure sed/echo — no custom UTF-8 conf file) so
  # pg_ctl (C binary) can start PG on every cold boot without issues.

  PG_CONF="/etc/postgresql/16/main/postgresql.conf"
  PG_HBA="/etc/postgresql/16/main/pg_hba.conf"

  # listen on all interfaces (agent dials guest_ip:5432 through tunnel)
  sed -i "s/^#*listen_addresses\s*=.*/listen_addresses = '*'/" "${PG_CONF}"
  grep -q "^listen_addresses" "${PG_CONF}" || echo "listen_addresses = '*'" >> "${PG_CONF}"

  # performance tuning for AI-agent workloads
  cat >> "${PG_CONF}" <<'PGCONF'

# PandaStack tuning (appended by bake-templates.sh)
shared_buffers              = 256MB
effective_cache_size        = 768MB
work_mem                    = 8MB
maintenance_work_mem        = 64MB
huge_pages                  = off
wal_level                   = logical
max_wal_size                = 256MB
min_wal_size                = 32MB
wal_buffers                 = 16MB
checkpoint_completion_target = 0.9
max_replication_slots       = 5
max_wal_senders             = 5
random_page_cost            = 1.1
effective_io_concurrency    = 200
default_statistics_target   = 200
jit                         = off
autovacuum_vacuum_scale_factor   = 0.01
autovacuum_analyze_scale_factor  = 0.005
shared_preload_libraries    = 'pg_stat_statements'
pg_stat_statements.max      = 5000
pg_stat_statements.track    = all
PGCONF

  # allow connections from any host (TAP network — microVM is isolated)
  grep -q "^host all all 0\.0\.0\.0" "${PG_HBA}" || \
    echo "host all all 0.0.0.0/0 scram-sha-256" >> "${PG_HBA}"
  grep -q "^host all all ::/0" "${PG_HBA}" || \
    echo "host all all ::/0 scram-sha-256" >> "${PG_HBA}"

  # ── Directories PgBouncer needs at runtime ──────────────────────────────────
  mkdir -p /etc/pgbouncer /var/log/pgbouncer /var/run/pgbouncer
  chown postgres:postgres /var/log/pgbouncer /var/run/pgbouncer
}

declare -A CPU_COUNT=(
  [base]=2
  [code-interpreter]=2
  [browser]=4
  [agent]=2
  [claude-agent]=2
  [postgres-16]=2
)

# Guest RAM in MB — must match marketing page.
declare -A MEMORY_MB=(
  [base]=2048
  [code-interpreter]=2048
  [browser]=4096
  [agent]=2048
  [claude-agent]=2048
  [postgres-16]=1024
)

ALL_TEMPLATES=(
  code-interpreter
  browser
  agent
  claude-agent
  postgres-16
)

# ---- bake one template -----------------------------------------------------

bake_one() {
  local name="$1"
  local fn="tpl::${name}"
  if ! declare -F "$fn" >/dev/null; then
    die "unknown template: $name"
  fi
  local dst="$TEMPLATES/$name"
  if [[ -f "$dst/rootfs.ext4" && "$FORCE" != "1" ]]; then
    log "SKIP $name (already baked; FORCE=1 to rebake)"
    return 0
  fi

  local size_mb="${SIZE_MB[$name]:-2048}"
  local work_img="$WORK/$name.ext4"
  local mnt="$WORK/$name.mnt"
  local log_file="$WORK/$name.log"

  log "BAKE $name  (size=${size_mb}MB, log=$log_file)"

  rm -f "$work_img"; mkdir -p "$mnt"

  # Clone base and grow to target size. NEVER shrink — truncating below the
  # current ext4 size corrupts the filesystem (mount then errors with
  # "wrong fs type, bad option, bad superblock"). If the base is already
  # larger than the requested size_mb, keep the base size.
  cp --reflink=auto "$BASE_ROOTFS" "$work_img"
  local base_bytes target_bytes
  base_bytes=$(stat -c%s "$work_img")
  target_bytes=$(( size_mb * 1024 * 1024 ))
  if (( target_bytes > base_bytes )); then
    truncate -s "${size_mb}M" "$work_img"
    e2fsck -fy "$work_img" >/dev/null 2>&1 || true
    resize2fs "$work_img" >/dev/null 2>&1 || true
  else
    log "  (base $((base_bytes/1024/1024))MB >= target ${size_mb}MB; keeping base size)"
  fi

  # Mount + bind dev/proc/sys for apt.
  mount -o loop "$work_img" "$mnt"
  mount --bind /dev  "$mnt/dev"
  mount --bind /proc "$mnt/proc"
  mount --bind /sys  "$mnt/sys"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf"

  # For postgres-16: copy broker source + config files into the chroot at /tmp/pds-broker-src
  # so tpl::postgres-16 can compile the broker and install configs.
  if [[ "$name" == "postgres-16" ]]; then
    local repo_root
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
    local tpl_dir="${repo_root}/templates/postgres-16"
    mkdir -p "$mnt/tmp/pds-broker-src"
    cp "${tpl_dir}/query-broker/main.go"    "$mnt/tmp/pds-broker-src/"
    cp "${tpl_dir}/query-broker/go.mod"     "$mnt/tmp/pds-broker-src/"
    cp "${tpl_dir}/query-broker/go.sum"     "$mnt/tmp/pds-broker-src/"
    cp "${tpl_dir}/scripts/init-db.sh"      "$mnt/tmp/pds-broker-src/init-db.sh"
    cp "${tpl_dir}/scripts/autostart.sh"    "$mnt/tmp/pds-broker-src/autostart.sh"
    cp "${tpl_dir}/scripts/healthcheck.sh"  "$mnt/tmp/pds-broker-src/healthcheck.sh"
    cp "${tpl_dir}/conf/postgresql.conf"    "$mnt/tmp/pds-broker-src/postgresql.conf"
    cp "${tpl_dir}/conf/pg_hba.conf"        "$mnt/tmp/pds-broker-src/pg_hba.conf"
    cp "${tpl_dir}/conf/pgbouncer.ini"      "$mnt/tmp/pds-broker-src/pgbouncer.ini"
    log "  copied postgres-16 sources into chroot"
  fi

  # Run template-specific installer inside chroot.
  local rc=0
  (
    set -euo pipefail
    export -f "$fn" _install_autostart_unit
    chroot "$mnt" /bin/bash -c "
      set -euo pipefail
      export DEBIAN_FRONTEND=noninteractive
      # Enable universe + updates + security so most packages resolve.
      cat > /etc/apt/sources.list <<'SRC'
deb http://archive.ubuntu.com/ubuntu noble main universe
deb http://archive.ubuntu.com/ubuntu noble-updates main universe
deb http://security.ubuntu.com/ubuntu noble-security main universe
SRC
      apt-get update -qq
      $(declare -f _install_autostart_unit)
      $(declare -f "$fn")
      $fn
      # cleanup to shrink size
      apt-get clean
      rm -rf /var/lib/apt/lists/* /tmp/* /root/.cache /root/.npm /root/.local/share/playwright || true
    "
  ) >"$log_file" 2>&1 || rc=$?

  # Always unmount.
  umount "$mnt/dev"  || true
  umount "$mnt/proc" || true
  umount "$mnt/sys"  || true

  # Platform-injected guest DNS (do this LAST, after the chroot install).
  # Line ~398 copied the BUILD HOST's /etc/resolv.conf in so apt could resolve
  # inside the chroot — but that is frequently the systemd-resolved 127.0.0.53
  # stub, which is useless inside a NATID guest (no resolved running there, and
  # the agent does not push DNS over vsock on restore). Overwrite it with public
  # resolvers so EVERY baked template ships working DNS regardless of the host.
  rm -f "$mnt/etc/resolv.conf"
  printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > "$mnt/etc/resolv.conf"

  sync
  umount "$mnt"      || true

  if [[ $rc -ne 0 ]]; then
    log "FAIL $name  (rc=$rc)"
    tail -20 "$log_file" >&2 || true
    return $rc
  fi

  # Install + meta.
  mkdir -p "$dst"
  mv "$work_img" "$dst/rootfs.ext4"
  local cpu_count="${CPU_COUNT[$name]:-2}"
  local memory_mb="${MEMORY_MB[$name]:-2048}"
  cat > "$dst/meta.json" <<JSON
{
  "name": "$name",
  "kernel": "$KERNEL",
  "arch": "x86_64",
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "size_mb": $size_mb,
  "cpu": $cpu_count,
  "memory_mb": $memory_mb,
  "base": "$BASE_NAME"
}
JSON
  local bytes
  bytes=$(stat -c%s "$dst/rootfs.ext4")
  log "OK   $name  ($(numfmt --to=iec --suffix=B "$bytes"))"

  # Invalidate any pre-existing memory snapshot for this template: the agent
  # will rebuild it on next sandbox creation so changes (autostart units,
  # pre-baked starter apps, …) actually take effect.
  local snap_dir="$DATA_DIR/template-snaps/$name"
  if [[ -d "$snap_dir" ]]; then
    log "purge template-snap $snap_dir (will be rebuilt by agent on next sandbox)"
    rm -rf "$snap_dir"
  fi
}

# ---- main ------------------------------------------------------------------

main() {
  local list=("$@")
  if [[ ${#list[@]} -eq 0 ]]; then
    list=("${ALL_TEMPLATES[@]}")
  fi
  log "baking ${#list[@]} template(s) into $TEMPLATES"
  local failed=()
  for t in "${list[@]}"; do
    if ! bake_one "$t"; then
      failed+=("$t")
    fi
  done
  log "---"
  log "succeeded: $(( ${#list[@]} - ${#failed[@]} )) / ${#list[@]}"
  if [[ ${#failed[@]} -gt 0 ]]; then
    log "failed: ${failed[*]}"
    exit 1
  fi
}

main "$@"
