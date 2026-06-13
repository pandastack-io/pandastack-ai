#!/bin/bash
# pandastack-wal-restore — postgres restore_command helper (failover recovery).
#
# Invoked by postgres as:  pandastack-wal-restore %f %p
#   $1 = WAL file name requested (segment or .history)
#   $2 = destination path to write it to (relative to PGDATA)
#
# Behaviour contract (OPPOSITE of pandastack-wal-archive for missing config):
#   - /etc/pandastack/wal.env absent: exit 1. restore_command only ever runs
#     on a volume restored by the agent, which always injects wal.env first;
#     silently succeeding with no file would corrupt recovery.
#   - relay 404 (segment genuinely not archived): curl -f exits 22 →
#     postgres treats it as end-of-archive and finishes recovery. Normal.
#   - relay 5xx / unreachable: curl --retry retries; if still failing the
#     nonzero exit also ends recovery — the relay therefore only returns 404
#     for true not-found and 502 for transient errors.
set -u

ENV=/etc/pandastack/wal.env
[ -f "$ENV" ] || exit 1
. "$ENV"
[ -n "${PANDASTACK_WAL_URL:-}" ] || exit 1
[ -n "${PANDASTACK_WAL_ID:-}" ] || exit 1

exec curl -fsS --max-time 120 --retry 5 --retry-connrefused \
  -H "Authorization: Bearer ${PANDASTACK_WAL_TOKEN:-}" \
  -o "$2" \
  "${PANDASTACK_WAL_URL}/wal/${PANDASTACK_WAL_ID}/$1"
