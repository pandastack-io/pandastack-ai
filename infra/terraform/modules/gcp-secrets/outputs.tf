output "node_token" {
  value     = random_password.node_token.result
  sensitive = true
}

output "edge_sa_email" {
  value = google_service_account.edge.email
}

output "agent_sa_email" {
  value = google_service_account.agent.email
}

output "secret_name_node_token" {
  value = google_secret_manager_secret.node_token.secret_id
}

output "secret_name_database_url" {
  value = google_secret_manager_secret.database_url.secret_id
}

output "secret_name_clickhouse_url" {
  value = google_secret_manager_secret.clickhouse_url.secret_id
}

output "secret_name_clickhouse_password" {
  value = google_secret_manager_secret.clickhouse_password.secret_id
}

output "clickhouse_password" {
  value     = random_password.clickhouse_password.result
  sensitive = true
}

output "clickhouse_sa_email" {
  value = google_service_account.clickhouse.email
}

output "secret_name_supabase_jwks_url" {
  value = google_secret_manager_secret.supabase_jwks_url.secret_id
}

output "secret_name_supabase_url" {
  value = google_secret_manager_secret.supabase_url.secret_id
}

output "secret_name_supabase_anon_key" {
  value = google_secret_manager_secret.supabase_anon_key.secret_id
}

output "secret_name_stripe_env" {
  value = {
    for name, secret in google_secret_manager_secret.stripe : name => secret.secret_id
  }
}

output "secret_name_github_env" {
  value = {
    for name, secret in google_secret_manager_secret.github : name => secret.secret_id
  }
}
