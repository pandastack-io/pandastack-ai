"""agent template — every public coding-agent CLI in one image.

The `agent` template bundles `claude-code`, `codex`, and `opencode`. Pick one
at run time via its API key. This example verifies the CLIs are present, then —
if a key is set — runs a low-token `--version` smoke against the matching CLI.

Set any of `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` to exercise a real CLI; with
none set the example still verifies the binaries exist and exits 0.

EXPECTED: agent smoke OK
"""

from __future__ import annotations

import os

from pandastack import Sandbox


# (env var, CLI lookup command) — first match with a key set gets a version probe.
CLIS = [
    ("ANTHROPIC_API_KEY", "claude", "command -v claude || command -v claude-code"),
    ("OPENAI_API_KEY", "codex", "command -v codex"),
    (None, "opencode", "command -v opencode"),
]


def main() -> None:
    with Sandbox.create(template="agent") as sb:
        found = []
        for _env, name, probe in CLIS:
            r = sb.exec(probe, timeout_seconds=10)
            if r.exit_code == 0 and r.stdout.strip():
                found.append((name, r.stdout.strip().splitlines()[0]))
        assert found, "no coding-agent CLI found in the agent template"
        for name, path in found:
            print(f"Found CLI: {name} -> {path}")

        # If a key is present, run a version smoke against the matching CLI.
        for env, name, _probe in CLIS:
            if env and os.environ.get(env, "").strip() and any(n == name for n, _ in found):
                cli = next(p for n, p in found if n == name)
                v = sb.exec(f"{cli} --version 2>&1 | head -3", timeout_seconds=60)
                print(v.stdout)
                break
        else:
            print("SKIP: no LLM API key set — verified CLIs exist only")

        print(f"EXPECTED: agent smoke OK  ({len(found)} CLIs)")


if __name__ == "__main__":
    main()
