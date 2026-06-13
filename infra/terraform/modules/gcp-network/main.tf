locals {
  cloudflare_ipv4_cidrs = [
    "173.245.48.0/20",
    "103.21.244.0/22",
    "103.22.200.0/22",
    "103.31.4.0/22",
    "141.101.64.0/18",
    "108.162.192.0/18",
    "190.93.240.0/20",
    "188.114.96.0/20",
    "197.234.240.0/22",
    "198.41.128.0/17",
    "162.158.0.0/15",
    "104.16.0.0/13",
    "104.24.0.0/14",
    "172.64.0.0/13",
    "131.0.72.0/22",
  ]

  labels = {
    project = var.project_tag
  }
}

resource "google_compute_network" "this" {
  name                    = "${var.project_tag}-net"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "public" {
  name          = "${var.project_tag}-subnet"
  ip_cidr_range = "10.20.0.0/24"
  region        = var.region
  network       = google_compute_network.this.self_link
}

resource "google_compute_address" "this" {
  name   = "${var.project_tag}-ip"
  region = var.region
}

resource "google_compute_firewall" "ssh" {
  name    = "${var.project_tag}-allow-ssh"
  network = google_compute_network.this.self_link

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = [var.ssh_allowed_cidr]
  target_tags   = ["pandastack-host"]
}

resource "google_compute_firewall" "http_https" {
  name    = "${var.project_tag}-allow-web-cf"
  network = google_compute_network.this.self_link

  allow {
    protocol = "tcp"
    ports    = ["80", "443"]
  }

  source_ranges = local.cloudflare_ipv4_cidrs
  target_tags   = ["pandastack-host"]
}
