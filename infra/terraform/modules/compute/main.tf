data "aws_ami" "ubuntu_noble" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "architecture"
    values = ["x86_64"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

locals {
  ami_id = var.ami_id != null ? var.ami_id : data.aws_ami.ubuntu_noble.id
  tags = {
    Project = var.project_tag
  }
}

resource "aws_key_pair" "this" {
  key_name   = "${var.project_tag}-ssh"
  public_key = var.ssh_pubkey

  tags = local.tags
}

data "aws_iam_policy_document" "assume_role" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "this" {
  name               = "${var.project_tag}-host"
  assume_role_policy = data.aws_iam_policy_document.assume_role.json

  tags = local.tags
}

data "aws_iam_policy_document" "s3" {
  statement {
    actions   = ["s3:ListBucket"]
    resources = ["arn:aws:s3:::${var.s3_bucket_name}"]
  }

  statement {
    actions = [
      "s3:AbortMultipartUpload",
      "s3:DeleteObject",
      "s3:GetObject",
      "s3:ListMultipartUploadParts",
      "s3:PutObject",
    ]
    resources = ["arn:aws:s3:::${var.s3_bucket_name}/*"]
  }
}

resource "aws_iam_policy" "s3" {
  name   = "${var.project_tag}-s3"
  policy = data.aws_iam_policy_document.s3.json

  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "s3" {
  role       = aws_iam_role.this.name
  policy_arn = aws_iam_policy.s3.arn
}

resource "aws_iam_instance_profile" "this" {
  name = "${var.project_tag}-host"
  role = aws_iam_role.this.name

  tags = local.tags
}

resource "aws_instance" "this" {
  ami                         = local.ami_id
  instance_type               = var.instance_type
  key_name                    = aws_key_pair.this.key_name
  subnet_id                   = var.subnet_id
  vpc_security_group_ids      = [var.security_group_id]
  associate_public_ip_address = true
  iam_instance_profile        = aws_iam_instance_profile.this.name
  user_data                   = file("${path.module}/../../../../cloud-init/user-data.sh")

  dynamic "instance_market_options" {
    for_each = var.use_spot ? [1] : []
    content {
      market_type = "spot"

      spot_options {
        max_price                      = var.spot_max_price
        spot_instance_type             = "persistent"
        instance_interruption_behavior = "stop"
      }
    }
  }

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }

  root_block_device {
    volume_size = var.root_volume_size_gb
    volume_type = "gp3"
  }

  tags = merge(local.tags, {
    Name = "${var.project_tag}-host"
  })

  volume_tags = local.tags
}

resource "aws_eip_association" "this" {
  allocation_id = var.eip_id
  instance_id   = aws_instance.this.id
}
