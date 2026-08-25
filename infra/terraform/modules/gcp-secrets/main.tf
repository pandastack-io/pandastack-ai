// gcp-secrets: Secret Manager entries for the multi-node control plane.
// All values are random-generated where possible. Bindings grant read access
// to the edge + agent service accounts.

resource "random_password" "node_token" {
  length  = 48
  special = false
}

resource "google_secret_manager_secret" "node_token" {
  secret_id = "${var.project_tag}-node-token"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "node_token" {
  secret      = google_secret_manager_secret.node_token.id
  secret_data = random_password.node_token.result
}

resource "google_secret_manager_secret" "database_url" {
  secret_id = "${var.project_tag}-database-url"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "database_url" {
  count       = var.database_url == "" ? 0 : 1
  secret      = google_secret_manager_secret.database_url.id
  secret_data = var.database_url
}

resource "google_secret_manager_secret" "clickhouse_url" {
  secret_id = "${var.project_tag}-clickhouse-url"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "clickhouse_url" {
  count       = var.clickhouse_url == "" ? 0 : 1
  secret      = google_secret_manager_secret.clickhouse_url.id
  secret_data = var.clickhouse_url
}

resource "google_secret_manager_secret" "supabase_jwks_url" {
  secret_id = "${var.project_tag}-supabase-jwks-url"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "supabase_jwks_url" {
  count       = var.supabase_jwks_url == "" ? 0 : 1
  secret      = google_secret_manager_secret.supabase_jwks_url.id
  secret_data = var.supabase_jwks_url
}

# Public Supabase URL + anon key. These are non-secret (browser-exposed) but
# stored in Secret Manager so the edge VMs fetch them at boot like every other
# value — rotating Supabase becomes a secret-version bump + rolling restart,
# with no instance-template change / VM replace.
resource "google_secret_manager_secret" "supabase_url" {
  secret_id = "${var.project_tag}-supabase-url"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "supabase_url" {
  count       = var.supabase_url == "" ? 0 : 1
  secret      = google_secret_manager_secret.supabase_url.id
  secret_data = var.supabase_url
}

resource "google_secret_manager_secret" "supabase_anon_key" {
  secret_id = "${var.project_tag}-supabase-anon-key"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "supabase_anon_key" {
  count       = var.supabase_anon_key == "" ? 0 : 1
  secret      = google_secret_manager_secret.supabase_anon_key.id
  secret_data = var.supabase_anon_key
}

# Auto-generated ClickHouse admin password. The CH VM reads this via instance
# metadata to set the `default` user; api+agent read the URL (with embedded
# user:pass) to write/query.
resource "random_password" "clickhouse_password" {
  length  = 40
  special = false
}

resource "google_secret_manager_secret" "clickhouse_password" {
  secret_id = "${var.project_tag}-clickhouse-password"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "clickhouse_password" {
  secret      = google_secret_manager_secret.clickhouse_password.id
  secret_data = random_password.clickhouse_password.result
}

# Dedicated SA for the ClickHouse VM (read its own password secret + write
# logs + read schema.sql from GCS).
resource "google_service_account" "clickhouse" {
  account_id   = "${var.project_tag}-ch-sa"
  display_name = "PandaStack ClickHouse node"
}

# Edge SA reads the control-plane secrets plus the public Supabase values.
resource "google_service_account" "edge" {
  account_id   = "${var.project_tag}-edge-sa"
  display_name = "PandaStack multi-node edge"
}

# Agent SA reads shared control-plane secrets + writes to GCS snapshot bucket.
resource "google_service_account" "agent" {
  account_id   = "${var.project_tag}-agent-sa"
  display_name = "PandaStack multi-node agent"
}

locals {
  base_secrets = {
    node_token          = google_secret_manager_secret.node_token.id
    database_url        = google_secret_manager_secret.database_url.id
    clickhouse_url      = google_secret_manager_secret.clickhouse_url.id
    clickhouse_password = google_secret_manager_secret.clickhouse_password.id
    supabase_jwks_url   = google_secret_manager_secret.supabase_jwks_url.id
  }

  # Edge also reads the public Supabase URL + anon key (dashboard build/runtime).
  # Agents do NOT need these, so they stay out of base_secrets.
  edge_secrets = merge(local.base_secrets, {
    supabase_url      = google_secret_manager_secret.supabase_url.id
    supabase_anon_key = google_secret_manager_secret.supabase_anon_key.id
  })
}

resource "google_secret_manager_secret_iam_member" "edge_read" {
  for_each  = local.edge_secrets
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.edge.email}"
}

resource "google_secret_manager_secret_iam_member" "agent_read" {
  for_each  = local.base_secrets
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.agent.email}"
}

# CH VM only needs to read its own password to set the admin user.
resource "google_secret_manager_secret_iam_member" "clickhouse_read_pw" {
  secret_id = google_secret_manager_secret.clickhouse_password.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.clickhouse.email}"
}

resource "google_project_iam_member" "clickhouse_log_writer" {
  project = var.gcp_project
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.clickhouse.email}"
}

# Read schema.sql from the GCS build bucket.
resource "google_storage_bucket_iam_member" "clickhouse_gcs_read" {
  bucket = var.gcs_bucket_name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.clickhouse.email}"
}

# Both SAs need observability emission perms.
resource "google_project_iam_member" "edge_log_writer" {
  project = var.gcp_project
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.edge.email}"
}

resource "google_project_iam_member" "edge_metric_writer" {
  project = var.gcp_project
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.edge.email}"
}

resource "google_project_iam_member" "agent_log_writer" {
  project = var.gcp_project
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.agent.email}"
}

resource "google_project_iam_member" "agent_metric_writer" {
  project = var.gcp_project
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.agent.email}"
}

# Agent reads templates and writes snapshots to GCS. We rely on the env
# wiring always passing a bucket name; if not, this binding is harmless.
resource "google_storage_bucket_iam_member" "agent_gcs" {
  bucket = var.gcs_bucket_name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.agent.email}"
}
