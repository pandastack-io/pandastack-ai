provider "aws" {
  region = var.region
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"]
  }
}

resource "aws_vpc" "pandastack" {
  cidr_block           = "10.42.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "pandastack-single-node"
  }
}

resource "aws_internet_gateway" "pandastack" {
  vpc_id = aws_vpc.pandastack.id

  tags = {
    Name = "pandastack-single-node"
  }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.pandastack.id
  cidr_block              = "10.42.1.0/24"
  map_public_ip_on_launch = true

  tags = {
    Name = "pandastack-single-node-public"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.pandastack.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.pandastack.id
  }

  tags = {
    Name = "pandastack-single-node-public"
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_security_group" "agent" {
  name        = "pandastack-agent"
  description = "PandaStack single-node agent access"
  vpc_id      = aws_vpc.pandastack.id

  tags = {
    Name = "pandastack-agent"
  }
}

resource "aws_vpc_security_group_ingress_rule" "ssh" {
  security_group_id = aws_security_group.agent.id
  cidr_ipv4         = var.ssh_allowed_cidr
  from_port         = 22
  ip_protocol       = "tcp"
  to_port           = 22
}

resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.agent.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

locals {
  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    control_plane_url = var.control_plane_url
    node_token        = var.node_token
  })
}

resource "aws_instance" "agent" {
  ami                         = data.aws_ami.ubuntu.id
  instance_type               = var.instance_type
  subnet_id                   = aws_subnet.public.id
  vpc_security_group_ids      = [aws_security_group.agent.id]
  associate_public_ip_address = true
  key_name                    = var.ssh_key_name
  user_data_replace_on_change = true
  user_data                   = local.user_data

  root_block_device {
    encrypted   = true
    volume_size = 100
    volume_type = "gp3"
  }

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }

  tags = {
    Name = "pandastack-agent-single-node"
  }
}
