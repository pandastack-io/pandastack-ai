# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

PandaStack is the open-source core of a self-hosted platform: disposable Firecracker microVM sandboxes plus managed PostgreSQL 16 (Beta), each database in its own microVM.

**OSS scope is sandboxes + databases only.** App hosting, serverless functions, cron, PR previews, custom domains, the GitHub App, managed env-secrets, and all billing/metering/quota code were deliberately removed — any leftover reference is a bug (known leftovers: `enforceQuotas` is a pass-through, `usage_events`/`workspaces` tables have no Go readers, the agent still accepts the `apps-api` auth method). Database branching, point-in-time restore, and DB connection hardening are Cloud-only. SDKs are published separately and are not in this repo. Commits use Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`).

## Modules and commands

No root `go.mod`/`go.work` — seven independent Go modules; always `cd` into one first (`GOTOOLCHAIN=auto` fetches the required toolchain). In CI and `make tidy`: `agent` (go 1.25.7), `api`, `db-proxy`, `cmd/pandastack` (user CLI). Not in CI: `templates/postgres-16/query-broker`, `bench/cold-start`, `scripts/lsp-probe`.

```bash
# What CI runs per module (agent on ubuntu only; api also on macOS)
cd agent && go vet ./... && go build ./... && go test ./...
# Single test
cd agent && go test ./internal/sandbox -run TestLifecycleStore -v -count=1
cd api   && go test ./cmd/api -run 'TestHealthzStatusOK$' -v -count=1   # most api tests are package main in cmd/api
# Opt-in Postgres store test (skips without a DSN)
cd agent && DATABASE_DIRECT_URL='postgres://pandastack:pandastack@localhost:5432/pandastack?sslmode=disable' \
  go test ./internal/store -run TestPostgresSandboxRoundTrip -v
make agent   # cross-compiles linux/arm64 for the Lima VM (pure Go, CGO_ENABLED=0, modernc SQLite)
# In-guest binaries: static, must match guest arch, no Makefile target
cd agent && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/pandastack-init ./cmd/pandastack-init   # likewise ./cmd/pandastack-daemon
```

Node apps (CI: Node 22, `npm ci && npm run build --if-present && npm test --if-present`; neither app has a test script):
```bash
cd dashboard && npm ci && npm run build && npx tsc --noEmit   # next build has ignoreBuildErrors — run tsc yourself
cd dashboard && NEXT_PUBLIC_PANDASTACK_API=http://localhost:8080 NEXT_PUBLIC_PANDASTACK_AUTH_MODE=stub npm run dev   # no Supabase
cd docs-site && nvm use && npm ci && npm run build   # Node >=20.19 (.nvmrc 22.13.0); dev server on :3010
cd docs-site && DOCS_TARGET=static npx next build    # static export to out/ (Cloudflare Pages)
```

**Platform requirements:**
- The agent module compiles only for `GOOS=linux` (firecracker-go-sdk pulls in Linux-only CNI netns). On macOS use `GOOS=linux go vet ./...` / `go build ./...` as a compile check and run agent tests on Linux/CI. `api`, `db-proxy`, `cmd/pandastack` build and test natively on macOS.
- Go unit tests need neither KVM nor root (`TestServer_RestartReacquire` needs root + `modprobe nbd` and self-skips).
- Running the agent, baking templates, and custom template builds need Linux, `/dev/kvm`, and root (loop mounts).

**Local dev loops.** Both e2e scripts run the API in stub auth with the real multi-node topology: Postgres + ClickHouse via `docker compose -f docker-compose.dev.yml`, the agent registers in Postgres (`PANDASTACK_DB_DRIVER=postgres`) and serves TCP :7070, and the API routes to it via `PANDASTACK_DB_DSN` + `X-Node-Token: pds_local_node_token`.
```bash
bash scripts/mac-local-e2e.sh     # Homebrew + Docker Desktop. Lima VM 'pandastack-lima-vm' (nested virt) runs agent+FC; forwards only TCP 7070/9100.
                                  # API :8080, dashboard :3000, db-proxy :5433; bakes base + postgres-16. ENABLE_DB_PROXY=0 skips dnsmasq/sudo.
                                  # State in .local-state/mac-local-e2e/. Teardown: scripts/mac-local-e2e-down.sh [--keep-vm]
bash scripts/linux-local-e2e.sh   # Ubuntu/Debian; uses sudo (apt installs Docker/Go/Node if missing, systemd unit, /etc/pandastack/local-e2e.env).
                                  # Requires rw on /dev/kvm (usermod -aG kvm). Teardown: scripts/linux-local-e2e-down.sh
docker compose -f docker-compose.dev.yml up -d postgres clickhouse   # data stores only (pandastack/pandastack)
TEMPLATE=base make test-api       # curl harness (scripts/api-tests.sh); without TEMPLATE it picks the first catalog entry (`agent`, unbaked → 500)
# Agent directly (no /v1 prefix):
curl -s -H 'X-Node-Token: pds_local_node_token' http://localhost:7070/sandboxes
curl -s --unix-socket /run/pandastack/agent.sock http://x/sandboxes   # Linux host; on Mac prefix with `limactl shell pandastack-lima-vm --`
```
Only baked templates can be created: `base`/`postgres-16` on the Mac stack. The Linux stack bakes only `ubuntu-24.04`, which is not in the API catalog (gateway returns 400), so create it against the agent directly.

Older paths are stale or partly broken — prefer the e2e scripts over `make setup/deploy-agent/api` (legacy Lima VM `pandastack`), `scripts/local/start-*.sh`, `docker-compose.yml`, and the api/dashboard services in `docker-compose.dev.yml`.

`make tf-{aws,gcp}-{apply,destroy}` run `-auto-approve` against real cloud accounts (`infra/terraform/envs/{dev-aws,dev-gcp-multi}`) — never run unless asked.

**Migrations and templates:**
```bash
cd agent && go run ./cmd/migrate --driver sqlite|postgres|pgx --dsn DSN up|down|status   # 'postgresql' is rejected
make pg-migrate-up | pg-migrate-status | pg-migrate-down   # uses .env.local DATABASE_DIRECT_URL; down drops everything
sudo PANDASTACK_INIT_BIN=... PANDASTACK_DAEMON_BIN=... bash scripts/build-base-rootfs.sh
sudo [FORCE=1] [BASE_NAME=base] bash scripts/bake-templates.sh [code-interpreter agent postgres-16]
```

## Architecture

**Control plane.** `api/` (`pandastack-api`) is a stateless gateway: cors → unifiedAuth → orgResolver (DSN mode only) → mux.
- **Auth:** `pds_` tokens, Supabase JWT via JWKS, or `PANDASTACK_AUTH_MODE=stub`. The gateway overwrites the identity headers agents trust: `X-Fcs-Workspace`, `X-Pandastack-User-Id`, `X-Pandastack-Auth-Method`.
- **Tenancy:** for jwt/stub callers, orgResolver rewrites `X-Fcs-Workspace` to the org slug (`X-Pandastack-Org` if a member, else 403; then `user_current_org`; then user id). `pds_` tokens keep their own workspace. The agent's `workspaceScope` isolates on this header and treats `""`/`admin`/`default` as unscoped.
- **Routing:** `/v1/me/tokens`, `/v1/metrics/*`, and MCP are gateway-owned; any other unregistered `/v1/*` path is reverse-proxied to an agent with `/v1` stripped.

`PANDASTACK_DB_DSN` is the mode switch:
- **Unset:** single-agent proxy over the Lima socket (`~/.lima/pandastack/sock/agent.sock` or `--agent-url`), file tokens; templates/snapshots/databases fall through to the agent.
- **Set:** tokens in Postgres; gateway registers orgs, databases, snapshots, templates catalog, natid routes. It intercepts `POST /v1/sandboxes` (400 unless the template has a `templates` row, global or in the caller's workspace) and records `POST /v1/sandboxes/{id}/snapshots`. `MultiNodeDirector` routes id-scoped requests via the `leases` table and everything else via `scheduler.Pick` (capacity scoring from the `agents` heartbeat table), over HTTP to each agent's TCP listener with `X-Node-Token`.

**Loopback calls.** Gateway handlers call agents in-process (`agentCall`), buffering the full response in an `httptest.ResponseRecorder` — never use it for streaming. They stamp `X-Pandastack-Auth-Method` as `db-api`/`templates-api`/`mcp`. The agent honors this in only two places: `templates-api` (and stale `apps-api`) may set reserved metadata keys `kind`/`app.id`; `db-api` (or `apps-api`) on `DELETE /sandboxes/{id}` bypasses managed-sandbox delete protection (otherwise 409 `ErrManagedSandbox`).

**Agent** (`agent/cmd/agent`, `pandastack-agent`) is the per-host data plane.
- HTTP on a unix socket (0666, unauthenticated unless `SUPABASE_JWKS_URL` set); TCP only when both `PANDASTACK_LISTEN_TCP` and `PANDASTACK_NODE_TOKEN` are set.
- Middleware: metrics → rateLimit → audit → workspaceScope → enforceQuotas → activityTracker.
- Multi-node registration (`agents` row, 10s heartbeat, lease sink/sweeper) needs `PANDASTACK_DB_DRIVER` and a resolvable endpoint (`PANDASTACK_AGENT_ENDPOINT` or `PANDASTACK_INTERNAL_IP|hostname` + listen-tcp); otherwise it logs "registration skipped" and is never scheduled.
- `PANDASTACK_AGENT_ID` (hostname fallback) must be unique per node — it's the row-ownership boundary, so per-agent loops use `ListSandboxesForAgent`, not `ListSandboxes`.
- Boot order in `main.go`: Recover → slot Reconcile → orphan netns/loop sweep → NATID prewarmer. Keep it.

**Sandbox lifecycle.**
- **Create** restores the baked per-template snapshot (NATID fast path when `PANDASTACK_NATID=1`) or cold-boots and bakes the snapshot in the background. Requested cpu/memory are overridden by the template's `meta.json` (snapshots can't resize); disk can only grow.
- **TTL** is *idle* TTL (default 300s); the reaper *deletes* ephemeral sandboxes.
- **Hibernation** has three triggers: the idle sweeper (`PANDASTACK_IDLE_AFTER`, off by default, `persistent:true` only, skips live pg-tunnels); the managed-DB postgres-activity sweep (`PANDASTACK_DB_IDLE_AFTER_SECONDS`, off by default); and the PSI pressure ladder (on by default, `PANDASTACK_PRESSURE_LADDER=0` disables, inert without CPU tiers) which hibernates idle plain sandboxes under critical memory pressure.
- **Pause** freezes in place; **hibernate** snapshots to `vms/<id>/hibernation/` and stops FC; `/stop`/`/start` alias hibernate/wake; activityTracker auto-wakes on any use.
- The agent can't re-attach to live FC processes after a restart — running ephemeral sandboxes become `failed`.

**Snapshots and object storage.**
- Baked snapshots live in `<data>/template-snaps/<tpl>/`. `vm.state` holds absolute paths under `build-vm/` — never delete `build-vm/` or punch holes in its rootfs (only in `clone.ext4`).
- A snapshot is "ready" only if flavor, cpu/mem/disk, and SSH key fingerprint all match; toggling NATID or rotating the key silently forces a rebake.
- `pandastack-agent seed-sync` pulls seeds from `gs://$PANDASTACK_GCS_BUCKET/seeds/` before service start (its streaming env must match the service's).
- UFFD memory streaming (`uffd`→`memstream`), NBD rootfs streaming (`nbdstream`→`diskstream`), and `snapstore` snapshot/fork mirroring (`PANDASTACK_SNAPSHOT_BUCKET`) all require a bucket. Object storage is **GCS-only** (gcloud/gsutil + metadata-server tokens) — there is no S3 code despite README wording.

**Host↔guest.**
- The base rootfs (inherited by baked templates) carries `pandastack-init` (one-shot identity setup over vsock/MMDS) and `pandastack-daemon` (vsock port 5252, `vsockwire` framed protocol: exec + 5 fs ops).
- `guest.Client` primarily uses SSH to guest sshd with the agent's single ed25519 key baked into the rootfs. With `PANDASTACK_VSOCK_EXEC` truthy (off in all shipped configs), `Exec`/`ReadFile`/`WriteFile`/`DeletePath`/`ListDir`/`Stat` try vsock first and fall back to SSH. `ExecStream` (SSE), `OpenPTY` (terminal WebSocket), and `OpenProc` (LSP, persistent REPL) are always SSH.
- **NATID networking:** each sandbox gets netns `ns-*` with a /30 veth from 10.200.0.0/16, DNATed to the guest IP baked into the snapshot (stored `guest_ip` is the veth proxy IP). Slot indices live in a boot-disk SQLite `slotstore` (`-slot-db`). Egress DROPs (cross-tenant, 169.254/16, mining ports) exist only on the NATID path.

**Persistence.**
- Agent `internal/store` owns the metadata DB: SQLite single-node, or shared Postgres (`PANDASTACK_DB_DRIVER=postgres|pgx` + `PANDASTACK_DB_DSN`). Embedded goose migrations from `agent/migrations/{sqlite,postgres}` run on every agent start.
- The API has no goose migrations; at boot it runs idempotent `SetupSchema` DDL for its own tables (api_tokens, orgs/org_members/org_invites/user_current_org, templates, template_builds, agent_natid_claims, db_archive_*/db_janitor_state/db_backup_health). It reads agent-owned `agents`/`leases`/`sandboxes` read-only, but also `ADD COLUMN`s to and writes the agent-owned `snapshots` table — keep both sides compatible (e.g. `created_at` stays BIGINT epoch).
- ClickHouse (`PANDASTACK_CLICKHOUSE_URL`, database `pandastack`) is an optional best-effort sink: agent writes events/metrics, API writes `http_requests` and serves `/v1/metrics/*`.
- Prometheus on the private `obs.Reg` registry; OTel when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The `obs` and `clickhouse` packages are duplicated in agent and api — mirror changes by hand (`schema.sql` has already drifted).

**Managed PostgreSQL** = a persistent sandbox on the allowlisted `postgres-16` template; DB id = sandbox id; requires DSN mode.
- **Two-phase boot:** the template snapshot is taken with Postgres stopped. On create the agent patches a durable `<data>/volumes/db/<id>.ext4` onto the baked `vol1` placeholder, then `kickPGPhase2` delivers credentials + WAL env via `/run/pandastack/*.new`. `templates/postgres-16/scripts/autostart.sh` runs initdb (or reuses the cluster), starts Postgres, PgBouncer, and the query broker, and writes `/run/pandastack/ready.json`.
- **Credentials** are never stored by the control plane; the API reads them on demand via `GET /sandboxes/{id}/postgres-info` (from `ready.json`).
- **Native connections:** `postgres://<id>.db.<domain>` → `db-proxy` (requires SSLRequest, routes on TLS SNI, looks up the lease) → `GET /sandboxes/{id}/pg-tunnel` HTTP Upgrade to the agent (wakes the DB).
- **REST:** `/v1/databases/{id}/proxy/...` skips unified auth, uses the `pds_pg_…` broker token, and reaches the in-VM `pds-query-broker` on :5544 via the agent port proxy.
- **WAL:** guest `archive_command` → agent WAL relay on :7071 → `gs://$PANDASTACK_SNAPSHOT_BUCKET/db/<id>/`. **Failover** (status `failed` or `{"force":true}`) bumps the archive generation, hibernates the old owner, and calls `/db/{id}/restore` on another agent to rebuild from base backup + WAL under the same id.

**Dashboard and docs.**
- `dashboard/` (Next.js 16, React 19) is almost entirely client components calling `${NEXT_PUBLIC_PANDASTACK_API}/v1/...` directly from the browser — no server-side proxy. Auth mode is baked at build time (Supabase default; `NEXT_PUBLIC_PANDASTACK_AUTH_MODE=stub` needs the API in `PANDASTACK_AUTH_MODE=stub`). Current org lives in localStorage and is sent as `X-Pandastack-Org`. `[id]` pages and route handlers use `runtime = 'edge'` for Cloudflare next-on-pages.
- `docs-site/` is fumadocs: MDX in `content/docs`, sidebar is an explicit list in `content/docs/meta.json` (add new pages there), `public/sitemap.xml` is hand-maintained.
- The dashboard's `npm run build` first regenerates the committed `public/support-index.json` from `../docs-site` MDX.

## Conventions and gotchas

- **Migrations:** every schema change goes into BOTH `agent/migrations/postgres` and `agent/migrations/sqlite`, using the next number *in each dir* (numbering has diverged). Never fill the postgres gaps (00011–00013, 00016) — goose rejects out-of-order migrations and the agent won't start. Name the mirror file in a comment. A new `sandboxes` column also needs `sandboxCols`, `sandboxRow`, and `scanSandbox` in `store.go`; `UpdateSandbox` must never write `created_at`.
- **Store SQL** is SQLite dialect with `?` placeholders; a shim rewrites to `$N` for pgx and translates only `INSERT OR IGNORE` and the allocations `INSERT OR REPLACE`. Write other upserts as `ON CONFLICT`; never use the JSONB `?` operator.
- **Test env leaks:** `store.Open` reads `PANDASTACK_DB_DRIVER`/`PANDASTACK_DB_DSN` from the environment, and `store_pg_test` auto-loads repo-root `.env.local` — `go test ./...` can write to a real DB. api healthz tests fail if the shell exports `PANDASTACK_DB_DSN` (without a DSN `/healthz` returns 503; `/readyz` is the real readiness check).
- **Adding endpoints:**
  - Agent: `registerX(mux, mgr)` in `agent/internal/api/<feature>.go`, wired from `NewRouter` in `router.go`; logic on `sandbox.Manager`.
  - Gateway: `api/cmd/api/<feature>.go` (single `package main`), `Register(mux)` called from `main.go` before the `/v1/` catch-all, inside `if pgDB != nil` if it needs Postgres.
  - Any added/removed api `HandleFunc("METHOD /v1/...")` must be mirrored in `api/cmd/api/openapi.json` — drift tests enforce it, and documented `/v1/sandboxes`/`/v1/volumes` paths are checked against agent route literals.
  - A new read-only GET under `/sandboxes/{id}` must be added to `readOnlyInspectionGETs` (`router.go`), or polling it counts as activity and auto-wakes the sandbox.
  - New dashboard request headers must be added to the API's CORS allow-headers.
  - `PANDASTACK_AUTH_SKIP_PREFIXES` replaces (not appends to) the default skip list; skipped paths can forge identity headers, so never put `/v1` paths in it.
- **Build tags:** Linux-only code uses `//go:build linux` paired with `*_other.go`/`*_stub.go` (uffd, nbdstream, sandbox clone/cpupin, init/daemon vsock). Change both sides. Linux-only tests: agent `internal/api` middleware/lsp, uffd, nbdstream.
- **Agent config:** `config.Config` holds only 5 flags; the 70+ `PANDASTACK_*` env vars are read ad hoc across packages — grep for the name.
- **Deploy env is duplicated:** GCP (`dev-gcp-multi`) boots from `cloud-init/user-data-*.sh`, AWS (`dev-aws`) has its own `user-data-*.sh.tftpl` copies, and `ansible/group_vars/all.yml` also sets agent env — propagate new env vars to all. On AWS, `PANDASTACK_GCS_BUCKET`/`PANDASTACK_SNAPSHOT_BUCKET` stay empty (never point at S3), so seeds, snapshot mirroring, WAL archive/failover, and streaming are disabled there.
- **Managed-DB template allowlist** is duplicated in four places: agent (`pgManagedTemplates`/`phasedBootTemplates`, strict), `api/cmd/api/databases.go`, db-proxy (prefix match), `bake-templates.sh`.
- **postgres-16 guest files** are built both by its Dockerfile and by `bake-templates.sh tpl::postgres-16` (used locally and in deploys); they've drifted — change both. Agent code (`db_monitor`, `wal_relay`, `db_idle`) hardcodes the guest layout.
- **Guest protocol:** `pandastack-daemon` handlers must stay byte-identical to the SSH commands in `guest/ssh.go`. `vsockwire` has no version negotiation — a protocol change needs a base rootfs rebuild (`FORCE=1`), template re-bake, and purge of `template-snaps/`.
- **Template sizing** is defined in both `bake-templates.sh` (`CPU_COUNT`/`MEMORY_MB`) and `api/cmd/api/templates.go`; RAM is frozen at bake time — rebake with `FORCE=1`.
- **License headers:** new source files start with `// SPDX-License-Identifier: Apache-2.0`. `scripts/add-license-headers.py` backfills agent/api/dashboard only — add it by hand in db-proxy and cmd/pandastack.
- **Stale config — trust code instead:** `PANDASTACK_POSTGRES_URL`, `PANDASTACK_LISTEN`, `PANDASTACK_TOKENS_FILE`, `NEXT_PUBLIC_API_BASE_URL` appear in compose/`.env.local.example`/Dockerfiles but are never read (see `main.go` flags and `dashboard/src/lib/api.ts`). `deploy/Dockerfile.{api,agent}` use golang:1.23 (below go.mod); `dashboard/Dockerfile` copies `.next/standalone`, which next.config never produces.
- **ClickHouse `schema.sql`** is split naively on `;` — no semicolons in comments, no comment directly before a statement.
- **Next.js:** docs-site has both `next.config.mjs` (loaded) and `next.config.ts` (ignored) — edit the `.mjs`. `.source/` is generated by fumadocs-mdx, so docs-site tsc errors until dev/build has run once. Keep `dashboard/src/middleware.ts`; Next 16's `proxy.ts` is Node-runtime only and breaks the next-on-pages edge build.
- **CI:** Go/Node CI and secret scans (gitleaks, TruffleHog over full PR history) run only on `pull_request`. The Firecracker e2e workflow is manual (`workflow_dispatch`). Templates are never built in CI.
