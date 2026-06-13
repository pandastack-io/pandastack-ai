#!/usr/bin/env bash
# IAP ProxyCommand for Ansible/OpenSSH.
#
# Agent VMs have NO public IP — SSH must tunnel through Google Identity-Aware
# Proxy. OpenSSH invokes this as:  iap-proxy.sh <host> <port> [zone]
# and we bridge stdin/stdout to the instance's TCP <port> via an IAP tunnel.
#
# Works on any gcloud that supports `start-iap-tunnel --listen-on-stdin`
# (verified on 472.0.0 and current GitHub Actions setup-gcloud). The same
# command path is what `gcloud compute ssh --tunnel-through-iap` uses under the
# hood (see release.yml), so behaviour is identical locally and in CI.
#
# Env overrides:
#   PANDASTACK_GCP_PROJECT  (default: demo)
set -euo pipefail

HOST="${1:?host required}"
PORT="${2:-22}"
ZONE="${3:-}"
PROJECT="${PANDASTACK_GCP_PROJECT:-}"

# The inventory may hand us a full zone URL
# (https://www.googleapis.com/compute/v1/projects/.../zones/us-central1-a);
# keep only the short name.
ZONE="${ZONE##*/}"

# Fall back to a one-shot lookup if the inventory didn't supply the zone.
if [[ -z "$ZONE" ]]; then
  ZONE="$(gcloud compute instances list \
    --project "$PROJECT" \
    --filter="name=${HOST}" \
    --format='value(zone)' --limit=1 2>/dev/null || true)"
  ZONE="${ZONE##*/}"
fi

exec gcloud compute start-iap-tunnel "$HOST" "$PORT" \
  --listen-on-stdin \
  --project="$PROJECT" \
  --zone="$ZONE" \
  --verbosity=warning
