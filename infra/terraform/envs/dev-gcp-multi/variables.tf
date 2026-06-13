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
  type    = string
  default = "e2-small"
}

variable "edge_count" {
  type        = number
  default     = 1
  description = "Minimum number of edge VMs."
}

variable "edge_max_count" {
  type        = number
  default     = 2
  description = "Maximum number of edge VMs (autoscaler ceiling)."
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
  type    = string
  default = "n2-standard-8"
}

variable "agent_count" {
  type        = number
  default     = 1
  description = "Minimum number of agent VMs (autoscaler floor)."
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
  # 400G: the agent stores a baked snapshot + based rootfs locally for every
  # public template (preseed for ~150ms launch). That is ~95G of template
  # artifacts alone; the 300G XFS loopback data volume (see
  # cloud-init/user-data-agent.sh) plus OS + headroom needs a 400G boot disk.
  default = 400
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
  type    = string
  default = "pandastack.ai"
}

# --- App / state
variable "database_url" {
  type        = string
  sensitive   = true
  description = "Supabase Postgres pgbouncer DSN (port 6543)."
}

variable "clickhouse_url" {
  type        = string
  sensitive   = true
  default     = ""
  description = "ClickHouse Cloud HTTPS URL incl. user:password@host:8443."
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

# --- GitHub App (apps feature). Each flows into Secret Manager and is fetched
# by the edge VMs at boot (see cloud-init/user-data-edge.sh). Leave blank to
# create only the empty secret container and populate later via
# `gcloud secrets versions add`. The private key must be the single-line
# `\n`-escaped PEM form so the rendered env file stays one line.
variable "github_app_id" {
  type    = string
  default = ""
}

variable "github_app_installation_id" {
  type    = string
  default = ""
}

variable "github_app_slug" {
  type    = string
  default = ""
}

variable "github_app_client_id" {
  type    = string
  default = ""
}

variable "github_app_client_secret" {
  type      = string
  sensitive = true
  default   = ""
}

variable "github_app_private_key" {
  type      = string
  sensitive = true
  default   = ""
}

variable "github_app_webhook_secret" {
  type      = string
  sensitive = true
  default   = ""
}
