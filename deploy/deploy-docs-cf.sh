#!/usr/bin/env bash
# deploy/deploy-docs-cf.sh
# Build docs-site/ as a Next.js static export and deploy to Cloudflare Pages.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DOCS="$REPO_ROOT/docs-site"

: "${CLOUDFLARE_API_TOKEN:?required}"
: "${CLOUDFLARE_ACCOUNT_ID:?required}"
: "${CLOUDFLARE_ZONE_ID:?required}"
: "${CLOUDFLARE_DOMAIN:?required}"
PAGES_PROJECT="${PAGES_PROJECT:-pandastack-docs}"
DOCS_FQDN="${DOCS_FQDN:-docs.$CLOUDFLARE_DOMAIN}"

GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
step() { printf "\n${GREEN}┌─ %s${NC}\n" "$*"; }
warn() { printf "${YELLOW}!${NC} %s\n" "$*"; }

cd "$DOCS"

step "Installing deps"
npm install --silent --no-audit --no-fund --legacy-peer-deps

step "Building static export"
rm -rf .next out .source
npx --yes fumadocs-mdx
# IS_WEBPACK_TEST=1 forces Next.js 16 to use webpack instead of Turbopack,
# which is needed because fumadocs-mdx webpack loaders are ESM and Turbopack's
# loader runner uses require() which cannot load ESM modules.
DOCS_TARGET=static IS_WEBPACK_TEST=1 npx next build

cat > out/_headers <<'HDR'
/_next/static/*
  Cache-Control: public, max-age=31536000, immutable
HDR

step "Ensuring CF Pages project: $PAGES_PROJECT"
proj_resp=$(curl -s -X POST \
  "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/pages/projects" \
  -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"$PAGES_PROJECT\",\"production_branch\":\"main\"}")
if echo "$proj_resp" | grep -q '"success":true'; then printf "  + project created\n"
elif echo "$proj_resp" | grep -qiE "already.*exist|duplicate"; then printf "  = project exists\n"
else warn "project create: $proj_resp"
fi

step "Deploying to Cloudflare Pages project: $PAGES_PROJECT"
CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" \
CLOUDFLARE_ACCOUNT_ID="$CLOUDFLARE_ACCOUNT_ID" \
  npx --yes wrangler@3 pages deploy out \
    --project-name="$PAGES_PROJECT" \
    --branch=main \
    --commit-dirty=true

step "Attaching custom domain: $DOCS_FQDN"
body=$(printf '{"name":"%s"}' "$DOCS_FQDN")
resp=$(curl -s -X POST \
  "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/pages/projects/$PAGES_PROJECT/domains" \
  -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
  -H "Content-Type: application/json" -d "$body")
if echo "$resp" | grep -q '"success":true'; then printf "  + attached\n"
elif echo "$resp" | grep -qiE "already.*exist|already added|duplicate"; then printf "  = already attached\n"
else warn "domain attach: $resp"
fi

step "Ensuring DNS CNAME $DOCS_FQDN → $PAGES_PROJECT.pages.dev"
target="$PAGES_PROJECT.pages.dev"
existing=$(curl -s "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records?name=$DOCS_FQDN" \
  -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
  | python3 -c "import json,sys; r=json.load(sys.stdin).get('result',[]); print(r[0]['id']+'|'+r[0]['type']+'|'+r[0].get('content','') if r else '')")
payload=$(printf '{"type":"CNAME","name":"%s","content":"%s","ttl":1,"proxied":true}' "$DOCS_FQDN" "$target")
if [ -z "$existing" ]; then
  curl -s -X POST "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records" \
    -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "Content-Type: application/json" \
    -d "$payload" | python3 -c "import json,sys; d=json.load(sys.stdin); print('  +' if d.get('success') else '  !', d.get('result',{}).get('name') or d.get('errors'))" || true
else
  id="${existing%%|*}"; rest="${existing#*|}"; type="${rest%%|*}"; content="${rest##*|}"
  if [ "$type" = "CNAME" ] && [ "$content" = "$target" ]; then
    echo "  = already → $target"
  else
    curl -s -X PUT "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records/$id" \
      -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "Content-Type: application/json" \
      -d "$payload" | python3 -c "import json,sys; d=json.load(sys.stdin); print('  ~' if d.get('success') else '  !', d.get('result',{}).get('name') or d.get('errors'))" || true
  fi
fi

step "Purging Cloudflare cache"
curl -s -X POST "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/purge_cache" \
  -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "Content-Type: application/json" \
  -d '{"purge_everything":true}' \
  | python3 -c "import json,sys; d=json.load(sys.stdin); print('  purge:', 'OK' if d.get('success') else d.get('errors'))" || true

printf "\n${GREEN}╰─ docs live at https://%s${NC}\n" "$DOCS_FQDN"
