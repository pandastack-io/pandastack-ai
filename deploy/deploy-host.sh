#!/usr/bin/env bash
# deploy/deploy-host.sh
#
# Cloud-agnostic deploy step: builds agent + api + dashboard + marketing locally,
# copies them to $HOST, writes /etc/pandastack/env, restarts services.
#
# Required env vars:
#   HOST                  ssh-reachable host (IP or FQDN)
#   SSH_USER              defaults to ubuntu
#   APP_FQDN              dashboard FQDN (eg app.pandastack.ai)
#   API_FQDN              api FQDN     (eg api.pandastack.ai)
#   WWW_FQDN              marketing FQDN (eg www.pandastack.ai). Optional.
#   DATABASE_URL          Postgres DSN (Supabase pooler).
#   SUPABASE_JWKS_URL     Supabase JWKS endpoint.
#   SUPABASE_ISSUER       Supabase auth issuer.
#   STORAGE_BUCKET        GCS / S3 bucket for kernels+templates+snapshots. Optional.
#   STORAGE_DRIVER        "gcs" | "s3" | "local" (default "local").
#
# Optional flags:
#   --skip-build          reuse existing artifacts in deploy/.build/
#   --skip-marketing      don't deploy the marketing site
#   --skip-dashboard      don't deploy the dashboard
#   --coming-soon=true    set COMING_SOON for the marketing build (default true)
set -euo pipefail

SKIP_BUILD=0
SKIP_MARKETING=0
SKIP_DASHBOARD=0
COMING_SOON="${COMING_SOON:-true}"
SSH_USER="${SSH_USER:-ubuntu}"

while [ $# -gt 0 ]; do
  case "$1" in
    --skip-build)      SKIP_BUILD=1; shift ;;
    --skip-marketing)  SKIP_MARKETING=1; shift ;;
    --skip-dashboard)  SKIP_DASHBOARD=1; shift ;;
    --coming-soon=*)   COMING_SOON="${1#*=}"; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

: "${HOST:?HOST is required}"
: "${APP_FQDN:?APP_FQDN is required}"
: "${API_FQDN:?API_FQDN is required}"
: "${DATABASE_URL:?DATABASE_URL is required}"
: "${SUPABASE_JWKS_URL:?SUPABASE_JWKS_URL is required}"
: "${SUPABASE_ISSUER:?SUPABASE_ISSUER is required}"
STORAGE_DRIVER="${STORAGE_DRIVER:-local}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BUILD_DIR="$SCRIPT_DIR/.build"
REMOTE="$SSH_USER@$HOST"
REMOTE_STAGE="/home/$SSH_USER/pandastack-deploy"

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; NC='\033[0m'
step()  { printf "\n${GREEN}==>${NC} %s\n" "$*"; }
warn()  { printf "${YELLOW}WARN:${NC} %s\n" "$*" >&2; }
fail()  { printf "${RED}FAIL${NC} %s\n" "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"; }
need ssh; need rsync; need scp; need curl

mkdir -p "$BUILD_DIR"

# ──────────────────────────────────────────────────────────────────────────────
# Build artifacts
# ──────────────────────────────────────────────────────────────────────────────
if [ "$SKIP_BUILD" -eq 0 ]; then
  need go
  step "Cross-compiling Go binaries for linux/amd64"
  (cd "$REPO_ROOT/agent" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BUILD_DIR/pandastack-agent" ./cmd/agent)
  (cd "$REPO_ROOT/api"   && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BUILD_DIR/pandastack-api"   ./cmd/api)
  (cd "$REPO_ROOT/agent" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BUILD_DIR/pandastack-init"  ./cmd/pandastack-init)

  if [ "$SKIP_DASHBOARD" -eq 0 ]; then
    need npm
    step "Building dashboard"
    (cd "$REPO_ROOT/dashboard" && npm ci --no-audit --no-fund && npm run build)
  fi

  if [ "$SKIP_MARKETING" -eq 0 ] && [ -d "$REPO_ROOT/marketing" ]; then
    need npm
    step "Building marketing (COMING_SOON=$COMING_SOON)"
    (cd "$REPO_ROOT/marketing" && npm ci --no-audit --no-fund && COMING_SOON="$COMING_SOON" npm run build)
  fi
fi

# ──────────────────────────────────────────────────────────────────────────────
# Stage env file
# ──────────────────────────────────────────────────────────────────────────────
step "Assembling /etc/pandastack/env"
cat > "$BUILD_DIR/pandastack.env" <<ENV
PANDASTACK_DB_DRIVER=postgres
PANDASTACK_DB_DSN=${DATABASE_URL}
SUPABASE_JWKS_URL=${SUPABASE_JWKS_URL}
SUPABASE_ISSUER=${SUPABASE_ISSUER}
SUPABASE_AUDIENCE=${SUPABASE_AUDIENCE:-authenticated}
PANDASTACK_NATID=${PANDASTACK_NATID:-1}
PANDASTACK_NATID_POOL_SIZE=${PANDASTACK_NATID_POOL_SIZE:-6}
PANDASTACK_DEFAULT_TTL_SECONDS=${PANDASTACK_DEFAULT_TTL_SECONDS:-300}
PANDASTACK_METRICS_LISTEN=${PANDASTACK_METRICS_LISTEN:-:9100}
PANDASTACK_APP_FQDN=${APP_FQDN}
PANDASTACK_API_FQDN=${API_FQDN}
PANDASTACK_AUTH_SKIP_PREFIXES=/healthz,/version,/metrics,/v1/metrics
ENV
case "$STORAGE_DRIVER" in
  gcs)
    echo "PANDASTACK_GCS_BUCKET=${STORAGE_BUCKET}" >> "$BUILD_DIR/pandastack.env"
    # Snapshot/WAL bucket: user snapshot+fork replication, managed-DB WAL
    # archive and db restore. Unset = all three silently disabled, so default
    # to the seeds bucket (its lifecycle rules only match the snapshots/ prefix).
    echo "PANDASTACK_SNAPSHOT_BUCKET=${PANDASTACK_SNAPSHOT_BUCKET:-${STORAGE_BUCKET}}" >> "$BUILD_DIR/pandastack.env"
    ;;
  s3)  echo "PANDASTACK_S3_BUCKET=${STORAGE_BUCKET}"  >> "$BUILD_DIR/pandastack.env" ;;
esac
chmod 0600 "$BUILD_DIR/pandastack.env"

# ──────────────────────────────────────────────────────────────────────────────
# Ship to host
# ──────────────────────────────────────────────────────────────────────────────
step "Staging on $REMOTE"
ssh -o StrictHostKeyChecking=accept-new "$REMOTE" "mkdir -p '$REMOTE_STAGE'"
scp "$BUILD_DIR/pandastack.env" "$REMOTE:$REMOTE_STAGE/pandastack.env"
ssh "$REMOTE" "sudo install -m 0600 -o root -g root '$REMOTE_STAGE/pandastack.env' /etc/pandastack/env && \
              sudo install -m 0600 -o root -g root '$REMOTE_STAGE/pandastack.env' /etc/pandastack/env.agent && \
              sudo sed -i 's|^SUPABASE_JWKS_URL=|#SUPABASE_JWKS_URL=|' /etc/pandastack/env.agent"

# ── XFS+reflink data volume (enables ~1ms FICLONE rootfs CoW → sub-second boots)
step "Ensuring /var/lib/pandastack is on XFS with reflink=1"
PANDASTACK_DATA_SIZE_GB="${PANDASTACK_DATA_SIZE_GB:-50}"
ssh "$REMOTE" "sudo bash -s" <<REMOTE_XFS
set -euo pipefail
need_migrate=1
if mountpoint -q /var/lib/pandastack && [ "\$(stat -f -c %T /var/lib/pandastack)" = "xfs" ] && xfs_info /var/lib/pandastack 2>/dev/null | grep -q "reflink=1"; then
  echo "already on xfs+reflink — skipping"
  need_migrate=0
fi
if [ "\$need_migrate" = "1" ]; then
  apt-get install -y -qq xfsprogs >/dev/null
  systemctl stop pandastack-api pandastack-agent 2>/dev/null || true
  if [ ! -f /opt/pandastack.img ]; then
    echo "creating /opt/pandastack.img (${PANDASTACK_DATA_SIZE_GB}G XFS reflink loopback)"
    truncate -s ${PANDASTACK_DATA_SIZE_GB}G /opt/pandastack.img
    mkfs.xfs -m reflink=1 -q /opt/pandastack.img
  fi
  if [ -d /var/lib/pandastack ] && [ ! -d /var/lib/pandastack.preXFS ]; then
    echo "migrating existing /var/lib/pandastack → XFS volume"
    mkdir -p /mnt/pandastack-stage
    mount -o loop /opt/pandastack.img /mnt/pandastack-stage
    cp -a /var/lib/pandastack/. /mnt/pandastack-stage/ 2>/dev/null || true
    umount /mnt/pandastack-stage
    mv /var/lib/pandastack /var/lib/pandastack.preXFS
  fi
  mkdir -p /var/lib/pandastack
  mount -o loop /opt/pandastack.img /var/lib/pandastack
  if ! grep -q "/opt/pandastack.img" /etc/fstab; then
    echo "/opt/pandastack.img /var/lib/pandastack xfs loop,defaults 0 0" >> /etc/fstab
  fi
fi
echo "verify:"
stat -f -c "fstype=%T" /var/lib/pandastack
xfs_info /var/lib/pandastack 2>/dev/null | grep -oE "reflink=[01]" | head -1 || true
REMOTE_XFS

step "Installing binaries"
rsync -avz "$BUILD_DIR/pandastack-agent" "$BUILD_DIR/pandastack-api" "$BUILD_DIR/pandastack-init" "$REMOTE:$REMOTE_STAGE/"
ssh "$REMOTE" "sudo install -m 0755 '$REMOTE_STAGE/pandastack-agent' /usr/local/bin/pandastack-agent && \
              sudo install -m 0755 '$REMOTE_STAGE/pandastack-api'   /usr/local/bin/pandastack-api && \
              sudo install -m 0755 '$REMOTE_STAGE/pandastack-init'  /usr/local/bin/pandastack-init"

if [ "$SKIP_DASHBOARD" -eq 0 ]; then
  step "Deploying dashboard"
  ssh "$REMOTE" "sudo mkdir -p /opt/pandastack-dashboard && sudo chown $SSH_USER:$SSH_USER /opt/pandastack-dashboard"
  rsync -az --delete \
    "$REPO_ROOT/dashboard/.next" \
    "$REPO_ROOT/dashboard/public" \
    "$REPO_ROOT/dashboard/package.json" \
    "$REPO_ROOT/dashboard/package-lock.json" \
    "$REMOTE:/opt/pandastack-dashboard/"
  ssh "$REMOTE" "cd /opt/pandastack-dashboard && npm ci --omit=dev --silent --no-audit --no-fund"
fi

if [ "$SKIP_MARKETING" -eq 0 ] && [ -d "$REPO_ROOT/marketing/.next" ]; then
  step "Deploying marketing (COMING_SOON=$COMING_SOON)"
  ssh "$REMOTE" "sudo mkdir -p /opt/pandastack-marketing && sudo chown $SSH_USER:$SSH_USER /opt/pandastack-marketing"
  rsync -az --delete "$REPO_ROOT/marketing/.next/standalone/"        "$REMOTE:/opt/pandastack-marketing/"
  rsync -az --delete "$REPO_ROOT/marketing/.next/static/"            "$REMOTE:/opt/pandastack-marketing/.next/static/"
  rsync -az --delete "$REPO_ROOT/marketing/public/"                  "$REMOTE:/opt/pandastack-marketing/public/"
fi

# ──────────────────────────────────────────────────────────────────────────────
# Systemd units + caddy
# ──────────────────────────────────────────────────────────────────────────────
step "Writing systemd units + Caddyfile"
ssh "$REMOTE" "sudo tee /etc/systemd/system/pandastack-agent.service >/dev/null" <<'UNIT'
[Unit]
Description=Pandastack Firecracker agent
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
EnvironmentFile=/etc/pandastack/env.agent
ExecStart=/usr/local/bin/pandastack-agent -socket /run/fcsandbox/agent.sock -data-dir /var/lib/pandastack -db /var/lib/pandastack-io/pandastack-ai-oss.db
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT

ssh "$REMOTE" "sudo tee /etc/systemd/system/pandastack-api.service >/dev/null" <<'UNIT'
[Unit]
Description=Pandastack public API
After=network-online.target pandastack-agent.service
Wants=network-online.target
[Service]
Type=simple
EnvironmentFile=/etc/pandastack/env
ExecStart=/usr/local/bin/pandastack-api -addr :8080 -agent-socket /run/fcsandbox/agent.sock -token-file /var/lib/pandastack/tokens.json
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT

if [ "$SKIP_DASHBOARD" -eq 0 ]; then
  ssh "$REMOTE" "sudo tee /etc/systemd/system/pandastack-dashboard.service >/dev/null" <<UNIT
[Unit]
Description=Pandastack dashboard
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
EnvironmentFile=/etc/pandastack/env
WorkingDirectory=/opt/pandastack-dashboard
ExecStart=/usr/bin/npm run start -- --hostname 127.0.0.1 --port 3000
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT
fi

if [ "$SKIP_MARKETING" -eq 0 ]; then
  ssh "$REMOTE" "sudo tee /etc/systemd/system/pandastack-marketing.service >/dev/null" <<UNIT
[Unit]
Description=PandaStack marketing site
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
Environment=NODE_ENV=production
Environment=PORT=3100
Environment=HOSTNAME=127.0.0.1
Environment=COMING_SOON=$COMING_SOON
WorkingDirectory=/opt/pandastack-marketing
ExecStart=/usr/bin/node server.js
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT
fi

CADDY_BLOCKS=$(cat <<CADDY
{
  auto_https off
}

http://{\$PANDASTACK_APP_FQDN}, https://{\$PANDASTACK_APP_FQDN} {
  tls internal
  reverse_proxy localhost:3000
}
http://{\$PANDASTACK_API_FQDN}, https://{\$PANDASTACK_API_FQDN} {
  tls internal
  reverse_proxy localhost:8080
}
CADDY
)
if [ -n "${WWW_FQDN:-}" ] && [ "$SKIP_MARKETING" -eq 0 ]; then
  CADDY_BLOCKS="$CADDY_BLOCKS
http://$WWW_FQDN, https://$WWW_FQDN {
  tls internal
  reverse_proxy localhost:3100
}"
fi
ssh "$REMOTE" "sudo tee /etc/caddy/Caddyfile >/dev/null" <<<"$CADDY_BLOCKS"
ssh "$REMOTE" "sudo install -d -m 0755 /etc/systemd/system/caddy.service.d && sudo tee /etc/systemd/system/caddy.service.d/pandastack-env.conf >/dev/null" <<'UNIT'
[Service]
EnvironmentFile=/etc/pandastack/env
UNIT

step "Restarting services"
RESTART_LIST="caddy pandastack-agent pandastack-api"
[ "$SKIP_DASHBOARD" -eq 0 ] && RESTART_LIST="$RESTART_LIST pandastack-dashboard"
[ "$SKIP_MARKETING" -eq 0 ] && RESTART_LIST="$RESTART_LIST pandastack-marketing"
ssh "$REMOTE" "sudo systemctl daemon-reload && sudo systemctl enable $RESTART_LIST && sudo systemctl restart $RESTART_LIST"

# ──────────────────────────────────────────────────────────────────────────────
# Health checks
# ──────────────────────────────────────────────────────────────────────────────
step "Health checks"
health() {
  local label="$1" url="$2"
  for _ in $(seq 1 15); do
    code=$(curl -ks -o /dev/null -w '%{http_code}' "$url" || true)
    if [[ "$code" =~ ^(2|3) ]]; then
      printf "  ${GREEN}OK${NC}  %-12s %s -> %s\n" "$label" "$url" "$code"
      return 0
    fi
    sleep 3
  done
  printf "  ${RED}FAIL${NC} %-12s %s\n" "$label" "$url" >&2
  return 1
}
health "api"        "https://$API_FQDN/healthz"        || warn "api health failed (DNS may not have propagated; verify locally on host)"
[ "$SKIP_DASHBOARD" -eq 0 ] && { health "dashboard" "https://$APP_FQDN/login" || warn "dashboard health failed (DNS may not have propagated)"; }
[ "$SKIP_MARKETING" -eq 0 ] && [ -n "${WWW_FQDN:-}" ] && { health "marketing" "https://$WWW_FQDN/" || warn "marketing health failed (DNS may not have propagated)"; }

printf "\n${GREEN}Deploy complete${NC} (host=%s)\n" "$HOST"
