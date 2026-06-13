// cloudsql.tf — Cloud SQL Postgres (smallest tier, private IP only).
//
// MIGRATION PLAN:
//   1. terraform apply → provisions instance + stores URL in secret
//   2. Dump Supabase: pg_dump <supabase_url> | gzip > schema_data.sql.gz
//   3. Restore: gunzip | psql <cloudsql_url>
//   4. Verify row counts match
//   5. Cut over: gcloud secrets versions add pandastack-database-url --data-file=<(terraform output -raw cloudsql_url)
//   6. Rolling restart: gcloud compute instance-groups managed rolling-action replace ...
//   7. Update terraform.tfvars database_url to point to Cloud SQL URL
//
// The existing pandastack-database-url secret is NOT touched here — we
// cut over manually after data validation to avoid downtime.

# ── Private services access (VPC peering required for Cloud SQL private IP) ──

resource "google_compute_global_address" "sql_private_ip_range" {
  name          = "${local.project_tag}-sql-private-ip"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = 16
  network       = module.network.vpc_id
}

resource "google_service_networking_connection" "sql_vpc_connection" {
  network                 = module.network.vpc_id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.sql_private_ip_range.name]
}

# ── Cloud SQL instance ────────────────────────────────────────────────────────

resource "random_password" "cloudsql_password" {
  length  = 32
  special = false
}

resource "google_sql_database_instance" "main" {
  name             = "${local.project_tag}-postgres"
  database_version = "POSTGRES_16"
  region           = var.gcp_region

  deletion_protection = true

  settings {
    tier              = "db-f1-micro"
    availability_type = "ZONAL"
    disk_size         = 10
    disk_type         = "PD_SSD"
    disk_autoresize   = true

    ip_configuration {
      ipv4_enabled                                  = false
      private_network                               = module.network.vpc_id
      enable_private_path_for_google_cloud_services = true
    }

    backup_configuration {
      enabled    = true
      start_time = "03:00"
    }

    database_flags {
      name  = "max_connections"
      value = "100"
    }
  }

  depends_on = [google_service_networking_connection.sql_vpc_connection]
}

resource "google_sql_database" "main" {
  name     = "pandastack"
  instance = google_sql_database_instance.main.name
}

resource "google_sql_user" "main" {
  name     = "pandastack"
  instance = google_sql_database_instance.main.name
  password = random_password.cloudsql_password.result
}

# ── Secret (separate from live database-url — cut over manually) ─────────────

resource "google_secret_manager_secret" "cloudsql_url" {
  secret_id = "${local.project_tag}-cloudsql-url"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "cloudsql_url" {
  secret      = google_secret_manager_secret.cloudsql_url.id
  secret_data = "postgresql://pandastack:${random_password.cloudsql_password.result}@${google_sql_database_instance.main.private_ip_address}:5432/pandastack?sslmode=require"
}

# Grant edge + agent SAs read access to the new cloudsql-url secret.
resource "google_secret_manager_secret_iam_member" "cloudsql_url_edge" {
  secret_id = google_secret_manager_secret.cloudsql_url.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${module.secrets.edge_sa_email}"
}

resource "google_secret_manager_secret_iam_member" "cloudsql_url_agent" {
  secret_id = google_secret_manager_secret.cloudsql_url.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${module.secrets.agent_sa_email}"
}
