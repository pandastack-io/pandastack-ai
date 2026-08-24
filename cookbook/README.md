# PandaStack Cookbook

Runnable end-to-end examples for every PandaStack sandbox template: each
example is a self-contained `main.py` that creates a sandbox, does something
useful, prints a summary, and exits 0. The same files run as a test sweep via
`test_all.py` against whatever `PANDASTACK_API` points at — your own
self-hosted control plane, or the hosted API.

## Quickstart

```bash
pip install pandastack
export PANDASTACK_API_KEY=pds_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
export PANDASTACK_API=http://localhost:8080         # your control plane (defaults to the hosted API)

# Run any single example
python cookbook/code-interpreter/main.py

# Run every template's example as a single test sweep
pytest cookbook/test_all.py -v
```

## Examples by template

| Template | What it shows | Needs LLM creds? |
|---|---|---|
| [`base`](./base/) | Universal language runtime — Node 22, Python 3.12, Go, Bun via mise | no |
| [`code-interpreter`](./code-interpreter/) | Python REPL with persistent kernel state | no |
| [`agent`](./agent/) | Every coding-agent CLI in one image: `claude-code`, `codex`, `opencode` | optional (`ANTHROPIC_API_KEY` / `OPENAI_API_KEY`) |

These are the four first-party templates. `postgres-16`, the fourth, is a
managed-database template accessed via the Databases API (`POST /v1/databases`),
not `Sandbox.create()`, so it has no cookbook entry here — see
[the databases docs](../docs-site/content/docs/concepts/databases.mdx).

For browser automation or anything else not covered by a first-party template,
[bake your own](../docs-site/content/docs/concepts/templates.mdx#bake-your-own).

## What gets tested

`pytest cookbook/test_all.py` walks every subdirectory, runs `main.py`, and
asserts:

1. Exit code 0
2. Stdout contains the per-example `EXPECTED:` line
3. Cleans up the sandbox afterwards

Examples that require LLM credentials are auto-skipped when the relevant env
var is missing.
