# PandaStack guest kernel 6.1

Our build of the Linux 6.1 LTS guest kernel for Firecracker VMs. Templates boot Firecracker's CI build of 5.10 (`vmlinux-5.10.x`) unless they name another kernel.

## What it is

- **Source:** kernel.org `linux-6.1.192`, no patches.
- **Base config:** `firecracker-ci-6.1.188.config`, Firecracker's own CI config for 6.1. It was read out of their `vmlinux-6.1.188` build (`CONFIG_IKCONFIG`).
- **Our additions:** `ps-6.1.fragment`.
- **Resolved config:** `config-ps-6.1.192.1`, the full result for build 1.

The additions:

| Option | Why |
|---|---|
| `BPF_JIT`, `BPF_LSM`, `DEBUG_INFO_BTF`, `KPROBES`, `KPROBE_EVENTS`, `UPROBE_EVENTS`, `FTRACE`, `FUNCTION_TRACER`, `DYNAMIC_FTRACE`, `FTRACE_SYSCALLS`, `BPF_EVENTS` | Observability and security daemons inside a sandbox. CO-RE tools need BTF. |
| `NETFILTER_XT_MATCH_COMMENT` | `iptables -m comment`. kube-proxy (k3s) cannot set up ClusterIP without it. |
| `MODULES` | In 6.1, the BPF JIT, kprobes and BPF LSM all depend on it. We ship no modules, and every option is built in. Each sandbox is its owner's own VM, and they are root in it already, so this changes nothing between tenants. |
| `CRYPTO_MANAGER_DISABLE_TESTS` | Same as 5.10: no crypto self-tests on every cold boot or bake. |

Debug info is stripped after the build, and the `.BTF` section stays (about 5 MB, read only when a tool asks). The file is 64.6 MB, against 45 MB for Firecracker's 6.1 and 39 MB for 5.10.

### Compared with 5.10

Only three options that 5.10 turns on and 6.1 could select are off:

- **Crypto self-tests:** now off again via the fragment.
- **`VIRTIO_MMIO_CMDLINE_DEVICES` and `X86_MPPARSE`:** Firecracker describes devices and CPUs through ACPI, and its own 6.1 config leaves both off.

The other 40 names no longer exist in 6.1, or were Amazon Linux additions in Firecracker's 5.10 build:

- **`SYSGENID`:** replaced upstream by `VMGENID`, which is on and bound in the guest. A restored clone reseeds its random numbers.
- **`PTP_1588_CLOCK_VMCLOCK`:** not needed. The agent sets the guest clock on every restore (`agent/internal/sandbox/clock.go`).

### Measured (2026-11-21, one n2-standard-8 test host, same image and size each time)

| | 5.10.239 | Firecracker 6.1.188 | **ps 6.1.192.1** |
|---|---|---|---|
| Restore start p50 / p90 (30 creates, 256 MiB) | 100 / 151 ms | 69 / 92 ms | **58 / 71 ms** |
| Memory per idle sandbox (rough: drop in free memory / 30) | ~29 MB | ~23 MB | **~21 MB** |
| Bake (cold boot + snapshot) | 30 s | 30 s | 31 s |
| `iptables -m comment` | fails (nft not supported) | — | works |
| Docker inside the sandbox (`docker run hello-world`) | dockerd fails | — | works |
| bpftrace kprobe program, BTF types | fails, no BTF | no BTF | works, BTF present |

Exec, files, exec/stream, network, DNS, clock, and hibernate + wake (265 ms, file kept) all pass. BPF LSM is active in the guest's LSM list (`capability,landlock,selinux,bpf`). Attaching an LSM program was not tested, because the bpftrace in Ubuntu 24.04 rejects `lsm:` probes on both kernels.

## Build

```bash
V=6.1.192 BUILD=1 kernel/guest-6.1/build.sh
```

It runs in an `ubuntu:24.04` container (Docker and network needed), about 5 minutes on 8 cores. It stops if a fragment option does not apply, if anything would be built as a module, or if the `.BTF` section is missing. The result is in `kernel/guest-6.1/work/out/`. Bump `BUILD` for a new config at the same kernel version; bump `V` for a new 6.1.y release.

`SHA256SUMS` holds the hash of our build 1 (`vmlinux-ps-6.1.192.1`). Your own build gives a different file (the kernel records when and where it was built), so check it against the `SHA256SUMS` the build writes next to it in `work/out/`.

## Use it

1. **Put the file on each agent host.** Copy `vmlinux-ps-6.1.192.1` into the agent's `kernels/` folder (`/var/lib/pandastack/kernels/` by default) and check its sha256. Keep only kernel files in that folder: the agent picks kernels by file name.
2. **Point a template at it.** A template's `meta.json` names its kernel (`"kernel": "vmlinux-5.10"` by default). Set it to `vmlinux-ps-6.1`:
   - `pandastack template build -f templates/<name>/Dockerfile -n <name> --kernel vmlinux-ps-6.1`, or
   - `sudo FORCE=1 KERNEL=vmlinux-ps-6.1 scripts/bake-templates.sh <name>`, or
   - edit `/var/lib/pandastack/templates/<name>/meta.json` by hand.

   The agent resolves the name to the highest matching file (`vmlinux-ps-6.1` → `vmlinux-ps-6.1.192.1`). A template that names a kernel the host does not have boots the host default, and the agent logs that once.
3. **Re-bake that template.** A kernel change takes effect only when the template's snapshot is baked again: remove `/var/lib/pandastack/template-snaps/<name>` (the bake script with `FORCE=1` does this for you), and the next create bakes it. Restores keep the kernel inside the snapshot, and the agent never re-bakes just because the kernel changed.

Adding a kernel file to a host changes nothing by itself. The host default stays 5.10 (`PANDASTACK_KERNEL_DEFAULT`, unset = `vmlinux-5.10`).

## Roll back

Set the template's kernel back to `vmlinux-5.10` and re-bake it.
