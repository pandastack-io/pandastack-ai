#!/usr/bin/env bash
# Idempotent teardown for scripts/mac-local-e2e.sh.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="$REPO_ROOT/.local-state/mac-local-e2e"
LOG_DIR="$STATE_DIR/logs"
LIMA_NAME="pandastack-lima-vm"
TOKEN="pds_local_dev_token"
COMPOSE=(docker compose -f "$REPO_ROOT/docker-compose.dev.yml")
API_PID="$STATE_DIR/api.pid"
DASHBOARD_PID="$STATE_DIR/dashboard.pid"
DB_PROXY_PID="$STATE_DIR/db-proxy.pid"
DELETE_VM=1

GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
step() { printf "\n${GREEN}┌─ %s${NC}\n" "$*"; }
warn() { printf "${YELLOW}!${NC} %s\n" "$*"; }

for arg in "$@"; do
  case "$arg" in
    --keep-vm) DELETE_VM=0 ;;
    --delete-vm) DELETE_VM=1 ;;
    *) warn "Ignoring unknown argument: $arg" ;;
  esac
done

kill_pid_file() {
  local file="$1" label="$2"
  if [[ -f "$file" ]]; then
    local pid
    pid="$(cat "$file" 2>/dev/null || true)"
    if [[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
      step "Stopping $label (pid $pid)"
      kill "$pid" 2>/dev/null || true
      for _ in {1..20}; do
        kill -0 "$pid" 2>/dev/null || break
        sleep 1
      done
      if kill -0 "$pid" 2>/dev/null; then
        warn "$label did not stop after SIGTERM; sending SIGKILL to pid $pid"
        kill -9 "$pid" 2>/dev/null || true
      fi
    fi
    rm -f "$file"
  fi
}

delete_sandboxes() {
  if command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1 && curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
    step "Deleting sandboxes through the API"
    local auth=(-H "Authorization: Bearer $TOKEN" -H "content-type: application/json")
    local ids id
    ids="$(curl -fsS "${auth[@]}" http://localhost:8080/v1/sandboxes 2>/dev/null | jq -r '.[].id' 2>/dev/null || true)"
    while IFS= read -r id; do
      [[ -n "$id" ]] || continue
      curl -fsS "${auth[@]}" -X DELETE "http://localhost:8080/v1/sandboxes/$id" >/dev/null 2>&1 || true
    done <<< "$ids"
  fi
}

cleanup_lima_guest() {
  if command -v limactl >/dev/null 2>&1 && limactl list --format '{{.Name}}' 2>/dev/null | grep -qx "$LIMA_NAME"; then
    if [[ "$(limactl list --format '{{.Status}}' "$LIMA_NAME" 2>/dev/null || echo Stopped)" == "Running" ]]; then
      step "Stopping agent and leftover Firecracker processes in Lima"
      limactl shell --workdir /workspace "$LIMA_NAME" -- sudo systemctl stop pandastack-agent-local-e2e.service >/dev/null 2>&1 || true
      limactl shell --workdir /workspace "$LIMA_NAME" -- sudo bash -lc '
        set +e
        while read -r pid comm _; do
          case "$pid" in ""|*[!0-9]*) continue;; esac
          [ "$comm" = "firecracker" ] && kill "$pid" 2>/dev/null || true
        done < <(ps -eo pid=,comm=)
        ip -o link show | while IFS=":" read -r _ link _; do
          link="${link# }"
          link="${link%%@*}"
          case "$link" in fc*) ip link del "$link" 2>/dev/null || true;; esac
        done
      ' >/dev/null 2>&1 || true
    fi
    if [[ "$DELETE_VM" -eq 1 ]]; then
      step "Deleting Lima VM ($LIMA_NAME)"
      limactl stop "$LIMA_NAME" >/dev/null 2>&1 || true
      limactl delete --force "$LIMA_NAME" >/dev/null 2>&1 || true
    else
      step "Stopping Lima VM ($LIMA_NAME)"
      limactl stop "$LIMA_NAME" >/dev/null 2>&1 || true
    fi
  fi
}

main() {
  delete_sandboxes
  kill_pid_file "$DB_PROXY_PID" "db-proxy"
  kill_pid_file "$DASHBOARD_PID" "dashboard"
  kill_pid_file "$API_PID" "API"
  cleanup_lima_guest

  if command -v docker >/dev/null 2>&1; then
    step "Stopping Postgres and ClickHouse"
    (cd "$REPO_ROOT" && "${COMPOSE[@]}" down -v --remove-orphans) >/dev/null 2>&1 || true
  fi

  rm -f "$STATE_DIR/last-sandbox-id"

  printf "\n${GREEN}╰─ PandaStack local E2E teardown complete${NC}\n"
  cat <<EOF
  Logs/state kept under: $STATE_DIR
  Homebrew packages installed by bootstrap are intentionally left installed.
  To remove local logs/state: rm -rf .local-state/mac-local-e2e
EOF
}

main "$@"
