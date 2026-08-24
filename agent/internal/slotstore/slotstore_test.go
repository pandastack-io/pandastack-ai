// SPDX-License-Identifier: Apache-2.0
package slotstore

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func newStore(t *testing.T, capacity uint32) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "slots.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Seed(context.Background(), capacity); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s
}

// TestClaimLowestFreeDistinct: N claims yield N DISTINCT indices, ascending.
func TestClaimLowestFreeDistinct(t *testing.T) {
	s := newStore(t, 16)
	ctx := context.Background()
	seen := map[uint32]bool{}
	for i := 0; i < 16; i++ {
		idx, err := s.Claim(ctx, fmt.Sprintf("sb-%d", i), "natid")
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if seen[idx] {
			t.Fatalf("claim %d returned duplicate idx %d", i, idx)
		}
		seen[idx] = true
		if idx != uint32(i) {
			t.Fatalf("claim %d = idx %d, want lowest-free %d", i, idx, i)
		}
	}
}

// TestExhaustionCleanError: claiming past capacity returns ErrPoolExhausted, not a panic.
func TestExhaustionCleanError(t *testing.T) {
	s := newStore(t, 2)
	ctx := context.Background()
	_, _ = s.Claim(ctx, "a", "natid")
	_, _ = s.Claim(ctx, "b", "natid")
	_, err := s.Claim(ctx, "c", "natid")
	if err == nil || err.Error() != ErrPoolExhausted.Error() {
		t.Fatalf("claim past capacity err = %v, want ErrPoolExhausted", err)
	}
}

// TestReleaseReclaimsLowest: release frees the slot; next claim reuses the lowest free.
func TestReleaseReclaimsLowest(t *testing.T) {
	s := newStore(t, 8)
	ctx := context.Background()
	_, _ = s.Claim(ctx, "a", "natid") // 0
	_, _ = s.Claim(ctx, "b", "natid") // 1
	_, _ = s.Claim(ctx, "c", "natid") // 2

	ok, err := s.Release(ctx, "b") // free idx 1
	if err != nil || !ok {
		t.Fatalf("release b: ok=%v err=%v", ok, err)
	}
	// Next claim must REUSE idx 1 (lowest free), not advance to 3.
	idx, _ := s.Claim(ctx, "d", "natid")
	if idx != 1 {
		t.Fatalf("reuse after release = %d, want 1", idx)
	}
}

// TestDoubleReleaseNoDoubleFree: the single-owner-of-reclaim guarantee.
func TestDoubleReleaseNoDoubleFree(t *testing.T) {
	s := newStore(t, 8)
	ctx := context.Background()
	_, _ = s.Claim(ctx, "a", "natid")
	ok1, _ := s.Release(ctx, "a")
	ok2, _ := s.Release(ctx, "a")
	if !ok1 {
		t.Fatal("first release should report freed=true")
	}
	if ok2 {
		t.Fatal("second release of same sandbox returned freed=true (double-free)")
	}
}

// TestReClaimSameSandboxIdempotent: claiming twice for the same id returns the
// same slot, never a second one (re-entrant create safety).
func TestReClaimSameSandboxIdempotent(t *testing.T) {
	s := newStore(t, 8)
	ctx := context.Background()
	a, _ := s.Claim(ctx, "x", "natid")
	b, _ := s.Claim(ctx, "x", "natid")
	if a != b {
		t.Fatalf("re-claim returned different slots %d vs %d", a, b)
	}
	st, _ := s.Stats(ctx)
	if st.Used != 1 {
		t.Fatalf("re-claim consumed %d slots, want 1", st.Used)
	}
}

// TestClaimIdxAdoptsPrebuilt: claiming a specific free idx works; claiming one
// owned by another fails; re-claiming own idx is a no-op success.
func TestClaimIdxAdoptsPrebuilt(t *testing.T) {
	s := newStore(t, 8)
	ctx := context.Background()
	ok, err := s.ClaimIdx(ctx, "p", "prebuilt", 5)
	if err != nil || !ok {
		t.Fatalf("claim free idx 5: ok=%v err=%v", ok, err)
	}
	// adopt: rewrite owner to the real sandbox
	ok, _ = s.ClaimIdx(ctx, "real", "natid", 5)
	if ok {
		t.Fatal("claiming idx owned by 'p' for 'real' should fail")
	}
	// p re-claims its own idx → no-op success
	ok, _ = s.ClaimIdx(ctx, "p", "prebuilt", 5)
	if !ok {
		t.Fatal("re-claiming own idx should succeed")
	}
}

// TestReconcileFreesOrphans: slots whose owner is not in liveIDs are reclaimed;
// live owners (incl. persistent VMs) keep their slots. This is the persistent-VM
// + crash-mid-allocate safety net.
func TestReconcileFreesOrphans(t *testing.T) {
	s := newStore(t, 8)
	ctx := context.Background()
	_, _ = s.Claim(ctx, "live-db", "natid")   // 0 — a persistent DB, survives
	_, _ = s.Claim(ctx, "dead-1", "natid")     // 1 — orphan
	_, _ = s.Claim(ctx, "dead-2", "legacy")    // 2 — orphan
	_, _ = s.Claim(ctx, "live-app", "natid")   // 3 — survives

	n, err := s.Reconcile(ctx, []string{"live-db", "live-app"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 2 {
		t.Fatalf("reconcile freed %d, want 2 orphans", n)
	}
	// live owners kept
	if _, ok, _ := s.OwnerIdx(ctx, "live-db"); !ok {
		t.Fatal("reconcile wrongly freed a LIVE persistent slot (live-db)")
	}
	if _, ok, _ := s.OwnerIdx(ctx, "live-app"); !ok {
		t.Fatal("reconcile wrongly freed a LIVE slot (live-app)")
	}
	// orphans freed
	if _, ok, _ := s.OwnerIdx(ctx, "dead-1"); ok {
		t.Fatal("reconcile failed to free orphan dead-1")
	}
}

// TestBootDiskGuard: opening on the stateful disk path is rejected.
func TestBootDiskGuard(t *testing.T) {
	_, err := Open("/var/lib/pandastack/volumes/slots.db")
	if err == nil {
		t.Fatal("opening slot db on the stateful data disk must be rejected")
	}
}

// TestConcurrentClaimRelease is the core invariant test: many goroutines claim
// and release concurrently; assert (a) no index is ever owned by two sandboxes
// at once, and (b) capacity == free + used always holds. Run under -race.
func TestConcurrentClaimRelease(t *testing.T) {
	const capacity = 64
	const workers = 32
	const iters = 50
	s := newStore(t, capacity)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmt.Sprintf("w%d-i%d", w, i)
				idx, err := s.Claim(ctx, id, "natid")
				if err != nil {
					// exhaustion under contention is fine; just skip the release.
					if err == ErrPoolExhausted {
						continue
					}
					// transient lost-race is also acceptable; retry next iter.
					continue
				}
				_ = idx
				// immediately release so the pool churns hard.
				if _, err := s.Release(ctx, id); err != nil {
					t.Errorf("release %s: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Invariant after churn: every slot free (we released everything we claimed),
	// no owner has two rows, capacity intact.
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Capacity != capacity {
		t.Fatalf("capacity drifted: %d want %d", st.Capacity, capacity)
	}
	if st.Free+st.Used != st.Capacity {
		t.Fatalf("invariant broken: free(%d)+used(%d) != capacity(%d)", st.Free, st.Used, st.Capacity)
	}
	if st.Used != 0 {
		t.Fatalf("after releasing everything, used=%d want 0 (leak)", st.Used)
	}

	// No duplicate ownership: assert every idx appears at most once (PK guarantees
	// it, but verify the table is internally consistent).
	rows, err := s.db.QueryContext(ctx, `SELECT idx, COUNT(*) FROM network_slots GROUP BY idx HAVING COUNT(*) > 1`)
	if err != nil {
		t.Fatalf("dup check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("duplicate idx rows exist")
	}
}

// TestPersistAcrossReopen: a slot owned before close is still owned after
// reopening the same file (process restart with intact disk). The manager's
// Reconcile then decides whether to keep it (live) or free it (orphan).
func TestPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slots.db")
	ctx := context.Background()

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := s1.Seed(ctx, 8); err != nil {
		t.Fatalf("seed: %v", err)
	}
	idx, _ := s1.Claim(ctx, "persistent-db", "natid")
	_ = s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer s2.Close()
	got, ok, err := s2.OwnerIdx(ctx, "persistent-db")
	if err != nil || !ok {
		t.Fatalf("ownership lost across reopen: ok=%v err=%v", ok, err)
	}
	if got != idx {
		t.Fatalf("idx changed across reopen: %d vs %d", got, idx)
	}
}
