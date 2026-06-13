# PandaStack tests

Unit tests live inside each SDK / module:

- `sdks/python/tests/` — pytest (25 tests)
- `sdks/typescript/tests/` — vitest (13 tests)
- `cmd/pandastack/` — `go test ./...`

This top-level folder orchestrates **end-to-end** tests against a real PandaStack
deployment (cloud or local). E2E tests are gated behind env vars so they never
run by accident in CI / `make test`.

## Quick start

```bash
# point to a deployment (any of these will work)
export PANDASTACK_API=https://api.pandastack.ai      # prod
# export PANDASTACK_API=http://localhost:8080        # mac-m1 local
export PANDASTACK_TOKEN=psk_...                      # from `pandastack auth login`
export PANDASTACK_E2E=1

./tests/e2e/run-all.sh
```

What it does:

1. Builds the CLI (`bin/pandastack`) if missing
2. Runs Python SDK e2e: `pytest sdks/python/tests/e2e`
3. Runs TS SDK e2e: `pnpm/npm test --filter e2e` in `sdks/typescript`
4. Runs CLI smoke tests (`tests/cli/*.sh`): create → exec → logs → delete

## Layout

```
tests/
├── README.md
├── e2e/
│   └── run-all.sh           # orchestrator (Python + TS + CLI)
└── cli/
    └── smoke.sh             # bash CLI lifecycle: create/exec/logs/delete
```

## Skipping

If `PANDASTACK_E2E` is unset, `run-all.sh` exits 0 with a warning.
Individual SDK e2e tests are `@pytest.mark.e2e` (Python) / `describe.skipIf` (TS)
so unit-test runs are unaffected.
