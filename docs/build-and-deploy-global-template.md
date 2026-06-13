# Build & Deploy a Global Template

Global (public/seeded) templates like `code-interpreter`, `ubuntu-24.04-net`, and `nextjs` are
**not** built via `pandastack template build`. They are baked directly on an agent VM from
shell scripts, stored in GCS, and synced to every agent at boot.

## Architecture Overview

```
scripts/build-base-rootfs.sh          # one-time base OS setup
        │
        ▼
ubuntu-24.04-net/rootfs.ext4          # base image (~2 GB)
        │
        ▼
scripts/bake-templates.sh             # chroot-installs packages per template
        │
        ▼
templates/<name>/rootfs.ext4          # baked template image
        │
        ▼
gcloud storage cp → GCS bucket        # push to gs://your-pandastack-bucket/
        │
        ▼ (agent VM boot / cloud-init)
gcloud storage rsync → /var/lib/pandastack/templates/   # pulled onto every agent
        │
        ▼
agent boots Firecracker VM → takes snapshot             # warm boot cache
```

---

## Prerequisites

- SSH access to an agent VM (xdzz or hn84) via IAP
- GCS bucket name: `your-pandastack-bucket` (read from VM metadata)
- Run all bake commands **as root** on the agent VM

---

## Step 1 — SSH into an agent VM

```bash
gcloud compute ssh pandastack-agent-xdzz \
  --zone=us-central1-b \
  --tunnel-through-iap
```

---

## Step 2 — (One-time) Build the base rootfs

Only needed if `ubuntu-24.04-net` does not exist or you need to rebuild it from scratch.

```bash
# On the agent VM, as root:
sudo PANDASTACK_GCS_BUCKET=your-pandastack-bucket \
  bash /path/to/scripts/build-base-rootfs.sh
```

This will:
- Run `debootstrap` to create a minimal Ubuntu 24.04 rootfs
- Install `pandastack-init` and `pandastack-autostart` systemd units
- Output to `/var/lib/pandastack/templates/ubuntu-24.04-net/rootfs.ext4`
- Upload the result to GCS automatically if `PANDASTACK_GCS_BUCKET` is set

> The base rootfs rarely changes. Skip this step if `/var/lib/pandastack/templates/ubuntu-24.04-net/rootfs.ext4` already exists.

---

## Step 3 — Copy bake script to agent VM

```bash
# From your local machine:
gcloud compute scp scripts/bake-templates.sh \
  pandastack-agent-xdzz:/tmp/bake-templates.sh \
  --zone=us-central1-b \
  --tunnel-through-iap
```

---

## Step 4 — Bake the template

```bash
# On the agent VM, as root:
sudo bash /tmp/bake-templates.sh code-interpreter
```

To force a rebuild even if already present:

```bash
sudo FORCE=1 bash /tmp/bake-templates.sh code-interpreter
```

To bake all templates at once:

```bash
sudo bash /tmp/bake-templates.sh
```

What this does:
1. Clones `ubuntu-24.04-net/rootfs.ext4` as the starting point
2. Resizes the image to the target `SIZE_MB` (12288 MB for `code-interpreter`)
3. Mounts the image + bind-mounts `/dev`, `/proc`, `/sys`
4. Runs `tpl::code-interpreter()` inside a chroot (installs Python 3.11, Node.js 22, full DS/AI/Playwright stack)
5. Writes the final image to `/var/lib/pandastack/templates/code-interpreter/rootfs.ext4`
6. Writes `meta.json` with size, cpu, memory specs
7. Purges any stale Firecracker snapshot so it gets rebuilt on next sandbox create

---

## Step 5 — Upload to GCS

The bake script does **not** upload automatically. Push manually after baking:

```bash
# On the agent VM, as root:
GCS_BUCKET=your-pandastack-bucket

gcloud storage cp \
  /var/lib/pandastack/templates/code-interpreter/rootfs.ext4 \
  gs://${GCS_BUCKET}/templates/code-interpreter/rootfs.ext4

gcloud storage cp \
  /var/lib/pandastack/templates/code-interpreter/meta.json \
  gs://${GCS_BUCKET}/templates/code-interpreter/meta.json
```

---

## Step 6 — Sync to all agent VMs

Agent VMs pull from GCS on boot. To sync without rebooting:

```bash
# Run on EACH agent VM (xdzz and hn84):
GCS_BUCKET=your-pandastack-bucket

sudo gcloud storage rsync --recursive \
  gs://${GCS_BUCKET}/templates/ \
  /var/lib/pandastack/templates/

sudo gcloud storage rsync --recursive \
  gs://${GCS_BUCKET}/template-snaps/ \
  /var/lib/pandastack/template-snaps/
```

To sync to hn84:

```bash
gcloud compute ssh pandastack-agent-hn84 \
  --zone=us-central1-a \
  --tunnel-through-iap \
  --command="sudo gcloud storage rsync --recursive gs://your-pandastack-bucket/templates/ /var/lib/pandastack/templates/"
```

---

## Step 7 — Verify

```bash
# Check template is present on agent:
gcloud compute ssh pandastack-agent-xdzz \
  --zone=us-central1-b \
  --tunnel-through-iap \
  --command="cat /var/lib/pandastack/templates/code-interpreter/meta.json && ls -lh /var/lib/pandastack/templates/code-interpreter/"

# Create a test sandbox using the template via the API:
curl -X POST https://api.pandastack.ai/v1/sandboxes \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"template": "code-interpreter"}'
```

---

## Updating a Template Definition

The source of truth for what gets installed in each template is `scripts/bake-templates.sh`.

- Edit `tpl::<name>()` to change installed packages
- Edit `SIZE_MB[<name>]` if the image needs more disk space
- Edit `CPU_COUNT[<name>]` / `MEMORY_MB[<name>]` for different VM sizing
- The Dockerfiles in `templates/<name>/Dockerfile` must stay in sync with `tpl::<name>()`

After editing, repeat Steps 3–6.

---

## Agent VMs Reference

| VM | Zone | Purpose |
|----|------|---------|
| `pandastack-agent-xdzz` | `us-central1-b` | Primary agent |
| `pandastack-agent-hn84` | `us-central1-a` | Secondary agent |
| `pandastack-edge-3tmq` | `us-central1-b` | Edge / API proxy |
| `pandastack-edge-ccbk` | — | Edge / API proxy |

GCS bucket: `your-pandastack-bucket`
