#!/usr/bin/env bash
# pds-pg-init — PostgreSQL initialiser for the postgres-16 template.
#
# Called ONCE during template bake (bake-templates.sh tpl::postgres-16)
# AFTER the auto-created cluster is dropped.  The result is baked into the
# rootfs.ext4 snapshot so every sandbox starts with a pre-initialised cluster.
#
# At sandbox boot autostart.sh just rotates the credentials with a single
# ALTER USER (fast) — no cluster creation needed.
#
# Idempotent: exits 0 immediately if PG_DATA/PG_VERSION already exists
# (protects against accidental double-run).
#
# What this script does:
#  1. Creates the postgres-16 cluster with correct locale/encoding.
#  2. Applies tuned postgresql.conf + pg_hba.conf.
#  3. Starts PostgreSQL temporarily to run SQL bootstrap.
#  4. Creates the pandastack role, database, and default extensions.
#  5. Generates placeholder credentials (rotated on every sandbox boot).
#  6. Configures PgBouncer userlist with placeholder hash.
#  7. Stops PostgreSQL cleanly (autostart.sh starts it on each boot).

set -euo pipefail

PG_VERSION=16
PG_CLUSTER=main
PG_DATA="/var/lib/postgresql/${PG_VERSION}/${PG_CLUSTER}"
PG_CONF_DIR="/etc/postgresql/${PG_VERSION}/${PG_CLUSTER}"
PANDASTACK_DIR="/etc/pandastack"
PGBOUNCER_DIR="/etc/pgbouncer"

log() { printf '[%s] [pds-pg-init] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# ── Already initialised? ──────────────────────────────────────────────────────
if [[ -f "$PG_DATA/PG_VERSION" ]]; then
  log "cluster already exists — skipping (bake already ran)"
  exit 0
fi

log "=== PandaStack PostgreSQL first-boot init ==="

# ── 1. Create cluster ─────────────────────────────────────────────────────────
log "creating cluster pg ${PG_VERSION}/${PG_CLUSTER}"
pg_createcluster --locale en_US.UTF-8 --encoding UTF8 "${PG_VERSION}" "${PG_CLUSTER}" \
  -- --auth-local peer --auth-host scram-sha-256 \
  >/dev/null 2>&1 || die "pg_createcluster failed"

# ── 2. Apply tuned config ─────────────────────────────────────────────────────
log "applying tuned postgresql.conf + pg_hba.conf"
cp "${PANDASTACK_DIR}/postgresql.conf" "${PG_CONF_DIR}/postgresql.conf"
cp "${PANDASTACK_DIR}/pg_hba.conf"     "${PG_CONF_DIR}/pg_hba.conf"

# ── 3. Start Postgres for bootstrap SQL ───────────────────────────────────────
log "starting postgres for bootstrap"
pg_ctlcluster "${PG_VERSION}" "${PG_CLUSTER}" start -- -w -t 30 \
  >/dev/null 2>&1 || die "postgres failed to start for bootstrap"

# ── 4. Generate credentials ───────────────────────────────────────────────────
PG_PASSWORD="$(openssl rand -base64 32 | tr -d '/+=')"
BROKER_TOKEN="pds_pg_$(openssl rand -hex 24)"

mkdir -p "${PANDASTACK_DIR}"
printf '%s' "${PG_PASSWORD}"   > "${PANDASTACK_DIR}/pg.password"
printf '%s' "${BROKER_TOKEN}"  > "${PANDASTACK_DIR}/broker.token"
chmod 600 "${PANDASTACK_DIR}/pg.password" "${PANDASTACK_DIR}/broker.token"

log "credentials written to ${PANDASTACK_DIR}/"

# ── 5. Bootstrap SQL ──────────────────────────────────────────────────────────
log "running bootstrap SQL"
sudo -u postgres psql -v ON_ERROR_STOP=1 <<SQL
-- Create application role with strong password.
CREATE ROLE pandastack WITH
  LOGIN
  PASSWORD '${PG_PASSWORD}'
  NOSUPERUSER
  NOCREATEDB
  NOCREATEROLE
  INHERIT
  CONNECTION LIMIT 200;

-- Allow pandastack to create databases (needed for broker's CREATE DATABASE).
ALTER ROLE pandastack CREATEDB;

-- Create the default application database.
CREATE DATABASE pandastack
  OWNER pandastack
  ENCODING 'UTF8'
  LC_COLLATE 'en_US.utf8'
  LC_CTYPE   'en_US.utf8'
  TEMPLATE template0;

-- Install extensions into the pandastack database.
\c pandastack

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "pg_stat_statements";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
CREATE EXTENSION IF NOT EXISTS "ltree";
CREATE EXTENSION IF NOT EXISTS "hstore";
CREATE EXTENSION IF NOT EXISTS "vector";        -- pgvector for AI embeddings
CREATE EXTENSION IF NOT EXISTS "unaccent";

-- Grant schema privileges.
GRANT ALL ON SCHEMA public TO pandastack;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES    TO pandastack;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO pandastack;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON FUNCTIONS TO pandastack;

-- Apply the same extensions to the postgres admin db so broker health check works.
\c postgres

CREATE EXTENSION IF NOT EXISTS "pg_stat_statements";
SQL

log "bootstrap SQL complete"

# ── 6. PgBouncer userlist ─────────────────────────────────────────────────────
log "configuring pgbouncer"
mkdir -p "${PGBOUNCER_DIR}"

# PgBouncer needs the scram-sha-256 hash from pg_shadow.
# pg_shadow.passwd already contains the full "SCRAM-SHA-256$..." string.
PG_HASH=$(sudo -u postgres psql -t -A -c \
  "SELECT passwd FROM pg_shadow WHERE usename='pandastack'" \
  2>/dev/null | head -1 || true)

if [[ -n "${PG_HASH}" ]]; then
  printf '"%s" "%s"\n' "pandastack" "${PG_HASH}" > "${PGBOUNCER_DIR}/userlist.txt"
else
  # Fallback: plain password (less secure, but functional if hash extraction fails)
  printf '"%s" "%s"\n' "pandastack" "${PG_PASSWORD}" > "${PGBOUNCER_DIR}/userlist.txt"
fi
chmod 640 "${PGBOUNCER_DIR}/userlist.txt"
chown root:postgres "${PGBOUNCER_DIR}/userlist.txt"

# Copy broker-ready pgbouncer.ini.
cp "${PANDASTACK_DIR}/pgbouncer.ini" "${PGBOUNCER_DIR}/pgbouncer.ini"
chown postgres:postgres "${PGBOUNCER_DIR}/pgbouncer.ini"

mkdir -p /var/log/pgbouncer /var/run/pgbouncer
chown postgres:postgres /var/log/pgbouncer /var/run/pgbouncer

# ── 7. Write PG_PASSWORD for broker env ───────────────────────────────────────
# Broker reads PG_PASSWORD from the environment file.
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

# ── 8. Write the init summary (readable by API on first create) ───────────────
cat > "${PANDASTACK_DIR}/init-complete.json" <<JSON
{
  "initialized_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "pg_version": "${PG_VERSION}",
  "default_database": "pandastack",
  "pg_user": "pandastack",
  "broker_port": 5544,
  "pgbouncer_port": 6432,
  "extensions": ["uuid-ossp","pgcrypto","pg_stat_statements","pg_trgm","ltree","hstore","vector","unaccent"]
}
JSON
chmod 644 "${PANDASTACK_DIR}/init-complete.json"

# ── 9. Stop Postgres (will be restarted by autostart.sh via systemd) ──────────
log "stopping postgres (will restart via systemd autostart)"
pg_ctlcluster "${PG_VERSION}" "${PG_CLUSTER}" stop -m fast \
  >/dev/null 2>&1 || true

log "=== first-boot init complete ==="
log "broker token: $(cat "${PANDASTACK_DIR}/broker.token")"
log "pg password : $(cat "${PANDASTACK_DIR}/pg.password")"
