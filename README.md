<p align="center">
  <img src="logos/pandastack-tile.svg" width="96" alt="PandaStack logo" />
</p>

# PandaStack

Open-source Firecracker microVM sandboxes and managed PostgreSQL, self-hosted.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![CI](https://github.com/pandastack-io/pandastack-ai/actions/workflows/ci.yml/badge.svg)](https://github.com/pandastack-io/pandastack-ai/actions/workflows/ci.yml)
[![GitHub stars](https://img.shields.io/github/stars/pandastack-io/pandastack-ai?style=social)](https://github.com/pandastack-io/pandastack-ai/stargazers)


## 60-second quickstart

```bash
git clone https://github.com/pandastack-io/pandastack-ai
cd pandastack-ai
bash scripts/mac-local-e2e.sh
open http://localhost:3000
```

Create a sandbox from the local API:

```bash
curl -sS http://localhost:8080/v1/sandboxes \
  -H 'Authorization: Bearer pds_local_dev_token' \
  -H 'Content-Type: application/json' \
  -d '{"template":"base"}'
```

Exec a command in it:

```bash
curl -sS http://localhost:8080/v1/sandboxes/<id>/exec \
  -H 'Authorization: Bearer pds_local_dev_token' \
  -H 'Content-Type: application/json' \
  -d '{"cmd":"echo","args":["hello"]}'
```

## What it does

PandaStack is two things on one control plane: **disposable Firecracker microVM
sandboxes** for AI agents and untrusted code, and **managed PostgreSQL 16**
instances that run in microVMs of their own.

- Firecracker microVM sandboxes with strong process and kernel isolation.
- **Managed PostgreSQL 16 databases (Beta)** — a real, durable database in its own microVM, with a native `postgres://` URL in seconds, continuous WAL archiving, and restore-based failover onto a healthy host.
- Sub-second boot on every create via baked snapshot restore — no warm pool of idle VMs.
- Snapshot anywhere and fork running environments instantly, including fork trees (fork a fork).
- **On-demand UFFD memory streaming** — restore microVMs by paging guest memory lazily from object storage (GCS Range GETs) instead of downloading the full snapshot up front.
- **Demand-paged rootfs streaming** — the same trick for the disk: the guest's root filesystem is served over an in-kernel NBD device backed by ranged reads, so a cold host never downloads a whole rootfs before booting.
- Full lifecycle: pause, resume, hibernate, wake, TTL expiry, and idle reaping.
- Per-template CPU, RAM, and disk sizing baked into each snapshot.
- Memory admission control, a host-pressure ladder, and CPU tiers so one noisy sandbox can't starve the host.
- Named persistent volumes (ext4 over virtio-blk) you can attach read-write to one sandbox or read-only to many.
- Network egress controls and per-sandbox network namespaces for safer code execution.
- Template-based images: OCI images converted to ext4 roots, plus a build pipeline for your own.
- Exec, REPL, LSP, MCP, and browser terminal surfaces.
- Audit log and observability backed by Postgres and ClickHouse.
- Multi-node scheduling with capacity scoring, leases, and slot reconciliation.

## Use it from your code

PandaStack exposes a token-authenticated REST API. Point any HTTP client at your
self-hosted control plane (`http://localhost:8080` for local dev) and authenticate
with an API token (`pds_…`):

```bash
export PANDASTACK_API=http://localhost:8080
export PANDASTACK_API_KEY=pds_local_dev_token

# create
SBX=$(curl -sS "$PANDASTACK_API/v1/sandboxes" \
  -H "Authorization: Bearer $PANDASTACK_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"template":"base"}' | jq -r .id)

# exec
curl -sS "$PANDASTACK_API/v1/sandboxes/$SBX/exec" \
  -H "Authorization: Bearer $PANDASTACK_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"cmd":"uname","args":["-a"]}'

# delete
curl -sS -X DELETE "$PANDASTACK_API/v1/sandboxes/$SBX" \
  -H "Authorization: Bearer $PANDASTACK_API_KEY"
```

See the [REST API reference](docs-site/content/docs/reference/rest-api.mdx) for the full surface.

## Managed databases (Beta)

Provision a managed **PostgreSQL 16** instance running in its own dedicated Firecracker
microVM — not a shared schema on a multi-tenant cluster. Each database gets its own kernel,
`postgres` process, connection pooler, and a durable data volume. Databases are persistent:
the idle reaper never deletes them, and only an explicit `DELETE` destroys the data.

> **Beta.** Create / list / get / delete are stable, and every database gets continuous WAL
> archiving plus restore-to-latest failover. Read replicas and storage autoscaling are
> [not here yet](docs-site/content/docs/concepts/databases.mdx#coming-soon); database
> branching and point-in-time restore are
> [PandaStack Cloud features](docs-site/content/docs/concepts/open-source-vs-cloud.mdx).
> Don't yet rely on a single beta database as the only copy of irreplaceable data.

```bash
# create (blocks until Postgres accepts connections, ~30–90s)
curl -X POST "$PANDASTACK_API/v1/databases" \
  -H "Authorization: Bearer $PANDASTACK_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"label":"my-app-db"}'

# connect with any PostgreSQL client (TLS required)
psql "$CONNECTION_URL?sslmode=require"
```

Manage them with `list` / `get` / `delete` (REST: `GET|DELETE /v1/databases[/{id}]`). Full
guide: [Databases docs](docs-site/content/docs/concepts/databases.mdx).

## Architecture

PandaStack separates the control plane from per-host agents. The API accepts sandbox requests, the scheduler selects capacity, and agents create or resume Firecracker microVMs. Every create restores a baked per-template snapshot (with optional UFFD memory streaming and demand-paged rootfs streaming from object storage), and a snapshot store persists VM state.

```text
+--------+      +-----+      +-----------+      +------------------+      +----------------------+
| Client | ---> | API | ---> | Scheduler | ---> | Agents per host  | ---> | Firecracker microVMs |
+--------+      +-----+      +-----------+      +------------------+      +----------------------+
                     |              |                    |
                     v              v                    v
               Postgres/audit   Capacity scoring   Snapshot seeds + UFFD/NBD streaming
```

## Repository layout

| Path | What it is |
| --- | --- |
| [`api/`](api) | Control-plane REST API (Go) — sandboxes, databases, templates, volumes, orgs, auth. |
| [`agent/`](agent) | Per-host agent (Go) that boots and manages Firecracker microVMs. |
| [`db-proxy/`](db-proxy) | SNI-routing proxy that maps `<id>.db.<your-domain>` to the right database VM. |
| [`dashboard/`](dashboard) | Web dashboard (Next.js) — sandboxes, databases, templates. |
| [`docs-site/`](docs-site) | Documentation site (Next.js + fumadocs). |
| [`templates/`](templates) | microVM template Dockerfiles — `base` (Ubuntu 24.04 + mise; Node/Python/Go/Bun), `code-interpreter`, `agent`, `postgres-16`. |
| [`cloud-init/`](cloud-init) | Host/guest provisioning scripts. |
| [`infra/`](infra), [`deploy/`](deploy), [`ansible/`](ansible) | Infrastructure and deployment. |
| [`cookbook/`](cookbook), [`examples/`](examples) | Tutorial recipes and example projects. |

## Self-host

| Mode | Path | Docs |
| --- | --- | --- |
| Local dev (Mac, Apple Silicon) | `bash scripts/mac-local-e2e.sh` | [Apple Silicon guide](docs-site/content/docs/getting-started/local-mac-apple-silicon.mdx) |
| Local dev (Linux KVM host) | `bash scripts/linux-local-e2e.sh` | [Self-host guide](docs-site/content/docs/getting-started/self-host.mdx) |
| Multi-node (AWS) | `infra/terraform/envs/dev-aws` | [`infra/README.md`](infra/README.md) |
| Multi-node (GCP) | `infra/terraform/envs/dev-gcp-multi` | [`deploy/DEPLOY.md`](deploy/DEPLOY.md) |

## Open source vs PandaStack Cloud

This repository is the whole engine, not a demo. Everything below in the
**Open source** column runs on your own hardware under Apache-2.0, with **no
usage limits of any kind** — the OSS edition ships unmetered and uncapped, and
contains no billing, metering, quota, or subscription code at all.

**PandaStack Cloud** is the hosted service run by the PandaStack team. It runs
this same core and adds a small number of features that stay hosted-only, plus
the operational work of running a Firecracker fleet.

| | Open source (this repo) | PandaStack Cloud |
| --- | --- | --- |
| **Sandbox lifecycle** | Full — create, exec, REPL, LSP, terminal, filesystem, pause/resume, hibernate/wake, TTL, idle reaping, delete | Same |
| **Snapshots & fork** | Full — named snapshots, instant fork of a running VM, fork trees, boot-from-snapshot | Same |
| **Memory streaming** | UFFD demand-paged guest memory from object storage | Same |
| **Rootfs streaming** | Demand-paged rootfs over in-kernel NBD | Same |
| **Templates** | Four first-party templates (`base`, `code-interpreter`, `agent`, `postgres-16`) + build your own from any Debian-based OCI image | Same, plus first-party templates kept baked and warm for you |
| **Volumes** | Named persistent ext4 volumes, rw-exclusive / ro-shared | Same |
| **Managed Postgres** | Full — per-database microVM, native `postgres://` URL, REST query broker, continuous WAL archiving, daily base backups, restore-to-latest failover, credential rotation, idle auto-suspend | Same |
| **Database branching** | — | Branch a running database into an independent copy with a warm memory-forked cache |
| **Point-in-time restore** | Restore to latest (used by failover) | Backup browser, clone-to-new-database, and restore to any second in the retention window, with tiered retention policies |
| **Connection hardening** | Direct `postgres://` URL, TLS required | Adds per-database IP allow lists, connection rate limits, and a pooled (PgBouncer) connection URL |
| **Scheduler** | Multi-node scheduler, capacity scoring, admission control, leases | Same, running across a managed multi-region fleet |
| **Observability** | Audit log, events, metrics, Postgres + ClickHouse pipeline | Same, pre-wired and retained for you |
| **Usage limits** | None. Unmetered, uncapped, no billing code | Plan-based, with support and an SLA |
| **Operations** | You run the KVM hosts, object storage, upgrades, and backups | Managed fleet, managed upgrades, support |

Full breakdown: [Open source vs PandaStack Cloud](docs-site/content/docs/concepts/open-source-vs-cloud.mdx).

## Roadmap

- [x] Local Apple Silicon developer path with Lima and Firecracker smoke test.
- [x] Managed PostgreSQL 16 databases (Beta) — durable, per-DB microVM, native `postgres://`.
- [x] On-demand UFFD memory streaming — lazily page guest memory from object storage on restore.
- [x] Demand-paged rootfs streaming over an in-kernel NBD device.
- [ ] Read replicas and storage autoscaling for managed databases.
- [ ] Cross-host durability for volumes and databases (object-storage staging on attach).
- [ ] Single-node Linux self-host quickstart.
- [ ] Snapshot store adapters for additional object storage backends.
- [ ] Multi-node scheduler examples for Kubernetes, Nomad, and managed instance groups.
- [ ] 1.0 API stability and steering committee formation.

## Limitations & scope

This is the **open-source core** of PandaStack. A few things to know before you build on it:

- **Hosts must run on bare-metal KVM.** Firecracker needs `/dev/kvm`, so agents run on Linux KVM hosts or `*.metal` cloud instances. On Apple Silicon, local dev runs Firecracker inside a Lima VM via Apple Virtualization.framework (nested virt). There is no Windows/macOS-native host path.
- **No billing or metering.** This cut ships **unmetered and uncapped** — no Stripe, no subscription tiers, no per-workspace sandbox/CPU/quota limits, and no metering code to strip out. Self-hosters run without usage limits; if you need billing, that's a layer you add yourself.
- **Sandboxes and databases only.** Git-driven app hosting, serverless functions, cron schedules, PR preview environments, custom domains, the GitHub App, and managed env-secrets are **not part of this repository**. If you find a stale reference to any of them, it's a bug — please open an issue.
- **Three features are Cloud-only.** Database branching, point-in-time restore with a backup browser and retention policies, and database connection hardening (IP allow lists, connection rate limits, pooled connection URLs) are not in this repo. See [Open source vs PandaStack Cloud](#open-source-vs-pandastack-cloud) for exactly where the line is.
- **No SDKs in this repo.** The Python/TypeScript SDKs and CLI are published separately (`pip install pandastack` / `npm install @pandastack/sdk`). Here you interact with the platform over the REST API directly.
- **Auth is bring-your-own.** Ships with a `stub` mode (local dev) and JWT verification against a JWKS endpoint (e.g. Supabase). There's no built-in user database or signup flow — wire it to your own identity provider.
- **Single-tenant-ish by default.** Org/tenancy tables exist, but the access-control model is intentionally minimal. Review it before exposing the API to untrusted users.
- **Managed databases are Beta.** They work, and they are continuously archived — but read replicas, storage autoscaling, and cross-host durability are not here yet. Don't make a single Beta database the only copy of irreplaceable data.
- **Object-storage coupling.** Snapshot seeds, UFFD memory streaming, and rootfs streaming currently assume GCS (or an S3-compatible store). Other backends need an adapter (see Roadmap).
- **Deployment is Terraform-first.** Production deploys use the multi-node Terraform envs ([`infra/terraform/envs/dev-aws`](infra/terraform/envs/dev-aws), [`dev-gcp-multi`](infra/terraform/envs/dev-gcp-multi)). The cloud-init/user-data bootstrap scripts are functional scaffolds — review them before a real apply, and expect to adapt AMI/image baking to your environment.
- **The dashboard screenshot is an illustration**, not a product screenshot (see note above).

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), and [GOVERNANCE.md](GOVERNANCE.md) before opening a substantial PR.

## License

PandaStack is licensed under the [Apache License 2.0](LICENSE).

## Credits

PandaStack stands on excellent open-source systems and tools, including Firecracker, Lima, ClickHouse, Next.js, Postgres, Go, TypeScript, Python, Terraform, and the broader Linux virtualization ecosystem. Thank you to the maintainers and communities behind them.
