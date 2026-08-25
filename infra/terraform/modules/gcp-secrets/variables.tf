variable "project_tag" {
  type    = string
  default = "pandastack"
}

variable "gcp_project" {
  type = string
}

variable "database_url" {
  type      = string
  sensitive = true
  default   = ""
}

variable "clickhouse_url" {
  type      = string
  sensitive = true
  default   = ""
}

variable "supabase_jwks_url" {
  type    = string
  default = ""
}

variable "supabase_url" {
  type    = string
  default = ""
}

variable "supabase_anon_key" {
  type      = string
  sensitive = true
  default   = ""
}

variable "gcs_bucket_name" {
  type    = string
  default = ""
}
