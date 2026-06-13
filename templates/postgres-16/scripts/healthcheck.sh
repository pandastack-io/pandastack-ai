#!/usr/bin/env bash
# pds-pg-health — Health check for the postgres-16 sandbox.
# Exits 0 if all services are up, non-zero otherwise.
# Used by systemd and the PandaStack agent healthz endpoint.

set -euo pipefail

PANDASTACK_DIR="/etc/pandastack"
ok=true

check() {
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then
    printf '[OK]  %s\n' "$name"
  else
    printf '[ERR] %s\n' "$name" >&2
    ok=false
  fi
}

# PostgreSQL
check "postgresql" pg_ctlcluster 16 main status

# PgBouncer
check "pgbouncer" test -f /var/run/pgbouncer/pgbouncer.pid
if [[ -f /var/run/pgbouncer/pgbouncer.pid ]]; then
  pid=$(cat /var/run/pgbouncer/pgbouncer.pid)
  check "pgbouncer-pid" kill -0 "$pid"
fi

# Query broker HTTP
check "broker-http" curl -sf --max-time 3 http://127.0.0.1:5544/v1/health

# Read token for authenticated check
BROKER_TOKEN=$(cat "${PANDASTACK_DIR}/broker.token" 2>/dev/null || echo "")
if [[ -n "$BROKER_TOKEN" ]]; then
  check "broker-auth" curl -sf --max-time 3 \
    -H "Authorization: Bearer ${BROKER_TOKEN}" \
    http://127.0.0.1:5544/v1/databases
fi

if [[ "$ok" == "true" ]]; then
  echo "healthy"
  exit 0
else
  echo "unhealthy" >&2
  exit 1
fi
