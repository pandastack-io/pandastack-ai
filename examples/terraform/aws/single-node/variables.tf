variable "region" {
  description = "AWS region for the single-node PandaStack agent."
  type        = string
  default     = "us-east-1"

  validation {
    condition     = can(regex("^[a-z]{2}-[a-z]+-[0-9]+$", var.region))
    error_message = "region must look like an AWS region, for example us-east-1."
  }
}

variable "instance_type" {
  description = "Bare-metal EC2 instance type. Firecracker requires hardware virtualization; c7i.metal or c6i.metal are good starting points."
  type        = string
  default     = "c7i.metal"

  validation {
    condition     = can(regex("\\.metal$", var.instance_type))
    error_message = "instance_type must be a bare-metal instance type ending in .metal."
  }
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
  description = "Bootstrap token for registering the agent. Use a short-lived token; this skeleton passes it through user data."
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

variable "ssh_key_name" {
  description = "Existing EC2 key pair name for SSH access. Leave null to disable SSH key attachment."
  type        = string
  default     = null
}
