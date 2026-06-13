variable "region" {
  description = "AWS region."
  type        = string
}

variable "instance_type" {
  description = "EC2 instance type."
  type        = string
}

variable "spot_max_price" {
  description = "Maximum hourly Spot price (ignored when use_spot=false)."
  type        = string
  default     = "1.00"
}

variable "use_spot" {
  description = "Use Spot pricing (true) or on-demand (false)."
  type        = bool
  default     = true
}

variable "root_volume_size_gb" {
  description = "Root EBS volume size in GB."
  type        = number
  default     = 100
}

variable "ami_id" {
  description = "Optional AMI ID override."
  type        = string
  default     = null
}

variable "ssh_pubkey" {
  description = "SSH public key contents."
  type        = string
}

variable "subnet_id" {
  description = "Subnet ID for the host."
  type        = string
}

variable "security_group_id" {
  description = "Security group ID for the host."
  type        = string
}

variable "eip_id" {
  description = "Elastic IP allocation ID."
  type        = string
}

variable "s3_bucket_name" {
  description = "S3 bucket name for host storage access."
  type        = string
}

variable "project_tag" {
  description = "Project tag value."
  type        = string
}
