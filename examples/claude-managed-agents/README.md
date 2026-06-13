# Claude Managed Agents on PandaStack

Run [Claude Managed Agents](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
sessions inside PandaStack Firecracker microVMs.

Anthropic runs the agent loop — the model, the reasoning, the session state.
PandaStack runs the **tool calls**: the filesystem the agent reads and writes,
the processes it spawns, the network it can reach all live in your microVMs.
This is Anthropic's "self-hosted sandbox" model, and PandaStack is a natural
fit — every session gets a fresh, isolated VM that boots from a snapshot in
~180 ms and can hibernate between turns.

```
┌──────────────────┐   work queue    ┌────────────────────┐   POST /v1/sandboxes   ┌─────────────────┐
│  Anthropic        │ ───────────────▶│  orchestrator.py    │ ──────────────────────▶│  PandaStack      │
│  (agent loop)     │                 │  (this example)     │   exec `ant worker`     │  microVM         │
│                   │◀─ tool results ─│                     │◀── stdout / outputs ────│  (claude-agent)  │
└──────────────────┘                 └────────────────────┘                         └─────────────────┘
```

## How it works

1. You create a Managed Agents **session** that targets your `self_hosted`
   environment. Anthropic enqueues the session as a **work item**.
2. `orchestrator.py` long-polls the queue, claims the work item, and creates a
   PandaStack sandbox from the **`claude-agent`** template (one sandbox per
   session; returning sessions reuse — and auto-wake — their existing sandbox).
3. It launches Anthropic's pre-built in-guest runner, `ant beta:worker run`,
   inside the sandbox. The runner downloads the agent's
   [skills](https://platform.claude.com/docs/en/managed-agents/skills), executes
   the agent's tool calls (`bash`, `read`, `write`, `edit`, `glob`, `grep`),
   heartbeats its work lease, and exits when the session goes idle.
4. When the runner exits, the orchestrator downloads anything the agent wrote to
   `/mnt/session/outputs`, then hibernates (default), leaves running, or deletes
   the sandbox per `CMA_ON_IDLE` (see [Multi-turn sessions](#multi-turn-sessions)).

Tool **inputs and outputs** still flow to Anthropic so the model can reason over
results — only execution stays in your infrastructure. See Anthropic's
[security model](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes-security).

## Prerequisites

- A PandaStack account + API token (`pds_...`), and the **`claude-agent`**
  template available on your hosts (it ships in the first-party catalog).
- An Anthropic account with Managed Agents access (the
  `managed-agents-2026-04-01` beta).
- Python 3.10+.

## Setup

```bash
cd examples/claude-managed-agents
pip install -r requirements.txt

# 1. Create the Anthropic environment + a demo agent (prints the ids).
export ANTHROPIC_API_KEY=sk-ant-api03-...      # your org key
./setup_anthropic.sh

# 2. Generate the environment KEY in the Console (Console-only):
#      Workspace > Environments > pandastack-self-hosted > Generate environment key

# 3. Configure the orchestrator.
cp .env.example .env      # then fill in the four required values
set -a; . ./.env; set +a
```

The four values the orchestrator needs:

| Variable | What | Where |
|---|---|---|
| `ANTHROPIC_ENVIRONMENT_ID` | `env_...` | printed by `setup_anthropic.sh` |
| `ANTHROPIC_ENVIRONMENT_KEY` | `sk-ant-oat01-...` | Console → Generate environment key |
| `PANDASTACK_TOKEN` | `pds_...` | PandaStack dashboard → API Tokens |
| `PANDASTACK_API` | API base URL | `https://api.pandastack.ai` (default) |

> The orchestrator host only ever holds the **environment key**, never your org
> API key. The environment key authenticates polling for this one environment
> and nothing else in your account.

## Run

In one terminal, start the worker:

```bash
python orchestrator.py
# [hh:mm:ss] PandaStack worker 'pandastack-…' polling environment env_…
```

In another, drive a session (uses your org API key + the demo agent id):

```bash
export ANTHROPIC_API_KEY=sk-ant-api03-...
export CMA_AGENT_ID=agent_...     # printed by setup_anthropic.sh
python run_demo.py "Write a haiku about microVMs to /mnt/session/outputs/haiku.txt"
```

You'll see the agent's activity stream in the `run_demo.py` terminal and the
sandbox lifecycle in the `orchestrator.py` terminal. Anything the agent writes
to `/mnt/session/outputs` lands in `./outputs/<session-id>/`.

Check the queue / worker liveness at any time (uses your org API key):

```bash
curl -s "https://api.anthropic.com/v1/environments/$ANTHROPIC_ENVIRONMENT_ID/work/stats" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "anthropic-beta: managed-agents-2026-04-01" | jq
# { "depth": 0, "pending": 0, "workers_polling": 1, ... }
```

## Multi-turn sessions

Managed Agents sessions are multi-turn: after the agent goes idle (`end_turn`),
sending another `user.message` to the same session re-queues work, and the
orchestrator routes it back to the same sandbox. What happens to that sandbox
*between* turns is controlled by `CMA_ON_IDLE`:

| `CMA_ON_IDLE` | Between turns | Next-turn latency | `/workspace` state | Best for |
|---|---|---|---|---|
| `hibernate` (default) | Snapshot + stop; frees CPU/RAM | Sub-second auto-wake | Preserved | Stateful multi-turn at scale |
| `running` | Left up, holds CPU/RAM | Instant (VM already up) | Preserved | Low-concurrency / latency-sensitive |
| `delete` | Torn down | ~180 ms recreate | **Lost** | One-shot / stateless agents |

`hibernate` is the default because it's the only mode that is both stateful and
resource-efficient at rest: the sandbox isn't holding a VM's CPU/RAM while the
user thinks, yet the agent resumes with its files (and process memory) intact in
under a second. `running` trades idle cost for the lowest next-turn latency;
`delete` is cheapest at rest but starts each turn from a clean `/workspace`.

A safety-net TTL (`CMA_SANDBOX_TTL_SECONDS`, default 1h) reaps a session's
sandbox if it's abandoned, in every mode. The orchestrator also self-heals: if a
hibernated sandbox can't be woken on a later turn, it recreates a fresh one
rather than stranding the session.

## How this maps to PandaStack

| Managed Agents concept | PandaStack mechanism |
|---|---|
| Self-hosted sandbox (per session) | `Sandbox.create(template="claude-agent")` — ~180 ms snapshot restore |
| `--workdir /workspace` | The template's working directory |
| `/mnt/session/outputs` | Pulled via the filesystem API at session end |
| Pause between turns | `sandbox.hibernate()` → auto-wake on next exec |
| Session → sandbox mapping | Sandbox `metadata["cma.session_id"]` |
| Queue depth for autoscaling | Anthropic `work/stats.depth` |

## Files

- `orchestrator.py` — the always-on worker: poll → create/reuse sandbox →
  launch runner → collect outputs → hibernate/delete.
- `run_demo.py` — creates a session and streams the agent's activity, to
  exercise the orchestrator end to end.
- `setup_anthropic.sh` — creates the `self_hosted` environment + a demo agent.
- `.env.example` — config template.

## Production notes

- **Always-on vs. webhook.** This example is an always-on poller (simplest:
  only needs outbound HTTPS). To avoid an idle poller, drive the same
  create-and-launch logic from a webhook handler that fires on
  `session.status_run_started`. PandaStack serverless functions can host that
  handler.
- **Scaling.** Run more orchestrator processes (each with a distinct
  `CMA_WORKER_ID`) and let them share the queue; alert/scale on
  `work/stats.depth`.
- **Outputs at rest.** `/mnt/session/outputs` is pulled to local disk here. For
  durable storage, push to a bucket. (A PandaStack volume mount would force a
  cold boot and forfeit the snapshot-restore fast path, so the orchestrator
  copies via the filesystem API instead.)
- **Staging session files in.** Anthropic does not mount files or repos into
  self-hosted sandboxes. Pass references (an S3 path, a commit SHA) in the
  session `metadata`; the orchestrator can read that off the work item and stage
  the files into `/workspace` before launching the runner.

## Known issues

- **`ant` 1.12.1 rejects empty tool output.** If an agent `bash` tool call
  succeeds but produces no stdout (e.g. `cd /workspace`, `true`, or a command
  that only writes a file), the session stalls: the runner posts a tool result
  whose text block is empty, and Anthropic's API rejects it with
  `400 invalid_request_error` (`events.0.content.0.text: minimum string length
  is 1`). The runner logs `tool result send hit permanent 4xx; not retrying`
  and never delivers the result, so the agent waits forever.

  This is an **upstream SDK bug**, not a PandaStack one — the sandbox and tool
  execution are fine. Root cause (read from `anthropic-sdk-go` v1.50.1, the
  version `ant` 1.12.1 ships): the bash tool returns `("", false)` on empty
  success (`tools/agenttoolset/bash.go`), which `Execute` wraps as a single
  empty-string text block via `textResult("")`. The session tool runner has a
  guard — `if len(blocks) == 0 { blocks = textOnlyResult("(no output)") }`
  (`betasessiontoolrunner.go`) — but it only fires for an empty *slice*, not a
  slice holding one empty *string*, so the empty block is posted. There is no
  newer release that fixes it (HEAD == v1.50.1).

  **Mitigation (applied):** the demo agent's system prompt (see
  `setup_anthropic.sh`) instructs the model to append a confirmation like
  `&& echo done` to any command that would print nothing. This avoids the
  empty-output path in practice. When a fixed SDK ships and `ant` re-releases,
  bump `ANT_VERSION` in `templates/claude-agent/Dockerfile` +
  `scripts/bake-templates.sh` and rebake.

- **Hibernate/wake (`CMA_ON_IDLE=hibernate`) needs PandaStack agent ≥ the
  vsock-wake fix.** Older agents leave the guest's vsock Unix socket bound when
  hibernating a snapshot-fast-path sandbox, so the wake's `/snapshot/load` fails
  with `VsockUnixBackend: Address in use (os error 98)`. The orchestrator
  self-heals (it recreates a fresh sandbox if a wake fails), so sessions never
  strand — but state in `/workspace` is lost on that turn. Run `CMA_ON_IDLE=delete`
  if your agent build predates the fix.
