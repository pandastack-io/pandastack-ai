// SPDX-License-Identifier: Apache-2.0
//
// cpuTiers mechanics against a fake cgroup root — weight clamping, membership
// idempotence, delta scraping (incl. counter-reset), and dead-VM reaping.
package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func fakeTiers(t *testing.T) *cpuTiers {
	t.Helper()
	root := t.TempDir()
	ct := newCPUTiers(root)
	ct.svcDir = filepath.Join(root, "system.slice", "agent.service")
	if err := os.MkdirAll(ct.svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ct.ready.Store(true)
	return ct
}

func writeCPUStat(t *testing.T, ct *cpuTiers, id string, usec uint64) {
	t.Helper()
	dir := filepath.Join(ct.svcDir, vmCgroupPrefix+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "usage_usec " + strconv.FormatUint(usec, 10) + "\nuser_usec 1\nsystem_usec 1\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureVM_WeightClampAndMembership(t *testing.T) {
	ct := fakeTiers(t)
	cases := []struct {
		vcpus, want int
	}{
		{0, cpuWeightMin},   // unknown → floor
		{2, 200},            // standard tier
		{8, 800},            // burst tier
		{500, cpuWeightMax}, // absurd → ceiling
	}
	for _, c := range cases {
		id := "sb-" + strconv.Itoa(c.vcpus)
		if err := ct.ensureVM(id, 4242, c.vcpus); err != nil {
			t.Fatalf("ensureVM(%d vcpus): %v", c.vcpus, err)
		}
		b, err := os.ReadFile(filepath.Join(ct.svcDir, vmCgroupPrefix+id, "cpu.weight"))
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := strconv.Atoi(string(b)); got != c.want {
			t.Errorf("vcpus=%d: weight=%d want %d", c.vcpus, got, c.want)
		}
		procs, err := os.ReadFile(filepath.Join(ct.svcDir, vmCgroupPrefix+id, cgroupProcsFile))
		if err != nil || string(procs) != "4242" {
			t.Errorf("vcpus=%d: procs=%q err=%v, want pid written", c.vcpus, procs, err)
		}
	}
	// Idempotent second call: pid already a member → procs file NOT rewritten
	// (readProcs sees 4242 and ensureVM returns before the write).
	id := "sb-2"
	marker := filepath.Join(ct.svcDir, vmCgroupPrefix+id, cgroupProcsFile)
	if err := os.WriteFile(marker, []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ct.ensureVM(id, 4242, 2); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(marker)
	if string(b) != "4242\n" {
		t.Errorf("membership not idempotent: procs=%q", b)
	}
}

func TestScrapeCPU_DeltaAndReset(t *testing.T) {
	ct := fakeTiers(t)
	writeCPUStat(t, ct, "sb1", 1_000_000) // 1s consumed
	if d, ok := ct.scrapeCPU("sb1"); !ok || d != 0 {
		t.Fatalf("first sight must prime baseline and return 0, got %v ok=%v", d, ok)
	}
	writeCPUStat(t, ct, "sb1", 3_500_000) // +2.5s
	if d, ok := ct.scrapeCPU("sb1"); !ok || d != 2.5 {
		t.Fatalf("delta: got %v ok=%v, want 2.5", d, ok)
	}
	if got := ct.totalSec["sb1"]; got != 2.5 {
		t.Fatalf("total: got %v want 2.5", got)
	}
	// Counter reset (VM replaced under same id) → no negative delta.
	writeCPUStat(t, ct, "sb1", 200_000)
	if d, ok := ct.scrapeCPU("sb1"); !ok || d != 0 {
		t.Fatalf("reset must yield 0 delta, got %v ok=%v", d, ok)
	}
	if got := ct.totalSec["sb1"]; got != 2.5 {
		t.Fatalf("total after reset: got %v want unchanged 2.5", got)
	}
}

func TestReapDead(t *testing.T) {
	ct := fakeTiers(t)
	writeCPUStat(t, ct, "live1", 1)
	writeCPUStat(t, ct, "dead1", 1)
	ct.scrapeCPU("dead1") // prime state that reap must clean up
	// cgroup rmdir only works on empty dirs; empty the dead one like a real
	// exited VM (cpu.stat is kernel-virtual; in the fake it's a plain file).
	os.Remove(filepath.Join(ct.svcDir, vmCgroupPrefix+"dead1", "cpu.stat"))
	ct.reapDead(map[string]struct{}{"live1": {}})
	if _, err := os.Stat(filepath.Join(ct.svcDir, vmCgroupPrefix+"dead1")); !os.IsNotExist(err) {
		t.Errorf("dead1 cgroup not reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ct.svcDir, vmCgroupPrefix+"live1")); err != nil {
		t.Errorf("live1 cgroup wrongly reaped: %v", err)
	}
	if _, ok := ct.lastUsec["dead1"]; ok {
		t.Error("dead1 scrape state not cleaned")
	}
}

func TestReadCPUStatUsage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cpu.stat")
	os.WriteFile(p, []byte("usage_usec 123456\nuser_usec 100\n"), 0o644)
	v, err := readCPUStatUsage(p)
	if err != nil || v != 123456 {
		t.Fatalf("got %d err=%v", v, err)
	}
	os.WriteFile(p, []byte("user_usec 100\n"), 0o644)
	if _, err := readCPUStatUsage(p); err == nil {
		t.Fatal("missing usage_usec must error")
	}
}

func TestParseResidentKB(t *testing.T) {
	content := "Rss:              3376 kB\nPss:              1403 kB\nShared_Hugetlb:      0 kB\nPrivate_Hugetlb: 299008 kB\nSwap:                0 kB\n"
	if got := parseResidentKB(content); got != 3376+299008 {
		t.Fatalf("got %d want %d (Rss+hugetlb)", got, 3376+299008)
	}
	if got := parseResidentKB("garbage\n"); got != 0 {
		t.Fatalf("garbage: got %d want 0", got)
	}
}
