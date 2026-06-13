# PandaStack agent fleet — Ansible config push

Push the agent binary, Firecracker, and non-secret env knobs to the **existing**
agent VMs **without recreating them** — roll out new code to nodes that may be
hosting live sandboxes (apps, databases) instead of replacing the VM via a MIG
instance-template change.

## Why this is sandbox-safe

The `pandastack-agent` systemd unit is `KillMode=mixed`, so `systemctl restart`
sends `SIGTERM` to **only the agent PID** — the Firecracker child processes keep
running across the restart. The agent re-attaches to them on startup. (systemd
only `SIGKILL`s the whole cgroup after `TimeoutStopSec=180`, which a clean agent
exit never hits.) So a binary push + restart does **not** drop running microVMs.

## Birth vs. update

| Path | Who provisions | What |
|------|----------------|------|
| **Birth** | `cloud-init/user-data-agent.sh` | a brand-new MIG node, from the boot bucket |
| **Update** | this Ansible | existing nodes, from `gs://<bucket>/bin/pandastack-agent` |

Both pull the **same** artifacts, so a node is identical however it got there.

## Prerequisites

```bash
cd ansible
ansible-galaxy collection install -r requirements.yml
```

- **Auth (local):** `gcloud auth application-default login` (ADC) for the dynamic
  inventory, plus the `~/.ssh/google_compute_engine` key that `gcloud compute ssh`
  already manages. Both are already present on the maintainer's box.
- **Auth (CI):** Workload Identity Federation via `google-github-actions/auth@v2`
  provides ADC; the SSH key is generated/registered in the job.
- Connectivity is over **IAP** (agents have no public IP) using `iap-proxy.sh` as
  the SSH `ProxyCommand` — same tunnel `gcloud compute ssh --tunnel-through-iap`
  uses in `release.yml`.

## Usage

```bash
cd ansible

# 0. Who's in the fleet right now? (no SSH, just GCP API)
ansible-inventory --graph

# 1. Connectivity + state report
ansible-playbook ping.yml

# 2. Push the agent binary (idempotent; restarts only on change, serial=1)
ansible-playbook agent.yml

# 3. Install/upgrade Firecracker (rare)
ansible-playbook firecracker.yml

# 4. Roll out non-secret env knobs from group_vars/all.yml
ansible-playbook env.yml

# 5. Converge everything
ansible-playbook site.yml

# Dry run anything:
ansible-playbook agent.yml --check --diff
```

## What each role manages

- **common** — asserts the host is really an agent; ensures `/etc/pandastack`.
- **firecracker** — idempotent install of `firecracker`/`jailer` v`{{ firecracker_version }}`
  (mirrors cloud-init). No-op when already at version.
- **agent** — pulls `bin/pandastack-agent` from GCS on the VM, sha256-compares to
  the installed binary, atomically swaps + restarts **only when changed**, then
  health-checks `systemctl is-active`.
- **agent_env** — manages an allowlist of fleet-wide, non-secret knobs in
  `/etc/pandastack/env.agent` via `lineinfile`. A denylist (`agent_env_denylist`)
  hard-fails if a managed key ever collides with a secret/per-host key, so this
  can never rewrite `DB_DSN`, `NODE_TOKEN`, `SHARED_KEY`, `AGENT_ID`, `NATID_SLOT`,
  etc. — those stay owned by cloud-init.

## Safety knobs

- `serial: 1` — one node at a time; a bad push can't take out the whole fleet.
- `agent_restart_on_change` (group_vars) — set `false` to stage a binary without
  restarting.
- `--limit pandastack-agent-XXXX` — target a single node.

## CI/CD (GitHub Actions)

`.github/workflows/ansible-agent.yml` runs these same playbooks from CI
(`workflow_dispatch`, pick the playbook + an optional `--check` dry run). It:

1. Authenticates with the **same** Workload Identity Federation provider + service
   account as `release.yml`'s `deploy-agent` job (no static keys).
2. Installs `ansible-core` + the collections in `requirements.yml`.
3. `ssh-keygen`s an ephemeral key at `~/.ssh/google_compute_engine` and lets
   `gcloud compute ssh <user>@<vm>` register it into each VM's `ssh-keys`
   metadata (OS Login is off, so gcloud *merges* the key — it never clobbers the
   maintainer's). The login user is a dedicated `ansible-ci`, which the GCE guest
   agent grants NOPASSWD sudo — so `become` works. `-e ansible_user=ansible-ci`
   overrides the local default.
4. Runs the chosen playbook over IAP, serial:1, sandbox-safe.

This replaces the hand-rolled SSH loop in `release.yml`'s `deploy-agent` step
(which once silently left a node on the old binary when a `while read` consumed
the ssh stdin). Converging `release.yml` to *call* this workflow on tag is a
recommended follow-up.

## Why the MIG can't undo this

The agent MIG (`infra/terraform/modules/gcp-agent-mig`) is **frozen**:
`update_policy` is `OPPORTUNISTIC` with `minimal_action` and
`most_disruptive_allowed_action` both `REFRESH`, and `instance_redistribution_type`
`NONE`. So a changed instance template (new cloud-init, new image) is staged as
the **birth spec for future nodes only** — it never rolls/recreates a running
agent out from under its sandboxes. Live-node config changes go through *this*
Ansible. Autohealing is also lenient (6 × 15 s ≈ 90 s of sustained `/healthz`
failure before recreate) so a 1-2 s agent restart never trips it.
