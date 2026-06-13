# Terraform self-host examples

These examples are community skeletons for running PandaStack agents outside the local Mac developer path.

- `aws/single-node`: one bare-metal EC2 host with nested virtualization suitable for Firecracker.
- `gcp/single-node`: one GCE VM with nested virtualization enabled.
- `fly/single-node`: Fly Machines notes and `fly.toml`; Firecracker-on-Firecracker support is TBD.

They intentionally do not include production PandaStack cloud, Cloudflare, DNS, observability, or secret-manager configuration. Treat them as starting points and review every value before applying.

> Token warning: the simple examples pass `node_token` through startup/user-data. That is easy to try, but it can appear in Terraform state and cloud metadata. For production, use a cloud secret manager or short-lived bootstrap token.
