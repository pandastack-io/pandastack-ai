// dev-aws: multi-node PandaStack stack on AWS, mirroring dev-gcp-multi.
//
// Topology:
//   Cloudflare (proxied) ──▶ ALB (public subnets, 2 AZ) ──▶ edge ASG (API+UI)
//                                                              │
//   agent ASG (private subnets, *.metal Firecracker) ◀────────┘ internal
//   RDS Postgres (private) · ClickHouse EC2 (private) · db-proxy EC2 (+EIP)
//
// Secrets live in AWS Secrets Manager (secrets.tf); RDS in rds.tf; ClickHouse
// in clickhouse.tf; db-proxy in dbproxy.tf.

provider "aws" {
  region = var.aws_region
}

provider "cloudflare" {
  api_token = var.cloudflare_api_token
}

locals {
  name = var.project_tag
  tags = {
    project = var.project_tag
    env     = "dev"
  }
  # Cloudflare's published IPv4 ranges — ingress to the ALB on 80/443 is locked
  # to these so the edge is only reachable through the Cloudflare proxy.
  cloudflare_ipv4_cidrs = [
    "173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
    "141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
    "197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
    "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
  ]
}

# Latest Ubuntu 24.04 LTS AMI (Canonical) for amd64.
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"] # Canonical
  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# =============================================================================
# Network — VPC with 2 public + 2 private subnets, IGW, single NAT gateway.
# =============================================================================

resource "aws_vpc" "this" {
  cidr_block           = "10.10.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = merge(local.tags, { Name = "${local.name}-vpc" })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${local.name}-igw" })
}

resource "aws_subnet" "public" {
  for_each                = { for i, az in var.availability_zones : az => i }
  vpc_id                  = aws_vpc.this.id
  cidr_block              = cidrsubnet(aws_vpc.this.cidr_block, 8, each.value) # 10.10.0.0/24, 10.10.1.0/24
  availability_zone       = each.key
  map_public_ip_on_launch = true
  tags                    = merge(local.tags, { Name = "${local.name}-public-${each.key}", tier = "public" })
}

resource "aws_subnet" "private" {
  for_each          = { for i, az in var.availability_zones : az => i }
  vpc_id            = aws_vpc.this.id
  cidr_block        = cidrsubnet(aws_vpc.this.cidr_block, 8, each.value + 10) # 10.10.10.0/24, 10.10.11.0/24
  availability_zone = each.key
  tags              = merge(local.tags, { Name = "${local.name}-private-${each.key}", tier = "private" })
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
  tags = merge(local.tags, { Name = "${local.name}-public-rt" })
}

resource "aws_route_table_association" "public" {
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

# Single NAT gateway (cost-conscious dev default) in the first public subnet.
resource "aws_eip" "nat" {
  domain = "vpc"
  tags   = merge(local.tags, { Name = "${local.name}-nat-eip" })
}

resource "aws_nat_gateway" "this" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public[var.availability_zones[0]].id
  tags          = merge(local.tags, { Name = "${local.name}-nat" })
  depends_on    = [aws_internet_gateway.this]
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.this.id
  }
  tags = merge(local.tags, { Name = "${local.name}-private-rt" })
}

resource "aws_route_table_association" "private" {
  for_each       = aws_subnet.private
  subnet_id      = each.value.id
  route_table_id = aws_route_table.private.id
}

# =============================================================================
# Security groups
# =============================================================================

# ALB: public ingress on 80/443 from Cloudflare ranges only.
resource "aws_security_group" "alb" {
  name        = "${local.name}-alb"
  description = "PandaStack ALB ingress (Cloudflare-proxied)"
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "HTTP from Cloudflare"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = local.cloudflare_ipv4_cidrs
  }
  ingress {
    description = "HTTPS from Cloudflare"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = local.cloudflare_ipv4_cidrs
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = merge(local.tags, { Name = "${local.name}-alb" })
}

# Edge: API/dashboard hosts. Ingress on 8080 from the ALB; SSH from operator.
resource "aws_security_group" "edge" {
  name        = "${local.name}-edge"
  description = "PandaStack edge hosts (API + dashboard)"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "API from ALB"
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
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
  tags = merge(local.tags, { Name = "${local.name}-edge" })
}

# Agent: Firecracker hosts in private subnets. Reachable from edge (proxy) and
# db-proxy (pg-tunnel) only; full intra-agent traffic allowed.
resource "aws_security_group" "agent" {
  name        = "${local.name}-agent"
  description = "PandaStack agent (Firecracker) hosts"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "Agent API from edge"
    from_port       = 7070
    to_port         = 7070
    protocol        = "tcp"
    security_groups = [aws_security_group.edge.id]
  }
  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_allowed_cidr]
  }
  ingress {
    description = "Intra-agent (all ports)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    self        = true
  }
  # db-proxy → agent :8081 (pg-tunnel upgrade). Inline (not a standalone
  # aws_security_group_rule) so this SG owns its full rule set — mixing inline
  # blocks with standalone rules makes the provider fight itself on apply.
  # db_proxy SG does not reference agent, so there is no dependency cycle.
  ingress {
    description     = "pg-tunnel from db-proxy"
    from_port       = 8081
    to_port         = 8081
    protocol        = "tcp"
    security_groups = [aws_security_group.db_proxy.id]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = merge(local.tags, { Name = "${local.name}-agent" })
}

# =============================================================================
# SSH key pair (shared by all tiers)
# =============================================================================

resource "aws_key_pair" "this" {
  key_name   = "${local.name}-key"
  public_key = var.ssh_pubkey
  tags       = local.tags
}

# =============================================================================
# IAM — instance role allowing S3 read of the build/template bucket and
# Secrets Manager access. Shared by edge + agent launch templates.
# =============================================================================

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name               = "${local.name}-node"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
  tags               = local.tags
}

data "aws_iam_policy_document" "node" {
  statement {
    sid       = "S3Read"
    actions   = ["s3:GetObject", "s3:ListBucket"]
    resources = [module.storage.bucket_arn, "${module.storage.bucket_arn}/*"]
  }
  statement {
    sid       = "SecretsRead"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
    resources = ["arn:aws:secretsmanager:${var.aws_region}:*:secret:${local.name}-*"]
  }
  statement {
    sid       = "CloudWatchLogs"
    actions   = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "node" {
  name   = "${local.name}-node"
  role   = aws_iam_role.node.id
  policy = data.aws_iam_policy_document.node.json
}

resource "aws_iam_instance_profile" "node" {
  name = "${local.name}-node"
  role = aws_iam_role.node.name
  tags = local.tags
}

# =============================================================================
# Storage — reuse the shared S3 module for build artifacts + template seeds.
# =============================================================================

module "storage" {
  source      = "../../modules/storage"
  env         = "dev-multi"
  project_tag = var.project_tag
}

# =============================================================================
# Edge tier — ALB + ASG of API/dashboard hosts.
# =============================================================================

resource "aws_lb" "edge" {
  name               = "${local.name}-edge"
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = [for s in aws_subnet.public : s.id]
  tags               = merge(local.tags, { role = "edge" })
}

resource "aws_lb_target_group" "edge" {
  name        = "${local.name}-edge"
  port        = 8080
  protocol    = "HTTP"
  vpc_id      = aws_vpc.this.id
  target_type = "instance"

  health_check {
    path                = "/healthz"
    port                = "8080"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    interval            = 15
    timeout             = 5
    matcher             = "200"
  }
  tags = local.tags
}

# HTTP listener — forwards to the edge target group. Cloudflare terminates
# public TLS (Full mode); for Full(strict) terminate at the ALB with an ACM
# cert and add a 443 listener (left out here to keep the dev stack credential-free).
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.edge.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.edge.arn
  }
}

resource "aws_launch_template" "edge" {
  name_prefix   = "${local.name}-edge-"
  image_id      = data.aws_ami.ubuntu.id
  instance_type = var.edge_instance_type
  key_name      = aws_key_pair.this.key_name

  iam_instance_profile {
    arn = aws_iam_instance_profile.node.arn
  }

  network_interfaces {
    associate_public_ip_address = true
    security_groups             = [aws_security_group.edge.id]
  }

  dynamic "instance_market_options" {
    for_each = var.use_spot ? [1] : []
    content {
      market_type = "spot"
    }
  }

  user_data = base64encode(templatefile("${path.module}/user-data-edge.sh.tftpl", {
    region               = var.aws_region
    project_tag          = var.project_tag
    secret_prefix        = local.name
    edge_binary_url      = var.edge_binary_url
    dashboard_bucket     = var.dashboard_bucket
    cloudflare_zone_name = var.cloudflare_zone_name
  }))

  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.tags, { role = "edge", Name = "${local.name}-edge" })
  }
  tags = local.tags
}

resource "aws_autoscaling_group" "edge" {
  name                = "${local.name}-edge"
  min_size            = var.edge_count
  max_size            = var.edge_max_count
  desired_capacity    = var.edge_count
  vpc_zone_identifier = [for s in aws_subnet.public : s.id]
  target_group_arns   = [aws_lb_target_group.edge.arn]
  health_check_type   = "ELB"

  launch_template {
    id      = aws_launch_template.edge.id
    version = "$Latest"
  }

  tag {
    key                 = "Name"
    value               = "${local.name}-edge"
    propagate_at_launch = true
  }
  tag {
    key                 = "role"
    value               = "edge"
    propagate_at_launch = true
  }
}

# =============================================================================
# Agent tier — ASG of *.metal Firecracker hosts in private subnets.
# =============================================================================

resource "aws_launch_template" "agent" {
  name_prefix   = "${local.name}-agent-"
  image_id      = data.aws_ami.ubuntu.id
  instance_type = var.agent_instance_type
  key_name      = aws_key_pair.this.key_name

  iam_instance_profile {
    arn = aws_iam_instance_profile.node.arn
  }

  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size           = var.agent_boot_disk_size_gb
      volume_type           = "gp3"
      delete_on_termination = true
    }
  }

  network_interfaces {
    associate_public_ip_address = false
    security_groups             = [aws_security_group.agent.id]
  }

  dynamic "instance_market_options" {
    for_each = var.use_spot ? [1] : []
    content {
      market_type = "spot"
    }
  }

  user_data = base64encode(templatefile("${path.module}/user-data-agent.sh.tftpl", {
    region           = var.aws_region
    project_tag      = var.project_tag
    secret_prefix    = local.name
    agent_binary_url = var.agent_binary_url
    s3_bucket_name   = module.storage.bucket_name
  }))

  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.tags, { role = "agent", Name = "${local.name}-agent" })
  }
  tags = local.tags
}

resource "aws_autoscaling_group" "agent" {
  name                = "${local.name}-agent"
  min_size            = var.agent_count
  max_size            = var.agent_max_count
  desired_capacity    = var.agent_count
  vpc_zone_identifier = [for s in aws_subnet.private : s.id]
  health_check_type   = "EC2"

  launch_template {
    id      = aws_launch_template.agent.id
    version = "$Latest"
  }

  tag {
    key                 = "Name"
    value               = "${local.name}-agent"
    propagate_at_launch = true
  }
  tag {
    key                 = "role"
    value               = "agent"
    propagate_at_launch = true
  }
}

# =============================================================================
# DNS — api.<zone> + preview wildcard *.<zone> → ALB. Cloudflare-proxied.
# =============================================================================

resource "cloudflare_record" "api" {
  zone_id = var.cloudflare_zone_id
  name    = "api"
  type    = "CNAME"
  content = aws_lb.edge.dns_name
  proxied = true
  ttl     = 1
}

# Wildcard for preview URLs: {port}-{sandbox_id}.<zone> → edge preview-host
# middleware. Cloudflare Universal SSL covers single-level wildcards.
resource "cloudflare_record" "preview_wildcard" {
  zone_id = var.cloudflare_zone_id
  name    = "*"
  type    = "CNAME"
  content = aws_lb.edge.dns_name
  proxied = true
  ttl     = 1
}
