"""pytest sweep — runs every cookbook/<template>/main.py as a subprocess.

Each example is required to:
  1. exit 0
  2. print a line beginning with "EXPECTED: " on stdout
  3. clean up its own sandbox (we don't garbage-collect here)

Examples that need LLM credentials must `print("SKIP: <reason>")` and
then still print the EXPECTED line + exit 0 — that way CI doesn't flake on
missing keys but local devs with creds get real coverage.

Set `COOKBOOK_TEMPLATES=base,code-interpreter` to run only a subset.
"""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

import pytest


COOKBOOK_DIR = Path(__file__).resolve().parent


def _all_templates() -> list[str]:
    out = []
    for child in sorted(COOKBOOK_DIR.iterdir()):
        if not child.is_dir() or child.name.startswith("."):
            continue
        if (child / "main.py").is_file():
            out.append(child.name)
    return out


def _selected() -> list[str]:
    env = os.environ.get("COOKBOOK_TEMPLATES", "").strip()
    if not env:
        return _all_templates()
    requested = [s.strip() for s in env.split(",") if s.strip()]
    available = set(_all_templates())
    missing = [r for r in requested if r not in available]
    if missing:
        raise pytest.UsageError(f"unknown COOKBOOK_TEMPLATES: {missing}")
    return requested


@pytest.mark.parametrize("template", _selected())
def test_cookbook_example(template: str) -> None:
    if not os.environ.get("PANDASTACK_TOKEN", "").strip():
        pytest.skip("PANDASTACK_TOKEN not set")
    script = COOKBOOK_DIR / template / "main.py"
    assert script.is_file(), script
    timeout = int(os.environ.get("COOKBOOK_TIMEOUT", "600"))
    proc = subprocess.run(
        [sys.executable, str(script)],
        capture_output=True,
        text=True,
        timeout=timeout,
        env={**os.environ},
    )
    sys.stdout.write(proc.stdout)
    sys.stderr.write(proc.stderr)
    assert proc.returncode == 0, f"{template} exited {proc.returncode}\nstderr:\n{proc.stderr}"
    expected_lines = [ln for ln in proc.stdout.splitlines() if ln.startswith("EXPECTED: ")]
    assert expected_lines, f"{template}: stdout missing EXPECTED: line"
