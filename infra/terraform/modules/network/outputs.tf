output "vpc_id" {
  value = aws_vpc.this.id
}

output "subnet_id" {
  value = aws_subnet.public.id
}

output "security_group_id" {
  value = aws_security_group.host.id
}

output "eip_id" {
  value = aws_eip.this.id
}

output "eip_address" {
  value = aws_eip.this.public_ip
}
