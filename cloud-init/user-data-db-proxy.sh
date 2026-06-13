#!/bin/bash
# cloud-init for pandastack-db-proxy node.
# Runs the TLS SNI postgres proxy: listens on :5432, routes by sandbox ID
# embedded in the SNI hostname (*.db.pandastack.ai → sandbox postgres).
#
# Reads identity + secrets via the GCP metadata server and Secret Manager.
# The TF db-proxy resource supplies the following instance metadata:
#   pandastack-binary-url      - HTTPS URL or gs:// path to db-proxy binary
#   pandastack-sni-suffix      - SNI suffix (default .db.pandastack.ai)
#   secret-node-token          - Secret Manager ID for the X-Node-Token
#   secret-database-url        - Secret Manager ID for control-plane Postgres DSN
#   secret-cloudflare-token    - Secret Manager ID for Cloudflare API token (certbot DNS-01)
set -euo pipefail
exec > >(tee -a /var/log/pandastack-db-proxy-cloud-init.log) 2>&1

export DEBIAN_FRONTEND=noninteractive

# Wait for concurrent apt locks (cloud-init's own package_update).
for _ in $(seq 1 120); do
  if ! fuser /var/lib/dpkg/lock-frontend /var/lib/apt/lists/lock /var/lib/dpkg/lock >/dev/null 2>&1; then
    break
  fi
  sleep 5
done

echo 'Acquire::ForceIPv4 "true";' > /etc/apt/apt.conf.d/99force-ipv4

apt-get update
apt-get install -y ca-certificates curl wget jq

# gcloud CLI for Secret Manager pulls.
if ! command -v gcloud >/dev/null 2>&1; then
  echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" \
    | tee /etc/apt/sources.list.d/google-cloud-sdk.list
  curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
    | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg
  apt-get update
  apt-get install -y google-cloud-cli
fi

# Certbot + Cloudflare DNS-01 plugin for wildcard cert.
apt-get install -y certbot python3-certbot-dns-cloudflare

MD="http://metadata.google.internal/computeMetadata/v1"
md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1" 2>/dev/null || true; }

BINARY_URL="$(md instance/attributes/pandastack-binary-url)"
SNI_SUFFIX="$(md instance/attributes/pandastack-sni-suffix)"
SECRET_TOKEN="$(md instance/attributes/secret-node-token)"
SECRET_DB="$(md instance/attributes/secret-database-url)"
SECRET_CF="$(md instance/attributes/secret-cloudflare-token)"

fetch_secret() {
  local name="$1"
  [ -n "$name" ] && gcloud secrets versions access latest --secret="$name" 2>/dev/null || true
}

NODE_TOKEN="$(fetch_secret "$SECRET_TOKEN")"
DATABASE_URL="$(fetch_secret "$SECRET_DB")"
CLOUDFLARE_API_TOKEN="$(fetch_secret "$SECRET_CF")"

SNI_SUFFIX="${SNI_SUFFIX:-.db.pandastack.ai}"
DOMAIN="${SNI_SUFFIX#.}"   # strip leading dot → db.pandastack.ai
CERT_DIR="/etc/letsencrypt/live/${DOMAIN}"

# ── Cloudflare credentials ────────────────────────────────────────────────────
CF_CREDS=/etc/cloudflare.ini
printf 'dns_cloudflare_api_token = %s\n' "$CLOUDFLARE_API_TOKEN" > "$CF_CREDS"
chmod 600 "$CF_CREDS"

# ── Obtain wildcard cert (DNS-01) ─────────────────────────────────────────────
if [ ! -f "${CERT_DIR}/fullchain.pem" ]; then
  certbot certonly \
    --non-interactive \
    --agree-tos \
    --email "ops@pandastack.ai" \
    --dns-cloudflare \
    --dns-cloudflare-credentials "$CF_CREDS" \
    --dns-cloudflare-propagation-seconds 30 \
    -d "${DOMAIN}" \
    -d "*.${DOMAIN}"
fi

# ── Cert renewal hook ─────────────────────────────────────────────────────────
HOOK=/etc/letsencrypt/renewal-hooks/deploy/db-proxy-reload.sh
cat > "$HOOK" <<'HOOK'
#!/bin/bash
systemctl kill --signal=SIGHUP pandastack-db-proxy.service || true
HOOK
chmod +x "$HOOK"
(crontab -l 2>/dev/null; echo "0 3 * * * certbot renew --quiet") | crontab -

# ── Download binary ───────────────────────────────────────────────────────────
BINARY=/usr/local/bin/pandastack-db-proxy
if [[ "$BINARY_URL" == gs://* ]]; then
  gsutil cp "$BINARY_URL" "$BINARY"
else
  curl -fsSL "$BINARY_URL" -o "$BINARY"
fi
chmod +x "$BINARY"

# ── Environment file ──────────────────────────────────────────────────────────
ENV_FILE=/etc/pandastack-db-proxy.env
cat > "$ENV_FILE" <<EOF
PANDASTACK_DB_DSN=${DATABASE_URL}
PANDASTACK_NODE_TOKEN=${NODE_TOKEN}
PANDASTACK_CERT_DIR=${CERT_DIR}
PANDASTACK_LISTEN_ADDR=:5432
PANDASTACK_SNI_SUFFIX=${SNI_SUFFIX}
PANDASTACK_METRICS_ADDR=:5433
EOF
chmod 600 "$ENV_FILE"

# ── Systemd service ───────────────────────────────────────────────────────────
cat > /etc/systemd/system/pandastack-db-proxy.service <<SERVICE
[Unit]
Description=PandaStack DB Proxy (postgres:// SNI router)
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
EnvironmentFile=${ENV_FILE}
ExecStart=${BINARY}
Restart=on-failure
RestartSec=5
KillSignal=SIGTERM
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=/etc/letsencrypt
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
StandardOutput=journal
StandardError=journal
SyslogIdentifier=pandastack-db-proxy

[Install]
WantedBy=multi-user.target
SERVICE

systemctl daemon-reload
systemctl enable pandastack-db-proxy
systemctl start pandastack-db-proxy

echo "DB Proxy cloud-init complete. Listening on :5432."
