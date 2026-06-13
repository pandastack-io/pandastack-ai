# AWS single-node PandaStack agent

Creates one public bare-metal EC2 instance for a PandaStack agent, plus a VPC, subnet, internet gateway, and security group.

## Apply

```bash
terraform init
terraform plan -out=tfplan \
  -var='control_plane_url=https://api.pandastack.ai' \
  -var='node_token=replace-with-short-lived-token' \
  -var='ssh_allowed_cidr=YOUR_IP/32'
terraform apply tfplan
```

## Expected cost

A `c7i.metal` or `c6i.metal` host can cost hundreds to thousands of USD per month if left running continuously, plus EBS and data transfer. Stop or destroy it when not in use.

## Not included

- load balancer
- private networking or VPN
- object storage snapshot store
- multi-node scheduler wiring
- managed observability stack
- production secret manager
