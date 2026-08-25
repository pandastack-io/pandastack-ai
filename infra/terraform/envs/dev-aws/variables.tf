# =============================================================================
# dev-aws: multi-node PandaStack stack on AWS (mirror of dev-gcp-multi).
#   - edge tier   : ASG of API+dashboard hosts behind an ALB (Cloudflare-proxied)
#   - agent tier  : ASG of Firecracker hosts (*.metal, KVM) in private subnets
#   - control DB  : RDS for PostgreSQL
#   - analytics   : single ClickHouse EC2 (private subnet)
#   - db-proxy    : single EC2 + EIP for *.db.<zone>:5432 SNI routing
#   - secrets     : AWS Secrets Manager (node token, DB/CH/Supabase/GitHub)
# =============================================================================

variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "availability_zones" {
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b"]
  description = "Two+ AZs for multi-AZ subnets (ALB + RDS subnet groups require >= 2)."

  validation {
    condition     = length(var.availability_zones) >= 2 && length(var.availability_zones) == length(distinct(var.availability_zones))
    error_message = "availability_zones must list at least 2 distinct AZs (the ALB and RDS subnet group both require >= 2)."
  }
}

variable "project_tag" {
  type    = string
  default = "pandastack-multi-aws"

  # The ALB and target-group names are "${project_tag}-edge" and AWS caps those
  # at 32 chars. Keep the prefix short so the derived names stay valid.
  validation {
    condition     = length(var.project_tag) <= 24 && can(regex("^[a-z][a-z0-9-]*[a-z0-9]$", var.project_tag))
    error_message = "project_tag must be <= 24 chars, lowercase alphanumeric/hyphen, and not start/end with a hyphen (ALB/target-group names derive from it and AWS caps them at 32)."
  }
}

variable "ssh_pubkey" {
  type        = string
  description = "SSH public key contents for the ubuntu user on all instances."
}

variable "ssh_allowed_cidr" {
  type        = string
  description = "CIDR allowed to SSH (e.g. <my-ip>/32)."
}

variable "use_spot" {
  type        = bool
  default     = false
  description = "Use Spot instances for edge/agent/db-proxy ASGs."
}

# --- Edge tier (API + dashboard) ---------------------------------------------
variable "edge_instance_type" {
  type = string
  # m6i.large (2 vCPU / 8 GiB). The edge runs pandastack-api and the dashboard
  # SSR process side by side. The API is not CPU-bound but it is *connection*-
  # bound: every sandbox create/exec/logs call is a long-lived HTTP stream held
  # open while the agent works, and the dashboard polls sandbox + database state
  # continuously. 8 GiB is the floor once Go's heap, the pgx pool to RDS and the
  # Node SSR process share a host; the 2 GiB of a t3.small leaves nothing for a
  # traffic spike, and t3 burst credits deplete under steady polling — exactly
  # the load shape this tier sees.
  default     = "m6i.large"
  description = "Edge (API + dashboard) instance type. Prefer a non-burstable family: this tier is under continuous load, so t3/t4g burst credits drain."
}

variable "edge_count" {
  type = number
  # 2, not 1: the edge is the only path to the control plane, and it sits behind
  # an ALB spanning two AZs. One instance means every deploy, AMI roll or AZ
  # event is a full API outage. Two lets the ASG replace one at a time.
  default     = 2
  description = "Desired/min number of edge instances (ASG floor). 2 = no single-instance outage during rolls."
}

variable "edge_max_count" {
  type        = number
  default     = 4
  description = "Max number of edge instances (ASG ceiling)."
}

variable "edge_boot_disk_size_gb" {
  type = number
  # 100G. The AMI default (8G) fills up: the edge bundle carries the API binary
  # plus the prebuilt dashboard, and journald keeps the API access/req logs that
  # are the first thing an operator reads during an incident. 100G is cheap
  # (~$8/mo) insurance against a disk-full edge, which fails closed for the
  # entire control plane.
  default     = 100
  description = "Edge root volume size (GB)."
}

variable "edge_binary_url" {
  type        = string
  default     = ""
  description = "Bundle URL with pandastack-api + dashboard + caddy config. Empty = baked into AMI."
}

# --- Agent tier (Firecracker hosts) ------------------------------------------
variable "agent_instance_type" {
  type        = string
  default     = "c5n.metal"
  description = "Must be a *.metal flavor for Firecracker (needs bare-metal KVM)."
}

variable "agent_count" {
  type = number
  # 2, not 1. Three reasons, all structural:
  #  1. Managed-database PGDATA is host-pinned to the agent's durable data
  #     volume. With a single agent there is nowhere to fail a database over to,
  #     so "restore from WAL to latest" has no target and the fleet has a hard
  #     single point of data-plane failure.
  #  2. Every agent pre-seeds a baked memory snapshot + base rootfs for each
  #     first-party template at boot (cloud-init/user-data-agent.sh runs
  #     `pandastack-agent seed-sync`). A cold agent is not useful until that
  #     completes, so scaling from 1 to 2 on demand adds seed-sync latency to
  #     the first create that needed the capacity.
  #  3. Draining a host for a kernel/Firecracker upgrade requires somewhere to
  #     put its sandboxes.
  default     = 2
  description = "Desired/min number of agent instances (ASG floor). 2 is the production floor: managed-DB failover needs a second host, and a cold agent is not schedulable until seed-sync finishes."
}

variable "agent_max_count" {
  type        = number
  default     = 8
  description = "Max number of agent instances (ASG ceiling)."
}

variable "agent_boot_disk_size_gb" {
  type = number
  # 1000G. This volume is not scratch — it is simultaneously the template cache,
  # the streaming chunk cache and customer data:
  #
  #  * Template preseed. seed-sync pulls a baked memory snapshot + base rootfs
  #    for EVERY first-party template (base, code-interpreter, agent,
  #    postgres-16) so a create restores in ~150ms instead of cold-booting. A
  #    baked snapshot is the guest's full RAM image, so a single 4 GiB template
  #    costs ~4 GiB of vm.mem plus its rootfs.
  #  * Streaming chunk cache. UFFD memory streaming and the NBD rootfs stream
  #    keep a per-host content-addressed chunk cache on local disk
  #    (agent/internal/memstream/sharedcache.go,
  #    agent/internal/diskstream/sharedcache.go). It is shared across sandboxes
  #    and grows with the working set of every template + user image the host
  #    has touched. Undersize it and the cache thrashes, turning a ~150ms
  #    restore back into a multi-second object-store fetch.
  #  * Durable customer data. Managed-Postgres PGDATA is host-pinned to this
  #    volume (the XFS loopback data volume created in
  #    cloud-init/user-data-agent.sh). Running out of space here is a customer
  #    data incident, not a cache miss.
  #  * Per-sandbox dm-snapshot CoW deltas for everything currently running.
  #
  # 400G was a single-tenant dev figure that leaves ~100G once templates and the
  # chunk cache are warm. 1000G is the smallest number that holds the warm
  # working set of a 72-vCPU host with real tenants on it.
  default     = 1000
  description = "Agent root volume size (GB). Holds template preseeds, the UFFD/NBD chunk cache, and host-pinned managed-database PGDATA."
}

variable "agent_boot_disk_iops" {
  type = number
  # gp3 ships 3000 IOPS / 125 MB/s regardless of size. 125 MB/s is a hard
  # ceiling on exactly the paths that make PandaStack fast: UFFD page faults
  # served from the chunk cache, NBD rootfs reads, and Postgres WAL fsyncs from
  # every managed database pinned to this host — all at once. Raise both.
  default     = 12000
  description = "Provisioned IOPS for the agent root volume."
}

variable "agent_boot_disk_throughput_mbps" {
  type        = number
  default     = 500
  description = "Provisioned throughput (MB/s) for the agent root volume. The gp3 default of 125 MB/s bottlenecks UFFD memory streaming, NBD rootfs reads and managed-Postgres WAL simultaneously."
}

variable "agent_binary_url" {
  type        = string
  default     = ""
  description = "HTTPS/s3:// URL where the pandastack-agent binary lives. Empty = baked into AMI."
}

# --- Cloudflare --------------------------------------------------------------
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
  description = "Public DNS zone this deployment serves. The db-proxy derives its *.db.<zone> SNI suffix from it — set it to YOUR zone; there is deliberately no default."
}

# --- App / state -------------------------------------------------------------
variable "clickhouse_url" {
  type        = string
  sensitive   = true
  default     = ""
  description = "Override ClickHouse URL. Empty = auto-filled from the provisioned ClickHouse EC2 internal IP."
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

variable "db_proxy_binary_url" {
  type        = string
  default     = ""
  description = "HTTPS/s3:// URL to the pandastack-db-proxy binary. Empty = baked into the AMI."
}

variable "dashboard_bucket" {
  type    = string
  default = ""
}

# --- RDS (control-plane Postgres) --------------------------------------------
variable "rds_instance_class" {
  type = string
  # db.m6g.xlarge (4 vCPU / 16 GiB). This database is the fleet's brain: it
  # holds sandbox and managed-database records, node registration + heartbeats,
  # and network-slot allocation. Every agent heartbeat and every scheduling
  # decision is a write, so write volume scales with fleet size, not with user
  # traffic. db.t4g.micro (2 vCPU / 1 GiB, burstable) has a working set smaller
  # than the indexes and stalls the whole control plane the moment CPU credits
  # run out.
  default     = "db.m6g.xlarge"
  description = "RDS instance class for the control-plane database. Non-burstable: heartbeat + slot-allocation writes are continuous."
}

variable "rds_allocated_storage_gb" {
  type = number
  # 200G. The row data itself is small, but Postgres also needs room for WAL,
  # autovacuum churn on the hot heartbeat/slot tables, and the 7-day automated
  # backup window. max_allocated_storage (rds.tf) autoscales to 5x this, so 200G
  # is a floor, not a cap.
  default     = 200
  description = "Initial RDS storage (GB). rds.tf autoscales up to 5x this."
}

variable "rds_multi_az" {
  type = bool
  # true. If the control-plane database is unreachable, nothing schedules: no
  # creates, no wakes, no node registration. Already-running sandboxes keep
  # serving traffic, but the fleet is frozen. Multi-AZ roughly doubles the RDS
  # line item and is the single most defensible spend in this stack — a
  # single-AZ RDS turns a routine AZ event into a full control-plane outage.
  # Set false only for a non-production trial.
  default     = true
  description = "Run the control-plane database Multi-AZ. Doubles RDS cost; removes the single-AZ control-plane outage."
}

variable "rds_engine_version" {
  type    = string
  default = "16"
}

variable "rds_deletion_protection" {
  type    = bool
  default = true
}

# --- ClickHouse (analytics) --------------------------------------------------
variable "clickhouse_instance_type" {
  type = string
  # m6i.xlarge (4 vCPU / 16 GiB). ClickHouse stores the boot-event stream and
  # per-sandbox metrics that back the dashboard's charts. Ingest is continuous
  # (every sandbox emits metrics for its whole lifetime, every create emits a
  # boot event), and the dashboard's range queries scan those tables on every
  # page load. ClickHouse wants RAM for its mark cache and merge buffers;
  # t3.medium's 4 GiB makes merges the bottleneck and the charts time out.
  default     = "m6i.xlarge"
  description = "ClickHouse instance type."
}

variable "clickhouse_disk_size_gb" {
  type = number
  # 1000G. Retention here is time-based, not size-based, so the disk has to hold
  # the whole window: per-sandbox metric samples across every sandbox that ran
  # in that window, plus boot events, plus the transient 2-3x space merges need
  # while rewriting parts. 70G is a demo figure — it fills within days of real
  # fleet traffic and a full ClickHouse disk silently drops the dashboard's
  # history.
  default     = 1000
  description = "ClickHouse data/root volume size (GB)."
}

# --- db-proxy (customer postgres:// SNI router) ------------------------------
variable "db_proxy_instance_type" {
  type = string
  # c6i.large (2 vCPU / 4 GiB). The proxy does no query work — it reads the SNI
  # name, picks the owning agent, and io.Copy()s bytes. But it terminates TLS
  # for EVERY customer database connection in the fleet and holds each one open
  # for the session's lifetime, so it is bound by network throughput, packets
  # per second and file descriptors. t3.small's burst credits are the wrong
  # shape for that; c6i.large gives a non-burstable core pair and real network
  # bandwidth for the same order of cost.
  default     = "c6i.large"
  description = "db-proxy instance type. Compute-light but connection- and bandwidth-heavy: it terminates TLS for every managed-database connection."
}

variable "db_proxy_disk_size_gb" {
  type = number
  # 100G. Stateless, but it keeps its Let's Encrypt material and the connection
  # logs that are the only record of who connected to which database and when —
  # the first thing anyone reads when a customer reports a connection problem.
  default     = 100
  description = "db-proxy root volume size (GB)."
}

variable "acme_email" {
  type        = string
  description = "Contact address for the Let's Encrypt account the db-proxy registers for its *.db.<zone> wildcard cert. Receives expiry warnings — use your own, there is deliberately no default."
}
