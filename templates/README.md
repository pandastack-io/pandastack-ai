# PandaStack Templates

Reference Dockerfiles for the 5 first-party templates we ship out of the box,
plus an example for custom builds.

Each subdirectory contains a `Dockerfile` you can pass straight to:

```sh
pandastack template build -f templates/<name>/Dockerfile -n <name>
```

The Dockerfile is baked into a Firecracker rootfs snapshot and registered with
the agent. From then on, sandboxes boot from the snapshot in ~240ms.

## Catalog

| Template            | Base                       | Use case                                          |
|---------------------|----------------------------|---------------------------------------------------|
| base                | ubuntu:24.04 + mise        | Universal apps runtime (Node/Python/Go/Bun)       |
| code-interpreter    | python:3.11 + node 22      | LLM tool-use, data analysis, Functions, agents    |
| agent               | ubuntu:24.04 + node 22     | Coding-agent CLIs: claude-code, codex, opencode   |
| browser             | ubuntu:24.04 + chromium    | Playwright scraping/RPA + crawl4ai extraction     |
| postgres-16         | ubuntu:24.04 + PGDG        | Managed PostgreSQL 16 (pgvector, pgbouncer)       |

## Custom builds

Bring any Dockerfile. The only requirement is that the image ships an init
system (`systemd`) + `sshd`. The guest agents — `pandastack-init` (identity)
and `pandastack-daemon` (the always-on vsock exec/fs fast-path) — and their
systemd units are injected into the exported rootfs by the bake pipeline
itself (CI: `.github/workflows/bake-templates.yml`; local:
`scripts/build-base-rootfs.sh`), so you do **not** add them to your Dockerfile:

```Dockerfile
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y openssh-server systemd-sysv ca-certificates
# ... your stack ...
# No COPY of pandastack-init/daemon needed — the bake injects + enables them.
```

Then:

```sh
pandastack template build -f Dockerfile -n my-custom-stack --size-mb 4096
```

## Notes

* All templates are baked from the `ubuntu-24.04-net` base on the host so
  `pandastack-init` + sshd wiring is preserved (see `scripts/bake-templates.sh`).
* The `agent` template ships every public coding-agent CLI in one image
  (`claude-code`, `codex`, `opencode` via `npm`). Pick which one to run by
  providing the matching API key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …) and
  invoking its command inside the sandbox.
