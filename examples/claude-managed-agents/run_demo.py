"""Drive a Claude Managed Agents session end to end against your environment.

Creates a session targeting your self-hosted environment, opens the event
stream, sends a prompt, and prints the agent's activity live. Run this while
orchestrator.py is polling — the session's tool calls will execute inside a
PandaStack sandbox.

Uses your org API key (ANTHROPIC_API_KEY), not the environment key: session
creation and event streaming are operator-side calls.

  export ANTHROPIC_API_KEY=sk-ant-api03-...
  export ANTHROPIC_ENVIRONMENT_ID=env_...
  export CMA_AGENT_ID=agent_...
  python run_demo.py "Write a haiku about microVMs to /mnt/session/outputs/haiku.txt"
"""

from __future__ import annotations

import json
import os
import sys
import threading

import requests

ANTHROPIC_API = os.environ.get("ANTHROPIC_BASE_URL", "https://api.anthropic.com")

HEADERS = {
    "x-api-key": os.environ.get("ANTHROPIC_API_KEY", ""),
    "anthropic-version": "2023-06-01",
    "anthropic-beta": "managed-agents-2026-04-01",
    "content-type": "application/json",
}

DEFAULT_PROMPT = (
    "Check which OS and CPU you are running on, then write a short summary of "
    "your sandbox environment to /mnt/session/outputs/environment.md"
)


def text_of(content: list | None) -> str:
    return "".join(b.get("text", "") for b in (content or []) if isinstance(b, dict))


def stream_events(session_id: str, ready: threading.Event, done: threading.Event) -> None:
    r = requests.get(
        f"{ANTHROPIC_API}/v1/sessions/{session_id}/events/stream",
        headers={**HEADERS, "accept": "text/event-stream"},
        stream=True,
        timeout=600,
    )
    r.raise_for_status()
    ready.set()
    for line in r.iter_lines(decode_unicode=True):
        if not line or not line.startswith("data:"):
            continue
        try:
            event = json.loads(line[len("data:"):].strip())
        except json.JSONDecodeError:
            continue
        etype = event.get("type", "")
        if etype == "agent.message":
            print(f"\n🤖 {text_of(event.get('content'))}")
        elif etype == "agent.tool_use":
            tool = event.get("tool_name") or event.get("name") or "tool"
            inp = json.dumps(event.get("input") or {})[:160]
            print(f"   ⚙ {tool} {inp}")
        elif etype == "session.status_running":
            print("   … agent running")
        elif etype == "session.status_idle":
            stop = (event.get("stop_reason") or {}).get("type", "")
            print(f"\n✓ session idle (stop_reason={stop})")
            if stop == "end_turn":
                done.set()
                return
        elif etype in ("session.status_terminated", "session.error"):
            print(f"\n✗ {etype}: {json.dumps(event)[:400]}")
            done.set()
            return


def main() -> None:
    for var in ("ANTHROPIC_API_KEY", "ANTHROPIC_ENVIRONMENT_ID", "CMA_AGENT_ID"):
        if not os.environ.get(var):
            sys.exit(f"missing required env: {var}")
    prompt = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_PROMPT

    r = requests.post(
        f"{ANTHROPIC_API}/v1/sessions",
        headers=HEADERS,
        json={
            "agent": os.environ["CMA_AGENT_ID"],
            "environment_id": os.environ["ANTHROPIC_ENVIRONMENT_ID"],
        },
        timeout=60,
    )
    r.raise_for_status()
    session_id = r.json()["id"]
    print(f"session created: {session_id}")

    # Open the stream BEFORE sending the message: only events emitted after
    # the stream opens are delivered.
    ready, done = threading.Event(), threading.Event()
    t = threading.Thread(target=stream_events, args=(session_id, ready, done), daemon=True)
    t.start()
    if not ready.wait(timeout=30):
        sys.exit("event stream did not open within 30s")

    r = requests.post(
        f"{ANTHROPIC_API}/v1/sessions/{session_id}/events",
        headers=HEADERS,
        json={
            "events": [
                {"type": "user.message", "content": [{"type": "text", "text": prompt}]}
            ]
        },
        timeout=60,
    )
    r.raise_for_status()
    print(f"prompt sent: {prompt!r}\nwaiting for the agent (watch orchestrator.py logs)…")

    if not done.wait(timeout=900):
        sys.exit("timed out after 15 minutes")
    print(f"\nsession: {session_id}")
    print("outputs (if any) land in ./outputs/<session-id>/ via the orchestrator")


if __name__ == "__main__":
    main()
