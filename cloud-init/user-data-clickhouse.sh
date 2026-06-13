#!/bin/bash
# user-data-clickhouse.sh — bootstrap single-node ClickHouse on a dedicated GCP VM.
#
# Mounts a persistent disk at /var/lib/clickhouse, installs docker, runs
# `clickhouse/clickhouse-server:24.8` with the workspace-scoped password from
# instance metadata, and bootstraps the pandastack schema. Idempotent: safe to
# re-run on reboot.
#
# Instance metadata expected:
#   clickhouse-password   - admin password for the `default` user
#   pandastack-schema-url - GCS URL of schema.sql (gs://bucket/clickhouse/schema.sql)
#
# Listens on 8123 (HTTP) on the internal subnet only. Firewall rule restricts
# source to edge + agent tags. No public IP on this VM.
set -euo pipefail

LOG=/var/log/pandastack-clickhouse-bootstrap.log
exec > >(tee -a "$LOG") 2>&1
echo "[$(date -Is)] clickhouse bootstrap starting"

# 1. Mount persistent disk at /var/lib/clickhouse ----------------------------
DISK_DEV="/dev/disk/by-id/google-clickhouse-data"
MNT="/var/lib/clickhouse"
mkdir -p "$MNT"
if [ -b "$DISK_DEV" ]; then
  if ! blkid "$DISK_DEV" >/dev/null 2>&1; then
    echo "formatting fresh persistent disk $DISK_DEV as ext4"
    mkfs.ext4 -F -L clickhouse-data "$DISK_DEV"
  fi
  if ! mountpoint -q "$MNT"; then
    mount -o defaults,noatime "$DISK_DEV" "$MNT"
  fi
  grep -q "$MNT" /etc/fstab || echo "$DISK_DEV $MNT ext4 defaults,noatime,nofail 0 2" >> /etc/fstab
else
  echo "WARN: no persistent disk at $DISK_DEV — using boot disk"
fi
chown -R 101:101 "$MNT"  # clickhouse user uid in the official image

# 2. Install docker (Ubuntu 24.04 LTS) ---------------------------------------
if ! command -v docker >/dev/null 2>&1; then
  apt-get update -y
  apt-get install -y ca-certificates curl gnupg jq
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  chmod a+r /etc/apt/keyrings/docker.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" > /etc/apt/sources.list.d/docker.list
  apt-get update -y
  apt-get install -y docker-ce docker-ce-cli containerd.io
  systemctl enable --now docker
fi

# 3. Read password + schema URL from instance metadata -----------------------
META="http://metadata.google.internal/computeMetadata/v1/instance/attributes"
HDR='Metadata-Flavor: Google'
CH_PASSWORD="$(curl -fsS -H "$HDR" "$META/clickhouse-password" || true)"
SCHEMA_URL="$(curl -fsS -H "$HDR" "$META/pandastack-schema-url" || true)"
if [ -z "$CH_PASSWORD" ]; then
  echo "FATAL: clickhouse-password metadata missing" >&2
  exit 1
fi

# 4. Run/refresh the container ----------------------------------------------
# The official entrypoint accepts CLICKHOUSE_PASSWORD/USER/DB env vars and
# writes its own users.d/default-user.xml inside the container — no host mount
# needed, no read-only conflicts.
CONTAINER="pandastack-clickhouse"
IMAGE="clickhouse/clickhouse-server:24.8"
docker pull "$IMAGE" || true

# Stop+remove if image/version differs.
if docker inspect -f '{{.Config.Image}}' "$CONTAINER" 2>/dev/null | grep -qv "^$IMAGE\$"; then
  docker rm -f "$CONTAINER" || true
fi

if ! docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}\$"; then
  docker rm -f "$CONTAINER" 2>/dev/null || true
  docker run -d --restart=unless-stopped \
    --name "$CONTAINER" \
    --network host \
    --ulimit nofile=262144:262144 \
    --memory=3g --memory-swap=3g \
    -v /var/lib/clickhouse:/var/lib/clickhouse \
    -e CLICKHOUSE_USER=default \
    -e CLICKHOUSE_PASSWORD="$CH_PASSWORD" \
    -e CLICKHOUSE_DB=pandastack \
    -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 \
    "$IMAGE"
fi

# 6. Wait for CH to be ready -------------------------------------------------
echo "waiting for clickhouse to accept connections..."
for i in $(seq 1 60); do
  if curl -fsS "http://localhost:8123/ping" >/dev/null 2>&1; then
    echo "clickhouse healthy"
    break
  fi
  sleep 2
done

# 7. Apply schema.sql --------------------------------------------------------
if [ -n "$SCHEMA_URL" ]; then
  echo "fetching schema from $SCHEMA_URL"
  if [[ "$SCHEMA_URL" == gs://* ]]; then
    # gsutil isn't installed by default on minimal Ubuntu — use the SA token.
    TOKEN="$(curl -fsS -H "$HDR" 'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token' | jq -r .access_token)"
    OBJ="${SCHEMA_URL#gs://}"
    BUCKET="${OBJ%%/*}"
    KEY="${OBJ#*/}"
    SCHEMA="$(curl -fsS -H "Authorization: Bearer $TOKEN" "https://storage.googleapis.com/storage/v1/b/$BUCKET/o/$(jq -rn --arg k "$KEY" '$k|@uri')?alt=media" || true)"
  else
    SCHEMA="$(curl -fsS "$SCHEMA_URL" || true)"
  fi
  if [ -n "$SCHEMA" ]; then
    echo "applying schema (size=${#SCHEMA})"
    # ClickHouse HTTP rejects multi-statement bodies by default. Pipe through
    # clickhouse-client inside the container instead — handles ; splits + DDL.
    printf '%s' "$SCHEMA" | docker exec -i pandastack-clickhouse \
      clickhouse-client \
        --user default \
        --password "$CH_PASSWORD" \
        --database pandastack \
        --multiquery \
      || echo "WARN: schema apply returned non-zero"
  else
    echo "WARN: schema fetch returned empty"
  fi
else
  echo "no pandastack-schema-url metadata — skipping schema bootstrap"
fi

echo "[$(date -Is)] clickhouse bootstrap complete"
touch /var/log/pandastack-clickhouse-bootstrap.done
