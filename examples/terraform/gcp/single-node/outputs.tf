output "agent_ip" {
  description = "Public IP address of the PandaStack agent host."
  value       = google_compute_instance.agent.network_interface[0].access_config[0].nat_ip
}

output "ssh_command" {
  description = "SSH command for the agent host. Requires OS Login or project SSH metadata."
  value       = "gcloud compute ssh pandastack-agent-single-node --zone ${var.zone}"
}
