variable "region" {
  description = "AWS region."
  type        = string
}

variable "ssh_allowed_cidr" {
  description = "CIDR allowed to SSH to the host."
  type        = string
}

variable "project_tag" {
  description = "Project tag value."
  type        = string
}
