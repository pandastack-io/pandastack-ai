// SPDX-License-Identifier: Apache-2.0
//
// Tests for the sandbox_metrics historical-chart sampler in ch_sink.go —
// specifically the delta math that turns cgroup cpu.stat usage_usec into
// a CPU% number, and the "first sight primes the baseline" rule that keeps
// a fresh VM from emitting a bogus 0 on its first tick.
//
// The rest of the sampler pipeline (cgroup file reads, ClickHouse insert) is
// covered by cputiers_test.go + the CHSink interface; here we exercise ONLY
// the metricsPollState state machine because it has all the arithmetic that
// can silently produce wrong dashboard values if the formula is off.
package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeltaCPUPct_FirstSightPrimesBaseline(t *testing.T) {
	s := newMetricsPollState()
	now := time.Now()
	// First sight: no baseline yet, so the sampler must SKIP emitting a
	// point (returned `primed=false`) rather than lie with 0. Emitting 0
	// would pull the chart line down every time a VM starts.
	if _, primed := s.deltaCPUPct("vm1", 1_000_000, now, 8); primed {
		t.Fatal("first sight must return primed=false so caller skips emit")
	}
	// Verify state was recorded.
	if s.lastUsec["vm1"] != 1_000_000 || s.lastAt["vm1"].IsZero() {
		t.Fatalf("baseline not recorded: usec=%d at=%v", s.lastUsec["vm1"], s.lastAt["vm1"])
	}
}

func TestDeltaCPUPct_OneCoreFullyBusy_EightVCPUs(t *testing.T) {
	s := newMetricsPollState()
	t0 := time.Unix(1_700_000_000, 0)
	// Prime.
	s.deltaCPUPct("vm1", 0, t0, 8)
	// 15s later: VM burned exactly 15s of CPU on one core = 15_000_000 usec.
	// On an 8-vCPU box that's 1/8 of capacity = 12.5%.
	pct, primed := s.deltaCPUPct("vm1", 15_000_000, t0.Add(15*time.Second), 8)
	if !primed {
		t.Fatal("second call must be primed")
	}
	if pct < 12.4 || pct > 12.6 {
		t.Fatalf("one core fully busy on 8vcpu = 12.5%%, got %.3f", pct)
	}
}

func TestDeltaCPUPct_AllEightCoresFullyBusy(t *testing.T) {
	s := newMetricsPollState()
	t0 := time.Unix(1_700_000_000, 0)
	s.deltaCPUPct("vm1", 0, t0, 8)
	// 15s of wall × 8 cores fully burned = 120s of CPU = 120_000_000 usec.
	// That's 100% of allocated compute.
	pct, primed := s.deltaCPUPct("vm1", 120_000_000, t0.Add(15*time.Second), 8)
	if !primed {
		t.Fatal("must be primed")
	}
	if pct < 99.5 || pct > 100.5 {
		t.Fatalf("8/8 cores = 100%%, got %.3f", pct)
	}
}

func TestDeltaCPUPct_IdleReturnsZero(t *testing.T) {
	s := newMetricsPollState()
	t0 := time.Unix(1_700_000_000, 0)
	s.deltaCPUPct("vm1", 5_000_000, t0, 8)
	// No new CPU consumed.
	pct, primed := s.deltaCPUPct("vm1", 5_000_000, t0.Add(15*time.Second), 8)
	if !primed || pct != 0 {
		t.Fatalf("idle should be 0%%, got primed=%v pct=%.3f", primed, pct)
	}
}

func TestDeltaCPUPct_CounterResetTreatedAsFirstSight(t *testing.T) {
	// VM replaced with same id (fresh cgroup, counter starts at 0 again).
	// The old baseline was 5_000_000; a new reading of 500_000 would produce
	// a hugely negative delta if the code trusted it. It must instead reset
	// the baseline and skip emit.
	s := newMetricsPollState()
	t0 := time.Unix(1_700_000_000, 0)
	s.deltaCPUPct("vm1", 5_000_000, t0, 8)
	pct, primed := s.deltaCPUPct("vm1", 500_000, t0.Add(15*time.Second), 8)
	if primed {
		t.Fatalf("counter reset must skip emit (primed=false), got primed=true pct=%.3f", pct)
	}
	if s.lastUsec["vm1"] != 500_000 {
		t.Fatalf("baseline should be updated to the new low value, got %d", s.lastUsec["vm1"])
	}
}

func TestDeltaCPUPct_ClampsSchedulerJitter(t *testing.T) {
	// Very briefly, cpu.stat usec_delta can outrun wallclock delta by a hair
	// on a heavily-scheduled system — the reported CPU% shouldn't be 500%,
	// which would blow the chart's y-axis. Cap at 200 so occasional jitter is
	// visible-but-bounded.
	s := newMetricsPollState()
	t0 := time.Unix(1_700_000_000, 0)
	s.deltaCPUPct("vm1", 0, t0, 8)
	// 1s wall, 100s of CPU claimed. Absurd, would compute (100/1/8)*100 = 1250%.
	pct, primed := s.deltaCPUPct("vm1", 100_000_000, t0.Add(1*time.Second), 8)
	if !primed {
		t.Fatal("must be primed")
	}
	if pct > 200 {
		t.Fatalf("clamp should cap at 200, got %.3f", pct)
	}
}

func TestDeltaCPUPct_ZeroVCPUsSafe(t *testing.T) {
	// A malformed sandbox row (cpu=0) must not divide by zero. Default to 1.
	s := newMetricsPollState()
	t0 := time.Unix(1_700_000_000, 0)
	s.deltaCPUPct("vm1", 0, t0, 0)
	pct, primed := s.deltaCPUPct("vm1", 500_000, t0.Add(1*time.Second), 0)
	if !primed || pct <= 0 {
		t.Fatalf("expected non-zero CPU with default vcpu fallback, got primed=%v pct=%.3f", primed, pct)
	}
}

// writeCgroupFixture writes memory.current (+ optional memory.stat) into a
// temp dir shaped like a cgroup-v2 vm-<id> directory.
func writeCgroupFixture(t *testing.T, current string, stat string) string {
	t.Helper()
	dir := t.TempDir()
	if current != "" {
		if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte(current), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if stat != "" {
		if err := os.WriteFile(filepath.Join(dir, "memory.stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestReadWorkingSet_SubtractsInactiveFile(t *testing.T) {
	// The live incident shape: 1 GiB guest + ~1.9 GB page cache from temp-file
	// I/O. Working set must report ~1 GiB, not ~3 GB.
	dir := writeCgroupFixture(t,
		"3000000000\n",
		"anon 900000000\nfile 2000000000\ninactive_file 1900000000\nactive_file 100000000\n")
	got, ok := readWorkingSetBytes(dir)
	if !ok || got != 1100000000 {
		t.Fatalf("want (1100000000,true), got (%d,%v)", got, ok)
	}
}

func TestReadWorkingSet_InactiveExceedsCurrent_FloorsAtZero(t *testing.T) {
	// stat and current are read at slightly different instants; a race can
	// make inactive_file momentarily exceed current. Never go negative.
	dir := writeCgroupFixture(t, "1000\n", "inactive_file 5000\n")
	got, ok := readWorkingSetBytes(dir)
	if !ok || got != 0 {
		t.Fatalf("want (0,true), got (%d,%v)", got, ok)
	}
}

func TestReadWorkingSet_MissingStat_DegradesToCurrent(t *testing.T) {
	// No memory.stat → serve raw memory.current (slightly inflated beats
	// nothing), still ok=true.
	dir := writeCgroupFixture(t, "123456\n", "")
	got, ok := readWorkingSetBytes(dir)
	if !ok || got != 123456 {
		t.Fatalf("want (123456,true), got (%d,%v)", got, ok)
	}
}

func TestReadWorkingSet_MissingCurrent_NotOK(t *testing.T) {
	// No memory.current at all (cgroup not set up) → ok=false so the caller
	// falls back to the baked tier cap.
	dir := writeCgroupFixture(t, "", "inactive_file 5\n")
	if _, ok := readWorkingSetBytes(dir); ok {
		t.Fatal("want ok=false when memory.current is unreadable")
	}
}

func TestReadWorkingSet_StatWithoutInactiveLine(t *testing.T) {
	// memory.stat present but no inactive_file line (controller variations):
	// degrade to raw current.
	dir := writeCgroupFixture(t, "777\n", "anon 1\nfile 2\n")
	got, ok := readWorkingSetBytes(dir)
	if !ok || got != 777 {
		t.Fatalf("want (777,true), got (%d,%v)", got, ok)
	}
}

func TestForget_DropsDeadVMs(t *testing.T) {
	// State grows with each VM the poller ever saw. When VMs die, the state
	// map must shrink or a long-lived agent's memory footprint climbs
	// forever. `forget` gets the LIVE set and drops everything else.
	s := newMetricsPollState()
	now := time.Now()
	s.deltaCPUPct("vm1", 1, now, 8)
	s.deltaCPUPct("vm2", 1, now, 8)
	s.deltaCPUPct("vm3", 1, now, 8)
	live := map[string]struct{}{"vm2": {}} // only vm2 survives
	s.forget(live)
	if _, ok := s.lastUsec["vm1"]; ok {
		t.Fatal("vm1 should be forgotten")
	}
	if _, ok := s.lastUsec["vm2"]; !ok {
		t.Fatal("vm2 should be retained")
	}
	if _, ok := s.lastUsec["vm3"]; ok {
		t.Fatal("vm3 should be forgotten")
	}
	if _, ok := s.lastAt["vm1"]; ok {
		t.Fatal("vm1 lastAt should be forgotten too (parallel map)")
	}
}
