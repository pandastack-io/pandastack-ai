#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# purge-legacy-templates.sh — remove legacy Firecracker templates from the GCS
# template bucket and from every agent/edge VM's local template store.
#
# Background
# ----------
# GET /v1/templates used to be a pure proxy to an agent that scanned its local
# filesystem (/var/lib/pandastack/templates/*/rootfs.ext4). Over time many
# one-off templates were baked onto hosts (amp, claude-code, codex, crawler,
# devin, nextjs, openai-agents, opencode, vite-react). Those rootfs dirs still
# linger on hosts and in GCS even though the curated catalog is now only the
# five first-party globals (base, code-interpreter, agent, browser, postgres-16).
#
# This script physically removes the legacy artifacts so they can never back a
# sandbox again. The control-plane catalog (Cloud SQL `templates` table) is the
# source of truth; this just garbage-collects the orphaned rootfs.
#
# Note: the bake workflow's sync-agents job runs
#   gcloud storage rsync --delete-unmatched-destination-objects
#       gs://$BUCKET/templates/ -> /var/lib/pandastack/templates/
# so deleting from GCS will already propagate deletions to agents on the next
# bake/sync. This script ALSO deletes directly on each VM so the cleanup is
# immediate and does not require a re-bake.
#
# Usage
# -----
#   scripts/purge-legacy-templates.sh                 # dry-run (default)
#   scripts/purge-legacy-templates.sh --apply         # actually delete
#   scripts/purge-legacy-templates.sh --apply --gcs-only
#   scripts/purge-legacy-templates.sh --apply --vms-only
#
# Env overrides:
#   GCS_BUCKET   (default: )
#   GCP_PROJECT  (default: )
#   VM_FILTERS   (default: "pandastack-agent pandastack-edge")

set -euo pipefail

GCS_BUCKET="${GCS_BUCKET:-}"
GCP_PROJECT="${GCP_PROJECT:-}"
VM_FILTERS="${VM_FILTERS:-pandastack-agent pandastack-edge}"

# KEEP — the curated catalog. These are NEVER deleted. ubuntu-24.04-net is the
# internal bake base image and is also preserved.
KEEP=(base code-interpreter agent browser postgres-16 ubuntu-24.04-net)

# LEGACY — templates to purge. Explicit allowlist-of-removal so we can never
# accidentally delete a current template by enumeration drift.
LEGACY=(amp claude-code codex crawler devin nextjs openai-agents opencode vite-react)

APPLY=0
DO_GCS=1
DO_VMS=1

for arg in "$@"; do
  case "$arg" in
    --apply)     APPLY=1 ;;
    --gcs-only)  DO_VMS=0 ;;
    --vms-only)  DO_GCS=0 ;;
    -h|--help)
      sed -n '2,40p' "$0"
      exit 0 ;;
    *)
      echo "unknown arg: $arg" >&2
      exit 2 ;;
  esac
done

is_kept() {
  local n="$1" k
  for k in "${KEEP[@]}"; do
    [[ "$n" == "$k" ]] && return 0
  done
  return 1
}

# Guard: refuse to run if any LEGACY entry collides with a KEEP entry.
for l in "${LEGACY[@]}"; do
  if is_kept "$l"; then
    echo "FATAL: '$l' is in both KEEP and LEGACY lists — aborting." >&2
    exit 1
  fi
done

run() {
  if [[ "$APPLY" -eq 1 ]]; then
    echo "  + $*"
    "$@"
  else
    echo "  [dry-run] $*"
  fi
}

echo "=============================================="
echo " purge-legacy-templates"
echo "   bucket : gs://$GCS_BUCKET/templates/"
echo "   project: $GCP_PROJECT"
echo "   vm flt : $VM_FILTERS"
echo "   mode   : $([[ $APPLY -eq 1 ]] && echo APPLY || echo DRY-RUN)"
echo "   keep   : ${KEEP[*]}"
echo "   purge  : ${LEGACY[*]}"
echo "=============================================="

############################################
# 1) GCS bucket cleanup
############################################
if [[ "$DO_GCS" -eq 1 ]]; then
  echo ""
  echo "── GCS: gs://$GCS_BUCKET/templates/ ──"
  for name in "${LEGACY[@]}"; do
    prefix="gs://$GCS_BUCKET/templates/$name/"
    if gcloud storage ls "$prefix" >/dev/null 2>&1; then
      echo "found $prefix"
      run gcloud storage rm --recursive "$prefix"
    else
      echo "absent $prefix (skip)"
    fi
  done
fi

############################################
# 2) Per-VM local store cleanup
############################################
if [[ "$DO_VMS" -eq 1 ]]; then
  echo ""
  echo "── Agent/edge VMs ──"
  # Build the remote rm command for all legacy names in one shot.
  remote_cmd=""
  for name in "${LEGACY[@]}"; do
    remote_cmd+="sudo rm -rf /var/lib/pandastack/templates/$name /var/lib/pandastack/template-snaps/$name; "
  done
  remote_cmd+="echo '✓ legacy templates removed on '\$(hostname); ls /var/lib/pandastack/templates/ 2>/dev/null"

  for filter in $VM_FILTERS; do
    echo ""
    echo "Discovering VMs matching: name~^$filter"
    VMS=$(gcloud compute instances list \
      --project="$GCP_PROJECT" \
      --filter="name~^${filter}" \
      --format="value(name,zone)" 2>/dev/null || true)
    if [[ -z "$VMS" ]]; then
      echo "  (no VMs matched '$filter')"
      continue
    fi
    while IFS=$'\t' read -r vm_name vm_zone; do
      [[ -z "$vm_name" ]] && continue
      echo ""
      echo "── $vm_name ($vm_zone) ──"
      if [[ "$APPLY" -eq 1 ]]; then
        gcloud compute ssh "$vm_name" \
          --zone="$vm_zone" \
          --project="$GCP_PROJECT" \
          --tunnel-through-iap \
          --quiet \
          --command="$remote_cmd" </dev/null || echo "  ⚠️  ssh failed on $vm_name (continuing)"
      else
        echo "  [dry-run] ssh $vm_name -- $remote_cmd"
      fi
    done <<< "$VMS"
  done
fi

echo ""
if [[ "$APPLY" -eq 1 ]]; then
  echo "✅ purge complete."
else
  echo "ℹ️  dry-run only. Re-run with --apply to delete."
fi
