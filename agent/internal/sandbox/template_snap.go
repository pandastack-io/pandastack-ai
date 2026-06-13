// SPDX-License-Identifier: Apache-2.0
// template_snap.go implements the sub-second cold-start path: a per-template
// snapshot taken once after first boot, then restored + reconfigured via the
// in-guest pandastack-init vsock agent on every subsequent create.
//
// Layout per template:
//   <dataDir>/template-snaps/<tpl>/
//     ready              -- empty marker file: snapshot is usable
//     vm.mem             -- FC memory dump
//     vm.state           -- FC device + cpu state
//     rootfs.snap.ext4   -- rootfs at moment of snapshot (consistent)
//     restore.sock       -- vsock UDS path encoded in the snapshot
//     build.log          -- build phase log for diagnostics
//
// Concurrency note (v1): FC encodes the vsock UDS path absolutely in vm.state
// and binds it for its lifetime; two concurrent restores of the same template
// would collide. v1 serializes via a per-template mutex held for the lifetime
// of the restored VM. Different templates restore in parallel.
// Future v2 will use unshare(CLONE_NEWNS) + bind mounts to give each restore
// its own view of the encoded path.
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mrand "math/rand"

	"github.com/pandastack/agent/internal/firecracker"
	"github.com/pandastack/agent/internal/guest"
	"github.com/pandastack/agent/internal/guest/vsockwire"
	"github.com/pandastack/agent/internal/memstream"
	"github.com/pandastack/agent/internal/network"
	"github.com/pandastack/agent/internal/seed"
)

// nilSignal is signal 0 — used to test whether a PID is alive without
// actually delivering anything.
var nilSignal = syscall.Signal(0)

// templateLocks guards per-template snapshot build + restore.
var (
	tplLockMu sync.Mutex
	tplLocks  = map[string]*sync.Mutex{}
)

func tplLock(template string) *sync.Mutex {
	tplLockMu.Lock()
	defer tplLockMu.Unlock()
	m := tplLocks[template]
	if m == nil {
		m = &sync.Mutex{}
		tplLocks[template] = m
	}
	return m
}

func templateSnapDir(dataDir, template string) string {
	return filepath.Join(dataDir, "template-snaps", template)
}

func templateSnapReady(dataDir, template, sshKeyFP string) bool {
	dir := templateSnapDir(dataDir, template)
	if _, err := os.Stat(filepath.Join(dir, "ready")); err != nil {
		return false
	}
	// Enforce flavor match. A NATID-mode agent treats a legacy snapshot
	// as "not ready" so it gets rebuilt clean (and vice versa).
	wantNatid := os.Getenv("PANDASTACK_NATID") == "1"
	flavor, _ := os.ReadFile(filepath.Join(dir, "flavor"))
	haveNatid := strings.TrimSpace(string(flavor)) == "natid"
	if wantNatid != haveNatid {
		return false
	}
	// Enforce size match: the template's meta.json is the source of truth
	// for cpu/memory_mb. If the existing snapshot was baked at a different
	// size (or carries no manifest at all — legacy bake), treat it as
	// not-ready so the next create rebakes at the now-correct size.
	want := ReadTemplateSize(dataDir, template)
	haveCPU, haveMem, haveDisk, haveKeyFP, ok := ReadSnapManifest(dir)
	if !ok {
		// Legacy snapshot pre-dating the manifest. It was baked at the
		// Legacy snapshot pre-dating the manifest; treat as baked at defaults.
		// Only honour it if the template still asks for those. Anything else MUST be rebaked.
		haveCPU, haveMem, haveDisk = DefaultTemplateCPU, DefaultTemplateMemoryMB, LegacySnapDiskGB
	}
	if haveCPU != want.CPU || haveMem != want.MemoryMB || haveDisk != want.DiskGB {
		return false
	}
	// Enforce SSH key match: if the agent key has been rotated since the
	// snapshot was baked, the snapshot's authorized_keys won't match the
	// current key and all exec/SSH calls will fail. Force a rebuild.
	// An empty haveKeyFP means a pre-key-check snapshot — always rebuild.
	return haveKeyFP != "" && haveKeyFP == sshKeyFP
}

// ensureTemplateSnapshot builds the snapshot for `template` if missing.
// Returns nil if the snapshot is ready (already or newly built).
// Safe to call concurrently for the same template: only the first call builds.
func (m *Manager) ensureTemplateSnapshot(ctx context.Context, template string) error {
	natidMode := os.Getenv("PANDASTACK_NATID") == "1"
	if templateSnapReady(m.cfg.DataDir, template, m.keys.Fingerprint()) {
		// Validate the existing snapshot matches the current mode.
		// natid bake writes a `flavor` file with "natid"; legacy doesn't.
		flavorPath := filepath.Join(templateSnapDir(m.cfg.DataDir, template), "flavor")
		flavor, _ := os.ReadFile(flavorPath)
		isNatid := strings.TrimSpace(string(flavor)) == "natid"
		if natidMode == isNatid {
			return nil
		}
		m.log.Info("template snapshot flavor mismatch — rebuilding",
			"template", template, "have_natid", isNatid, "want_natid", natidMode)
		_ = os.RemoveAll(templateSnapDir(m.cfg.DataDir, template))
	}
	lk := tplLock(template)
	lk.Lock()
	defer lk.Unlock()
	// Re-check after acquiring lock.
	if templateSnapReady(m.cfg.DataDir, template, m.keys.Fingerprint()) {
		return nil
	}

	t0 := time.Now()
	snapDir := templateSnapDir(m.cfg.DataDir, template)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return fmt.Errorf("mkdir snap dir: %w", err)
	}

	// Allocate a transient network slot for the build VM. We release it
	// at the end — the snapshot's encoded MAC/IP/GW will be overwritten
	// by pandastack-init on every restore.
	buildID := "tplsnap-" + template + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	alloc, err := m.netPool.Allocate(ctx, buildID)
	if err != nil {
		return fmt.Errorf("allocate build network: %w", err)
	}
	defer func() { _ = m.netPool.Release(ctx, buildID) }()

	buildVMDir := filepath.Join(snapDir, "build-vm")
	if err := os.MkdirAll(buildVMDir, 0o755); err != nil {
		return err
	}
	// NOTE: do NOT delete buildVMDir on exit. FC's /snapshot/load opens
	// the rootfs at the encoded absolute path BEFORE the drive PATCH
	// redirects it; the file must continue to exist on disk for the
	// lifetime of the snapshot. The rootfs here is the "baseline" that
	// every restore copies from.

	// Ensure key is baked into the template rootfs and copy it for the
	// build VM. The snapshot will preserve the booted state of this rootfs;
	// we save a parallel copy as rootfs.snap.ext4 (the consistent base for
	// future restores).
	tplRootfs := filepath.Join(m.cfg.DataDir, "templates", template, "rootfs.ext4")
	if !m.keys.IsBakedInto(tplRootfs) {
		if err := m.keys.BakeInto(tplRootfs); err != nil {
			return fmt.Errorf("bake template key: %w", err)
		}
	}
	buildRootfs := filepath.Join(buildVMDir, "rootfs.ext4")
	if err := cloneFile(tplRootfs, buildRootfs); err != nil {
		return fmt.Errorf("copy rootfs to build dir: %w", err)
	}

	kernelPath, err := m.kernelCache.get(m.cfg.DataDir)
	if err != nil {
		return err
	}

	// Critical (legacy mode only): vsock UDS at the STABLE per-template
	// path. The snapshot will encode this absolute path; every restore
	// must be able to bind it (hence the per-template mutex in legacy
	// mode). NATID mode bakes WITHOUT a vsock device — restores reach the
	// guest over TCP via the baked NAT identity alone.
	restoreSock := filepath.Join(snapDir, "restore.sock")
	_ = os.Remove(restoreSock)

	// Template-owned size: bake at whatever the template's meta.json asks
	// for (defaults apply for custom templates without an explicit size).
	// This is the only place a baked snapshot's vcpu/memory_mb is decided —
	// the matching snap-meta.json written at the end of bake (see
	// WriteSnapManifest below) ties the snapshot to that decision so
	// templateSnapReady can invalidate on drift.
	want := ReadTemplateSize(m.cfg.DataDir, template)

	// Grow the build VM's rootfs to want.DiskGB before booting FC. The
	// snapshot taken from this booted VM will encode a kernel that mounted
	// a want.DiskGB ext4 — every subsequent warm-clone (NATID or vsock)
	// inherits that size for free. resize2fs is no-op if the file already
	// matches; failure is logged and bake proceeds with the un-grown image
	// (graceful degradation, same policy as cold-boot path).
	if want.DiskGB > 0 {
		tGrow := time.Now()
		if err := growRootfs(buildRootfs, int64(want.DiskGB)*1024*1024*1024); err != nil {
			m.log.Warn("template build rootfs grow failed (non-fatal)", "err", err, "template", template, "disk_gb", want.DiskGB)
		} else {
			m.log.Info("template build rootfs grown", "template", template, "disk_gb", want.DiskGB, "ms", time.Since(tGrow).Milliseconds())
		}
	}

	spec := firecracker.Spec{
		ID:          buildID,
		KernelPath:  kernelPath,
		RootfsPath:  buildRootfs,
		SocketPath:  filepath.Join("/tmp", fmt.Sprintf("fc-build-%s.socket", template)),
		LogPath:     filepath.Join(snapDir, "build.log"),
		ConsolePath: filepath.Join(snapDir, "build-console.log"),
		CPUs:        want.CPU,
		MemoryMB:    want.MemoryMB,
		Network:     alloc,
	}
	// Phased-boot templates (postgres-16) carry a durable data device. Bake an
	// UNFORMATTED placeholder into the build VM so the snapshot's device
	// topology includes drive "vol1"; at restore we PatchDrive it to the
	// per-database ext4 image. The guest must not mount/format it during bake.
	if phasedBootTemplates[template] {
		placeholder := filepath.Join(buildVMDir, "data-placeholder.img")
		if err := makeSparseImage(placeholder, int64(pgDataPlaceholderGB)*1024*1024*1024); err != nil {
			return fmt.Errorf("create data placeholder: %w", err)
		}
		spec.Volumes = []firecracker.VolumeMount{{
			Name: "pgdata", HostPath: placeholder, ReadOnly: false,
		}}
	}
	if !natidMode {
		spec.Vsock = firecracker.VsockSpec{
			UDSPath: restoreSock,
			CID:     alloc.VsockCID,
		}
		spec.MMDS = firecracker.MMDSSpec{Enabled: true, Address: "169.254.169.254"}
	} else {
		// NATID mode (Phase 2): bake a vsock device at the FIXED, well-known
		// BakedVsockPath. The path is frozen into vm.state; every restore
		// re-creates the listener here. To keep concurrent NATID restores
		// from colliding on this single path, the restore wraps firecracker
		// in a private mount namespace that bind-mounts a per-sandbox dir
		// over BakedVsockDir (see startFromTemplateSnapNATID). At BAKE time
		// there is exactly one build VM, so we bind FC straight to the real
		// BakedVsockPath. The guest's pandastack-daemon listens on vsock
		// port 5252; the host reaches it via this UDS. This is what makes
		// the (Phase-1) vsock fast-path actually reach a daemon on restore.
		if err := os.MkdirAll(firecracker.BakedVsockDir, 0o755); err != nil {
			return fmt.Errorf("mkdir baked vsock dir: %w", err)
		}
		_ = os.Remove(firecracker.BakedVsockPath)
		spec.Vsock = firecracker.VsockSpec{
			UDSPath: firecracker.BakedVsockPath,
			CID:     alloc.VsockCID,
		}
	}
	drv := firecracker.NewDriver(spec, m.log.With("template_build", template))

	if err := drv.Start(context.Background()); err != nil {
		return fmt.Errorf("start build vm: %w", err)
	}
	// Always clean up the build VM.
	defer func() {
		_ = drv.Stop(context.Background())
		_ = os.Remove(filepath.Join("/tmp", fmt.Sprintf("fc-build-%s.socket", template)))
	}()

	// Push identity to the build VM via vsock+MMDS so pandastack-init in the
	// guest can configure eth0 with alloc.GuestIP. Without this the guest
	// has no IP and the SSH probe below will time out (the kernel cmdline
	// has no `ip=` autoconfig and there is no DHCP server on the TAP).
	if !natidMode {
		buildCfg := map[string]string{
			"ip":       alloc.GuestIP + "/30",
			"mac":      alloc.MAC,
			"gateway":  alloc.HostIP,
			"hostname": "build-" + template,
			"dns":      "1.1.1.1",
		}
		if err := drv.PutMMDS(ctx, map[string]any{"identity": buildCfg}); err != nil {
			m.log.Warn("build vm mmds put failed (non-fatal)", "err", err)
		}
		if _, err := deliverConfigRace(restoreSock, 52525, nil, alloc.GuestIP, 22, buildCfg, 25*time.Second); err != nil {
			m.log.Warn("build vm identity push failed; ssh probe may fail", "err", err)
		}
	}

	// Wait for guest SSH to come up — this proves the kernel + systemd
	// + sshd are all healthy. Use a generous deadline (cold boots can
	// take 5-10s on a Mac/Lima setup).
	gc := guest.NewClient(alloc.GuestIP, "root", m.keys.Signer())
	defer gc.Close()
	sshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := gc.WaitReady(sshCtx, 30*time.Second); err != nil {
		return fmt.Errorf("build vm ssh: %w", err)
	}

	// In NATID mode the snapshot has no vsock device and the guest is
	// reachable via TCP through the baked NAT identity — no pandastack-init
	// or MMDS handshake needed.
	if !natidMode {
		// Wait for pandastack-init to be in accept() — probe the vsock with a
		// CONNECT-then-close; on success we know the listener is up. The
		// snapshot must capture pandastack-init mid-accept so restored guests
		// can be reconfigured immediately.
		if err := waitVsockReady(restoreSock, 52525, 10*time.Second); err != nil {
			return fmt.Errorf("pandastack-init not listening on vsock: %w", err)
		}

		// Set up MMDS in the build VM: add route to 169.254.169.254 via eth0
		// inside the guest (required for the metadata HTTP service to be
		// reachable). Then push a sentinel value so MMDS storage is
		// initialized; on restore we'll overwrite with per-sandbox identity.
		if _, err := gc.Exec(ctx, "ip route add 169.254.169.254/32 dev eth0 || true"); err != nil {
			m.log.Warn("mmds route add failed (non-fatal)", "err", err)
		}
		if err := drv.PutMMDS(ctx, map[string]any{"identity": map[string]string{"placeholder": "1"}}); err != nil {
			m.log.Warn("mmds put placeholder failed (non-fatal)", "err", err)
		}
	}

	// For phased-boot templates (e.g. postgres-16): wait for
	// /run/pandastack/snapshot-ready, written by autostart.sh once the OS is
	// booted (Phase 1). In the durable-volume model postgres is STOPPED at
	// this point and the placeholder data device is untouched, so the
	// snapshot captures a clean booted OS ready to mount a per-database
	// volume and start postgres on restore (Phase 2).
	waitSnapshotReady(ctx, gc, template, 120*time.Second)
	m.log.Info("template snapshot point reached", "template", template)

	// NATID mode (Phase 2): before snapshotting, confirm the guest
	// pandastack-daemon is listening on vsock so the snapshot captures it
	// mid-accept(). The probe CONNECTs to BakedVsockPath at the daemon port
	// and closes; the daemon tolerates the EOF and loops back to accept.
	// Non-fatal: if the daemon isn't up the bake still proceeds (the
	// host fast-path falls back to SSH at runtime), but we log loudly so a
	// missing daemon is visible.
	if natidMode {
		if err := waitVsockReady(firecracker.BakedVsockPath, vsockwire.DaemonPort, 10*time.Second); err != nil {
			m.log.Warn("pandastack-daemon not listening on vsock at bake; restores will fall back to SSH",
				"template", template, "err", err)
		} else {
			m.log.Info("pandastack-daemon vsock ready at bake", "template", template)
		}
	}

	// PauseAndSnapshot pauses the VM and snapshots; we then Stop() in the
	// deferred cleanup. After Stop, the rootfs file is in a consistent
	// post-shutdown state and serves as the baseline for all future restores.
	if err := drv.PauseAndSnapshot(context.Background(), snapDir); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	// Emit the chunked memstream header for vm.mem. This zero-eliding index
	// is what lets a cold host stream the memory file on demand (UFFD) from
	// GCS instead of downloading the whole thing before boot. It is purely
	// additive metadata: the full-download restore path ignores it, so a
	// failure here is non-fatal (logged, bake proceeds without streaming).
	memPath := filepath.Join(snapDir, "vm.mem")
	if h, herr := memstream.BuildHeader(memPath, memstream.DefaultChunkSize); herr != nil {
		m.log.Warn("memstream header build failed (non-fatal; streaming disabled for this bake)",
			"template", template, "err", herr)
	} else if werr := h.WriteFile(memPath + ".header"); werr != nil {
		m.log.Warn("memstream header write failed (non-fatal)", "template", template, "err", werr)
	} else {
		m.log.Info("memstream header emitted",
			"template", template,
			"total_chunks", h.NumChunks(),
			"present_chunks", h.PresentChunks(),
			"chunk_bytes", memstream.DefaultChunkSize,
		)
	}

	// Create a *separate* sparsified clone for per-VM cloning. We MUST
	// NOT punch holes in `buildRootfs` itself: FC's /snapshot/load opens
	// that exact path and reads it lazily as memory pages are paged in;
	// changing the on-disk byte pattern under FC would corrupt the
	// restored guest. So we copy buildRootfs → clone.ext4, then
	// fallocate -d the clone; per-VM rootfs comes from the clone.
	cloneRootfs := filepath.Join(snapDir, "clone.ext4")
	if err := cloneFile(buildRootfs, cloneRootfs); err != nil {
		m.log.Warn("snapshot clone copy failed; falling back to full copy on restore", "err", err)
	} else {
		if out, err := exec.Command("fallocate", "-d", cloneRootfs).CombinedOutput(); err != nil {
			m.log.Warn("fallocate -d clone failed (non-fatal)", "err", err, "out", string(out))
		}
	}

	// Persist the baked identity so NATID restores can re-create matching
	// in-netns TAP host IPs without re-discovery.
	idBlob, _ := json.Marshal(map[string]string{
		"tap_host_ip": alloc.HostIP,
		"guest_ip":    alloc.GuestIP,
		"mac":         alloc.MAC,
		"subnet":      alloc.Subnet,
	})
	_ = os.WriteFile(filepath.Join(snapDir, "identity.json"), idBlob, 0o644)

	// Persist flavor marker so we can detect mode mismatches on restart.
	flavor := "legacy"
	if natidMode {
		flavor = "natid"
	}
	if err := os.WriteFile(filepath.Join(snapDir, "flavor"), []byte(flavor+"\n"), 0o644); err != nil {
		return fmt.Errorf("write flavor marker: %w", err)
	}

	// Mark ready (atomic-ish — last step). Also persist the size manifest
	// so a future config drift (operator changes template meta) invalidates
	// this snapshot in templateSnapReady.
	if err := WriteSnapManifest(snapDir, want.CPU, want.MemoryMB, want.DiskGB, m.keys.Fingerprint()); err != nil {
		return fmt.Errorf("write snap manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "ready"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write ready marker: %w", err)
	}
	m.log.Info("template snapshot built",
		"template", template,
		"snap_dir", snapDir,
		"elapsed_ms", time.Since(t0).Milliseconds(),
	)
	// Attach the dm-snapshot base for this freshly-baked template so the very
	// next create takes the ~2ms CoW device path instead of the ~100ms
	// cloneFile fallback. InitBase is idempotent. Without this, any template
	// baked at runtime (e.g. the first create of a template on a fresh agent)
	// is stuck on the slow copy path until the agent restarts and the startup
	// sweep attaches the base — the dominant cold-start cost we measured.
	if m.dmsnap.Enabled() {
		if base := dmsnapBaseRootfs(m.cfg.DataDir, template); base != "" {
			if err := m.dmsnap.InitBase(template, base); err != nil {
				m.log.Warn("dmsnap InitBase after bake failed (non-fatal)",
					"template", template, "err", err)
			}
		}
	}
	// Seed this freshly-baked snapshot to GCS so other agents in the fleet
	// (and future spot/standard replacements) can restore it without a cold
	// bake. Fire-and-forget: never block create on the upload. Guarded so we
	// only publish when the agent is using the fleet-wide SHARED ssh key —
	// a seed baked with a per-agent key is useless to the rest of the fleet
	// and could shadow a good generation — AND only for PUBLIC templates:
	// a user-built (owned) template's snapshot may contain private state, so
	// it must never land in the fleet-shared seed bucket. Owned templates are
	// baked locally (fast first create on the building agent) but stay local.
	if m.seedStore.Enabled() && seed.SharedKeyActive() && IsPublicTemplate(m.cfg.DataDir, template) {
		up := seed.UploadParams{
			DataDir:  m.cfg.DataDir,
			Template: template,
			SnapDir:  snapDir,
			CPU:      want.CPU,
			MemoryMB: want.MemoryMB,
			DiskGB:   want.DiskGB,
			SSHKeyFP: m.keys.Fingerprint(),
			Flavor:   flavor,
			AgentID:  m.agentID,
		}
		// Phased-boot templates bake a data-device placeholder into the
		// snapshot (build-vm/data-placeholder.img). Record its size so the
		// restoring agent can recreate it — build-vm/ is stripped from the
		// seed tarball, and without the file /snapshot/load fails.
		if phasedBootTemplates[template] {
			up.DataPlaceholderGB = pgDataPlaceholderGB
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if m.seedStore.AlreadyPublished(ctx, up) {
				return
			}
			if err := m.seedStore.Upload(ctx, up); err != nil {
				m.log.Warn("seed upload failed (non-fatal)", "template", template, "err", err)
			} else {
				m.log.Info("seed uploaded to GCS", "template", template)
			}
		}()
	}
	// Page-cache prime the freshly-built snapshot so the first cold restore
	// (likely seconds away) doesn't pay disk-read latency.
	if m.prefetcher != nil {
		go m.prefetcher.PrefetchTemplate(ctx, template)
	}
	return nil
}

// listReadyTemplates returns names of all templates that have a `ready` marker
// under <dataDir>/template-snaps/. Used at boot to pre-warm the page cache.
func listReadyTemplates(dataDir string) ([]string, error) {
	root := filepath.Join(dataDir, "template-snaps")
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "ready")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// listLocalTemplates enumerates every template installed locally (i.e. with a
// <dataDir>/templates/<name>/rootfs.ext4), baked or not.
func listLocalTemplates(dataDir string) ([]string, error) {
	root := filepath.Join(dataDir, "templates")
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "rootfs.ext4")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// BakeTemplateSnapshot synchronously ensures the per-template Firecracker
// snapshot exists so the NEXT create restores in ~150ms instead of cold
// booting. No-op if already present (idempotent under tplLock). Public
// templates additionally seed to GCS; owned (user-built) templates stay local.
// Used by the template-build path to make a freshly-built template's FIRST
// create fast.
func (m *Manager) BakeTemplateSnapshot(ctx context.Context, template string) error {
	return m.ensureTemplateSnapshot(ctx, template)
}

// preseedPublicTemplates runs once shortly after boot. For every PUBLIC,
// non-phased template lacking a ready snapshot, it ensures one exists —
// preferring to PULL a published seed from GCS over a cold bake. This makes
// every first-party template fast "from second zero" rather than lazily on the
// first customer create. Sequential + jittered so it never competes with live
// traffic, and seed-first so fresh agents don't stampede the fleet by all
// cold-baking the same template. Best-effort; every failure is non-fatal.
// Disable with PANDASTACK_PRESEED_TEMPLATES=0.
func (m *Manager) preseedPublicTemplates() {
	if os.Getenv("PANDASTACK_PRESEED_TEMPLATES") == "0" {
		return
	}
	// Grace period so any creates racing in at boot take priority over
	// background bakes.
	time.Sleep(preseedStartDelay())

	tmpls, err := listLocalTemplates(m.cfg.DataDir)
	if err != nil {
		m.log.Warn("preseed: enumerate templates failed (non-fatal)", "err", err)
		return
	}
	flavor := "legacy"
	if os.Getenv("PANDASTACK_NATID") == "1" {
		flavor = "natid"
	}
	fp := m.keys.Fingerprint()
	for _, t := range tmpls {
		if phasedBootTemplates[t] {
			continue // durable DBs: created on demand, special phased bake
		}
		if !IsPublicTemplate(m.cfg.DataDir, t) {
			continue // owned templates are baked at build time and stay local
		}
		if templateSnapReady(m.cfg.DataDir, t, fp) {
			continue // already fast
		}
		// Prefer a published seed (no VM boot, no fleet stampede) when one
		// exists but this agent missed it at boot-time seed-sync.
		if m.seedStore.Enabled() && m.seedStore.SeedPublished(context.Background(), t) {
			sctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			serr := m.seedStore.SyncTemplate(sctx, m.cfg.DataDir, t, fp, flavor, m.log)
			cancel()
			if serr == nil && templateSnapReady(m.cfg.DataDir, t, fp) {
				if m.dmsnap != nil && m.dmsnap.Enabled() {
					if rp := dmsnapBaseRootfs(m.cfg.DataDir, t); rp != "" {
						_ = m.dmsnap.InitBase(t, rp)
					}
				}
				m.log.Info("preseed: pulled published seed", "template", t)
				continue
			}
		}
		// No published seed (or pull failed): cold-bake once. ensureTemplateSnapshot
		// publishes the seed for the rest of the fleet (public templates only).
		bctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		if err := m.ensureTemplateSnapshot(bctx, t); err != nil {
			m.log.Warn("preseed: bake failed (non-fatal)", "template", t, "err", err)
		} else {
			m.log.Info("preseed: baked snapshot", "template", t)
		}
		cancel()
		time.Sleep(preseedJitter())
	}
	m.log.Info("preseed: pass complete", "templates", len(tmpls))
}

// preseedStartDelay is the grace period before the background pre-seed pass
// begins (override with PANDASTACK_PRESEED_DELAY_SECONDS).
func preseedStartDelay() time.Duration {
	if v := os.Getenv("PANDASTACK_PRESEED_DELAY_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 30 * time.Second
}

// preseedJitter spreads cold bakes across the fleet so simultaneous fresh
// agents don't all bake the same un-seeded template at the same instant.
func preseedJitter() time.Duration {
	return time.Duration(2000+mrand.Intn(8000)) * time.Millisecond
}

// waitVsockReady opens the FC vsock UDS, sends CONNECT <port>\n, waits for
// an OK reply, and immediately closes. Retries until deadline. On success,
// guest-side pandastack-init's accept() has returned + read EOF + looped back
// to a fresh accept (loop-on-probe is implemented in pandastack-init).
func waitVsockReady(sockPath string, port uint32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", sockPath, 1*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(150 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(1 * time.Second))
		if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
			c.Close()
			lastErr = err
			time.Sleep(150 * time.Millisecond)
			continue
		}
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		c.Close()
		if err != nil || n < 2 || string(buf[:2]) != "OK" {
			lastErr = fmt.Errorf("bad ok line: %q err=%v", string(buf[:n]), err)
			time.Sleep(150 * time.Millisecond)
			continue
		}
		return nil
	}
	return fmt.Errorf("vsock not ready after %s: %v", timeout, lastErr)
}

// startFromTemplateSnap performs the sub-second restore:
//  1. Acquire per-template lock (held for VM lifetime due to vsock UDS).
//  2. Copy template rootfs.snap.ext4 to the child's vmDir/rootfs.ext4.
//  3. Spawn FC with /snapshot/load + network_overrides + drive override.
//  4. Dial the (encoded) restore vsock UDS, send identity JSON, await '1' ack.
//  5. /vm Resume — guest comes back with its NEW identity.
//
// Returns the started driver and the duration spent inside this function
// (excluding the initial network allocation, which the caller did).
func (m *Manager) startFromTemplateSnap(
	ctx context.Context,
	template, id, vmDir string,
	alloc network.Allocation,
	cpu, memMB int,
) (*firecracker.Driver, map[string]int64, error) {
	phases := map[string]int64{}
	mark := func(name string, t time.Time) { phases[name] = time.Since(t).Milliseconds() }

	snapDir := templateSnapDir(m.cfg.DataDir, template)
	restoreSock := filepath.Join(snapDir, "restore.sock")

	tLock := time.Now()
	lk := tplLock(template)
	lk.Lock()
	mark("tpl_lock_wait_ms", tLock)
	releaseLock := lk.Unlock
	// NOTE: lock release is deferred to the driver's Stop() through the
	// closure stored on the spec. For now we release after a confirmed
	// FC bind would fail anyway; we hand off responsibility to the
	// manager.Delete path via a goroutine that watches the FC process.
	heldLock := true
	defer func() {
		if heldLock {
			releaseLock()
		}
	}()

	// Stale socket cleanup — FC will fail if file exists.
	_ = os.Remove(restoreSock)

	// Per-sandbox rootfs from the sparsified clone (if available) else
	// from the snapshot baseline. `cp --sparse=always` preserves holes
	// from the source — the clone is ~60% smaller, halving copy time.
	// Pre-warm the page cache for clone.ext4 so the snapshot load and
	// initial post-Resume rootfs reads don't dominate cold-cache restores.
	// Without this, first-restore-after-eviction can blow past the wake
	// timeout (observed 35s outlier in the 10-restore benchmark, vs ~1.7s
	// warm). posix_fadvise(WILLNEED) is non-blocking — kernel schedules
	// async readahead. We also kick off a parallel read on the snapshot's
	// vm.mem so FC's mmap-page-in is hot too.
	cloneRootfs := filepath.Join(snapDir, "clone.ext4")
	go warmFile(cloneRootfs)
	go warmFile(filepath.Join(snapDir, "vm.mem"))

	// Copy the per-sandbox rootfs CONCURRENTLY with FC spawn + /snapshot/load
	// (the same overlap the NATID path uses): the copy is 50-400ms and the
	// load is 80-120ms, and neither depends on the other. The drive is
	// patched onto the paused VM after both finish, before Resume.
	tCopy := time.Now()
	childRootfs := filepath.Join(vmDir, "rootfs.ext4")
	srcRootfs := cloneRootfs
	if _, err := os.Stat(cloneRootfs); err != nil {
		srcRootfs = filepath.Join(snapDir, "build-vm", "rootfs.ext4")
	}
	rootfsCh := make(chan error, 1)
	go func() { rootfsCh <- cloneFile(srcRootfs, childRootfs) }()

	kernelPath, err := m.kernelCache.get(m.cfg.DataDir)
	if err != nil {
		<-rootfsCh // join the copy so the caller can safely reap vmDir
		return nil, phases, err
	}

	drv := firecracker.NewDriver(firecracker.Spec{
		ID:          id,
		KernelPath:  kernelPath,
		RootfsPath:  childRootfs,
		SocketPath:  filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id)),
		LogPath:     filepath.Join(vmDir, "firecracker.log"),
		ConsolePath: filepath.Join(vmDir, "console.log"),
		CPUs:        cpu,
		MemoryMB:    memMB,
		Network:     alloc,
		Vsock: firecracker.VsockSpec{
			UDSPath: restoreSock,
			CID:     alloc.VsockCID,
		},
		MMDS: firecracker.MMDSSpec{Enabled: true, Address: "169.254.169.254"},
	}, m.log.With("sandbox", id))

	tStart := time.Now()
	if err := drv.StartFromSnapNoDrive(context.Background(), snapDir, alloc.TAP); err != nil {
		// A failed snapshot load (e.g. a missing baked backing file) can leave
		// the firecracker process alive holding the unix socket + netns. Stop
		// it so the caller's cold-boot fallback isn't blocked by a stale
		// /tmp/fc-<id>.socket and we don't orphan a VMM/tap.
		<-rootfsCh
		_ = drv.Stop(context.Background())
		return nil, phases, fmt.Errorf("start from snap: %w", err)
	}
	mark("fc_load_snap_ms", tStart)

	// Barrier: the rootfs copy must be done before we point the paused VM's
	// drive at it. rootfs_copy_ms records copy-start→join, i.e. only the
	// portion that was NOT hidden behind the snapshot load shows up in the
	// create's wallclock beyond fc_load_snap_ms.
	if err := <-rootfsCh; err != nil {
		_ = drv.Stop(context.Background())
		return nil, phases, fmt.Errorf("clone snapshot rootfs: %w", err)
	}
	mark("rootfs_copy_ms", tCopy)
	if err := drv.PatchRootfs(context.Background(), childRootfs); err != nil {
		_ = drv.Stop(context.Background())
		return nil, phases, fmt.Errorf("patch rootfs: %w", err)
	}

	// Push per-sandbox identity into MMDS BEFORE Resume. This is the
	// patent-grade zero-config delivery primitive: MMDS state survives
	// snapshot/restore, FC permits PUT /mmds while paused, and the guest
	// reads it via virtio-net HTTP — no vsock state recovery required.
	cfg := map[string]string{
		"ip":       alloc.GuestIP + "/30",
		"mac":      alloc.MAC,
		"gateway":  alloc.HostIP,
		"hostname": "sb-" + shortID(id),
		"dns":      "1.1.1.1",
	}
	tMmds := time.Now()
	if err := drv.PutMMDS(context.Background(), map[string]any{"identity": cfg}); err != nil {
		m.log.Warn("mmds put identity failed; relying on vsock paths", "err", err)
	}
	mark("mmds_put_ms", tMmds)

	// Wake-on-dial fast path: open a UDS listener at the encoded path
	// FC uses for guest→host vsock forwarding (`<base>_<port>`). pandastack-init
	// inside the guest spins on an outbound dial to (CID=2, 52526) the
	// instant its vsock subsystem is alive post-Resume — the dial succeeds
	// → this accept() returns → we deliver JSON instantly, no host poll.
	wakePort := uint32(52526)
	wakeUDS := fmt.Sprintf("%s_%d", restoreSock, wakePort)
	_ = os.Remove(wakeUDS)
	wakeLn, wakeErr := net.Listen("unix", wakeUDS)
	if wakeErr != nil {
		m.log.Warn("wake-on-dial listen failed; relying on poll path", "err", wakeErr)
	}

	// Resume the VM FIRST. The guest must be running for pandastack-init's
	// vsock accept() to be live (FC can't forward vsock traffic to a
	// paused guest). The guest comes back with stale network identity
	// (the template's MAC/IP), which pandastack-init will fix below before
	// any user traffic.
	tResume := time.Now()
	if err := drv.Resume(context.Background()); err != nil {
		if wakeLn != nil {
			wakeLn.Close()
		}
		_ = drv.Stop(context.Background())
		return nil, phases, fmt.Errorf("resume: %w", err)
	}
	mark("vm_resume_ms", tResume)

	// Reconfigure the guest network identity — race three host-side paths:
	//   (1) vsock-listen poll       — original CONNECT-based protocol.
	//   (2) vsock-dial wake-on-dial — guest dials us back post-Resume.
	//   (3) guest IP TCP probe      — if MMDS path won in pandastack-init,
	//       the guest is reachable at alloc.GuestIP. Vsock can be totally
	//       broken (post-restore EOF mode) and we still succeed.
	tReconf := time.Now()
	via, err := deliverConfigRace(restoreSock, 52525, wakeLn, alloc.GuestIP, 22, cfg, 60*time.Second)
	if wakeLn != nil {
		wakeLn.Close()
		_ = os.Remove(wakeUDS)
	}
	if err != nil {
		// Preserve diagnostics: copy FC + console log to a sidecar so the
		// cold-boot fallback doesn't overwrite them.
		diagDir := filepath.Join(snapDir, "last-failed-restore")
		_ = os.MkdirAll(diagDir, 0o755)
		_ = copyFile(filepath.Join(vmDir, "firecracker.log"), filepath.Join(diagDir, "firecracker.log"))
		_ = copyFile(filepath.Join(vmDir, "console.log"), filepath.Join(diagDir, "console.log"))
		_ = drv.Stop(context.Background())
		return nil, phases, fmt.Errorf("vsock reconfig: %w", err)
	}
	mark("vsock_reconfig_ms", tReconf)
	phases["vsock_via_dial"] = 0
	if via == "dial" {
		phases["vsock_via_dial"] = 1
	}

	// Lock stays held for VM lifetime; release in a watcher goroutine
	// that exits when the driver's PID is gone.
	heldLock = false
	go watchAndReleaseLock(drv, releaseLock)

	return drv, phases, nil
}

func watchAndReleaseLock(drv *firecracker.Driver, release func()) {
	defer release()
	for {
		pid := drv.PID()
		if pid <= 0 {
			return
		}
		p, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		if err := p.Signal(nilSignal); err != nil {
			return
		}
		// 50ms poll cadence: when the previous restore tears down (DELETE),
		// the next restore is gated on us releasing the per-template lock.
		// At 500ms cadence this added ~370ms to every back-to-back restore
		// (waiting for the watcher to notice the PID was gone).
		time.Sleep(50 * time.Millisecond)
	}
}

// deliverConfigRace augments deliverVsockConfig with a third success criterion:
// TCP-reachability of the (newly identified) guest IP. The MMDS path of
// pandastack-init applies the network identity inside the guest using only
// virtio-net + HTTP — vsock can be totally broken (post-restore EOF mode)
// and the guest still ends up correctly configured. We just need a way
// to detect that. A successful TCP connect to guest:port proves identity
// has been applied AND the guest is reachable. Returns the channel that
// won ("poll", "dial", or "ipprobe").
func deliverConfigRace(sockPath string, port uint32, wakeLn net.Listener, guestIP string, guestPort int, cfg map[string]string, timeout time.Duration) (string, error) {
	body, _ := json.Marshal(cfg)
	body = append(body, '\n')

	type result struct {
		via string
		err error
	}
	results := make(chan result, 3)

	if wakeLn != nil {
		_ = wakeLn.(*net.UnixListener).SetDeadline(time.Now().Add(timeout))
		go func() {
			c, err := wakeLn.Accept()
			if err != nil {
				results <- result{via: "dial", err: fmt.Errorf("wake accept: %w", err)}
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Write(body); err != nil {
				results <- result{via: "dial", err: fmt.Errorf("wake write: %w", err)}
				return
			}
			resp, err := readLine(c)
			if err != nil {
				results <- result{via: "dial", err: fmt.Errorf("wake ack: %w", err)}
				return
			}
			if len(resp) == 0 || resp[0] != '1' {
				results <- result{via: "dial", err: fmt.Errorf("wake reject: %q", resp)}
				return
			}
			results <- result{via: "dial"}
		}()
	}

	go func() {
		deadline := time.Now().Add(timeout)
		var lastErr error
		attempts := 0
		// Exponential backoff 5ms → 80ms: right after Resume the guest's
		// pandastack-init is usually already in accept(), so the first
		// couple of attempts land within ~10ms instead of an 80ms tick.
		backoff := 5 * time.Millisecond
		for time.Now().Before(deadline) {
			attempts++
			err := sendVsockConfigOnce(sockPath, port, cfg, 1500*time.Millisecond)
			if err == nil {
				results <- result{via: "poll"}
				return
			}
			lastErr = err
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 80*time.Millisecond {
				backoff = 80 * time.Millisecond
			}
		}
		results <- result{via: "poll", err: fmt.Errorf("after %d attempts: %w", attempts, lastErr)}
	}()

	go func() {
		deadline := time.Now().Add(timeout)
		var lastErr error
		attempts := 0
		addr := fmt.Sprintf("%s:%d", guestIP, guestPort)
		// 5ms cadence (was 50ms): a failed dial inside the host is a cheap
		// immediate RST/route error, and this probe gates readiness when
		// the MMDS path applied identity before vsock came up.
		for time.Now().Before(deadline) {
			attempts++
			c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
			if err == nil {
				_ = c.Close()
				results <- result{via: "ipprobe"}
				return
			}
			lastErr = err
			time.Sleep(5 * time.Millisecond)
		}
		results <- result{via: "ipprobe", err: fmt.Errorf("after %d attempts: %w", attempts, lastErr)}
	}()

	// First success wins. Collect all errors if every channel fails.
	var errs []string
	for i := 0; i < 3; i++ {
		r := <-results
		if r.err == nil {
			return r.via, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", r.via, r.err))
		// Only require 2 results if wakeLn was nil (then the dial goroutine wasn't started).
		if wakeLn == nil && i == 1 {
			break
		}
	}
	return "", fmt.Errorf("%s", strings.Join(errs, "; "))
}

// deliverVsockConfig races two delivery channels: (1) wake-on-dial — accept()
// on a UDS that FC routes guest→host vsock connections to; the moment pandastack-init
// can dial, we win instantly. (2) Host-poll CONNECT — the original protocol,
// kept as a safety net if FC's outbound vsock forwarding isn't set up in time.
// Returns which path delivered ("dial" or "poll").
func deliverVsockConfig(sockPath string, port uint32, wakeLn net.Listener, cfg map[string]string, timeout time.Duration) (string, error) {
	body, _ := json.Marshal(cfg)
	body = append(body, '\n')

	type result struct {
		via string
		err error
	}
	results := make(chan result, 2)

	// Path 1: wake-on-dial — accept() the guest's outbound connection.
	if wakeLn != nil {
		_ = wakeLn.(*net.UnixListener).SetDeadline(time.Now().Add(timeout))
		go func() {
			c, err := wakeLn.Accept()
			if err != nil {
				results <- result{via: "dial", err: fmt.Errorf("wake accept: %w", err)}
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Write(body); err != nil {
				results <- result{via: "dial", err: fmt.Errorf("wake write: %w", err)}
				return
			}
			resp, err := readLine(c)
			if err != nil {
				results <- result{via: "dial", err: fmt.Errorf("wake ack: %w", err)}
				return
			}
			if len(resp) == 0 || resp[0] != '1' {
				results <- result{via: "dial", err: fmt.Errorf("wake reject: %q", resp)}
				return
			}
			results <- result{via: "dial"}
		}()
	}

	// Path 2: host-poll CONNECT — original behavior.
	go func() {
		deadline := time.Now().Add(timeout)
		var lastErr error
		attempts := 0
		for time.Now().Before(deadline) {
			attempts++
			err := sendVsockConfigOnce(sockPath, port, cfg, 1500*time.Millisecond)
			if err == nil {
				results <- result{via: "poll"}
				return
			}
			lastErr = err
			time.Sleep(20 * time.Millisecond)
		}
		results <- result{via: "poll", err: fmt.Errorf("after %d attempts: %w", attempts, lastErr)}
	}()

	// First success wins. If both fail, return the more informative error.
	first := <-results
	if first.err == nil {
		return first.via, nil
	}
	second := <-results
	if second.err == nil {
		return second.via, nil
	}
	return "", fmt.Errorf("%s: %v; %s: %v", first.via, first.err, second.via, second.err)
}

// sendVsockConfig kept for backward compat / tests.
func sendVsockConfig(sockPath string, port uint32, cfg map[string]string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		err := sendVsockConfigOnce(sockPath, port, cfg, 1500*time.Millisecond)
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(80 * time.Millisecond)
	}
	return fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

func sendVsockConfigOnce(sockPath string, port uint32, cfg map[string]string, perAttemptTimeout time.Duration) error {
	c, err := net.DialTimeout("unix", sockPath, perAttemptTimeout)
	if err != nil {
		return fmt.Errorf("dial uds: %w", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(perAttemptTimeout))

	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		return fmt.Errorf("write CONNECT: %w", err)
	}
	if _, err := readLine(c); err != nil {
		return fmt.Errorf("read OK: %w", err)
	}
	body, _ := json.Marshal(cfg)
	body = append(body, '\n')
	if _, err := c.Write(body); err != nil {
		return fmt.Errorf("write cfg: %w", err)
	}
	resp, err := readLine(c)
	if err != nil {
		return fmt.Errorf("read ack: %w", err)
	}
	if len(resp) == 0 || resp[0] != '1' {
		return fmt.Errorf("guest rejected config: %q", resp)
	}
	return nil
}

func readLine(c net.Conn) ([]byte, error) {
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return line, nil
			}
			line = append(line, buf[0])
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return line, nil
			}
			return line, err
		}
	}
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// warmFile asks the kernel to readahead the entire file into the page cache.
// Non-blocking — kernel schedules async I/O. Best-effort: errors silently
// ignored. Used to defeat cold-cache outliers on first restore after eviction.
func warmFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	// POSIX_FADV_WILLNEED = 3. Kernel schedules readahead asynchronously.
	_, _, _ = syscallFadvise(f.Fd(), 0, st.Size(), 3)
}

// loadTemplateIdentity reads the baked guest identity persisted by
// ensureTemplateSnapshot (identity.json next to ready).
func loadTemplateIdentity(snapDir string) (tapHostIP, guestIP, mac string, err error) {
b, err := os.ReadFile(filepath.Join(snapDir, "identity.json"))
if err != nil {
return "", "", "", err
}
var id struct {
TapHostIP string `json:"tap_host_ip"`
GuestIP   string `json:"guest_ip"`
MAC       string `json:"mac"`
}
if err := json.Unmarshal(b, &id); err != nil {
return "", "", "", err
}
if id.TapHostIP == "" || id.GuestIP == "" {
return "", "", "", fmt.Errorf("identity.json missing fields")
}
return id.TapHostIP, id.GuestIP, id.MAC, nil
}

// startFromTemplateSnapNATID is the NAT-identity restore path: each sandbox
// runs FC inside its own netns where the baked guest IP/MAC just work.
// No vsock reconfig, no MMDS, no per-template lock — the snapshot is
// stateless w.r.t. host artifacts after netns_overrides.
//
// alloc is the NATIDAlloc from m.netPool.AllocateNATID (already created
// the netns + veth + tap + iptables).
func (m *Manager) startFromTemplateSnapNATID(
ctx context.Context,
template, id, vmDir string,
alloc network.NATIDAlloc,
cpu, memMB int,
dataDrivePath string,
) (*firecracker.Driver, map[string]int64, error) {
phases := map[string]int64{}
mark := func(name string, t time.Time) { phases[name] = time.Since(t).Milliseconds() }

snapDir := templateSnapDir(m.cfg.DataDir, template)

// Remove any stale legacy baked socket defensively (harmless if absent).
_ = os.Remove(filepath.Join(snapDir, "restore.sock"))

// Phase-2 vsock: NATID snapshots now bake a vsock device at the FIXED
// firecracker.BakedVsockPath. FC re-creates that listener on restore at the
// same absolute path. To stop concurrent restores colliding on the single
// baked path, we hand StartFromSnapNoDrive a per-sandbox bind dir; it spawns
// FC in a private mount namespace and bind-mounts vsockBindDir over
// BakedVsockDir, so each FC's socket lands on its own inode here. The host
// reaches the guest daemon via vsockBindDir/<BakedVsockName>.
vsockBindDir := filepath.Join(vmDir, "vsock")
if err := os.MkdirAll(vsockBindDir, 0o755); err != nil {
return nil, phases, fmt.Errorf("mkdir vsock bind dir: %w", err)
}

// Prepare the per-sandbox rootfs in parallel with FC spawn + snapshot load.
// Preference order:
//   1. reflink (FICLONE)  — pure CoW on XFS/btrfs, ~2-25ms, fastest on prod.
//   2. dm-snapshot        — CoW block device, ~80ms (losetup+dmsetup+udev),
//                           used on non-reflink filesystems (e.g. Lima ext4).
//   3. cloneFile copy     — last resort, scales with image size.
cloneRootfs := filepath.Join(snapDir, "clone.ext4")
go warmFile(filepath.Join(snapDir, "vm.mem")) // warm vm.mem into page cache

type rootfsResult struct {
	path string
	mode string // "reflink" | "dmsnap" | "clonefile"
	err  error
}
tRootfs := time.Now()
rootfsCh := make(chan rootfsResult, 1)
go func() {
	srcRootfs := cloneRootfs
	if _, statErr := os.Stat(cloneRootfs); statErr != nil {
		srcRootfs = filepath.Join(snapDir, "build-vm", "rootfs.ext4")
	}
	childRootfs := filepath.Join(vmDir, "rootfs.ext4")

	// 1. Fastest path: pure reflink (no copy fallback). On reflink-capable
	//    filesystems this is a metadata-only CoW and beats dm-snapshot.
	if err := tryReflink(srcRootfs, childRootfs); err == nil {
		rootfsCh <- rootfsResult{path: childRootfs, mode: "reflink"}
		return
	} else {
		m.log.Warn("reflink failed, falling back to dmsnap/copy",
			"sandbox", id, "template", template, "src", srcRootfs, "err", err)
	}

	// 2. dm-snapshot CoW (non-reflink filesystems).
	if m.dmsnap.Enabled() {
		dmDev, derr := m.dmsnap.CreateSnap(template, id, vmDir)
		if derr == nil {
			rootfsCh <- rootfsResult{path: dmDev, mode: "dmsnap"}
			return
		}
		m.log.Warn("dmsnap CreateSnap failed, falling back to cloneFile",
			"sandbox", id, "template", template, "err", derr)
	}

	// 3. Last resort: full copy (FICLONE → copy_file_range → io.Copy).
	go warmFile(srcRootfs)
	if err := cloneFile(srcRootfs, childRootfs); err != nil {
		rootfsCh <- rootfsResult{err: fmt.Errorf("clone rootfs: %w", err)}
		return
	}
	rootfsCh <- rootfsResult{path: childRootfs, mode: "clonefile"}
}()

kernelPath, err := m.kernelCache.get(m.cfg.DataDir)
if err != nil {
return nil, phases, err
}

drv := firecracker.NewDriver(firecracker.Spec{
ID:          id,
KernelPath:  kernelPath,
RootfsPath:  "", // filled in after rootfsCh below
SocketPath:  filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id)),
LogPath:     filepath.Join(vmDir, "firecracker.log"),
ConsolePath: filepath.Join(vmDir, "console.log"),
CPUs:        cpu,
MemoryMB:    memMB,
Netns:       alloc.Netns,
VsockBindDir: vsockBindDir,
}, m.log.With("sandbox", id, "natid", true))

tStart := time.Now()
if err := drv.StartFromSnapNoDrive(context.Background(), snapDir, alloc.TapName); err != nil {
// A failed snapshot load (e.g. a missing baked backing file like the
// phased-boot data placeholder) can leave the firecracker process alive
// holding the unix socket + netns. Stop it so the caller's cold-boot
// fallback isn't blocked by a stale /tmp/fc-<id>.socket and we don't
// orphan a VMM/tap.
_ = drv.Stop(context.Background())
return nil, phases, fmt.Errorf("start from snap: %w", err)
}
mark("fc_load_snap_ms", tStart)

// Wait for rootfs setup (dm-snapshot: ~2ms; cloneFile: 50-400ms).
rr := <-rootfsCh
if rr.err != nil {
_ = drv.Stop(context.Background())
return nil, phases, rr.err
}
phases["rootfs_"+rr.mode+"_ms"] = time.Since(tRootfs).Milliseconds()

tPatch := time.Now()
if err := drv.PatchRootfs(context.Background(), rr.path); err != nil {
if rr.mode == "dmsnap" {
	_ = m.dmsnap.RemoveSnap(id)
}
_ = drv.Stop(context.Background())
return nil, phases, fmt.Errorf("patch rootfs: %w", err)
}
mark("drive_patch_ms", tPatch)

// Phased-boot templates carry a durable data device baked into the
// snapshot as drive "vol1". Patch it to this sandbox's own ext4 image
// before Resume so postgres sees its persistent volume at /dev/vdb.
// Snapshot device topology is frozen, so this only works because the
// placeholder was baked in at build time (see ensureTemplateSnapshot).
if dataDrivePath != "" {
	tData := time.Now()
	if err := drv.PatchDrive(context.Background(), pgDataDriveID, dataDrivePath); err != nil {
		_ = drv.Stop(context.Background())
		return nil, phases, fmt.Errorf("patch data drive: %w", err)
	}
	mark("data_drive_patch_ms", tData)
}

// Start SSH probe concurrently with Resume so the first SYN is in-flight
// the moment guest vCPUs unfreeze. The probe result is collected after
// Resume returns (saves the vm_resume_ms overlap, typically 1-12ms).
tReady := time.Now()
sshAddr := alloc.ProxyGuestIP + ":22"
sshDone := make(chan error, 1)
go func() { sshDone <- waitTCP(sshAddr, 30*time.Second) }()

tResume := time.Now()
if err := drv.Resume(context.Background()); err != nil {
if rr.mode == "dmsnap" {
	_ = m.dmsnap.RemoveSnap(id)
}
_ = drv.Stop(context.Background())
return nil, phases, fmt.Errorf("resume: %w", err)
}
mark("vm_resume_ms", tResume)

// Pin firecracker vCPU threads to dedicated cores for stable p99.
// Best-effort: failures are logged, not fatal.
if m.cpuPinner != nil {
cores := m.cpuPinner.Assign(cpu)
if len(cores) > 0 {
	m.cpuPinner.PinFC(drv.PID(), cores)
}
}

// Optimistic create gate: the VM is usable the instant vCPUs unfreeze
// (Resume returned above without error). We do NOT block create-return
// on the TCP:22 probe — that join was the entire ssh_ready_ms tax on
// boot_ms (170-300ms). sshd/daemon warmth is finished asynchronously by
// the caller's `go m.prewarmSSH(...)` (manager.go), and phased-boot
// (postgres-16) readiness is driven by `go m.kickPGPhase2(...)`, neither
// of which depends on this probe. A dead-on-arrival restore is caught by
// the caller's health monitor within its first cycle. We keep the probe
// running purely to record the ssh_ready_ms metric off the hot path.
go func(start time.Time) {
	if err := <-sshDone; err != nil {
		m.log.Warn("ssh probe (non-gating) failed", "id", id, "err", err)
		return
	}
	readyMS := time.Since(start).Milliseconds()
	m.log.Info("ssh ready (post-return)", "id", id, "ssh_ready_ms", readyMS)
	if m.bus != nil {
		m.bus.Emit(id, "sandbox.ssh_ready", map[string]any{"ssh_ready_ms": readyMS})
	}
}(tReady)

return drv, phases, nil
}

// phasedBootTemplates is the set of templates that split boot into two phases:
//   Phase 1: expensive one-time work (e.g. DB init) → writes /run/pandastack/snapshot-ready
//   Phase 2: per-sandbox credential injection → writes /run/pandastack/ready.json
//
// For these templates the snapshot is taken at the Phase 1/2 boundary so
// every sandbox restore starts with Phase 1 already done (sub-2s launch).
var phasedBootTemplates = map[string]bool{
	"postgres-16": true,
}

const (
	// pgDataDriveID is the Firecracker drive_id of the durable data device.
	// A placeholder of this id is baked into phased-boot template snapshots
	// (so the snapshot's device topology includes it) and patched to the
	// per-database ext4 image at restore via Driver.PatchDrive. It matches
	// the cold-boot volume naming where the first volume is "vol1".
	pgDataDriveID = "vol1"
	// pgDataPlaceholderGB is the size of the placeholder data device baked
	// into the snapshot. The real per-database image MUST be >= this size:
	// the guest can cache device capacity across the snapshot, so a smaller
	// real device is rejected and a larger one needs an in-guest resize.
	pgDataPlaceholderGB = 5
)

// makeSparseImage creates (or truncates) a sparse file of exactly size bytes.
// Used to bake the unformatted placeholder data device into phased-boot
// template snapshots — it must NOT be formatted or mounted, so the guest
// sees an unused block device that autostart.sh initialises on first boot.
func makeSparseImage(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

// waitSnapshotReady polls for /run/pandastack/snapshot-ready inside the guest.
// For phased-boot templates this file is written by autostart.sh Phase 1 after
// all expensive bootstrap is complete. For other templates this is a no-op.
func waitSnapshotReady(ctx context.Context, gc *guest.Client, template string, timeout time.Duration) {
	if !phasedBootTemplates[template] {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		res, err := gc.Exec(ctx, "test -f /run/pandastack/snapshot-ready && echo yes || echo no")
		if err == nil && strings.TrimSpace(res.Stdout) == "yes" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Timed out — proceed with snapshot anyway (best-effort degradation).
}

// waitTCP polls addr with 2ms cadence until a TCP connect succeeds or timeout.
// 2ms (vs old 20ms) cuts worst-case poll latency from 19ms to 1ms per cycle.
func waitTCP(addr string, timeout time.Duration) error {deadline := time.Now().Add(timeout)
var lastErr error
for time.Now().Before(deadline) {
c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
if err == nil {
_ = c.Close()
return nil
}
lastErr = err
time.Sleep(2 * time.Millisecond)
}
return lastErr
}
