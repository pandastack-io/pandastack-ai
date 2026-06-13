// dbproxy.tf — native postgres:// TLS proxy with SNI routing (mirror of the
// db-proxy section in the GCP main.tf). Routes *.db.<zone>:5432 to the correct
// sandbox's Postgres port on the owning agent. Public EIP, NOT Cloudflare-
// proxied (CF can't proxy raw TCP 5432).

resource "aws_security_group" "db_proxy" {
  name        = "${local.name}-db-proxy"
  description = "PandaStack db-proxy SNI router"
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "Postgres (TLS) from customers"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
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
  tags = merge(local.tags, { Name = "${local.name}-db-proxy" })
}

resource "aws_eip" "db_proxy" {
  domain = "vpc"
  tags   = merge(local.tags, { Name = "${local.name}-db-proxy-ip" })
}

# Single small VM — the proxy is lightweight (io.Copy only, no compute).
resource "aws_instance" "db_proxy" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = "t3.small"
  subnet_id              = aws_subnet.public[var.availability_zones[0]].id
  vpc_security_group_ids = [aws_security_group.db_proxy.id]
  key_name               = aws_key_pair.this.key_name
  iam_instance_profile   = aws_iam_instance_profile.node.name

  root_block_device {
    volume_size = 20
    volume_type = "gp3"
  }

  user_data = base64encode(templatefile("${path.module}/user-data-db-proxy.sh.tftpl", {
    region              = var.aws_region
    secret_prefix       = local.name
    db_proxy_binary_url = var.db_proxy_binary_url
    sni_suffix          = ".db.${var.cloudflare_zone_name}"
  }))

  tags = merge(local.tags, { role = "db-proxy", Name = "${local.name}-db-proxy" })

  lifecycle {
    # Stateless but long-lived: pin boot AMI + user_data so routine applies never
    # recreate it and kill live database connections. Taint explicitly to roll.
    ignore_changes = [ami, user_data]
  }
}

resource "aws_eip_association" "db_proxy" {
  instance_id   = aws_instance.db_proxy.id
  allocation_id = aws_eip.db_proxy.id
}

# DNS: *.db.<zone> + db.<zone> → db-proxy EIP. NOT proxied (raw TCP 5432).
resource "cloudflare_record" "db_proxy_wildcard" {
  zone_id = var.cloudflare_zone_id
  name    = "*.db"
  type    = "A"
  content = aws_eip.db_proxy.public_ip
  proxied = false
  ttl     = 60
}

resource "cloudflare_record" "db_proxy_apex" {
  zone_id = var.cloudflare_zone_id
  name    = "db"
  type    = "A"
  content = aws_eip.db_proxy.public_ip
  proxied = false
  ttl     = 60
}
