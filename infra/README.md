# PandaStack infrastructure (Terraform)

Two multi-node deployment targets live under `infra/terraform/envs/`:

| Env | Cloud | Topology |
| --- | --- | --- |
| [`dev-aws`](terraform/envs/dev-aws) | AWS | VPC (2 AZ) · edge ASG + ALB · agent ASG (`*.metal` Firecracker) · RDS Postgres · ClickHouse EC2 · db-proxy EC2 + EIP · Secrets Manager · Cloudflare DNS |
| [`dev-gcp-multi`](terraform/envs/dev-gcp-multi) | GCP | Private VPC · edge MIG + global HTTPS LB · agent MIG · Cloud SQL · ClickHouse VM · db-proxy VM · Secret Manager · Cloudflare DNS |

Shared modules are in [`terraform/modules/`](terraform/modules).

## Prerequisites

- Terraform >= 1.6
- Cloud credentials: an AWS profile (EC2/VPC/IAM/S3/RDS/SecretsManager) **or**
  `gcloud auth application-default login` for GCP
- A Cloudflare API token with `Zone:DNS:Edit` on your zone
- An SSH key pair (`ssh-keygen -t ed25519`)

## Setup

```bash
# Pick an env:
cd infra/terraform/envs/dev-aws          # or dev-gcp-multi

cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars                  # set cloudflare_*, ssh_pubkey, ssh_allowed_cidr, …

# State bucket: edit backend.tf to point at your own bucket, or:
terraform init -backend-config="bucket=<your-tfstate-bucket>"
terraform plan
terraform apply
```

`make tf-aws-plan` / `make tf-gcp-plan` (and the `-apply` / `-destroy` / `-output`
variants) wrap these for convenience — see the repo `Makefile`.

## Notes

- `terraform.tfvars` and all `*.tfstate` are git-ignored — never commit them.
- Cloudflare should use SSL/TLS mode **Full** for these dev stacks (the edge
  serves over HTTP behind the Cloudflare proxy; for Full(strict), terminate TLS
  at the load balancer with a managed/ACM cert).
- For the GCP multi-node operational runbook (rolling updates, secrets, smoke
  tests), see [`deploy/DEPLOY.md`](../deploy/DEPLOY.md).
