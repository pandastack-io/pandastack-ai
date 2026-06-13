// clickhouse.tf — single-node ClickHouse on a dedicated EC2 in a private
// subnet (mirror of the GCP clickhouse.tf). Auto-deploys via
// cloud-init/user-data-clickhouse.sh which mounts a data volume, runs the
// official CH container, and applies the pandastack schema from S3.
//
// Reachable only from edge + agent security groups on 8123 (HTTP) / 9000.
// The clickhouse-url secret is auto-filled with the internal IP unless
// var.clickhouse_url overrides it.

locals {
  ch_password_special = false
}

resource "random_password" "clickhouse" {
  length  = 24
  special = local.ch_password_special
}

resource "aws_security_group" "clickhouse" {
  name        = "${local.name}-clickhouse"
  description = "PandaStack ClickHouse"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "CH HTTP/native from edge"
    from_port       = 8123
    to_port         = 9000
    protocol        = "tcp"
    security_groups = [aws_security_group.edge.id]
  }
  ingress {
    description     = "CH HTTP/native from agents"
    from_port       = 8123
    to_port         = 9000
    protocol        = "tcp"
    security_groups = [aws_security_group.agent.id]
  }
  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_allowed_cidr]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = merge(local.tags, { Name = "${local.name}-clickhouse" })
}

# Upload schema.sql to S3 so the VM can fetch it on boot.
resource "aws_s3_object" "clickhouse_schema" {
  bucket = module.storage.bucket_name
  key    = "clickhouse/schema.sql"
  source = "${path.module}/../../../../agent/internal/clickhouse/schema.sql"
  etag   = filemd5("${path.module}/../../../../agent/internal/clickhouse/schema.sql")
}

resource "aws_instance" "clickhouse" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = "t3.medium"
  subnet_id              = aws_subnet.private[var.availability_zones[0]].id
  vpc_security_group_ids = [aws_security_group.clickhouse.id]
  key_name               = aws_key_pair.this.key_name
  iam_instance_profile   = aws_iam_instance_profile.node.name

  root_block_device {
    volume_size = 70
    volume_type = "gp3"
  }

  user_data = base64encode(templatefile("${path.module}/user-data-clickhouse.sh.tftpl", {
    region              = var.aws_region
    clickhouse_password = random_password.clickhouse.result
    schema_s3_url       = "s3://${module.storage.bucket_name}/clickhouse/schema.sql"
  }))

  tags = merge(local.tags, { role = "clickhouse", Name = "${local.name}-clickhouse-1" })

  depends_on = [aws_s3_object.clickhouse_schema]

  lifecycle {
    # The data volume is the root disk here; pin the boot AMI + user_data so a
    # floating AMI or edited cloud-init doesn't recreate the CH node on every
    # apply. Taint explicitly to roll it.
    ignore_changes = [ami, user_data]
  }
}

# Auto-fill the clickhouse-url secret with the internal IP (unless overridden).
resource "aws_secretsmanager_secret_version" "clickhouse_url_auto" {
  count         = var.clickhouse_url == "" ? 1 : 0
  secret_id     = aws_secretsmanager_secret.clickhouse_url.id
  secret_string = "http://default:${random_password.clickhouse.result}@${aws_instance.clickhouse.private_ip}:8123/?database=pandastack"
  depends_on    = [aws_instance.clickhouse]
}
