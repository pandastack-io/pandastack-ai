# Benchmark Methodology

Performance numbers quoted in the docs and README are sourced from runs of
`scripts/bench_boot.py` against a live deployment.

## How to reproduce

```bash
export PANDASTACK_API=http://localhost:8080   # or your control-plane URL
export PANDASTACK_TOKEN=<your-token>
export BENCH_N=50
export BENCH_TEMPLATE=ubuntu-24.04-net        # or any template id
python3 scripts/bench_boot.py > bench-$(date -u +%Y-%m-%dT%H%MZ)-$BENCH_TEMPLATE.json
```

The script does N serial trials (so the host quiesces between trials),
records:

- `boot_ms` — server-measured Firecracker boot time, returned in the create
  response. This is the pure microVM cold-boot number, comparable to AWS
  Lambda's "init duration".
- `wall_ms` — client wall-clock from `POST /v1/sandboxes` to `201` response.
  Includes network round-trip latency between the client and the control
  plane, so it varies a lot by client location.
- `exec_ms` — round-trip for `POST /v1/sandboxes/{id}/exec` running
  `echo $((1+1))`.

## Reference run (June 1, 2026)

From `scripts/bench-results/bench-2026-06-01T0936Z-ubuntu.json` (n=50,
ubuntu-24.04-net, snapshot-natid path, run from a laptop over the public
internet against a cloud deployment):

| metric | min | p50 | p90 | p99 | max |
|--------|-----|-----|-----|-----|-----|
| `boot_ms` | 157 | 179 | 188 | 195 | 203 |
| `wall_ms` | 663 | 756 | 1814 | 4239 | 4739 |

`boot_ms` is the "~180 ms cold start" figure — it matches what a self-hoster on
a Linux box with KVM gets locally. `wall_ms` is what a client geographically far
from the control plane will actually experience, due to network round-trips.

## When to re-run

Re-run after any change to the boot path (snapshot restore, NATID allocation,
streamed rootfs) and update the numbers quoted in:

- `docs-site/content/docs/concepts/sandbox-lifecycle.mdx` — "Cold create →
  running" line
- `docs-site/content/docs/index.mdx` — feature bullet
- `docs-site/content/docs/concepts/networking-natid.mdx` — opening line
- `README.md` — table + quickstart comment

Commit the raw `bench-results/*.json` artifact so there is an audit trail.
