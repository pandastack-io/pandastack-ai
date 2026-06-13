output "vpc_self_link" {
  value = google_compute_network.vpc.self_link
}

output "vpc_id" {
  value = google_compute_network.vpc.id
}

output "edge_subnet_self_link" {
  value = google_compute_subnetwork.edge.self_link
}

output "agents_subnet_self_link" {
  value = google_compute_subnetwork.agents.self_link
}

output "edge_tag" {
  value = "${var.project_tag}-edge"
}

output "agent_tag" {
  value = "${var.project_tag}-agent"
}

output "lb_ip_address" {
  value = google_compute_global_address.lb_ip.address
}

output "lb_ip_name" {
  value = google_compute_global_address.lb_ip.name
}
