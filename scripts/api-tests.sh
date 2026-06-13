#!/usr/bin/env bash
# pandastack API test harness. Exercises every endpoint added through Phase 2.
# Run against the macOS control-plane API (default :8080).
#
#   ./scripts/api-tests.sh                 # full sweep
#   API=http://localhost:8080 ./scripts/api-tests.sh
#
# Requires: curl, jq.

set -euo pipefail

API="${API:-http://localhost:8080}"
KEY="${PANDASTACK_API_KEY:-}"

H=(-H "content-type: application/json")
[[ -n "$KEY" ]] && H+=(-H "X-API-Key: $KEY")

c() { curl -sS --fail-with-body "${H[@]}" "$@"; }

section() { printf "\n\033[1;36m── %s ──\033[0m\n" "$*"; }
ok()      { printf "  \033[32m✓\033[0m %s\n" "$*"; }
fail()    { printf "  \033[31m✗\033[0m %s\n" "$*"; exit 1; }

section "healthz"
c "$API/healthz" >/dev/null && ok "control-plane up"

section "templates"
TPLS=$(c "$API/v1/templates")
echo "$TPLS" | jq -e 'length > 0' >/dev/null && ok "at least 1 template"
TPL_NAME=$(echo "$TPLS" | jq -r '.[0].name')
ok "first template: $TPL_NAME"
c "$API/v1/templates/$TPL_NAME" | jq -e .name >/dev/null && ok "GET /templates/$TPL_NAME"

section "create sandbox"
SB=$(c -X POST "$API/v1/sandboxes" -d "{\"template\":\"$TPL_NAME\",\"cpu\":1,\"memory_mb\":256}")
ID=$(echo "$SB" | jq -r .id)
IP=$(echo "$SB" | jq -r .guest_ip)
ok "created $ID (ip=$IP)"

cleanup() {
  echo
  curl -sS "${H[@]}" -X DELETE "$API/v1/sandboxes/$ID" >/dev/null || true
  echo "cleaned up $ID"
}
trap cleanup EXIT

section "wait for ssh ready (poll exec)"
for i in {1..60}; do
  if RES=$(curl -sS "${H[@]}" -X POST "$API/v1/sandboxes/$ID/exec" -d '{"cmd":"echo ready"}' 2>/dev/null); then
    if [[ "$(echo "$RES" | jq -r .stdout 2>/dev/null)" == "ready" ]]; then
      ok "ssh up after ${i}s"
      break
    fi
  fi
  sleep 1
  [[ $i -eq 60 ]] && fail "ssh never came up"
done

section "exec sync"
RES=$(c -X POST "$API/v1/sandboxes/$ID/exec" -d '{"cmd":"uname -a"}')
echo "$RES" | jq -e '.stdout | contains("Linux")' >/dev/null && ok "uname -a"
RES=$(c -X POST "$API/v1/sandboxes/$ID/exec" -d '{"cmd":"false"}')
echo "$RES" | jq -e '.exit_code == 1' >/dev/null && ok "non-zero exit captured"

section "exec stream (SSE)"
OUT=$(curl -sS -N "${H[@]}" -X POST "$API/v1/sandboxes/$ID/exec/stream" \
        -d '{"cmd":"for i in 1 2 3; do echo line$i; sleep 0.1; done"}')
echo "$OUT" | grep -q 'event: stdout' && ok "stdout frames received"
echo "$OUT" | grep -q '"exit_code":0' && ok "exit frame received"

section "filesystem"
c -X PUT "$API/v1/sandboxes/$ID/fs?path=/tmp/hello.txt" \
   -H "content-type: application/octet-stream" --data-binary "hello pandastack" >/dev/null
ok "PUT /tmp/hello.txt"
BODY=$(c "$API/v1/sandboxes/$ID/fs?path=/tmp/hello.txt")
[[ "$BODY" == "hello pandastack" ]] && ok "GET roundtrip"
c "$API/v1/sandboxes/$ID/fs/dir?path=/tmp" | jq -e '.entries | map(.name) | index("hello.txt")' >/dev/null && ok "listed in /tmp"
c "$API/v1/sandboxes/$ID/fs/stat?path=/tmp/hello.txt" | jq -e '.size == 15' >/dev/null && ok "stat size correct"
c -X DELETE "$API/v1/sandboxes/$ID/fs?path=/tmp/hello.txt" >/dev/null && ok "DELETE"

section "logs"
c "$API/v1/sandboxes/$ID/logs" >/dev/null && ok "GET logs (snapshot)"

section "metrics"
M=$(c "$API/v1/sandboxes/$ID/metrics")
echo "$M" | jq -e '.pid > 0' >/dev/null && ok "metrics returned pid=$(echo "$M" | jq .pid)"
echo "$M" | jq -e '.host_rss_bytes > 0' >/dev/null && ok "RSS > 0 ($(echo "$M" | jq .host_rss_bytes) bytes)"

section "list / get / pause / resume / snapshot"
c "$API/v1/sandboxes" | jq -e --arg id "$ID" 'map(.id) | index($id)' >/dev/null && ok "appears in list"
c -X POST "$API/v1/sandboxes/$ID/pause" >/dev/null && ok "paused"
c -X POST "$API/v1/sandboxes/$ID/resume" >/dev/null && ok "resumed"
SNAP=$(c -X POST "$API/v1/sandboxes/$ID/snapshots")
echo "$SNAP" | jq -e .id >/dev/null && ok "snapshot created: $(echo "$SNAP" | jq -r .id)"

section "events (phase 3)"
EV=$(c "$API/v1/sandboxes/$ID/events?tail=100")
echo "$EV" | jq -e '.events | length > 0' >/dev/null && ok "events log has $(echo "$EV" | jq '.events | length') entries"
echo "$EV" | jq -e '.events | map(.type) | index("sandbox.running")' >/dev/null && ok "sandbox.running event recorded"
echo "$EV" | jq -e '.events | map(.type) | index("sandbox.paused")' >/dev/null && ok "sandbox.paused event recorded"

section "fork (phase 3)"
FORK=$(c -X POST "$API/v1/sandboxes/$ID/fork" -d '{"count":2}')
echo "$FORK" | jq -e '.children | length == 2' >/dev/null && ok "forked 2 children"
C1=$(echo "$FORK" | jq -r '.children[0]')
C2=$(echo "$FORK" | jq -r '.children[1]')
ok "child 1: $C1"
ok "child 2: $C2"
sleep 3
c "$API/v1/sandboxes/$C1" | jq -e '.status == "running"' >/dev/null && ok "child 1 is running"
c "$API/v1/sandboxes/$C2" | jq -e '.status == "running"' >/dev/null && ok "child 2 is running"

section "hibernate / wake (phase 3)"
c -X POST "$API/v1/sandboxes/$ID/hibernate" >/dev/null && ok "hibernated"
sleep 1
c "$API/v1/sandboxes/$ID" | jq -e '.status == "hibernated"' >/dev/null && ok "status=hibernated"
c -X POST "$API/v1/sandboxes/$ID/wake" >/dev/null && ok "woken"
sleep 2
c "$API/v1/sandboxes/$ID" | jq -e '.status == "running"' >/dev/null && ok "status=running"
NEWEV=$(c "$API/v1/sandboxes/$ID/events?tail=10")
echo "$NEWEV" | jq -e '.events | map(.type) | index("sandbox.hibernated")' >/dev/null && ok "hibernated event"
echo "$NEWEV" | jq -e '.events | map(.type) | index("sandbox.woken")' >/dev/null && ok "woken event"

section "cleanup forks"
c -X DELETE "$API/v1/sandboxes/$C1" >/dev/null && ok "deleted child 1"
c -X DELETE "$API/v1/sandboxes/$C2" >/dev/null && ok "deleted child 2"

printf "\n\033[1;32mAll phase 1+2+3 tests passed.\033[0m\n"
