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

# --- GitHub App (apps feature: connect flow + auto-deploy webhook + clone auth)
# Each maps to the identically-named env var the API reads (apps_github.go,
# github_oauth.go, github_webhook.go). Leave a value blank to create only the
# secret container (populate later with `gcloud secrets versions add`, like the
# Stripe secrets). The private key must be the single-line `\n`-escaped PEM form
# (normalizePEM in apps_github.go tolerates it) so the env file stays one line.
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
