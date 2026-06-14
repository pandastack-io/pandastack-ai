#!/usr/bin/env bash
# autostart.sh — postgres-16 durable-volume boot (Phase 4 persistence model).
#
# Boot model (see db-persistence-plan):
#   Phase 1 (snapshot boundary): OS boots, PG is STOPPED, the data device is
#     NOT touched. We write /run/pandastack/snapshot-ready and block on the
#     credential trigger. The template snapshot is taken here, so it captures a
#     booted OS with postgres stopped and an unused placeholder /dev/vdb.
#   Phase 2 (per-sandbox restore): the agent patches /dev/vdb to this database's
#     own durable ext4 image and delivers credentials. We then format-if-blank,
#     mount, initdb-if-empty (fail-closed on inconsistency), start postgres,
#     bootstrap objects, rotate credentials, and start pgbouncer + broker.
#
# Durability: all postgres data lives on /dev/vdb (a per-database image on the
# agent's durable disk), NOT in the rootfs. The rootfs cluster baked by the
# Dockerfile is never started (Debian's postgresql units are disabled) and is
# shadowed by the /dev/vdb mount.

set -euo pipefail

PG_VERSION=16
PG_BIN="/usr/lib/postgresql/${PG_VERSION}/bin"
PG_CTL="${PG_BIN}/pg_ctl"
DATA_DEV="/dev/vdb"
MOUNT="/var/lib/postgresql/data"
PGDATA="${MOUNT}/pgdata"
PANDASTACK_DIR="/etc/pandastack"
READY_FILE="/run/pandastack/ready.json"

log() { printf '[%s] [autostart] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { log "ERROR: $*"; exit 1; }

mkdir -p /run/pandastack /workspace /var/log/postgresql "${MOUNT}"
chown postgres:postgres /var/log/postgresql "${MOUNT}"

# ── Phase 1: OS ready, postgres stopped, data device untouched ───────────────
# This is the snapshot boundary. Do NOT start postgres and do NOT touch
# /dev/vdb before this point — the snapshot must capture an unused data device.
log "phase1: OS ready (postgres stopped, ${DATA_DEV} untouched) — signalling snapshot-ready"
touch /run/pandastack/snapshot-ready
log "phase1: waiting for credentials from agent (restore boundary)"

# ── Wait for credentials (delivered by agent on every restore) ───────────────
# The agent writes pg.password.new + broker.token.new then touches creds-ready.
# On bake this never arrives: the snapshot is taken while we block here, so the
# data-device work below only ever runs on a real per-database restore.
CRED_TRIGGER="/run/pandastack/creds-ready"
CRED_PASS="/run/pandastack/pg.password.new"
CRED_TOK="/run/pandastack/broker.token.new"

CRED_DEADLINE=$((SECONDS + 60))
until [[ -f "${CRED_TRIGGER}" ]]; do
  sleep 0.2
  if [[ ${SECONDS} -ge ${CRED_DEADLINE} ]]; then
    log "phase2: no credentials from agent within 60s — generating internally"
    break
  fi
done

if [[ -f "${CRED_TRIGGER}" ]]; then
  log "phase2: credentials received from agent"
  PG_PASSWORD="$(cat "${CRED_PASS}")"
  BROKER_TOKEN="$(cat "${CRED_TOK}")"
  rm -f "${CRED_TRIGGER}" "${CRED_PASS}" "${CRED_TOK}"
else
  # Generate credentials without a pipe whose consumer closes early. The old
  # `tr </dev/urandom | head -c N` made head close the pipe after N bytes, which
  # SIGPIPEs tr; under `set -o pipefail` + `set -e` that aborted the whole
  # autostart (the bug that left postgres stuck "activating", never writing
  # ready.json). Here we read a bounded chunk into a var, filter it, and slice
  # with bash parameter expansion — no early-closing pipe involved.
  _rnd() {
    local charset="$1" want="$2" out=""
    while [[ ${#out} -lt $want ]]; do
      out+="$(head -c 8192 /dev/urandom | tr -dc "$charset")"
    done
    printf '%s' "${out:0:$want}"
  }
  PG_PASSWORD="$(_rnd 'A-Za-z0-9' 44)"
  BROKER_TOKEN="pds_pg_$(_rnd 'a-f0-9' 48)"
fi

# ── Phase 2: bring up the durable data device ────────────────────────────────
log "phase2: preparing durable data device ${DATA_DEV}"
for _ in $(seq 1 50); do
  [[ -b "${DATA_DEV}" ]] && break
  sleep 0.2
done
[[ -b "${DATA_DEV}" ]] || die "data device ${DATA_DEV} not present"

FS_TYPE="$(blkid -p -o value -s TYPE "${DATA_DEV}" 2>/dev/null || true)"
FRESH=0
if [[ -z "${FS_TYPE}" ]]; then
  log "phase2: no filesystem on ${DATA_DEV} — formatting ext4 (new database)"
  mkfs.ext4 -F -q -L pgdata "${DATA_DEV}" || die "mkfs.ext4 failed"
  FRESH=1
elif [[ "${FS_TYPE}" != "ext4" ]]; then
  die "data device filesystem is '${FS_TYPE}', expected ext4 — refusing to mount (corruption guard)"
fi

mountpoint -q "${MOUNT}" || mount -o noatime "${DATA_DEV}" "${MOUNT}" || die "mount ${DATA_DEV} failed"
mkdir -p "${PGDATA}"
chown -R postgres:postgres "${MOUNT}"
chmod 700 "${PGDATA}"

# initdb-if-empty with a fail-closed guard: a formatted but cluster-less device
# that we did NOT just create indicates a half-initialised or wrong volume —
# never re-initdb over it (would destroy data).
if [[ -f "${PGDATA}/PG_VERSION" ]]; then
  log "phase2: existing cluster found on ${DATA_DEV} — reusing"
elif [[ "${FRESH}" == "1" ]]; then
  log "phase2: initialising new cluster on ${DATA_DEV}"
  sudo -u postgres "${PG_BIN}/initdb" -D "${PGDATA}" \
    --locale=en_US.UTF-8 --encoding=UTF8 \
    --auth-local=peer --auth-host=scram-sha-256 \
    >/var/log/postgresql/initdb.log 2>&1 || die "initdb failed"
  cat >> "${PGDATA}/postgresql.conf" <<'CONF'

# PandaStack tuning
# Listen on all interfaces: the agent reaches PG by dialing the guest's
# network IP (guest_ip:5432) through the per-sandbox NATID netns, NOT via
# loopback. Binding to 127.0.0.1 only would refuse the tunnel. Each microVM
# is network-isolated (only its agent can route to it) and scram auth is
# still enforced below.
listen_addresses = '*'
shared_buffers = 256MB
work_mem = 8MB
max_connections = 200
wal_level = logical
max_replication_slots = 10
max_wal_senders = 10
CONF
  # Allow the agent's PG tunnel (arrives on the guest network interface) in
  # addition to loopback (local broker/pgbouncer). Password (scram) required.
  printf 'host all all 0.0.0.0/0 scram-sha-256\nhost all all ::1/128 scram-sha-256\n' \
    >> "${PGDATA}/pg_hba.conf"
  chown postgres:postgres "${PGDATA}/postgresql.conf" "${PGDATA}/pg_hba.conf"
else
  die "ext4 present but no postgres cluster and device was not freshly formatted — refusing to initdb (corruption guard)"
fi

# ── WAL archiving (relay to host agent) ──────────────────────────────────────
# Idempotent: applied to fresh AND pre-existing clusters. The helper script
# no-ops (exit 0) until the agent injects /etc/pandastack/wal.env, so turning
# archive_mode on is safe even when no relay is configured — postgres recycles
# segments instead of retaining them.
if ! grep -q '^archive_mode' "${PGDATA}/postgresql.conf" 2>/dev/null; then
  log "phase2: enabling WAL archiving (relay helper)"
  cat >> "${PGDATA}/postgresql.conf" <<'CONF'

# PandaStack WAL archiving (segments POSTed to the host agent relay)
archive_mode = on
archive_command = '/usr/local/bin/pandastack-wal-archive %p %f'
archive_timeout = 60
CONF
  chown postgres:postgres "${PGDATA}/postgresql.conf"
fi

# ── Start postgresql (pg_ctl, self-contained PGDATA) ─────────────────────────
log "phase2: starting postgresql ${PG_VERSION}"
if sudo -u postgres "${PG_CTL}" -D "${PGDATA}" status >/dev/null 2>&1; then
  log "postgresql already running"
else
  sudo -u postgres "${PG_CTL}" -D "${PGDATA}" \
    -l /var/log/postgresql/postgresql.log \
    start -w -t 60 \
    || die "postgresql failed to start"
fi

for _ in $(seq 1 30); do
  sudo -u postgres "${PG_BIN}/pg_isready" -q && break
  sleep 1
done
log "postgresql ready"

# ── Bootstrap user/db/extensions (idempotent) ────────────────────────────────
log "bootstrapping database objects"
sudo -u postgres psql -v ON_ERROR_STOP=1 -c \
  "DO \$\$BEGIN
     IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='pandastack') THEN
       CREATE ROLE pandastack WITH LOGIN CREATEDB NOSUPERUSER CONNECTION LIMIT 200;
     END IF;
   END\$\$;"

DB_EXISTS=$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='pandastack'" 2>/dev/null || true)
if [[ "${DB_EXISTS}" != "1" ]]; then
  sudo -u postgres psql -v ON_ERROR_STOP=1 -c \
    "CREATE DATABASE pandastack OWNER pandastack ENCODING 'UTF8'
     LC_COLLATE 'en_US.utf8' LC_CTYPE 'en_US.utf8' TEMPLATE template0;"
fi

sudo -u postgres psql -v ON_ERROR_STOP=1 -d pandastack <<'SQL'
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "pg_stat_statements";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
CREATE EXTENSION IF NOT EXISTS "ltree";
CREATE EXTENSION IF NOT EXISTS "hstore";
CREATE EXTENSION IF NOT EXISTS "vector";
CREATE EXTENSION IF NOT EXISTS "unaccent";
GRANT ALL ON SCHEMA public TO pandastack;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES    TO pandastack;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO pandastack;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON FUNCTIONS TO pandastack;
SQL
log "bootstrap complete"

# ── Rotate credentials (per-sandbox) ─────────────────────────────────────────
mkdir -p "${PANDASTACK_DIR}"
printf '%s' "${PG_PASSWORD}"  > "${PANDASTACK_DIR}/pg.password"
printf '%s' "${BROKER_TOKEN}" > "${PANDASTACK_DIR}/broker.token"
chmod 600 "${PANDASTACK_DIR}/pg.password" "${PANDASTACK_DIR}/broker.token"

log "phase2: rotating credentials"
sudo -u postgres psql -v ON_ERROR_STOP=1 -c \
  "ALTER USER pandastack WITH PASSWORD '${PG_PASSWORD}';" \
  >/dev/null 2>&1 || die "ALTER USER failed"

PG_HASH=$(sudo -u postgres psql -t -A -c \
  "SELECT passwd FROM pg_shadow WHERE usename='pandastack'" \
  2>/dev/null | head -1 || true)

if [[ -n "${PG_HASH}" ]]; then
  printf '"pandastack" "%s"\n' "${PG_HASH}" > /etc/pgbouncer/userlist.txt
else
  printf '"pandastack" "%s"\n' "${PG_PASSWORD}" > /etc/pgbouncer/userlist.txt
fi
chmod 640 /etc/pgbouncer/userlist.txt

# BROKER_ADDR binds all interfaces for the same reason postgres sets
# listen_addresses='*' above: the platform reaches the broker by dialing the
# guest's network IP (guest_ip:5544) through the per-sandbox NATID netns, NOT
# via loopback. The microVM is network-isolated (only its host agent can route
# to it) and every data endpoint still requires the bearer broker token.
cat > "${PANDASTACK_DIR}/broker.env" <<ENV
PG_HOST=127.0.0.1
PG_PORT=5432
PG_USER=pandastack
PG_PASSWORD=${PG_PASSWORD}
PG_SSLMODE=disable
BROKER_TOKEN_FILE=${PANDASTACK_DIR}/broker.token
BROKER_ADDR=0.0.0.0:5544
BROKER_METRICS_ADDR=127.0.0.1:5545
ENV
chmod 600 "${PANDASTACK_DIR}/broker.env"
log "credentials rotated"

# ── PgBouncer ────────────────────────────────────────────────────────────────
log "starting pgbouncer"
mkdir -p /var/run/pgbouncer /var/log/pgbouncer
chown postgres:postgres /var/run/pgbouncer /var/log/pgbouncer
if [[ -f /var/run/pgbouncer/pgbouncer.pid ]]; then
  old_pid=$(cat /var/run/pgbouncer/pgbouncer.pid 2>/dev/null || true)
  kill "$old_pid" 2>/dev/null || true
  rm -f /var/run/pgbouncer/pgbouncer.pid
  sleep 0.3
fi
sudo -u postgres pgbouncer -d /etc/pgbouncer/pgbouncer.ini \
  || log "WARNING: pgbouncer start failed (non-fatal)"

# ── Query broker ─────────────────────────────────────────────────────────────
log "starting pds-query-broker"
set -a
# shellcheck disable=SC1091
source "${PANDASTACK_DIR}/broker.env"
set +a
if [[ -f /run/pandastack/broker.pid ]]; then
  old_pid=$(cat /run/pandastack/broker.pid 2>/dev/null || true)
  kill "$old_pid" 2>/dev/null || true
  rm -f /run/pandastack/broker.pid
fi
touch /var/log/pds-query-broker.log
/usr/local/bin/pds-query-broker >>/var/log/pds-query-broker.log 2>&1 &
echo $! > /run/pandastack/broker.pid
log "pds-query-broker started (pid $(cat /run/pandastack/broker.pid))"

for i in $(seq 1 15); do
  if curl -sf http://127.0.0.1:5544/v1/health >/dev/null 2>&1; then
    log "broker healthy after ${i}s"
    break
  fi
  sleep 1
done

# ── Write ready file ─────────────────────────────────────────────────────────
cat > "${READY_FILE}" <<JSON
{
  "status": "ready",
  "broker_url": "http://127.0.0.1:5544",
  "broker_token": "${BROKER_TOKEN}",
  "pg_host": "127.0.0.1",
  "pg_port": 5432,
  "pg_user": "pandastack",
  "pg_password": "${PG_PASSWORD}",
  "pgbouncer_port": 6432,
  "default_database": "pandastack",
  "started_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
JSON
chmod 644 "${READY_FILE}"

log "=== postgres-16 sandbox ready (durable volume ${DATA_DEV}) ==="
log "  broker : http://127.0.0.1:5544"
log "  psql   : postgres://pandastack:****@127.0.0.1:5432/pandastack"

exec tail -f /var/log/pds-query-broker.log
