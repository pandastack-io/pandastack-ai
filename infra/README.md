# PandaStack infrastructure (Terraform)

Two multi-node deployment targets live under `infra/terraform/envs/`:

| Env | Cloud | Topology |
| --- | --- | --- |
| [`dev-aws`](terraform/envs/dev-aws) | AWS | VPC (2 AZ) · edge ASG + ALB · agent ASG (`*.metal` Firecracker) · RDS Postgres · ClickHouse EC2 · db-proxy EC2 + EIP · Secrets Manager · Cloudflare DNS |
| [`dev-gcp-multi`](terraform/envs/dev-gcp-multi) | GCP | Private VPC · edge MIG + global HTTPS LB · agent MIG · Cloud SQL · ClickHouse VM · db-proxy VM · Secret Manager · Cloudflare DNS |

Shared modules are in [`terraform/modules/`](terraform/modules).

---

## Minimum production footprint

**Running a Firecracker fleet is not cheap, and this README is not going to
pretend otherwise.** The defaults in `variables.tf` are sized for a real fleet
with real tenants — roughly **US$7,000/month on AWS list pricing**. Most of that
is two bare-metal hosts, and there is no way around them: Firecracker needs
bare-metal KVM, so the agent tier must be a `*.metal` flavor on EC2.

Every number below is justified in a comment on the matching Terraform variable.
The short version of *why* each component is sized the way it is:

- **Agents are big because the per-host costs are fixed.** Each agent pre-seeds
  a baked memory snapshot + base rootfs for *every* first-party template so a
  create restores in ~150 ms instead of cold-booting
  (`cloud-init/user-data-agent.sh` runs `pandastack-agent seed-sync`;
  `agent/internal/sandbox/template_snap.go`). UFFD memory streaming and the NBD
  rootfs stream keep a large content-addressed chunk cache on local disk, shared
  across every sandbox on the host
  (`agent/internal/memstream/sharedcache.go`, `agent/internal/diskstream/sharedcache.go`).
  That overhead is paid once per host regardless of how many sandboxes run on
  it, so small hosts are strictly worse economics, not a cheaper starting point.
- **The agent's disk holds customer data, not scratch.** Managed-Postgres
  PGDATA is host-pinned to the agent's durable data volume. Filling that disk is
  a data incident.
- **Two agents is a floor, not a performance target.** With one agent a managed
  database has no failover target, and a freshly scaled-out agent is not
  schedulable until its seed-sync completes.
- **ClickHouse retains the boot events and per-sandbox metrics** behind the
  dashboard's charts. Retention is time-based, so the disk has to hold the whole
  window plus merge headroom.

### AWS (`dev-aws`), us-east-1, on-demand list price

| Component | Instance | vCPU | RAM | Disk | Count | ~US$/mo |
| --- | --- | ---: | ---: | --- | ---: | ---: |
| Agent (Firecracker hosts) | `c5n.metal` | 72 | 192 GiB | 1,000 GB gp3 @ 12k IOPS / 500 MB/s | 2 | 5,960 |
| Edge (API + dashboard) | `m6i.large` | 2 | 8 GiB | 100 GB gp3 | 2 | 156 |
| Control-plane DB | `db.m6g.xlarge` (Multi-AZ) | 4 | 16 GiB | 200 GB gp3 | 1 | 612 |
| ClickHouse (analytics) | `m6i.xlarge` | 4 | 16 GiB | 1,000 GB gp3 | 1 | 220 |
| db-proxy (Postgres SNI router) | `c6i.large` | 2 | 4 GiB | 100 GB gp3 | 1 | 70 |
| ALB | — | — | — | — | 1 | 31 |
| NAT gateway | — | — | — | — | 1 | 53 |
| S3 + Secrets Manager + egress | — | — | — | — | — | ~60 |
| **All-in** | | | | | | **≈ 7,160** |

Basis and caveats, so you can check the arithmetic:

- 730 hours/month, **on-demand list price, no commitment discounts**. A 1-year
  Compute Savings Plan typically takes 25–30% off the EC2 lines, which is most
  of the bill — budget against list, then buy commitment once your floor is
  known.
- Prices are region- and time-dependent. Confirm current rates with the AWS
  pricing calculator before you commit; these are accurate to the nearest few
  percent as written, not to the cent.
- Excludes data transfer out to the internet, which depends entirely on your
  workload. Sandboxes that stream large artifacts can make egress a top-three
  line item.
- Excludes anything you would run around the platform (log retention beyond
  journald, external monitoring, a CI runner fleet).

### GCP (`dev-gcp-multi`)

The GCP env is sized to be comparable, not identical. GCP exposes nested VT-x on
N2, so Firecracker does **not** need a bare-metal flavor there — the agent tier
is `n2-standard-64` (64 vCPU / 256 GiB) against AWS's `c5n.metal`
(72 vCPU / 192 GiB). On GCP the agent has two disks: an 800 GB boot disk for
template preseeds and the streaming chunk cache, and a separate 500 GB
**stateful** PD for customer volumes and managed-database PGDATA, which survives
a MIG autoheal. Cloud SQL runs `db-custom-4-16384` with `REGIONAL` availability
(the Cloud SQL equivalent of Multi-AZ). All-in cost lands in the same
US$6–8k/month band; run `gcloud compute instances describe` against your own
region for exact figures.

### Shrinking it

These are defaults, not requirements. For an evaluation you can drop to one
agent, a single edge instance, `ZONAL` / single-AZ for the control-plane
database, and much smaller disks — the stack will come up and work. What you
lose is specific and worth knowing before you choose it:

| If you set | You lose |
| --- | --- |
| `agent_count = 1` | Managed-database failover (no second host to restore onto) and the ability to drain a host for upgrades. |
| `rds_multi_az = false` / `cloudsql_availability_type = "ZONAL"` | An AZ/zone event freezes all scheduling: no creates, no wakes, no node registration. |
| A smaller `agent_boot_disk_size_gb` | Chunk-cache thrashing. Restores fall back from ~150 ms to a multi-second object-store fetch. Shrink `/opt/pandastack.img` in `cloud-init/user-data-agent.sh` to match, or it will ENOSPC mid-write. |
| A smaller `clickhouse_disk_size_gb` | The dashboard's history window, silently, once the disk fills. |
| `use_spot` / `use_preemptible = true` on agents | Reclamation becomes a data-plane event — an evicted agent takes its running sandboxes and host-pinned PGDATA with it. |

If you would rather not operate this, [PandaStack Cloud](https://pandastack.ai)
runs the same stack as a managed service. The code here is the same code; the
difference is who carries the pager.

---

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

- `terraform.tfvars`, all `*.tfstate`, and all saved plan files (`*.tfplan`,
  `tfplan*`, `*.plan`, `*.out`) are git-ignored — never commit them. A saved
  plan is a serialized copy of the resource graph: it embeds attribute values
  and the variables that produced them, including ones marked `sensitive`.
- `cloudflare_zone_name` has no default in either env. Set it to your own zone —
  the edge derives its dashboard/API URLs from it, and the db-proxy derives its
  `*.db.<zone>` SNI suffix from it.
- Cloudflare should use SSL/TLS mode **Full** for these stacks (the edge serves
  over HTTP behind the Cloudflare proxy; for Full(strict), terminate TLS at the
  load balancer with a managed/ACM cert).
- The db-proxy is a single instance behind a single static IP. It is not on the
  path for sandboxes or the control plane, but it *is* a single point of failure
  for customer database connectivity — front two proxies with an NLB if that
  matters to you.
- For the GCP multi-node operational runbook (rolling updates, secrets, smoke
  tests), see [`deploy/DEPLOY.md`](../deploy/DEPLOY.md).
