// SPDX-License-Identifier: Apache-2.0
// dmsnap.go — device-mapper snapshot CoW rootfs for Firecracker sandboxes.
//
// # Why
//
// The NATID restore path currently copies the template's rootfs for every
// sandbox create (100-400ms on ext4 without reflink, 2-8 GB per sandbox).
// dm-snapshot replaces the copy with a kernel CoW device:
//
//   Template base  → read-only loop device over clone.ext4 (one per template, shared)
//   Per sandbox    → sparse cow.img (1 GB, starts at ~0 bytes written)
//   dm device      → /dev/mapper/pdssnap-<shortID> combines the two
//
// FC is patched to use the dm device via PatchRootfs before Resume.
// Delete tears down the dm device + cow loop + file.
//
// # Patent-relevant claims
//
//  1. Using dm-snapshot to provide per-FC-VM CoW rootfs with zero-copy setup.
//  2. Shared read-only loop device across concurrent creates for the same template.
//  3. Sparse CoW backing: per-sandbox disk cost = 0 at create time.
//  4. Combined with vm.mem page cache sharing for O(1) concurrent restore scaling.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/diskstream"
	"github.com/pandastack/agent/internal/nbdstream"
	"github.com/pandastack/agent/internal/obs"
)

const (
	// cowSizeBytes is the sparse upper-bound of the per-sandbox CoW file.
	// Sparse truncate costs 0 bytes on disk; blocks are allocated only as the
	// guest writes. 1 GB is well above typical sandbox write volume.
	cowSizeBytes = int64(1 * 1024 * 1024 * 1024)

	// dmChunkSectors is the dm-snapshot chunk granularity in 512-byte sectors.
	// 8 sectors = 4 KB — matches the ext4 block size baked into our rootfs images.
	dmChunkSectors = 8

	// dmNamePrefix is prepended to every dm device this manager creates.
	// "pdssnap-" makes them easy to enumerate and clean up on restart.
	dmNamePrefix = "pdssnap-"
)

// baseRef tracks the shared read-only block device that is the dm-snapshot
// ORIGIN for one template. Normally loopDev is a losetup loop over a local
// clone.ext4; for a streaming base it is an NBD device served by nbdOrigin,
// demand-paged from GCS. Everything downstream (CreateSnap's dm-snapshot table,
// CoW, teardown) is identical regardless of which it is.
type baseRef struct {
	loopDev   string
	path      string    // rootfs file this loop was attached from ("" when streaming)
	mtime     time.Time // modtime at attach — detect stale after rebake
	refs      int       // number of live sandboxes using this base
	nbdOrigin *nbdstream.Origin // non-nil when loopDev is a streamed NBD device
}

// snapEntry records what we created for one sandbox so RemoveSnap can tear it down.
type snapEntry struct {
	template string // used to decRef the base
	dmName   string // "pdssnap-<shortID>"
	cowLoop  string // "/dev/loopN"
	cowPath  string // vmDir/cow.img
}

// DMSnapManager creates and destroys device-mapper CoW devices for sandboxes.
// A single instance lives on the Manager for the agent's lifetime.
type DMSnapManager struct {
	mu      sync.Mutex
	bases   map[string]*baseRef  // template name → shared base loop
	snaps   map[string]snapEntry // sandboxID → per-sandbox entry
	enabled bool
	log     func(msg string, args ...any) // slog.Logger.Info
}

// NewDMSnapManager probes for dm_snapshot support and returns a manager.
// If dm_snapshot is unavailable the manager is returned in disabled mode
// and all methods become safe no-ops returning ErrDMSnapDisabled.
func NewDMSnapManager(logInfo func(string, ...any)) *DMSnapManager {
	d := &DMSnapManager{
		bases: map[string]*baseRef{},
		snaps: map[string]snapEntry{},
		log:   logInfo,
	}
	// Try to load the module; non-fatal if already built-in.
	_ = exec.Command("modprobe", "dm_snapshot").Run()
	// Verify dmsetup is functional.
	if err := exec.Command("dmsetup", "version").Run(); err != nil {
		logInfo("dm_snapshot unavailable, using cloneFile fallback", "err", err)
		return d
	}
	d.enabled = true
	logInfo("dm_snapshot enabled")
	return d
}

// Enabled reports whether the dm-snapshot path is operational.
func (d *DMSnapManager) Enabled() bool {
	if d == nil {
		return false
	}
	return d.enabled
}

// InitBase attaches a read-only loop device for the template's rootfs clone.
// Safe to call multiple times for the same template (idempotent).
// Called at agent startup for every ready template and after each bake.
// If the backing file changed (rebake), the stale loop is detached and replaced.
func (d *DMSnapManager) InitBase(template, rootfsPath string) error {
	if !d.enabled {
		return nil
	}

	fi, err := os.Stat(rootfsPath)
	if err != nil {
		return fmt.Errorf("dmsnap InitBase %s: stat %s: %w", template, rootfsPath, err)
	}
	newMtime := fi.ModTime()

	d.mu.Lock()
	existing, ok := d.bases[template]
	if ok {
		// If path and mtime are the same, nothing to do.
		if existing.path == rootfsPath && existing.mtime.Equal(newMtime) {
			d.mu.Unlock()
			return nil
		}
		// Backing file changed (rebake). Only detach if no live sandboxes are using it.
		if existing.refs > 0 {
			d.mu.Unlock()
			d.log("dmsnap: template rebaked with live snapshots, skipping base refresh",
				"template", template, "refs", existing.refs)
			return nil
		}
		oldLoop := existing.loopDev
		delete(d.bases, template)
		d.mu.Unlock()
		if detachErr := loopDetach(oldLoop); detachErr != nil {
			d.log("dmsnap: stale base loop detach failed (non-fatal)", "template", template, "loop", oldLoop, "err", detachErr)
		}
	} else {
		d.mu.Unlock()
	}

	loopDev, err := loopAttachRO(rootfsPath)
	if err != nil {
		return fmt.Errorf("dmsnap InitBase %s: attach loop: %w", template, err)
	}
	d.mu.Lock()
	d.bases[template] = &baseRef{loopDev: loopDev, path: rootfsPath, mtime: newMtime, refs: 0}
	d.mu.Unlock()
	d.log("dmsnap: base loop attached", "template", template, "loop", loopDev)
	return nil
}

// EnsureBase establishes the dm-snapshot ORIGIN for a template, choosing the
// streaming or local path automatically. It is the single dispatcher the
// create/restore flow should call instead of InitBase/InitStreamingBase
// directly: when disk streaming is enabled AND a rootfs.gcs sidecar exists in
// snapDir (a thin schema-v4 seed installed without a local clone.ext4), it
// streams the rootfs over NBD; otherwise it falls back to the local loop over
// rootfsPath, exactly as before. Both paths are idempotent per template.
//
// snapDir is the installed seed directory (template-snaps/<tpl>); rootfsPath is
// the local rootfs file for the non-streaming path. gen identifies the seed
// generation for streaming-base reuse/refresh.
func (d *DMSnapManager) EnsureBase(template, snapDir, rootfsPath, gen string) error {
	if !d.enabled {
		return nil
	}
	if diskStreamEnabled() {
		if _, err := os.Stat(filepath.Join(snapDir, diskstream.DiskRefFile)); err == nil {
			if serr := d.InitStreamingBase(template, snapDir, gen); serr == nil {
				return nil
			} else if rootfsPath != "" {
				// Never-fail: streaming setup failed (no /dev/nbd free, GCS auth,
				// header drift, …) but a local rootfs exists → fall back to the
				// local loop rather than strand the template. A thin v4 seed has
				// only a sparse placeholder clone.ext4 (rootfsPath==""), so this
				// branch is taken only when a real local image is present.
				d.log("dmsnap: streaming base setup failed, falling back to local loop",
					"template", template, "err", serr)
			} else {
				return fmt.Errorf("dmsnap EnsureBase %s: streaming failed and no local rootfs: %w", template, serr)
			}
		}
	}
	return d.InitBase(template, rootfsPath)
}

// nbdOriginConfig builds the OriginConfig from the agent's env knobs.
func nbdOriginConfig(template string, logf func(string, string, ...any)) nbdstream.OriginConfig {
	ioTimeout, fetchBudget := nbdTimeouts()
	return nbdstream.OriginConfig{
		CacheRoot:             diskcacheRoot(),
		CacheMaxBytes:         diskcacheMaxBytes(),
		Template:              template,
		IOTimeout:             ioTimeout,
		FetchBudget:           fetchBudget,
		EgressRateBytesPerSec: egressRateBps(),
		EgressCapMultiplier:   egressCapMultiplier(),
		Logf:                  logf,
	}
}

// egressRateBps is the per-(template,gen)-per-host GCS fetch rate ceiling in
// bytes/sec (PANDASTACK_NBD_EGRESS_MBPS, 0 = unlimited).
func egressRateBps() float64 {
	if v := os.Getenv("PANDASTACK_NBD_EGRESS_MBPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return float64(int64(n) << 20)
		}
	}
	return 0
}

// egressCapMultiplier k: lifetime hard cap = k × working-set. Default 4 (design
// §13 F13); PANDASTACK_NBD_EGRESS_CAP_K, 0 disables the cap.
func egressCapMultiplier() float64 {
	if v := os.Getenv("PANDASTACK_NBD_EGRESS_CAP_K"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
			return n
		}
	}
	return 4
}

// diskStreamEnabled gates the disk-streaming origin path (separate from the
// memory-streaming gate so disk can roll out independently).
func diskStreamEnabled() bool { return os.Getenv("PANDASTACK_STREAM_DISK") == "1" }

// nbdTimeouts resolves the NBD device's kernel io_timeout and the server-side
// per-read fetch breaker. io_timeout is the no-wedge backstop (must clear GCS
// p99); the breaker is tighter and keeps the guest moving on a stuck fetch.
func nbdTimeouts() (ioTimeout, fetchBudget time.Duration) {
	ioTimeout = nbdstream.DefaultIOTimeout
	if v := os.Getenv("PANDASTACK_NBD_IO_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ioTimeout = time.Duration(n) * time.Second
		}
	}
	fetchBudget = 45 * time.Second // < io_timeout; a stuck Range-GET → EIO, not a wedge
	if v := os.Getenv("PANDASTACK_NBD_FETCH_BUDGET_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			fetchBudget = time.Duration(n) * time.Second
		}
	}
	return
}

// diskcacheRoot/diskcacheMaxBytes configure the shared disk-chunk cache. It
// shares the memcache root directory (PANDASTACK_MEMCACHE_DIR) but has its OWN
// budget knob (PANDASTACK_DISK_CACHE_MAX_GB, default 20 GiB) because rootfs
// working sets and image counts differ from memory; the diskstream SharedCache
// keys are "disk-" prefixed so the two caches evict independently.
func diskcacheRoot() string {
	if os.Getenv("PANDASTACK_MEMCACHE") == "0" {
		return ""
	}
	if dir := os.Getenv("PANDASTACK_MEMCACHE_DIR"); dir != "" {
		return dir
	}
	return "/var/lib/pandastack/memcache"
}

func diskcacheMaxBytes() int64 {
	const defaultGB = 20
	gb := defaultGB
	if v := os.Getenv("PANDASTACK_DISK_CACHE_MAX_GB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			gb = n
		}
	}
	return int64(gb) << 30
}

// InitStreamingBase establishes the dm-snapshot ORIGIN for a template from a
// streamed (GCS-backed, demand-paged) rootfs instead of a local clone.ext4. It
// reads the rootfs.gcs sidecar + clone.ext4.header in snapDir, starts an
// nbdstream Origin serving /dev/nbdN, and registers that device as the base.
// CreateSnap, RemoveSnap, and the CoW path are all unchanged — they just layer
// over an NBD origin instead of a loop origin.
//
// It is idempotent per template: a second call with a live base is a no-op
// (the NBD device + its shared cache are reused across every restore). gen, when
// non-empty, identifies the seed generation so a re-baked template (new gen)
// re-establishes the device; an unchanged gen reuses the existing one.
func (d *DMSnapManager) InitStreamingBase(template, snapDir, gen string) error {
	if !d.enabled {
		return fmt.Errorf("dmsnap: dm_snapshot disabled, cannot stream base")
	}

	d.mu.Lock()
	if existing, ok := d.bases[template]; ok {
		// Same generation (encoded in path for streaming bases) → reuse.
		if existing.nbdOrigin != nil && existing.path == gen {
			d.mu.Unlock()
			return nil
		}
		if existing.refs > 0 {
			d.mu.Unlock()
			d.log("dmsnap: streaming base rebaked with live snapshots, skipping refresh",
				"template", template, "refs", existing.refs)
			return nil
		}
		old := existing
		delete(d.bases, template)
		d.mu.Unlock()
		d.detachBase(template, old)
		d.mu.Lock()
	}
	d.mu.Unlock()

	origin, err := nbdstream.OpenOrigin(snapDir, nbdOriginConfig(template,
		func(level, msg string, kv ...any) { d.log(msg, kv...) }))
	if err != nil {
		return fmt.Errorf("dmsnap InitStreamingBase %s: open origin: %w", template, err)
	}

	d.mu.Lock()
	// path stores the generation for streaming bases (no local rootfs file).
	d.bases[template] = &baseRef{loopDev: origin.Device, path: gen, mtime: time.Now(), refs: 0, nbdOrigin: origin}
	d.mu.Unlock()
	d.log("dmsnap: streaming base attached", "template", template, "device", origin.Device, "gen", gen)
	return nil
}

// detachBase tears down a base's ORIGIN: an NBD origin is Closed (which
// disconnects the device + stops the daemon), otherwise the loop is detached.
func (d *DMSnapManager) detachBase(template string, b *baseRef) {
	if b == nil {
		return
	}
	if b.nbdOrigin != nil {
		if err := b.nbdOrigin.Close(); err != nil {
			d.log("dmsnap: streaming base origin close failed (non-fatal)", "template", template, "err", err)
		}
		return
	}
	if b.loopDev != "" {
		if err := loopDetach(b.loopDev); err != nil {
			d.log("dmsnap: base loop detach failed (non-fatal)", "template", template, "loop", b.loopDev, "err", err)
		}
	}
}

// CreateSnap creates a dm-snapshot CoW device for a new sandbox.
// Returns the /dev/mapper/… path to pass to FC via PatchRootfs.
// Falls back gracefully: if any step fails, the caller should use cloneFile.
func (d *DMSnapManager) CreateSnap(template, sandboxID, vmDir string) (string, error) {
	if !d.enabled {
		return "", fmt.Errorf("dm_snapshot disabled")
	}

	// Claim a reference to the template's base loop.
	d.mu.Lock()
	base, ok := d.bases[template]
	if !ok {
		d.mu.Unlock()
		return "", fmt.Errorf("dmsnap: no base for template %q (call InitBase first)", template)
	}
	base.refs++
	baseLoop := base.loopDev
	d.mu.Unlock()

	// Create the sparse CoW backing file.
	cowPath := filepath.Join(vmDir, "cow.img")
	if err := createSparseFile(cowPath, cowSizeBytes); err != nil {
		d.mu.Lock()
		base.refs--
		d.mu.Unlock()
		return "", fmt.Errorf("dmsnap: create cow file: %w", err)
	}

	// Attach a read-write loop device over the CoW file.
	cowLoop, err := loopAttachRW(cowPath)
	if err != nil {
		os.Remove(cowPath)
		d.mu.Lock()
		base.refs--
		d.mu.Unlock()
		return "", fmt.Errorf("dmsnap: attach cow loop: %w", err)
	}

	// Get the base device size in 512-byte sectors.
	sectors, err := blockDevSectors(baseLoop)
	if err != nil {
		_ = loopDetach(cowLoop)
		os.Remove(cowPath)
		d.mu.Lock()
		base.refs--
		d.mu.Unlock()
		return "", fmt.Errorf("dmsnap: base dev sectors: %w", err)
	}

	// Create the dm-snapshot device.
	// Table format: "<start> <length> snapshot <origin> <cow> P <chunksize>"
	// P = persistent mode (CoW data stored in cow.img).
	// Use first 16 hex chars of UUID for the device name (vs 8) to avoid collisions
	// while keeping the name short enough for dm device limits (127 chars).
	dmName := dmNamePrefix + longID(sandboxID)
	table := fmt.Sprintf("0 %d snapshot %s %s P %d", sectors, baseLoop, cowLoop, dmChunkSectors)
	if out, err := exec.Command("dmsetup", "create", dmName, "--table", table).CombinedOutput(); err != nil {
		_ = loopDetach(cowLoop)
		os.Remove(cowPath)
		d.mu.Lock()
		base.refs--
		d.mu.Unlock()
		return "", fmt.Errorf("dmsetup create %s: %w: %s", dmName, err, out)
	}

	d.mu.Lock()
	d.snaps[sandboxID] = snapEntry{
		template: template,
		dmName:   dmName,
		cowLoop:  cowLoop,
		cowPath:  cowPath,
	}
	d.mu.Unlock()

	dmDev := "/dev/mapper/" + dmName
	d.log("dmsnap: snapshot created", "sandbox", sandboxID, "template", template, "dev", dmDev)
	return dmDev, nil
}

// RemoveSnap tears down the dm-snapshot for the given sandbox.
// Safe to call even if the sandbox never used dm-snapshot (no-op).
// Must be called BEFORE os.RemoveAll(vmDir) since cow.img is inside vmDir.
func (d *DMSnapManager) RemoveSnap(sandboxID string) error {
	if d == nil || !d.enabled {
		return nil
	}
	d.mu.Lock()
	entry, ok := d.snaps[sandboxID]
	if !ok {
		d.mu.Unlock()
		return nil
	}
	delete(d.snaps, sandboxID)
	base := d.bases[entry.template]
	if base != nil {
		base.refs--
	}
	d.mu.Unlock()

	var errs []string
	// Replace dmsetup's built-in --retry (loops forever if device is busy)
	// with a bounded Go retry. FC must be dead before we reach this point,
	// but if the dm device is briefly busy we give it up to 10 s.
	if err := dmsetupRemove(entry.dmName, 10*time.Second); err != nil {
		errs = append(errs, err.Error())
	}
	if err := loopDetach(entry.cowLoop); err != nil {
		errs = append(errs, fmt.Sprintf("losetup detach %s: %v", entry.cowLoop, err))
	}
	// cow.img is removed by the caller's os.RemoveAll(vmDir); do it here too
	// in case the caller skips RemoveAll (e.g., persistent sandboxes).
	if err := os.Remove(entry.cowPath); err != nil && !os.IsNotExist(err) {
		// non-fatal; cow.img is inside vmDir and RemoveAll will catch it
		d.log("dmsnap: cow remove non-fatal", "path", entry.cowPath, "err", err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("dmsnap RemoveSnap %s: %s", sandboxID, strings.Join(errs, "; "))
	}
	d.log("dmsnap: snapshot removed", "sandbox", sandboxID)
	return nil
}

// dmsetupRemove retries "dmsetup remove <name>" up to the given deadline.
// This replaces dmsetup's --retry flag which loops forever when the device
// is held open (e.g. by an orphaned firecracker process). With a bounded
// deadline we guarantee termination.
func dmsetupRemove(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const sleep = 200 * time.Millisecond
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		out, err := exec.CommandContext(ctx, "dmsetup", "remove", name).CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dmsetup remove %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
		time.Sleep(sleep)
	}
}

// CleanupStale removes any pdssnap-* dm devices left by a previous agent run.
// Called once at startup before InitBase.
func (d *DMSnapManager) CleanupStale() {
	if !d.enabled {
		return
	}
	out, err := exec.Command("dmsetup", "ls", "--target", "snapshot").CombinedOutput()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if strings.HasPrefix(name, dmNamePrefix) {
			if err := dmsetupRemove(name, 10*time.Second); err != nil {
				d.log("dmsnap: stale remove failed (non-fatal)", "name", name, "err", err)
			} else {
				d.log("dmsnap: stale device removed", "name", name)
			}
		}
	}
}

// RunWatchdog polls every live streaming base every 5s and force-recovers any
// whose NBD device is wedged — a request accepted but neither completed nor
// errored for the stall window (default 60s, env PANDASTACK_NBD_WATCHDOG_STALL_SEC).
// This is the F5 backstop for a failure the per-read breaker (45s) and the
// in-kernel io_timeout (90s) don't catch: a daemon stuck in a way that produces
// no completion at all (deadlocked serve goroutine, stuck syscall). The stall
// window MUST exceed the breaker so a legitimately slow fetch — which the
// breaker answers with EIO, stamping progress — never looks wedged.
//
// Recovery = detach the stalled base (Close the origin, which NBD_DISCONNECTs
// and unblocks any pending reader) and drop it from the map. The next create
// re-establishes it via EnsureBase (idempotent) — or, if streaming keeps
// failing, the never-fail layer falls back to a local download. Blocks until ctx
// is cancelled; run it in a goroutine.
func (d *DMSnapManager) RunWatchdog(ctx context.Context) {
	if d == nil || !d.enabled {
		return
	}
	stall := 60 * time.Second
	if v := os.Getenv("PANDASTACK_NBD_WATCHDOG_STALL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			stall = time.Duration(n) * time.Second
		}
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reapStalledBases(stall)
		}
	}
}

// reapStalledBases force-recovers any streaming base wedged longer than stall.
func (d *DMSnapManager) reapStalledBases(stall time.Duration) {
	// Snapshot the stalled set under the lock; detach outside it (Close blocks).
	type victim struct {
		tpl string
		b   *baseRef
	}
	var victims []victim
	d.mu.Lock()
	for tpl, b := range d.bases {
		if b.nbdOrigin != nil && b.nbdOrigin.StalledFor(stall) {
			victims = append(victims, victim{tpl, b})
			delete(d.bases, tpl)
		}
	}
	d.mu.Unlock()
	for _, v := range victims {
		d.log("dmsnap: WATCHDOG force-recovering wedged streaming base",
			"template", v.tpl, "device", v.b.loopDev, "stall", stall.String(), "refs", v.b.refs)
		obs.DiskWatchdogRecoveriesTotal.Inc()
		d.detachBase(v.tpl, v.b)
	}
}

// Shutdown detaches all base loop devices. Called when the agent exits.
func (d *DMSnapManager) Shutdown() {
	if d == nil || !d.enabled {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for tpl, base := range d.bases {
		d.detachBase(tpl, base)
	}
	d.bases = map[string]*baseRef{}
}

// --- helpers -----------------------------------------------------------------

func loopAttachRO(path string) (string, error) {
	out, err := exec.Command("losetup", "--find", "--show", "--read-only", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("losetup RO %s: %w: %s", path, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func loopAttachRW(path string) (string, error) {
	out, err := exec.Command("losetup", "--find", "--show", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("losetup RW %s: %w: %s", path, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func loopDetach(loopDev string) error {
	if loopDev == "" {
		return nil
	}
	out, err := exec.Command("losetup", "--detach", loopDev).CombinedOutput()
	if err != nil {
		return fmt.Errorf("losetup detach %s: %w: %s", loopDev, err, out)
	}
	return nil
}

func createSparseFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size) // sparse: no disk blocks allocated
}

func blockDevSectors(dev string) (int64, error) {
	out, err := exec.Command("blockdev", "--getsz", dev).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsz %s: %w: %s", dev, err, out)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("blockdev parse %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n, nil
}

// longID returns the first 16 hex chars of a UUID (after stripping dashes).
// 64 bits of UUID uniqueness is sufficient for concurrent sandbox names.
func longID(id string) string {
	s := strings.ReplaceAll(id, "-", "")
	if len(s) >= 16 {
		return s[:16]
	}
	return s
}
