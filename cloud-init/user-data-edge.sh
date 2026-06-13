#!/bin/bash
# cloud-init for pandastack-edge nodes (multi-node mode).
# Runs the public web surface: pandastack-api + dashboard + caddy. Does NOT run
# Firecracker. Talks to agents over the private VPC on :8081 with X-Node-Token.
#
# Reads identity + secrets via the GCP metadata server and Secret Manager.
# The TF edge-mig module supplies the following instance metadata:
#   pandastack-region        - GCP region label
#   pandastack-binary-url    - HTTPS URL serving the edge release bundle (tar)
#   pandastack-dashboard-bucket - GCS prefix for the prebuilt dashboard
#   secret-supabase-anon     - Secret Manager secret ID containing the public anon key
#   secret-supabase-url      - Secret Manager secret ID containing the public Supabase URL
#   secret-node-token        - Secret Manager secret ID containing the shared bearer
#   secret-database-url      - Secret Manager secret ID containing the Supabase DSN
#   secret-clickhouse-url    - Secret Manager secret ID containing the CH URL
#   secret-jwks-url          - Secret Manager secret ID containing the Supabase JWKS URL
#   secret-stripe-env        - JSON map of STRIPE_* env names to Secret Manager IDs
set -euo pipefail
exec > >(tee -a /var/log/pandastack-cloud-init.log) 2>&1

export DEBIAN_FRONTEND=noninteractive

# Wait for any concurrent apt/dpkg (e.g. cloud-init's own package_update) to release locks.
for _ in $(seq 1 120); do
  if ! fuser /var/lib/dpkg/lock-frontend /var/lib/apt/lists/lock /var/lib/dpkg/lock >/dev/null 2>&1; then
    break
  fi
  sleep 5
done

# Force IPv4 for apt — this VPC has no IPv6 route to GCE package mirrors.
echo 'Acquire::ForceIPv4 "true";' > /etc/apt/apt.conf.d/99force-ipv4

apt-get update
# caddy is not in the default Ubuntu 24.04 apt repo and is disabled in this deployment.
# Install only what the API actually needs at runtime.
apt-get install -y ca-certificates curl wget jq iproute2

# gcloud CLI for Secret Manager + GCS pulls.
if ! command -v gcloud >/dev/null 2>&1; then
  echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" \
    | tee /etc/apt/sources.list.d/google-cloud-sdk.list
  curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
    | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg
  apt-get update
  apt-get install -y google-cloud-cli
fi

# Node.js for dashboard SSR if needed.
if ! command -v node >/dev/null 2>&1 || ! node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 20 ? 0 : 1)' >/dev/null 2>&1; then
  curl -fsSL https://deb.nodesource.com/setup_22.x -o /usr/local/src/nodesource_setup_22.x
  bash /usr/local/src/nodesource_setup_22.x
  apt-get install -y nodejs
fi

MD="http://metadata.google.internal/computeMetadata/v1"
md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1" 2>/dev/null || true; }

REGION="$(md instance/attributes/pandastack-region)"
BINARY_URL="$(md instance/attributes/pandastack-binary-url)"
DASH_BUCKET="$(md instance/attributes/pandastack-dashboard-bucket)"
SECRET_SUPA_ANON="$(md instance/attributes/secret-supabase-anon)"
SECRET_SUPA_URL="$(md instance/attributes/secret-supabase-url)"
SECRET_TOKEN="$(md instance/attributes/secret-node-token)"
SECRET_DB="$(md instance/attributes/secret-database-url)"
SECRET_CH="$(md instance/attributes/secret-clickhouse-url)"
SECRET_JWKS="$(md instance/attributes/secret-jwks-url)"
SECRET_STRIPE_ENV="$(md instance/attributes/secret-stripe-env)"
SECRET_GITHUB_ENV="$(md instance/attributes/secret-github-env)"
INSTANCE_NAME="$(md instance/name)"
INTERNAL_IP="$(md instance/network-interfaces/0/ip)"

fetch_secret() {
  local name="$1"
  [ -n "$name" ] && gcloud secrets versions access latest --secret="$name" 2>/dev/null || true
}
NODE_TOKEN="$(fetch_secret "$SECRET_TOKEN")"
DATABASE_URL="$(fetch_secret "$SECRET_DB")"
CLICKHOUSE_URL="$(fetch_secret "$SECRET_CH")"
SUPABASE_JWKS_URL="$(fetch_secret "$SECRET_JWKS")"
SUPA_ANON="$(fetch_secret "$SECRET_SUPA_ANON")"
SUPA_URL="$(fetch_secret "$SECRET_SUPA_URL")"

install -d -m 0755 /var/lib/pandastack /etc/pandastack /etc/caddy /opt/pandastack-dashboard /opt/pandastack-bin

# Pull edge bundle. Expected layout:
#   bin/pandastack-api        - api binary
#   dashboard/            - prebuilt nextjs/Astro output (or .next/standalone)
if [ -n "$BINARY_URL" ]; then
  curl -fsSL "$BINARY_URL" -o /var/lib/pandastack/edge-bundle.tgz
  tar -xzf /var/lib/pandastack/edge-bundle.tgz -C /opt/pandastack-bin
  install -m 0755 /opt/pandastack-bin/bin/pandastack-api /usr/local/bin/pandastack-api
  if [ -d /opt/pandastack-bin/dashboard ]; then
    cp -rT /opt/pandastack-bin/dashboard /opt/pandastack-dashboard 2>/dev/null || true
  fi
elif [ -n "$DASH_BUCKET" ]; then
  gcloud storage cp "gs://${DASH_BUCKET}/bin/pandastack-api" /usr/local/bin/pandastack-api || true
  chmod 0755 /usr/local/bin/pandastack-api 2>/dev/null || true
  gcloud storage rsync --recursive "gs://${DASH_BUCKET}/dashboard/" /opt/pandastack-dashboard/ || true
fi

ENV_API_NEXT="/etc/pandastack/env.api.next.$$"
cat > "$ENV_API_NEXT" <<EOF
PANDASTACK_DB_DRIVER=pgx
PANDASTACK_DB_DSN=${DATABASE_URL}
PANDASTACK_NODE_TOKEN=${NODE_TOKEN}
PANDASTACK_REGION=${REGION}
SUPABASE_JWKS_URL=${SUPABASE_JWKS_URL}
SUPABASE_ISSUER=
SUPABASE_AUDIENCE=authenticated
CLICKHOUSE_URL=${CLICKHOUSE_URL}
PANDASTACK_CLICKHOUSE_URL=${CLICKHOUSE_URL}
PANDASTACK_PREVIEW_HOST_SUFFIX=pandastack.ai
PANDASTACK_DASHBOARD_URL=https://app.pandastack.ai
EOF

STRIPE_API_KEY_VALUE=""
STRIPE_SECRET_MISSING=0
if [ -n "$SECRET_STRIPE_ENV" ] && [ "$SECRET_STRIPE_ENV" != "{}" ]; then
  while IFS=$'\t' read -r env_name secret_name; do
    [ -n "$env_name" ] || continue
    value="$(fetch_secret "$secret_name")"
    if [ -z "$value" ]; then
      echo "WARN: missing Secret Manager value for $env_name ($secret_name) — billing will be limited" >&2
      STRIPE_SECRET_MISSING=1
      continue
    fi
    printf '%s=%s\n' "$env_name" "$value" >> "$ENV_API_NEXT"
    case "$env_name" in
      STRIPE_SECRET_KEY|STRIPE_API_KEY) STRIPE_API_KEY_VALUE="$value" ;;
    esac
  done < <(printf '%s' "$SECRET_STRIPE_ENV" | jq -r 'to_entries[] | [.key, .value] | @tsv')
fi
if [ -n "$STRIPE_API_KEY_VALUE" ] && ! grep -q '^STRIPE_API_KEY=' "$ENV_API_NEXT"; then
  printf 'STRIPE_API_KEY=%s\n' "$STRIPE_API_KEY_VALUE" >> "$ENV_API_NEXT"
fi
if [ "$STRIPE_SECRET_MISSING" -ne 0 ]; then
  echo "WARN: one or more Stripe secrets missing; API will start with billing disabled." >&2
fi

# GitHub App secrets (apps feature: connect flow + auto-deploy webhook + clone
# auth). Same metadata-JSON → fetch loop as Stripe. Blank/empty containers are
# skipped, so the API falls back to "apps disabled" until they are populated.
if [ -n "$SECRET_GITHUB_ENV" ] && [ "$SECRET_GITHUB_ENV" != "{}" ]; then
  while IFS=$'\t' read -r env_name secret_name; do
    [ -n "$env_name" ] || continue
    value="$(fetch_secret "$secret_name")"
    if [ -z "$value" ]; then
      echo "WARN: missing Secret Manager value for $env_name ($secret_name) — apps/GitHub will be limited" >&2
      continue
    fi
    # systemd's EnvironmentFile treats backslash as an escape character, so a
    # one-line PEM stored with literal "\n" would reach the process as plain
    # "n" (unparseable key). Double the backslashes: systemd collapses "\\n"
    # back to "\n", and the API's normalizePEM expands that to real newlines.
    value="${value//\\/\\\\}"
    printf '%s=%s\n' "$env_name" "$value" >> "$ENV_API_NEXT"
  done < <(printf '%s' "$SECRET_GITHUB_ENV" | jq -r 'to_entries[] | [.key, .value] | @tsv')
fi

install -m 0600 -o root -g root "$ENV_API_NEXT" /etc/pandastack/env.api
rm -f "$ENV_API_NEXT"

cat > /etc/pandastack/env.dashboard <<EOF
NEXT_PUBLIC_SUPABASE_URL=${SUPA_URL}
NEXT_PUBLIC_SUPABASE_ANON_KEY=${SUPA_ANON}
NEXT_PUBLIC_API_BASE=https://api.pandastack.ai
PORT=3000
EOF
chmod 0644 /etc/pandastack/env.dashboard

cat > /etc/systemd/system/pandastack-api.service <<'UNIT'
[Unit]
Description=Pandastack public API (multi-node edge)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/pandastack/env.api
ExecStart=/usr/local/bin/pandastack-api -addr :8080 -token-file /var/lib/pandastack/tokens.json
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT

cat > /etc/systemd/system/pandastack-dashboard.service <<'UNIT'
[Unit]
Description=Pandastack dashboard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/pandastack/env.dashboard
WorkingDirectory=/opt/pandastack-dashboard
ExecStart=/usr/bin/node server.js
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

cat > /etc/caddy/Caddyfile <<'CADDY'
# Behind GCP HTTPS LB: TLS terminates at the LB, edge serves plain HTTP on :8080
# so the LB health check works. The LB does host-based routing via URL map.
# Internal :8080 host-splits: /v1/* -> api, everything else -> dashboard.
:8080 {
  @api path /v1/* /healthz /version /metrics
  reverse_proxy @api localhost:8080 {
    transport http {
      versions 1.1
    }
  }
  reverse_proxy localhost:3000
}
CADDY
# Note: above is a placeholder; the api binary itself already serves /v1
# directly on :8080, so caddy is only needed for dashboard static. We just
# point the dashboard service at :3000 and rely on pandastack-api to route /v1.
# Disable caddy in this minimal first-cut deployment.
systemctl disable --now caddy || true

systemctl daemon-reload
systemctl enable --now pandastack-api || true
systemctl enable --now pandastack-dashboard || true

echo "edge cloud-init done at $(date -u)"
