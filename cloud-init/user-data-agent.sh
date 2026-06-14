#!/bin/bash
# cloud-init for pandastack-agent nodes (multi-node mode).
# Runs the Firecracker compute plane only. No public web surface; reachable
# only from edge VMs over the agents subnet on :8081 (gated by X-Node-Token).
#
# Reads identity + secrets via the GCP metadata server and Secret Manager.
# The TF agent-mig module supplies the following instance metadata:
#   pandastack-region        - GCP region label
#   pandastack-gcs-bucket    - GCS bucket name for kernels/templates/snapshots
#   pandastack-binary-url    - HTTPS URL serving the pandastack-agent linux/amd64 binary
#   secret-node-token        - Secret Manager secret ID containing the shared bearer
#   secret-database-url      - Secret Manager secret ID containing the Supabase DSN
#   secret-clickhouse-url    - Secret Manager secret ID containing the CH URL
#   secret-jwks-url          - Secret Manager secret ID containing the Supabase JWKS URL
set -euxo pipefail
exec > >(tee -a /var/log/pandastack-cloud-init.log) 2>&1

export DEBIAN_FRONTEND=noninteractive

# Wait for any concurrent apt/dpkg (e.g. cloud-init's own package_update) to release locks.
for _ in $(seq 1 120); do
  if ! fuser /var/lib/dpkg/lock-frontend /var/lib/apt/lists/lock /var/lib/dpkg/lock >/dev/null 2>&1; then
    break
  fi
  sleep 5
done

# Force IPv4 for apt — this VPC has no IPv6 route to GCE package mirrors.
echo 'Acquire::ForceIPv4 "true";' > /etc/apt/apt.conf.d/99force-ipv4

apt-get update
# Base runtime tooling + extras needed for template baking
# (debootstrap/chroot used by scripts/build-base-rootfs.sh + scripts/bake-templates.sh).
# postgresql-client: used by NATID claim (psql) and heartbeat timer.
apt-get install -y ca-certificates curl wget jq squashfs-tools iproute2 iptables \
  uuid-runtime e2fsprogs sqlite3 xfsprogs debootstrap rsync coreutils postgresql-client

# Install gcloud CLI (Google Cloud SDK) for Secret Manager access.
if ! command -v gcloud >/dev/null 2>&1; then
  echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" \
    | tee /etc/apt/sources.list.d/google-cloud-sdk.list
  curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
    | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg
  apt-get update
  apt-get install -y google-cloud-cli
fi

MD="http://metadata.google.internal/computeMetadata/v1"
md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1" 2>/dev/null || true; }

REGION="$(md instance/attributes/pandastack-region)"
GCS_BUCKET="$(md instance/attributes/pandastack-gcs-bucket)"
# Snapshot/WAL bucket. Unset => the agent silently disables WAL archiving,
# db failover restore, and snapshot/fork GCS replication. Terraform defaults
# this to the seeds bucket; fall back the same way here for instances whose
# metadata predates the key.
SNAPSHOT_BUCKET="$(md instance/attributes/pandastack-snapshot-bucket)"
[ -n "$SNAPSHOT_BUCKET" ] || SNAPSHOT_BUCKET="$GCS_BUCKET"
BINARY_URL="$(md instance/attributes/pandastack-binary-url)"
SECRET_TOKEN="$(md instance/attributes/secret-node-token)"
SECRET_DB="$(md instance/attributes/secret-database-url)"
SECRET_CH="$(md instance/attributes/secret-clickhouse-url)"
SECRET_JWKS="$(md instance/attributes/secret-jwks-url)"
ZONE_FULL="$(md instance/zone | awk -F/ '{print $NF}')"
INSTANCE_NAME="$(md instance/name)"
INTERNAL_IP="$(md instance/network-interfaces/0/ip)"

# Fetch secrets.
fetch_secret() {
  local name="$1"
  [ -n "$name" ] && gcloud secrets versions access latest --secret="$name" 2>/dev/null || true
}
NODE_TOKEN="$(fetch_secret "$SECRET_TOKEN")"
DATABASE_URL="$(fetch_secret "$SECRET_DB")"
CLICKHOUSE_URL="$(fetch_secret "$SECRET_CH")"
SUPABASE_JWKS_URL="$(fetch_secret "$SECRET_JWKS")"

# Install Firecracker.
# v1.16.0: brings the vsock UDS path override at /snapshot/load (PR #5323)
# used for the per-sandbox readiness ping. NOTE: Firecracker snapshots are
# version-bound — seeds baked on another FC version are rejected by seed-sync
# (manifest fc_version check) and re-baked on first spawn.
install -d -m 0755 /usr/local/src/pandastack-firecracker
if ! command -v firecracker >/dev/null 2>&1 || ! firecracker --version 2>/dev/null | grep -q '1.16.0'; then
  for i in 1 2 3 4 5; do
    wget -qO /usr/local/src/pandastack-firecracker/firecracker-v1.16.0-x86_64.tgz \
      https://github.com/firecracker-microvm/firecracker/releases/download/v1.16.0/firecracker-v1.16.0-x86_64.tgz \
      && file /usr/local/src/pandastack-firecracker/firecracker-v1.16.0-x86_64.tgz | grep -q gzip && break
    echo "firecracker download attempt $i failed, retrying"; sleep $((i*5))
  done
  tar -xzf /usr/local/src/pandastack-firecracker/firecracker-v1.16.0-x86_64.tgz -C /usr/local/src/pandastack-firecracker
  install -m 0755 /usr/local/src/pandastack-firecracker/release-v1.16.0-x86_64/firecracker-v1.16.0-x86_64 /usr/local/bin/firecracker
  install -m 0755 /usr/local/src/pandastack-firecracker/release-v1.16.0-x86_64/jailer-v1.16.0-x86_64 /usr/local/bin/jailer
fi

groupadd -f kvm
usermod -aG kvm ubuntu
cat > /etc/udev/rules.d/99-kvm.rules <<'KVMRULES'
KERNEL=="kvm", GROUP="kvm", MODE="0660"
KVMRULES
udevadm control --reload-rules
udevadm trigger --name-match=kvm || true

sysctl -w net.ipv4.ip_forward=1
# 2 MiB hugepages for Firecracker guest memory (PANDASTACK_HUGEPAGES).
#
# TWO knobs, and the difference is the whole ballgame:
#   * nr_overcommit_hugepages = an ON-DEMAND PERMISSION. The kernel only tries
#     to assemble a 2 MiB page when one is requested. Under memory FRAGMENTATION
#     (free RAM shattered into <2 MiB buddy orders) that assembly FAILS, and the
#     snapshot dump faults mid-write -> "Cannot dump memory: Guest memory error:
#     Bad address (EFAULT)". Plenty of *free* RAM does not help; what matters is
#     contiguous 2 MiB runs (/proc/buddyinfo order-9), which collapse to 0 once
#     the host has done any real work. MemAvailable cannot see this.
#   * nr_hugepages = a BOOT-TIME RESERVATION. The kernel carves N contiguous
#     2 MiB blocks out of pristine, unfragmented RAM at boot and parks them in a
#     dedicated pool. A bake/restore then draws from that guaranteed-contiguous
#     pool instead of gambling on on-demand assembly. THIS is what makes
#     hugepages actually work on a long-lived agent host.
#
# Reserve enough for one bake of the largest template (clone.ext4 guest mem is
# 2 GiB) plus headroom: ceil(RAM_MiB / 2 * 0.30) pages, floored at 1152 (=2.25
# GiB on an 8 GB n2-standard-2) and capped at 8192 (=16 GiB) so it scales up to
# n2-standard-8 without stranding most of RAM. The pool is reclaimable: pages
# not held by a running guest are returned to the buddy allocator for page
# cache, so an idle host is not penalised.
MEMTOTAL_MIB=$(( $(awk '/MemTotal/{print $2}' /proc/meminfo) / 1024 ))
HUGEPAGE_RESERVE=$(( MEMTOTAL_MIB * 30 / 100 / 2 ))
if [ "${HUGEPAGE_RESERVE}" -lt 1152 ]; then HUGEPAGE_RESERVE=1152; fi
if [ "${HUGEPAGE_RESERVE}" -gt 8192 ]; then HUGEPAGE_RESERVE=8192; fi
# Overcommit stays as a best-effort secondary path on top of the reservation,
# for transient bursts beyond the reserved pool.
HUGEPAGE_OVERCOMMIT=$(( MEMTOTAL_MIB / 2048 ))
cat > /etc/sysctl.d/99-pandastack.conf <<SYSCTL
net.ipv4.ip_forward=1
vm.nr_hugepages=${HUGEPAGE_RESERVE}
vm.nr_overcommit_hugepages=${HUGEPAGE_OVERCOMMIT}
SYSCTL
# Apply nr_hugepages NOW, as early as possible in cloud-init, while RAM is still
# largely unfragmented (later boot work shrinks the contiguous pool the kernel
# can reserve). On reboot the sysctl drop-in re-applies it from a clean slate.
sysctl -w vm.nr_hugepages="${HUGEPAGE_RESERVE}" || true
sysctl -w vm.nr_overcommit_hugepages="${HUGEPAGE_OVERCOMMIT}" || true
echo "pandastack: hugepage reservation -> requested=${HUGEPAGE_RESERVE} actual=$(awk '/HugePages_Total/{print $2}' /proc/meminfo)"

# Ensure dm_snapshot kernel module is loaded at boot (required for Option B
# dm-snapshot CoW rootfs — eliminates per-sandbox 100-400ms file copy).
echo "dm_snapshot" > /etc/modules-load.d/pandastack.conf
modprobe dm_snapshot || true

install -d -m 0755 /var/lib/pandastack /var/lib/pandastack-io /run/fcsandbox /etc/pandastack

# /var/lib/pandastack on XFS+reflink loopback so FICLONE (CoW) works between
# template-snaps/*/clone.ext4 and vms/<id>/rootfs.ext4 — drops rootfs
# clone from ~5500ms (full copy) to ~1ms (reflink). Same FS is critical.
if [ "$(stat -f -c %T /var/lib/pandastack)" != "xfs" ]; then
  if [ ! -f /opt/pandastack.img ]; then
    # 300G XFS+reflink data volume. Must stay below agent_boot_disk_size_gb
    # (400G) minus OS/headroom. Holds template-snaps + based rootfs (~95G of
    # preseeded public-template artifacts) plus per-sandbox CoW rootfs, fork
    # trees, user snapshots and volumes.
    truncate -s 300G /opt/pandastack.img
    mkfs.xfs -m reflink=1 -m crc=1 -q /opt/pandastack.img
  fi
  # Stage any existing data into the new FS before swapping.
  install -d -m 0755 /mnt/pandastack-stage
  mount -o loop /opt/pandastack.img /mnt/pandastack-stage
  if [ -d /var/lib/pandastack ] && [ "$(ls -A /var/lib/pandastack 2>/dev/null)" ]; then
    cp -a /var/lib/pandastack/. /mnt/pandastack-stage/ || true
  fi
  umount /mnt/pandastack-stage
  rmdir /mnt/pandastack-stage
  mount -o loop /opt/pandastack.img /var/lib/pandastack
  if ! grep -q "/opt/pandastack.img" /etc/fstab; then
    echo "/opt/pandastack.img /var/lib/pandastack xfs loop,defaults 0 0" >> /etc/fstab
  fi
fi

# ── Durable volumes disk (P0 durability fix) ─────────────────────────────────
# Customer volumes + managed-DB PGDATA live at /var/lib/pandastack/volumes.
# Backed by the STATEFUL per-instance PD (terraform device_name
# "pandastack-volumes") so a MIG autoheal/recreate detaches and reattaches the
# SAME disk instead of wiping customer data with the boot disk. Mounted ON TOP
# of the XFS loopback above (nested mount), so volume files no longer consume
# the 300G pandastack.img budget. Instances without the disk (pre-disk
# template) fall through — volumes stay on the loopback as before.
VOLDEV=/dev/disk/by-id/google-pandastack-volumes
if [ -e "$VOLDEV" ]; then
  if ! blkid "$VOLDEV" >/dev/null 2>&1; then
    # Blank disk (first boot of this instance) — format once. blkid guard means
    # a reattached disk with existing data is NEVER reformatted.
    mkfs.ext4 -F -q -L pandastack-vol "$VOLDEV"
  fi
  install -d -m 0755 /var/lib/pandastack/volumes
  if ! grep -q "google-pandastack-volumes" /etc/fstab; then
    # x-systemd.requires orders this after the XFS loopback mount it nests in;
    # nofail keeps boot alive if the disk is ever missing.
    echo "$VOLDEV /var/lib/pandastack/volumes ext4 defaults,nofail,x-systemd.requires=/var/lib/pandastack 0 2" >> /etc/fstab
  fi
  mountpoint -q /var/lib/pandastack/volumes || mount "$VOLDEV" /var/lib/pandastack/volumes
fi

install -d -m 0755 /var/lib/pandastack/kernels /var/lib/pandastack/templates \
  /var/lib/pandastack/template-snaps /var/lib/pandastack/vms /var/lib/pandastack/snapshots
# Key dir must exist before bootstrap_shared_key installs the fleet-wide key
# into it (install(1) does not create the parent). 0700: private key material.
install -d -m 0700 /var/lib/pandastack/keys

# Pull pandastack-agent binary.
if [ -n "$BINARY_URL" ]; then
  curl -fsSL "$BINARY_URL" -o /usr/local/bin/pandastack-agent
  chmod 0755 /usr/local/bin/pandastack-agent
elif [ -n "$GCS_BUCKET" ]; then
  gcloud storage cp "gs://${GCS_BUCKET}/bin/pandastack-agent" /usr/local/bin/pandastack-agent || true
  chmod 0755 /usr/local/bin/pandastack-agent 2>/dev/null || true
fi

# Always pull the agent binary fresh — it's small (40MB) and we redeploy often.
# Kernels/templates are large and rarely change; sync them but rsync is already
# a no-op when the local copy is byte-identical (golden-image friendly).
if [ -n "$GCS_BUCKET" ]; then
  gcloud storage cp "gs://${GCS_BUCKET}/bin/pandastack-init" /usr/local/bin/pandastack-init || true
  chmod 0755 /usr/local/bin/pandastack-init 2>/dev/null || true
fi

# Sync kernels + baked templates from GCS into the agent's data dir.
# These paths MUST match the agent's -data-dir (= /var/lib/pandastack).
# `gcloud storage rsync` is idempotent: it skips unchanged files, so on a
# golden-image boot this is effectively a metadata-only check (~1s).
if [ -n "$GCS_BUCKET" ]; then
  mkdir -p /var/lib/pandastack/kernels /var/lib/pandastack/templates /var/lib/pandastack/template-snaps
  gcloud storage rsync --recursive "gs://${GCS_BUCKET}/kernels/"        /var/lib/pandastack/kernels/        || true
  gcloud storage rsync --recursive "gs://${GCS_BUCKET}/templates/"      /var/lib/pandastack/templates/      || true
  # Drop any stale agent-key bake markers (<rootfs>.dkey) that rsync may leave
  # next to a refreshed rootfs. The marker says "my SSH key is already baked
  # into this rootfs", but a re-synced rootfs no longer carries it; a surviving
  # marker would make the agent skip key injection and lose SSH into every
  # guest. Deleting them forces a correct re-bake on first use. (The agent code
  # also binds the marker to the rootfs identity, so this is defense-in-depth.)
  find /var/lib/pandastack/templates/ -name '*.ext4.dkey' -delete 2>/dev/null || true
  # NOTE: pre-baked Firecracker *snapshots* are no longer pulled with a blind
  # rsync of gs://$BUCKET/template-snaps/ (which shipped 10G build scratch and
  # mismatched-key snapshots). They are fetched, integrity-checked and
  # compatibility-gated by `pandastack-agent seed-sync` further below, AFTER
  # the fleet-shared ssh key is in place.
fi

# ── Fleet-shared SSH key ──────────────────────────────────────────────────────
# Every agent must inject the SAME ed25519 public key into guest rootfs/snapshots
# so that a snapshot baked on one agent can be restored (and SSH'd into) by any
# other agent. Without this, each agent generates its own key and rejects every
# peer's seeded snapshot (fingerprint mismatch -> rebake), defeating seeding.
#
# Bootstrap is atomic and fail-safe:
#   * download gs://$BUCKET/keys/agent_keypair.tar (a single object holding both
#     the private key and its .pub, so the pair can never be split);
#   * if absent, generate locally and try to publish it with an
#     if-generation-match=0 precondition (atomic create — only the first racer
#     wins), then re-download the winner;
#   * verify the public half derived from the private key matches the .pub;
#   * on ANY failure, leave the keys dir empty so the agent self-generates a
#     per-agent key (today's behaviour) — seeding becomes a no-op, never a
#     correctness failure.
SHARED_KEY_OK=0
install -d -m 0700 /var/lib/pandastack/keys
bootstrap_shared_key() {
  [ -n "$GCS_BUCKET" ] || return 1
  local obj="gs://${GCS_BUCKET}/keys/agent_keypair.tar"
  local tmp; tmp="$(mktemp -d)"
  # 1) Try to fetch an existing bundle.
  if ! gcloud storage cp "$obj" "$tmp/agent_keypair.tar" 2>/dev/null; then
    # 2) Generate and attempt an atomic create. Lose the race -> fall through
    #    to the re-download below.
    ssh-keygen -t ed25519 -N '' -C 'pandastack-agent-shared' -f "$tmp/agent_ed25519" >/dev/null 2>&1 || return 1
    tar -cf "$tmp/agent_keypair.tar" -C "$tmp" agent_ed25519 agent_ed25519.pub || return 1
    gcloud storage cp --if-generation-match=0 "$tmp/agent_keypair.tar" "$obj" 2>/dev/null || true
    # Always re-download the authoritative winner (whoever created gen 0).
    gcloud storage cp "$obj" "$tmp/agent_keypair.tar" 2>/dev/null || return 1
  fi
  # 3) Unpack + verify the pair is consistent.
  tar -xf "$tmp/agent_keypair.tar" -C "$tmp" || return 1
  [ -f "$tmp/agent_ed25519" ] && [ -f "$tmp/agent_ed25519.pub" ] || return 1
  local derived stored
  derived="$(ssh-keygen -y -f "$tmp/agent_ed25519" 2>/dev/null | awk '{print $1" "$2}')" || return 1
  stored="$(awk '{print $1" "$2}' "$tmp/agent_ed25519.pub")" || return 1
  [ -n "$derived" ] && [ "$derived" = "$stored" ] || return 1
  install -m 0600 "$tmp/agent_ed25519"     /var/lib/pandastack/keys/agent_ed25519
  install -m 0644 "$tmp/agent_ed25519.pub" /var/lib/pandastack/keys/agent_ed25519.pub
  rm -rf "$tmp"
  return 0
}
if bootstrap_shared_key; then
  SHARED_KEY_OK=1
  echo "Fleet-shared ssh key installed."
else
  echo "WARN: shared ssh key bootstrap failed; agent will self-generate (seeding disabled this boot)."
fi

# ── NATID claim ──────────────────────────────────────────────────────────────
# NATID is a unique integer per agent VM that defines a non-overlapping IP
# namespace for Firecracker VMs on this host. We claim it from Postgres under
# an advisory lock so concurrent boots don't race. The claim is renewed every
# 2 min by pandastack-natid-heartbeat.timer; stale claims (>30 min) are
# evicted on the next boot. If the pool is exhausted, startup FAILS rather
# than silently assigning a duplicate NATID.
#
# Uses: postgresql-client (installed above), DATABASE_URL (fetched above).
# :'varname' is psql's properly-quoted string-literal syntax for -v variables.
claim_natid() {
  local db_dsn="$1"
  local instance_id="$2"
  local region="$3"
  psql "$db_dsn" \
    -v "instance_id=${instance_id}" \
    -v "region=${region}" \
    -t -A 2>/dev/null <<'NATID_SQL'
\set ON_ERROR_STOP on
BEGIN;
SELECT pg_advisory_xact_lock(8765432100);
DELETE FROM agent_natid_claims
  WHERE last_heartbeat < NOW() - INTERVAL '30 minutes'
    AND instance_id != :'instance_id';
UPDATE agent_natid_claims SET last_heartbeat = NOW() WHERE instance_id = :'instance_id';
INSERT INTO agent_natid_claims (natid, instance_id, region)
  SELECT n, :'instance_id', :'region'
  FROM generate_series(1, 24) AS n
  WHERE NOT EXISTS (SELECT 1 FROM agent_natid_claims WHERE natid = n)
    AND NOT EXISTS (SELECT 1 FROM agent_natid_claims WHERE instance_id = :'instance_id')
  ORDER BY n LIMIT 1;
SELECT natid FROM agent_natid_claims WHERE instance_id = :'instance_id';
COMMIT;
NATID_SQL
}

# Retry up to 5 times in case DB is momentarily unreachable at boot.
NATID=""
for attempt in 1 2 3 4 5; do
  NATID=$(claim_natid "$DATABASE_URL" "$INSTANCE_NAME" "$REGION" | grep -E '^[0-9]+$' | head -1 | tr -d '[:space:]' || true)
  if echo "$NATID" | grep -qE '^[0-9]+$'; then break; fi
  echo "NATID claim attempt $attempt failed, retrying in 10s..."
  sleep 10
done
if ! echo "$NATID" | grep -qE '^[0-9]+$'; then
  echo "ERROR: Failed to claim NATID after 5 attempts. Pool exhausted or DB unreachable. Aborting."
  exit 1
fi
echo "Claimed NATID=${NATID} for instance ${INSTANCE_NAME}"

# Persist NATID to instance metadata for observability (best-effort).
gcloud compute instances add-metadata "$INSTANCE_NAME" \
  --zone="$ZONE_FULL" --metadata="pandastack-natid=${NATID}" 2>/dev/null || true

# Write environment file.
cat > /etc/pandastack/env.agent <<EOF
PANDASTACK_DB_DRIVER=pgx
PANDASTACK_DB_DSN=${DATABASE_URL}
PANDASTACK_NODE_TOKEN=${NODE_TOKEN}
PANDASTACK_LISTEN_TCP=:8081
PANDASTACK_METRICS_LISTEN=:9100
PANDASTACK_AGENT_ID=${INSTANCE_NAME}
PANDASTACK_AGENT_ENDPOINT=http://${INTERNAL_IP}:8081
PANDASTACK_REGION=${REGION}
PANDASTACK_ZONE=${ZONE_FULL}
PANDASTACK_INTERNAL_IP=${INTERNAL_IP}
PANDASTACK_GCS_BUCKET=${GCS_BUCKET}
# Snapshot/WAL bucket: user snapshot+fork replication, managed-DB WAL archive
# (wal_relay) and db failover restore. Unset = all three silently disabled.
PANDASTACK_SNAPSHOT_BUCKET=${SNAPSHOT_BUCKET}
# UFFD streaming first-boot: serve guest page faults on demand from the seed's
# vm.mem (and GCS range reads) instead of downloading the whole memory image
# before boot. Eliminates the cold-host first-boot cliff. The agent advertises
# this in its heartbeat (registry.Capacity.StreamRestoreEnabled) so the
# scheduler prefers streaming-capable hosts in the cold tier.
PANDASTACK_STREAM_RESTORE=1
# Back guest memory with 2 MiB hugetlbfs pages on cold boots: one page fault
# covers 2 MiB instead of 4 KiB (512x fewer faults on restore) and EPT/TLB
# pressure drops. Snapshots taken from hugepage VMs carry a "hugepages" marker
# and restore through the UFFD backend on ANY agent regardless of this flag
# (it gates cold boots only). Requires vm.nr_overcommit_hugepages (sysctl
# above). Re-bake templates after enabling: 4 KiB snapshots stay 4 KiB.
PANDASTACK_HUGEPAGES=1
# NATID here is a MODE FLAG, not the claimed slot. The agent only ever tests
# PANDASTACK_NATID == "1" to enable NAT-identity networking; it never parses it
# as an integer. Setting it to the claimed slot (1..24) was a latent bug: only
# the agent that happened to claim slot 1 enabled NATID mode, so a second agent
# (slot 2) would silently fall back to legacy networking and mismatch every
# natid-flavored snapshot. We pin the mode to 1 and keep the claimed slot in a
# separate, observability-only variable.
PANDASTACK_NATID=1
PANDASTACK_NATID_SLOT=${NATID}
PANDASTACK_NATID_POOL_SIZE=24
PANDASTACK_SHARED_KEY=${SHARED_KEY_OK}
SUPABASE_JWKS_URL=${SUPABASE_JWKS_URL}
CLICKHOUSE_URL=${CLICKHOUSE_URL}
PANDASTACK_CLICKHOUSE_URL=${CLICKHOUSE_URL}
EOF
chmod 0600 /etc/pandastack/env.agent

# ── Seed fleet-shared template snapshots (before the agent starts serving) ────
# Pulls compatible, integrity-checked snapshots from gs://$BUCKET/seeds/ so this
# agent is fast from second zero instead of cold-baking every template. Always
# exits 0 (best-effort); a template that can't be seeded is simply cold-baked on
# first use. Requires the shared key (above) so fingerprints match.
if [ -n "$GCS_BUCKET" ] && [ "$SHARED_KEY_OK" = "1" ] && [ -x /usr/local/bin/pandastack-agent ]; then
  PANDASTACK_GCS_BUCKET="$GCS_BUCKET" PANDASTACK_NATID=1 \
    /usr/local/bin/pandastack-agent seed-sync --data-dir /var/lib/pandastack || true
fi

cat > /etc/systemd/system/pandastack-agent.service <<'UNIT'
[Unit]
Description=Pandastack Firecracker agent (multi-node)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/pandastack/env.agent
ExecStart=/usr/local/bin/pandastack-agent \
  -socket /run/fcsandbox/agent.sock \
  -data-dir /var/lib/pandastack \
  -db /var/lib/pandastack/pandastack.db \
  -metrics-listen :9100
Restart=always
RestartSec=3
LimitNOFILE=1048576
# Phase 1 always-on: SIGTERM only the agent PID. The default
# control-group mode would also SIGTERM every firecracker child
# concurrently, which kills the FC API socket the agent needs to
# Pause+Snapshot for hibernation. KillMode=mixed lets the agent
# orchestrate FC shutdown itself (Hibernate -> Stop). On budget
# overrun systemd still SIGKILLs the whole cgroup as a safety net.
KillMode=mixed
# Hibernate budget (120s default) + 60s slack for HTTP server,
# tracer, registry teardown. Must exceed
# PANDASTACK_HIBERNATE_BUDGET_SECONDS or systemd preempts us.
TimeoutStopSec=180

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now pandastack-agent || true

# ── NATID heartbeat timer ────────────────────────────────────────────────────
# Renews the NATID claim every 2 minutes. If heartbeat stops for 30+ minutes
# (e.g. VM going away), the next booting agent will evict the stale claim.
cat > /usr/local/bin/pandastack-natid-heartbeat.sh <<'HEARTBEAT'
#!/bin/bash
set -euo pipefail
AGENT_ID=$(grep '^PANDASTACK_AGENT_ID=' /etc/pandastack/env.agent | cut -d= -f2-)
DB_DSN=$(grep '^PANDASTACK_DB_DSN=' /etc/pandastack/env.agent | cut -d= -f2-)
psql "$DB_DSN" \
  -v "agent_id=$AGENT_ID" \
  -c "UPDATE agent_natid_claims SET last_heartbeat = NOW() WHERE instance_id = :'agent_id'" \
  -q 2>/dev/null || true
HEARTBEAT
chmod 0755 /usr/local/bin/pandastack-natid-heartbeat.sh

cat > /etc/systemd/system/pandastack-natid-heartbeat.service <<'HBUNIT'
[Unit]
Description=Pandastack NATID claim heartbeat
After=network-online.target pandastack-agent.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/pandastack-natid-heartbeat.sh
HBUNIT

cat > /etc/systemd/system/pandastack-natid-heartbeat.timer <<'HBTIMER'
[Unit]
Description=Pandastack NATID heartbeat every 2 minutes
Requires=pandastack-natid-heartbeat.service

[Timer]
OnBootSec=2min
OnUnitActiveSec=2min
AccuracySec=30s

[Install]
WantedBy=timers.target
HBTIMER

systemctl daemon-reload
systemctl enable --now pandastack-natid-heartbeat.timer || true

echo "agent cloud-init done at $(date -u)"
