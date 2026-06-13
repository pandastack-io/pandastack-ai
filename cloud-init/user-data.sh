#!/bin/bash
set -euxo pipefail
exec > >(tee -a /var/log/pandastack-cloud-init.log) 2>&1

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y ca-certificates curl wget jq squashfs-tools iproute2 iptables uuid-runtime e2fsprogs sqlite3 caddy
# awscli is not in the Ubuntu 24.04 archive; install only if S3 bucket is configured
if [ -n "${PANDASTACK_S3_BUCKET:-}" ]; then
  apt-get install -y python3-pip && pip3 install --break-system-packages awscli || true
fi

if ! command -v node >/dev/null 2>&1 || ! node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 20 ? 0 : 1)' >/dev/null 2>&1; then
  curl -fsSL https://deb.nodesource.com/setup_22.x -o /usr/local/src/nodesource_setup_22.x
  bash /usr/local/src/nodesource_setup_22.x
  apt-get install -y nodejs
fi

install -d -m 0755 /usr/local/src/pandastack-firecracker
if ! command -v firecracker >/dev/null 2>&1 || ! firecracker --version 2>/dev/null | grep -q '1.16.0'; then
  wget -qO /usr/local/src/pandastack-firecracker/firecracker-v1.16.0-x86_64.tgz \
    https://github.com/firecracker-microvm/firecracker/releases/download/v1.16.0/firecracker-v1.16.0-x86_64.tgz
  tar -xzf /usr/local/src/pandastack-firecracker/firecracker-v1.16.0-x86_64.tgz -C /usr/local/src/pandastack-firecracker
  install -m 0755 /usr/local/src/pandastack-firecracker/release-v1.16.0-x86_64/firecracker-v1.16.0-x86_64 /usr/local/bin/firecracker
  install -m 0755 /usr/local/src/pandastack-firecracker/release-v1.16.0-x86_64/jailer-v1.16.0-x86_64 /usr/local/bin/jailer
fi

groupadd -f kvm
usermod -aG kvm ubuntu
cat > /etc/udev/rules.d/99-kvm.rules <<'KVMRULES'
KERNEL=="kvm", GROUP="kvm", MODE="0660"
KVMRULES
udevadm control --reload-rules
udevadm trigger --name-match=kvm || true

sysctl -w net.ipv4.ip_forward=1
cat > /etc/sysctl.d/99-pandastack.conf <<'SYSCTL'
net.ipv4.ip_forward=1
SYSCTL

install -d -m 0755 /var/lib/pandastack /var/lib/fcsandbox/kernels /var/lib/fcsandbox/templates /var/lib/fcsandbox/vms /var/lib/fcsandbox/snapshots /run/fcsandbox /etc/pandastack /etc/caddy /opt/pandastack-dashboard

if [ ! -f /etc/pandastack/env ]; then
  cat > /etc/pandastack/env <<'ENV'
PANDASTACK_APP_FQDN=dev.pandastack.dev
PANDASTACK_API_FQDN=api.dev.pandastack.dev
PANDASTACK_S3_BUCKET=
PANDASTACK_GCS_BUCKET=
ENV
  chmod 0600 /etc/pandastack/env
fi

set -a
# shellcheck disable=SC1091
. /etc/pandastack/env
set +a

if [ -n "${PANDASTACK_GCS_BUCKET:-}" ]; then
  if command -v gcloud >/dev/null 2>&1; then
    gcloud storage rsync --recursive "gs://${PANDASTACK_GCS_BUCKET}/kernels/" /var/lib/fcsandbox/kernels/ || true
    gcloud storage rsync --recursive "gs://${PANDASTACK_GCS_BUCKET}/templates/" /var/lib/fcsandbox/templates/ || true
  elif command -v gsutil >/dev/null 2>&1; then
    gsutil -m rsync -r "gs://${PANDASTACK_GCS_BUCKET}/kernels/" /var/lib/fcsandbox/kernels/ || true
    gsutil -m rsync -r "gs://${PANDASTACK_GCS_BUCKET}/templates/" /var/lib/fcsandbox/templates/ || true
  else
    echo "PANDASTACK_GCS_BUCKET is set but neither gcloud nor gsutil is installed; skipping GCS kernel/template sync"
  fi
elif [ -n "${PANDASTACK_S3_BUCKET:-}" ]; then
  aws s3 sync "s3://${PANDASTACK_S3_BUCKET}/kernels/" /var/lib/fcsandbox/kernels/ || true
  aws s3 sync "s3://${PANDASTACK_S3_BUCKET}/templates/" /var/lib/fcsandbox/templates/ || true
else
  echo "Neither PANDASTACK_GCS_BUCKET nor PANDASTACK_S3_BUCKET is set yet; skipping kernel/template sync"
fi

cat > /etc/systemd/system/pandastack-agent.service <<'UNIT'
[Unit]
Description=Pandastack Firecracker agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/pandastack/env
ExecStart=/usr/local/bin/pandastack-agent -socket /run/fcsandbox/agent.sock -data-dir /var/lib/pandastack -db /var/lib/pandastack-io/pandastack-ai-oss.db
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

cat > /etc/systemd/system/pandastack-api.service <<'UNIT'
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

cat > /etc/systemd/system/pandastack-dashboard.service <<'UNIT'
[Unit]
Description=Pandastack dashboard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/pandastack/env
WorkingDirectory=/opt/pandastack-dashboard
ExecStart=/bin/false
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

install -d -m 0755 /etc/systemd/system/caddy.service.d
cat > /etc/systemd/system/caddy.service.d/pandastack-env.conf <<'UNIT'
[Service]
EnvironmentFile=/etc/pandastack/env
UNIT

cat > /etc/caddy/Caddyfile <<'CADDY'
# Cloudflare should use SSL/TLS mode "Full" for dev: Caddy serves an internal
# self-signed certificate at the origin. For Full (strict), switch to DNS-01
# with the Cloudflare Caddy provider later.
{$PANDASTACK_APP_FQDN} {
  tls internal
  reverse_proxy localhost:3000
}

{$PANDASTACK_API_FQDN} {
  tls internal
  reverse_proxy localhost:8080
}
CADDY

systemctl daemon-reload
systemctl enable pandastack-agent pandastack-api pandastack-dashboard
systemctl enable --now caddy

echo "cloud-init done at $(date -u)"
