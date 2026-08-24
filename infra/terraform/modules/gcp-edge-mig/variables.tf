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

variable "machine_type" {
  type = string
  # Dedicated-core: the edge is under continuous polling load, so a shared-core
  # e2 family runs out of the burst it depends on. See the env's
  # var.edge_machine_type for the full reasoning.
  default = "n2-standard-2"
}

variable "boot_disk_size_gb" {
  type = number
  # The edge bundle (API binary + prebuilt dashboard) plus journald's request
  # logs outgrow a 30G disk, and a disk-full edge fails closed for the whole
  # control plane.
  default = 100
}

variable "use_preemptible" {
  type = bool
  # false. Preemptible/Spot edge VMs mean the control-plane API can be reclaimed
  # with 30s notice. Opt in deliberately for a throwaway trial.
  default = false
}

variable "edge_count" {
  type        = number
  default     = 2
  description = "Minimum number of edge VMs (autoscaler floor). 2 = no single-VM outage during rolling updates."
}

variable "edge_max_count" {
  type        = number
  default     = 2
  description = "Maximum number of edge VMs (autoscaler ceiling)."
}

variable "edge_autoscale_cpu_target" {
  type        = number
  default     = 0.70
  description = "Target average CPU utilization (0-1) for the edge MIG autoscaler."
}

variable "subnet_self_link" {
  type = string
}

variable "edge_tag" {
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

variable "lb_ip_address" {
  type = string
}

variable "lb_domains" {
  type = list(string)
}

variable "dashboard_bucket" {
  type    = string
  default = ""
}

variable "edge_binary_url" {
  type    = string
  default = ""
}

variable "secret_supabase_anon_key" {
  type        = string
  description = "Secret Manager secret ID holding the public Supabase anon key."
}

variable "secret_supabase_url" {
  type        = string
  description = "Secret Manager secret ID holding the public Supabase URL."
}

variable "zone_name" {
  type        = string
  description = "Public DNS zone this deployment serves (e.g. example.com). cloud-init/user-data-edge.sh derives the dashboard + API URLs from it."
}
