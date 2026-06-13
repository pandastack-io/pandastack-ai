// clickhouse.tf — single-node ClickHouse on a dedicated GCP VM in the agents
// subnet. Auto-deploys via cloud-init/user-data-clickhouse.sh which mounts a
// 50GB persistent disk, runs the official CH container, and applies the
// pandastack schema from GCS.
//
// Why a dedicated VM:
//   - 2GB e2-small edges already run the Go API; CH starvation could stall
//     api.pandastack.ai. The dedicated VM isolates analytics writes/queries.
//   - No public IP — only reachable from edge+agent tags via internal 8123.
//   - On the cloud-nat router so it can pull docker images.
//
// Reads/writes flow:
//   - agent VMs: write sandbox_metrics, sandbox_events, boot_events.
//   - edge VMs (api): write http_requests + read all tables for /v1/metrics/*.

locals {
  ch_vm_name = "${local.project_tag}-clickhouse-1"
  ch_zone    = var.gcp_zone
}

# Persistent disk for /var/lib/clickhouse. Survives instance recreation.
resource "google_compute_disk" "clickhouse_data" {
  name = "${local.project_tag}-clickhouse-data"
  type = "pd-ssd"
  size = 50
  zone = local.ch_zone
  labels = {
    project = local.project_tag
    role    = "clickhouse"
  }
}

# Upload schema.sql to the GCS build bucket so the VM can fetch it on boot.
resource "google_storage_bucket_object" "clickhouse_schema" {
  name   = "clickhouse/schema.sql"
  bucket = module.storage.bucket_name
  source = "${path.module}/../../../../agent/internal/clickhouse/schema.sql"
}

resource "google_compute_instance" "clickhouse" {
  name         = local.ch_vm_name
  machine_type = "e2-medium"
  zone         = local.ch_zone
  tags         = [module.network.agent_tag, "${local.project_tag}-clickhouse"]

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
      size  = 20
      type  = "pd-balanced"
    }
  }

  # Mount the persistent data disk with stable device name "clickhouse-data"
  # so cloud-init can find it at /dev/disk/by-id/google-clickhouse-data.
  attached_disk {
    source      = google_compute_disk.clickhouse_data.id
    device_name = "clickhouse-data"
    mode        = "READ_WRITE"
  }

  network_interface {
    subnetwork = module.network.agents_subnet_self_link
    # No access_config block → no public IP. Egress via Cloud NAT.
  }

  service_account {
    email  = module.secrets.clickhouse_sa_email
    scopes = ["cloud-platform"]
  }

  metadata = {
    "ssh-keys"              = "ubuntu:${var.ssh_pubkey}"
    "clickhouse-password"   = module.secrets.clickhouse_password
    "pandastack-schema-url" = "gs://${module.storage.bucket_name}/clickhouse/schema.sql"
    "enable-oslogin"        = "FALSE"
  }

  metadata_startup_script = file("${path.module}/../../../../cloud-init/user-data-clickhouse.sh")

  allow_stopping_for_update = true

  labels = {
    project = local.project_tag
    role    = "clickhouse"
  }

  depends_on = [google_storage_bucket_object.clickhouse_schema]
}

# Firewall: only edge + agent tags may reach CH on 8123 (HTTP). The CH VM
# carries the agent_tag so intra-tag traffic is already allowed, but be
# explicit for the edge.
resource "google_compute_firewall" "edge_to_clickhouse" {
  name    = "${local.project_tag}-edge-to-clickhouse"
  network = module.network.vpc_self_link

  allow {
    protocol = "tcp"
    ports    = ["8123", "9000"]
  }
  source_tags = [module.network.edge_tag]
  target_tags = ["${local.project_tag}-clickhouse"]
}

# Auto-populate the existing clickhouse_url secret with the internal URL.
# Agents/edge already read this secret to construct their CH client. This
# replaces any manually-set value with the dedicated VM's internal IP.
resource "google_secret_manager_secret_version" "clickhouse_url_auto" {
  secret      = "projects/${var.gcp_project}/secrets/${module.secrets.secret_name_clickhouse_url}"
  secret_data = "http://default:${module.secrets.clickhouse_password}@${google_compute_instance.clickhouse.network_interface[0].network_ip}:8123/?database=pandastack"

  depends_on = [google_compute_instance.clickhouse]
}

output "clickhouse_internal_ip" {
  value = google_compute_instance.clickhouse.network_interface[0].network_ip
}

output "clickhouse_vm_name" {
  value = google_compute_instance.clickhouse.name
}
