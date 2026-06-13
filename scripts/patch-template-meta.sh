#!/usr/bin/env bash
# patch-template-meta.sh — Push correct cpu/memory_mb into every template's
# meta.json on both prod agent VMs without a full rootfs rebake.
#
# The agent reads <dataDir>/templates/<name>/meta.json at sandbox-create time.
# If cpu or memory_mb are missing it falls back to 1/512 (wrong). This script
# writes the correct values, then invalidates the stale snapshot so the agent
# rebakes it on next use (automatic — no manual rebake needed).
#
# Usage:
#   ./scripts/patch-template-meta.sh          # patch both VMs
#   ./scripts/patch-template-meta.sh x1bm     # patch x1bm only
#   ./scripts/patch-template-meta.sh pn4q     # patch pn4q only

set -euo pipefail

PROJECT="${GCP_PROJECT:-}"
DATA_DIR="${DATA_DIR:-/var/lib/pandastack}"

# ---- template specs (must match pandastack.ai/templates/) ------------------
# Format: "name:cpu:mem_mb"
TEMPLATE_SPECS=(
  "base:2:2048"
  "code-interpreter:2:2048"
  "agent:2:2048"
  "browser:4:4096"
  "postgres-16:2:1024"
)

# ---- VM list (name:zone) ---------------------------------------------------
ALL_VMS=(
  "pandastack-agent-x1bm:us-central1-a"
  "pandastack-agent-pn4q:us-central1-b"
)

# Apply suffix filter if requested.
target_suffix="${1:-}"
if [[ -n "$target_suffix" ]]; then
  filtered=()
  for entry in "${ALL_VMS[@]}"; do
    vm="${entry%%:*}"
    [[ "$vm" == *"$target_suffix" ]] && filtered+=("$entry")
  done
  if [[ ${#filtered[@]} -eq 0 ]]; then
    echo "No VM matching suffix '$target_suffix'." >&2
    printf '  known: %s\n' "${ALL_VMS[@]%%:*}" >&2
    exit 1
  fi
  ALL_VMS=("${filtered[@]}")
fi

# ---- helpers ---------------------------------------------------------------
log() { printf '\e[36m[%s]\e[0m %s\n' "$(date +%H:%M:%S)" "$*"; }

patch_vm() {
  local vm="$1"
  local zone="$2"

  log "=== $vm ($zone) ==="

  # Build patch commands: one block per template.
  # Both $DATA_DIR and $name are expanded *locally* so the remote receives
  # fully-resolved paths with no shell variables to re-expand.
  local patch_cmds="set -euo pipefail"$'\n'
  for spec in "${TEMPLATE_SPECS[@]}"; do
    local name cpu mem
    IFS=: read -r name cpu mem <<< "$spec"
    local tpl_dir="$DATA_DIR/templates/$name"
    local snap_dir="$DATA_DIR/template-snaps/$name"
    patch_cmds+="
# ---- $name ----
if [ ! -d '$tpl_dir' ]; then
  echo '  SKIP $name (no template dir)'
else
  python3 -c \"
import json
path = '$tpl_dir/meta.json'
try:
    d = json.load(open(path))
except Exception:
    d = {}
if d.get('cpu') == $cpu and d.get('memory_mb') == $mem:
    print('  OK   $name (already cpu=$cpu mem=${mem}MB)')
else:
    d['cpu'] = $cpu
    d['memory_mb'] = $mem
    open(path, 'w').write(json.dumps(d, indent=2) + '\\\n')
    print('  PATCHED $name  cpu=$cpu mem=${mem}MB')
\"
  if [ -d '$snap_dir' ]; then
    rm -f '$snap_dir/snap-meta.json'
    echo '  SNAP  $name (invalidated — will rebake on next use)'
  fi
fi"
  done

  gcloud compute ssh "$vm" \
    --zone="$zone" \
    --project="$PROJECT" \
    --tunnel-through-iap \
    --quiet \
    --command="sudo bash -s" <<< "$patch_cmds"

  log "=== $vm done ==="
}

# ---- main ------------------------------------------------------------------
for entry in "${ALL_VMS[@]}"; do
  vm="${entry%%:*}"
  zone="${entry##*:}"
  patch_vm "$vm" "$zone"
done

log "All done."
log "Verify: gcloud compute ssh pandastack-agent-x1bm --tunnel-through-iap --zone=us-central1-a --project=$PROJECT -- sudo cat /var/lib/pandastack/templates/agent/meta.json"
