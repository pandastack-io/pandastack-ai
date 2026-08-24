// SPDX-License-Identifier: Apache-2.0
package store

import (
	"context"
	"testing"
	"time"
)

// insertSandboxRow + insertSnap are tiny helpers for the GC tests.
func mkSandbox(t *testing.T, s *Store, id, status string) {
	t.Helper()
	sb := map[string]any{
		"id": id, "template": "base", "cpu": 2, "memory_mb": 1024,
		"status": status, "created_at": time.Now().Format(time.RFC3339Nano),
	}
	if err := s.InsertSandbox(context.Background(), sb); err != nil {
		t.Fatalf("InsertSandbox(%s): %v", id, err)
	}
}

func mkSnapshot(t *testing.T, s *Store, id, sandboxID string) {
	t.Helper()
	snap := map[string]any{
		"id": id, "sandbox_id": sandboxID,
		"mem_path":   "/var/lib/pandastack/snapshots/" + id + "/vm.mem",
		"state_path": "/var/lib/pandastack/snapshots/" + id + "/vm.state",
	}
	if err := s.InsertSnapshot(context.Background(), snap); err != nil {
		t.Fatalf("InsertSnapshot(%s): %v", id, err)
	}
}

func TestSnapshotGC_CascadeAndOrphan(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Live sandbox with two snapshots.
	mkSandbox(t, s, "live-sb", "running")
	mkSnapshot(t, s, "snap-a", "live-sb")
	mkSnapshot(t, s, "snap-b", "live-sb")
	// Orphan snapshots: their source sandbox was never inserted (deleted long ago).
	mkSnapshot(t, s, "orphan-1", "gone-sb-1")
	mkSnapshot(t, s, "orphan-2", "gone-sb-2")

	// SnapshotsForSandbox returns exactly the two for the live sandbox.
	got, err := s.SnapshotsForSandbox(ctx, "live-sb")
	if err != nil {
		t.Fatalf("SnapshotsForSandbox: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 snapshots for live-sb, got %d: %+v", len(got), got)
	}

	// With the age gate disabled (cutoff<=0), ExpiredOrphanSnapshots returns the
	// two whose sandbox is gone — NOT the live sandbox's snapshots.
	orphans, err := s.ExpiredOrphanSnapshots(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ExpiredOrphanSnapshots: %v", err)
	}
	if len(orphans) != 2 {
		t.Fatalf("want 2 orphans, got %d: %+v", len(orphans), orphans)
	}
	for _, o := range orphans {
		if o.ID == "snap-a" || o.ID == "snap-b" {
			t.Fatalf("live sandbox snapshot %q wrongly flagged orphan", o.ID)
		}
	}

	// DeleteSnapshot removes a row; afterwards it must not appear as an orphan.
	if err := s.DeleteSnapshot(ctx, "orphan-1"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	orphans, _ = s.ExpiredOrphanSnapshots(ctx, 0, 100)
	if len(orphans) != 1 || orphans[0].ID != "orphan-2" {
		t.Fatalf("after delete want only orphan-2, got %+v", orphans)
	}

	// Limit is honoured.
	mkSnapshot(t, s, "orphan-3", "gone-sb-3")
	limited, _ := s.ExpiredOrphanSnapshots(ctx, 0, 1)
	if len(limited) != 1 {
		t.Fatalf("limit=1 should return 1 row, got %d", len(limited))
	}
}

// TestSnapshotGC_GracePeriod proves a fresh orphan survives the grace window
// (durability) and is only reclaimed once older than the cutoff.
func TestSnapshotGC_GracePeriod(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Two orphans (no sandbox row): one created "now", one created 10 days ago.
	mkSnapshot(t, s, "fresh-orphan", "gone-1")
	mkSnapshot(t, s, "old-orphan", "gone-2")
	tenDaysAgo := time.Now().Add(-10 * 24 * time.Hour).Unix()
	if _, err := s.db.ExecContext(ctx, `UPDATE snapshots SET created_at = ? WHERE id = ?`, tenDaysAgo, "old-orphan"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Cutoff = 7 days ago: only the 10-day-old orphan is past it.
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Unix()
	got, err := s.ExpiredOrphanSnapshots(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("ExpiredOrphanSnapshots: %v", err)
	}
	if len(got) != 1 || got[0].ID != "old-orphan" {
		t.Fatalf("with 7d grace, only old-orphan should expire; got %+v", got)
	}

	// created_at=0 (legacy/unknown age) is treated as ancient → always eligible.
	mkSnapshot(t, s, "legacy-orphan", "gone-3")
	if _, err := s.db.ExecContext(ctx, `UPDATE snapshots SET created_at = 0 WHERE id = ?`, "legacy-orphan"); err != nil {
		t.Fatalf("zero created_at: %v", err)
	}
	got, _ = s.ExpiredOrphanSnapshots(ctx, cutoff, 100)
	ids := map[string]bool{}
	for _, g := range got {
		ids[g.ID] = true
	}
	if !ids["old-orphan"] || !ids["legacy-orphan"] || ids["fresh-orphan"] {
		t.Fatalf("want {old-orphan, legacy-orphan} expired, fresh-orphan kept; got %+v", got)
	}
}
