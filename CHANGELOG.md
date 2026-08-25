# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### ⚠️ Operator action required — re-bake every template snapshot

`agent/internal/sandbox/template_snap.go` now configures **MMDS on the build VM
in NATID mode too**, not only in vsock mode.

A Firecracker snapshot freezes device state at bake time. A snapshot baked
**before** this change carries no MMDS data store, so every restore from it
fails `PUT /mmds` with `400 The MMDS data store is not initialized`. That kills
the one identity-delivery channel independent of vsock, which in turn makes a
single vsock hiccup an unrecoverable restore.

**Every template snapshot baked before this change must be re-baked.** There is
no runtime migration — device state cannot be added to an existing snapshot.

```bash
# On each agent host, as root:
sudo FORCE=1 scripts/bake-templates.sh            # re-bake all templates
sudo FORCE=1 scripts/bake-templates.sh postgres-16 code-interpreter   # or a subset
```

`FORCE=1` is required; without it the baker skips templates that already have a
`rootfs.ext4`. Baking also purges `template-snaps/<name>/`, so the agent
re-bakes the snapshot on the next sandbox create.

### Changed

- **`DefaultTemplateCPU` is now `8`** (was `1`). This applies to custom and dev
  templates that do not carry an explicit `cpu` in their `meta.json`; all
  first-party templates set theirs explicitly and are unaffected. Every
  template runs burstable vCPUs fair-shared by cgroup `cpu.weight`, so a
  smaller value only weakened a sandbox's share without saving anything.
- Template catalog reduced to **four** first-party templates: `base`,
  `code-interpreter`, `agent`, `postgres-16`. `claude-agent`, `browser`, and
  the `postgres-16-4g` / `postgres-16-16g` RAM tiers are no longer part of the
  open-source catalog. Bake your own for those workloads.
- Managed databases surface the **direct** `postgres://` URL only (port 5432,
  full session semantics). A pooled PgBouncer URL is not part of this edition.

### Added

- Demand-paged **rootfs streaming** over an in-kernel NBD device, gated behind
  `PANDASTACK_STREAM_DISK=1`, with per-host egress guards
  (`PANDASTACK_NBD_EGRESS_MBPS`, `PANDASTACK_NBD_EGRESS_CAP_K`).
- Managed-database **credential rotation** (`POST /v1/databases/{id}/reset-credentials`)
  and **idle auto-suspend** with wake-on-connect, opt-in via
  `PANDASTACK_DB_IDLE_AFTER_SECONDS` and exemptable per database with
  `always_on`.
- Snapshot catalog endpoints: `GET /v1/snapshots`, `GET /v1/snapshots/{id}`,
  `DELETE /v1/snapshots/{id}`.
- Root-namespace egress guards: cross-tenant `FORWARD` drop, link-local
  (cloud-metadata) drop, and a Stratum mining-port denylist overridable with
  `PANDASTACK_BLOCKED_EGRESS_PORTS`.
- Docs: [Open source vs Cloud](docs-site/content/docs/concepts/open-source-vs-cloud.mdx)
  and a [Troubleshooting](docs-site/content/docs/troubleshooting/index.mdx) page.

### Removed

- All billing, metering, quota, subscription, and usage-tracking code and
  documentation. This edition ships **unmetered and uncapped**.
- Documentation for git-driven app hosting, serverless functions, cron
  schedules, PR preview environments, custom domains, the GitHub App, and
  managed env-secrets — none of which are part of this repository.
- The Claude Managed Agents guide and its example orchestrator, which depended
  on the dropped `claude-agent` template.
- The `browser` cookbook recipe, which depended on the dropped `browser`
  template.

### Fixed

- The README no longer advertises app hosting or serverless functions, and the
  repository-layout and template tables match what ships.
- `PANDASTACK_TOKEN` renamed to `PANDASTACK_API_KEY` throughout the docs,
  cookbook, and examples — that is the variable the SDKs and CLI actually read.
- Supabase auth docs rewritten against the real JWKS verification path
  (`SUPABASE_JWKS_URL`, `ES256`/`RS256`, `PANDASTACK_DB_DSN`); the previous page
  documented a shared-JWT-secret setup the API never implemented.
