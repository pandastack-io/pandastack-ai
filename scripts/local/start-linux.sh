#!/usr/bin/env bash
# start-linux.sh — one-command local dev on a Linux host with KVM.
#
# On native Linux, Firecracker can run directly on /dev/kvm — no Lima VM
# required. This script:
#   1. Sanity-checks KVM, docker, go, node.
#   2. Boots Postgres + ClickHouse + api + dashboard via docker compose.
#   3. Builds + runs the agent natively on the host (needs /dev/kvm).
#   4. Drops a default token into ./scripts/local/tokens/tokens.json.
#
# Usage:
#   ./scripts/local/start-linux.sh [up|down|status|logs|reset]
#
#   up      (default) start everything
#   down    stop services (keeps state)
#   status  show what's running
#   logs    tail agent + compose logs
#   reset   wipe state (docker volumes + agent state dir)
#
# Self-host quickstart after `up`:
#   - dashboard: http://localhost:3000
#   - api:       http://localhost:8080
#   - token:     scripts/local/tokens/tokens.json (default: pds_local_dev_token)

set -euo pipefail

CMD="${1:-up}"
GREEN='\033[0;32m'; YELLOW='\033[0;33m'; RED='\033[0;31m'; NC='\033[0m'
step() { printf "\n${GREEN}┌─ %s${NC}\n" "$*"; }
warn() { printf "${YELLOW}!${NC} %s\n" "$*"; }
die()  { printf "${RED}✗${NC} %s\n" "$*" >&2; exit 1; }

[[ "$(uname)" == "Linux" ]] || die "Linux only; on Mac use scripts/local/start-mac.sh (Lima)"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
TOKENS_DIR="$SCRIPT_DIR/tokens"
AGENT_STATE="$REPO_ROOT/.local-state/agent"
AGENT_PID="$REPO_ROOT/.local-state/agent.pid"
AGENT_LOG="$REPO_ROOT/.local-state/agent.log"
COMPOSE="docker compose -f $REPO_ROOT/docker-compose.dev.yml"

ensure_deps() {
  command -v docker >/dev/null || die "docker not found; install docker engine"
  command -v go     >/dev/null || die "go not found; install Go 1.25+"
  command -v node   >/dev/null || warn "node not found; dashboard will run in docker (slower rebuilds)"
  [[ -e /dev/kvm   ]] || die "/dev/kvm missing; KVM not enabled. Check 'kvm-ok' or BIOS virtualization."
  [[ -r /dev/kvm && -w /dev/kvm ]] || die "/dev/kvm not readable+writable by $USER. Add yourself to the 'kvm' group: sudo usermod -aG kvm $USER && newgrp kvm"
}

cmd_up() {
  ensure_deps

  step "Seeding default dev token"
  mkdir -p "$TOKENS_DIR" "$AGENT_STATE" "$(dirname "$AGENT_PID")"
  if [[ ! -f "$TOKENS_DIR/tokens.json" ]]; then
    cat > "$TOKENS_DIR/tokens.json" <<'JSON'
{
  "tokens": [
    {"token": "pds_local_dev_token", "name": "local-dev", "workspace": "local"}
  ]
}
JSON
    warn "wrote default token: pds_local_dev_token (rotate before exposing)"
  fi

  step "Booting Postgres + ClickHouse + api + dashboard (docker compose)"
  $COMPOSE up -d --build

  step "Building agent (host-side; needs /dev/kvm)"
  (cd "$REPO_ROOT/agent" && go build -o "$REPO_ROOT/.local-state/agent-bin" ./cmd/agent)

  step "Starting agent on :7070"
  if [[ -f "$AGENT_PID" ]] && kill -0 "$(cat "$AGENT_PID")" 2>/dev/null; then
    warn "agent already running (pid $(cat "$AGENT_PID")); skipping"
  else
    PANDASTACK_AGENT_STATE_DIR="$AGENT_STATE" \
      nohup "$REPO_ROOT/.local-state/agent-bin" --listen :7070 \
        >"$AGENT_LOG" 2>&1 &
    echo $! > "$AGENT_PID"
    sleep 1
    kill -0 "$(cat "$AGENT_PID")" 2>/dev/null || die "agent failed to start; tail $AGENT_LOG"
  fi

  cat <<EOF

${GREEN}✓ PandaStack is up.${NC}

  dashboard:  http://localhost:3000
  api:        http://localhost:8080
  postgres:   localhost:5432  (user/pass: pandastack-io/pandastack-ai-oss)
  clickhouse: http://localhost:8123  (user/pass: pandastack-io/pandastack-ai-oss)
  agent:      http://localhost:7070  (pid $(cat "$AGENT_PID"))
  token:      pds_local_dev_token

Try:
  export PANDASTACK_API=http://localhost:8080
  export PANDASTACK_TOKEN=pds_local_dev_token
  pip install pandastack && python -c "from pandastack import Sandbox; print(Sandbox.create('python').exec('print(1+1)').stdout)"

Stop:  $0 down
EOF
}

cmd_down() {
  step "Stopping agent"
  if [[ -f "$AGENT_PID" ]] && kill -0 "$(cat "$AGENT_PID")" 2>/dev/null; then
    kill "$(cat "$AGENT_PID")" || true
    rm -f "$AGENT_PID"
  fi
  step "Stopping compose"
  $COMPOSE down
}

cmd_status() {
  $COMPOSE ps
  echo
  if [[ -f "$AGENT_PID" ]] && kill -0 "$(cat "$AGENT_PID")" 2>/dev/null; then
    echo "agent: running (pid $(cat "$AGENT_PID"))"
  else
    echo "agent: stopped"
  fi
}

cmd_logs() {
  ( $COMPOSE logs -f --tail=50 ) &
  CPID=$!
  trap "kill $CPID 2>/dev/null || true" EXIT
  tail -n 50 -F "$AGENT_LOG" 2>/dev/null || true
}

cmd_reset() {
  cmd_down
  step "Wiping volumes + agent state"
  $COMPOSE down -v
  rm -rf "$REPO_ROOT/.local-state"
}

case "$CMD" in
  up)     cmd_up ;;
  down)   cmd_down ;;
  status) cmd_status ;;
  logs)   cmd_logs ;;
  reset)  cmd_reset ;;
  *)      die "unknown command: $CMD (use up|down|status|logs|reset)" ;;
esac
