#!/usr/bin/env bash
# Pandastack end-to-end integration test.
# Hits a live API+agent and asserts every major surface works.
# Use: API_URL=http://localhost:8080 ./bench/integration.sh
set -euo pipefail

API="${API_URL:-http://localhost:8080}"
TEMPLATE="${TEMPLATE:-ubuntu-24.04}"
RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; DIM=$'\033[2m'; OFF=$'\033[0m'
PASS=0; FAIL=0

pass() { printf "${GREEN}✓${OFF} %s\n" "$1"; PASS=$((PASS+1)); }
fail() { printf "${RED}✗${OFF} %s\n   ${DIM}%s${OFF}\n" "$1" "${2:-}"; FAIL=$((FAIL+1)); }
section() { printf "\n${YELLOW}── %s ──${OFF}\n" "$1"; }

ms() { python3 -c 'import time;print(int(time.time()*1000))'; }
get() { curl -fsS "$API$1" -H "X-Request-Id: it-$$-$1"; }
post() { curl -fsS -X POST "$API$1" -H 'content-type: application/json' -d "${2:-}"; }
del() { curl -fsS -X DELETE "$API$1"; }

section "Liveness"
get /healthz | grep -q '"ok"' && pass "/healthz" || fail "/healthz"
get /version | grep -q "pandastack-api" && pass "/version (api)" || fail "/version (api)"
get /v1/version | grep -q "pandastack-agent" && pass "/v1/version (agent via proxy)" || fail "/v1/version"

section "Request correlation"
RID=$(curl -sS -D - -o /dev/null "$API/v1/sandboxes" -H "X-Request-Id: it-cor-test" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-request-id"{print $2; exit}')
[ "$RID" = "it-cor-test" ] && pass "X-Request-Id echoed through proxy" || fail "X-Request-Id not echoed (got: $RID)"

section "Observability"
get /v1/metrics | grep -q 'pandastack_http_requests_total' && pass "/metrics emits Prometheus text" || fail "/metrics"
get /v1/quotas | grep -q '"workspace"' && pass "/quotas returns policy + usage" || fail "/quotas"
get /v1/stats/boot | grep -q 'total_samples' && pass "/stats/boot returns aggregates" || fail "/stats/boot"

section "Sandbox lifecycle"
T0=$(ms)
SB=$(post /v1/sandboxes '{"template":"'"$TEMPLATE"'","cpu":1,"memory_mb":512}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
T1=$(ms)
[ -n "$SB" ] && pass "create sandbox ($(( T1-T0 ))ms wall)" || fail "create sandbox"

sleep 1
STATUS=$(get "/v1/sandboxes/$SB" | python3 -c 'import json,sys;print(json.load(sys.stdin)["status"])')
[ "$STATUS" = "running" ] && pass "sandbox reaches running" || fail "sandbox status=$STATUS"

BMS=$(get "/v1/sandboxes/$SB" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("boot_ms",0))')
[ "$BMS" -gt 0 ] && pass "boot_ms recorded: ${BMS}ms" || fail "boot_ms missing"

section "Secrets hygiene"
post "/v1/sandboxes/$SB/metadata" '{"secret.api_key":"sup3rs3cret","label":"team-a"}' >/dev/null 2>&1 || true
ROW=$(get "/v1/sandboxes/$SB" || true)
if echo "$ROW" | grep -q 'sup3rs3cret'; then
  fail "secret.* metadata leaked through API"
else
  pass "secret.* metadata not echoed back"
fi

section "Pause/Resume"
post "/v1/sandboxes/$SB/pause" '' >/dev/null && pass "pause" || fail "pause"
sleep 1
post "/v1/sandboxes/$SB/resume" '' >/dev/null && pass "resume" || fail "resume"

section "Cleanup"
del "/v1/sandboxes/$SB" >/dev/null && pass "delete" || fail "delete"

section "Audit log records mutations"
sleep 1
AUDIT=$(get "/v1/audit?limit=10")
echo "$AUDIT" | grep -q '"POST"' && pass "audit log captures POST" || fail "audit missing POST"
echo "$AUDIT" | grep -q '"DELETE"' && pass "audit log captures DELETE" || fail "audit missing DELETE"

printf "\n"
if [ $FAIL -eq 0 ]; then
  printf "${GREEN}━━ %d passed, 0 failed ━━${OFF}\n" "$PASS"
  exit 0
else
  printf "${RED}━━ %d passed, %d failed ━━${OFF}\n" "$PASS" "$FAIL"
  exit 1
fi
