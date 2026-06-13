# GCP single-node PandaStack agent

Creates one GCE VM with nested virtualization support, a custom VPC/subnet, SSH firewall rule, and startup script for a PandaStack agent.

## Apply

```bash
terraform init
terraform plan -out=tfplan \
  -var='project_id=YOUR_PROJECT' \
  -var='control_plane_url=https://api.pandastack.ai' \
  -var='node_token=replace-with-short-lived-token' \
  -var='ssh_allowed_cidr=YOUR_IP/32'
terraform apply tfplan
```

## Expected cost

An `n2-standard-4` VM with a 100 GB balanced persistent disk is typically in the low hundreds of USD per month if left running continuously. Costs vary by region and committed-use discounts.

## Not included

- load balancer
- private service connect or VPN
- object storage snapshot store
- multi-node managed instance group
- managed observability stack
- production secret manager
