output "alb_dns_name" {
  value       = aws_lb.edge.dns_name
  description = "ALB hostname — api.<zone> + *.<zone> CNAME here."
}

output "edge_asg" {
  value = aws_autoscaling_group.edge.name
}

output "agent_asg" {
  value = aws_autoscaling_group.agent.name
}

output "s3_bucket" {
  value = module.storage.bucket_name
}

output "node_token" {
  value     = random_password.node_token.result
  sensitive = true
}

output "rds_endpoint" {
  value       = aws_db_instance.main.address
  description = "RDS Postgres endpoint (private)."
}

output "database_url" {
  value       = aws_secretsmanager_secret_version.database_url.secret_string
  sensitive   = true
  description = "Full RDS connection string (postgresql://...)."
}

output "clickhouse_internal_ip" {
  value = aws_instance.clickhouse.private_ip
}

output "db_proxy_ip" {
  value       = aws_eip.db_proxy.public_ip
  description = "Static public IP of the db-proxy (*.db.<zone> points here)."
}

output "db_proxy_instance" {
  value = aws_instance.db_proxy.id
}
