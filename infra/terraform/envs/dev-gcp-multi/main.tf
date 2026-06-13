// dev-gcp-multi: enterprise multi-node stack served at api.pandastack.ai
// (Cloudflare-proxied). The old "green" intermediate was retired once the
// multi-node cluster was promoted to the live api.pandastack.ai DNS record.

provider "google" {
  project = var.gcp_project
  region  = var.gcp_region
  zone    = var.gcp_zone
}

provider "cloudflare" {
  api_token = var.cloudflare_api_token
}

locals {
  env         = "dev"
  project_tag = "pandastack"
  lb_domains = [
    "green.${var.cloudflare_zone_name}",
    "dev.${var.cloudflare_zone_name}",
    "api-dev.${var.cloudflare_zone_name}",
  ]
}

module "network" {
  source = "../../modules/gcp-private-network"

  project_tag      = local.project_tag
  region           = var.gcp_region
  ssh_allowed_cidr = var.ssh_allowed_cidr
}

module "storage" {
  source = "../../modules/gcp-storage"

  env         = "${local.env}-multi"
  region      = var.gcp_region
  project_tag = local.project_tag
}

module "secrets" {
  source = "../../modules/gcp-secrets"

  project_tag       = local.project_tag
  gcp_project       = var.gcp_project
  database_url      = var.database_url
  clickhouse_url    = var.clickhouse_url
  supabase_jwks_url = var.supabase_jwks_url
  supabase_url      = var.supabase_url
  supabase_anon_key = var.supabase_anon_key
  gcs_bucket_name   = module.storage.bucket_name

  github_app_id              = var.github_app_id
  github_app_installation_id = var.github_app_installation_id
  github_app_slug            = var.github_app_slug
  github_app_client_id       = var.github_app_client_id
  github_app_client_secret   = var.github_app_client_secret
  github_app_private_key     = var.github_app_private_key
  github_app_webhook_secret  = var.github_app_webhook_secret
}

module "agents" {
  source = "../../modules/gcp-agent-mig"

  project_tag              = local.project_tag
  region                   = var.gcp_region
  zones                    = var.agent_zones
  machine_type             = var.agent_machine_type
  min_cpu_platform         = var.agent_min_cpu_platform
  boot_disk_size_gb        = var.agent_boot_disk_size_gb
  boot_disk_type           = "pd-ssd"
  use_preemptible          = var.use_preemptible
  agent_count              = var.agent_count
  agent_max_count          = var.agent_max_count
  subnet_self_link         = module.network.agents_subnet_self_link
  agent_tag                = module.network.agent_tag
  ssh_pubkey               = var.ssh_pubkey
  service_account_email    = module.secrets.agent_sa_email
  secret_node_token        = module.secrets.secret_name_node_token
  secret_database_url      = module.secrets.secret_name_database_url
  secret_clickhouse_url    = module.secrets.secret_name_clickhouse_url
  secret_supabase_jwks_url = module.secrets.secret_name_supabase_jwks_url
  gcs_bucket_name          = module.storage.bucket_name
  agent_binary_url         = var.agent_binary_url
}

module "edge" {
  source = "../../modules/gcp-edge-mig"

  project_tag              = local.project_tag
  region                   = var.gcp_region
  zones                    = var.edge_zones
  machine_type             = var.edge_machine_type
  use_preemptible          = var.use_preemptible
  edge_count               = var.edge_count
  edge_max_count           = var.edge_max_count
  subnet_self_link         = module.network.edge_subnet_self_link
  edge_tag                 = module.network.edge_tag
  ssh_pubkey               = var.ssh_pubkey
  service_account_email    = module.secrets.edge_sa_email
  secret_node_token        = module.secrets.secret_name_node_token
  secret_database_url      = module.secrets.secret_name_database_url
  secret_clickhouse_url    = module.secrets.secret_name_clickhouse_url
  secret_supabase_jwks_url = module.secrets.secret_name_supabase_jwks_url
  secret_stripe_env        = module.secrets.secret_name_stripe_env
  secret_github_env        = module.secrets.secret_name_github_env
  lb_ip_address            = module.network.lb_ip_address
  lb_domains               = local.lb_domains
  dashboard_bucket         = var.dashboard_bucket
  edge_binary_url          = var.edge_binary_url
  secret_supabase_anon_key = module.secrets.secret_name_supabase_anon_key
  secret_supabase_url      = module.secrets.secret_name_supabase_url
}

# DNS for api.pandastack.ai → multi-node LB. Cloudflare-proxied (orange-cloud)
# so CF terminates TLS and talks Full(strict) to the GCP-managed cert on the
# global HTTPS load balancer. This is the live customer-facing record.
resource "cloudflare_record" "api" {
  zone_id = var.cloudflare_zone_id
  name    = "api"
  type    = "A"
  value   = module.network.lb_ip_address
  proxied = true
  ttl     = 1
}

# Wildcard for preview URLs: {port}-{sandbox_id}.pandastack.ai →
# routed by pandastack-api's preview-host middleware to the matching sandbox's
# /proxy/{port}/... path. Cloudflare Universal SSL covers single-level
# wildcards for free; multi-level (*.*) would need Advanced Cert Manager,
# which is why the middleware rejects multi-label preview hosts.
resource "cloudflare_record" "preview_wildcard" {
  zone_id = var.cloudflare_zone_id
  name    = "*"
  type    = "A"
  value   = module.network.lb_ip_address
  proxied = true
  ttl     = 1
}

# =============================================================================
# DB Proxy — native postgres:// TLS proxy with SNI routing
# Routes *.db.pandastack.ai:5432 to the correct sandbox's Postgres port.
# Must NOT be Cloudflare-proxied (CF doesn't proxy raw TCP on port 5432).
# =============================================================================

# Stable static external IP for the db-proxy VM.
resource "google_compute_address" "db_proxy" {
  name   = "${local.project_tag}-db-proxy-ip"
  region = var.gcp_region
}

# Dedicated service account for the db-proxy VM (least privilege).
resource "google_service_account" "db_proxy" {
  account_id   = "${local.project_tag}-db-proxy-sa"
  display_name = "PandaStack DB Proxy"
}

# Secret for Cloudflare API token (used by certbot DNS-01 for *.db.pandastack.ai).
resource "google_secret_manager_secret" "cloudflare_token" {
  secret_id = "${local.project_tag}-cloudflare-api-token"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "cloudflare_token" {
  secret      = google_secret_manager_secret.cloudflare_token.id
  secret_data = var.cloudflare_api_token
}

# DB proxy SA reads the three secrets it needs.
resource "google_secret_manager_secret_iam_member" "db_proxy_node_token" {
  secret_id = module.secrets.secret_name_node_token
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.db_proxy.email}"
}

resource "google_secret_manager_secret_iam_member" "db_proxy_database_url" {
  secret_id = module.secrets.secret_name_database_url
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.db_proxy.email}"
}

resource "google_secret_manager_secret_iam_member" "db_proxy_cloudflare_token" {
  secret_id = google_secret_manager_secret.cloudflare_token.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.db_proxy.email}"
}

# Logging + metrics emission.
resource "google_project_iam_member" "db_proxy_log_writer" {
  project = var.gcp_project
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.db_proxy.email}"
}

resource "google_project_iam_member" "db_proxy_metric_writer" {
  project = var.gcp_project
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.db_proxy.email}"
}

# Binary download from GCS build bucket.
resource "google_storage_bucket_iam_member" "db_proxy_gcs_read" {
  bucket = module.storage.bucket_name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.db_proxy.email}"
}

# Firewall: allow inbound :5432 (postgres) from the internet to db-proxy.
# The db-proxy VM is tagged pandastack-db-proxy.
resource "google_compute_firewall" "db_proxy_postgres" {
  name    = "${local.project_tag}-db-proxy-postgres"
  network = module.network.vpc_self_link

  allow {
    protocol = "tcp"
    ports    = ["5432"]
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["${local.project_tag}-db-proxy"]

  description = "Allow inbound postgres (TLS) from customers to the db-proxy SNI router."
}

# Allow IAP-tunnelled SSH for debugging.
resource "google_compute_firewall" "db_proxy_iap_ssh" {
  name    = "${local.project_tag}-db-proxy-iap-ssh"
  network = module.network.vpc_self_link

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["35.235.240.0/20"] # Google IAP range
  target_tags   = ["${local.project_tag}-db-proxy"]
}

# Allow db-proxy to reach agent VMs on :8081 (pg-tunnel upgrade).
# db-proxy tag → agent tag, port 8081 only.
resource "google_compute_firewall" "db_proxy_to_agent" {
  name    = "${local.project_tag}-db-proxy-to-agent"
  network = module.network.vpc_self_link

  allow {
    protocol = "tcp"
    ports    = ["8081"]
  }

  source_tags = ["${local.project_tag}-db-proxy"]
  target_tags = ["${local.project_tag}-agent"]
}

data "google_compute_image" "db_proxy_ubuntu" {
  family  = "ubuntu-2404-lts-amd64"
  project = "ubuntu-os-cloud"
}

# Single e2-small VM — the proxy is lightweight (io.Copy only, no compute).
resource "google_compute_instance" "db_proxy" {
  name         = "${local.project_tag}-db-proxy"
  machine_type = "e2-small"
  zone         = var.gcp_zone
  tags         = ["${local.project_tag}-db-proxy"]

  boot_disk {
    initialize_params {
      image = data.google_compute_image.db_proxy_ubuntu.self_link
      size  = 20
      type  = "pd-balanced"
    }
  }

  network_interface {
    subnetwork = module.network.edge_subnet_self_link
    access_config {
      nat_ip = google_compute_address.db_proxy.address
    }
  }

  service_account {
    email  = google_service_account.db_proxy.email
    scopes = ["cloud-platform"]
  }

  metadata = {
    ssh-keys                = "ubuntu:${var.ssh_pubkey}"
    enable-oslogin          = "FALSE"
    google-logging-enabled  = "true"
    pandastack-binary-url   = var.db_proxy_binary_url
    pandastack-sni-suffix   = ".db.pandastack.ai"
    secret-node-token       = module.secrets.secret_name_node_token
    secret-database-url     = module.secrets.secret_name_database_url
    secret-cloudflare-token = google_secret_manager_secret.cloudflare_token.secret_id
  }

  metadata_startup_script = file("${path.module}/../../../../cloud-init/user-data-db-proxy.sh")

  scheduling {
    # db-proxy is stateless — preemptible is fine (reconnect on eviction).
    preemptible                 = var.use_preemptible
    automatic_restart           = !var.use_preemptible
    provisioning_model          = var.use_preemptible ? "SPOT" : "STANDARD"
    instance_termination_action = var.use_preemptible ? "STOP" : null
  }

  labels = {
    project = local.project_tag
    role    = "db-proxy"
  }

  lifecycle {
    # The boot image floats (ubuntu-2404-lts-amd64 family resolves to the latest
    # published image), and the startup script changes whenever cloud-init is
    # edited. Either one would otherwise force-replace this running VM on every
    # `terraform apply`, killing live database connections. The proxy is
    # stateless, so we pin it: routine applies must never recreate or restart it.
    # To intentionally roll the VM (new binary, OS patch), taint it explicitly:
    #   terraform taint google_compute_instance.db_proxy
    ignore_changes = [
      boot_disk[0].initialize_params[0].image,
      metadata_startup_script,
    ]
  }
}

# DNS: *.db.pandastack.ai → db-proxy static IP.
# NOT proxied — Cloudflare cannot proxy raw TCP port 5432.
# Cloudflare "gray-cloud" (proxied=false) with short TTL.
resource "cloudflare_record" "db_proxy_wildcard" {
  zone_id = var.cloudflare_zone_id
  name    = "*.db"
  type    = "A"
  content = google_compute_address.db_proxy.address
  proxied = false
  ttl     = 60
}

# Also add the apex db.pandastack.ai record so the cert SAN validates.
resource "cloudflare_record" "db_proxy_apex" {
  zone_id = var.cloudflare_zone_id
  name    = "db"
  type    = "A"
  content = google_compute_address.db_proxy.address
  proxied = false
  ttl     = 60
}
