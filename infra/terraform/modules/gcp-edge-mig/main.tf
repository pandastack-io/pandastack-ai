// gcp-edge-mig: regional MIG of e2-small edge VMs running caddy + pandastack-api
// + dashboard, fronted by a global HTTPS LB with Google-managed cert. This
// is the only public-internet surface of the multi-node cluster.
//
// LB layout:
//   - Static IPv4 (passed in via global_address_name)
//   - HTTPS proxy + managed cert for {dev,api-dev,green}.pandastack.ai
//   - URL map: default → edge backend (api+dashboard both served from :8080
//     since the existing caddy already path-splits internally)
//   - Cloud Armor: rate-limit 600 req/min per IP (basic DoS guard)
//   - HTTP→HTTPS redirect on :80 for browsers that hit naked http://
//
// The edge MIG itself is auto-healed against /healthz on :8080.

locals {
  labels = {
    project = var.project_tag
    role    = "edge"
  }
}

data "google_compute_image" "ubuntu" {
  family  = "ubuntu-2404-lts-amd64"
  project = "ubuntu-os-cloud"
}

resource "google_compute_instance_template" "edge" {
  name_prefix  = "${var.project_tag}-edge-"
  machine_type = var.machine_type
  region       = var.region
  tags         = [var.edge_tag]

  disk {
    source_image = data.google_compute_image.ubuntu.self_link
    auto_delete  = true
    boot         = true
    disk_size_gb = 30
    disk_type    = "pd-balanced"
  }

  network_interface {
    subnetwork = var.subnet_self_link
    access_config {
      # ephemeral public IP; LB still owns the customer-facing IP. The
      # ephemeral one is for outbound to Supabase/GCS bootstrap.
    }
  }

  scheduling {
    preemptible                 = var.use_preemptible
    automatic_restart           = !var.use_preemptible
    provisioning_model          = var.use_preemptible ? "SPOT" : "STANDARD"
    instance_termination_action = var.use_preemptible ? "STOP" : null
  }

  service_account {
    email  = var.service_account_email
    scopes = ["cloud-platform"]
  }

  metadata = {
    ssh-keys                    = "ubuntu:${var.ssh_pubkey}"
    enable-oslogin              = "FALSE"
    google-logging-enabled      = "true"
    secret-node-token           = var.secret_node_token
    secret-database-url         = var.secret_database_url
    secret-clickhouse-url       = var.secret_clickhouse_url
    secret-jwks-url             = var.secret_supabase_jwks_url
    secret-stripe-env           = jsonencode(var.secret_stripe_env)
    secret-github-env           = jsonencode(var.secret_github_env)
    pandastack-region           = var.region
    pandastack-binary-url       = var.edge_binary_url
    pandastack-dashboard-bucket = var.dashboard_bucket
    secret-supabase-anon        = var.secret_supabase_anon_key
    secret-supabase-url         = var.secret_supabase_url
  }

  metadata_startup_script = file("${path.module}/../../../../cloud-init/user-data-edge.sh")

  labels = local.labels

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_compute_health_check" "edge" {
  name = "${var.project_tag}-edge-hc"
  # Lenient autohealing, mirroring the agent MIG. Edge VMs front the public
  # HTTPS load balancer (caddy + api + dashboard). We never want a transient
  # /healthz blip — a brief CPU spike or a rolling restart of a service — to
  # trip autohealing and RECREATE a public-facing node. 6 consecutive failures
  # × 15s ≈ 90s of sustained downtime is required before the MIG recreates; a
  # genuinely dead node still self-heals in ~1.5 min.
  check_interval_sec  = 15
  timeout_sec         = 5
  healthy_threshold   = 2
  unhealthy_threshold = 6

  http_health_check {
    port         = 8080
    request_path = "/healthz"
  }
}

resource "google_compute_region_instance_group_manager" "edge" {
  name                      = "${var.project_tag}-edge-mig"
  region                    = var.region
  base_instance_name        = "${var.project_tag}-edge"
  distribution_policy_zones = var.zones

  version {
    instance_template = google_compute_instance_template.edge.id
  }

  target_size = var.edge_count

  named_port {
    name = "http"
    port = 8080
  }

  auto_healing_policies {
    health_check      = google_compute_health_check.edge.id
    initial_delay_sec = 900
  }

  # FROZEN ROLLOUT (mirrors the agent MIG). Edge VMs front the public HTTPS load
  # balancer, so a template change (e.g. cloud-init / edge code drift) must NEVER
  # auto-recreate a running public-facing node out from under live traffic. The
  # instance template is only the BIRTH spec for brand-new nodes (autoscale /
  # autoheal / manual recreate); existing nodes are never rolled to match it.
  # Deliberate edge rollouts are done out-of-band (push-based config, or an
  # explicit `gcloud compute instance-groups managed rolling-action replace`).
  #   - type OPPORTUNISTIC          -> the MIG never proactively replaces VMs;
  #                                    template changes are staged, not rolled.
  #   - most_disruptive_allowed_action REFRESH -> even a manual update can't go
  #                                    beyond a metadata refresh; it can never
  #                                    RESTART or REPLACE a running edge node.
  #   - instance_redistribution_type NONE -> a regional MIG won't proactively
  #                                    rebalance zones by deleting/recreating VMs.
  # NB: with minimal_action REFRESH the update is in-place (no surge VM), so GCP
  # requires max_surge_fixed = 0 and max_unavailable_fixed > 0. These bounds only
  # gate a *manual* opportunistic refresh; type OPPORTUNISTIC means nothing rolls
  # on its own. A REFRESH is metadata-only — it does not restart or replace the VM.
  update_policy {
    type                           = "OPPORTUNISTIC"
    minimal_action                 = "REFRESH"
    most_disruptive_allowed_action = "REFRESH"
    instance_redistribution_type   = "NONE"
    max_surge_fixed                = 0
    max_unavailable_fixed          = length(var.zones)
  }

  lifecycle {
    ignore_changes = [target_size]
  }
}

# ---- Autoscaler ------------------------------------------------------------
# CPU-based autoscaler for edge VMs. Scales between edge_count (min) and
# edge_max_count, adding capacity when sustained CPU exceeds the target.
resource "google_compute_region_autoscaler" "edge" {
  name   = "${var.project_tag}-edge-autoscaler"
  region = var.region
  target = google_compute_region_instance_group_manager.edge.id

  autoscaling_policy {
    min_replicas    = var.edge_count
    max_replicas    = var.edge_max_count
    cooldown_period = 60

    cpu_utilization {
      target = var.edge_autoscale_cpu_target
    }
  }
}

# ---- Load Balancer ---------------------------------------------------------

resource "google_compute_backend_service" "edge" {
  name                  = "${var.project_tag}-edge-backend"
  protocol              = "HTTP"
  port_name             = "http"
  timeout_sec           = 300
  load_balancing_scheme = "EXTERNAL_MANAGED"
  health_checks         = [google_compute_health_check.edge.id]
  enable_cdn            = false

  backend {
    group           = google_compute_region_instance_group_manager.edge.instance_group
    balancing_mode  = "UTILIZATION"
    capacity_scaler = 1.0
  }

  log_config {
    enable      = true
    sample_rate = 0.5
  }

  security_policy = google_compute_security_policy.edge.id
}

resource "google_compute_security_policy" "edge" {
  name = "${var.project_tag}-armor"

  rule {
    action   = "rate_based_ban"
    priority = 1000
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
    rate_limit_options {
      conform_action = "allow"
      exceed_action  = "deny(429)"
      enforce_on_key = "IP"
      rate_limit_threshold {
        count        = 600
        interval_sec = 60
      }
      ban_duration_sec = 300
    }
    description = "rate limit per IP"
  }

  rule {
    action   = "allow"
    priority = 2147483647
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
    description = "default allow"
  }
}

resource "google_compute_url_map" "edge" {
  name            = "${var.project_tag}-urlmap"
  default_service = google_compute_backend_service.edge.id
}

resource "google_compute_managed_ssl_certificate" "edge" {
  name = "${var.project_tag}-cert"

  managed {
    domains = var.lb_domains
  }
}

resource "google_compute_target_https_proxy" "edge" {
  name             = "${var.project_tag}-https-proxy"
  url_map          = google_compute_url_map.edge.id
  ssl_certificates = [google_compute_managed_ssl_certificate.edge.id]
}

resource "google_compute_global_forwarding_rule" "https" {
  name                  = "${var.project_tag}-https-fr"
  ip_address            = var.lb_ip_address
  port_range            = "443"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  target                = google_compute_target_https_proxy.edge.id
}

# HTTP -> HTTPS redirect.
resource "google_compute_url_map" "http_redirect" {
  name            = "${var.project_tag}-http-redirect"
  default_service = google_compute_backend_service.edge.id
}

resource "google_compute_target_http_proxy" "http" {
  name    = "${var.project_tag}-http-proxy"
  url_map = google_compute_url_map.http_redirect.id
}

resource "google_compute_global_forwarding_rule" "http" {
  name                  = "${var.project_tag}-http-fr"
  ip_address            = var.lb_ip_address
  port_range            = "80"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  target                = google_compute_target_http_proxy.http.id
}
