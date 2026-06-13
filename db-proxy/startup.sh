#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
#
# db-proxy startup / bootstrap script.
# Run once on a fresh GCP VM to install the PandaStack DB Proxy.
#
# What this does:
#  1. Installs system packages (certbot, cloudflare plugin, curl, jq)
#  2. Obtains a wildcard TLS cert for *.db.pandastack.ai via Cloudflare DNS-01
#  3. Downloads the db-proxy binary from GCS
#  4. Writes a systemd service + environment file
#  5. Sets up cert auto-renewal with SIGHUP reload
#
# Required env vars (set before running or pass inline):
#   CLOUDFLARE_API_TOKEN   Cloudflare API token with Zone.DNS edit permission
#   PANDASTACK_DB_DSN      postgres://... (control-plane PG DSN)
#   PANDASTACK_NODE_TOKEN  shared X-Node-Token for agent auth
#   GCS_BUCKET             GCS bucket name (e.g. pandastacknode-builds)
#
# Optional:
#   BINARY_PATH            path inside bucket (default bin/pandastack-db-proxy)
#   DOMAIN                 base domain (default db.pandastack.ai)
#   LISTEN_ADDR            TCP listen (default :5432)
#   SNI_SUFFIX             SNI suffix (default .db.pandastack.ai)
#   METRICS_ADDR           Prometheus metrics (default :5433)

set -euo pipefail

DOMAIN="${DOMAIN:-db.pandastack.ai}"
WILDCARD_DOMAIN="*.${DOMAIN}"
CERT_DIR="/etc/letsencrypt/live/${DOMAIN}"
BINARY_DEST="/usr/local/bin/pandastack-db-proxy"
GCS_BUCKET="${GCS_BUCKET:-pandastacknode-builds}"
BINARY_PATH="${BINARY_PATH:-bin/pandastack-db-proxy}"
SERVICE_NAME="pandastack-db-proxy"
ENV_FILE="/etc/pandastack-db-proxy.env"

log() { echo "[db-proxy-startup] $*"; }

# ── 1. Packages ─────────────────────────────────────────────────────────────
log "Installing packages..."
apt-get update -q
apt-get install -y -q \
    certbot \
    python3-certbot-dns-cloudflare \
    curl \
    jq \
    google-cloud-cli

# ── 2. Cloudflare credentials ────────────────────────────────────────────────
log "Writing Cloudflare credentials..."
CF_CREDS_FILE="/etc/cloudflare.ini"
cat > "$CF_CREDS_FILE" <<EOF
dns_cloudflare_api_token = ${CLOUDFLARE_API_TOKEN}
EOF
chmod 600 "$CF_CREDS_FILE"

# ── 3. Obtain / renew wildcard cert (DNS-01) ─────────────────────────────────
if [ ! -f "${CERT_DIR}/fullchain.pem" ]; then
    log "Requesting wildcard cert for ${WILDCARD_DOMAIN}..."
    certbot certonly \
        --non-interactive \
        --agree-tos \
        --email "ops@pandastack.ai" \
        --dns-cloudflare \
        --dns-cloudflare-credentials "$CF_CREDS_FILE" \
        --dns-cloudflare-propagation-seconds 30 \
        -d "${DOMAIN}" \
        -d "${WILDCARD_DOMAIN}"
else
    log "Cert already exists at ${CERT_DIR}, skipping issuance."
fi

# ── 4. Cert renewal hook (SIGHUP reload) ─────────────────────────────────────
log "Installing certbot deploy hook..."
HOOK_FILE="/etc/letsencrypt/renewal-hooks/deploy/pandastack-db-proxy-reload.sh"
cat > "$HOOK_FILE" <<'HOOK'
#!/bin/bash
# Signal db-proxy to reload its cert (cert manager listens for SIGHUP).
systemctl kill --signal=SIGHUP pandastack-db-proxy.service || true
HOOK
chmod +x "$HOOK_FILE"

# Certbot timer runs twice daily; also add a cron fallback.
(crontab -l 2>/dev/null; echo "0 3 * * * certbot renew --quiet") | crontab -

# ── 5. Download binary from GCS ───────────────────────────────────────────────
log "Downloading db-proxy binary from gs://${GCS_BUCKET}/${BINARY_PATH}..."
gsutil cp "gs://${GCS_BUCKET}/${BINARY_PATH}" "$BINARY_DEST"
chmod +x "$BINARY_DEST"
log "Binary installed at ${BINARY_DEST}"

# ── 6. Environment file ───────────────────────────────────────────────────────
log "Writing environment file ${ENV_FILE}..."
cat > "$ENV_FILE" <<EOF
PANDASTACK_DB_DSN=${PANDASTACK_DB_DSN}
PANDASTACK_NODE_TOKEN=${PANDASTACK_NODE_TOKEN}
PANDASTACK_CERT_DIR=${CERT_DIR}
PANDASTACK_LISTEN_ADDR=${LISTEN_ADDR:-:5432}
PANDASTACK_SNI_SUFFIX=${SNI_SUFFIX:-.db.pandastack.ai}
PANDASTACK_METRICS_ADDR=${METRICS_ADDR:-:5433}
EOF
chmod 600 "$ENV_FILE"

# ── 7. Systemd service ────────────────────────────────────────────────────────
log "Installing systemd service..."
cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<SERVICE
[Unit]
Description=PandaStack DB Proxy (postgres:// SNI router)
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
EnvironmentFile=${ENV_FILE}
ExecStart=${BINARY_DEST}
Restart=on-failure
RestartSec=5
# Allow the cert reload SIGHUP without treating it as a termination
KillSignal=SIGTERM
# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=/etc/letsencrypt
# Prometheus + proxy ports need CAP_NET_BIND_SERVICE if <1024
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=pandastack-db-proxy

[Install]
WantedBy=multi-user.target
SERVICE

systemctl daemon-reload
systemctl enable "${SERVICE_NAME}"
systemctl restart "${SERVICE_NAME}"

log "Done. Status:"
systemctl status "${SERVICE_NAME}" --no-pager || true
log ""
log "Verify: curl http://localhost:5433/healthz"
log "DNS: Create A record  *.${DOMAIN} → $(curl -sf http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip -H 'Metadata-Flavor: Google' 2>/dev/null || hostname -I | awk '{print $1}')"
