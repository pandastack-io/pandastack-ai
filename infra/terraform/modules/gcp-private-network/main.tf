// gcp-private-network: VPC with dual subnets (public for edge, private for
// agents), Cloud Router + Cloud NAT for agent egress, and a tight firewall
// matrix:
//   - public ingress: ports 80/443 from world (LB-only via tags)
//   - ssh: from operator CIDR to edge only
//   - internal edge→agent: port 8081 from edge tag to agent tag (X-Node-Token
//     bearer is enforced at app layer)
//   - intra-agent: all (FC tap, vsock, NFS template share)
//   - egress: open (Cloud NAT pins source IP for agent→Supabase/CH/GCS)

locals {
  labels = {
    project = var.project_tag
    role    = "multinode"
  }
}

resource "google_compute_network" "vpc" {
  name                    = "${var.project_tag}-vpc"
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"
}

resource "google_compute_subnetwork" "edge" {
  name                     = "${var.project_tag}-edge-subnet"
  ip_cidr_range            = var.edge_cidr
  region                   = var.region
  network                  = google_compute_network.vpc.self_link
  private_ip_google_access = true
}

resource "google_compute_subnetwork" "agents" {
  name                     = "${var.project_tag}-agents-subnet"
  ip_cidr_range            = var.agents_cidr
  region                   = var.region
  network                  = google_compute_network.vpc.self_link
  private_ip_google_access = true
}

# Static external IP for the HTTPS LB (CF DNS points here after cutover).
resource "google_compute_global_address" "lb_ip" {
  name = "${var.project_tag}-lb-ip"
}

# Health check probes come from GCP-internal LB ranges.
resource "google_compute_firewall" "lb_to_edge" {
  name    = "${var.project_tag}-lb-to-edge"
  network = google_compute_network.vpc.self_link

  allow {
    protocol = "tcp"
    ports    = ["80", "443", "8080"]
  }
  # 35.191.0.0/16 + 130.211.0.0/22 are the GCP LB / health-check ranges.
  source_ranges = ["35.191.0.0/16", "130.211.0.0/22"]
  target_tags   = ["${var.project_tag}-edge"]
}

resource "google_compute_firewall" "ssh_edge" {
  name    = "${var.project_tag}-ssh-edge"
  network = google_compute_network.vpc.self_link

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
  source_ranges = [var.ssh_allowed_cidr]
  target_tags   = ["${var.project_tag}-edge"]
}

# IAP-tunneled SSH to agents (private VMs). Source is Google's IAP range.
resource "google_compute_firewall" "iap_ssh_agent" {
  name    = "${var.project_tag}-iap-ssh-agent"
  network = google_compute_network.vpc.self_link

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
  source_ranges = ["35.235.240.0/20"]
  target_tags   = ["${var.project_tag}-agent"]
}

# Edge → Agent on the agent API port. The bearer X-Node-Token check happens
# at app layer; this rule just restricts L3.
resource "google_compute_firewall" "edge_to_agent" {
  name    = "${var.project_tag}-edge-to-agent"
  network = google_compute_network.vpc.self_link

  allow {
    protocol = "tcp"
    ports    = ["8081", "9100"]
  }
  source_tags = ["${var.project_tag}-edge"]
  target_tags = ["${var.project_tag}-agent"]
}

# Allow Google LB / health-check probers to reach the agent /healthz on :8081.
# Without this rule, MIG auto-healing probes time out and instances get
# recreated in an infinite loop (us: 2026-05-29 outage).
resource "google_compute_firewall" "hc_to_agent" {
  name    = "${var.project_tag}-hc-to-agent"
  network = google_compute_network.vpc.self_link

  allow {
    protocol = "tcp"
    ports    = ["8081"]
  }
  source_ranges = ["35.191.0.0/16", "130.211.0.0/22"]
  target_tags   = ["${var.project_tag}-agent"]
}

# Allow agents to talk to each other (for future peer seed sharing, gossip).
resource "google_compute_firewall" "agents_internal" {
  name    = "${var.project_tag}-agents-internal"
  network = google_compute_network.vpc.self_link

  allow {
    protocol = "tcp"
    ports    = ["0-65535"]
  }
  allow {
    protocol = "udp"
    ports    = ["0-65535"]
  }
  source_tags = ["${var.project_tag}-agent"]
  target_tags = ["${var.project_tag}-agent"]
}

# Cloud NAT for agent egress (no public IPs on agent VMs).
resource "google_compute_router" "nat_router" {
  name    = "${var.project_tag}-router"
  region  = var.region
  network = google_compute_network.vpc.self_link
}

resource "google_compute_router_nat" "nat" {
  name                               = "${var.project_tag}-nat"
  router                             = google_compute_router.nat_router.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  log_config {
    enable = false
    filter = "ERRORS_ONLY"
  }
}
