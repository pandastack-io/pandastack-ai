locals {
  cloudflare_ipv4_cidrs = [
    "173.245.48.0/20",
    "103.21.244.0/22",
    "103.22.200.0/22",
    "103.31.4.0/22",
    "141.101.64.0/18",
    "108.162.192.0/18",
    "190.93.240.0/20",
    "188.114.96.0/20",
    "197.234.240.0/22",
    "198.41.128.0/17",
    "162.158.0.0/15",
    "104.16.0.0/13",
    "104.24.0.0/14",
    "172.64.0.0/13",
    "131.0.72.0/22",
  ]

  tags = {
    Project = var.project_tag
  }
}

resource "aws_vpc" "this" {
  cidr_block           = "10.10.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = merge(local.tags, {
    Name = "${var.project_tag}-vpc"
  })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(local.tags, {
    Name = "${var.project_tag}-igw"
  })
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.this.id
  cidr_block              = "10.10.1.0/24"
  availability_zone       = "${var.region}a"
  map_public_ip_on_launch = true

  tags = merge(local.tags, {
    Name = "${var.project_tag}-public-a"
  })
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = merge(local.tags, {
    Name = "${var.project_tag}-public"
  })
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_security_group" "host" {
  name        = "pandastack-dev-host"
  description = "Pandastack dev host ingress"
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_allowed_cidr]
  }

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
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.tags, {
    Name = "pandastack-dev-host"
  })
}

resource "aws_eip" "this" {
  domain = "vpc"

  tags = merge(local.tags, {
    Name = "${var.project_tag}-eip"
  })
}
