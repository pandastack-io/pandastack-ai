// SPDX-License-Identifier: Apache-2.0
package sandbox

import "testing"

// TestTrackedUsage_NotTrackedUntilAWindowIsIntegrated pins the fix for a live
// accounting leak. The tiers reconciler primes its baselines on the first sight of
// a sandbox and integrates nothing; only the SECOND scrape produces a real
// measurement. The tracked-usage accessors used to key "am I tracking this?" on
// those baselines, so after exactly one 15s scrape they answered
// (0 seconds, tracked). Callers trust `tracked` over the committed figure, so
// every sandbox that lived less than two scrapes reported zero utilization —
// that is every short invocation, plus the opening window of every long-lived
// sandbox.
func TestTrackedUsage_NotTrackedUntilAWindowIsIntegrated(t *testing.T) {
	m := &Manager{cpuTiers: newTestCPUTiers()}
	const id = "sb-fresh"

	// State after ONE scrape: baselines primed, nothing accumulated.
	m.cpuTiers.lastUsec[id] = 12345
	m.cpuTiers.lastResidAt[id] = testNow()

	if _, ok := m.activeCPUTracked(id); ok {
		t.Fatal("activeCPUTracked said tracked after a single scrape — the caller " +
			"would read 0 CPU-seconds as a real measurement")
	}
	if _, ok := m.residentGiBTracked(id); ok {
		t.Fatal("residentGiBTracked said tracked after a single scrape — same flaw for memory")
	}

	// State after a SECOND scrape: a real window has been integrated. Even when
	// the VM burned nothing, that is now a measurement and must be trusted.
	m.cpuTiers.totalSec[id] = 0
	m.cpuTiers.residGiBSec[id] = 0

	if v, ok := m.activeCPUTracked(id); !ok || v != 0 {
		t.Fatalf("activeCPUTracked = (%v, %v), want (0, true) once a window exists", v, ok)
	}
	if v, ok := m.residentGiBTracked(id); !ok || v != 0 {
		t.Fatalf("residentGiBTracked = (%v, %v), want (0, true) once a window exists", v, ok)
	}

	// And a real measurement flows through.
	m.cpuTiers.totalSec[id] = 42.5
	if v, ok := m.activeCPUTracked(id); !ok || v != 42.5 {
		t.Fatalf("activeCPUTracked = (%v, %v), want (42.5, true)", v, ok)
	}
}
