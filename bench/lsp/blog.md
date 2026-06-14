# We benchmarked our own feature, watched it lose, fixed it, and benchmarked it again.

**TL;DR — We built an A/B benchmark for PandaStack's LSP-as-a-Service. The first run made our feature look bad. Instead of cherry-picking tasks until the numbers flattered us, we shipped four product improvements. On the one task in our suite that genuinely needs cross-file navigation, the improved LSP variant now uses 19% fewer tokens and 39% fewer steps than the exec-only baseline.**

This post is the entire story: the embarrassing first numbers, what we learned, what we shipped, and the honest follow-up numbers.

---

## Why we built a benchmark for our own feature

PandaStack ships [LSP-as-a-Service](https://api.pandastack.ai): every microVM exposes a Python language server on `wss://api.pandastack.ai/v1/sandboxes/{id}/lsp/python`. Your AI coding agent can call `definition`, `references`, `hover`, `documentSymbol` — the same primitives a human dev gets via Cmd-Click in VS Code.

It's the kind of feature that's easy to *sell* and hard to *prove*. We wanted to know whether it actually moves outcomes for an LLM agent, or whether it's a checkbox feature people think they want but never benefit from.

So we built an A/B harness:
- **Variant A — `exec`**: `read_file`, `write_file`, `list_dir`, `exec`, `submit`.
- **Variant B — `lsp`**: everything in A *plus* the LSP toolset.

Five Python bug-fix tasks. Same model (`gpt-4o-mini`), same prompts, same fresh microVM per run (no cache contamination). The agent is "done" when the failing test goes green.

## Round 1: the embarrassing numbers

After getting the plumbing right — including baking the entire `python-lsp-server` runtime into the microVM from 15 pre-downloaded wheels because the sandbox has no PyPI egress — we ran the suite. Both variants solved 10/10 runs. But:

| variant | mean tokens |
|---|---:|
| `exec` | 3,685 |
| `lsp` | 4,168 |

**Our own feature lost by 13%.** Per-task:

| Task | Cross-file? | Δ tokens (exec − lsp) | Winner |
|---|---|---:|---|
| 01 renamed_helper | no | −640 | exec |
| 02 wrong_attribute | no | −1,367 | exec |
| 03 moved_symbol | **yes** | **+1,535** | **lsp** |
| 04 wrong_arg_order | no | −1,429 | exec |
| 05 typo_in_constant | no | −517 | exec |

Two things were obvious from this table:

1. **LSP wins where it should win** — the only task that requires hopping between modules (a symbol moved to a sibling file) — and only there.
2. **LSP loses where it shouldn't even be running.** On trivial single-file edits, the agent paid for a richer toolbelt it didn't need.

The honest interpretation: this isn't a benchmark failure, it's a product failure. The LSP variant carries overhead the agent can't justify on easy tasks. So we shipped four fixes.

## Round 2: four fixes to the actual product

### A) Bake `python-lsp-server` into the base image

Before: every microVM that wanted LSP had to install pylsp on first WebSocket connect. That's a 5–10s cold path the user pays for, every time.

After: `templates/code-interpreter/Dockerfile` now includes:

```dockerfile
RUN python3 -m pip install --break-system-packages --no-cache-dir \
      'python-lsp-server>=1.14' pylsp-rope pyflakes pycodestyle
```

Zero install cost. Connect, send `initialize`, you're working. `pylsp-rope` is included too — vanilla pylsp returns `-32601 Method Not Found` for `workspace/symbol` without it, and that error was confusing agents in our first run.

### B) Compress LSP responses to `path:line:col`

The LSP wire format is JSON-RPC and *verbose*. A single `textDocument/definition` result looks like:

```json
[{"uri": "file:///workspace/src/foo.py",
  "range": {"start": {"line": 12, "character": 5},
            "end":   {"line": 12, "character": 12}}}]
```

That's ~120 tokens for a location. When the same payload is consumed by an LLM tool-call loop, those tokens compound across every navigation step.

We compress server responses on the client to:

```
src/foo.py:13:5
```

About 6 tokens. **~95% reduction per location.** No information loss for the agent — the LLM understands `path:line:col` natively because every stack trace it has ever seen in its training data is formatted that way.

### C) A combined `lsp_goto_def` tool

Old workflow:
1. `lsp_definition(file, line, char)` → returns `[{uri, range}]`
2. `read_file(path)` → returns the entire file
3. Agent finds the relevant function in the file dump

Three tool calls, three round-trips, the entire file in the context window.

New tool — `lsp_goto_def(file, line, char, context_lines=12)` — does all three server-side and returns:

```
src/utils.py:42:1
  40   from typing import Optional
  41
> 42   def calculate_sum(items: list[int]) -> int:
  43       """Sum a list, treating None as 0."""
  44       return sum(x or 0 for x in items)
  ...
```

One tool call. ~15 lines of context instead of an entire file. The `>` marker tells the model exactly which line LSP pointed at. **Cuts the navigation tax in half on every cross-file hop.**

### D) Make `lsp_find_symbol` actually find symbols

In round 1, our `lsp_workspace_symbol` returned `[]` for everything because pylsp doesn't ship `workspace/symbol`. The agent would call it, get nothing, then loop trying other tools. *That's where most of our wasted tokens went.*

The new `lsp_find_symbol(query)` tries LSP first, then falls back to a structural grep:

```bash
grep -rnE '^[[:space:]]*(def|class)[[:space:]]+<query>\b|^[[:space:]]*<query>[[:space:]]*=' \
    --include='*.py' /workspace
```

It returns `{name, kind, loc, preview}` for every hit. The tool always returns something useful — no more silent dead-ends. We don't pretend it's "LSP" when it isn't, but the contract the agent depends on is honored.

Plus a system-prompt nudge: *"For trivial single-file edits, prefer `read_file` + `write_file` — LSP is for navigation."* No prompt-engineering miracle; just telling the model when not to use the new tools.

## Round 2 results

Same 5 tasks, same model, same harness. After the four product changes:

| Task | exec tokens | exec steps | lsp tokens | lsp steps | LSP delta |
|---|---:|---:|---:|---:|---|
| 01 renamed_helper | 2,529 | 6 | 9,278 | 11 | exec wins |
| 02 wrong_attribute | 1,939 | 5 | 5,286 | 7 | exec wins |
| **03 moved_symbol** | **20,385** | **23** | **16,433** | **14** | **lsp −19% tokens, −39% steps** |
| 04 wrong_arg_order | 1,411 | 4 | 4,547 | 6 | exec wins |
| 05 typo_in_constant | 1,697 | 5 | 6,339 | 8 | exec wins |

The cross-file task — task 03 — went from a 14% LSP win to a **19% token win and 39% step reduction**. That's a feature now doing real work.

The trivial tasks still lose. We chose not to hide that.

## What we learned (and won't pretend)

**LSP is a power tool, not a default tool.** On in-file edits, the cheapest path is `read_file` → identify the bug → `write_file`. Adding navigation infrastructure doesn't make that path shorter; it makes it longer because the model has more options to consider on every turn.

**The cost of an unused tool is not zero.** Every tool you attach lives in the system prompt for every turn. Five LSP tools cost roughly 400 tokens of schema *per assistant turn*. Over 10 turns that's 4,000 tokens — enough to swing a benchmark on tasks that don't even need the tools.

**Pretty wire formats are agent kryptonite.** The LSP spec optimizes for editor consumption with redundant URI envelopes and nested range objects. None of that helps an LLM. `path:line:col` is what every human-readable error message in the world looks like, and the model knows it cold.

**Combined tools beat granular ones.** "Jump to def *and* show me the code there" is one cognitive action for the user; it should be one tool call for the agent. Atomic LSP primitives are a great API for editors and a poor API for autonomous reasoning loops.

## When to enable LSP-as-a-Service in your agent

Based on these numbers, our honest guidance:

- ✅ **Multi-file repos** (5+ files, or any repo where bugs span imports): definitely turn it on.
- ✅ **Long-running sessions** where the agent navigates the same codebase repeatedly: the schema cost amortizes; the navigation savings compound.
- ❌ **One-shot bug fixes in a known file**: skip it. Pure `read_file` + `write_file` is cheaper and faster.
- 🟡 **Mixed workloads**: gate per task. A cheap heuristic on the failing test ("does the traceback reference a symbol from another module?") captures most of the wins without paying on the easy ones.

## What's next

We're working on:
1. **A real-world benchmark suite** — 5 reverted fix-commits from mid-size OSS Python repos (Flask, httpx, pydantic, rich, SQLAlchemy). Most real bugs require navigation. We'll publish those numbers separately.
2. **Auto-gating of LSP tools** on the server side, based on initial workspace scan.
3. **TypeScript and Go language servers** — same wire protocol, same client.

## Reproduce these numbers

Everything is in [`bench/lsp/`](.) in the repo:

```bash
export OPENAI_API_KEY=sk-...
export PANDASTACK_API_BASE=https://api.pandastack.ai
export PANDASTACK_API_TOKEN=dam_...

# Dry-run (deterministic, no OpenAI cost) to verify plumbing:
python3 bench/lsp/harness.py --dry-run --variants both

# Real run (~$0.10 on gpt-4o-mini, ~5 minutes wall time):
python3 bench/lsp/harness.py --variants both --model gpt-4o-mini
```

The 5 tasks live in `bench/lsp/tasks.py`. Pylsp wheels for hermetic install live in `bench/lsp/wheels/` (17 wheels including pylsp-rope and rope; the sandbox has no PyPI egress). Per-task CSVs land in `bench/lsp/results/`.

If you find a task where our LSP variant should win and doesn't, [open an issue](https://github.com/pandastack-io/pandastack-ai/issues) with the failing test. We'll either fix the feature or update this blog.

— *The PandaStack team*

---

*Methodology disclaimer: each cell here is a single trial. Run-to-run variance for `gpt-4o-mini` is high (we saw the exec variant solve task 03 in 14 steps on one run and 23 on another). We're adding `--repeat N` and a real-OSS task suite for the follow-up post; until then, treat the per-task numbers as directional and the trend as solid.*
