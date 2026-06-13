#!/usr/bin/env bash
# Create the Anthropic side of the integration: a self_hosted environment and a
# demo agent. Prints the ids to export. The environment KEY must still be
# generated in the Console (it is Console-only) — this script cannot do that.
#
# Usage:
#   export ANTHROPIC_API_KEY=sk-ant-api03-...
#   ./setup_anthropic.sh
set -euo pipefail

: "${ANTHROPIC_API_KEY:?set ANTHROPIC_API_KEY (your org key) first}"
API="${ANTHROPIC_BASE_URL:-https://api.anthropic.com}"
H=(-H "x-api-key: $ANTHROPIC_API_KEY"
   -H "anthropic-version: 2023-06-01"
   -H "anthropic-beta: managed-agents-2026-04-01"
   -H "content-type: application/json")

echo "Creating self_hosted environment…"
ENV_JSON=$(curl -fsSL "$API/v1/environments" "${H[@]}" \
  -d '{"name":"pandastack-self-hosted","config":{"type":"self_hosted"}}')
ENV_ID=$(printf '%s' "$ENV_JSON" | jq -r '.id')
echo "  environment: $ENV_ID"

echo "Creating demo agent…"
AGENT_JSON=$(curl -fsSL "$API/v1/agents" "${H[@]}" -d @- <<'JSON'
{
  "name": "pandastack-demo-agent",
  "description": "Demo agent for the PandaStack self-hosted sandbox integration",
  "model": {"id": "claude-sonnet-4-6"},
  "system": "You are a general-purpose agent running in a PandaStack Firecracker microVM. You can write code, run commands, and read/write files in /workspace. Write any final deliverables to /mnt/session/outputs. When you run a shell command that would produce no output (for example cd, a redirect, or a file write), append a confirmation such as `&& echo done` so the command always prints at least one line.",
  "tools": [{"type": "agent_toolset_20260401", "default_config": {"enabled": true, "permission_policy": {"type": "always_allow"}}}]
}
JSON
)
AGENT_ID=$(printf '%s' "$AGENT_JSON" | jq -r '.id')
echo "  agent: $AGENT_ID"

cat <<EOF

Done. Next:
  1. Generate the environment KEY in the Console (Console-only):
       Workspace > Environments > pandastack-self-hosted > Generate environment key
  2. Export for the orchestrator + demo:
       export ANTHROPIC_ENVIRONMENT_ID=$ENV_ID
       export ANTHROPIC_ENVIRONMENT_KEY=sk-ant-oat01-...   # from step 1
       export CMA_AGENT_ID=$AGENT_ID
EOF
