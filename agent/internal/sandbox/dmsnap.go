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

// baseRef tracks the shared read-only loop device for one template.
type baseRef struct {
	loopDev string
	path    string    // rootfs file this loop was attached from
	mtime   time.Time // modtime at attach — detect stale after rebake
	refs    int        // number of live sandboxes using this base
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

// Shutdown detaches all base loop devices. Called when the agent exits.
func (d *DMSnapManager) Shutdown() {
	if d == nil || !d.enabled {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for tpl, base := range d.bases {
		if err := loopDetach(base.loopDev); err != nil {
			d.log("dmsnap: shutdown loop detach failed", "template", tpl, "loop", base.loopDev, "err", err)
		}
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
