variable "region" {
  description = "GCP region for regional network resources."
  type        = string
}

variable "ssh_allowed_cidr" {
  description = "CIDR allowed to SSH to the host."
  type        = string
}

variable "project_tag" {
  description = "Project tag/label value."
  type        = string
}
