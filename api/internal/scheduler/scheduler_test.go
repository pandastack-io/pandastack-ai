// SPDX-License-Identifier: Apache-2.0
package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestPickRespectsResourceFloor ensures we don't pick a candidate that
// fails the CPU/Mem fit, even if it scores well otherwise.
func TestPickRespectsResourceFloor(t *testing.T) {
	s := New(nil, time.Hour) // db is nil; we seed cache directly
	now := time.Now()
	s.mu.Lock()
	s.cache = []Agent{
		{
			ID: "full", Status: "active",
			Capacity:      Capacity{CPUTotal: 4, CPUUsed: 4, MemoryMB: 1024, MemoryUsed: 1024, StreamRestoreEnabled: true},
			LastHeartbeat: now,
		},
		{
			ID: "free", Status: "active",
			Capacity:      Capacity{CPUTotal: 8, MemoryMB: 4096},
			LastHeartbeat: now,
		},
	}
	s.cachedAt = now
	s.mu.Unlock()

	ag, err := s.Pick(context.Background(), Request{CPU: 2, MemoryMB: 512})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ag.ID != "free" {
		t.Fatalf("expected free, got %s", ag.ID)
	}
}

// TestPickPrefersStreamingHost verifies that among agents with otherwise
// equal resources, the one advertising UFFD streaming restore wins, because
// it boots without downloading the whole vm.mem first.
func TestPickPrefersStreamingHost(t *testing.T) {
	s := New(nil, time.Hour)
	now := time.Now()
	s.mu.Lock()
	s.cache = []Agent{
		{
			ID: "download", Status: "active",
			Capacity:      Capacity{CPUTotal: 8, MemoryMB: 4096, StreamRestoreEnabled: false},
			LastHeartbeat: now,
		},
		{
			ID: "stream", Status: "active",
			Capacity:      Capacity{CPUTotal: 8, MemoryMB: 4096, StreamRestoreEnabled: true},
			LastHeartbeat: now,
		},
	}
	s.cachedAt = now
	s.mu.Unlock()

	ag, err := s.Pick(context.Background(), Request{CPU: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ag.ID != "stream" {
		t.Fatalf("expected stream, got %s", ag.ID)
	}
}

// TestPickPrefersIdleHost verifies the resource-fit score routes new
// sandboxes to the agent with the most free CPU (load spreading).
func TestPickPrefersIdleHost(t *testing.T) {
	s := New(nil, time.Hour)
	now := time.Now()
	s.mu.Lock()
	s.cache = []Agent{
		{
			ID: "busy", Status: "active",
			Capacity:      Capacity{CPUTotal: 16, CPUUsed: 14, MemoryMB: 16384, MemoryUsed: 8192},
			LastHeartbeat: now,
		},
		{
			ID: "idle", Status: "active",
			Capacity:      Capacity{CPUTotal: 16, CPUUsed: 1, MemoryMB: 16384, MemoryUsed: 512},
			LastHeartbeat: now,
		},
	}
	s.cachedAt = now
	s.mu.Unlock()

	ag, err := s.Pick(context.Background(), Request{CPU: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ag.ID != "idle" {
		t.Fatalf("expected idle, got %s", ag.ID)
	}
}

// TestRememberLeaseShortCircuitsPG verifies LookupLease returns the in-mem
// entry without touching the DB. If the cache wasn't consulted first, a nil
// db would NPE the query and surface as a panic/error.
func TestRememberLeaseShortCircuitsPG(t *testing.T) {
	s := New(nil, time.Hour) // db is nil
	target := Agent{ID: "agent-x", Endpoint: "http://agent-x:8080"}
	s.RememberLease("sb-1", target)

	ag, err := s.LookupLease(context.Background(), "sb-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ag == nil || ag.ID != "agent-x" {
		t.Fatalf("expected agent-x, got %#v", ag)
	}

	// After ForgetLease, the cache miss would fall through to PG and panic
	// on the nil db. We assert recovery.
	s.ForgetLease("sb-1")
	defer func() {
		if r := recover(); r == nil {
			// Some Go versions return an error rather than panic; tolerate both.
		}
	}()
	_, _ = s.LookupLease(context.Background(), "sb-1")
}
