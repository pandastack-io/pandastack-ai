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
  type    = string
  default = "n2d-standard-2"
}

variable "boot_disk_size_gb" {
  type    = number
  default = 100
}

variable "boot_disk_type" {
  type    = string
  default = "pd-ssd"
}

variable "use_preemptible" {
  type    = bool
  default = true
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
  type        = number
  default     = 200
  description = "Size of the per-agent STATEFUL data disk mounted at /var/lib/pandastack/volumes (customer volumes + managed-DB PGDATA). Survives MIG autoheal/recreate; grow online with `gcloud compute disks resize` + resize2fs."
}

variable "volumes_disk_type" {
  type        = string
  default     = "pd-balanced"
  description = "Disk type for the volumes data disk. pd-balanced: ~$0.10/GB-month, enough IOPS for sparse ext4 volume images."
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

