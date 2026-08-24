// SPDX-License-Identifier: Apache-2.0
package seed

import (
	"sort"
	"strconv"
	"testing"
	"time"
)

// gcDeletableGenerations is the pure decision core of the user-template GC.
// These tests pin the invariant that broke static-builder live: the GC must
// NEVER delete the generation CURRENT points at, nor a newer in-flight peer
// generation — while still reclaiming superseded losers so storage stays
// bounded under the N-agent fan-out.
func sortedEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// genAt returns a real epoch-nanosecond generation name for a gen created `ago`
// before `now` — so the age-based grace logic can be exercised deterministically.
func genAt(now time.Time, ago time.Duration) string {
	return strconv.FormatInt(now.Add(-ago).UnixNano(), 10)
}

func TestGCDeletable_ProtectsCurrentAndEverythingNewer(t *testing.T) {
	// The core orphan-safety invariant: never delete CURRENT's target, and never
	// delete a gen numerically >= CURRENT (a peer's in-flight winner a future
	// monotonic flip may name). Only the strictly-older, past-grace loser goes.
	now := time.Now()
	old := genAt(now, 2*time.Hour)      // < current, past grace → deletable
	cur := genAt(now, 90*time.Minute)   // CURRENT
	newer := genAt(now, 80*time.Minute) // > cur numerically → must be protected
	got := gcDeletableGenerations([]string{old, cur, newer}, cur, now, gcGraceWindow)
	if !sortedEqual(got, []string{old}) {
		t.Fatalf("deletable=%v want [%s]; must protect CURRENT and everything >= it", got, old)
	}
	for _, g := range got {
		if g == cur || g == newer {
			t.Fatalf("GC would delete CURRENT (%s) or a newer gen (%s) — orphan/rollback bug", cur, newer)
		}
	}
}

func TestGCDeletable_ReclaimsFanoutLosersImmediately(t *testing.T) {
	// N-agent fan-out storage bound: a single build leaves N gens; the winner
	// (numeric-max) becomes CURRENT and every LOSER is < CURRENT. Losers must be
	// reclaimable even though they are seconds old — grace must NOT retain a gen
	// below a landed CURRENT, or storage grows unboundedly (the regression the
	// adversarial review caught). Here all gens are "just uploaded".
	now := time.Now()
	l1 := genAt(now, 3*time.Second)
	l2 := genAt(now, 2*time.Second)
	winner := genAt(now, 1*time.Second) // greatest → CURRENT
	got := gcDeletableGenerations([]string{l1, l2, winner}, winner, now, gcGraceWindow)
	if !sortedEqual(got, []string{l1, l2}) {
		t.Fatalf("deletable=%v want [%s %s]; young losers below CURRENT must be reclaimable", got, l1, l2)
	}
}

func TestGCDeletable_ProtectsInFlightPeerAboveCurrent(t *testing.T) {
	// A peer just uploaded a gen NEWER than the CURRENT we read, but its flip
	// hasn't landed yet (CURRENT still names the older winner-so-far). The newer
	// gen is >= current → protected by the numeric guard, so a concurrent GC
	// can't delete a gen that is about to become live.
	now := time.Now()
	cur := genAt(now, 10*time.Second)
	peerInFlight := genAt(now, 1*time.Second) // > cur, flip not yet landed
	got := gcDeletableGenerations([]string{cur, peerInFlight}, cur, now, gcGraceWindow)
	if len(got) != 0 {
		t.Fatalf("deletable=%v want []; an in-flight peer gen above CURRENT must be protected", got)
	}
}

func TestGCDeletable_DeletesOldBelowCurrent(t *testing.T) {
	now := time.Now()
	old := genAt(now, 3*time.Hour)
	cur := genAt(now, 20*time.Minute)
	if got := gcDeletableGenerations([]string{old, cur}, cur, now, gcGraceWindow); !sortedEqual(got, []string{old}) {
		t.Fatalf("deletable=%v want [%s]", got, old)
	}
}

func TestGCDeletable_CorruptCurrent_GraceProtectsRecent(t *testing.T) {
	// If CURRENT is a corrupt/unparseable value we cannot order gens against it,
	// so we must NOT delete a recently-uploaded gen (it may be the live winner a
	// flip is about to name). Only a genuinely-old gen is reclaimable. The GC
	// caller ALSO aborts entirely on empty/unreadable CURRENT; this covers the
	// nonempty-but-garbage case.
	now := time.Now()
	young := genAt(now, 1*time.Minute)
	old := genAt(now, 2*time.Hour)
	got := gcDeletableGenerations([]string{young, old}, "not-a-number", now, gcGraceWindow)
	if !sortedEqual(got, []string{old}) {
		t.Fatalf("deletable=%v want [%s]; under corrupt CURRENT, grace must protect the young gen", got, old)
	}
}

func TestGCDeletable_UnparseableGenNameProtected(t *testing.T) {
	now := time.Now()
	cur := genAt(now, time.Minute)
	old := genAt(now, 2*time.Hour)
	got := gcDeletableGenerations([]string{"garbage", old, cur}, cur, now, gcGraceWindow)
	if !sortedEqual(got, []string{old}) {
		t.Fatalf("deletable=%v want [%s]; an unparseable gen name must never be deleted", got, old)
	}
}

func TestGCDeletable_SingleGeneration_NeverDeletes(t *testing.T) {
	now := time.Now()
	g := genAt(now, time.Hour)
	if got := gcDeletableGenerations([]string{g}, g, now, gcGraceWindow); len(got) != 0 {
		t.Fatalf("deletable=%v want [] (never delete the only/current gen)", got)
	}
}

func TestGCDeletable_EmptyNamesIgnored(t *testing.T) {
	now := time.Now()
	old := genAt(now, 2*time.Hour)
	cur := genAt(now, time.Minute)
	got := gcDeletableGenerations([]string{"", old, cur}, cur, now, gcGraceWindow)
	if !sortedEqual(got, []string{old}) {
		t.Fatalf("deletable=%v want [%s] (blank names skipped)", got, old)
	}
}

// TestIsPreconditionFailed pins the 412 classification the CAS flip relies on:
// a gcloud precondition failure must be recognized (→ retry), anything else must
// not (→ hard error). Wording taken from the live gcloud 412 output.
func TestIsPreconditionFailed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"live 412 wording", errTest("gcloud storage cp: HTTPError 412: At least one of the pre-conditions you specified did not hold."), true},
		{"conditionNotMet", errTest("googleapi: Error 412: conditionNotMet"), true},
		{"bare 412", errTest("stripe 412 whatever"), true},
		{"unrelated failure", errTest("gcloud storage cp: 503 backend error"), false},
		{"matched no objects is not a precondition", errTest("matched no objects or files"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPreconditionFailed(c.err); got != c.want {
				t.Fatalf("isPreconditionFailed(%v)=%v want %v", c.err, got, c.want)
			}
		})
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
