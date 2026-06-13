output "instance_group" {
  value = google_compute_region_instance_group_manager.agent.instance_group
}

output "mig_name" {
  value = google_compute_region_instance_group_manager.agent.name
}

output "template_id" {
  value = google_compute_instance_template.agent.id
}
