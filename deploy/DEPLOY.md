# PandaStack Production Deploy Guide

**Project:** `your-gcp-project` (GCP)  
**Topology:** edge MIG (API + dashboard, `us-central1`) + agent MIG (Firecracker, `us-central1`) + ClickHouse VM  
**Terraform state:** `gs://REPLACE_WITH_YOUR_TFSTATE_BUCKET/pandastack-ai/`  
**Terraform env:** `infra/terraform/envs/dev-gcp-multi/`

---

## Prerequisites

```bash
gcloud auth login
gcloud config set project your-gcp-project
```

---

## Rolling updates — how it works

All infra is managed by Terraform + GCP regional MIGs. Rolling updates are zero-downtime:
- `max_unavailable_fixed = 0` — old instances stay up until new ones are HEALTHY
- `max_surge_fixed = zones` — GCP spins up new instances first, verifies health, then removes old ones
- Edge health check: `GET :8080/healthz` — returns 200 when DB connected
- Agent health check: `GET :8081/healthz`

---

## Update 1 — API/agent binary only (no infra change)

**Step 1:** Build and upload the binary:

```bash
# Build for linux/amd64
(cd api   && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../deploy/.build/bin/pandastack-api   ./cmd/api)
(cd agent && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../deploy/.build/bin/pandastack-agent ./cmd/agent)

# Pack and upload edge bundle (api + dashboard)
mkdir -p deploy/.build/dashboard
tar -czf deploy/.build/edge-bundle.tgz -C deploy/.build bin dashboard
gcloud storage cp deploy/.build/edge-bundle.tgz gs://REPLACE_WITH_YOUR_BUILDS_BUCKET/bundles/edge-latest.tgz

# Upload agent binary separately
gcloud storage cp deploy/.build/bin/pandastack-agent gs://REPLACE_WITH_YOUR_BUILDS_BUCKET/bin/pandastack-agent
```

**Step 2:** Trigger rolling restart — GCP replaces each instance with a fresh boot (pulls new binary from GCS):

```bash
# Edge (API) — ~3 min per instance, 0 downtime
gcloud compute instance-groups managed rolling-action replace pandastack-edge-mig \
  --region=us-central1 --max-unavailable=0 --max-surge=2

# Agent — ~15 min per instance (Firecracker setup)
gcloud compute instance-groups managed rolling-action replace pandastack-agent-mig \
  --region=us-central1 --max-unavailable=0 --max-surge=2
```

**Monitor:**
```bash
watch -n10 'gcloud compute instance-groups managed list-instances pandastack-edge-mig \
  --region=us-central1 --format="table(name,currentAction,instanceStatus,version.instanceTemplate.basename())"'
```

---

## Update 2 — Infra change (startup script, metadata, new secrets)

Terraform handles this end-to-end. It creates a new instance template and the MIG auto-rolls.

```bash
cd infra/terraform/envs/dev-gcp-multi
terraform plan -out=tfplan.out   # review changes
terraform apply tfplan.out
```

Terraform will:
1. Create a new instance template (new startup script / metadata)
2. Update the MIG `version.instanceTemplate` → triggers proactive rolling update
3. GCP spins up new instances, waits for `/healthz`, removes old ones

---

## Update 3 — Secret update (DB DSN, tokens, etc.)

Secrets are stored in GCP Secret Manager. Running instances read them **once at boot**. To propagate a secret change:

**Step 1:** Update the secret:
```bash
# Add a new version (old version stays accessible until you destroy it)
echo -n "NEW_VALUE" | gcloud secrets versions add pandastack-database-url --data-file=-

# Or from file:
gcloud secrets versions add pandastack-database-url --data-file=./db-url.txt
```

**Available secrets:**
| Secret name | Used by |
|---|---|
| `pandastack-database-url` | Edge + Agent (Postgres DSN) |
| `pandastack-clickhouse-url` | Edge + Agent (ClickHouse HTTP URL, auto-managed by TF) |
| `pandastack-node-token` | Edge + Agent (bearer auth between edge↔agent) |
| `pandastack-supabase-jwks-url` | Edge (JWT verification) |

**Step 2:** Rolling restart to pick up new secret:
```bash
gcloud compute instance-groups managed rolling-action replace pandastack-edge-mig \
  --region=us-central1 --max-unavailable=0 --max-surge=2
```

---

## Smoke test

```bash
curl -fsS https://api.pandastack.ai/healthz | jq .
# Expected: {"status":"ok","checks":{"PANDASTACK_DB_DSN":"ok"}}

curl -fsS https://api.pandastack.ai/version | jq .
```

---

## Monitor MIG health

```bash
# Backend service health (what the LB sees)
gcloud compute backend-services get-health pandastack-edge-backend --global

# Instance status
gcloud compute instance-groups managed list-instances pandastack-edge-mig --region=us-central1
gcloud compute instance-groups managed list-instances pandastack-agent-mig --region=us-central1

# Serial logs for a specific instance (replace name/zone)
gcloud compute instances get-serial-port-output pandastack-edge-b39v \
  --zone=us-central1-b | tail -50
```

---

## ClickHouse

ClickHouse runs on a dedicated VM `pandastack-clickhouse-1` (us-central1-a, no public IP).  
Data is on a persistent disk `pandastack-clickhouse-data` — survives VM recreation.  
Internal URL is auto-written to Secret Manager as `pandastack-clickhouse-url` by Terraform.

```bash
# Check ClickHouse status
gcloud compute instances describe pandastack-clickhouse-1 --zone=us-central1-a \
  --format="value(status,networkInterfaces[0].networkIP)"

# View startup log
gcloud compute instances get-serial-port-output pandastack-clickhouse-1 \
  --zone=us-central1-a | grep -E "clickhouse|schema|bootstrap|error" | tail -20
```

---

## Dashboard deploy (Cloudflare Pages → `app.pandastack.ai`)

Dashboard runs on **Cloudflare Pages** (not the edge VMs).

```bash
bash deploy/deploy-dashboard-cf.sh
```

---

## Docs deploy (Cloudflare Pages → `docs.pandastack.ai`)

```bash
bash deploy/deploy-docs-cf.sh
```

---

## Notes

- No public SSH on any VM — IAP tunnel only (but not needed for standard deploys)
- Never use `git add -A`. Stage specific files explicitly.
- GCS bucket for builds: `REPLACE_WITH_YOUR_BUILDS_BUCKET` (project `your-gcp-project`)
- Terraform state backend: `gs://REPLACE_WITH_YOUR_TFSTATE_BUCKET/pandastack-ai/`

