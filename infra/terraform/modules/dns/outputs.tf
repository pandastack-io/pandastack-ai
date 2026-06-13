output "app_fqdn" {
  value = "${var.app_subdomain}.${var.zone_name}"
}

output "api_fqdn" {
  value = "${var.api_subdomain}.${var.zone_name}"
}
