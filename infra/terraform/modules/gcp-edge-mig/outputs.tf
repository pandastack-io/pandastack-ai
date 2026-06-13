output "instance_group" {
  value = google_compute_region_instance_group_manager.edge.instance_group
}

output "mig_name" {
  value = google_compute_region_instance_group_manager.edge.name
}

output "backend_service_id" {
  value = google_compute_backend_service.edge.id
}

output "lb_https_proxy" {
  value = google_compute_target_https_proxy.edge.id
}

output "managed_cert_name" {
  value = google_compute_managed_ssl_certificate.edge.name
}
