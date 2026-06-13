#!/usr/bin/env bash
# pandastack apps API test harness — git-driven app hosting.
# Exercises the full /v1/apps surface against the local control-plane (default :8080).
# Apps are Postgres-backed, so this requires the API running with a Postgres DSN
# (the mac-local-e2e flow uses local Docker Postgres — the no-cloud path).
#
#   ./scripts/apps-tests.sh                              # CRUD + deploy lifecycle
#   API=http://localhost:8080 ./scripts/apps-tests.sh
#   GIT_URL=https://github.com/me/site ./scripts/apps-tests.sh   # custom repo
#   SKIP_DEPLOY=1 ./scripts/apps-tests.sh                # CRUD only (no build)
#
# Requires: curl, jq.

set -euo pipefail

API="${API:-http://localhost:8080}"
KEY="${PANDASTACK_API_KEY:-}"
# A tiny static repo deploys fast and needs no toolchain.
GIT_URL="${GIT_URL:-https://github.com/pandastack/example-static-site}"
SKIP_DEPLOY="${SKIP_DEPLOY:-}"
DEPLOY_TIMEOUT="${DEPLOY_TIMEOUT:-300}"

H=(-H "content-type: application/json")
[[ -n "$KEY" ]] && H+=(-H "X-API-Key: $KEY")

c() { curl -sS --fail-with-body "${H[@]}" "$@"; }

section() { printf "\n\033[1;36m── %s ──\033[0m\n" "$*"; }
ok()      { printf "  \033[32m✓\033[0m %s\n" "$*"; }
fail()    { printf "  \033[31m✗\033[0m %s\n" "$*"; exit 1; }

section "healthz"
c "$API/healthz" >/dev/null && ok "control-plane up"

section "create app"
NAME="apptest-$(date +%s)"
APP=$(c -X POST "$API/v1/apps" -d "{\"name\":\"$NAME\",\"git_url\":\"$GIT_URL\"}")
APP_ID=$(echo "$APP" | jq -r .id)
[[ -n "$APP_ID" && "$APP_ID" != "null" ]] || fail "no app id returned"
ok "created app $APP_ID (name=$NAME)"
echo "$APP" | jq -e '.git_url != ""' >/dev/null && ok "git_url stored"
echo "$APP" | jq -e '.status != ""' >/dev/null && ok "status=$(echo "$APP" | jq -r .status)"

cleanup() {
  echo
  curl -sS "${H[@]}" -X DELETE "$API/v1/apps/$APP_ID" >/dev/null 2>&1 || true
  echo "cleaned up $APP_ID"
}
trap cleanup EXIT

section "list / get"
c "$API/v1/apps" | jq -e --arg id "$APP_ID" 'map(.id) | index($id)' >/dev/null && ok "appears in list"
c "$API/v1/apps/$APP_ID" | jq -e --arg id "$APP_ID" '.id == $id' >/dev/null && ok "GET /apps/$APP_ID"

section "update"
c -X PATCH "$API/v1/apps/$APP_ID" -d '{"port":8080}' | jq -e '.port == 8080' >/dev/null && ok "patched port=8080"

if [[ -n "$SKIP_DEPLOY" ]]; then
  section "deploy (skipped)"
  ok "SKIP_DEPLOY set — CRUD-only run"
  printf "\n\033[1;32mApps CRUD tests passed.\033[0m\n"
  exit 0
fi

section "deploy"
DEP=$(c -X POST "$API/v1/apps/$APP_ID/deploys" -d '{}')
DEP_ID=$(echo "$DEP" | jq -r .id)
[[ -n "$DEP_ID" && "$DEP_ID" != "null" ]] || fail "no deployment id"
ok "deployment queued: $DEP_ID"

c "$API/v1/apps/$APP_ID/deploys" | jq -e --arg id "$DEP_ID" 'map(.id) | index($id)' >/dev/null && ok "deployment appears in list"

section "poll deploy status (up to ${DEPLOY_TIMEOUT}s)"
STATUS=""
for ((i=0; i<DEPLOY_TIMEOUT; i+=3)); do
  STATUS=$(c "$API/v1/apps/$APP_ID/deploys/$DEP_ID" | jq -r .status)
  case "$STATUS" in
    succeeded|live) ok "deploy reached terminal status: $STATUS after ${i}s"; break ;;
    failed) fail "deploy failed — check build logs (GET /v1/apps/$APP_ID/deploys/$DEP_ID)" ;;
  esac
  sleep 3
done
[[ "$STATUS" == "succeeded" || "$STATUS" == "live" ]] || fail "deploy did not finish within ${DEPLOY_TIMEOUT}s (status=$STATUS)"

section "build logs are persisted"
c "$API/v1/apps/$APP_ID/deploys/$DEP_ID" | jq -e '.build_logs | length > 0' >/dev/null && ok "build_logs captured"

section "app has a live URL"
URL=$(c "$API/v1/apps/$APP_ID" | jq -r '.url // empty')
[[ -n "$URL" ]] && ok "stable URL: $URL" || fail "no app URL after successful deploy"

section "proxy serves the app"
for ((i=0; i<30; i+=2)); do
  if curl -sS --fail "${H[@]}" "$URL" >/dev/null 2>&1; then ok "proxy returned 2xx"; break; fi
  sleep 2
  [[ $i -ge 28 ]] && fail "proxy never served 2xx"
done

section "rollback (requires a prior live deploy)"
RB=$(c -X POST "$API/v1/apps/$APP_ID/deploys" -d '{}')
RB_ID=$(echo "$RB" | jq -r .id)
for ((i=0; i<DEPLOY_TIMEOUT; i+=3)); do
  S=$(c "$API/v1/apps/$APP_ID/deploys/$RB_ID" | jq -r .status)
  [[ "$S" == "succeeded" || "$S" == "live" ]] && break
  [[ "$S" == "failed" ]] && fail "second deploy failed"
  sleep 3
done
ROLL=$(c -X POST "$API/v1/apps/$APP_ID/rollback")
echo "$ROLL" | jq -e '.id != ""' >/dev/null && ok "rollback queued: $(echo "$ROLL" | jq -r .id)"

printf "\n\033[1;32mAll apps tests passed.\033[0m\n"
