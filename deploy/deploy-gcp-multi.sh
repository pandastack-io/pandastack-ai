#!/usr/bin/env bash
# deploy-gcp-multi.sh — enterprise multi-node deploy (api.pandastack.ai).
#
# Topology (built fresh; existing pandastack-host remains as blue):
#   - Private VPC + dual subnets + Cloud NAT + Secret Manager
#   - 2 edge VMs (e2-small) behind global HTTPS LB + managed cert + Cloud Armor
#   - 2 agent VMs (n2d-standard-2, nested-virt, no public IP) in MIG
#
# Workflow:
#   ./deploy-gcp-multi.sh up        # build/update stack + push binaries
#   ./deploy-gcp-multi.sh status    # show LB IP + agent heartbeat
#   ./deploy-gcp-multi.sh smoke     # exercise api.pandastack.ai end-to-end
#   ./deploy-gcp-multi.sh cutover   # ensure api.pandastack.ai DNS → LB
#   ./deploy-gcp-multi.sh scale N   # scale agent MIG target_size
#   ./deploy-gcp-multi.sh down      # tear down the stack
#
# Requires .env.local with DATABASE_URL, SUPABASE_*, CLOUDFLARE_* set.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TF="$REPO/infra/terraform/envs/dev-gcp-multi"
ENV_FILE="$REPO/.env.local"
TFVARS_FILE="$TF/terraform.tfvars"

GREEN='\033[0;32m'; YELLOW='\033[0;33m'; RED='\033[0;31m'; NC='\033[0m'
step() { printf "\n${GREEN}┌─ %s${NC}\n" "$*"; }
warn() { printf "${YELLOW}!${NC} %s\n" "$*"; }
die()  { printf "${RED}✗${NC} %s\n" "$*" >&2; exit 1; }

[ -f "$ENV_FILE" ] || die "missing $ENV_FILE"
set -a; . "$ENV_FILE"; set +a

require() { command -v "$1" >/dev/null || die "$1 not installed"; }
require terraform
require gcloud
require jq
require curl

: "${GCS_BUILD_BUCKET:?set GCS_BUILD_BUCKET to your build-artifacts bucket}"

write_tfvars() {
  : "${DATABASE_URL:?need DATABASE_URL in .env.local}"
  : "${CLOUDFLARE_API_TOKEN:?need CLOUDFLARE_API_TOKEN}"
  : "${CLOUDFLARE_ZONE_ID:?need CLOUDFLARE_ZONE_ID}"
  SSH_PUB="$(cat "${SSH_PUBKEY_PATH:-$HOME/.ssh/id_ed25519.pub}")"
  SSH_CIDR="${SSH_ALLOWED_CIDR:-$(curl -fsS https://api.ipify.org)/32}"
  cat > "$TFVARS_FILE" <<EOF
gcp_project          = "${GCP_PROJECT:?set GCP_PROJECT to your GCP project id}"
gcp_region           = "${GCP_REGION:-us-central1}"
gcp_zone             = "${GCP_ZONE:-us-central1-a}"
use_preemptible      = ${USE_PREEMPTIBLE:-true}

ssh_pubkey           = <<KEY
${SSH_PUB}
KEY
ssh_allowed_cidr     = "${SSH_CIDR}"

cloudflare_api_token = "${CLOUDFLARE_API_TOKEN}"
cloudflare_zone_id   = "${CLOUDFLARE_ZONE_ID}"
cloudflare_zone_name = "${CLOUDFLARE_DOMAIN:-pandastack.ai}"

database_url         = "${DATABASE_URL}"
clickhouse_url       = "${CLICKHOUSE_URL:-}"
supabase_jwks_url    = "${SUPABASE_JWKS_URL:-}"
supabase_anon_key    = "${SUPABASE_ANON_KEY:-}"
supabase_url         = "${SUPABASE_URL:-}"

agent_machine_type   = "${AGENT_MACHINE_TYPE:-n2-standard-8}"
agent_count          = ${AGENT_COUNT:-1}
agent_max_count      = ${AGENT_MAX_COUNT:-8}
edge_count           = ${EDGE_COUNT:-2}

agent_binary_url     = "${AGENT_BINARY_URL:-}"
edge_binary_url      = "${EDGE_BINARY_URL:-}"
dashboard_bucket     = "${DASHBOARD_BUCKET:-${GCS_BUILD_BUCKET}}"
EOF
  chmod 0600 "$TFVARS_FILE"
}

build_and_publish_binaries() {
  step "Cross-build agent + api + pandastack-init + pandastack-daemon for linux/amd64"
  local out="$REPO/dist/multinode"
  rm -rf "$out"; mkdir -p "$out/bin" "$out/dashboard"
  (cd "$REPO/agent" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$out/bin/pandastack-agent" ./cmd/agent)
  (cd "$REPO/agent" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$out/bin/pandastack-init"  ./cmd/pandastack-init)
  # Phase-2 vsock transport: in-guest daemon baked into the base rootfs.
  (cd "$REPO/agent" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$out/bin/pandastack-daemon" ./cmd/pandastack-daemon)
  (cd "$REPO/api"   && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$out/bin/pandastack-api"   ./cmd/api)

  step "Bundle dashboard"
  if [ -d "$REPO/dashboard/dist" ]; then
    cp -r "$REPO/dashboard/dist/"* "$out/dashboard/"
  elif [ -d "$REPO/dashboard/.next/standalone" ]; then
    cp -r "$REPO/dashboard/.next/standalone/"* "$out/dashboard/"
    cp -r "$REPO/dashboard/.next/static" "$out/dashboard/.next/static" 2>/dev/null || true
  else
    warn "no dashboard build found; edge will serve api-only (dashboard lives on Cloudflare Pages)"
  fi

  step "Publish to gs://${GCS_BUILD_BUCKET}/"
  gcloud storage buckets describe "gs://${GCS_BUILD_BUCKET}" >/dev/null 2>&1 || \
    gcloud storage buckets create "gs://${GCS_BUILD_BUCKET}" --location="${GCP_REGION:-us-central1}"
  gcloud storage cp "$out/bin/pandastack-agent" "gs://${GCS_BUILD_BUCKET}/bin/pandastack-agent"
  gcloud storage cp "$out/bin/pandastack-init"  "gs://${GCS_BUILD_BUCKET}/bin/pandastack-init"
  gcloud storage cp "$out/bin/pandastack-daemon" "gs://${GCS_BUILD_BUCKET}/bin/pandastack-daemon"
  gcloud storage cp "$out/bin/pandastack-api"   "gs://${GCS_BUILD_BUCKET}/bin/pandastack-api"
  if [ -d "$out/dashboard" ] && [ "$(ls -A "$out/dashboard" 2>/dev/null)" ]; then
    gcloud storage rsync --recursive "$out/dashboard/" "gs://${GCS_BUILD_BUCKET}/dashboard/"
  fi
  step "Pack edge bundle"
  tar -czf "$out/edge-bundle.tgz" -C "$out" bin dashboard
  gcloud storage cp "$out/edge-bundle.tgz" "gs://${GCS_BUILD_BUCKET}/bundles/edge-latest.tgz"
  tar -czf "$out/agent-bundle.tgz" -C "$out" bin
  gcloud storage cp "$out/agent-bundle.tgz" "gs://${GCS_BUILD_BUCKET}/bundles/agent-latest.tgz"
}

cmd_up() {
  build_and_publish_binaries
  AGENT_BINARY_URL="https://storage.googleapis.com/${GCS_BUILD_BUCKET}/bin/pandastack-agent"
  EDGE_BINARY_URL="https://storage.googleapis.com/${GCS_BUILD_BUCKET}/bundles/edge-latest.tgz"
  DASHBOARD_BUCKET="${GCS_BUILD_BUCKET}"
  export AGENT_BINARY_URL EDGE_BINARY_URL DASHBOARD_BUCKET
  write_tfvars

  step "terraform init"
  terraform -chdir="$TF" init -upgrade
  step "terraform apply (multi-node infra)"
  terraform -chdir="$TF" apply -auto-approve

  LB_IP=$(terraform -chdir="$TF" output -raw lb_ip)
  step "Infra up. LB IP: $LB_IP"

  step "Wait for edge healthcheck to be HEALTHY (up to 15min)"
  _wait_edge_healthy "$LB_IP" 900 || warn "edge not yet HEALTHY — continuing; first cloud-init can take ~5min"

  step "Bake templates if missing in gs://${GCS_BUILD_BUCKET}/templates/"
  if _gcs_templates_present; then
    echo " ✓ templates already present in GCS — skipping bake (use FORCE=1 cmd bake-templates to rebuild)"
  else
    cmd_bake_templates
  fi

  step "Deploy frontend dashboard to Cloudflare Pages"
  cmd_deploy_frontend || warn "frontend deploy failed; run './deploy-gcp-multi.sh deploy-frontend' to retry"

  step "Upsert Cloudflare DNS: api.pandastack.ai → $LB_IP (proxied)"
  CF_PROXIED=true _cf_upsert_a api "$LB_IP"

  step "Smoke tests"
  cmd_smoke || warn "smoke tests had failures — investigate before declaring victory"

  step "✅ One-click deploy done."
  echo "  frontend: https://app.${CLOUDFLARE_DOMAIN:-pandastack.ai}"
  echo "  api:      https://api.${CLOUDFLARE_DOMAIN:-pandastack.ai}"
}

_wait_edge_healthy() {
  local lb_ip="$1" timeout_s="$2"
  local deadline=$(( $(date +%s) + timeout_s ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if curl -fsS --max-time 5 "http://${lb_ip}/healthz" >/dev/null 2>&1; then
      echo " ✓ edge HEALTHY (via LB)"
      return 0
    fi
    sleep 15
    printf "."
  done
  echo
  return 1
}

_gcs_templates_present() {
  local bucket
  bucket="$(terraform -chdir="$TF" output -raw gcs_bucket 2>/dev/null || echo "$GCS_BUILD_BUCKET")"
  bucket="${TEMPLATES_BUCKET:-$bucket}"
  gcloud storage ls "gs://${bucket}/templates/ubuntu-24.04-net/rootfs.ext4" >/dev/null 2>&1 || return 1
  gcloud storage ls "gs://${bucket}/templates/code-interpreter/rootfs.ext4" >/dev/null 2>&1
}

_cf_upsert_a() {
  local name="$1" ip="$2" proxied="${CF_PROXIED:-true}"
  local existing
  existing=$(curl -fsS -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records?name=${name}.${CLOUDFLARE_DOMAIN:-pandastack.ai}&type=A" \
    | jq -r '.result[0].id // ""')
  if [ -n "$existing" ]; then
    curl -fsS -X PATCH -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "content-type: application/json" \
      "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records/$existing" \
      -d "{\"content\":\"$ip\",\"proxied\":$proxied,\"ttl\":60}" >/dev/null
    echo " ✓ patched ${name} → $ip (proxied=$proxied)"
  else
    curl -fsS -X POST -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "content-type: application/json" \
      "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records" \
      -d "{\"type\":\"A\",\"name\":\"$name\",\"content\":\"$ip\",\"proxied\":$proxied,\"ttl\":60}" >/dev/null
    echo " ✓ created ${name} → $ip (proxied=$proxied)"
  fi
}

_cf_upsert_cname() {
  local name="$1" target="$2" proxied="${CF_PROXIED:-true}"
  local existing
  existing=$(curl -fsS -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records?name=${name}.${CLOUDFLARE_DOMAIN:-pandastack.ai}&type=CNAME" \
    | jq -r '.result[0].id // ""')
  if [ -n "$existing" ]; then
    curl -fsS -X PATCH -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "content-type: application/json" \
      "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records/$existing" \
      -d "{\"content\":\"$target\",\"proxied\":$proxied,\"ttl\":60}" >/dev/null
    echo " ✓ patched ${name} CNAME → $target (proxied=$proxied)"
  else
    curl -fsS -X POST -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "content-type: application/json" \
      "https://api.cloudflare.com/client/v4/zones/$CLOUDFLARE_ZONE_ID/dns_records" \
      -d "{\"type\":\"CNAME\",\"name\":\"$name\",\"content\":\"$target\",\"proxied\":$proxied,\"ttl\":60}" >/dev/null
    echo " ✓ created ${name} CNAME → $target (proxied=$proxied)"
  fi
}

# ----------------------------------------------------------------------
# bake-templates: build base ubuntu-24.04-net rootfs + 11 first-party
# templates on a chosen agent VM, then push to GCS so future cloud-inits
# can sync them down quickly. Idempotent: skips templates already in GCS.
# ----------------------------------------------------------------------
cmd_bake_templates() {
  # The bucket the agent's cloud-init reads from is the per-env GCS bucket
  # (terraform output `gcs_bucket`). Agent SA has write access there.
  local bucket="${TEMPLATES_BUCKET:-$(terraform -chdir="$TF" output -raw gcs_bucket 2>/dev/null || echo "$GCS_BUILD_BUCKET")}"
  local region="${GCP_REGION:-us-central1}"
  local builder zone

  step "Picking builder agent VM"
  # Allow pinning the builder via BUILDER=<vm-name> (e.g. to avoid a VM already
  # saturated by an in-flight bake, whose sshd becomes unreachable under load).
  if [ -n "${BUILDER:-}" ]; then
    builder=$(gcloud compute instances list --filter="name=${BUILDER}" \
      --format="value(name,zone)" | head -1)
    [ -n "$builder" ] || die "BUILDER=${BUILDER} not found"
  else
    builder=$(gcloud compute instances list --filter="name~^pandastack-agent OR name~^pandastack-agent" \
      --format="value(name,zone)" | head -1)
  fi
  [ -n "$builder" ] || die "no agent VM found; run terraform apply first"
  zone=$(echo "$builder" | awk '{print $2}')
  builder=$(echo "$builder" | awk '{print $1}')
  echo " ✓ builder: $builder ($zone)"

  step "Upload bake scripts + pandastack-init + pandastack-daemon to builder"
  # bake-templates.sh installs to /usr/local/bin, so its repo_root derivation
  # ($(dirname BASH_SOURCE)/..) resolves to /usr/local. The postgres-16 bake
  # copies broker source + configs from ${repo_root}/templates/postgres-16, so
  # we must stage that tree at /usr/local/templates/postgres-16 on the builder
  # (otherwise postgres-16 fails: "cp: cannot stat '/tmp/pds-broker-src/main.go'").
  local pg16_tar="$REPO/dist/multinode/pg16-tree.tgz"
  mkdir -p "$(dirname "$pg16_tar")"
  tar czf "$pg16_tar" -C "$REPO/templates" postgres-16
  gcloud compute scp --zone="$zone" --tunnel-through-iap --quiet \
    "$REPO/scripts/build-base-rootfs.sh" \
    "$REPO/scripts/bake-templates.sh" \
    "$REPO/dist/multinode/bin/pandastack-init" \
    "$REPO/dist/multinode/bin/pandastack-daemon" \
    "$pg16_tar" \
    "$builder:/tmp/" || die "scp failed"

  step "Run base + bake in background on builder (survives SSH drops)"
  local bake_args="${BAKE_TEMPLATES:-}"
  # Resolve FORCE locally ONCE and substitute the literal value into the remote
  # command. The previous version deferred \${FORCE:-0} to runtime on the
  # builder, where FORCE is unset -> it always evaluated to 0, so the base
  # rootfs rebuild was silently SKIPPED (it already exists), and templates were
  # cloned from the STALE base. Substituting the literal here guarantees the
  # base is rebuilt (with the daemon) whenever the operator passes FORCE=1.
  local force_val="${FORCE:-0}"
  gcloud compute ssh "$builder" --zone="$zone" --tunnel-through-iap --quiet --command="
    set -e
    sudo install -m0755 /tmp/pandastack-init /usr/local/bin/pandastack-init
    sudo install -m0755 /tmp/pandastack-daemon /usr/local/bin/pandastack-daemon
    sudo install -m0755 /tmp/build-base-rootfs.sh /usr/local/bin/pandastack-build-base-rootfs.sh
    sudo install -m0755 /tmp/bake-templates.sh /usr/local/bin/pandastack-bake-templates.sh
    # Stage the postgres-16 template tree where bake-templates.sh expects it
    # (repo_root=/usr/local since the script lives in /usr/local/bin).
    sudo mkdir -p /usr/local/templates
    sudo tar xzf /tmp/pg16-tree.tgz -C /usr/local/templates
    sudo rm -f /var/log/pandastack-bake.log /var/log/pandastack-bake.done
    sudo bash -c 'nohup setsid bash -c \"
      set -e
      if [ ! -f /var/lib/pandastack/templates/ubuntu-24.04-net/rootfs.ext4 ] || [ ${force_val} = 1 ]; then
        FORCE=${force_val} bash /usr/local/bin/pandastack-build-base-rootfs.sh
      fi
      FORCE=${force_val} bash /usr/local/bin/pandastack-bake-templates.sh $bake_args
      echo DONE > /var/log/pandastack-bake.done
    \" >>/var/log/pandastack-bake.log 2>&1 </dev/null &'
    sleep 2
    echo 'bake started; PID:'; pgrep -af pandastack-bake-templates || true
  " || die "failed to launch bake"

  step "Poll bake status (this takes 10–60 min)"
  local poll_deadline=$(( $(date +%s) + 5400 ))
  while [ "$(date +%s)" -lt "$poll_deadline" ]; do
    if gcloud compute ssh "$builder" --zone="$zone" --tunnel-through-iap --quiet \
         --command='test -f /var/log/pandastack-bake.done && echo DONE' 2>/dev/null | grep -q DONE; then
      echo " ✓ bake done"
      break
    fi
    gcloud compute ssh "$builder" --zone="$zone" --tunnel-through-iap --quiet \
      --command='sudo tail -2 /var/log/pandastack-bake.log 2>/dev/null' 2>/dev/null | tail -2 || true
    sleep 60
  done
  gcloud compute ssh "$builder" --zone="$zone" --tunnel-through-iap --quiet \
    --command='test -f /var/log/pandastack-bake.done' || die "bake did not finish in time; tail /var/log/pandastack-bake.log on $builder"

  step "Push baked templates + kernels to gs://${bucket}/ (background)"
  gcloud compute ssh "$builder" --zone="$zone" --tunnel-through-iap --quiet --command="
    set -e
    sudo rm -f /var/log/pandastack-push.log /var/log/pandastack-push.done
    sudo bash -c 'nohup setsid bash -c \"
      set -e
      gcloud storage rsync --recursive /var/lib/pandastack/templates/ gs://${bucket}/templates/
      gcloud storage rsync --recursive /var/lib/pandastack/kernels/   gs://${bucket}/kernels/
      echo DONE > /var/log/pandastack-push.done
    \" >>/var/log/pandastack-push.log 2>&1 </dev/null &'
  " || die "failed to launch push"

  step "Poll GCS push (uploading ~15GB)"
  local push_deadline=$(( $(date +%s) + 3600 ))
  while [ "$(date +%s)" -lt "$push_deadline" ]; do
    if gcloud compute ssh "$builder" --zone="$zone" --tunnel-through-iap --quiet \
         --command='test -f /var/log/pandastack-push.done && echo DONE' 2>/dev/null | grep -q DONE; then
      echo " ✓ GCS push done"
      break
    fi
    sleep 60
    printf "."
  done
  echo
  gcloud compute ssh "$builder" --zone="$zone" --tunnel-through-iap --quiet \
    --command='test -f /var/log/pandastack-push.done' || die "GCS push did not finish in time"

  echo " ✓ templates synced to gs://${bucket}/templates/"
}

# ----------------------------------------------------------------------
# deploy-frontend: build dashboard with @cloudflare/next-on-pages and
# deploy to Cloudflare Pages project 'pandastack-dashboard'. Idempotent.
# ----------------------------------------------------------------------
cmd_deploy_frontend() {
  : "${CLOUDFLARE_API_TOKEN:?need CLOUDFLARE_API_TOKEN}"
  : "${CLOUDFLARE_ACCOUNT_ID:?need CLOUDFLARE_ACCOUNT_ID in .env.local}"
  local project="${CF_PAGES_PROJECT:-pandastack-dashboard}"
  local api_host="api.${CLOUDFLARE_DOMAIN:-pandastack.ai}"
  local app_host="app.${CLOUDFLARE_DOMAIN:-pandastack.ai}"

  step "Build dashboard for Cloudflare Pages"
  (cd "$REPO/dashboard" && cat > .env.production <<EOF
NEXT_PUBLIC_PANDASTACK_API=https://${api_host}
NEXT_PUBLIC_API_BASE_URL=https://${api_host}
NEXT_PUBLIC_PANDASTACK_ENV=cloud
NEXT_PUBLIC_SUPABASE_URL=${SUPABASE_URL:-}
NEXT_PUBLIC_SUPABASE_ANON_KEY=${SUPABASE_ANON_KEY:-}
EOF
   npm install --no-audit --no-fund --silent
   npx @cloudflare/next-on-pages )

  step "Ensure CF Pages project '${project}' exists"
  curl -fsS -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/accounts/${CLOUDFLARE_ACCOUNT_ID}/pages/projects/${project}" \
    | jq -e '.success' >/dev/null || \
  curl -fsS -X POST -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "content-type: application/json" \
    "https://api.cloudflare.com/client/v4/accounts/${CLOUDFLARE_ACCOUNT_ID}/pages/projects" \
    -d "$(jq -nc --arg name "$project" --arg br main '{name:$name,production_branch:$br}')" >/dev/null

  step "Set production env vars"
  local env_payload
  env_payload=$(jq -nc \
    --arg api "https://${api_host}" \
    --arg sb "${SUPABASE_URL:-}" \
    --arg sak "${SUPABASE_ANON_KEY:-}" \
    '{deployment_configs:{production:{env_vars:{
      NEXT_PUBLIC_PANDASTACK_API:{value:$api},
      NEXT_PUBLIC_API_BASE_URL:{value:$api},
      NEXT_PUBLIC_PANDASTACK_ENV:{value:"cloud"},
      NEXT_PUBLIC_SUPABASE_URL:{value:$sb},
      NEXT_PUBLIC_SUPABASE_ANON_KEY:{value:$sak}
    },compatibility_flags:["nodejs_compat"]}}}')
  curl -fsS -X PATCH -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "content-type: application/json" \
    "https://api.cloudflare.com/client/v4/accounts/${CLOUDFLARE_ACCOUNT_ID}/pages/projects/${project}" \
    -d "$env_payload" >/dev/null

  step "Deploy via wrangler"
  (cd "$REPO/dashboard" && CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" CLOUDFLARE_ACCOUNT_ID="$CLOUDFLARE_ACCOUNT_ID" \
    npx wrangler pages deploy .vercel/output/static --project-name "$project" --branch main --commit-dirty=true)

  step "Attach custom domain ${app_host}"
  curl -fsS -X POST -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "content-type: application/json" \
    "https://api.cloudflare.com/client/v4/accounts/${CLOUDFLARE_ACCOUNT_ID}/pages/projects/${project}/domains" \
    -d "{\"name\":\"${app_host}\"}" >/dev/null 2>&1 || true
  CF_PROXIED=true _cf_upsert_cname app "${project}.pages.dev"

  echo " ✓ dashboard live at https://${app_host}"
}

cmd_status() {
  step "Cert status"
  CERT_NAME=$(terraform -chdir="$TF" output -raw managed_cert_name 2>/dev/null || true)
  if [ -n "$CERT_NAME" ]; then
    gcloud compute ssl-certificates describe "$CERT_NAME" --global --format=json \
      | jq -r '.managed | {status:.status, domain_status:.domainStatus}'
  fi
  step "LB IP"
  terraform -chdir="$TF" output -raw lb_ip; echo
  step "Agent MIG"
  AGENT_MIG=$(terraform -chdir="$TF" output -raw agent_mig)
  gcloud compute instance-groups managed list-instances "$AGENT_MIG" \
    --region "${GCP_REGION:-us-central1}" --format="table(instance,instanceStatus,currentAction)"
  step "Edge MIG"
  EDGE_MIG=$(terraform -chdir="$TF" output -raw edge_mig)
  gcloud compute instance-groups managed list-instances "$EDGE_MIG" \
    --region "${GCP_REGION:-us-central1}" --format="table(instance,instanceStatus,currentAction)"
  step "Registered agents (Supabase)"
  if [ -n "${DATABASE_URL:-}" ]; then
    psql "$DATABASE_URL" -c "SELECT id, endpoint, region, status, age(now(), last_heartbeat) AS lag FROM agents ORDER BY last_heartbeat DESC LIMIT 10;" 2>/dev/null || warn "psql query failed"
  fi
}

cmd_smoke() {
  step "Smoke test against api.pandastack.ai"
  HOST="api.pandastack.ai"
  curl -fsS "https://$HOST/healthz" || die "healthz failed"
  echo " ✓ healthz"
  curl -fsS "https://$HOST/version" | jq . || true
  echo " ✓ version"
  if [ -n "${SMOKE_API_TOKEN:-}" ]; then
    BODY='{"template":"alpine","cpu":1,"memory_mb":256}'
    SB=$(curl -fsS -X POST "https://$HOST/v1/sandboxes" \
      -H "Authorization: Bearer $SMOKE_API_TOKEN" \
      -H "content-type: application/json" \
      -d "$BODY" | jq -r .id)
    [ -n "$SB" ] && echo " ✓ sandbox created: $SB"
    sleep 2
    curl -fsS -X DELETE "https://$HOST/v1/sandboxes/$SB" -H "Authorization: Bearer $SMOKE_API_TOKEN"
    echo " ✓ sandbox deleted"
  else
    warn "set SMOKE_API_TOKEN to run sandbox lifecycle"
  fi
}

cmd_cutover() {
  step "Cloudflare DNS: ensure api.pandastack.ai → LB IP (orange-cloud)"
  LB_IP=$(terraform -chdir="$TF" output -raw lb_ip)
  ZONE="$CLOUDFLARE_ZONE_ID"
  TOK="$CLOUDFLARE_API_TOKEN"
  PROXIED="${CF_PROXIED:-true}"
  upsert() {
    local name="$1" ip="$2"
    EXISTING=$(curl -fsS -H "Authorization: Bearer $TOK" \
      "https://api.cloudflare.com/client/v4/zones/$ZONE/dns_records?name=${name}.${CLOUDFLARE_DOMAIN:-pandastack.ai}&type=A" \
      | jq -r '.result[0].id // ""')
    if [ -n "$EXISTING" ]; then
      curl -fsS -X PATCH -H "Authorization: Bearer $TOK" -H "content-type: application/json" \
        "https://api.cloudflare.com/client/v4/zones/$ZONE/dns_records/$EXISTING" \
        -d "{\"content\":\"$ip\",\"proxied\":$PROXIED,\"ttl\":60}" | jq -r .success
    else
      curl -fsS -X POST -H "Authorization: Bearer $TOK" -H "content-type: application/json" \
        "https://api.cloudflare.com/client/v4/zones/$ZONE/dns_records" \
        -d "{\"type\":\"A\",\"name\":\"$name\",\"content\":\"$ip\",\"proxied\":$PROXIED,\"ttl\":60}" | jq -r .success
    fi
  }
  upsert api "$LB_IP"
  step "DNS updated. proxied=$PROXIED, TTL 60s."
}

cmd_scale() {
  local n="${1:?usage: scale N}"
  AGENT_MIG=$(terraform -chdir="$TF" output -raw agent_mig)
  step "Resizing $AGENT_MIG to $n"
  gcloud compute instance-groups managed resize "$AGENT_MIG" \
    --region "${GCP_REGION:-us-central1}" --size "$n"
}

cmd_down() {
  step "Destroying multi-node stack"
  terraform -chdir="$TF" destroy -auto-approve
}

case "${1:-}" in
  up)              cmd_up ;;
  status)          cmd_status ;;
  smoke)           cmd_smoke ;;
  cutover)         cmd_cutover ;;
  scale)           shift; cmd_scale "$@" ;;
  down)            cmd_down ;;
  bake-templates)  shift; BAKE_TEMPLATES="$*" cmd_bake_templates ;;
  deploy-frontend) cmd_deploy_frontend ;;
  *)
    sed -n '2,18p' "$0"
    echo
    echo "Commands:"
    echo "  up                 one-click: terraform apply + wait healthy + bake + deploy frontend + DNS"
    echo "  status             show LB IP, cert status, MIG state"
    echo "  smoke              healthz/version + (with SMOKE_API_TOKEN) sandbox lifecycle"
    echo "  bake-templates [N…] (re)bake templates on agent VM and push to GCS (FORCE=1 to rebuild)"
    echo "  deploy-frontend    build dashboard + deploy to Cloudflare Pages + attach app.* DNS"
    echo "  scale N            resize agent MIG to N"
    echo "  down               destroy multi-node infra"
    exit 2 ;;
esac
