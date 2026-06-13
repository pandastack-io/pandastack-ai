# PandaStack Cookbook

Runnable end-to-end examples for every PandaStack sandbox template.: each
example is a self-contained `main.py` that creates a sandbox, does something
useful, prints a summary, and exits 0. The same files are CI'd via
`test_all.py` against `https://api.pandastack.ai`.

## Quickstart

```bash
pip install pandastack
export PANDASTACK_TOKEN=pds_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
export PANDASTACK_API=https://api.pandastack.ai    # optional, this is the default

# Run any single example
python cookbook/code-interpreter/main.py

# Run every template's example as a single test sweep
pytest cookbook/test_all.py -v
```

## Examples by template

| Template | What it shows | Needs LLM creds? |
|---|---|---|
| [`base`](./base/) | Universal apps runtime — Node 22, Python 3.12, Go, Bun via mise | no |
| [`code-interpreter`](./code-interpreter/) | Python REPL with persistent kernel state | no |
| [`agent`](./agent/) | Every coding-agent CLI in one image: `claude-code`, `codex`, `opencode` | optional (`ANTHROPIC_API_KEY` / `OPENAI_API_KEY`) |
| [`browser`](./browser/) | Headless Chromium screenshot + scraping (Playwright + crawl4ai) | no |

> `postgres-16` is a managed-database template accessed via the Databases API
> (`POST /v1/databases`), not `Sandbox.create()`, so it has no cookbook entry
> here — see [the databases docs](../docs-site/content/docs/concepts/databases.mdx).

## What gets tested

`pytest cookbook/test_all.py` walks every subdirectory, runs `main.py`, and
asserts:

1. Exit code 0
2. Stdout contains the per-example `EXPECTED:` line
3. Cleans up the sandbox afterwards

Examples that require LLM credentials are auto-skipped when the relevant env
var is missing.
