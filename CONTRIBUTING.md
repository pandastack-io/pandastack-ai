# Contributing to PandaStack

Thanks for helping make PandaStack better. We welcome bug reports, docs fixes, examples, and focused pull requests.

## Development setup

The fastest local path on Apple Silicon is:

```bash
bash scripts/mac-local-e2e.sh
```

That starts Postgres, ClickHouse, the API, dashboard, a Lima VM, the agent, and a real Firecracker smoke test. For the long-form walkthrough, see `docs-site/content/docs/getting-started/local-mac-apple-silicon.mdx`.

## Commit conventions

Use [Conventional Commits](https://www.conventionalcommits.org/):

- `feat: add sandbox hibernate endpoint`
- `fix: handle empty exec output`
- `docs: expand local setup guide`
- `test: cover scheduler fallback`

## Pull request process

1. Fork the repository.
2. Create a focused branch.
3. Open a PR with a clear description of what changed and why.
4. Include tests or explain why the change is docs/config-only.
5. Keep CI green.
6. Respond to review feedback; maintainers merge after approval.

## Testing requirements

Before opening a PR, run the checks that apply to the files you changed:

```bash
(cd agent && go test ./...)
(cd api && go test ./...)
(cd dashboard && npm test)       # if a test script exists
(cd docs-site && npm run build)
```

For local runtime changes, also run the smoke path:

```bash
bash scripts/mac-local-e2e.sh
```

## Code style

- Go: `gofmt` and `go vet`.
- TypeScript/JavaScript/MDX: Prettier defaults.
- Python: Ruff formatting/linting when available.
- Keep changes small, readable, and directly tied to the issue or PR.
