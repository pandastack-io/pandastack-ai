// SPDX-License-Identifier: Apache-2.0
//
// Regression tests for burst placement (in-flight reservations).
//
// Incident 2026-08-22: five sandbox creates arrived within one second and all
// five landed on the same agent — load average 40 on an 8-core host — while a
// second, healthy, completely idle agent sat at load 0.08. The scoring rule was
// fine; the inputs were stale. Pick() scores against a capacity snapshot that
// lags reality (30s scheduler cache on top of a 10s agent heartbeat), so every
// create in a burst saw identical numbers and chose the same "best" host.
//
// These tests pin the fix: a Pick reserves the capacity it just handed out, so
// the next Pick in the burst sees it.
package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// twoIdenticalAgents seeds the cache with two agents that are exactly
// equivalent — same capacity, same streaming support, both idle. Under the old
// code every Pick scored them identically and returned the same one forever.
func twoIdenticalAgents(t *testing.T) *Scheduler {
	t.Helper()
	s := New(nil, time.Hour) // nil db: cache is seeded directly
	now := time.Now()
	s.mu.Lock()
	s.cache = []Agent{
		{
			ID: "agent-a", Status: "active",
			Capacity:      Capacity{CPUTotal: 8, MemoryMB: 32090, StreamRestoreEnabled: true},
			LastHeartbeat: now,
		},
		{
			ID: "agent-b", Status: "active",
			Capacity:      Capacity{CPUTotal: 8, MemoryMB: 32090, StreamRestoreEnabled: true},
			LastHeartbeat: now,
		},
	}
	s.cachedAt = now
	s.mu.Unlock()
	return s
}

// TestBurstSpreadsAcrossAgents is the direct regression for the incident: five
// UNSIZED creates (CPU/MemoryMB unset — the common SDK call, since the template
// owns the guest size) must not all land on one agent.
func TestBurstSpreadsAcrossAgents(t *testing.T) {
	s := twoIdenticalAgents(t)
	counts := map[string]int{}
	for i := 0; i < 5; i++ {
		ag, err := s.Pick(context.Background(), Request{})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		counts[ag.ID]++
	}
	if len(counts) < 2 {
		t.Fatalf("all 5 creates landed on one agent (%v) — burst did not spread", counts)
	}
	// With two equivalent agents the split should be near-even; anything worse
	// than 4/1 means reservations are not really biting.
	for id, n := range counts {
		if n > 4 {
			t.Fatalf("agent %s took %d of 5 creates — expected a near-even split, got %v", id, n, counts)
		}
	}
}

// TestBurstSpreadsUnderConcurrency is the same scenario with the Picks racing,
// which is how it actually happened. This is what fails if the reservation is
// taken without holding the lock across scoring+selection: every goroutine
// reads capacity before any of them records a claim.
func TestBurstSpreadsUnderConcurrency(t *testing.T) {
	s := twoIdenticalAgents(t)
	const n = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ag, err := s.Pick(context.Background(), Request{})
			if err != nil {
				return
			}
			mu.Lock()
			counts[ag.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(counts) < 2 {
		t.Fatalf("all %d concurrent creates landed on one agent (%v)", n, counts)
	}
	for id, c := range counts {
		if c > n*3/4 {
			t.Fatalf("agent %s took %d of %d concurrent creates — %v", id, c, n, counts)
		}
	}
}

// TestReservationsExpire proves the claims are not permanent. A reservation
// that outlived its TTL must stop counting, or a failed create would strand
// capacity forever and the agent would look busy indefinitely.
func TestReservationsExpire(t *testing.T) {
	s := twoIdenticalAgents(t)
	// Place one, then age its reservation past the TTL.
	if _, err := s.Pick(context.Background(), Request{}); err != nil {
		t.Fatalf("pick: %v", err)
	}
	s.resMu.Lock()
	total := 0
	for id := range s.reservations {
		for i := range s.reservations[id] {
			s.reservations[id][i].at = time.Now().Add(-reservationTTL - time.Second)
		}
		total += len(s.reservations[id])
	}
	s.resMu.Unlock()
	if total == 0 {
		t.Fatal("expected a reservation to be recorded after Pick")
	}

	s.resMu.Lock()
	s.pruneReservationsLocked(time.Now())
	remaining := len(s.reservations)
	s.resMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired reservations were not pruned: %d agent(s) still hold claims", remaining)
	}
}

// TestUnsizedCreateStillReservesCPU guards the subtlety that makes the fix work
// at all: a create with no declared size must still claim CPU. If this returned
// 0 the mechanism would be inert for the exact call shape that caused the
// incident.
func TestUnsizedCreateStillReservesCPU(t *testing.T) {
	s := twoIdenticalAgents(t)
	ag, err := s.Pick(context.Background(), Request{}) // no CPU / MemoryMB
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	s.resMu.Lock()
	cpu, mem, _ := s.reservedLocked(ag.ID)
	s.resMu.Unlock()
	if cpu != defaultReservedCPU {
		t.Fatalf("unsized create reserved %d CPU, want %d", cpu, defaultReservedCPU)
	}
	// Memory must NOT be defaulted — it is a hard admission gate and guessing
	// high there could turn a burst into spurious "no agents available".
	if mem != 0 {
		t.Fatalf("unsized create reserved %d MB memory, want 0 (memory must not be guessed)", mem)
	}
}

// TestSizedCreateReservesExactRequest confirms a declared size is honoured
// verbatim rather than replaced by the default.
func TestSizedCreateReservesExactRequest(t *testing.T) {
	s := twoIdenticalAgents(t)
	ag, err := s.Pick(context.Background(), Request{CPU: 2, MemoryMB: 4096})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	s.resMu.Lock()
	cpu, mem, _ := s.reservedLocked(ag.ID)
	s.resMu.Unlock()
	if cpu != 2 || mem != 4096 {
		t.Fatalf("reserved cpu=%d mem=%d, want cpu=2 mem=4096", cpu, mem)
	}
}

// TestSingleAgentStillPlaces is the anti-over-correction check: with only one
// agent, reservations must never starve it into returning ErrNoAgents for
// unsized creates. Spreading is a preference, not an admission gate.
func TestSingleAgentStillPlaces(t *testing.T) {
	s := New(nil, time.Hour)
	now := time.Now()
	s.mu.Lock()
	s.cache = []Agent{{
		ID: "only", Status: "active",
		Capacity:      Capacity{CPUTotal: 8, MemoryMB: 32090, StreamRestoreEnabled: true},
		LastHeartbeat: now,
	}}
	s.cachedAt = now
	s.mu.Unlock()

	for i := 0; i < 10; i++ {
		ag, err := s.Pick(context.Background(), Request{})
		if err != nil {
			t.Fatalf("pick %d on a single healthy agent failed: %v", i, err)
		}
		if ag.ID != "only" {
			t.Fatalf("unexpected agent %s", ag.ID)
		}
	}
}

// TestIncident20260822Replica reproduces the live incident with the capacity
// numbers actually observed on the fleet, rather than a synthetic pair. At
// 16:11:08 two agents were up:
//
//	bddt — 8 vCPU / 32090 MB, streaming, already running pogoda-bot (8 vCPU baked)
//	30ph — 8 vCPU / 32090 MB, streaming, idle
//
// Five `code-interpreter` creates arrived inside one second, all unsized (the
// template owns the guest size), and all five landed on bddt — load average 40
// on 8 cores — while 30ph sat at load 0.08.
func TestIncident20260822Replica(t *testing.T) {
	s := New(nil, time.Hour)
	now := time.Now()
	s.mu.Lock()
	s.cache = []Agent{
		{
			ID: "bddt", Status: "active",
			// pogoda-bot already resident: 8 baked vCPU, ~4 GiB.
			Capacity: Capacity{
				CPUTotal: 8, CPUUsed: 8,
				MemoryMB: 32090, MemoryUsed: 4096,
				StreamRestoreEnabled: true,
			},
			LastHeartbeat: now,
		},
		{
			ID: "30ph", Status: "active",
			Capacity: Capacity{
				CPUTotal: 8, CPUUsed: 0,
				MemoryMB: 32090, MemoryUsed: 0,
				StreamRestoreEnabled: true,
			},
			LastHeartbeat: now,
		},
	}
	s.cachedAt = now
	s.mu.Unlock()

	counts := map[string]int{}
	for i := 0; i < 5; i++ {
		ag, err := s.Pick(context.Background(), Request{}) // unsized, as in prod
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		counts[ag.ID]++
	}
	t.Logf("placement split across the 5-create burst: %v", counts)

	if counts["bddt"] == 5 {
		t.Fatal("all 5 landed on bddt — this is the original incident, unfixed")
	}
	// The idle host must take the larger share: it started with 8 free vCPU
	// against bddt's 0.
	if counts["30ph"] <= counts["bddt"] {
		t.Fatalf("idle agent should absorb most of the burst, got %v", counts)
	}
}
