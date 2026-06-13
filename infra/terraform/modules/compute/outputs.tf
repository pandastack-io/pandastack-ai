output "instance_id" {
  value = aws_instance.this.id
}

output "private_ip" {
  value = aws_instance.this.private_ip
}

output "public_ip" {
  value = aws_eip_association.this.public_ip
}

output "iam_role_arn" {
  value = aws_iam_role.this.arn
}
