variable "project_tag" {
  type    = string
  default = "pandastack"
}

variable "region" {
  type = string
}

variable "zones" {
  type    = list(string)
  default = ["us-central1-a", "us-central1-b"]
}

variable "min_cpu_platform" {
  type    = string
  default = "Intel Cascade Lake"
}

variable "machine_type" {
  type = string
  # A Firecracker host carries a fixed per-host cost — the template preseed and
  # the shared UFFD/NBD chunk cache — that only amortises across many sandboxes.
  # A 2-vCPU agent pays that cost for almost no capacity. See the env's
  # var.agent_machine_type.
  default = "n2-standard-64"
}

variable "boot_disk_size_gb" {
  type = number
  # Template preseeds + the shared streaming chunk cache + per-sandbox CoW
  # deltas all live here. See the env's var.agent_boot_disk_size_gb.
  default = 800
}

variable "boot_disk_type" {
  type    = string
  default = "pd-ssd"
}

variable "use_preemptible" {
  type = bool
  # false. An agent holds running sandboxes and host-pinned managed-database
  # PGDATA; preemption is a data-plane event, not a cost optimisation. Opt in
  # deliberately for a throwaway trial.
  default = false
}

variable "agent_count" {
  type        = number
  default     = 2
  description = "Minimum number of agent VMs (autoscaler floor)."
}

variable "agent_max_count" {
  type        = number
  default     = 5
  description = "Maximum number of agent VMs (autoscaler ceiling)."
}

variable "agent_autoscale_cpu_target" {
  type        = number
  default     = 0.65
  description = "Target average CPU utilization (0-1) for the agent MIG autoscaler. Above this, MIG scales out."
}

variable "subnet_self_link" {
  type = string
}

variable "agent_tag" {
  type = string
}

variable "ssh_pubkey" {
  type = string
}

variable "service_account_email" {
  type = string
}

variable "secret_node_token" {
  type = string
}

variable "secret_database_url" {
  type = string
}

variable "secret_clickhouse_url" {
  type = string
}

variable "secret_supabase_jwks_url" {
  type = string
}

variable "gcs_bucket_name" {
  type = string
}

variable "snapshot_bucket_name" {
  type        = string
  default     = ""
  description = "GCS bucket for user snapshots/forks and the managed-DB WAL archive (PANDASTACK_SNAPSHOT_BUCKET). Empty = reuse gcs_bucket_name (its lifecycle rules already scope to the snapshots/ prefix; db/ WAL archives are untouched)."
}

variable "volumes_disk_size_gb" {
  type = number
  # Customer data, not cache: running out here is a data incident. See the env's
  # var.agent_volumes_disk_size_gb.
  default     = 500
  description = "Size of the per-agent STATEFUL data disk mounted at /var/lib/pandastack/volumes (customer volumes + managed-DB PGDATA). Survives MIG autoheal/recreate; grow online with `gcloud compute disks resize` + resize2fs."
}

variable "volumes_disk_type" {
  type = string
  # pd-ssd: managed-Postgres WAL fsyncs from every database pinned to this host
  # land on this disk concurrently. pd-balanced's IOPS/GB is tuned for sparse
  # image files, not for many independent WAL streams.
  default     = "pd-ssd"
  description = "Disk type for the volumes data disk."
}

variable "agent_binary_url" {
  type    = string
  default = ""
}

variable "agent_source_image_name" {
  type        = string
  default     = ""
  description = "Name of a custom GCE image (golden image) to use as the agent boot disk. Empty = use stock Ubuntu 24.04."
}

variable "agent_source_image_project" {
  type        = string
  default     = ""
  description = "Project hosting agent_source_image_name. Empty = current project."
}

