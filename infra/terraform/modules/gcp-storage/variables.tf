variable "env" {
  description = "Environment name used in bucket naming."
  type        = string
}

variable "region" {
  description = "GCP region for the bucket."
  type        = string
}

variable "project_tag" {
  description = "Project label value."
  type        = string
}
