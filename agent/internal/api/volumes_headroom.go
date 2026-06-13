// SPDX-License-Identifier: Apache-2.0
package api

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Host-level headroom gate for volume creation.
//
// Tier quotas (tier.go) bound what one WORKSPACE may provision; nothing bounds
// what the HOST has promised in aggregate. Volumes are sparse ext4 images, so
// provisioned size >> bytes actually used is normal and healthy — but sparse
// files grow on write, and when the backing filesystem runs out of real space
// every volume on the host EIO/ENOSPCs at once, taking down running sandboxes.
// So POST /volumes admits a new volume only while BOTH limits hold:
//
//  1. Oversubscription cap — total provisioned bytes across all workspaces
//     (volumes/*.ext4, volumes/ws/<ws>/*.ext4, volumes/db/*.ext4) plus the
//     request must stay under oversubFactor × filesystem size. Default 3×:
//     with typical sparse utilisation well under 1/3, the disk can absorb it.
//  2. Free-space reserve — the volumes filesystem must still have
//     freeReserve bytes genuinely free. Default 20 GiB. This is the hard
//     backstop that stops admission long before running volumes start failing
//     writes.
//
// On a miss the handler returns HTTP 507 Insufficient Storage; the control
// plane treats that as "place this volume on another agent / add capacity",
// not as a user error. Stat failures fail OPEN (admission allowed) so an
// exotic filesystem never bricks volume creation.

const (
	defaultVolumeOversubFactor   = 3.0
	defaultVolumeFreeReserveByte = int64(20) << 30 // 20 GiB
)

// headroomState carries everything headroomDecision needs, so the two-limit
// arithmetic is pure and unit-testable without a real filesystem.
type headroomState struct {
	ProvisionedBytes int64 // sum of existing *.ext4 apparent sizes (sparse)
	RequestBytes     int64 // size of the volume being created
	FSSizeBytes      int64 // total size of the volumes filesystem (0 = unknown)
	FSFreeBytes      int64 // bytes actually free on the volumes filesystem
	OversubFactor    float64
	FreeReserveBytes int64
}

// headroomDecision returns "" when admission is allowed, otherwise a
// human-readable refusal reason.
func headroomDecision(st headroomState) string {
	if st.FSSizeBytes <= 0 {
		return "" // statfs unavailable — fail open
	}
	budget := int64(st.OversubFactor * float64(st.FSSizeBytes))
	if st.ProvisionedBytes+st.RequestBytes > budget {
		return fmt.Sprintf(
			"provisioned volume bytes would exceed host budget: %d existing + %d requested > %d (%.1fx of %d-byte filesystem)",
			st.ProvisionedBytes, st.RequestBytes, budget, st.OversubFactor, st.FSSizeBytes)
	}
	if st.FSFreeBytes < st.FreeReserveBytes {
		return fmt.Sprintf(
			"host free space below reserve: %d free < %d reserved",
			st.FSFreeBytes, st.FreeReserveBytes)
	}
	return ""
}

func volumeOversubFactor() float64 {
	if v := strings.TrimSpace(os.Getenv("PANDASTACK_VOLUME_OVERSUB_FACTOR")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return defaultVolumeOversubFactor
}

func volumeFreeReserveBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("PANDASTACK_VOLUME_FREE_RESERVE_GB")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n << 30
		}
	}
	return defaultVolumeFreeReserveByte
}

// provisionedVolumeBytes sums the APPARENT size of every volume image under
// <dataDir>/volumes, recursively — legacy root volumes, per-workspace ws/<ws>/
// volumes, and managed-DB db/ volumes all count against the same host budget.
func provisionedVolumeBytes(dataDir string) int64 {
	var total int64
	root := filepath.Join(dataDir, "volumes")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".ext4") {
			return nil //nolint:nilerr // best-effort scan
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// volumesFSStat reports (size, free) of the filesystem backing the volumes
// dir. With the stateful PD mounted at <dataDir>/volumes this measures the
// dedicated disk; without it, the parent data filesystem. (0,0) on error.
func volumesFSStat(dataDir string) (size, free int64) {
	dir := filepath.Join(dataDir, "volumes")
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		// volumes/ may not exist yet (first volume on a fresh host).
		if err = syscall.Statfs(dataDir, &st); err != nil {
			return 0, 0
		}
	}
	bsize := int64(st.Bsize)
	return int64(st.Blocks) * bsize, int64(st.Bavail) * bsize
}

// VolumeStorageStats exposes the volume-storage numbers the agent heartbeat
// advertises to the control-plane scheduler (capacity_json), so volume
// creation can be placed on the agent with the most storage headroom instead
// of the most free CPU. Same primitives the local 507 gate uses — the
// scheduler's view is advisory; checkVolumeHeadroom remains authoritative.
func VolumeStorageStats(dataDir string) (provisioned, fsSize, fsFree int64) {
	fsSize, fsFree = volumesFSStat(dataDir)
	return provisionedVolumeBytes(dataDir), fsSize, fsFree
}

// checkVolumeHeadroom gathers live filesystem state and applies the two-limit
// decision. Returns the state (for error payloads/logging) and a non-empty
// reason when the volume must be refused.
func checkVolumeHeadroom(dataDir string, requestBytes int64) (headroomState, string) {
	size, free := volumesFSStat(dataDir)
	st := headroomState{
		ProvisionedBytes: provisionedVolumeBytes(dataDir),
		RequestBytes:     requestBytes,
		FSSizeBytes:      size,
		FSFreeBytes:      free,
		OversubFactor:    volumeOversubFactor(),
		FreeReserveBytes: volumeFreeReserveBytes(),
	}
	return st, headroomDecision(st)
}
