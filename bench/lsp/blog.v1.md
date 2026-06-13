# Does LSP-as-a-Service actually make code agents smarter? We benchmarked it.

**TL;DR — On 5 real Python bug-fix tasks run inside PandaStack microVMs with `gpt-4o-mini`:**

- **Both variants solved 10/10 runs.** LSP didn't break anything.
- **On the only cross-file task** (a moved symbol), the LSP-enabled agent used **14% fewer tokens and one less step** to find and fix the bug.
- **On trivial single-file tasks**, LSP cost **~30% more tokens** — the agent paid for a richer toolbelt it didn't need.

The interesting question turned out not to be "does LSP help?" but "*when* does LSP help, and is it worth wiring up?".

---

## Why we built this benchmark

PandaStack ships [LSP-as-a-Service](https://api.pandastack.ai): every microVM exposes a Python language server over WebSocket at `/v1/sandboxes/{id}/lsp/python`. Your agent can call `definition`, `references`, `hover`, `documentSymbol` — the same primitives a human developer hits via Cmd-Click in VS Code.

We wanted to know if that actually changes outcomes for an LLM agent, or if it's a feature people *think* they want but never benefit from.

So we built an A/B harness:
- Two agents with the same model (`gpt-4o-mini`), same system prompt, same task, same sandbox.
- **Variant A — `exec`**: shell, read_file, write_file, list_dir, submit.
- **Variant B — `lsp`**: everything in A *plus* `lsp_definition`, `lsp_references`, `lsp_hover`, `lsp_document_symbol`, `lsp_workspace_symbol`.

Every run lives inside a fresh PandaStack microVM (boot ≈100ms, real network isolation), so the two variants are truly independent — no cross-contamination, no cached file system, no chance one agent learns from another's mistakes.

## The five tasks

We curated five small Python bugs that we expect a junior dev to fix in under a minute:

| # | Name | Bug type |
|---|---|---|
| 01 | `renamed_helper` | Function was renamed; caller uses the old name |
| 02 | `wrong_attribute` | Accessing `.foo` but field is `.foo_value` |
| 03 | `moved_symbol` | Symbol moved to a sibling module; import is stale |
| 04 | `wrong_arg_order` | `divide(0, 10)` instead of `divide(10, 0)` |
| 05 | `typo_in_constant` | `MAX_RETRIES` vs `MAX_RETRY` |

Each task has a failing test. An agent is "done" when it submits and the test goes green. We cap at 16 tool calls per run; a hallucination counter trips when the agent edits a file it never read.

## The numbers

After patching pylsp (1.14.0) into the microVM from 15 pre-downloaded wheels (no internet egress allowed), here's the head-to-head, one trial per cell:

| variant | pass rate | mean tokens | median tokens | median steps | hallucinations |
|---|---:|---:|---:|---:|---:|
| `exec` | 5/5 | 3,685 | 1,926 | 5 | 1 |
| `lsp`  | 5/5 | 4,168 | 2,840 | 5 | 1 |

Per-task token delta (`exec − lsp`, positive = exec used more):

| Task | Δ tokens | Winner |
|---|---:|---|
| 01 renamed_helper | −640 | exec |
| 02 wrong_attribute | −1,367 | exec |
| 03 moved_symbol | **+1,535** | **lsp** |
| 04 wrong_arg_order | −1,429 | exec |
| 05 typo_in_constant | −517 | exec |

## What the numbers mean

**On trivial bugs, LSP loses.** When the bug is right where the failing test points (`AttributeError: 'Foo' has no attribute 'foo'`), an exec-only agent reads one file, fixes the typo, and submits. The LSP variant has a fatter system prompt (extra tool schemas), so every assistant turn costs more tokens — and the savings from "skip grep, just ask the language server" never materialize because there's nothing to search for.

**On the cross-file bug, LSP wins.** Task 03 moves a symbol to a sibling module. The exec-only agent burned 14 steps and 11,242 tokens grepping for the symbol's new home; the LSP variant called `lsp_definition` once, got pointed at the new file, and shipped a fix in 13 steps using 9,707 tokens — a 14% token saving on the hardest task in the suite.

**Both variants hallucinated on task 03.** The agent edited a file it hadn't read in both runs. The LSP variant still got there faster, suggesting tool-aided navigation reduces — but doesn't eliminate — guessing.

## What we'd build differently in production

If you're shipping an autonomous coding agent and considering wiring up an LSP, here's our honest take based on this run:

1. **Gate it.** Don't expose LSP tools on every task. A cheap classifier on the failing test ("does the traceback name a symbol from another file?") gets you the wins without the schema tax.
2. **Skip `workspace/symbol` for vanilla pylsp.** It requires the `pylsp_rope` plugin, returns `-32601 Method Not Found` otherwise. We made our client gracefully degrade to an empty list.
3. **Trust `definition`, distrust `hover`.** Definition is deterministic and high-signal. Hover output is doc-heavy and your model already knows the stdlib.
4. **Measure tokens, not just pass rate.** Both variants solved everything. The only honest discriminator was cost.

## Reproducing this benchmark

Every file is in [bench/lsp/](.) in the repo. To run it yourself against any PandaStack workspace:

```bash
export OPENAI_API_KEY=sk-...
export PANDASTACK_API_BASE=https://api.pandastack.ai
export PANDASTACK_API_TOKEN=dam_...

# Dry-run with the stub LLM (no OpenAI cost) to verify plumbing:
python3 bench/lsp/harness.py --dry-run --variants both

# Real run:
python3 bench/lsp/harness.py --variants both --model gpt-4o-mini
```

Wheels for python-lsp-server live in `bench/lsp/wheels/` so the harness can install pylsp inside a microVM with **no internet egress** — the sandbox is hermetic.

Each run takes ~3–5 minutes for 10 trials and costs under $0.05 on `gpt-4o-mini`. The CSV at `bench/lsp/results/run-*.csv` has every per-task data point.

## What's next

We'll re-run with `gpt-4o` and a tougher task suite (multi-file refactors, cross-package imports, real OSS repos with `git bisect`-able regressions) to see if the LSP advantage compounds when the model is smarter and the search space is bigger. Watch this space.

— *The PandaStack team*
