# Benchmark Methodology

All public performance numbers on the marketing site, docs, and README are
sourced from runs of `scripts/bench_boot.py` against the production endpoint
`api.pandastack.ai`.

## How to reproduce

```bash
export PANDASTACK_TOKEN=<your-token>
export BENCH_N=50
export BENCH_TEMPLATE=ubuntu-24.04-net   # or any template id
python3 scripts/bench_boot.py > bench-$(date -u +%Y-%m-%dT%H%MZ)-$BENCH_TEMPLATE.json
```

The script does N serial trials (so the host quiesces between trials),
records:

- `boot_ms` — server-measured Firecracker boot time, returned in the create
  response. This is the pure microVM cold-boot number, comparable to AWS
  Lambda's "init duration".
- `wall_ms` — client wall-clock from `POST /v1/sandboxes` to `201` response.
  Includes Cloudflare → GCP us-central1 round-trip latency, so it varies a lot
  by client location.
- `exec_ms` — round-trip for `POST /v1/sandboxes/{id}/exec` running
  `echo $((1+1))`.

## Current published numbers (June 1, 2026)

From `scripts/bench-results/bench-2026-06-01T0936Z-ubuntu.json` (n=50,
ubuntu-24.04-net, snapshot-natid path, run from a London-based laptop over
public internet):

| metric | min | p50 | p90 | p99 | max |
|--------|-----|-----|-----|-----|-----|
| `boot_ms` | 157 | 179 | 188 | 195 | 203 |
| `wall_ms` | 663 | 756 | 1814 | 4239 | 4739 |

The `boot_ms` numbers are what marketing claims as "~180ms cold start" — they
match what a self-hoster on a Linux box with KVM gets locally. The `wall_ms`
numbers are what a client geographically far from us-central1 will actually
experience due to network round-trips.

## When to re-run

Re-run before any major marketing push and update:

- `marketing/components/boot-ticker.tsx` — header stats
- `marketing/components/benchmarks.tsx` — bar chart data array + wall_ms footnote
- `marketing/components/features.tsx` — feature card subtitle
- `marketing/app/benchmarks/page.tsx` — comparison table p50/p99 row
- `marketing/app/features/page.tsx` — lifecycle card
- `marketing/app/templates/page.tsx` — per-template boot column (currently
  showing a flat `~180 ms` because we don't have per-template runs)
- `docs-site/content/docs/concepts/sandbox-lifecycle.mdx` — "Cold create →
  running" line
- `docs-site/content/docs/index.mdx` — feature bullet
- `docs-site/content/docs/concepts/networking-natid.mdx` — opening line
- `README.md` — table + quickstart comment

Commit the raw `bench-results/*.json` artifact so we have an audit trail.
