variable "region" {
  description = "GCP region for regional dependencies."
  type        = string
}

variable "zone" {
  description = "GCP zone for the VM."
  type        = string
}

variable "machine_type" {
  description = "Compute Engine machine type."
  type        = string
}

variable "boot_disk_size_gb" {
  description = "Boot disk size in GiB."
  type        = number
}

variable "boot_disk_type" {
  description = "Boot disk type."
  type        = string
}

variable "use_preemptible" {
  description = "Use Spot/preemptible provisioning for the VM."
  type        = bool
}

variable "ssh_pubkey" {
  description = "SSH public key contents for ubuntu@host access."
  type        = string
}

variable "subnet_self_link" {
  description = "Subnet self link for the VM network interface."
  type        = string
}

variable "external_ip" {
  description = "Static external IP address to attach to the VM."
  type        = string
}

variable "gcs_bucket_name" {
  description = "GCS bucket name for tight storage IAM binding."
  type        = string
}

variable "project_tag" {
  description = "Project label value."
  type        = string
}
