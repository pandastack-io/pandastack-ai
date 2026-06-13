#!/usr/bin/env python3
"""
bench_boot.py — measure cold-boot performance against prod api.pandastack.ai.

Spawns N sandboxes serially (so the host quiesces between trials), records:
  - boot_ms: server-side measured pure microVM boot (from agent response)
  - wall_ms: client-side wall clock from POST start to 201 response

Also benchmarks one exec roundtrip per sandbox.

Outputs JSON to stdout + a brief table to stderr.
"""

import json
import os
import statistics
import sys
import time
import urllib.request
import urllib.error

API = os.environ.get("PANDASTACK_API", "https://api.pandastack.ai")
TOKEN = os.environ["PANDASTACK_TOKEN"]
TEMPLATE = os.environ.get("BENCH_TEMPLATE", "ubuntu-24.04-net")
N = int(os.environ.get("BENCH_N", "50"))


def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(
        API + path, data=data, method=method,
        headers={
            "Authorization": "Bearer " + TOKEN,
            "Content-Type": "application/json",
            "User-Agent": "pandastack-bench/1.0 (+https://github.com/pandastack-io/pandastack-ai-oss)",
        },
    )
    with urllib.request.urlopen(r, timeout=30) as resp:
        return resp.status, json.loads(resp.read() or b"null") if resp.status != 204 else None


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round((p / 100.0) * (len(xs) - 1)))))
    return xs[k]


def main():
    print(f"# {TEMPLATE} · n={N} · api={API}", file=sys.stderr)
    boots, walls, execs = [], [], []
    ids = []
    for i in range(N):
        t0 = time.perf_counter()
        try:
            _, sb = req("POST", "/v1/sandboxes", {"template": TEMPLATE})
        except urllib.error.HTTPError as e:
            print(f"[{i+1}/{N}] create FAILED: {e.code} {e.read()[:200]!r}", file=sys.stderr)
            continue
        wall_ms = (time.perf_counter() - t0) * 1000
        boot_ms = sb.get("boot_ms", 0)
        boots.append(boot_ms)
        walls.append(wall_ms)
        ids.append(sb["id"])

        # exec roundtrip: print 1+1
        te0 = time.perf_counter()
        try:
            _, _ = req("POST", f"/v1/sandboxes/{sb['id']}/exec",
                       {"cmd": "echo $((1+1))"})
            exec_ms = (time.perf_counter() - te0) * 1000
            execs.append(exec_ms)
        except Exception as e:
            print(f"[{i+1}/{N}] exec err: {e}", file=sys.stderr)

        print(f"[{i+1:>3}/{N}] boot={boot_ms:>5}ms wall={wall_ms:>6.1f}ms exec={execs[-1] if execs else 0:>6.1f}ms id={sb['id'][:8]}", file=sys.stderr)

        # Cleanup
        try:
            req("DELETE", f"/v1/sandboxes/{sb['id']}")
        except Exception:
            pass

        # Small sleep so the host quiesces between trials
        time.sleep(0.5)

    out = {
        "template": TEMPLATE,
        "n": len(boots),
        "boot_ms": {
            "min": min(boots) if boots else None,
            "p50": pct(boots, 50),
            "p90": pct(boots, 90),
            "p99": pct(boots, 99),
            "max": max(boots) if boots else None,
            "mean": round(statistics.mean(boots), 1) if boots else None,
        },
        "wall_ms": {
            "min": round(min(walls), 1) if walls else None,
            "p50": round(pct(walls, 50), 1) if walls else None,
            "p90": round(pct(walls, 90), 1) if walls else None,
            "p99": round(pct(walls, 99), 1) if walls else None,
            "max": round(max(walls), 1) if walls else None,
            "mean": round(statistics.mean(walls), 1) if walls else None,
        },
        "exec_ms": {
            "min": round(min(execs), 1) if execs else None,
            "p50": round(pct(execs, 50), 1) if execs else None,
            "p99": round(pct(execs, 99), 1) if execs else None,
        },
    }
    print(json.dumps(out, indent=2))


if __name__ == "__main__":
    main()
