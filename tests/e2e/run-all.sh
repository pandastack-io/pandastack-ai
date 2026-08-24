#!/usr/bin/env bash
# tests/e2e/run-all.sh — orchestrate the end-to-end tests
# against a real PandaStack deployment.
#
# This repo does not vendor the client SDKs (they are published separately), so
# the suite is the CLI lifecycle smoke test. Point PANDASTACK_API at a running
# control plane and export a token first.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

if [[ "${PANDASTACK_E2E:-0}" != "1" ]]; then
  echo "[e2e] PANDASTACK_E2E is not set to 1 — skipping. (Set PANDASTACK_E2E=1 to run.)"
  exit 0
fi

: "${PANDASTACK_API:?must set PANDASTACK_API (e.g. http://localhost:8080)}"
: "${PANDASTACK_TOKEN:?must set PANDASTACK_TOKEN (run \`pandastack auth login\` first)}"

echo "[e2e] target: $PANDASTACK_API"

# --- 0) Build CLI if missing -------------------------------------------------
if [[ ! -x bin/pandastack ]]; then
  echo "[e2e] building CLI..."
  (cd cmd/pandastack && go build -o "$ROOT/bin/pandastack" .)
fi

# --- 1) CLI smoke ------------------------------------------------------------
echo "[e2e] cli smoke..."
bash tests/cli/smoke.sh

echo "[e2e] ✅ all passed"
