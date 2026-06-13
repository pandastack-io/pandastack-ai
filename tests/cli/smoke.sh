#!/usr/bin/env bash
# tests/cli/smoke.sh — pandastack CLI lifecycle smoke test.
# Requires PANDASTACK_API + PANDASTACK_TOKEN already exported.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CLI="$ROOT/bin/pandastack"
[[ -x "$CLI" ]] || { echo "[cli] missing $CLI — build it first (go build ./cmd/pandastack)"; exit 1; }

TEMPLATE="${PANDASTACK_E2E_TEMPLATE:-ubuntu-24.04}"

echo "[cli] whoami"
"$CLI" auth whoami || true   # may print "anonymous" if token has no email scope

echo "[cli] template list"
"$CLI" template list -o json | head -c 400 && echo

echo "[cli] sandbox create --template=$TEMPLATE"
SB=$("$CLI" -o json sandbox create --template "$TEMPLATE" --ttl 5m | tr -d '\n')
ID=$(printf '%s' "$SB" | sed -E 's/.*"id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')
[[ -n "$ID" && "$ID" != "$SB" ]] || { echo "[cli] failed to parse sandbox id: $SB"; exit 1; }
echo "[cli] sandbox id = $ID"

cleanup() {
  echo "[cli] cleanup: delete $ID"
  "$CLI" sandbox delete "$ID" || true
}
trap cleanup EXIT

echo "[cli] sandbox get"
"$CLI" -o json sandbox get "$ID" | head -c 400 && echo

# Poll until running (max ~30s)
for _ in $(seq 1 30); do
  STATE=$("$CLI" -o json sandbox get "$ID" | sed -E 's/.*"state"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')
  [[ "$STATE" == "running" || "$STATE" == "ready" ]] && break
  sleep 1
done

echo "[cli] sandbox exec -- echo hello"
"$CLI" sandbox exec "$ID" --timeout 10 -- echo hello-from-pandastack

echo "[cli] ✅ cli smoke ok"
