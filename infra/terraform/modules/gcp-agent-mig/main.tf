// gcp-agent-mig: regional Managed Instance Group of pandastack-agent VMs.
// - n2d-standard-2 with nested virtualization (required for Firecracker).
// - No external IP (egress via Cloud NAT only).
// - Cloud-init at boot fetches secrets via gcloud + starts pandastack-agent
//   with --listen-tcp :8081, registering itself in the shared Postgres.
// - Auto-healing health check probes /healthz on :8081 (X-Node-Token bypass).

locals {
  labels = {
    project = var.project_tag
    role    = "agent"
  }
}

data "google_compute_image" "ubuntu" {
  family  = "ubuntu-2404-lts-amd64"
  project = "ubuntu-os-cloud"
}

# Optional pre-baked golden image. When set, new MIG VMs boot from a snapshot
# of an already-provisioned agent — kernel, rootfs templates, agent binary, even
# a (briefly stale) baked template-snap are already on disk, so the agent is
# serve-ready in ~60-90s instead of 3-5 min. Create with:
#   gcloud compute machine-images create pandastack-agent-golden-vN \
#     --source-instance pandastack-agent-xxxx \
#     --source-instance-zone us-central1-a
data "google_compute_image" "golden" {
  count   = var.agent_source_image_name == "" ? 0 : 1
  name    = var.agent_source_image_name
  project = var.agent_source_image_project == "" ? null : var.agent_source_image_project
}

resource "google_compute_instance_template" "agent" {
  name_prefix      = "${var.project_tag}-agent-"
  machine_type     = var.machine_type
  min_cpu_platform = var.min_cpu_platform
  region           = var.region
  tags             = [var.agent_tag]

  disk {
    source_image = var.agent_source_image_name == "" ? data.google_compute_image.ubuntu.self_link : data.google_compute_image.golden[0].self_link
    auto_delete  = true
    boot         = true
    disk_size_gb = var.boot_disk_size_gb
    disk_type    = var.boot_disk_type
  }

  # Durable data disk for customer volumes + managed-DB PGDATA (the P0
  # durability gap: with a boot disk only, every MIG recreate wiped all
  # customer data). device_name surfaces in the guest as
  # /dev/disk/by-id/google-pandastack-volumes; cloud-init formats it on first
  # boot (blank ext4) and mounts it at /var/lib/pandastack/volumes. Declared
  # STATEFUL on the MIG below, so autoheal/recreate detaches and reattaches
  # the SAME per-instance disk instead of provisioning a blank one.
  disk {
    device_name  = "pandastack-volumes"
    auto_delete  = false
    boot         = false
    disk_size_gb = var.volumes_disk_size_gb
    disk_type    = var.volumes_disk_type
  }

  advanced_machine_features {
    enable_nested_virtualization = true
  }

  network_interface {
    subnetwork = var.subnet_self_link
    # no access_config -> no external IP
  }

  scheduling {
    preemptible                 = var.use_preemptible
    automatic_restart           = !var.use_preemptible
    provisioning_model          = var.use_preemptible ? "SPOT" : "STANDARD"
    instance_termination_action = var.use_preemptible ? "STOP" : null
    on_host_maintenance         = "TERMINATE"
  }

  service_account {
    email  = var.service_account_email
    scopes = ["cloud-platform"]
  }

  metadata = {
    ssh-keys               = "ubuntu:${var.ssh_pubkey}"
    enable-oslogin         = "FALSE"
    google-logging-enabled = "true"
    secret-node-token      = var.secret_node_token
    secret-database-url    = var.secret_database_url
    secret-clickhouse-url  = var.secret_clickhouse_url
    secret-jwks-url        = var.secret_supabase_jwks_url
    pandastack-region      = var.region
    pandastack-gcs-bucket  = var.gcs_bucket_name
    # Snapshot/WAL bucket (PANDASTACK_SNAPSHOT_BUCKET). Without it the agent
    # silently disables WAL archiving, db failover restore, and snapshot/fork
    # GCS replication. Defaults to the seeds bucket — its lifecycle rules only
    # match the snapshots/ prefix, so db/ WAL archives are never auto-deleted.
    pandastack-snapshot-bucket = var.snapshot_bucket_name == "" ? var.gcs_bucket_name : var.snapshot_bucket_name
    pandastack-binary-url      = var.agent_binary_url
  }

  metadata_startup_script = file("${path.module}/../../../../cloud-init/user-data-agent.sh")

  labels = local.labels

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_compute_health_check" "agent" {
  name = "${var.project_tag}-agent-hc"
  # Lenient autohealing: a running agent VM hosts live Firecracker sandboxes
  # (apps, databases). We never want a transient /healthz blip — e.g. an agent
  # binary swap (the :8081 listener is down for ~1-2s) or a brief CPU overload —
  # to trip autohealing and RECREATE the node, which would destroy those
  # sandboxes. 6 consecutive failures × 15s ≈ 90s of sustained downtime is
  # required before the MIG recreates; a genuinely dead node still self-heals in
  # ~1.5 min, but a healthy-but-busy node is safe.
  check_interval_sec  = 15
  timeout_sec         = 5
  healthy_threshold   = 2
  unhealthy_threshold = 6

  http_health_check {
    port         = 8081
    request_path = "/healthz"
  }
}

resource "google_compute_region_instance_group_manager" "agent" {
  name                      = "${var.project_tag}-agent-mig"
  region                    = var.region
  base_instance_name        = "${var.project_tag}-agent"
  distribution_policy_zones = var.zones

  version {
    instance_template = google_compute_instance_template.agent.id
  }

  target_size = var.agent_count

  named_port {
    name = "agent-api"
    port = 8081
  }

  # The volumes data disk is STATEFUL: when the MIG recreates an instance
  # (autoheal, manual recreate), the per-instance disk is detached and
  # reattached to the replacement instead of being deleted — customer volumes
  # and managed-DB PGDATA survive. delete_rule NEVER also keeps the disk if
  # the instance is deleted outright (manual cleanup required on scale-in).
  stateful_disk {
    device_name = "pandastack-volumes"
    delete_rule = "NEVER"
  }

  auto_healing_policies {
    health_check      = google_compute_health_check.agent.id
    initial_delay_sec = 900
  }

  # FROZEN ROLLOUT. Agent VMs host live Firecracker sandboxes (apps, databases),
  # so a template change must NEVER auto-recreate a running node. Updates to the
  # config of a live node go through Ansible (../../../../ansible) — a sandbox-safe
  # binary swap + `systemctl restart` (KillMode=mixed keeps the Firecracker
  # children alive). The instance template is only the BIRTH spec for brand-new
  # nodes (autoscale / autoheal / manual recreate); existing nodes are never
  # rolled to match it.
  #   - type OPPORTUNISTIC          -> the MIG never proactively replaces VMs;
  #                                    template changes are staged, not rolled.
  #   - most_disruptive_allowed_action REFRESH -> even a manual update can't go
  #                                    beyond a metadata refresh; it can never
  #                                    RESTART or REPLACE a running agent.
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

# CPU-based autoscaler: if average CPU across the MIG sustains above the
# target, GCP adds another VM (cloud-init provisions it on its own in ~90s
# once the golden image is in place).
#
# MODE OFF (deliberate): GCP rejects a stateful policy (the stateful
# pandastack-volumes data disk above) on a regional MIG whose autoscaler is in
# mode ON — they are mutually exclusive. Volume/DB durability is the P0, and
# an autoscaler that can scale-in hosts holding host-pinned customer volumes
# and managed-DB PGDATA is unsafe anyway. Scale manually via var.agent_count
# (target_size). Long-term plan: split fleet — an autoscaled ephemeral MIG for
# stateless sandboxes + a fixed-size stateful MIG for volumes/databases, with
# scheduler steering.
resource "google_compute_region_autoscaler" "agent" {
  name   = "${var.project_tag}-agent-autoscaler"
  region = var.region
  target = google_compute_region_instance_group_manager.agent.id

  autoscaling_policy {
    mode            = "OFF"
    min_replicas    = var.agent_count
    max_replicas    = var.agent_max_count
    cooldown_period = 120

    cpu_utilization {
      target = var.agent_autoscale_cpu_target
    }
  }
}
