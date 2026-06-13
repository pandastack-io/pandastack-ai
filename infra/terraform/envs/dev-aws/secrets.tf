// secrets.tf — AWS Secrets Manager (mirror of modules/gcp-secrets).
//
// One secret per value the edge/agent instances fetch at boot. The node role
// (main.tf) is scoped to "${local.name}-*" so every secret here is readable by
// edge + agent instances. GitHub-App / Supabase values are created even when
// blank so they can be populated later via `aws secretsmanager put-secret-value`.

# Shared node token (agent <-> edge auth). Generated if not supplied.
resource "random_password" "node_token" {
  length  = 40
  special = false
}

resource "aws_secretsmanager_secret" "node_token" {
  name                    = "${local.name}-node-token"
  recovery_window_in_days = 0
  tags                    = local.tags
}

resource "aws_secretsmanager_secret_version" "node_token" {
  secret_id     = aws_secretsmanager_secret.node_token.id
  secret_string = random_password.node_token.result
}

# database_url is populated by rds.tf (the RDS connection string). Container
# created here so the ARN/name is stable.
resource "aws_secretsmanager_secret" "database_url" {
  name                    = "${local.name}-database-url"
  recovery_window_in_days = 0
  tags                    = local.tags
}

# clickhouse_url is auto-filled from the ClickHouse EC2 (clickhouse.tf) unless
# var.clickhouse_url overrides it.
resource "aws_secretsmanager_secret" "clickhouse_url" {
  name                    = "${local.name}-clickhouse-url"
  recovery_window_in_days = 0
  tags                    = local.tags
}

resource "aws_secretsmanager_secret_version" "clickhouse_url_override" {
  count         = var.clickhouse_url != "" ? 1 : 0
  secret_id     = aws_secretsmanager_secret.clickhouse_url.id
  secret_string = var.clickhouse_url
}

# Supabase auth bits (optional — used by edge for JWT verification).
resource "aws_secretsmanager_secret" "supabase" {
  name                    = "${local.name}-supabase"
  recovery_window_in_days = 0
  tags                    = local.tags
}

resource "aws_secretsmanager_secret_version" "supabase" {
  secret_id = aws_secretsmanager_secret.supabase.id
  secret_string = jsonencode({
    supabase_url      = var.supabase_url
    supabase_jwks_url = var.supabase_jwks_url
    supabase_anon_key = var.supabase_anon_key
  })
}

# GitHub App env (apps feature). Single JSON blob fetched by the edge.
resource "aws_secretsmanager_secret" "github_env" {
  name                    = "${local.name}-github-env"
  recovery_window_in_days = 0
  tags                    = local.tags
}

resource "aws_secretsmanager_secret_version" "github_env" {
  secret_id = aws_secretsmanager_secret.github_env.id
  secret_string = jsonencode({
    GITHUB_APP_ID              = var.github_app_id
    GITHUB_APP_INSTALLATION_ID = var.github_app_installation_id
    GITHUB_APP_SLUG            = var.github_app_slug
    GITHUB_APP_CLIENT_ID       = var.github_app_client_id
    GITHUB_APP_CLIENT_SECRET   = var.github_app_client_secret
    GITHUB_APP_PRIVATE_KEY     = var.github_app_private_key
    GITHUB_APP_WEBHOOK_SECRET  = var.github_app_webhook_secret
  })
}

# Cloudflare API token (db-proxy uses it for DNS-01 on *.db.<zone>).
resource "aws_secretsmanager_secret" "cloudflare_token" {
  name                    = "${local.name}-cloudflare-api-token"
  recovery_window_in_days = 0
  tags                    = local.tags
}

resource "aws_secretsmanager_secret_version" "cloudflare_token" {
  secret_id     = aws_secretsmanager_secret.cloudflare_token.id
  secret_string = var.cloudflare_api_token
}
