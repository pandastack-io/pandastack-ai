# LSP A/B benchmark

Benchmark OpenAI Python bug-fixing agents with and without Python LSP tools inside live PandaStack microVM sandboxes.

## Setup

```bash
pip install -r bench/lsp/requirements.txt
export OPENAI_API_KEY=sk-...
```

## Smoke tests without OpenAI tokens

```bash
python bench/lsp/harness.py --dry-run --variants exec --tasks 01
python bench/lsp/harness.py --dry-run --variants lsp --tasks 01
```

## Full benchmark

```bash
python bench/lsp/harness.py --variants both
```

Useful flags: `--model gpt-4o-mini`, `--out bench/lsp/results/run-custom.csv`, `--tasks 01,03,05`, `--max-steps 25`, `--token "$PANDASTACK_TOKEN"`.

Outputs are a CSV plus `bench/lsp/report.md` with aggregate pass-rate, token, step, and hallucination metrics.

Notes discovered against the live API: file upload/read is `/v1/sandboxes/{id}/fs?path=...`; `/fs/files` returns 404. Some dry-run sandboxes also lack PyPI DNS egress, so dry-run LSP smoke tests install a tiny in-sandbox `pylsp` shim only when `python-lsp-server` is absent. Full benchmark runs use the prod LSP bootstrap path.
