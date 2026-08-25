// cloudsql.tf — Cloud SQL Postgres for the control plane. Private IP only.
//
// This instance holds sandbox + managed-database records, node registration and
// heartbeats, and network-slot allocation. Every agent heartbeat and every
// scheduling decision writes here, so load scales with fleet size rather than
// with user traffic. See var.cloudsql_tier / var.cloudsql_availability_type for
// why it is sized and replicated the way it is.
//
// The secret written below is deliberately SEPARATE from the live
// <project_tag>-database-url secret: cut over manually after validating the
// data so the switch is not tied to an apply.
//
// MIGRATION (from an existing control-plane database):
//   1. terraform apply → provisions the instance + stores its URL in a secret
//   2. Dump:    pg_dump <old_url> | gzip > schema_data.sql.gz
//   3. Restore: gunzip -c schema_data.sql.gz | psql <cloudsql_url>
//   4. Verify row counts match
//   5. Cut over: gcloud secrets versions add <project_tag>-database-url \
//                  --data-file=<(terraform output -raw cloudsql_url)
//   6. Rolling restart: gcloud compute instance-groups managed rolling-action replace ...
//   7. Point terraform.tfvars database_url at the Cloud SQL URL

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
    tier              = var.cloudsql_tier
    availability_type = var.cloudsql_availability_type
    disk_size         = var.cloudsql_disk_size_gb
    disk_type         = "PD_SSD"
    disk_autoresize   = true

    ip_configuration {
      ipv4_enabled                                  = false
      private_network                               = module.network.vpc_id
      enable_private_path_for_google_cloud_services = true
    }

    backup_configuration {
      enabled                        = true
      start_time                     = "03:00"
      point_in_time_recovery_enabled = false
    }

    # Every edge VM keeps a pgx pool open, and every agent holds connections for
    # heartbeat + slot allocation. 100 is the shared-core default and is reached
    # by a handful of edge VMs alone, after which new agents fail to register.
    database_flags {
      name  = "max_connections"
      value = "500"
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
