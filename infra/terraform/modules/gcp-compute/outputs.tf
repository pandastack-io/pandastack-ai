output "instance_name" {
  description = "Compute Engine instance name."
  value       = google_compute_instance.this.name
}

output "internal_ip" {
  description = "Internal IP address."
  value       = google_compute_instance.this.network_interface[0].network_ip
}

output "external_ip" {
  description = "External NAT IP address."
  value       = google_compute_instance.this.network_interface[0].access_config[0].nat_ip
}

output "service_account_email" {
  description = "Service account email attached to the VM."
  value       = google_service_account.this.email
}
