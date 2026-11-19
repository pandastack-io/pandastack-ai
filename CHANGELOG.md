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

### ⚠️ Custom images must bring their own init and sshd

A custom template image that has no `/sbin/init` or no `/usr/sbin/sshd` is now
**refused** at build time. Until now the agent fixed such an image up by
`chroot`-ing into its filesystem on the host and running the image's own `apt`
there as root, with the host's `/dev`, `/proc` and `/sys` bound in: whoever
built the image could run code as root on the agent. Add one line to your
Dockerfile:

```dockerfile
RUN apt-get update && apt-get install -y --no-install-recommends systemd-sysv openssh-server
```

If you are the only person who builds templates on your deployment, the old
behaviour is still available with `PANDASTACK_TEMPLATE_HOST_INSTALL=1` in the
agent's environment. Do not set it on a host that bakes images from people you
would not give a root shell.

### Security

- Every guest disk the agent mounts on the host to prepare it is mounted
  `nosuid,nodev,noexec`, and every write into one (the agent's
  `authorized_keys`, the sshd drop-in, `resolv.conf`) goes through
  `internal/guestfs`, which refuses a path that leaves the image through a
  symbolic link. An image with `root/.ssh -> /root/.ssh` used to send the
  agent's key into the **host's** `authorized_keys`.
- `GET /v1/templates/builds`, `GET /v1/templates/builds/{id}` and its log
  stream now answer only for the caller's own workspace. The agent keeps every
  workspace's builds in one list, and the API used to pass it through whole.
- A template build under a name that a first-party template or another
  workspace already holds is refused by the API before an agent does any work.

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

- **Coding agents by name**: `claude`, `codex`, `opencode`, `gemini`, `grok`,
  `amp` and `copilot` are accepted as template names and start the `agent`
  image. They are aliases resolved in the API (no second rootfs, no second
  snapshot); the name asked for is kept on the sandbox as
  `metadata.template_alias`, and the catalog lists each with `alias_of`.
- Dashboard: a **Now** page (the new home) that draws every sandbox and
  database as a line on one clock, a **command bar** (`⌘K` or `/`) that takes
  the CLI's own words, a shell in a sandbox from the page, and "this screen, as
  code". The dashboard is dark only; the light theme is gone.
- `exec/stream`, `logs?follow=1` and `events?follow=1` send a `: keep-alive`
  comment every 15 s (`PANDASTACK_SSE_KEEPALIVE_SEC`), so a proxy in front does
  not drop a quiet stream.

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

- `exec/stream` could corrupt its response when a command wrote to stdout and
  stderr at the same time: the two pumps wrote to one connection without a
  lock. All SSE streams now share one writer (`internal/sse`).
- `timeout_seconds` on exec now really stops the command in the guest (TERM,
  then KILL 2 s later) and reports `timed_out: true`. The old path sent an SSH
  signal request, which sshd ignores for a root login.
- Output that ends in the middle of a multi-byte character is no longer split
  across two stream events.
- Boot-time percentiles use nearest rank computed in integers, so `p50` of an
  even number of samples is the lower middle one.
- The confirm dialog in the dashboard could be answered by the same Enter key
  press that opened it.
- Several agent tests depended on the machine they ran on (a database written
  next to the sources, a real `gcloud` on `PATH`).

- The README no longer advertises app hosting or serverless functions, and the
  repository-layout and template tables match what ships.
- `PANDASTACK_TOKEN` renamed to `PANDASTACK_API_KEY` throughout the docs,
  cookbook, and examples — that is the variable the SDKs and CLI actually read.
- Supabase auth docs rewritten against the real JWKS verification path
  (`SUPABASE_JWKS_URL`, `ES256`/`RS256`, `PANDASTACK_DB_DSN`); the previous page
  documented a shared-JWT-secret setup the API never implemented.
