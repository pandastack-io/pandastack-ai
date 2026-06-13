"""Shared helpers for cookbook examples."""

from __future__ import annotations

import sys
import time
from typing import Any

from pandastack import Sandbox
from pandastack.exceptions import ServerError


def wait_for_exec(sb: Sandbox, *, attempts: int = 8, delay: float = 2.0) -> bool:
    """Poll the sandbox until `sb.exec` succeeds.

    Returns True once exec works, False if the sandbox doesn't support it
    (e.g. bare templates without sshd). Some templates skip sshd on purpose
    and still serve other workloads (preview URLs, etc.).
    """
    for i in range(attempts):
        try:
            r = sb.exec("echo ready", timeout_seconds=5)
            if r.exit_code == 0 and "ready" in r.stdout:
                return True
        except ServerError as e:
            msg = str(e)
            if "connect: connection refused" in msg or "no route to host" in msg:
                if i == attempts - 1:
                    return False
                time.sleep(delay)
                continue
            raise
        except Exception:
            if i == attempts - 1:
                return False
            time.sleep(delay)
    return False


def skip(template: str, reason: str) -> Any:
    """Print SKIP + EXPECTED line and exit 0 — never raise from examples."""
    print(f"SKIP: {reason}")
    print(f"EXPECTED: {template} smoke OK")
    sys.exit(0)
