"""Claude Managed Agents on PandaStack — reference orchestrator.

Anthropic's Managed Agents run the agent loop (the model, reasoning, session
state) on Anthropic's control plane. With a `self_hosted` environment, the
agent's tool calls execute inside infrastructure you control. This orchestrator
is the glue: it claims session work items from Anthropic's work queue and runs
each session inside its own PandaStack Firecracker microVM.

Flow per work item:

  1. Long-poll the environment's work queue (Anthropic API, environment key).
  2. Acknowledge the work item (removes it from the queue).
  3. Get or create the session's sandbox (template: claude-agent). Returning
     sessions reuse their sandbox — a hibernated sandbox auto-wakes on the
     first exec, restoring both memory and filesystem.
  4. Launch Anthropic's pre-built in-guest runner (`ant beta:worker run`)
     detached inside the sandbox. The runner downloads the agent's skills,
     executes tool calls, heartbeats its work lease, and exits when the
     session goes idle.
  5. When the runner exits: download /mnt/session/outputs, then hibernate
     (default) or delete the sandbox.

Environment variables:

  ANTHROPIC_ENVIRONMENT_ID    required — env_... id of the self_hosted environment
  ANTHROPIC_ENVIRONMENT_KEY   required — sk-ant-oat01-... key generated in the Console
  PANDASTACK_TOKEN            required — PandaStack API token
  PANDASTACK_API              optional — PandaStack API URL (default https://api.pandastack.ai)
  CMA_TEMPLATE                optional — sandbox template (default "claude-agent")
  CMA_ON_IDLE                 optional — what to do with the sandbox when a turn's
                              runner exits (default "hibernate"):
                                hibernate — snapshot + stop; auto-wakes on the next
                                  turn with state intact. Frees CPU/RAM between turns.
                                running   — leave it up; lowest next-turn latency,
                                  full state, but holds resources while idle.
                                delete    — tear down; recreated fresh next turn
                                  (~180ms), loses /workspace state. For one-shot tasks.
  CMA_OUTPUTS_DIR             optional — where session outputs land (default ./outputs)
  CMA_SANDBOX_TTL_SECONDS     optional — safety-net TTL on session sandboxes (default 7200)
  CMA_MAX_SESSION_SECONDS     optional — force-stop a runner after this long (default 3600)
  CMA_WORKER_ID               optional — worker id reported to Anthropic (default host name)

Run:  python orchestrator.py
"""

from __future__ import annotations

import os
import shlex
import signal
import socket
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path

import requests
from pandastack import Client, Sandbox, set_default_client

ANTHROPIC_API = os.environ.get("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
ANTHROPIC_VERSION = "2023-06-01"
ANTHROPIC_BETA = "managed-agents-2026-04-01"

ENVIRONMENT_ID = os.environ.get("ANTHROPIC_ENVIRONMENT_ID", "")
ENVIRONMENT_KEY = os.environ.get("ANTHROPIC_ENVIRONMENT_KEY", "")

TEMPLATE = os.environ.get("CMA_TEMPLATE", "claude-agent")
ON_IDLE = os.environ.get("CMA_ON_IDLE", "hibernate")  # hibernate | running | delete
OUTPUTS_DIR = Path(os.environ.get("CMA_OUTPUTS_DIR", "outputs"))
# TTL is a safety net so an abandoned session's sandbox is eventually reaped.
# Keep it under the workspace tier's maximum; if the create is rejected for
# exceeding it, we retry without a TTL (see get_or_create_sandbox).
SANDBOX_TTL = int(os.environ.get("CMA_SANDBOX_TTL_SECONDS", "3600"))
MAX_SESSION_SECONDS = int(os.environ.get("CMA_MAX_SESSION_SECONDS", "3600"))
WORKER_ID = os.environ.get("CMA_WORKER_ID", f"pandastack-{socket.gethostname()}")

POLL_BLOCK_MS = 999          # Anthropic long-poll ceiling (1-999)
RUNNER_POLL_SECONDS = 5      # how often a watcher checks the runner sentinel
OUTPUTS_GUEST_DIR = "/mnt/session/outputs"
RUNNER_LOG = "/var/log/cma-runner.log"
EXIT_SENTINEL = "/tmp/cma-runner.exit"
LAUNCH_SCRIPT = "/tmp/cma-launch.sh"

# session_id -> sandbox_id for sessions with a live runner; guards double-launch
_active: dict[str, str] = {}
_active_lock = threading.Lock()
_shutdown = threading.Event()


def log(msg: str) -> None:
    ts = datetime.now(timezone.utc).strftime("%H:%M:%S")
    print(f"[{ts}] {msg}", flush=True)


# ── Anthropic work queue (environment-key auth) ──────────────────────────────

def _anthropic_headers() -> dict[str, str]:
    return {
        "Authorization": f"Bearer {ENVIRONMENT_KEY}",
        "anthropic-version": ANTHROPIC_VERSION,
        "anthropic-beta": ANTHROPIC_BETA,
        "Anthropic-Worker-ID": WORKER_ID,
    }


def poll_work() -> dict | None:
    """Long-poll the work queue. Returns a work item dict or None."""
    r = requests.get(
        f"{ANTHROPIC_API}/v1/environments/{ENVIRONMENT_ID}/work/poll",
        params={"block_ms": POLL_BLOCK_MS},
        headers=_anthropic_headers(),
        timeout=30,
    )
    if r.status_code == 200:
        body = r.json() if r.content else None
        if body and body.get("id"):
            return body
        return None
    if r.status_code in (204, 404):
        return None  # empty queue
    if r.status_code == 401:
        log("FATAL: environment key rejected (401). Check ANTHROPIC_ENVIRONMENT_KEY.")
        _shutdown.set()
        return None
    log(f"poll: unexpected {r.status_code}: {r.text[:200]}")
    time.sleep(2)
    return None


def ack_work(work_id: str) -> bool:
    r = requests.post(
        f"{ANTHROPIC_API}/v1/environments/{ENVIRONMENT_ID}/work/{work_id}/ack",
        headers=_anthropic_headers(),
        timeout=30,
    )
    if r.status_code == 200:
        return True
    log(f"ack {work_id}: {r.status_code} {r.text[:200]} — skipping (claimed elsewhere?)")
    return False


# ── PandaStack sandbox lifecycle ─────────────────────────────────────────────

def _pandastack_api() -> tuple[str, dict[str, str]]:
    base = os.environ.get("PANDASTACK_API", "https://api.pandastack.ai").rstrip("/")
    return base, {"Authorization": f"Bearer {os.environ['PANDASTACK_TOKEN']}"}


def find_session_sandbox(session_id: str) -> Sandbox | None:
    """Sandbox list has no metadata filter — match client-side."""
    for sb in Sandbox.list():
        if (sb.metadata or {}).get("cma.session_id") == session_id and sb.status not in (
            "deleted",
            "failed",
        ):
            return sb
    return None


def _create_sandbox(session_id: str) -> Sandbox:
    meta = {"cma.session_id": session_id, "cma.environment_id": ENVIRONMENT_ID}
    t0 = time.monotonic()
    try:
        sb = Sandbox.create(template=TEMPLATE, ttl_seconds=SANDBOX_TTL, metadata=meta)
    except Exception as e:
        # Some workspace tiers cap ttl_seconds below our safety-net default.
        # Fall back to the platform default TTL rather than failing the session.
        if "ttl_seconds exceeds tier maximum" in str(e):
            log(f"session {session_id[:24]}…: ttl {SANDBOX_TTL}s over tier cap — creating without TTL")
            sb = Sandbox.create(template=TEMPLATE, metadata=meta)
        else:
            raise
    boot_ms = int((time.monotonic() - t0) * 1000)
    log(f"session {session_id[:24]}…: sandbox {sb.id[:12]}… created in {boot_ms}ms (boot_mode={sb.boot_mode})")
    return sb


def get_or_create_sandbox(session_id: str) -> tuple[Sandbox, bool]:
    sb = find_session_sandbox(session_id)
    if sb is not None:
        # A returning turn reuses the session's sandbox. If it was hibernated,
        # poke it with a trivial exec to force the auto-wake now — so a wake
        # failure surfaces here (where we can recreate) instead of stranding
        # the runner launch. A fresh sandbox is created if the wake fails.
        try:
            sb.exec("true", timeout_seconds=60)
            log(f"session {session_id[:24]}…: reusing sandbox {sb.id[:12]}… (status={sb.status})")
            return sb, False
        except Exception as e:
            log(f"session {session_id[:24]}…: reuse of {sb.id[:12]}… failed ({str(e)[:120]}); recreating")
            try:
                sb.kill()
            except Exception:
                pass
    return _create_sandbox(session_id), True


def launch_runner(sb: Sandbox, session_id: str, work_id: str) -> None:
    """Start `ant beta:worker run` detached inside the guest.

    The launch script is written via the filesystem API (not inlined in the
    exec command line) so the environment key stays out of command logs; the
    script deletes itself after the runner inherits the environment. The
    runner is detached under setsid because the platform's plain exec is
    bounded by the orchestrator's HTTP timeout — a session can run for many
    minutes, so we poll a sentinel file instead of holding the connection.
    """
    # The runner and every agent bash tool call it spawns must find the mise
    # runtimes (node/python). mise shims are inert without MISE_DATA_DIR/
    # MISE_CONFIG_DIR, and `ant`'s child `sh -c` sessions are non-login, so we
    # export them here in addition to /etc/environment baked into the template.
    script = f"""#!/bin/sh
# written by the PandaStack CMA orchestrator; self-deletes after launch
export ANTHROPIC_ENVIRONMENT_ID={shlex.quote(ENVIRONMENT_ID)}
export ANTHROPIC_ENVIRONMENT_KEY={shlex.quote(ENVIRONMENT_KEY)}
export ANTHROPIC_SESSION_ID={shlex.quote(session_id)}
export ANTHROPIC_WORK_ID={shlex.quote(work_id)}
export MISE_DATA_DIR=/opt/mise
export MISE_CONFIG_DIR=/opt/mise
export PATH=/opt/mise/shims:$PATH
export HOME=/root
rm -f {EXIT_SENTINEL}
{{ setsid sh -c 'ant beta:worker run --workdir /workspace --log-format json >> {RUNNER_LOG} 2>&1; echo $? > {EXIT_SENTINEL}' </dev/null >/dev/null 2>&1 & }}
echo launched
"""
    sb.filesystem.write(LAUNCH_SCRIPT, script)
    out = sb.exec(f"sh {LAUNCH_SCRIPT}; rm -f {LAUNCH_SCRIPT}", timeout_seconds=30)
    if out.exit_code != 0 or "launched" not in out.stdout:
        raise RuntimeError(f"runner launch failed: exit={out.exit_code} stderr={out.stderr[:300]}")


def pull_outputs(sb: Sandbox, session_id: str) -> int:
    """Recursively download /mnt/session/outputs via the fs API.

    The Python SDK has no list-dir wrapper yet, so /fs/dir is called raw.
    """
    base, headers = _pandastack_api()
    count = 0

    def walk(guest_dir: str, local_dir: Path) -> None:
        nonlocal count
        r = requests.get(
            f"{base}/v1/sandboxes/{sb.id}/fs/dir",
            params={"path": guest_dir},
            headers=headers,
            timeout=30,
        )
        if r.status_code != 200:
            return
        # /fs/dir returns {"entries": null} for an empty directory — coalesce.
        for entry in (r.json().get("entries") or []):
            name = entry.get("name", "")
            if not name or name in (".", ".."):
                continue
            guest_path = f"{guest_dir.rstrip('/')}/{name}"
            if entry.get("is_dir"):
                walk(guest_path, local_dir / name)
            else:
                local_dir.mkdir(parents=True, exist_ok=True)
                (local_dir / name).write_bytes(sb.filesystem.read(guest_path))
                count += 1

    walk(OUTPUTS_GUEST_DIR, OUTPUTS_DIR / session_id)
    return count


# ── Session watcher ──────────────────────────────────────────────────────────

def watch_session(sb: Sandbox, session_id: str, work_id: str) -> None:
    """Poll the runner's exit sentinel; on exit, collect outputs and park."""
    started = time.monotonic()
    try:
        while not _shutdown.is_set():
            time.sleep(RUNNER_POLL_SECONDS)
            if time.monotonic() - started > MAX_SESSION_SECONDS:
                log(f"session {session_id[:24]}…: exceeded {MAX_SESSION_SECONDS}s — force-stopping runner")
                try:
                    sb.exec("pkill -f 'ant beta:worker run' || true", timeout_seconds=15)
                except Exception:
                    pass
                break
            try:
                out = sb.exec(f"cat {EXIT_SENTINEL} 2>/dev/null || true", timeout_seconds=15)
            except Exception as e:  # transient exec failure — keep watching
                msg = str(e).lower()
                if "not found" in msg or "404" in msg or "no such sandbox" in msg:
                    # Sandbox was deleted out from under us (external kill, TTL
                    # reap, or a prior turn cleaned up). Nothing left to watch.
                    log(f"session {session_id[:24]}…: sandbox gone — stopping watcher")
                    return
                log(f"session {session_id[:24]}…: watcher exec error ({e}); retrying")
                continue
            code = out.stdout.strip()
            if code:
                log(f"session {session_id[:24]}…: runner exited with code {code}")
                break

        n = 0
        try:
            n = pull_outputs(sb, session_id)
        except Exception as e:
            log(f"session {session_id[:24]}…: output pull failed: {e}")
        if n:
            log(f"session {session_id[:24]}…: saved {n} output file(s) to {OUTPUTS_DIR / session_id}")

        if ON_IDLE == "delete":
            sb.kill()
            log(f"session {session_id[:24]}…: sandbox {sb.id[:12]}… deleted")
        elif ON_IDLE == "running":
            # Leave the sandbox up for the next turn — lowest next-turn latency,
            # full state, no snapshot cost. The TTL safety net (and any host-side
            # idle reaper) still reclaims it if the session is abandoned.
            log(f"session {session_id[:24]}…: sandbox {sb.id[:12]}… left running for next turn")
        else:  # hibernate (default)
            try:
                sb.hibernate()
                log(f"session {session_id[:24]}…: sandbox {sb.id[:12]}… hibernated (wakes on next turn)")
            except Exception as e:
                log(f"session {session_id[:24]}…: hibernate failed ({e}); leaving running")
    finally:
        with _active_lock:
            _active.pop(session_id, None)


# ── Main loop ────────────────────────────────────────────────────────────────

def handle_work(work: dict) -> None:
    work_id = work["id"]
    session_id = work.get("data", {}).get("id", "")
    if not session_id:
        log(f"work {work_id}: no session id — ignoring")
        return

    with _active_lock:
        if session_id in _active:
            # The session's runner is still attached (quick follow-up turns ride
            # the existing runner); a duplicate work item would double-launch.
            log(f"work {work_id}: session {session_id[:24]}… already active — skipping")
            return
        _active[session_id] = "claiming"

    try:
        if not ack_work(work_id):
            with _active_lock:
                _active.pop(session_id, None)
            return
        log(f"work {work_id}: claimed session {session_id[:24]}… (metadata={work.get('metadata') or {}})")
        sb, _created = get_or_create_sandbox(session_id)
        launch_runner(sb, session_id, work_id)
        log(f"session {session_id[:24]}…: runner launched in sandbox {sb.id[:12]}…")
        with _active_lock:
            _active[session_id] = sb.id
        threading.Thread(
            target=watch_session, args=(sb, session_id, work_id), daemon=True
        ).start()
    except Exception as e:
        log(f"work {work_id}: failed to start session: {e}")
        with _active_lock:
            _active.pop(session_id, None)


def main() -> None:
    missing = [
        name
        for name, val in (
            ("ANTHROPIC_ENVIRONMENT_ID", ENVIRONMENT_ID),
            ("ANTHROPIC_ENVIRONMENT_KEY", ENVIRONMENT_KEY),
            ("PANDASTACK_TOKEN", os.environ.get("PANDASTACK_TOKEN", "")),
        )
        if not val
    ]
    if missing:
        sys.exit(f"missing required env: {', '.join(missing)}")

    # One client with a generous timeout; exec calls here are all short, but
    # the default 30s is kept high-margin for slow first-boot warmups.
    set_default_client(Client(timeout=60.0))

    OUTPUTS_DIR.mkdir(parents=True, exist_ok=True)
    signal.signal(signal.SIGINT, lambda *_: _shutdown.set())
    signal.signal(signal.SIGTERM, lambda *_: _shutdown.set())

    log(f"PandaStack worker '{WORKER_ID}' polling environment {ENVIRONMENT_ID}")
    log(f"template={TEMPLATE} on_idle={ON_IDLE} outputs={OUTPUTS_DIR.resolve()}")

    # Long-polling naturally drops idle connections (reset / EOF / read timeout);
    # a single failure is normal and not worth logging. Only surface it once a
    # run of consecutive failures suggests the queue is genuinely unreachable.
    consecutive_errors = 0
    while not _shutdown.is_set():
        try:
            work = poll_work()
            consecutive_errors = 0
        except requests.RequestException as e:
            consecutive_errors += 1
            if consecutive_errors == 3:
                log(f"poll: {consecutive_errors} consecutive errors (latest: {e}); retrying")
            time.sleep(min(5, consecutive_errors))
            continue
        if work:
            handle_work(work)

    log("shutting down — in-flight runners keep their own work leases")


if __name__ == "__main__":
    main()
