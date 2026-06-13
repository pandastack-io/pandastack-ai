// rds.tf — RDS for PostgreSQL (mirror of cloudsql.tf). Private subnets only;
// reachable from edge + agent security groups on 5432. The connection string
// is written to the database-url secret (secrets.tf) so edge/agent instances
// pick it up at boot.

resource "aws_db_subnet_group" "main" {
  name       = "${local.name}-db"
  subnet_ids = [for s in aws_subnet.private : s.id]
  tags       = merge(local.tags, { Name = "${local.name}-db" })
}

# RDS ingress on 5432 from edge + agent tiers only.
resource "aws_security_group" "rds" {
  name        = "${local.name}-rds"
  description = "PandaStack RDS Postgres"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "Postgres from edge"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.edge.id]
  }
  ingress {
    description     = "Postgres from agents"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.agent.id]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = merge(local.tags, { Name = "${local.name}-rds" })
}

resource "random_password" "rds_password" {
  length  = 32
  special = false
}

resource "aws_db_instance" "main" {
  identifier     = "${local.name}-postgres"
  engine         = "postgres"
  engine_version = var.rds_engine_version
  instance_class = var.rds_instance_class

  # rds_engine_version may be a major-only "16"; AWS resolves the latest minor
  # and applies minor upgrades in the maintenance window.
  auto_minor_version_upgrade = true

  allocated_storage     = var.rds_allocated_storage_gb
  max_allocated_storage = var.rds_allocated_storage_gb * 5 # storage autoscaling
  storage_type          = "gp3"
  storage_encrypted     = true

  db_name  = "pandastack"
  username = "pandastack"
  password = random_password.rds_password.result

  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.rds.id]
  publicly_accessible    = false
  multi_az               = false

  backup_retention_period = 7
  backup_window           = "03:00-04:00"

  deletion_protection = var.rds_deletion_protection
  skip_final_snapshot = !var.rds_deletion_protection

  tags = merge(local.tags, { role = "control-plane-db" })

  lifecycle {
    # With a major-only engine_version, AWS reports the resolved minor (e.g.
    # 16.4) which would otherwise show a perpetual diff. Ignore minor drift —
    # change the major intentionally via var.rds_engine_version + a manual apply.
    ignore_changes = [engine_version]
  }
}

# Write the RDS connection string into the database-url secret. Edge + agent
# instances read this at boot to construct their Postgres DSN.
resource "aws_secretsmanager_secret_version" "database_url" {
  secret_id     = aws_secretsmanager_secret.database_url.id
  secret_string = "postgresql://pandastack:${random_password.rds_password.result}@${aws_db_instance.main.address}:5432/pandastack?sslmode=require"
}
