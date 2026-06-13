"""A/B harness for Python bug-fixing agents with and without LSP tools."""

from __future__ import annotations

import argparse
import asyncio
import csv
import os
import statistics
import sys
import time
from datetime import datetime
from pathlib import Path
from typing import Any

from agent import BugFixAgent
from api_client import PandaStackClient
from lsp_client import LSPClient
from tasks import select_tasks

DEFAULT_API = os.environ.get("PANDASTACK_API", "https://api.pandastack.ai")
DEFAULT_TOKEN = os.environ.get("PANDASTACK_TOKEN", "")
ROOT = Path(__file__).resolve().parent
RESULTS = ROOT / "results"
REPORT = ROOT / "report.md"
FIELDS = ["task_id", "variant", "passed", "steps", "input_tokens", "output_tokens", "total_tokens", "wall_seconds", "hallucinated_count", "error"]


def parse_args() -> argparse.Namespace:
    ts = datetime.now().strftime("%Y%m%d-%H%M%S")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", default=DEFAULT_API)
    parser.add_argument("--token", default=DEFAULT_TOKEN)
    parser.add_argument("--model", default="gpt-4o-mini")
    parser.add_argument("--variants", choices=["both", "exec", "lsp"], default="both")
    parser.add_argument("--max-steps", type=int, default=25)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--out", default=str(RESULTS / f"run-{ts}.csv"))
    parser.add_argument("--tasks", default=None, help="Comma-separated task ids or numeric prefixes, e.g. 01,03")
    return parser.parse_args()


def variants(value: str) -> list[str]:
    return ["exec", "lsp"] if value == "both" else [value]


async def upload_task(api: PandaStackClient, sandbox_id: str, task: dict[str, Any]) -> None:
    dirs = sorted({str(Path(path).parent) for path in task["files"]})
    if dirs:
        await api.exec(sandbox_id, "mkdir -p " + " ".join(sh_quote(d) for d in dirs), 30)
    for path, content in task["files"].items():
        await api.upload_file(sandbox_id, path, content)


def sh_quote(value: str) -> str:
    return "'" + value.replace("'", "'\\''") + "'"


async def ensure_pytest(api: PandaStackClient, sandbox_id: str) -> None:
    shim = r'''#!/usr/bin/env python3
import importlib.util, inspect, pathlib, sys, traceback
args = [a for a in sys.argv[1:] if not a.startswith('-')]
files = [a for a in args if a.endswith('.py')]
if not files:
    print('minimal pytest shim: no test files', file=sys.stderr)
    sys.exit(2)
failures = 0
for file_name in files:
    path = pathlib.Path(file_name)
    sys.path.insert(0, str(path.parent))
    spec = importlib.util.spec_from_file_location(path.stem, path)
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
        for name, fn in sorted(vars(mod).items()):
            if name.startswith('test_') and inspect.isfunction(fn):
                try:
                    fn()
                    print(f'{file_name}::{name} PASSED')
                except Exception:
                    failures += 1
                    print(f'{file_name}::{name} FAILED')
                    traceback.print_exc()
                    if '-x' in sys.argv[1:]:
                        sys.exit(1)
    except Exception:
        failures += 1
        traceback.print_exc()
        if '-x' in sys.argv[1:]:
            sys.exit(1)
sys.exit(1 if failures else 0)
'''
    cmd = "command -v pytest >/dev/null 2>&1 || (cat > /usr/local/bin/pytest <<'PYTEST'\n" + shim + "PYTEST\nchmod +x /usr/local/bin/pytest)"
    res = await api.exec(sandbox_id, cmd, 30)
    if int(res.get("exit", 1)) != 0:
        raise RuntimeError("failed to install pytest shim: " + str(res))


async def baseline_fails(api: PandaStackClient, sandbox_id: str, task: dict[str, Any]) -> tuple[bool, str]:
    res = await api.exec(sandbox_id, task["failing_test"], 60)
    output = f"exit={res.get('exit')}\nstdout:\n{res.get('stdout','')}\nstderr:\n{res.get('stderr','')}"
    return int(res.get("exit", 1)) != 0, output


async def ensure_pylsp(api: PandaStackClient, sandbox_id: str) -> None:
    """Install real python-lsp-server from bundled wheels (sandbox has no PyPI egress)."""
    probe = await api.exec(sandbox_id, "python3 -c 'import pylsp' 2>/dev/null && echo OK || echo MISSING", 10)
    if "OK" in probe.get("stdout", ""):
        return
    wheels_dir = ROOT / "wheels"
    wheels = sorted(wheels_dir.glob("*.whl"))
    if not wheels:
        raise RuntimeError(f"no wheels found in {wheels_dir}; run `pip download` first")
    await api.exec(sandbox_id, "mkdir -p /tmp/wheels", 10)
    for wheel in wheels:
        await api.upload_file(sandbox_id, f"/tmp/wheels/{wheel.name}", wheel.read_bytes())
    install = await api.exec(
        sandbox_id,
        "pip3 install --no-index --no-deps --target /usr/local/lib/python3.12/dist-packages --quiet /tmp/wheels/*.whl 2>&1 | tail -3",
        90,
    )
    verify = await api.exec(sandbox_id, "python3 -c 'import pylsp; print(pylsp.__version__)' 2>&1", 10)
    if int(verify.get("exit", 1)) != 0 or "pylsp" not in (verify.get("stdout") + verify.get("stderr", "")).lower() and not verify.get("stdout","").strip():
        raise RuntimeError(f"pylsp install failed: install={install} verify={verify}")
    print(f"[lsp] installed pylsp {verify.get('stdout','').strip()} from {len(wheels)} wheels")


async def prepare_lsp(api: PandaStackClient, sandbox_id: str, task: dict[str, Any]):
    cm = api.lsp_ws(sandbox_id)
    transport = await cm.__aenter__()
    lsp = LSPClient(transport)
    await lsp.initialize("/workspace")
    for path, content in task["files"].items():
        if path.endswith(".py"):
            await lsp.did_open(path, content)
    return cm, lsp


async def run_one(api: PandaStackClient, args: argparse.Namespace, task: dict[str, Any], variant: str) -> dict[str, Any]:
    sandbox_id = ""
    lsp_cm = None
    start = time.monotonic()
    row = {field: "" for field in FIELDS}
    row.update({"task_id": task["id"], "variant": variant, "passed": False, "steps": 0, "input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "hallucinated_count": 0})
    try:
        sandbox = await api.create_sandbox()
        sandbox_id = sandbox["id"]
        print(f"[{task['id']}:{variant}] sandbox={sandbox_id} boot_ms={sandbox.get('boot_ms')}")
        await upload_task(api, sandbox_id, task)
        print(f"[{task['id']}:{variant}] uploaded {len(task['files'])} files")
        await ensure_pytest(api, sandbox_id)
        failed, baseline = await baseline_fails(api, sandbox_id, task)
        print(f"[{task['id']}:{variant}] red baseline={'yes' if failed else 'NO'}")
        if not failed:
            raise RuntimeError("baseline test unexpectedly passed\n" + baseline)

        lsp = None
        if variant == "lsp":
            await ensure_pylsp(api, sandbox_id)
            lsp_cm, lsp = await prepare_lsp(api, sandbox_id, task)
            symbols = await lsp.workspace_symbol("calc")
            print(f"[{task['id']}:{variant}] workspace_symbol('calc') -> {symbols}")

        agent = BugFixAgent(api, sandbox_id, task, model=args.model, max_steps=args.max_steps, lsp=lsp)
        result = await (agent.run_stub(apply_fix=True) if args.dry_run else agent.run())
        row.update({
            "passed": result.passed,
            "steps": result.steps,
            "input_tokens": result.input_tokens,
            "output_tokens": result.output_tokens,
            "total_tokens": result.total_tokens,
            "hallucinated_count": result.hallucinated_count,
            "error": result.error,
        })
    except Exception as exc:
        row["error"] = f"{type(exc).__name__}: {exc}"
        print(f"[{task['id']}:{variant}] ERROR {row['error']}")
    finally:
        if lsp_cm is not None:
            try:
                await lsp_cm.__aexit__(None, None, None)
            except Exception as exc:
                print(f"[{task['id']}:{variant}] LSP close warning: {exc}")
        if sandbox_id:
            try:
                await api.delete_sandbox(sandbox_id)
                print(f"[{task['id']}:{variant}] deleted sandbox")
            except Exception as exc:
                row["error"] = (row.get("error") or "") + f" delete_failed={exc}"
    row["wall_seconds"] = f"{time.monotonic() - start:.2f}"
    return row


def append_csv(path: Path, rows: list[dict[str, Any]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    write_header = not path.exists() or path.stat().st_size == 0
    with path.open("a", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=FIELDS)
        if write_header:
            writer.writeheader()
        writer.writerows(rows)


def make_report(rows: list[dict[str, Any]], out_path: Path) -> None:
    by_variant: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        by_variant.setdefault(str(row["variant"]), []).append(row)

    lines = ["# LSP Benchmark Report", "", f"CSV: `{out_path}`", ""]
    lines.append("## Aggregate stats")
    lines.append("")
    lines.append("| variant | pass rate | mean tokens | median tokens | median steps | hallucinations |")
    lines.append("|---|---:|---:|---:|---:|---:|")
    for variant, items in sorted(by_variant.items()):
        passed = sum(str(r["passed"]).lower() == "true" for r in items)
        tokens = [int(r.get("total_tokens") or 0) for r in items]
        steps = [int(r.get("steps") or 0) for r in items]
        halls = sum(int(r.get("hallucinated_count") or 0) for r in items)
        lines.append(f"| {variant} | {passed}/{len(items)} | {statistics.mean(tokens):.1f} | {statistics.median(tokens):.1f} | {statistics.median(steps):.1f} | {halls} |")

    lines.extend(["", "## Token delta (exec - lsp)", "", "| task | delta |", "|---|---:|"])
    by_task: dict[str, dict[str, int]] = {}
    for row in rows:
        by_task.setdefault(str(row["task_id"]), {})[str(row["variant"])] = int(row.get("total_tokens") or 0)
    for task_id, vals in sorted(by_task.items()):
        delta = vals["exec"] - vals["lsp"] if "exec" in vals and "lsp" in vals else "n/a"
        lines.append(f"| {task_id} | {delta} |")

    lines.extend([
        "",
        "## Methodology",
        "",
        "Each task runs in a fresh PandaStack `code-interpreter` microVM. The exec variant exposes file, directory, shell, and submit tools. The lsp variant adds Python LSP definition/reference/hover/document/workspace-symbol tools over the sandbox WebSocket. Dry runs use a deterministic stub LLM and report zero token usage.",
    ])
    REPORT.write_text("\n".join(lines) + "\n")


def print_summary(rows: list[dict[str, Any]], dry_run: bool) -> None:
    passed = sum(str(r["passed"]).lower() == "true" for r in rows)
    print("\nSummary")
    print("task_id,variant,passed,steps,tokens,hallucinations,error")
    for r in rows:
        print(f"{r['task_id']},{r['variant']},{r['passed']},{r['steps']},{r['total_tokens']},{r['hallucinated_count']},{r.get('error','')}")
    suffix = " (dry-run, stub LLM)" if dry_run else ""
    print(f"passed: {passed}/{len(rows)} tasks{suffix}")


async def amain() -> int:
    args = parse_args()
    selected = select_tasks(args.tasks)
    if not selected:
        print("No tasks selected", file=sys.stderr)
        return 2
    if not args.dry_run and not os.environ.get("OPENAI_API_KEY"):
        print("OPENAI_API_KEY is not set. Use --dry-run to smoke-test without OpenAI tokens.", file=sys.stderr)
        return 2

    api = PandaStackClient(args.api, args.token)
    rows: list[dict[str, Any]] = []
    try:
        sandboxes = await api.list_sandboxes()
        print(f"PandaStack API OK: {sandboxes}")
        for task in selected:
            for variant in variants(args.variants):
                rows.append(await run_one(api, args, task, variant))
    finally:
        await api.close()

    out_path = Path(args.out)
    if not out_path.is_absolute():
        out_path = (ROOT / out_path) if out_path.parts and out_path.parts[0] == "results" else (Path.cwd() / out_path)
    append_csv(out_path, rows)
    make_report(rows, out_path)
    print_summary(rows, args.dry_run)
    print(f"CSV: {out_path}")
    print(f"Report: {REPORT}")
    return 0 if all(str(r["passed"]).lower() == "true" and not r.get("error") for r in rows) else 1


def main() -> None:
    raise SystemExit(asyncio.run(amain()))


if __name__ == "__main__":
    main()
