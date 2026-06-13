#!/usr/bin/env bash
# Sync NEXT_PUBLIC_* vars from root .env.local → dashboard/.env.local
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$HERE/.env.local"
DST="$HERE/dashboard/.env.local"
[ -f "$SRC" ] || { echo "missing $SRC"; exit 1; }
{
  echo "# Auto-generated from root .env.local on $(date -Iseconds)"
  echo "# Run scripts/sync-dashboard-env.sh after editing root .env.local"
  echo
  grep -E "^NEXT_PUBLIC_" "$SRC"
} > "$DST"
chmod 600 "$DST"
echo "synced $(grep -c "^NEXT_PUBLIC_" "$DST") NEXT_PUBLIC_* vars → $DST"
