#!/bin/bash
# pandastack-wal-archive — postgres archive_command helper.
#
# Invoked by postgres as:  pandastack-wal-archive %p %f
#   $1 = path to the WAL segment (relative to PGDATA)
#   $2 = segment file name
#
# Behaviour contract:
#   - /etc/pandastack/wal.env absent (archiving unconfigured): exit 0 so
#     postgres recycles the segment instead of retaining WAL forever and
#     filling the data volume.
#   - relay unreachable / non-2xx: exit 1 so postgres keeps the segment and
#     retries — nothing is reported "archived" until the host relay has it.
set -u

ENV=/etc/pandastack/wal.env
[ -f "$ENV" ] || exit 0
. "$ENV"
[ -n "${PANDASTACK_WAL_URL:-}" ] || exit 0
[ -n "${PANDASTACK_WAL_ID:-}" ] || exit 0

exec curl -fsS --max-time 60 --retry 2 \
  -H "Authorization: Bearer ${PANDASTACK_WAL_TOKEN:-}" \
  --data-binary @"$1" \
  "${PANDASTACK_WAL_URL}/wal/${PANDASTACK_WAL_ID}/$2" >/dev/null
