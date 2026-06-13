// SPDX-License-Identifier: Apache-2.0
// Package firecracker wraps firecracker-go-sdk to expose a small Driver type
// per microVM. Each Driver owns one firecracker process and its REST socket.
package firecracker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"github.com/pandastack/agent/internal/network"
)

type VolumeMount struct {
	Name     string // logical name
	HostPath string // path to ext4 image on host
	ReadOnly bool
}

type VsockSpec struct {
	UDSPath string // host-side unix socket FC will bind/proxy
	CID     uint32 // guest CID; must be >= 3
}

// BakedVsockDir is the FIXED directory the vsock UDS is baked into at
// template-snapshot time. FC re-creates the listener at the same absolute
// path on every restore (the path is frozen in vm.state). To give each
// restored sandbox its OWN socket inode without serializing restores, the
// agent spawns each restored firecracker in a private mount namespace and
// bind-mounts a per-sandbox directory over BakedVsockDir (see Spec.VsockBindDir
// and StartFromSnapNoDrive). Every FC binds the same baked path; the kernel
// resolves it into a distinct per-sandbox directory.
const (
	BakedVsockDir  = "/run/pandastack/vsock"
	BakedVsockName = "fc-vsock.sock"
	BakedVsockPath = BakedVsockDir + "/" + BakedVsockName
)

// MMDSSpec, if Enabled, configures Firecracker's MicroVM Metadata Service
// on the primary NIC. Guest reads identity from http://Address (typically
// 169.254.169.254). MMDS state survives snapshots and is updatable via
// FC API between /snapshot/load and /vm Resume — this is the zero-config
// restore primitive that replaces vsock-based reconfiguration.
type MMDSSpec struct {
	Enabled bool
	Address string // typically "169.254.169.254"
}

type Spec struct {
	ID          string
	KernelPath  string
	RootfsPath  string
	SocketPath  string
	LogPath     string
	ConsolePath string
	CPUs        int
	MemoryMB    int
	Network     network.Allocation
	Vsock       VsockSpec // optional: if UDSPath set, vsock device attached on cold boot
	MMDS        MMDSSpec  // optional: enable FC MMDS for zero-config restores
	FromSnapDir string    // if non-empty, restore from snapshot at this dir
	Volumes     []VolumeMount
	// Netns, if non-empty, causes the firecracker process to be spawned
	// inside this Linux network namespace via `ip netns exec`. Used by
	// the NAT-identity restore path (see internal/netns).
	Netns string
	// VsockBindDir, if non-empty (restore path only), is a per-sandbox host
	// directory bind-mounted over BakedVsockDir inside a private mount
	// namespace before firecracker exec's. This makes the baked vsock UDS
	// path resolve to a per-sandbox inode so concurrent NATID restores never
	// collide on the single frozen path. See StartFromSnapNoDrive.
	VsockBindDir string
}

type Driver struct {
	spec    Spec
	log     *slog.Logger
	machine *fc.Machine
	console *os.File
	proc    *os.Process // set when started via StartWarmFork (no SDK Machine)

	// uffd* are populated only on the streaming-restore path
	// (PANDASTACK_STREAM_RESTORE=1). The handler serves guest page faults from
	// the resolver for the lifetime of the VM; closeUffd tears it all down.
	uffd uffdRestore

	// hugepages records that THIS VM's guest memory is 2 MiB hugetlbfs-backed,
	// either because it cold-booted with PANDASTACK_HUGEPAGES=1 or because it
	// was restored from a snapshot carrying the hugepages marker. Snapshots
	// taken from this VM inherit the marker (see markSnapshotHugepages).
	hugepages bool
}

// PID returns the underlying firecracker process ID, or 0 if not running.
func (d *Driver) PID() int {
	if d.proc != nil {
		return d.proc.Pid
	}
	if d.machine == nil {
		return 0
	}
	if p, err := d.machine.PID(); err == nil {
		return p
	}
	return 0
}

// LogPath returns the firecracker log file path.
func (d *Driver) LogPath() string { return d.spec.LogPath }

// ConsolePath returns the per-sandbox serial console capture file.
func (d *Driver) ConsolePath() string { return d.spec.ConsolePath }

func NewDriver(spec Spec, log *slog.Logger) *Driver {
	return &Driver{spec: spec, log: log}
}

// Start spawns a firecracker process and either boots fresh or restores a snapshot.
func (d *Driver) Start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.spec.LogPath), 0o755); err != nil {
		return err
	}

	// Remove any stale unix socket at this path before launching. The
	// firecracker-go-sdk's validate.Cfg handler refuses to start when the
	// socket already exists ("socket ... already exists"), so a prior boot
	// attempt for the SAME sandbox id that died without cleaning up (e.g. a
	// snapshot restore that failed mid-flight and returned before Stop) would
	// otherwise block the cold-boot fallback indefinitely. The socket path is
	// unique per sandbox id; if one survives here its owner is already gone.
	if d.spec.SocketPath != "" {
		_ = os.Remove(d.spec.SocketPath)
	}

	cfg := fc.Config{
		SocketPath: d.spec.SocketPath,
		LogPath:    d.spec.LogPath,
		LogLevel:   "Info",
	}
	if d.spec.FromSnapDir == "" {
		// Fresh boot: declare full boot config.
		cfg.KernelImagePath = d.spec.KernelPath
		cfg.MachineCfg = fcmodels.MachineConfiguration{
			VcpuCount:  int64Ptr(int64(d.spec.CPUs)),
			MemSizeMib: int64Ptr(int64(d.spec.MemoryMB)),
		}
		cfg.Drives = []fcmodels.Drive{{
			DriveID:      strPtr("rootfs"),
			PathOnHost:   strPtr(d.spec.RootfsPath),
			IsRootDevice: boolPtr(true),
			IsReadOnly:   boolPtr(false),
		}}
		for i, v := range d.spec.Volumes {
			cfg.Drives = append(cfg.Drives, fcmodels.Drive{
				DriveID:      strPtr(fmt.Sprintf("vol%d", i+1)),
				PathOnHost:   strPtr(v.HostPath),
				IsRootDevice: boolPtr(false),
				IsReadOnly:   boolPtr(v.ReadOnly),
			})
		}
		cfg.NetworkInterfaces = fc.NetworkInterfaces{{
			AllowMMDS: d.spec.MMDS.Enabled,
			StaticConfiguration: &fc.StaticNetworkConfiguration{
				MacAddress:  d.spec.Network.MAC,
				HostDevName: d.spec.Network.TAP,
			},
		}}
		if d.spec.MMDS.Enabled {
			addr := d.spec.MMDS.Address
			if addr == "" {
				addr = "169.254.169.254"
			}
			cfg.MmdsAddress = net.ParseIP(addr)
			cfg.MmdsVersion = fc.MMDSv1
		}
		// Optional: attach a vsock device. Each sandbox uses its own
		// vmDir/fc-vsock.sock so cold boots never collide. This is
		// used by pandastack-init for sub-second network reconfiguration
		// when restoring from a template snapshot — but we attach it
		// on every cold boot so future restores Just Work, and so the
		// first cold boot can become the template snapshot itself.
		if d.spec.Vsock.UDSPath != "" {
			cfg.VsockDevices = []fc.VsockDevice{{
				ID:   "v1",
				Path: d.spec.Vsock.UDSPath,
				CID:  d.spec.Vsock.CID,
			}}
		}
		cfg.KernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"
		// Kernel-level IP autoconfig: configures eth0 with the allocated
		// guest IP/gateway BEFORE userspace runs. Avoids the need for
		// DHCP or for pandastack-init to be the only path that brings the
		// network up — required for the snapshot-bake build VM whose
		// SSH probe runs before any identity push.
		if d.spec.Network.GuestIP != "" && d.spec.Network.HostIP != "" {
			cfg.KernelArgs += fmt.Sprintf(
				" ip=%s::%s:255.255.255.252::eth0:off",
				d.spec.Network.GuestIP, d.spec.Network.HostIP,
			)
		}
	}
	// When restoring from snapshot, firecracker rejects boot-config calls.
	// The SDK's WithSnapshot removes the CreateMachine/Drives/Network/KernelArgs
	// handlers, but NOT AddVsocks — so we must leave VsockDevices empty here
	// (the snapshot encodes the vsock device internally).

	// Firecracker must outlive any request-scoped context. Use background.
	machineCtx := context.Background()

	cmd := fc.VMCommandBuilder{}.
		WithBin("/usr/local/bin/firecracker").
		WithSocketPath(d.spec.SocketPath)

	// Capture serial console (kernel boot + guest stdout) to per-sandbox file
	// while still teeing to agent stdout for live debugging.
	if d.spec.ConsolePath != "" {
		f, err := os.OpenFile(d.spec.ConsolePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("open console file: %w", err)
		}
		d.console = f
		cmd = cmd.WithStdout(io.MultiWriter(os.Stdout, f)).
			WithStderr(io.MultiWriter(os.Stderr, f))
	} else {
		cmd = cmd.WithStdout(os.Stdout).WithStderr(os.Stderr)
	}

	built := cmd.Build(machineCtx)
	opts := []fc.Opt{fc.WithProcessRunner(built)}
	if d.spec.FromSnapDir != "" {
		statePath := filepath.Join(d.spec.FromSnapDir, "vm.state")
		if snapshotHasHugepages(d.spec.FromSnapDir) {
			// Hugepage snapshots can ONLY be restored through a UFFD memory
			// backend — Firecracker rejects mem_file_path for hugetlbfs-backed
			// guests. Stand up the userfaultfd handler (local vm.mem file
			// source, or the GCS sidecar on a thin seed) and point the SDK's
			// /snapshot/load at its handoff socket instead of the file.
			d.hugepages = true
			sock, err := d.beginUffdRestore(d.spec.FromSnapDir)
			if err != nil {
				return fmt.Errorf("uffd restore setup (hugepages): %w", err)
			}
			// Serve BEFORE the load: FC faults guest pages while restoring
			// device/vCPU state mid-load and blocks until they are serviced.
			d.serveUffd()
			opts = append(opts, fc.WithSnapshot("", statePath,
				fc.WithMemoryBackend(fcmodels.MemoryBackendBackendTypeUffd, sock)))
		} else {
			opts = append(opts, fc.WithSnapshot(
				filepath.Join(d.spec.FromSnapDir, "vm.mem"),
				statePath,
			))
		}
	}

	m, err := fc.NewMachine(machineCtx, cfg, opts...)
	if err != nil {
		d.closeUffd()
		return fmt.Errorf("new machine: %w", err)
	}
	d.machine = m

	// Fresh boots opted into hugepages: re-PUT /machine-config with
	// huge_pages="2M" right after the SDK's own CreateMachine PUT (the pinned
	// SDK model predates the field). Must run before boot-source/InstanceStart,
	// hence an FcInit handler rather than a post-Start call.
	//
	// Gated on hugePagesFit: a guest that doesn't fit the host's hugetlb
	// overcommit budget boots fine but EFAULTs later when the snapshot dump
	// faults in every page (see hugePagesFit). Fall back to 4 KiB pages —
	// the snapshot then simply carries no hugepages marker.
	if d.spec.FromSnapDir == "" && hugePagesEnabled() {
		if ok, reason := hugePagesFit(d.spec.MemoryMB); !ok {
			if d.log != nil {
				d.log.Warn("hugepages: over hugetlb budget, falling back to 4 KiB pages",
					"id", d.spec.ID, "mem_mb", d.spec.MemoryMB, "reason", reason)
			}
		} else {
			d.hugepages = true
			m.Handlers.FcInit = m.Handlers.FcInit.AppendAfter(fc.CreateMachineHandlerName, fc.Handler{
				Name: "pandastack.HugePages",
				Fn: func(ctx context.Context, _ *fc.Machine) error {
					return d.applyHugePagesConfig()
				},
			})
		}
	}

	// IMPORTANT: the firecracker-go-sdk uses the context passed to Start()
	// not only for the Start RPC itself but as the machine *lifetime* — when
	// that ctx is cancelled, the SDK sends SIGTERM to the firecracker process.
	// So we MUST pass a never-cancelling context here, and apply the
	// start-timeout by racing Start against a timer in a goroutine.
	errCh := make(chan error, 1)
	go func() { errCh <- m.Start(machineCtx) }()
	select {
	case err := <-errCh:
		if err != nil {
			d.closeUffd()
			return fmt.Errorf("start: %w", err)
		}
	case <-time.After(30 * time.Second):
		_ = m.StopVMM()
		d.closeUffd()
		return fmt.Errorf("firecracker start timed out after 30s")
	}

	// After Start, a snapshot-loaded VM is paused; resume it.
	if d.spec.FromSnapDir != "" {
		if err := m.ResumeVM(context.Background()); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
	}
	return nil
}

func (d *Driver) Stop(ctx context.Context) error {
	return d.stopInternal(ctx, false)
}

// FastStop skips the graceful CtrlAltDel handshake and SIGKILLs the FC
// process immediately. Cuts ~2-3s off the delete critical path for
// ephemeral sandboxes where guest state is throwaway.
func (d *Driver) FastStop(ctx context.Context) error {
	return d.stopInternal(ctx, true)
}

func (d *Driver) stopInternal(ctx context.Context, fast bool) error {
	// Warm-fork driver (raw process, no SDK).
	if d.proc != nil {
		if !fast {
			hc := newUnixHTTP(d.spec.SocketPath)
			_ = putJSON(hc, "/actions", map[string]string{"action_type": "SendCtrlAltDel"})
		}
		done := make(chan struct{})
		go func() { _, _ = d.proc.Wait(); close(done) }()
		if fast {
			// Kill the entire process group (PGID == PID because fcExecCommand
			// sets Setpgid=true). This ensures the firecracker child spawned by
			// "ip netns exec" is also killed — without this, fc is orphaned and
			// keeps the dm-mapper device and netns alive, causing ip-netns-del to
			// hang indefinitely and blocking the pool mutex for all future deletes.
			_ = syscall.Kill(-d.proc.Pid, syscall.SIGKILL)
			<-done
		} else {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = d.proc.Kill()
				<-done
			}
		}
		d.proc = nil
		_ = os.Remove(d.spec.SocketPath)
		// Tear down the streaming-restore handler only after FC is gone, so no
		// page fault is in flight against the resolver we are about to close.
		d.closeUffd()
		if d.console != nil {
			_ = d.console.Close()
			d.console = nil
		}
		return nil
	}

	if d.machine == nil {
		return nil
	}
	if fast {
		// machine.StopVMM sends SIGTERM which can be slow; if we have the PID
		// SIGKILL directly.
		if pid, err := d.machine.PID(); err == nil && pid > 0 {
			_ = syscallKill(pid)
		}
	}
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.machine.StopVMM(); err != nil {
		d.log.Warn("StopVMM error", "err", err)
	}
	_ = d.machine.Wait(stopCtx)
	_ = os.Remove(d.spec.SocketPath)
	if d.spec.Vsock.UDSPath != "" {
		_ = os.Remove(d.spec.Vsock.UDSPath)
	}
	_ = os.Remove(filepath.Join(filepath.Dir(d.spec.SocketPath),
		fmt.Sprintf("fc-vsock-%s.sock", d.spec.ID)))
	if d.console != nil {
		_ = d.console.Close()
		d.console = nil
	}
	return nil
}

func (d *Driver) Pause(ctx context.Context) error {
	if d.proc != nil {
		return patchJSON(newUnixHTTP(d.spec.SocketPath), "/vm", map[string]string{"state": "Paused"})
	}
	if d.machine == nil {
		return fmt.Errorf("machine not started")
	}
	return d.machine.PauseVM(ctx)
}

func (d *Driver) Resume(ctx context.Context) error {
	if d.proc != nil {
		return patchJSON(newUnixHTTP(d.spec.SocketPath), "/vm", map[string]string{"state": "Resumed"})
	}
	if d.machine == nil {
		return fmt.Errorf("machine not started")
	}
	return d.machine.ResumeVM(ctx)
}

// PutMMDS overwrites the MicroVM Metadata Service content. Works on both
// SDK-managed (cold-boot) and raw HTTP (snapshot-restore) processes. The
// guest reads this via http://169.254.169.254/<key> after AllowMMDS=true.
// Idempotent — call as many times as needed.
func (d *Driver) PutMMDS(ctx context.Context, body any) error {
	hc := newUnixHTTP(d.spec.SocketPath)
	return putJSON(hc, "/mmds", body)
}

func (d *Driver) CreateSnapshot(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mem := filepath.Join(dir, "vm.mem")
	state := filepath.Join(dir, "vm.state")

	// Raw HTTP path: snapshot-restored VMs were spawned outside the SDK
	// (see warmfork.go::StartFromSnapNoDrive), so d.machine is nil; we
	// drive firecracker directly over its unix socket.
	if d.proc != nil {
		hc := newUnixHTTP(d.spec.SocketPath)
		if err := patchJSON(hc, "/vm", map[string]string{"state": "Paused"}); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		defer func() {
			_ = patchJSON(hc, "/vm", map[string]string{"state": "Resumed"})
		}()
		// Snapshot can take 10-60s on multi-GB guests (FC writes the full
		// memory image to disk synchronously). The default 10s client times
		// out, so use the long-timeout client for the create call.
		hcLong := newUnixHTTPLong(d.spec.SocketPath)
		if err := putJSON(hcLong, "/snapshot/create", map[string]any{
			"snapshot_type": "Full",
			"snapshot_path": state,
			"mem_file_path": mem,
		}); err != nil {
			return fmt.Errorf("create snapshot: %w", err)
		}
		if d.hugepages {
			d.markSnapshotHugepages(dir)
		}
		return nil
	}

	if d.machine == nil {
		return fmt.Errorf("machine not started")
	}
	if err := d.machine.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	defer func() { _ = d.machine.ResumeVM(ctx) }()

	if err := d.machine.CreateSnapshot(ctx, mem, state); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	if d.hugepages {
		d.markSnapshotHugepages(dir)
	}
	return nil
}

// PauseCopyResume pauses the VM, copies the rootfs to dst for a consistent
// disk snapshot, then resumes the VM. Used by rootfs-clone fork.
func (d *Driver) PauseCopyResume(ctx context.Context, src, dst string) error {
	// Raw-HTTP path: snapshot-restored VMs are spawned outside the
	// firecracker-go-sdk (d.machine == nil), so drive firecracker directly over
	// its unix socket — mirrors CreateSnapshot/Pause/Resume.
	if d.proc != nil {
		hc := newUnixHTTP(d.spec.SocketPath)
		if err := patchJSON(hc, "/vm", map[string]string{"state": "Paused"}); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		defer func() { _ = patchJSON(hc, "/vm", map[string]string{"state": "Resumed"}) }()
		return copyFileLocal(src, dst)
	}
	if d.machine == nil {
		return fmt.Errorf("machine not started")
	}
	if err := d.machine.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	defer func() { _ = d.machine.ResumeVM(ctx) }()
	return copyFileLocal(src, dst)
}

// PauseForFork pauses the VM, copies the rootfs to rootfsDst for a consistent
// view, snapshots memory+state into dir, then resumes the VM.
func (d *Driver) PauseForFork(ctx context.Context, dir, rootfsDst, rootfsSrc string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mem := filepath.Join(dir, "vm.mem")
	state := filepath.Join(dir, "vm.state")

	// Raw-HTTP path: snapshot-restored VMs are spawned outside the
	// firecracker-go-sdk (d.machine == nil), so drive firecracker directly over
	// its unix socket — mirrors PauseAndSnapshot.
	if d.proc != nil {
		hc := newUnixHTTP(d.spec.SocketPath)
		if err := patchJSON(hc, "/vm", map[string]string{"state": "Paused"}); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		defer func() { _ = patchJSON(hc, "/vm", map[string]string{"state": "Resumed"}) }()
		// Copy rootfs while VM is paused (no in-flight writes).
		if err := copyFileLocal(rootfsSrc, rootfsDst); err != nil {
			return fmt.Errorf("copy rootfs: %w", err)
		}
		// Full-memory snapshot can take 10-60s; use the long-timeout client.
		hcLong := newUnixHTTPLong(d.spec.SocketPath)
		if err := putJSON(hcLong, "/snapshot/create", map[string]any{
			"snapshot_type": "Full",
			"snapshot_path": state,
			"mem_file_path": mem,
		}); err != nil {
			return fmt.Errorf("create snapshot: %w", err)
		}
		if d.hugepages {
			d.markSnapshotHugepages(dir)
		}
		return nil
	}

	if d.machine == nil {
		return fmt.Errorf("machine not started")
	}
	if err := d.machine.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	defer func() { _ = d.machine.ResumeVM(ctx) }()

	// Copy rootfs while VM is paused (no in-flight writes).
	if err := copyFileLocal(rootfsSrc, rootfsDst); err != nil {
		return fmt.Errorf("copy rootfs: %w", err)
	}
	if err := d.machine.CreateSnapshot(ctx, mem, state); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	if d.hugepages {
		d.markSnapshotHugepages(dir)
	}
	return nil
}

// PauseAndSnapshot pauses the VM, snapshots into dir, and leaves it paused.
// Caller is expected to call Stop() next (used for hibernation).
func (d *Driver) PauseAndSnapshot(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mem := filepath.Join(dir, "vm.mem")
	state := filepath.Join(dir, "vm.state")

	if d.proc != nil {
		hc := newUnixHTTP(d.spec.SocketPath)
		if err := patchJSON(hc, "/vm", map[string]string{"state": "Paused"}); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		// Snapshot can take 10-60s on multi-GB guests (FC writes the full
		// memory image to disk synchronously). Use the long-timeout client.
		hcLong := newUnixHTTPLong(d.spec.SocketPath)
		if err := putJSON(hcLong, "/snapshot/create", map[string]any{
			"snapshot_type": "Full",
			"snapshot_path": state,
			"mem_file_path": mem,
		}); err != nil {
			return fmt.Errorf("create snapshot: %w", err)
		}
		if d.hugepages {
			d.markSnapshotHugepages(dir)
		}
		return nil
	}

	if d.machine == nil {
		return fmt.Errorf("machine not started")
	}
	if err := d.machine.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := d.machine.CreateSnapshot(ctx, mem, state); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	if d.hugepages {
		d.markSnapshotHugepages(dir)
	}
	return nil
}

func copyFileLocal(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func int64Ptr(v int64) *int64 { return &v }
func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }
