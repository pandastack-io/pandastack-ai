// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Durable-volume auto-grow (managed databases).
//
// Each managed postgres-16 database stores PGDATA on a sparse host ext4 image
// ({DataDir}/volumes/db/{id}.ext4, created at pgDataPlaceholderGB). Without
// intervention the database fills the 5 GB image and postgres starts failing
// writes. This sweeper watches in-guest disk usage and grows the volume
// transparently before that happens:
//
//  1. guest `df` on the PGDATA mount → used%
//  2. at >= autoGrowThresholdPct: grow the host sparse file (×1.5, ≥ +1 GiB,
//     capped at autoGrowMaxBytes), bounded by free space on the host fs
//  3. PATCH /drives/vol1 on the RUNNING VM (same path) — Firecracker re-opens
//     the file and raises a virtio config-change so the guest re-reads the
//     new capacity (PatchDriveLive; never kills the VM on failure)
//  4. guest `resize2fs` grows the ext4 filesystem online
//
// Growth is one step per sweep with a per-sandbox cooldown, so a runaway
// writer ratchets up gradually instead of jumping to the cap. Hibernated DBs
// are skipped (no driver); their volume is grown on a later sweep after wake.
// Hibernate/wake stays consistent: the snapshot captures the guest's view of
// the larger device and wake re-patches to the same (larger) image.

const (
	// autoGrowThresholdPct: grow when the guest reports PGDATA usage at or
	// above this percentage.
	autoGrowThresholdPct = 80
	// autoGrowFactorNum/Den: new size = old × 3/2, rounded up to a whole GiB.
	autoGrowFactorNum = 3
	autoGrowFactorDen = 2
	// autoGrowMinStepBytes: never grow by less than 1 GiB (keeps small
	// volumes from churning in tiny increments).
	autoGrowMinStepBytes = 1 << 30
	// autoGrowMaxBytes: hard per-volume ceiling (100 GiB). Beyond this we
	// log and leave it to the operator / a paid tier.
	autoGrowMaxBytes = 100 << 30
	// autoGrowHostReserveBytes: refuse to grow if the host filesystem would
	// be left with less than this much free space for the new logical bytes.
	autoGrowHostReserveBytes = 10 << 30
	// autoGrowSweepInterval / autoGrowCooldown: sweep cadence and the
	// minimum gap between grow attempts on one sandbox (lets resize2fs and
	// postgres settle, and rate-limits retries after failures).
	autoGrowSweepInterval = 60 * time.Second
	autoGrowCooldown      = 3 * time.Minute
)

// pgDataMount is where autostart.sh mounts the durable volume in the guest.
const pgDataMount = "/var/lib/postgresql/data"

// RunVolumeAutoGrow runs the durable-volume auto-grow sweeper until ctx is
// cancelled. Enabled from main unless PANDASTACK_VOLUME_AUTOGROW=0.
func (m *Manager) RunVolumeAutoGrow(ctx context.Context) {
	t := time.NewTicker(autoGrowSweepInterval)
	defer t.Stop()
	lastGrow := map[string]time.Time{} // sandbox id → last attempt
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		m.mu.RLock()
		ids := make([]string, 0, len(m.drivers))
		for id := range m.drivers {
			ids = append(ids, id)
		}
		m.mu.RUnlock()

		now := time.Now()
		for _, id := range ids {
			// Only sandboxes with a durable DB volume participate; the
			// volume file existing on disk is the discriminator (only the
			// managed-database path creates one).
			volPath := m.dbVolumePath(id)
			if _, err := os.Stat(volPath); err != nil {
				continue
			}
			if now.Sub(lastGrow[id]) < autoGrowCooldown {
				continue
			}
			grew, err := m.autoGrowOne(ctx, id, volPath)
			if err != nil {
				m.log.Warn("volume autogrow failed", "id", id, "err", err)
				lastGrow[id] = now // cooldown applies to failures too
				continue
			}
			if grew {
				lastGrow[id] = now
			}
		}

		// Drop cooldown entries for sandboxes that no longer exist.
		for id := range lastGrow {
			if _, err := os.Stat(m.dbVolumePath(id)); err != nil {
				delete(lastGrow, id)
			}
		}
	}
}

// autoGrowOne checks one database sandbox and performs a single grow step if
// it is at/above the usage threshold. Returns true if a grow was performed.
func (m *Manager) autoGrowOne(ctx context.Context, id, volPath string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	gc, err := m.Guest(id)
	if err != nil {
		return false, fmt.Errorf("guest client: %w", err)
	}

	// One df call: device, fs size, used (bytes). tail skips the header.
	res, err := gc.Exec(cctx, "df -B1 --output=source,size,used "+pgDataMount+" 2>/dev/null | tail -n 1")
	if err != nil {
		return false, fmt.Errorf("guest df: %w", err)
	}
	dev, fsSize, fsUsed, ok := parseDFLine(res.Stdout)
	if !ok || fsSize <= 0 {
		return false, nil // mount not up yet (bootstrap in progress)
	}
	// Guard: make sure we're looking at the durable data device, not the
	// rootfs (volume not mounted ⇒ df reports the parent fs of the path).
	if !strings.HasPrefix(dev, "/dev/vd") || dev == "/dev/vda" {
		return false, nil
	}
	usedPct := float64(fsUsed) * 100 / float64(fsSize)
	if usedPct < autoGrowThresholdPct {
		return false, nil
	}

	st, err := os.Stat(volPath)
	if err != nil {
		return false, fmt.Errorf("stat volume: %w", err)
	}
	cur := st.Size()
	if cur >= autoGrowMaxBytes {
		m.log.Warn("volume autogrow: at max size, not growing",
			"id", id, "size_bytes", cur, "used_pct", fmt.Sprintf("%.1f", usedPct))
		return false, nil
	}
	next := nextVolumeSize(cur)

	// Host capacity guard: the file is sparse, but the new logical bytes
	// will eventually be backed by real blocks. Refuse to promise space the
	// host doesn't have (minus a reserve for snapshots/rootfs churn).
	var hfs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(volPath), &hfs); err == nil {
		avail := uint64(hfs.Bavail) * uint64(hfs.Bsize)
		if need := uint64(next - cur); avail < need+autoGrowHostReserveBytes {
			return false, fmt.Errorf("host fs low on space: need %d + reserve, have %d", need, avail)
		}
	}

	m.log.Info("volume autogrow: growing",
		"id", id, "used_pct", fmt.Sprintf("%.1f", usedPct),
		"from_bytes", cur, "to_bytes", next)

	// 1. Grow the sparse host image (instant; no blocks allocated).
	if err := os.Truncate(volPath, next); err != nil {
		return false, fmt.Errorf("truncate volume: %w", err)
	}

	// 2. Nudge the running VM: PATCH /drives with the same path makes
	//    Firecracker re-open the file and tell the guest the new capacity.
	drv := m.driver(id)
	if drv == nil {
		return false, fmt.Errorf("sandbox driver gone (hibernated mid-sweep?)")
	}
	if err := drv.PatchDriveLive(cctx, pgDataDriveID, volPath); err != nil {
		return false, err
	}

	// 3. Online ext4 grow inside the guest. resize2fs without a size arg
	//    grows to the full device.
	rr, err := gc.Exec(cctx, "resize2fs "+dev+" 2>&1")
	if err != nil {
		return false, fmt.Errorf("guest resize2fs: %w", err)
	}
	if rr.ExitCode != 0 {
		return false, fmt.Errorf("guest resize2fs exit %d: %s", rr.ExitCode, strings.TrimSpace(rr.Stdout+rr.Stderr))
	}

	// 4. Confirm the guest sees the new size (best-effort, log only).
	if vr, err := gc.Exec(cctx, "df -B1 --output=size "+pgDataMount+" 2>/dev/null | tail -n 1"); err == nil {
		if sz, err := strconv.ParseInt(strings.TrimSpace(vr.Stdout), 10, 64); err == nil {
			m.log.Info("volume autogrow: done", "id", id, "guest_fs_bytes", sz, "image_bytes", next)
		}
	}
	return true, nil
}

// nextVolumeSize returns the next size for a volume currently cur bytes
// large: ×3/2, at least +1 GiB, rounded up to a whole GiB, capped at
// autoGrowMaxBytes.
func nextVolumeSize(cur int64) int64 {
	next := cur * autoGrowFactorNum / autoGrowFactorDen
	if next < cur+autoGrowMinStepBytes {
		next = cur + autoGrowMinStepBytes
	}
	next = (next + (1 << 30) - 1) &^ ((1 << 30) - 1) // round up to whole GiB
	if next > autoGrowMaxBytes {
		next = autoGrowMaxBytes
	}
	return next
}

// parseDFLine parses one `df -B1 --output=source,size,used` data row.
func parseDFLine(s string) (dev string, size, used int64, ok bool) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) != 3 {
		return "", 0, 0, false
	}
	size, err1 := strconv.ParseInt(f[1], 10, 64)
	used, err2 := strconv.ParseInt(f[2], 10, 64)
	if err1 != nil || err2 != nil {
		return "", 0, 0, false
	}
	return f[0], size, used, true
}
