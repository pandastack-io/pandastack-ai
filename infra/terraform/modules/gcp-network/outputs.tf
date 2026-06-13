output "network_self_link" {
  description = "VPC network self link."
  value       = google_compute_network.this.self_link
}

output "subnet_self_link" {
  description = "Subnet self link."
  value       = google_compute_subnetwork.public.self_link
}

output "external_ip_address" {
  description = "Reserved static external IP address."
  value       = google_compute_address.this.address
}

output "external_ip_name" {
  description = "Reserved static external IP resource name."
  value       = google_compute_address.this.name
}
