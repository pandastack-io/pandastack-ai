variable "project_tag" {
  type    = string
  default = "pandastack"
}

variable "region" {
  type = string
}

variable "edge_cidr" {
  type    = string
  default = "10.30.0.0/24"
}

variable "agents_cidr" {
  type    = string
  default = "10.30.1.0/24"
}

variable "ssh_allowed_cidr" {
  type = string
}
