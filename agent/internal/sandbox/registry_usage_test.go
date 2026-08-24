// SPDX-License-Identifier: Apache-2.0
package sandbox

import "testing"

// TestSumLiveSandboxUsage verifies the scheduler-accounting fix: usage is the
// SUM of each running sandbox's REAL baked cpu/memory_mb (not a flat 512 MB),
// only counts sandboxes live on this host, and falls back safely so a running
// sandbox is never invisible to the scheduler.
func TestSumLiveSandboxUsage(t *testing.T) {
	row := func(id string, cpu, mem int) map[string]interface{} {
		return map[string]interface{}{"id": id, "cpu": cpu, "memory_mb": mem}
	}
	liveSet := func(ids ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, id := range ids {
			m[id] = struct{}{}
		}
		return m
	}

	t.Run("heterogeneous mix sums real sizes", func(t *testing.T) {
		// postgres 1GiB/2vCPU + app 2GiB/2vCPU + browser 4GiB/4vCPU = 7GiB / 8 vCPU.
		// The old flat estimate would have reported 3×512MB = 1.5GiB (4.6× under).
		rows := []any{
			row("db", 2, 1024),
			row("app", 2, 2048),
			row("browser", 4, 4096),
		}
		cpu, mem := sumLiveSandboxUsage(rows, liveSet("db", "app", "browser"))
		if cpu != 8 || mem != 7168 {
			t.Fatalf("got cpu=%d mem=%d, want cpu=8 mem=7168", cpu, mem)
		}
	})

	t.Run("only counts sandboxes live on this host", func(t *testing.T) {
		// "remote" exists in the DB but isn't running here → must be excluded.
		rows := []any{
			row("here", 2, 2048),
			row("remote", 4, 4096),
		}
		cpu, mem := sumLiveSandboxUsage(rows, liveSet("here"))
		if cpu != 2 || mem != 2048 {
			t.Fatalf("got cpu=%d mem=%d, want cpu=2 mem=2048 (remote excluded)", cpu, mem)
		}
	})

	t.Run("missing or zero size falls back to the common baked app/base size", func(t *testing.T) {
		// Fallback is 2 vCPU / 4096 MB (fallbackSandboxCPU/MemMB) — the dominant
		// app/base size — NOT 512 MB, so a mid-create sandbox isn't under-counted.
		rows := []any{
			row("zero", 0, 0),                      // zero size → fallback
			map[string]interface{}{"id": "noSize"}, // missing cpu/mem keys → fallback
		}
		cpu, mem := sumLiveSandboxUsage(rows, liveSet("zero", "noSize"))
		if cpu != 2*fallbackSandboxCPU || mem != 2*fallbackSandboxMemMB {
			t.Fatalf("got cpu=%d mem=%d, want cpu=%d mem=%d (2× fallback)",
				cpu, mem, 2*fallbackSandboxCPU, 2*fallbackSandboxMemMB)
		}
	})

	t.Run("live driver with no matching row is charged the fallback", func(t *testing.T) {
		// "ghost" is live (mid-create) but has no DB row yet → never invisible,
		// charged the common baked size (the OOM-safe over-estimate).
		rows := []any{row("real", 2, 2048)}
		cpu, mem := sumLiveSandboxUsage(rows, liveSet("real", "ghost"))
		wantCPU, wantMem := 2+fallbackSandboxCPU, 2048+fallbackSandboxMemMB
		if cpu != wantCPU || mem != wantMem {
			t.Fatalf("got cpu=%d mem=%d, want cpu=%d mem=%d", cpu, mem, wantCPU, wantMem)
		}
	})

	t.Run("no live sandboxes is zero usage", func(t *testing.T) {
		cpu, mem := sumLiveSandboxUsage([]any{row("x", 2, 2048)}, liveSet())
		if cpu != 0 || mem != 0 {
			t.Fatalf("got cpu=%d mem=%d, want 0/0", cpu, mem)
		}
	})
}
