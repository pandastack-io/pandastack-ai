// SPDX-License-Identifier: Apache-2.0
//
// snapshot_gc.go — snapshot garbage collection.
//
// Before this, snapshots had NO lifecycle: taking a snapshot wrote a `snapshots`
// row + local bytes (DataDir/snapshots/<id>) + a GCS copy (snapshots/<id>/), and
// NOTHING ever removed them — deleting the source sandbox left all three behind.
// Over time that leaks rows, local disk, and GCS storage indefinitely.
//
// Two reapers close the leak:
//   - cascadeDeleteSnapshots: on sandbox delete, purge that sandbox's snapshots.
//   - RunSnapshotReaper: a periodic sweep that purges ORPHANED snapshots (source
//     sandbox already gone) — catches anything created before this shipped, or a
//     cascade that partially failed.
//
// purgeSnapshot is the single teardown primitive both paths share: remove the
// local dir, the GCS blobs (best-effort), then the DB row LAST (so an
// interrupted purge re-appears as an orphan and the reaper finishes it — we
// never drop the row while bytes still exist).

package sandbox

import (
	"context"
	"os"
	"time"

	"github.com/pandastack/agent/internal/store"
)

// purgeSnapshot removes one snapshot across all three layers. DB row is removed
// LAST so a crash mid-purge leaves a recoverable orphan, never dangling bytes.
func (m *Manager) purgeSnapshot(ctx context.Context, snapID string) error {
	// 1. Local dir (DataDir/snapshots/<id>). Idempotent.
	if dir := snapshotDir(m.cfg, snapID); dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			m.log.Warn("snapshot gc: local dir remove failed (continuing)", "snapshot", snapID, "dir", dir, "err", err)
		}
	}
	// 2. GCS blobs (snapshots/<id>/). Best-effort; nil when bucket unset.
	if m.snapStore != nil && m.snapStore.Enabled() {
		gcsCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := m.snapStore.Delete(gcsCtx, snapID); err != nil {
			m.log.Warn("snapshot gc: gcs purge failed (continuing)", "snapshot", snapID, "err", err)
		}
		cancel()
	}
	// 3. DB row LAST.
	if err := m.store.DeleteSnapshot(ctx, snapID); err != nil {
		m.log.Warn("snapshot gc: db row delete failed", "snapshot", snapID, "err", err)
		return err
	}
	return nil
}

// cascadeDeleteSnapshots purges every snapshot taken from a sandbox that is being
// deleted. Best-effort and non-fatal — a sandbox delete must still succeed even
// if a snapshot purge fails (the reaper will catch the leftover as an orphan).
func (m *Manager) cascadeDeleteSnapshots(ctx context.Context, sandboxID string) {
	snaps, err := m.store.SnapshotsForSandbox(ctx, sandboxID)
	if err != nil {
		m.log.Warn("snapshot gc: list-for-sandbox failed (orphans will be reaped later)", "sandbox", sandboxID, "err", err)
		return
	}
	if len(snaps) == 0 {
		return
	}
	for _, s := range snaps {
		_ = m.purgeSnapshot(ctx, s.ID)
	}
	m.log.Info("snapshot gc: cascade-deleted snapshots for sandbox", "sandbox", sandboxID, "count", len(snaps))
}

// RunSnapshotReaper periodically purges orphaned snapshots — those whose source
// sandbox no longer exists. Catches pre-cascade leftovers and any partial
// cascade. Snapshots are DURABLE: an orphan (source sandbox gone, e.g. after an
// idle-reap) is only reclaimed once it has been orphaned for `grace` — giving a
// restore/fork window. Bounded per tick so a backlog drains without a storm.
func (m *Manager) RunSnapshotReaper(ctx context.Context, interval, grace time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	const perTick = 100
	t := time.NewTicker(interval)
	defer t.Stop()
	m.log.Info("snapshot reaper started", "interval", interval.String(), "grace", grace.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.reapExpiredOrphanSnapshots(ctx, grace, perTick)
	}
}

func (m *Manager) reapExpiredOrphanSnapshots(ctx context.Context, grace time.Duration, limit int) {
	// Cutoff: orphans whose created_at is older than (now - grace) are expired.
	// grace<=0 → cutoff 0 → age gate disabled (reap any orphan immediately).
	var cutoff int64
	if grace > 0 {
		cutoff = time.Now().Add(-grace).Unix()
	}
	orphans, err := m.store.ExpiredOrphanSnapshots(ctx, cutoff, limit)
	if err != nil {
		m.log.Warn("snapshot reaper: expired-orphan query failed", "err", err)
		return
	}
	if len(orphans) == 0 {
		return
	}
	purged := 0
	for _, s := range orphans {
		if err := m.purgeSnapshot(ctx, s.ID); err == nil {
			purged++
		}
	}
	m.log.Info("snapshot reaper: purged expired orphaned snapshots",
		"purged", purged, "found", len(orphans), "grace", grace.String())
}

// compile-time guard: SnapshotRow is consumed here.
var _ = store.SnapshotRow{}
