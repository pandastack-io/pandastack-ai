resource "random_id" "suffix" {
  byte_length = 3
}

resource "google_storage_bucket" "this" {
  name                        = "pandastack-${var.env}-${random_id.suffix.hex}"
  location                    = var.region
  uniform_bucket_level_access = true
  force_destroy               = true

  versioning {
    enabled = true
  }

  lifecycle_rule {
    condition {
      age            = 30
      matches_prefix = ["snapshots/"]
    }

    action {
      type          = "SetStorageClass"
      storage_class = "NEARLINE"
    }
  }

  lifecycle_rule {
    condition {
      age            = 90
      matches_prefix = ["snapshots/"]
    }

    action {
      type = "Delete"
    }
  }

  labels = {
    project = var.project_tag
  }
}
