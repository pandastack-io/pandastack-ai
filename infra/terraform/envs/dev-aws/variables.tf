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
  type    = string
  default = "t3.small"
}

variable "edge_count" {
  type        = number
  default     = 1
  description = "Desired/min number of edge instances."
}

variable "edge_max_count" {
  type        = number
  default     = 2
  description = "Max number of edge instances (ASG ceiling)."
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
  type        = number
  default     = 1
  description = "Desired/min number of agent instances (ASG floor)."
}

variable "agent_max_count" {
  type        = number
  default     = 8
  description = "Max number of agent instances (ASG ceiling)."
}

variable "agent_boot_disk_size_gb" {
  type = number
  # 400G: each agent stores a baked snapshot + base rootfs locally for every
  # public template (preseed for ~150ms launch) plus a 300G XFS loopback data
  # volume. See cloud-init/user-data-agent.sh.
  default = 400
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
  type    = string
  default = "pandastack.ai"
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
  type    = string
  default = "db.t4g.micro"
}

variable "rds_allocated_storage_gb" {
  type    = number
  default = 20
}

variable "rds_engine_version" {
  type    = string
  default = "16"
}

variable "rds_deletion_protection" {
  type    = bool
  default = true
}

# --- GitHub App (apps feature). Each flows into Secrets Manager and is fetched
# by the edge instances at boot (see cloud-init/user-data-edge.sh). Leave blank
# to create only the empty secret container and populate later. The private key
# must be the single-line `\n`-escaped PEM form so the rendered env file stays
# one line.
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
