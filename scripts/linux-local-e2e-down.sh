#!/usr/bin/env bash
# Tear down PandaStack local E2E on Linux: stop API/dashboard/agent and
# Postgres+ClickHouse containers. Local state on disk is preserved.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="$REPO_ROOT/.local-state/linux-local-e2e"
COMPOSE=(docker compose -f "$REPO_ROOT/docker-compose.dev.yml")

YELLOW='\033[0;33m'; GREEN='\033[0;32m'; NC='\033[0m'
info() { printf "${GREEN}┌─ %s${NC}\n" "$*"; }
warn() { printf "${YELLOW}!${NC} %s\n" "$*"; }

stop_pid_file() {
  local pid_file="$1" label="$2"
  if [[ -f "$pid_file" ]]; then
    local pid
    pid=$(cat "$pid_file")
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      info "Stopping $label (pid $pid)"
      kill "$pid" 2>/dev/null || true
      sleep 1
      kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$pid_file"
  fi
}

stop_pid_file "$STATE_DIR/dashboard.pid" "dashboard"
stop_pid_file "$STATE_DIR/api.pid" "API"

if systemctl list-unit-files | grep -q pandastack-agent-local-e2e.service; then
  info "Stopping pandastack-agent-local-e2e systemd unit"
  sudo systemctl stop pandastack-agent-local-e2e.service || true
  sudo systemctl disable pandastack-agent-local-e2e.service || true
fi

info "Stopping Postgres and ClickHouse containers"
(cd "$REPO_ROOT" && "${COMPOSE[@]}" down) || warn "docker compose down failed (containers may already be stopped)"

cat <<EOF

${GREEN}✓ Local E2E stack stopped.${NC}

State preserved at:
  $STATE_DIR
  $REPO_ROOT/data
  /var/lib/pandastack (Firecracker templates and kernels)

To wipe state and reinstall from scratch:
  sudo rm -rf $STATE_DIR $REPO_ROOT/data /var/lib/pandastack
  sudo rm -f /etc/systemd/system/pandastack-agent-local-e2e.service /etc/pandastack/local-e2e.env
  sudo systemctl daemon-reload
  bash scripts/linux-local-e2e.sh
EOF
