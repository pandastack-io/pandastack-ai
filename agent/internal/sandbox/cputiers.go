// SPDX-License-Identifier: Apache-2.0
//
// CPU burst-to-physical via cgroup v2 cpu.weight tiers,
// plus the per-sandbox active-CPU accounting that feeds utilization signals.
//
// Design: a 15s reconciler loop (not per-boot-path wiring — Create, warm-fork,
// wake, restore, and Recover all converge on m.drivers, so one loop catches
// every path and self-heals after agent restarts). Each live Firecracker
// process is placed in its own child cgroup under the agent's DELEGATED service
// cgroup with cpu.weight proportional to its vCPU entitlement:
//
//	weight = 100 × vCPUs   (2-vCPU sandbox → 200, 8-vCPU → 800)
//
// Semantics (Linux CFS proportional shares — this is the whole feature):
//   - idle host → ANY sandbox bursts to all physical cores (weights only bind
//     under contention), which is the "burst to physical CPU" product promise;
//   - contention → cores divide in weight proportion, so an 8-vCPU sandbox
//     genuinely outweighs a 2-vCPU one instead of a thread-count lottery.
//
// The same loop scrapes each VM cgroup's cpu.stat usage_usec into
// obs.SandboxCPUSeconds (the :9100 metrics endpoint) and an in-memory
// per-sandbox counter exposed via Manager.ActiveCPUSeconds — the seam
// utilization consumers such as GET /sandboxes/{id}/util read from.
//
// cgroup-v2 delegation dance (no-internal-process rule): before the service
// cgroup can gain child cgroups with controllers, its own processes must move
// into a leaf. init() therefore creates <svc>/agent/, moves all current pids
// there, then writes "+cpu" to <svc>/cgroup.subtree_control. Requires
// Delegate=yes on the agent unit (cloud-init sets it; without it systemd may
// fight the subtree and init degrades to a one-time warning).
//
// Failure posture: STRICTLY best-effort. Any cgroup error disables the loop
// with one warning — sandboxes run exactly as today (agent service cgroup,
// unweighted). Kill switch: PANDASTACK_CPU_TIERS=0.
package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pandastack/agent/internal/obs"
)

const (
	cpuTiersTick      = 15 * time.Second
	cpuWeightPerVCPU  = 100
	cpuWeightMin      = 100   // never below cgroup default
	cpuWeightMax      = 10000 // cgroup v2 cpu.weight ceiling
	vmCgroupPrefix    = "vm-"
	agentLeafName     = "agent"
	cgroupProcsFile   = "cgroup.procs"
	cpuTiersEnableEnv = "PANDASTACK_CPU_TIERS"
)

func cpuTiersEnabled() bool { return os.Getenv(cpuTiersEnableEnv) != "0" }

// cpuTiers holds the reconciler state. root is parameterized for tests.
type cpuTiers struct {
	root   string // cgroup2 mountpoint, "/sys/fs/cgroup" in prod
	svcDir string // absolute path of the agent's delegated service cgroup
	ready  atomic.Bool
	warned bool

	// memDelegated: +memory made it into cgroup.subtree_control — vm-<id>
	// cgroups have memory.current/high/max (the squeeze substrate, T2.2).
	memDelegated bool

	shadowTick int // reconcile passes since start (shadow log cadence)

	mu       sync.Mutex
	lastUsec map[string]uint64  // sandbox id -> last cpu.stat usage_usec seen
	totalSec map[string]float64 // sandbox id -> accumulated active CPU seconds
	// residGiBSec accumulates resident-memory GiB-seconds per sandbox
	// (resident bytes x seconds-since-last-scrape, integrated at each 15s
	// reconcile) — the working-set RAM utilization signal.
	residGiBSec map[string]float64
	lastResidAt map[string]time.Time
}

func newCPUTiers(root string) *cpuTiers {
	return &cpuTiers{
		root: root, lastUsec: map[string]uint64{}, totalSec: map[string]float64{},
		residGiBSec: map[string]float64{}, lastResidAt: map[string]time.Time{},
	}
}

// ActiveCPUSeconds returns the accumulated active-CPU seconds observed for a
// sandbox since the agent started tracking it (0 if unknown). This is the
// util-signal seam for scale-out triggers and capacity reporting.
func (m *Manager) ActiveCPUSeconds(id string) float64 {
	sec, _ := m.activeCPUTracked(id)
	return sec
}

// residentGiBTracked reports the accumulated resident GiB-seconds for id
// (the working-set RAM signal). ok=false when the tiers reconciler has never
// integrated a window for it — callers fall back to committed memory.
func (m *Manager) residentGiBTracked(id string) (float64, bool) {
	if m.cpuTiers == nil {
		return 0, false
	}
	m.cpuTiers.mu.Lock()
	defer m.cpuTiers.mu.Unlock()
	// Keyed on the ACCUMULATOR, not on lastResidAt. lastResidAt is set by the
	// very first scrape, which only primes the baseline and integrates nothing
	// — so keying on it reported "tracked: 0 GiB-seconds" for a VM that had
	// simply not been measured twice yet, and callers then believed a real
	// zero. residGiBSec only gains a key once a real window has been
	// integrated, which is exactly the condition callers need.
	if _, integrated := m.cpuTiers.residGiBSec[id]; !integrated {
		return 0, false
	}
	return m.cpuTiers.residGiBSec[id], true
}

// activeCPUTracked reports the accumulated active-CPU seconds for id and
// whether the tiers reconciler is actually tracking its cgroup. ok=false
// (tiers disabled, init never succeeded, or the VM was never scraped) means
// callers MUST fall back to committed accounting — treating "untracked" as
// "burned zero" understates a busy host's real load.
func (m *Manager) activeCPUTracked(id string) (float64, bool) {
	if m.cpuTiers == nil {
		return 0, false
	}
	m.cpuTiers.mu.Lock()
	defer m.cpuTiers.mu.Unlock()
	// Keyed on totalSec (the accumulator), NOT lastUsec (the baseline).
	// scrapeCPU primes lastUsec on first sight and returns without touching
	// totalSec, so keying on lastUsec answered "tracked: 0 seconds" after a
	// single scrape — free compute for anything that lived less than two
	// 15-second ticks, which is every function invocation. A genuinely idle
	// VM still reports (0, true) once a window has been integrated, because
	// `totalSec[id] += 0` creates the key.
	if _, integrated := m.cpuTiers.totalSec[id]; !integrated {
		return 0, false
	}
	return m.cpuTiers.totalSec[id], true
}

// runCPUTiers is the loop entrypoint (started from NewManager alongside the
// other background loops).
func (m *Manager) runCPUTiers() {
	if !cpuTiersEnabled() {
		m.log.Info("cpu tiers: disabled via PANDASTACK_CPU_TIERS=0")
		return
	}
	ct := m.cpuTiers // published synchronously in NewManager (happens-before the goroutines)
	if ct == nil {
		return
	}
	warnedInit := false
	t := time.NewTicker(cpuTiersTick)
	defer t.Stop()
	for range t.C {
		if !ct.ready.Load() {
			// init races the agent's own startup exec storm (helpers landing in
			// the service root make +cpu return EBUSY) — retry each tick until
			// the root drains rather than giving up forever on boot.
			if err := ct.init(); err != nil {
				if !warnedInit {
					warnedInit = true
					m.log.Warn("cpu tiers: cgroup delegation not ready yet (will retry)", "err", err)
				}
				continue
			}
			m.log.Info("cpu tiers: enabled", "svc_cgroup", ct.svcDir, "memory_delegated", ct.memDelegated)
		}
		m.cpuTiersReconcile(ct)
	}
}

// init prepares the delegated subtree: <svc>/agent leaf for our own pids, then
// +cpu in <svc>/cgroup.subtree_control.
func (ct *cpuTiers) init() error {
	if _, err := os.Stat(filepath.Join(ct.root, "cgroup.controllers")); err != nil {
		return fmt.Errorf("cgroup v2 not mounted at %s: %w", ct.root, err)
	}
	svc, err := ownCgroupDir(ct.root)
	if err != nil {
		return err
	}
	ct.svcDir = svc

	// Move every current process of the service cgroup into the agent/ leaf.
	leaf := filepath.Join(svc, agentLeafName)
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", leaf, err)
	}
	// Evacuate the service root repeatedly: the agent concurrently exec's
	// helpers (seed-sync, gcloud) that land in the root between passes, and
	// cgroup.subtree_control returns EBUSY while ANY process remains (the
	// no-internal-process rule). Bounded passes; the caller retries next tick
	// if the root still won't drain.
	for pass := 0; pass < 5; pass++ {
		pids, err := readProcs(filepath.Join(svc, cgroupProcsFile))
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			break
		}
		for _, pid := range pids {
			_ = os.WriteFile(filepath.Join(leaf, cgroupProcsFile), []byte(strconv.Itoa(pid)), 0o644)
		}
	}
	if err := os.WriteFile(filepath.Join(svc, "cgroup.subtree_control"), []byte("+cpu"), 0o644); err != nil {
		return fmt.Errorf("enable +cpu in %s: %w", svc, err)
	}
	// Memory controller too: gives every vm-<id> cgroup
	// memory.current/high/max so the squeezable (no-hugepage) class can be
	// accounted and squeezed. Enabled separately and best-effort — a kernel
	// without the memory controller on this subtree must not take down CPU
	// tiers, which work on their own. Outcome is surfaced by the loop's
	// "enabled" log line via memDelegated.
	ct.memDelegated = os.WriteFile(filepath.Join(svc, "cgroup.subtree_control"), []byte("+memory"), 0o644) == nil
	ct.ready.Store(true)
	return nil
}

// cpuTiersReconcile does one pass: ensure each live driver's pid sits in its
// weighted cgroup, scrape cpu.stat, and reap cgroups of dead VMs.
func (m *Manager) cpuTiersReconcile(ct *cpuTiers) {
	if !ct.ready.Load() {
		return
	}
	// Snapshot live (id, pid, vcpus) under the manager lock.
	type vm struct {
		id   string
		pid  int
		vcpu int
	}
	var vms []vm
	m.mu.RLock()
	for id, drv := range m.drivers {
		if drv == nil {
			continue
		}
		pid := drv.PID()
		if pid <= 0 {
			continue
		}
		vms = append(vms, vm{id: id, pid: pid, vcpu: 0})
	}
	m.mu.RUnlock()

	live := make(map[string]struct{}, len(vms))
	var committedMB, residentMB int64
	for i := range vms {
		v := &vms[i]
		live[v.id] = struct{}{}
		var memMB int
		v.vcpu, memMB = m.sandboxSpec(v.id)
		committedMB += int64(memMB)
		if err := ct.ensureVM(v.id, v.pid, v.vcpu); err != nil {
			if !ct.warned {
				ct.warned = true
				m.log.Warn("cpu tiers: ensure failed (will keep retrying quietly)",
					"sandbox", v.id, "err", err)
			}
			continue
		}
		if sec, ok := ct.scrapeCPU(v.id); ok {
			obs.SandboxCPUSeconds.WithLabelValues(v.id).Add(sec)
		}
		// Measured residency = Rss + hugetlb from
		// smaps_rollup — the working-set truth admission will one day use.
		// Hugetlb terms are load-bearing: guest RAM is hugetlb on this fleet
		// and invisible to plain Rss (T0.1/T0.3 findings).
		if rb, err := readResidentBytes(v.pid); err == nil {
			residentMB += rb / (1 << 20)
			obs.SandboxResidentBytes.WithLabelValues(v.id).Set(float64(rb))
			// Integrate resident GiB-seconds for the working-set signal (T4.1).
			rnow := time.Now()
			ct.mu.Lock()
			if last, ok := ct.lastResidAt[v.id]; ok {
				if dt := rnow.Sub(last).Seconds(); dt > 0 && dt < 120 {
					ct.residGiBSec[v.id] += float64(rb) / (1 << 30) * dt
				}
			}
			ct.lastResidAt[v.id] = rnow
			ct.mu.Unlock()
		}
	}
	ct.reapDead(live)

	// Shadow admission comparison, once a minute (every 4th 15s tick): what the
	// packer counts today (committed baked MiB) vs what physics says (resident
	// MiB). ZERO behavior change — this line is the dataset that justifies (or
	// kills) flipping admission to working-set in T2.1's enforce step.
	ct.shadowTick++
	if ct.shadowTick%4 == 0 && len(vms) > 0 {
		m.log.Info("tidal shadow admission",
			"vms", len(vms),
			"committed_mb", committedMB,
			"resident_mb", residentMB,
			"overcommit_headroom_mb", committedMB-residentMB)
	}
}

// readResidentBytes returns the process's resident memory including hugetlb
// mappings (Rss + Shared_Hugetlb + Private_Hugetlb), in bytes.
func readResidentBytes(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0, err
	}
	return parseResidentKB(string(b)) * 1024, nil
}

// parseResidentKB sums Rss + Shared_Hugetlb + Private_Hugetlb (kB) from
// smaps_rollup content. Split out for testability.
func parseResidentKB(content string) int64 {
	var kb int64
	for _, line := range strings.Split(content, "\n") {
		for _, pfx := range []string{"Rss:", "Shared_Hugetlb:", "Private_Hugetlb:"} {
			if strings.HasPrefix(line, pfx) {
				f := strings.Fields(line)
				if len(f) >= 2 {
					if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
						kb += v
					}
				}
			}
		}
	}
	return kb
}

// sandboxSpec reads the (vCPU, memory MiB) entitlement from the store row
// (falls back to 2 vCPU / 2048 MiB, the fleet's most common baked size).
func (m *Manager) sandboxSpec(id string) (vcpus, memMB int) {
	vcpus, memMB = 2, 2048
	sbAny, err := m.store.GetSandbox(context.Background(), id)
	if err != nil || sbAny == nil {
		return
	}
	rmap, ok := sbAny.(map[string]any)
	if !ok {
		return
	}
	geti := func(k string) int {
		if v, ok := rmap[k].(int); ok && v > 0 {
			return v
		}
		if v, ok := rmap[k].(int64); ok && v > 0 {
			return int(v)
		}
		return 0
	}
	if v := geti("cpu"); v > 0 {
		vcpus = v
	}
	if v := geti("memory_mb"); v > 0 {
		memMB = v
	}
	return
}

// ensureVM guarantees vm-<id> exists with the right weight and contains pid.
func (ct *cpuTiers) ensureVM(id string, pid, vcpus int) error {
	dir := filepath.Join(ct.svcDir, vmCgroupPrefix+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	w := vcpus * cpuWeightPerVCPU
	if w < cpuWeightMin {
		w = cpuWeightMin
	}
	if w > cpuWeightMax {
		w = cpuWeightMax
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.weight"), []byte(strconv.Itoa(w)), 0o644); err != nil {
		return err
	}
	// Membership check via the cgroup's own procs file (cheap; avoids a write
	// syscall storm on every tick for already-placed pids).
	if pids, err := readProcs(filepath.Join(dir, cgroupProcsFile)); err == nil {
		for _, p := range pids {
			if p == pid {
				return nil
			}
		}
	}
	return os.WriteFile(filepath.Join(dir, cgroupProcsFile), []byte(strconv.Itoa(pid)), 0o644)
}

// scrapeCPU reads vm-<id>/cpu.stat usage_usec and returns the DELTA in seconds
// since the previous scrape (first sight primes the baseline and returns 0).
func (ct *cpuTiers) scrapeCPU(id string) (float64, bool) {
	usec, err := readCPUStatUsage(filepath.Join(ct.svcDir, vmCgroupPrefix+id, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	prev, seen := ct.lastUsec[id]
	ct.lastUsec[id] = usec
	if !seen || usec < prev { // first sight, or counter reset (VM replaced)
		return 0, true
	}
	d := float64(usec-prev) / 1e6
	ct.totalSec[id] += d
	return d, true
}

// reapDead removes vm-* cgroups whose sandbox no longer has a live driver.
func (ct *cpuTiers) reapDead(live map[string]struct{}) {
	entries, err := os.ReadDir(ct.svcDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), vmCgroupPrefix) {
			continue
		}
		id := strings.TrimPrefix(e.Name(), vmCgroupPrefix)
		if _, ok := live[id]; ok {
			continue
		}
		// rmdir only succeeds once the process is gone — exactly the semantics
		// we want (never yank a still-running VM out of its cgroup).
		if err := os.Remove(filepath.Join(ct.svcDir, e.Name())); err == nil {
			ct.mu.Lock()
			delete(ct.lastUsec, id)
			delete(ct.residGiBSec, id)
			delete(ct.lastResidAt, id)
			ct.mu.Unlock()
			// Drop the dead VM's metric series so labels don't accumulate.
			obs.SandboxResidentBytes.DeleteLabelValues(id)
			obs.SandboxCPUSeconds.DeleteLabelValues(id)
		}
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// ownCgroupDir resolves this process's cgroup v2 directory from /proc/self/cgroup.
func ownCgroupDir(root string) (string, error) {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// v2 unified line: "0::/system.slice/pandastack-agent.service"
		line := sc.Text()
		if strings.HasPrefix(line, "0::") {
			return filepath.Join(root, strings.TrimPrefix(line, "0::")), nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry in /proc/self/cgroup")
}

func readProcs(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, l := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(l); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

func readCPUStatUsage(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("usage_usec not found in %s", path)
}
