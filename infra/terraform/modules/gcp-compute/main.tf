locals {
  labels = {
    project = var.project_tag
  }
}

resource "google_service_account" "this" {
  account_id   = "pandastack-host-sa"
  display_name = "Pandastack dev host service account"
}

resource "google_storage_bucket_iam_member" "object_admin" {
  bucket = var.gcs_bucket_name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.this.email}"
}

resource "google_compute_instance" "this" {
  name         = "pandastack-host"
  machine_type = var.machine_type
  zone         = var.zone
  tags         = ["pandastack-host"]

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
      size  = var.boot_disk_size_gb
      type  = var.boot_disk_type
    }
  }

  advanced_machine_features {
    enable_nested_virtualization = true
  }

  network_interface {
    subnetwork = var.subnet_self_link

    access_config {
      nat_ip = var.external_ip
    }
  }

  scheduling {
    preemptible                 = var.use_preemptible
    automatic_restart           = !var.use_preemptible
    provisioning_model          = var.use_preemptible ? "SPOT" : "STANDARD"
    instance_termination_action = var.use_preemptible ? "STOP" : null
  }

  service_account {
    email  = google_service_account.this.email
    scopes = ["cloud-platform"]
  }

  metadata = {
    ssh-keys       = "ubuntu:${var.ssh_pubkey}"
    enable-oslogin = "FALSE"
  }

  metadata_startup_script   = file("${path.module}/../../../../cloud-init/user-data.sh")
  allow_stopping_for_update = true
  labels                    = local.labels

  depends_on = [google_storage_bucket_iam_member.object_admin]
}
