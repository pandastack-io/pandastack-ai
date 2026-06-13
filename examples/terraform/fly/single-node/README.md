# Fly single-node notes

Fly Machines themselves are powered by Firecracker. Running PandaStack's Firecracker agent inside another Firecracker VM is not currently supported as a general-purpose path.

## Current status

TBD for direct Firecracker-on-Firecracker support.

## Workaround

Use Fly for lightweight control-plane-adjacent services and run PandaStack agents on hosts with direct hardware virtualization access, such as AWS bare-metal EC2, GCE nested virtualization, or your own Linux hosts.

The included `fly.toml` is a placeholder for future agent packaging experiments. Do not expect it to run microVM sandboxes today.

## Expected cost

A performance VM with 4 CPUs and 8 GB RAM can cost tens to low hundreds of USD per month depending on region and usage.

## Not included

- working nested Firecracker support
- load balancer
- multi-node scheduler wiring
- observability stack
- persistent snapshot storage
