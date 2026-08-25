#!/usr/bin/env bash
# deploy/deploy-dashboard-cf.sh
# Build dashboard/ via @cloudflare/next-on-pages and deploy to Cloudflare Pages.
#
# Required env (typically sourced from .env.local at repo root):
#   CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID
# Optional:
#   PAGES_PROJECT       (default: pandastack-dashboard)
# Also required:
#   NEXT_PUBLIC_PANDASTACK_API   your API origin, e.g. https://api.example.com
#   APP_ORIGIN                   your dashboard origin, e.g. https://app.example.com

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DASH="$REPO_ROOT/dashboard"

if [ -f "$REPO_ROOT/.env.local" ]; then
  set -a; source "$REPO_ROOT/.env.local"; set +a
fi

: "${CLOUDFLARE_API_TOKEN:?required}"
: "${CLOUDFLARE_ACCOUNT_ID:?required}"
PAGES_PROJECT="${PAGES_PROJECT:-pandastack-dashboard}"
# No defaults for these: a default would build the dashboard against someone
# else's API origin and print links to someone else's deployment.
: "${NEXT_PUBLIC_PANDASTACK_API:?set NEXT_PUBLIC_PANDASTACK_API to your API origin, e.g. https://api.example.com}"
: "${APP_ORIGIN:?set APP_ORIGIN to your dashboard origin, e.g. https://app.example.com}"
export NEXT_PUBLIC_PANDASTACK_API

GREEN='\033[0;32m'; NC='\033[0m'
step() { printf "\n${GREEN}┌─ %s${NC}\n" "$*"; }

cd "$DASH"

# Next.js loads .env.local in ALL environments and it overrides .env.production.
# dashboard/.env.local exists for local dev (points API at localhost:8080).
# Move it aside during the prod build so .env.production wins.
LOCAL_ENV_SWAPPED=0
if [ -f "$DASH/.env.local" ]; then
  mv "$DASH/.env.local" "$DASH/.env.local.devbackup"
  LOCAL_ENV_SWAPPED=1
  trap 'if [ "$LOCAL_ENV_SWAPPED" = "1" ] && [ -f "$DASH/.env.local.devbackup" ]; then mv "$DASH/.env.local.devbackup" "$DASH/.env.local"; fi' EXIT
fi

step "next-on-pages build (api=$NEXT_PUBLIC_PANDASTACK_API)"
rm -rf .vercel/output
npm ci --prefer-offline --no-audit --no-fund --legacy-peer-deps
npx -y @cloudflare/next-on-pages@latest

step "wrangler pages deploy -> project=$PAGES_PROJECT"
CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" \
CLOUDFLARE_ACCOUNT_ID="$CLOUDFLARE_ACCOUNT_ID" \
  npx -y wrangler@4 pages deploy .vercel/output/static \
    --project-name="$PAGES_PROJECT" \
    --branch=main \
    --commit-dirty=true

step "Done. Test pages:"
echo "  ${APP_ORIGIN}/sandboxes"
echo "  ${APP_ORIGIN}/databases"
