#!/usr/bin/env bash
# tests/e2e/run-all.sh — orchestrate Python + TS + CLI e2e tests
# against a real PandaStack deployment.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

if [[ "${PANDASTACK_E2E:-0}" != "1" ]]; then
  echo "[e2e] PANDASTACK_E2E is not set to 1 — skipping. (Set PANDASTACK_E2E=1 to run.)"
  exit 0
fi

: "${PANDASTACK_API:?must set PANDASTACK_API (e.g. https://api.pandastack.ai)}"
: "${PANDASTACK_TOKEN:?must set PANDASTACK_TOKEN (run \`pandastack auth login\` first)}"

echo "[e2e] target: $PANDASTACK_API"

# --- 0) Build CLI if missing -------------------------------------------------
if [[ ! -x bin/pandastack ]]; then
  echo "[e2e] building CLI..."
  (cd cmd/pandastack && go build -o "$ROOT/bin/pandastack" .)
fi

# --- 1) Python SDK e2e -------------------------------------------------------
if command -v python3 >/dev/null 2>&1 && [[ -d sdks/python ]]; then
  echo "[e2e] python sdk..."
  (
    cd sdks/python
    if [[ ! -d .venv ]]; then
      python3 -m venv .venv
      .venv/bin/pip install -q --upgrade pip
      .venv/bin/pip install -q -e ".[dev]" || .venv/bin/pip install -q -e .
    fi
    PANDASTACK_E2E=1 .venv/bin/pytest -q tests/e2e || exit 1
  )
fi

# --- 2) TypeScript SDK e2e ---------------------------------------------------
if command -v node >/dev/null 2>&1 && [[ -d sdks/typescript ]]; then
  echo "[e2e] typescript sdk..."
  (
    cd sdks/typescript
    [[ -d node_modules ]] || npm install --silent
    PANDASTACK_E2E=1 npm test --silent || exit 1
  )
fi

# --- 3) CLI smoke ------------------------------------------------------------
echo "[e2e] cli smoke..."
bash tests/cli/smoke.sh

echo "[e2e] ✅ all passed"
