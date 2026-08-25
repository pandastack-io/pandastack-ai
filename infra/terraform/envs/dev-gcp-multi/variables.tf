variable "gcp_project" {
  type    = string
  default = ""
}

variable "gcp_region" {
  type    = string
  default = "us-central1"
}

variable "gcp_zone" {
  type    = string
  default = "us-central1-a"
}

variable "use_preemptible" {
  type    = bool
  default = false
}

variable "ssh_pubkey" {
  type = string
}

variable "ssh_allowed_cidr" {
  type = string
}

# --- Edge tier
variable "edge_machine_type" {
  type = string
  # n2-standard-2 (2 vCPU / 8 GiB) — the GCP counterpart of the AWS tier's
  # m6i.large. The edge runs pandastack-api and the dashboard SSR process side
  # by side. It is connection-bound rather than CPU-bound: every sandbox
  # create/exec/logs call holds an HTTP stream open while the agent works, and
  # the dashboard polls sandbox + database state continuously. e2-small's 2 GiB
  # cannot hold Go's heap, the pgx pool and Node SSR at the same time, and e2
  # is a shared-core family — the wrong shape for steady load.
  default     = "n2-standard-2"
  description = "Edge (API + dashboard) machine type. Prefer a dedicated-core family: this tier is under continuous load."
}

variable "edge_count" {
  type = number
  # 2, not 1: the edge is the only path to the control plane and sits behind a
  # global HTTPS LB spanning two zones. One VM makes every rolling update or
  # zone event a full API outage.
  default     = 2
  description = "Minimum number of edge VMs (autoscaler floor). 2 = no single-VM outage during rolling updates."
}

variable "edge_max_count" {
  type        = number
  default     = 4
  description = "Maximum number of edge VMs (autoscaler ceiling)."
}

variable "edge_boot_disk_size_gb" {
  type = number
  # 100G. 30G fills: the edge bundle carries the API binary plus the prebuilt
  # dashboard, and journald keeps the API request logs an operator reads first
  # during an incident. A disk-full edge fails closed for the control plane.
  default     = 100
  description = "Edge boot disk size (GB)."
}

variable "edge_zones" {
  type    = list(string)
  default = ["us-central1-a", "us-central1-b"]
}

# --- Agent tier
variable "agent_min_cpu_platform" {
  type    = string
  default = "Intel Cascade Lake"
}

variable "agent_machine_type" {
  type = string
  # n2-standard-64 (64 vCPU / 256 GiB), the GCP counterpart of the AWS tier's
  # c5n.metal (72 vCPU / 192 GiB). GCP exposes nested VT-x on N2, so Firecracker
  # does NOT need a bare-metal flavor here the way it does on EC2 — but the host
  # still has to be big, because the economics of this design are per-host:
  # every agent pre-seeds a baked memory snapshot + base rootfs for each
  # first-party template, and the UFFD/NBD chunk cache is shared across every
  # sandbox on the host. That fixed per-host cost only amortises if the host
  # runs many sandboxes. n2-standard-8 pays the same seed + cache overhead for
  # ~8x less sandbox capacity.
  #
  # min_cpu_platform (above) must stay set: Firecracker needs the VT-x features
  # GCP only guarantees on the named platform.
  default     = "n2-standard-64"
  description = "Agent (Firecracker host) machine type. Must be a family with nested virtualization; keep min_cpu_platform pinned."
}

variable "agent_count" {
  type = number
  # 2, not 1. Same three structural reasons as the AWS env:
  #  1. Managed-database PGDATA is host-pinned to the agent's stateful volumes
  #     disk, so a single agent has no failover target.
  #  2. A freshly created agent is not schedulable until its boot-time
  #     `pandastack-agent seed-sync` finishes pulling template snapshots
  #     (cloud-init/user-data-agent.sh), so scaling from 1 on demand adds that
  #     latency to the create that needed the capacity.
  #  3. Draining a host for a kernel/Firecracker upgrade needs somewhere to put
  #     its sandboxes.
  default     = 2
  description = "Minimum number of agent VMs (autoscaler floor). 2 is the production floor: managed-DB failover needs a second host, and a cold agent is not schedulable until seed-sync finishes."
}

variable "agent_max_count" {
  type        = number
  default     = 8
  description = "Maximum number of agent VMs (autoscaler ceiling)."
}

variable "agent_zones" {
  type    = list(string)
  default = ["us-central1-a", "us-central1-b"]
}

variable "agent_boot_disk_size_gb" {
  type = number
  # 800G. On GCP the agent has TWO disks: this boot disk (cache + scratch) and a
  # separate stateful volumes disk (customer data — see agent_volumes_disk_size_gb).
  # The boot disk carries:
  #  * Template preseed. seed-sync pulls a baked memory snapshot + base rootfs
  #    for EVERY first-party template (base, code-interpreter, agent,
  #    postgres-16) so a create restores in ~150ms instead of cold-booting. A
  #    baked snapshot is the guest's whole RAM image, so template artifacts are
  #    measured in tens of GB before any user image exists.
  #  * The streaming chunk cache. UFFD memory streaming and the NBD rootfs
  #    stream keep a per-host content-addressed chunk cache on local disk
  #    (agent/internal/memstream/sharedcache.go,
  #    agent/internal/diskstream/sharedcache.go). It grows with the working set
  #    of every template and user image the host has touched. Undersize it and
  #    the cache thrashes, turning a ~150ms restore back into a multi-second
  #    object-store fetch — which is the entire product.
  #  * Per-sandbox dm-snapshot CoW deltas for everything currently running.
  #
  # 400G was a single-tenant dev figure. 800G is the smallest number that keeps
  # the cache warm on a 64-vCPU host with real tenants on it.
  default     = 800
  description = "Agent boot disk size (GB). Holds template preseeds, the UFFD/NBD chunk cache and per-sandbox CoW deltas. Customer data lives on the separate stateful volumes disk."
}

variable "agent_volumes_disk_size_gb" {
  type = number
  # 500G. This is the STATEFUL disk mounted at /var/lib/pandastack/volumes: it
  # holds customer volumes and the host-pinned PGDATA of every managed Postgres
  # on this agent. It survives MIG autoheal/recreate. Running out of space here
  # is a customer data incident, not a cache miss, so it gets its own disk with
  # its own headroom rather than sharing the boot disk's.
  default     = 500
  description = "Per-agent stateful data disk (GB) for customer volumes + managed-database PGDATA."
}

variable "agent_volumes_disk_type" {
  type = string
  # pd-ssd, not pd-balanced. Managed-Postgres WAL fsyncs land on this disk for
  # every database pinned to the host, concurrently. pd-balanced's IOPS/GB is
  # tuned for sparse image files, not for many independent WAL streams.
  default     = "pd-ssd"
  description = "Disk type for the agent stateful volumes disk."
}

# --- Cloudflare
variable "cloudflare_api_token" {
  type      = string
  sensitive = true
}

variable "cloudflare_zone_id" {
  type      = string
  sensitive = true
}

variable "cloudflare_zone_name" {
  type        = string
  description = "Public DNS zone this deployment serves. The edge derives its dashboard/API URLs and the db-proxy its *.db.<zone> SNI suffix from this — set it to YOUR zone."
}

# --- App / state
variable "database_url" {
  type        = string
  sensitive   = true
  description = "Control-plane Postgres DSN. Leave pointing at your existing control-plane database until you cut over to the Cloud SQL instance provisioned by cloudsql.tf."
}

variable "clickhouse_url" {
  type        = string
  sensitive   = true
  default     = ""
  description = "Override the ClickHouse URL (incl. user:password@host:8443). Empty = auto-filled from the ClickHouse VM provisioned by clickhouse.tf."
}

variable "supabase_jwks_url" {
  type    = string
  default = ""
}

variable "supabase_anon_key" {
  type      = string
  sensitive = true
  default   = ""
}

variable "supabase_url" {
  type    = string
  default = ""
}

variable "agent_binary_url" {
  type        = string
  default     = ""
  description = "HTTPS URL where the pandastack-agent binary lives (e.g. gs:// or signed URL). Empty = baked into image."
}

variable "edge_binary_url" {
  type        = string
  default     = ""
  description = "Bundle URL containing pandastack-api + dashboard + caddy config."
}

variable "dashboard_bucket" {
  type    = string
  default = ""
}

variable "db_proxy_binary_url" {
  type        = string
  default     = ""
  description = "HTTPS or gs:// URL to the pandastack-db-proxy binary. Empty = binary must be baked into the image."
}

# --- Cloud SQL (control-plane Postgres) --------------------------------------
variable "cloudsql_tier" {
  type = string
  # db-custom-4-16384 (4 vCPU / 16 GiB) — the counterpart of the AWS env's
  # db.m6g.xlarge. This database is the fleet's brain: sandbox and
  # managed-database records, node registration + heartbeats, network-slot
  # allocation. Every agent heartbeat and every scheduling decision is a write,
  # so write volume scales with FLEET SIZE, not with user traffic. db-f1-micro
  # is a shared-core 0.6 GiB instance — smaller than the working set of the
  # indexes it has to serve.
  default     = "db-custom-4-16384"
  description = "Cloud SQL tier for the control-plane database."
}

variable "cloudsql_disk_size_gb" {
  type = number
  # 200G. Row data is small, but Postgres also needs room for WAL, autovacuum
  # churn on the hot heartbeat/slot tables, and the retained backup window.
  # disk_autoresize is on, so this is a floor rather than a cap.
  default     = 200
  description = "Cloud SQL data disk size (GB). Autoresize is enabled, so this is a floor."
}

variable "cloudsql_availability_type" {
  type = string
  # REGIONAL (synchronous standby in a second zone) — the Cloud SQL equivalent
  # of Multi-AZ RDS. If the control-plane database is unreachable nothing
  # schedules: no creates, no wakes, no node registration. Already-running
  # sandboxes keep serving, but the fleet is frozen. REGIONAL roughly doubles
  # the Cloud SQL line item and is the most defensible spend in this stack. Use
  # "ZONAL" only for a non-production trial.
  default     = "REGIONAL"
  description = "Cloud SQL availability type. REGIONAL removes the single-zone control-plane outage at roughly 2x the cost."

  validation {
    condition     = contains(["ZONAL", "REGIONAL"], var.cloudsql_availability_type)
    error_message = "cloudsql_availability_type must be ZONAL or REGIONAL."
  }
}

# --- ClickHouse (analytics) --------------------------------------------------
variable "clickhouse_machine_type" {
  type = string
  # n2-standard-4 (4 vCPU / 16 GiB) — counterpart of the AWS env's m6i.xlarge.
  # ClickHouse stores the boot-event stream and the per-sandbox metrics behind
  # the dashboard's charts. Ingest is continuous (every sandbox emits metrics
  # for its whole lifetime; every create emits a boot event) and the dashboard's
  # range queries scan those tables on each page load. ClickHouse wants RAM for
  # its mark cache and merge buffers — e2-medium's 4 GiB makes merges the
  # bottleneck and the charts time out.
  default     = "n2-standard-4"
  description = "ClickHouse machine type."
}

variable "clickhouse_data_disk_size_gb" {
  type = number
  # 1000G. Retention is time-based, not size-based, so the disk must hold the
  # whole window: metric samples for every sandbox that ran in it, plus boot
  # events, plus the transient 2-3x headroom merges need while rewriting parts.
  # 50G is a demo figure that fills within days of real fleet traffic, and a
  # full ClickHouse disk silently drops the dashboard's history.
  default     = 1000
  description = "ClickHouse persistent data disk size (GB)."
}

# --- db-proxy (customer postgres:// SNI router) ------------------------------
variable "db_proxy_machine_type" {
  type = string
  # n2-standard-2 (2 vCPU / 8 GiB). The proxy does no query work — it reads the
  # SNI name, picks the owning agent and io.Copy()s bytes. But it terminates TLS
  # for EVERY managed-database connection in the fleet and holds each one open
  # for the session's lifetime, so it is bound by connections, packets/sec and
  # bandwidth. e2-small is a shared-core VM with a low egress cap; that shows up
  # directly as customer query latency.
  default     = "n2-standard-2"
  description = "db-proxy machine type. Compute-light but connection- and bandwidth-heavy: it terminates TLS for every managed-database connection."
}

variable "db_proxy_boot_disk_size_gb" {
  type = number
  # 100G. Stateless, but it holds its ACME/TLS material and the connection logs
  # that are the only record of who connected to which database and when.
  default     = 100
  description = "db-proxy boot disk size (GB)."
}

variable "acme_email" {
  type        = string
  description = "Contact address for the Let's Encrypt account the db-proxy registers for its *.db.<zone> wildcard cert. Receives expiry warnings — use your own, there is deliberately no default."
}
