#!/bin/bash
# pandastack-wal-archive — postgres archive_command helper.
#
# Invoked by postgres as:  pandastack-wal-archive %p %f
#   $1 = path to the WAL segment (relative to PGDATA)
#   $2 = segment file name
#
# Behaviour contract:
#   - /etc/pandastack/wal.env absent AND postgres started recently: exit 1 so
#     postgres RETAINS the segment. The agent delivers wal.env over SSH right
#     after boot; if that delivery is merely late, recycling segments in the
#     window would punch a permanent gap in the archive timeline (segments
#     reported "archived" but never uploaded), silently breaking restore
#     until the next base backup. Retained segments all flow, in order, the
#     moment wal.env lands — no gap.
#   - wal.env still absent after the grace window: exit 0 so postgres recycles
#     the segment — this deployment genuinely runs without archiving
#     (dev/local), and retaining forever would fill the data volume.
#   - relay unreachable / non-2xx: exit 1 so postgres keeps the segment and
#     retries — nothing is reported "archived" until the host relay has it.
set -u

WAL_ENV_GRACE_MIN=15
# Recycle regardless of grace once the data volume passes this usage — WAL
# retention must never ENOSPC postgres itself (a write-heavy first 15 minutes
# on a no-archiving deployment would otherwise fill the data volume).
WAL_DISK_PCT_MAX=80

ENV=/etc/pandastack/wal.env
if [ ! -f "$ENV" ]; then
  # Grace anchor: /run/pandastack/creds-ready — written exactly once per VM
  # BOOT (tmpfs, by the agent's phase-2 delivery or autostart's fallback).
  # Deliberately NOT postmaster.pid: postgres rewrites that on every
  # postmaster start, so a crash-looping postgres would re-arm the grace
  # window forever and retain WAL indefinitely.
  ANCHOR=/run/pandastack/creds-ready
  [ -f "$ANCHOR" ] || ANCHOR=postmaster.pid  # pre-rebake boots / belt-and-braces
  # archive_command runs with cwd=$PGDATA (on the data volume), so df . is
  # the durable volume's usage.
  DISK_PCT=$(df --output=pcent . 2>/dev/null | tail -1 | tr -dc '0-9')
  if [ -n "$DISK_PCT" ] && [ "$DISK_PCT" -ge "$WAL_DISK_PCT_MAX" ]; then
    logger -t pandastack-wal-archive \
      "wal.env absent and data volume at ${DISK_PCT}%; recycling $2 to protect postgres" 2>/dev/null || true
    exit 0
  fi
  if [ -f "$ANCHOR" ] && [ -n "$(find "$ANCHOR" -mmin +$WAL_ENV_GRACE_MIN 2>/dev/null)" ]; then
    logger -t pandastack-wal-archive \
      "wal.env absent ${WAL_ENV_GRACE_MIN}m after boot; recycling $2 (archiving disabled for this deployment)" 2>/dev/null || true
    exit 0
  fi
  # Within grace: retain and retry — delivery is expected imminently, and
  # retained segments all flow in order once wal.env lands (no archive gap).
  exit 1
fi
. "$ENV"
[ -n "${PANDASTACK_WAL_URL:-}" ] || exit 0
[ -n "${PANDASTACK_WAL_ID:-}" ] || exit 0

exec curl -fsS --max-time 60 --retry 2 \
  -H "Authorization: Bearer ${PANDASTACK_WAL_TOKEN:-}" \
  --data-binary @"$1" \
  "${PANDASTACK_WAL_URL}/wal/${PANDASTACK_WAL_ID}/$2" >/dev/null
