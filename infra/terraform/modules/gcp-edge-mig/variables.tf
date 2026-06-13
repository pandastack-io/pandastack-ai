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
  type    = string
  default = "e2-small"
}

variable "use_preemptible" {
  type    = bool
  default = true
}

variable "edge_count" {
  type        = number
  default     = 1
  description = "Minimum number of edge VMs (autoscaler floor)."
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

variable "secret_stripe_env" {
  type        = map(string)
  default     = {}
  description = "Map of STRIPE_* environment variable names to Secret Manager secret IDs."
}

variable "secret_github_env" {
  type        = map(string)
  default     = {}
  description = "Map of GITHUB_APP_* environment variable names to Secret Manager secret IDs."
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
