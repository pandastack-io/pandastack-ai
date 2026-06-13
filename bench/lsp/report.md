# LSP Benchmark Report

CSV: `/Users/ajaykumar/Documents/firecracker-on-m3-mac/bench/lsp/results/run-20260531-125655.csv`

## Aggregate stats

| variant | pass rate | mean tokens | median tokens | median steps | hallucinations |
|---|---:|---:|---:|---:|---:|
| exec | 5/5 | 5592.2 | 1939.0 | 5.0 | 2 |
| lsp | 5/5 | 8376.6 | 6339.0 | 8.0 | 8 |

## Token delta (exec - lsp)

| task | delta |
|---|---:|
| 01_renamed_helper | -6749 |
| 02_wrong_attribute | -3347 |
| 03_moved_symbol | 3952 |
| 04_wrong_arg_order | -3136 |
| 05_typo_in_constant | -4642 |

## Methodology

Each task runs in a fresh PandaStack `code-interpreter` microVM. The exec variant exposes file, directory, shell, and submit tools. The lsp variant adds Python LSP definition/reference/hover/document/workspace-symbol tools over the sandbox WebSocket. Dry runs use a deterministic stub LLM and report zero token usage.
