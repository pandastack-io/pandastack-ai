# PandaStack template · postgres-16
#
# Enterprise-grade PostgreSQL 16 sandbox with:
#   - PostgreSQL 16 (tuned for AI-agent workloads)
#   - PgBouncer 1.22 (connection pooler)
#   - pds-query-broker (REST JSON-over-SQL bridge, port 5544)
#   - pg_stat_statements, pgcrypto, uuid-ossp, pg_trgm, ltree, hstore, vector
#   - Logical replication ready (wal_level = logical)
#   - Prometheus metrics (port 5545)
#
# Bake:
#   pandastack template build -f Dockerfile -n postgres-16 --size-mb 4096

# ── Stage 1: build the REST query broker ─────────────────────────────────────
FROM golang:1.23-alpine AS broker-build

WORKDIR /src
COPY query-broker/go.mod query-broker/go.sum* ./
RUN go mod download
COPY query-broker/ ./
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=$(date -u +%Y%m%d)" \
    -trimpath \
    -o /pds-query-broker .

# ── Stage 2: final rootfs ─────────────────────────────────────────────────────
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8 \
    TZ=UTC

# ── Base system + locale ─────────────────────────────────────────────────────
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl gnupg lsb-release locales tzdata \
    procps net-tools iproute2 \
    busybox e2fsprogs \
    systemd systemd-sysv openssh-server sudo \
    chrony socat fuse3 iptables nfs-common \
    && locale-gen en_US.UTF-8 \
    && rm -rf /var/lib/apt/lists/* \
    && rm -rf /usr/share/doc /usr/share/man /usr/share/info \
    && rm -rf /usr/share/locale/*

# ── PostgreSQL 16 via PGDG ───────────────────────────────────────────────────
RUN curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
        | gpg --dearmor -o /etc/apt/trusted.gpg.d/postgresql.gpg \
    && echo "deb [signed-by=/etc/apt/trusted.gpg.d/postgresql.gpg] \
        http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update && apt-get install -y --no-install-recommends \
        postgresql-16 \
        postgresql-client-16 \
        postgresql-16-pgvector \
        postgresql-contrib \
        pgbouncer \
    && rm -rf /var/lib/apt/lists/* \
    && rm -rf /usr/share/doc /usr/share/man /usr/share/info

# ── Bake cluster into rootfs (Sandflare pattern) ─────────────────────────────
# Ensure a cluster exists after apt-get (Ubuntu 24.04 usually creates one, but
# not guaranteed in Docker). Patch conf files IN-PLACE with sed (ASCII only).
# NEVER copy our custom UTF-8 conf over the cluster conf — pg_ctlcluster (Perl)
# crashes with "invalid line 1" on any non-ASCII byte. autostart.sh uses pg_ctl
# (C binary) directly which handles any encoding, but a clean ASCII conf is best.
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
RUN \
    PG_CONF="/etc/postgresql/16/main/postgresql.conf" && \
    PG_HBA="/etc/postgresql/16/main/pg_hba.conf" && \
    (pg_lsclusters | grep -q "^16[[:space:]]*main" \
      || pg_createcluster --locale en_US.UTF-8 --encoding UTF8 16 main -- \
           --auth-local peer --auth-host scram-sha-256) && \
    sed -i "s/^#*listen_addresses[[:space:]]*=.*/listen_addresses = '*'/" "$PG_CONF" && \
    (grep -q "^listen_addresses" "$PG_CONF" || echo "listen_addresses = '*'" >> "$PG_CONF") && \
    printf '\n# PandaStack tuning\nshared_buffers = 256MB\nwork_mem = 8MB\nmax_connections = 200\nwal_level = logical\nmax_replication_slots = 10\nmax_wal_senders = 10\n' >> "$PG_CONF" && \
    (grep -q "0\.0\.0\.0/0" "$PG_HBA" \
      || printf 'host all all 0.0.0.0/0 scram-sha-256\nhost all all ::/0 scram-sha-256\n' >> "$PG_HBA") && \
    mkdir -p /etc/pgbouncer /var/log/pgbouncer /var/run/pgbouncer && \
    chown postgres:postgres /var/log/pgbouncer /var/run/pgbouncer

# Phase 4 durable-volume model: postgres data lives on a per-database /dev/vdb
# image, NOT in this rootfs cluster. autostart.sh manages postgres via pg_ctl,
# so Debian's auto-start units must be disabled — otherwise the rootfs cluster
# would start during boot and be captured RUNNING in the template snapshot,
# which is unsafe when the data device is swapped in at restore.
RUN systemctl disable postgresql.service 2>/dev/null || true; \
    systemctl disable postgresql@16-main.service 2>/dev/null || true; \
    systemctl mask postgresql@16-main.service 2>/dev/null || true

SHELL ["/bin/sh", "-c"]

# ── Copy broker binary ───────────────────────────────────────────────────────
COPY --from=broker-build /pds-query-broker /usr/local/bin/pds-query-broker

# ── Copy pgbouncer config ────────────────────────────────────────────────────
# pgbouncer.ini goes directly to /etc/pgbouncer/ — autostart.sh starts it there.
# postgresql.conf and pg_hba.conf are NOT copied here; they are patched in-place
# above so no UTF-8 ever reaches those files.
COPY conf/pgbouncer.ini /etc/pgbouncer/pgbouncer.ini
RUN chown postgres:postgres /etc/pgbouncer/pgbouncer.ini

# ── Copy autostart + health scripts ─────────────────────────────────────────
# init-db.sh (pds-pg-init) is no longer needed: the cluster is pre-baked above
# and autostart.sh bootstraps user/db/extensions idempotently on each boot.
COPY scripts/autostart.sh     /etc/pandastack/autostart.sh
COPY scripts/healthcheck.sh   /usr/local/bin/pds-pg-health
COPY scripts/pandastack-wal-archive.sh /usr/local/bin/pandastack-wal-archive
COPY scripts/pandastack-wal-restore.sh /usr/local/bin/pandastack-wal-restore
RUN chmod +x \
    /usr/local/bin/pds-query-broker \
    /etc/pandastack/autostart.sh \
    /usr/local/bin/pds-pg-health \
    /usr/local/bin/pandastack-wal-archive \
    /usr/local/bin/pandastack-wal-restore

# ── pandastack-autostart systemd service ─────────────────────────────────────
RUN printf '#!/bin/sh\nset -e\nSCRIPT=/etc/pandastack/autostart.sh\n[ -x "$SCRIPT" ] || exit 0\nexec /bin/bash -lc "$SCRIPT"\n' \
        > /usr/local/bin/pandastack-autostart && \
    chmod 0755 /usr/local/bin/pandastack-autostart && \
    printf '[Unit]\nDescription=PandaStack template autostart hook\nAfter=network-online.target sshd.service\nWants=network-online.target\nConditionPathExists=/etc/pandastack/autostart.sh\n\n[Service]\nType=simple\nExecStartPre=/bin/mkdir -p /workspace\nExecStart=/usr/local/bin/pandastack-autostart\nRestart=on-failure\nRestartSec=3s\nStandardOutput=journal+console\nStandardError=journal+console\n\n[Install]\nWantedBy=multi-user.target\n' \
        > /etc/systemd/system/pandastack-autostart.service && \
    systemctl enable pandastack-autostart.service

WORKDIR /workspace
CMD ["/bin/bash"]
