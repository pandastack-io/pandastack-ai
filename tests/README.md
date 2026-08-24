# PandaStack tests

Unit tests live inside each Go module:

- `agent/` — `go test ./...`
- `api/` — `go test ./...`
- `db-proxy/` — `go test ./...`
- `cmd/pandastack/` — `go test ./...`

The client SDKs are published from a separate repository and are not vendored
here, so they are not part of this suite.

This top-level folder orchestrates **end-to-end** tests against a real PandaStack
deployment (cloud or local). E2E tests are gated behind env vars so they never
run by accident in CI.

## Quick start

```bash
# point to a deployment
export PANDASTACK_API=http://localhost:8080        # local (scripts/mac-local-e2e.sh)
# export PANDASTACK_API=https://<your-control-plane>
export PANDASTACK_TOKEN=pds_...                    # from `pandastack auth login`
export PANDASTACK_E2E=1

./tests/e2e/run-all.sh
```

What it does:

1. Builds the CLI (`bin/pandastack`) if missing
2. Runs the CLI smoke tests (`tests/cli/*.sh`): create → exec → logs → delete

## Layout

```
tests/
├── README.md
├── e2e/
│   └── run-all.sh           # orchestrator
└── cli/
    └── smoke.sh             # bash CLI lifecycle: create/exec/logs/delete
```

## Skipping

If `PANDASTACK_E2E` is unset, `run-all.sh` exits 0 with a warning.

## Other harnesses

- `scripts/api-tests.sh` — curl harness over the REST surface (`make test-api`).
  Set `TEMPLATE=<name>` to pin the template under test.
- `bench/integration.sh` — live API+agent integration assertions.
