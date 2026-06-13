#!/usr/bin/env bash
# Fully automated PandaStack local E2E bootstrap for standalone Linux.
# Tested on Ubuntu 22.04, Ubuntu 24.04, and Debian 12 (x86_64 and aarch64).
#
# Runs Firecracker directly on the Linux host (no Lima/no nested VM), with
# Postgres, ClickHouse, API, dashboard, and agent all on this machine.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="$REPO_ROOT/.local-state/linux-local-e2e"
BIN_DIR="$STATE_DIR/bin"
LOG_DIR="$STATE_DIR/logs"
TOKEN="pds_local_dev_token"
NODE_TOKEN="pds_local_node_token"
PANDASTACK_STUB_USER_EMAIL="${PANDASTACK_STUB_USER_EMAIL:-dev@local.pandastack}"
PANDASTACK_STUB_USER_ID="${PANDASTACK_STUB_USER_ID:-00000000-0000-0000-0000-000000000001}"
PANDASTACK_STUB_ORG_ID="${PANDASTACK_STUB_ORG_ID:-00000000-0000-0000-0000-000000000002}"
PANDASTACK_STUB_WORKSPACE="${PANDASTACK_STUB_WORKSPACE:-local-dev}"
WORKSPACE="$PANDASTACK_STUB_WORKSPACE"
COMPOSE=(docker compose -f "$REPO_ROOT/docker-compose.dev.yml")
API_PID="$STATE_DIR/api.pid"
DASHBOARD_PID="$STATE_DIR/dashboard.pid"
AGENT_PID="$STATE_DIR/agent.pid"
TOKENS_FILE="$STATE_DIR/tokens.json"
PG_DSN="postgres://pandastack:pandastack@localhost:5432/pandastack?sslmode=disable"
CLICKHOUSE_URL="http://pandastack:pandastack@localhost:8123/pandastack"
FC_DATA_DIR="${FC_DATA_DIR:-/var/lib/pandastack}"

GREEN='\033[0;32m'; YELLOW='\033[0;33m'; RED='\033[0;31m'; NC='\033[0m'
step() { printf "\n${GREEN}┌─ %s${NC}\n" "$*"; }
warn() { printf "${YELLOW}!${NC} %s\n" "$*"; }
die()  { printf "${RED}✗ %s${NC}\n" "$*" >&2; exit 1; }

need_linux() {
  [[ "$(uname -s)" == "Linux" ]] || die "This bootstrap is for Linux only. On Mac, use scripts/mac-local-e2e.sh."
  if [[ -r /etc/os-release ]]; then
    . /etc/os-release
    case "${ID:-}" in
      ubuntu|debian) ;;
      *) warn "Detected $PRETTY_NAME. This script targets Ubuntu 22.04+/24.04 and Debian 12. Other distros may work but are untested." ;;
    esac
  fi
  [[ -e /dev/kvm ]] || die "/dev/kvm not available. KVM is required for Firecracker. Enable virtualization in BIOS, or run on a bare-metal/nested-virt-enabled host."
  [[ -r /dev/kvm && -w /dev/kvm ]] || die "/dev/kvm exists but is not readable+writable by $USER. Add yourself to the kvm group: sudo usermod -aG kvm \$USER && newgrp kvm"
}

apt_install_if_missing() {
  local pkg="$1" cmd="$2"
  if command -v "$cmd" >/dev/null 2>&1; then return; fi
  step "Installing $pkg"
  sudo apt-get install -y "$pkg"
}

ensure_prereqs() {
  step "Refreshing apt index"
  sudo apt-get update -y

  apt_install_if_missing curl curl
  apt_install_if_missing jq jq
  apt_install_if_missing git git
  apt_install_if_missing ca-certificates update-ca-certificates
  apt_install_if_missing iptables iptables
  apt_install_if_missing iproute2 ip
  apt_install_if_missing squashfs-tools unsquashfs
  apt_install_if_missing e2fsprogs mkfs.ext4
  apt_install_if_missing kmod modprobe

  if ! command -v docker >/dev/null 2>&1; then
    step "Installing Docker Engine"
    sudo install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor --yes -o /etc/apt/keyrings/docker.gpg
    sudo chmod a+r /etc/apt/keyrings/docker.gpg
    . /etc/os-release
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/${ID} ${VERSION_CODENAME} stable" | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
    sudo apt-get update -y
    sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    sudo usermod -aG docker "$USER" || true
    sudo systemctl enable --now docker
  fi
  docker info >/dev/null 2>&1 || die "Docker is installed but not reachable for $USER. If you were just added to the docker group, run: newgrp docker, then rerun this script."

  if ! command -v go >/dev/null 2>&1 || ! go version 2>/dev/null | grep -qE 'go1\.(2[2-9]|[3-9][0-9])'; then
    step "Installing Go 1.22"
    GO_VERSION="1.22.10"
    GO_ARCH="$(dpkg --print-architecture)"
    case "$GO_ARCH" in amd64) GO_ARCH=amd64;; arm64) GO_ARCH=arm64;; *) die "Unsupported arch: $GO_ARCH";; esac
    curl -fL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" -o /tmp/go.tgz
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf /tmp/go.tgz
    rm /tmp/go.tgz
    export PATH="/usr/local/go/bin:$PATH"
    grep -q '/usr/local/go/bin' "$HOME/.profile" 2>/dev/null || echo 'export PATH=/usr/local/go/bin:$PATH' >> "$HOME/.profile"
  fi

  if ! command -v node >/dev/null 2>&1 || ! node --version 2>/dev/null | grep -qE '^v(2[0-9]|[3-9][0-9])\.'; then
    step "Installing Node.js 20"
    curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash -
    sudo apt-get install -y nodejs
  fi
}

install_firecracker_and_template() {
  step "Installing Firecracker and default template"
  sudo install -d -m 0755 "$FC_DATA_DIR/install" "$FC_DATA_DIR/kernels" "$FC_DATA_DIR/templates/ubuntu-24.04"
  local FC_VERSION="v1.16.0"
  local ARCH
  case "$(uname -m)" in
    x86_64) ARCH="x86_64" ;;
    aarch64) ARCH="aarch64" ;;
    *) die "Unsupported arch for Firecracker: $(uname -m)" ;;
  esac

  if ! command -v firecracker >/dev/null 2>&1; then
    (
      cd "$FC_DATA_DIR/install"
      sudo curl -fL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz" -o firecracker.tgz
      sudo tar -xzf firecracker.tgz
      sudo install -m 0755 "release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" /usr/local/bin/firecracker
      sudo install -m 0755 "release-${FC_VERSION}-${ARCH}/jailer-${FC_VERSION}-${ARCH}" /usr/local/bin/jailer
      sudo rm -rf firecracker.tgz "release-${FC_VERSION}-${ARCH}"
    )
  fi

  if ! ls "$FC_DATA_DIR"/kernels/vmlinux-5.10* >/dev/null 2>&1; then
    local key
    key=$(curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min?prefix=firecracker-ci/v1.13/${ARCH}/vmlinux-5.10&list-type=2" | grep -oE "firecracker-ci/v1.13/${ARCH}/vmlinux-5\.10\.[0-9]+" | sort -V | tail -1)
    [[ -n "$key" ]] || die "Could not list Firecracker CI kernels"
    sudo curl -fL "https://s3.amazonaws.com/spec.ccfc.min/${key}" -o "$FC_DATA_DIR/kernels/$(basename "$key")"
  fi

  if [[ ! -f "$FC_DATA_DIR/templates/ubuntu-24.04/rootfs.ext4" ]]; then
    local key
    key=$(curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min?prefix=firecracker-ci/v1.13/${ARCH}/ubuntu-&list-type=2" | grep -oE "firecracker-ci/v1.13/${ARCH}/ubuntu-[0-9]+\.[0-9]+\.squashfs" | sort -V | tail -1)
    [[ -n "$key" ]] || die "Could not list Firecracker CI rootfs"
    (
      cd "$FC_DATA_DIR/templates/ubuntu-24.04"
      sudo curl -fL "https://s3.amazonaws.com/spec.ccfc.min/${key}" -o ubuntu.squashfs
      sudo rm -rf squashfs-root
      sudo unsquashfs -d squashfs-root ubuntu.squashfs >/dev/null
      sudo truncate -s 10G rootfs.ext4
      sudo mkfs.ext4 -d squashfs-root -F rootfs.ext4 >/dev/null
      sudo rm -rf squashfs-root ubuntu.squashfs
      printf '%s\n' '{"name":"ubuntu-24.04","arch":"'$ARCH'","cpu":1,"memory_mb":256,"disk_gb":10,"kernel":"vmlinux-5.10"}' | sudo tee meta.json >/dev/null
    )
  fi
  sudo chown -R "$USER:$USER" "$FC_DATA_DIR"
  firecracker --version
}

seed_tokens() {
  step "Seeding local dev token"
  mkdir -p "$STATE_DIR" "$LOG_DIR" "$BIN_DIR"
  cat > "$TOKENS_FILE" <<JSON
[
  {"token":"$TOKEN","workspace":"$WORKSPACE","label":"local-e2e","created_at":"2026-01-01T00:00:00Z"}
]
JSON
}

start_databases() {
  step "Starting Postgres and ClickHouse with Docker Compose"
  (cd "$REPO_ROOT" && "${COMPOSE[@]}" up -d postgres clickhouse)
  for _ in {1..60}; do
    docker exec pandastack-dev-postgres-1 pg_isready -U pandastack >/dev/null 2>&1 && break
    sleep 2
  done
  docker exec pandastack-dev-postgres-1 pg_isready -U pandastack >/dev/null 2>&1 || die "Postgres did not become healthy"

  # Stale pg_data volume guard. Postgres only runs initdb on an empty volume;
  # if the volume was created by a prior compose run with different env vars,
  # the pandastack role/db never get created. Detect and bootstrap.
  if ! docker exec pandastack-dev-postgres-1 psql -U pandastack -d pandastack -c '\q' >/dev/null 2>&1; then
    step "Bootstrapping pandastack role/db (stale pg_data volume detected)"
    bootstrapped=0
    for super_user in postgres pandastack; do
      if docker exec pandastack-dev-postgres-1 psql -U "$super_user" -d postgres -c '\q' >/dev/null 2>&1; then
        docker exec pandastack-dev-postgres-1 psql -U "$super_user" -d postgres -v ON_ERROR_STOP=0 <<SQL >/dev/null 2>&1 || true
DO \$\$BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pandastack') THEN
    CREATE ROLE pandastack LOGIN PASSWORD 'pandastack' SUPERUSER;
  ELSE
    ALTER ROLE pandastack WITH LOGIN PASSWORD 'pandastack' SUPERUSER;
  END IF;
END\$\$;
SQL
        docker exec pandastack-dev-postgres-1 psql -U "$super_user" -d postgres -tc \
          "SELECT 1 FROM pg_database WHERE datname='pandastack'" 2>/dev/null | grep -q 1 || \
          docker exec pandastack-dev-postgres-1 psql -U "$super_user" -d postgres \
            -c "CREATE DATABASE pandastack OWNER pandastack;" >/dev/null 2>&1 || true
        bootstrapped=1
        break
      fi
    done
    if [[ "$bootstrapped" == "0" ]]; then
      die "Cannot bootstrap pandastack role. Stale pg_data volume — run: docker compose -f docker-compose.dev.yml down -v && retry"
    fi
    docker exec pandastack-dev-postgres-1 psql -U pandastack -d pandastack -c '\q' >/dev/null 2>&1 \
      || die "pandastack role bootstrap failed"
  fi

  for _ in {1..60}; do
    curl -fsS http://localhost:8123/ping >/dev/null 2>&1 && break
    sleep 2
  done
  curl -fsS http://localhost:8123/ping >/dev/null 2>&1 || die "ClickHouse did not become healthy"

  # Apply ClickHouse schema (idempotent; CREATE DATABASE/TABLE IF NOT EXISTS).
  # Use clickhouse-client inside the container — the HTTP endpoint rejects
  # multi-statement bodies on CH 24+ ('Multi-statements are not allowed').
  if [[ -f "$REPO_ROOT/agent/internal/clickhouse/schema.sql" ]]; then
    step "Applying ClickHouse schema (pandastack db + tables)"
    docker exec -i pandastack-dev-clickhouse-1 clickhouse-client \
      --user=pandastack --password=pandastack --multiquery \
      < "$REPO_ROOT/agent/internal/clickhouse/schema.sql" \
      || warn "ClickHouse schema apply failed (metrics flush may 404 — non-fatal)"
  fi
}

seed_stub_identity() {
  step "Seeding local stub user, organization, and workspace"
  docker exec -i pandastack-dev-postgres-1 psql -U pandastack -d pandastack -v ON_ERROR_STOP=1 \
    -v user_id="$PANDASTACK_STUB_USER_ID" \
    -v user_email="$PANDASTACK_STUB_USER_EMAIL" \
    -v org_id="$PANDASTACK_STUB_ORG_ID" \
    -v workspace="$PANDASTACK_STUB_WORKSPACE" <<'SQL'
CREATE TABLE IF NOT EXISTS workspaces (
    name TEXT PRIMARY KEY,
    max_sandboxes BIGINT DEFAULT 16,
    max_cpu_total BIGINT DEFAULT 16,
    max_memory_mb_total BIGINT DEFAULT 32768,
    hourly_create_limit BIGINT DEFAULT 200,
    created_at BIGINT
);
CREATE TABLE IF NOT EXISTS orgs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    owner_user_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS org_members (
    org_id UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    email TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL CHECK (role IN ('owner','admin','member')),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
CREATE TABLE IF NOT EXISTS user_current_org (
    user_id TEXT PRIMARY KEY,
    org_id UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO workspaces (name, created_at) VALUES (:'workspace', EXTRACT(EPOCH FROM now())::BIGINT) ON CONFLICT (name) DO NOTHING;
INSERT INTO orgs (id, slug, name, owner_user_id) VALUES (:'org_id'::uuid, :'workspace', 'Local Dev', :'user_id') ON CONFLICT (id) DO NOTHING;
INSERT INTO org_members (org_id, user_id, email, role) VALUES (:'org_id'::uuid, :'user_id', :'user_email', 'owner') ON CONFLICT DO NOTHING;
INSERT INTO user_current_org (user_id, org_id) VALUES (:'user_id', :'org_id'::uuid) ON CONFLICT (user_id) DO NOTHING;
SQL
}

build_and_start_agent() {
  step "Building and starting agent on Linux host"
  mkdir -p "$BIN_DIR" "$LOG_DIR"
  (cd "$REPO_ROOT/agent" && CGO_ENABLED=0 go build -o "$BIN_DIR/pandastack-agent" ./cmd/agent)

  sudo install -m 0755 "$BIN_DIR/pandastack-agent" /usr/local/bin/pandastack-agent
  sudo install -d -m 0755 /etc/pandastack /run/pandastack /var/lib/pandastack
  sudo tee /etc/pandastack/local-e2e.env >/dev/null <<ENV
PANDASTACK_DB_DRIVER=postgres
PANDASTACK_DB_DSN=$PG_DSN
PANDASTACK_NODE_TOKEN=$NODE_TOKEN
PANDASTACK_LISTEN_TCP=127.0.0.1:7070
PANDASTACK_AGENT_ENDPOINT=http://127.0.0.1:7070
PANDASTACK_AGENT_ID=linux-local
PANDASTACK_REGION=local
PANDASTACK_ZONE=linux
PANDASTACK_METRICS_LISTEN=127.0.0.1:9100
PANDASTACK_CLICKHOUSE_URL=$CLICKHOUSE_URL
STRIPE_API_KEY=
STRIPE_WEBHOOK_SECRET=
ENV

  sudo tee /etc/systemd/system/pandastack-agent-local-e2e.service >/dev/null <<'UNIT'
[Unit]
Description=PandaStack local E2E Firecracker host agent
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/pandastack/local-e2e.env
ExecStart=/usr/local/bin/pandastack-agent -socket /run/pandastack/agent.sock -data-dir /var/lib/pandastack -db /var/lib/pandastack/pandastack.db
Restart=on-failure
RestartSec=5
StartLimitInterval=120
StartLimitBurst=5

[Install]
WantedBy=multi-user.target
UNIT
  sudo systemctl daemon-reload
  sudo systemctl enable --now pandastack-agent-local-e2e.service

  for _ in {1..60}; do
    curl -fsS http://localhost:7070/healthz >/dev/null 2>&1 && break
    sleep 2
  done
  curl -fsS http://localhost:7070/healthz >/dev/null 2>&1 || {
    sudo journalctl -u pandastack-agent-local-e2e.service -n 80 --no-pager || true
    die "Agent did not become healthy"
  }
}

build_and_start_api() {
  step "Building and starting API"
  (cd "$REPO_ROOT/api" && go build -o "$BIN_DIR/pandastack-api" ./cmd/api)
  if [[ -f "$API_PID" ]] && kill -0 "$(cat "$API_PID")" 2>/dev/null; then
    warn "API already running with pid $(cat "$API_PID")"
  else
    (
      cd "$REPO_ROOT"
      env \
        PANDASTACK_DB_DSN="$PG_DSN" \
        PANDASTACK_NODE_TOKEN="$NODE_TOKEN" \
        PANDASTACK_REGION=local \
        PANDASTACK_CLICKHOUSE_URL="$CLICKHOUSE_URL" \
        PANDASTACK_METRICS_LISTEN=:9101 \
        PANDASTACK_AUTH_SKIP_PREFIXES=/healthz,/version \
        PANDASTACK_AUTH_MODE=stub \
        PANDASTACK_STUB_USER_EMAIL="$PANDASTACK_STUB_USER_EMAIL" \
        PANDASTACK_STUB_USER_ID="$PANDASTACK_STUB_USER_ID" \
        PANDASTACK_STUB_ORG_ID="$PANDASTACK_STUB_ORG_ID" \
        PANDASTACK_STUB_WORKSPACE="$PANDASTACK_STUB_WORKSPACE" \
        STRIPE_API_KEY= \
        STRIPE_WEBHOOK_SECRET= \
        "$BIN_DIR/pandastack-api" -addr :8080 -token-file "$TOKENS_FILE" \
          >"$LOG_DIR/api.log" 2>&1 &
      echo $! > "$API_PID"
    )
  fi
  for _ in {1..60}; do
    curl -fsS http://localhost:8080/healthz >/dev/null 2>&1 && break
    sleep 2
  done
  curl -fsS http://localhost:8080/healthz >/dev/null 2>&1 || die "API did not become healthy; see $LOG_DIR/api.log"
}

start_dashboard() {
  step "Starting dashboard"
  if [[ -f "$DASHBOARD_PID" ]] && kill -0 "$(cat "$DASHBOARD_PID")" 2>/dev/null; then
    warn "Dashboard already running with pid $(cat "$DASHBOARD_PID")"
    return
  fi
  (cd "$REPO_ROOT/dashboard" && npm ci --no-audit --no-fund)
  (
    cd "$REPO_ROOT/dashboard"
    env \
      NEXT_PUBLIC_PANDASTACK_API=http://localhost:8080 \
      NEXT_PUBLIC_PANDASTACK_AUTH_MODE=stub \
      NEXT_PUBLIC_PANDASTACK_STUB_USER_EMAIL="$PANDASTACK_STUB_USER_EMAIL" \
      NEXT_PUBLIC_PANDASTACK_STUB_USER_ID="$PANDASTACK_STUB_USER_ID" \
      NEXT_PUBLIC_PANDASTACK_STUB_ORG_ID="$PANDASTACK_STUB_ORG_ID" \
      NEXT_PUBLIC_PANDASTACK_STUB_WORKSPACE="$PANDASTACK_STUB_WORKSPACE" \
      npm run dev -- --hostname 127.0.0.1 --port 3000 \
      >"$LOG_DIR/dashboard.log" 2>&1 &
    echo $! > "$DASHBOARD_PID"
  )
  for _ in {1..60}; do
    curl -fsS http://localhost:3000 >/dev/null 2>&1 && break
    sleep 2
  done
  curl -fsS http://localhost:3000 >/dev/null 2>&1 || die "Dashboard did not become healthy; see $LOG_DIR/dashboard.log"
}

smoke_test() {
  step "Running smoke test: create sandbox, exec echo hello, verify dashboard"
  local auth=(-H "Authorization: Bearer $TOKEN" -H "content-type: application/json")
  local tpl id out
  for _ in {1..60}; do
    tpl=$(curl -fsS "${auth[@]}" http://localhost:8080/v1/templates | jq -r '.[0].name // empty' 2>/dev/null || true)
    [[ -n "$tpl" ]] && break
    sleep 2
  done
  [[ -n "${tpl:-}" ]] || die "No templates visible through API"
  id=$(curl -fsS "${auth[@]}" -X POST http://localhost:8080/v1/sandboxes \
    -d "{\"template\":\"$tpl\",\"cpu\":1,\"memory_mb\":256,\"ttl_seconds\":600}" | jq -r .id)
  [[ -n "$id" && "$id" != "null" ]] || die "Sandbox create failed"
  echo "$id" > "$STATE_DIR/last-sandbox-id"

  for i in {1..90}; do
    out=$(curl -fsS "${auth[@]}" -X POST "http://localhost:8080/v1/sandboxes/$id/exec" -d '{"cmd":"echo hello"}' 2>/dev/null | jq -r '.stdout // empty' || true)
    if [[ "$out" == "hello" ]]; then
      local code
      code=$(curl -o /dev/null -sS -w "%{http_code}" http://localhost:3000/sandboxes || true)
      [[ "$code" == "200" ]] || die "Dashboard /sandboxes returned HTTP $code, expected 200"
      curl -fsS "${auth[@]}" http://localhost:8080/v1/sandboxes | jq -e --arg id "$id" 'any(.[]; .id == $id)' >/dev/null \
        || die "Smoke-test sandbox is not visible in the sandbox list"
      printf "${GREEN}✓ smoke test passed${NC}\n"
      return
    fi
    sleep 2
    [[ $i -eq 90 ]] && break
  done
  curl -fsS "${auth[@]}" -X DELETE "http://localhost:8080/v1/sandboxes/$id" >/dev/null || true
  die "Smoke test failed: exec did not return hello"
}

main() {
  need_linux
  ensure_prereqs
  seed_tokens
  start_databases
  seed_stub_identity
  install_firecracker_and_template
  build_and_start_agent
  build_and_start_api
  start_dashboard
  smoke_test

  printf "\n${GREEN}╰─ PandaStack local E2E is up${NC}\n"
  cat <<EOF
  Dashboard:  http://localhost:3000
  API:        http://localhost:8080
  Agent:      http://localhost:7070
  Postgres:   localhost:5432 (pandastack/pandastack)
  ClickHouse: http://localhost:8123 (pandastack/pandastack)
  Default user: $PANDASTACK_STUB_USER_EMAIL
  Auth:       Auto-login is enabled (stub mode)
  Token:      $TOKEN

Tear down everything with:
  bash scripts/linux-local-e2e-down.sh
EOF
}

main "$@"
