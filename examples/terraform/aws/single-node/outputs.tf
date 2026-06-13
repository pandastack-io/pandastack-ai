output "agent_ip" {
  description = "Public IP address of the PandaStack agent host."
  value       = aws_instance.agent.public_ip
}

output "ssh_command" {
  description = "SSH command for the agent host."
  value       = "ssh ubuntu@${aws_instance.agent.public_ip}"
}
