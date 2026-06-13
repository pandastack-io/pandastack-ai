output "lb_ip" {
  value = module.network.lb_ip_address
}

output "edge_mig" {
  value = module.edge.mig_name
}

output "agent_mig" {
  value = module.agents.mig_name
}

output "gcs_bucket" {
  value = module.storage.bucket_name
}

output "node_token" {
  value     = module.secrets.node_token
  sensitive = true
}

output "managed_cert_name" {
  value = module.edge.managed_cert_name
}

output "green_fqdn" {
  value = "green.${var.cloudflare_zone_name}"
}

output "cloudsql_private_ip" {
  value       = google_sql_database_instance.main.private_ip_address
  description = "Cloud SQL private IP — use this in the VPN/internal network only"
}

output "cloudsql_url" {
  value       = google_secret_manager_secret_version.cloudsql_url.secret_data
  sensitive   = true
  description = "Full Cloud SQL connection string (postgresql://...)"
}

output "db_proxy_ip" {
  value       = google_compute_address.db_proxy.address
  description = "Static public IP of the DB Proxy VM (*.db.pandastack.ai points here)."
}

output "db_proxy_instance" {
  value       = google_compute_instance.db_proxy.name
  description = "Name of the DB Proxy GCE instance."
}
