variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "region" {
  description = "GCP region for the single-node PandaStack agent."
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone for the agent VM."
  type        = string
  default     = "us-central1-a"
}

variable "instance_type" {
  description = "GCE machine type. Nested virtualization is enabled by min_cpu_platform."
  type        = string
  default     = "n2-standard-4"
}

variable "control_plane_url" {
  description = "PandaStack API/control-plane URL the agent should connect to."
  type        = string

  validation {
    condition     = can(regex("^https?://", var.control_plane_url))
    error_message = "control_plane_url must start with http:// or https://."
  }
}

variable "node_token" {
  description = "Bootstrap token for registering the agent. Use a short-lived token; this skeleton passes it through instance metadata."
  type        = string
  sensitive   = true
  nullable    = false

  validation {
    condition     = length(var.node_token) >= 16
    error_message = "node_token should be at least 16 characters."
  }
}

variable "ssh_allowed_cidr" {
  description = "CIDR allowed to SSH to the host. Narrow this to your workstation IP."
  type        = string
  default     = "0.0.0.0/0"
}
