// SPDX-License-Identifier: Apache-2.0
//
// pressure.go — TIDAL T2.3: the PSI pressure ladder.
//
// Host RAM is a cache, not an allocation (tidal-plan.md §2). This controller
// watches host memory pressure and walks the coldest VMs down the ladder
// before the host ever reaches the kernel OOM killer:
//
//	OK        → lift one squeeze per tick (gradual promotion)
//	ELEVATED  → squeeze the coldest squeezable VMs: memory.high = 70% of
//	            residency (floor 256 MiB) → zswap compresses the cold tail
//	            (T2.2 proof: capped VMs degrade instead of dying)
//	CRITICAL  → keep squeezing AND Thaw-freeze the coldest idle plain
//	            sandbox (existing Hibernate path; wakes on touch)
//
// Scope guards: DB-class VMs are never touched (guaranteed class, own idle
// governance); app-class VMs are squeezed but never frozen here (the apps
// monitor owns their sleep lifecycle); hugepage-backed VMs are skipped
// empirically (their memory.current is a few MiB — nothing to squeeze).
//
// State is derived, not stored: the squeezed set is "memory.high != max" read
// from the cgroup files each tick, so an agent restart adopts existing
// squeezes with no bookkeeping. Kill switch: PANDASTACK_PRESSURE_LADDER=0.
package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/obs"
)

const (
	pressureTick = 10 * time.Second

	// squeezeShrinkPct: memory.high target as % of current residency.
	squeezeShrinkPct = 70
	// squeezeFloorBytes: never cap below this (a guest needs room to breathe).
	squeezeFloorBytes = 256 << 20
	// squeezeSkipBelowBytes: memory.current under this means hugepage-backed
	// or trivially small — nothing worth squeezing either way.
	squeezeSkipBelowBytes = 32 << 20
	// maxSqueezesPerTick bounds churn under sustained pressure.
	maxSqueezesPerTick = 2

	// freeze eligibility + storm cap.
	freezeMinAge      = 2 * time.Minute
	freezeMinIdle     = 60 * time.Second
	freezeMaxCPURate  = 0.05 // cores — above this the VM is "doing something"
	freezeMinInterval = 60 * time.Second
	// freezeFailCooldown parks a victim whose Hibernate failed — without it
	// the failed (and therefore idle-looking) VM is re-picked every interval
	// and spends most of its life paused (the pause-wedge storm class).
	freezeFailCooldown = 15 * time.Minute

	// squeeze activity gates: never cap a VM that is demonstrably in use.
	squeezeMaxCPURate = 0.5 // cores
	squeezeMinIdle    = 60 * time.Second

	// Reclaim cycle (T2.4): on a CALM host, periodically squeeze long-idle
	// VMs to their working set for a dwell so zswap absorbs the cold tail,
	// then release. Without it an idle VM keeps its PEAK residency forever
	// (the ratchet — guest page reuse never returns host pages).
	reclaimInterval = 10 * time.Minute // per-VM cadence
	reclaimDwell    = 90 * time.Second // cap hold time (zswap needs a window to reclaim)
	reclaimMinIdle  = 5 * time.Minute  // only genuinely idle VMs
	reclaimPerTick  = 1                // bound churn
	// appSqueezeWarmup: app-class VMs are never squeezed while younger than
	// this — covers the 12-minute deploy budget (builds need their RAM).
	appSqueezeWarmup = 15 * time.Minute
)

type pressureLevel int

const (
	levelOK pressureLevel = iota
	levelElevated
	levelCritical
)

func (l pressureLevel) String() string {
	switch l {
	case levelElevated:
		return "elevated"
	case levelCritical:
		return "critical"
	}
	return "ok"
}

// pressureThresholds — env-tunable trip points.
type pressureThresholds struct {
	lowPct  float64 // MemAvailable% below → ELEVATED
	critPct float64 // MemAvailable% below → CRITICAL
	psiSome float64 // memory PSI some avg10 above → ELEVATED
	psiFull float64 // memory PSI full avg10 above → CRITICAL
}

func pressureThresholdsFromEnv() pressureThresholds {
	f := func(env string, def float64) float64 {
		if v := os.Getenv(env); v != "" {
			if x, err := strconv.ParseFloat(v, 64); err == nil && x > 0 {
				return x
			}
		}
		return def
	}
	return pressureThresholds{
		lowPct:  f("PANDASTACK_PRESSURE_LOW_PCT", 15),
		critPct: f("PANDASTACK_PRESSURE_CRIT_PCT", 8),
		psiSome: f("PANDASTACK_PRESSURE_PSI_SOME", 10),
		psiFull: f("PANDASTACK_PRESSURE_PSI_FULL", 5),
	}
}

func pressureLadderEnabled() bool {
	return os.Getenv("PANDASTACK_PRESSURE_LADDER") != "0"
}

// reclaimEnabled gates the calm-host reclaim cycle (T2.4) separately from the
// pressure ladder proper.
func reclaimEnabled() bool {
	return os.Getenv("PANDASTACK_RECLAIM") != "0"
}

// classifyPressure maps host signals to a ladder level. CRITICAL wins.
// Thresholds are percent-of-total clamped to absolute ceilings (4 GiB / 2 GiB)
// so big hosts don't idle in ELEVATED with plenty of absolute headroom.
func classifyPressure(t pressureThresholds, availMB, totalMB int64, psiSome, psiFull float64) pressureLevel {
	lowMB := int64(float64(totalMB) * t.lowPct / 100)
	if lowMB > 4096 {
		lowMB = 4096
	}
	critMB := critWaterMB(t, totalMB)
	if availMB < critMB || psiFull > t.psiFull {
		return levelCritical
	}
	if availMB < lowMB || psiSome > t.psiSome {
		return levelElevated
	}
	return levelOK
}

// critWaterMB is the absolute-scarcity water line: percent-of-total clamped
// to 2 GiB. Shared by classification and the freeze gate.
func critWaterMB(t pressureThresholds, totalMB int64) int64 {
	critMB := int64(float64(totalMB) * t.critPct / 100)
	if critMB > 2048 {
		critMB = 2048
	}
	return critMB
}

// applyHysteresis lets a level RISE immediately but only FALL when the raw
// signal has fallen for two consecutive ticks — a Schmitt trigger that stops
// squeeze/unsqueeze flapping right at a threshold.
func applyHysteresis(prev, raw pressureLevel, fallStreak *int) pressureLevel {
	if raw >= prev {
		*fallStreak = 0
		return raw
	}
	*fallStreak++
	if *fallStreak >= 2 {
		*fallStreak = 0
		return raw
	}
	return prev
}

// hostMemInfo reads MemAvailable/MemTotal/SwapTotal from /proc/meminfo.
// SwapTotal gates the squeeze rung: memory.high without swap doesn't reclaim,
// it stalls (T0.2 measured PSI 96 + DNF).
func hostMemInfo() (availMB, totalMB, swapTotalMB int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			totalMB = kb / 1024
		case "MemAvailable:":
			availMB = kb / 1024
		case "SwapTotal:":
			swapTotalMB = kb / 1024
		}
	}
	if totalMB <= 0 {
		return 0, 0, 0, fmt.Errorf("MemTotal not found")
	}
	return availMB, totalMB, swapTotalMB, nil
}

// memoryPSI reads "some" and "full" avg10 from /proc/pressure/memory.
func memoryPSI() (some10, full10 float64, err error) {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var v float64
		for _, kv := range fields[1:] {
			if s, ok := strings.CutPrefix(kv, "avg10="); ok {
				v, _ = strconv.ParseFloat(s, 64)
			}
		}
		switch fields[0] {
		case "some":
			some10 = v
		case "full":
			full10 = v
		}
	}
	return some10, full10, nil
}

// pressureVM is one running, tenant-owned VM as the ladder sees it.
type pressureVM struct {
	id       string
	class    string // workloadClass: sandbox | app | db
	cpuRate  float64
	lastAct  time.Time
	current  int64 // memory.current bytes (0 = unreadable / no memory ctrl)
	squeezed bool  // memory.high != "max"
}

// coldnessLess orders VMs coldest-first: lowest CPU rate, then oldest activity.
func coldnessLess(a, b pressureVM) bool {
	if a.cpuRate != b.cpuRate {
		return a.cpuRate < b.cpuRate
	}
	return a.lastAct.Before(b.lastAct)
}

// squeezeTarget computes the memory.high cap for a VM.
func squeezeTarget(currentBytes int64) int64 {
	t := currentBytes * squeezeShrinkPct / 100
	if t < squeezeFloorBytes {
		t = squeezeFloorBytes
	}
	return t
}

// freezeEligible: only plain sandboxes, warmed up, idle, and quiet.
func freezeEligible(vm pressureVM, createdAt, now time.Time) bool {
	if vm.class != "sandbox" {
		return false
	}
	if now.Sub(createdAt) < freezeMinAge {
		return false
	}
	if !vm.lastAct.IsZero() && now.Sub(vm.lastAct) < freezeMinIdle {
		return false
	}
	return vm.cpuRate <= freezeMaxCPURate
}

// pressureState is the controller's cross-tick memory.
type pressureState struct {
	prevUsec     map[string]uint64 // per-VM cpu.stat usage_usec at last tick (rate over OUR window)
	prevTickAt   time.Time
	lastFreeze   time.Time
	level        pressureLevel // post-hysteresis level from last tick
	fallStreak   int
	squeezeBasis map[string]int64 // pre-squeeze residency floor (anti-ratchet)
	cooldown     map[string]time.Time
	squeezedAt   map[string]time.Time // when WE applied the current cap (dwell release)
	lastReclaim  map[string]time.Time // per-VM reclaim cadence

	warnedMeminfo bool
	warnedPSI     bool
	warnedNoSwap  bool
	warnedNoMem   bool

	mu       sync.Mutex // guards freezing (written by freeze goroutines)
	freezing map[string]bool
}

func (st *pressureState) isFreezing(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.freezing[id]
}

func (st *pressureState) setFreezing(id string, v bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if v {
		st.freezing[id] = true
	} else {
		delete(st.freezing, id)
	}
}

// runPressureLadder is the loop entrypoint (started from NewManager).
func (m *Manager) runPressureLadder() {
	if !pressureLadderEnabled() {
		m.log.Info("pressure ladder: disabled via PANDASTACK_PRESSURE_LADDER=0")
		return
	}
	th := pressureThresholdsFromEnv()
	if !cpuTiersEnabled() {
		m.log.Warn("pressure ladder: inert — requires cpu tiers (PANDASTACK_CPU_TIERS=0)")
		return
	}
	st := &pressureState{
		prevUsec:     make(map[string]uint64),
		squeezeBasis: make(map[string]int64),
		cooldown:     make(map[string]time.Time),
		squeezedAt:   make(map[string]time.Time),
		lastReclaim:  make(map[string]time.Time),
		freezing:     make(map[string]bool),
	}
	m.log.Info("pressure ladder: enabled",
		"low_pct", th.lowPct, "crit_pct", th.critPct,
		"psi_some", th.psiSome, "psi_full", th.psiFull)
	t := time.NewTicker(pressureTick)
	defer t.Stop()
	for range t.C {
		m.pressureTickOnce(th, st)
	}
}

func (m *Manager) pressureTickOnce(th pressureThresholds, st *pressureState) {
	availMB, totalMB, swapMB, err := hostMemInfo()
	if err != nil {
		if !st.warnedMeminfo {
			st.warnedMeminfo = true
			m.log.Warn("pressure ladder: /proc/meminfo unreadable — ladder inert", "err", err)
		}
		return
	}
	psiSome, psiFull, err := memoryPSI()
	if err != nil {
		// Degrade to meminfo-only classification rather than going inert.
		if !st.warnedPSI {
			st.warnedPSI = true
			m.log.Warn("pressure ladder: PSI unavailable — meminfo-only classification", "err", err)
		}
		psiSome, psiFull = 0, 0
	}
	raw := classifyPressure(th, availMB, totalMB, psiSome, psiFull)
	level := applyHysteresis(st.level, raw, &st.fallStreak)
	st.level = level
	obs.MemPressureLevel.Set(float64(level))
	// Freeze requires ABSOLUTE scarcity, not just a CRITICAL classification:
	// PSI-full spikes are often self-induced (a hibernate's own multi-GB
	// vm.mem write causes page-cache reclaim stalls) — storm sim #1 froze
	// three VMs while 11-16 GB were available. Squeezes may act on PSI alone;
	// freezing may not.
	scarce := availMB < critWaterMB(th, totalMB)

	if m.cpuTiers == nil || !m.cpuTiers.ready.Load() {
		return // no delegated cgroups yet — nothing actionable
	}
	now := time.Now()
	vms, created := m.pressureInventory(st, now)
	st.prevTickAt = now
	if len(vms) == 0 {
		return
	}

	switch level {
	case levelOK:
		// Release caps whose dwell has passed (pressure squeezes from an
		// earlier episode release immediately — squeezedAt is zero for caps
		// we didn't place this cycle, e.g. adopted after an agent restart).
		lifted := 0
		for _, vm := range vms {
			if !vm.squeezed || lifted >= maxSqueezesPerTick {
				continue
			}
			if at, ok := st.squeezedAt[vm.id]; ok && now.Sub(at) < reclaimDwell {
				continue // zswap still working the cold tail
			}
			if err := m.writeVMMemoryHigh(vm.id, "max"); err == nil {
				lifted++
				delete(st.squeezedAt, vm.id)
				obs.PressureActionsTotal.WithLabelValues("unsqueeze").Inc()
				m.log.Info("pressure ladder: unsqueeze", "id", vm.id, "avail_mb", availMB)
			}
		}
		// T2.4 reclaim cycle: proactively reclaim from one long-idle VM per
		// tick even without host pressure, so the residency ratchet resets
		// while nobody is looking. Uses memory.reclaim (cgroup-v2 proactive
		// reclaim) rather than memory.high: a cap only reclaims through the
		// cgroup's own allocation paths, and an IDLE VM never allocates —
		// measured live: capped idle VM held 3.26 GB indefinitely. Requires
		// reclaim backing (zswap+swap) so cold anon pages have a destination.
		if reclaimEnabled() && swapMB > 0 && m.cpuTiers.memDelegated {
			launched := 0
			for _, vm := range vms {
				if launched >= reclaimPerTick {
					break
				}
				if vm.squeezed || vm.class == "db" || vm.current < squeezeSkipBelowBytes {
					continue
				}
				if vm.cpuRate > squeezeMaxCPURate ||
					(!vm.lastAct.IsZero() && now.Sub(vm.lastAct) < reclaimMinIdle) ||
					now.Sub(created[vm.id]) < reclaimMinIdle {
					continue
				}
				if last, ok := st.lastReclaim[vm.id]; ok && now.Sub(last) < reclaimInterval {
					continue
				}
				if st.isFreezing(vm.id) { // shares the in-flight guard
					continue
				}
				want := vm.current - squeezeTarget(vm.current)
				if want <= 0 {
					continue
				}
				launched++
				st.lastReclaim[vm.id] = now
				st.setFreezing(vm.id, true) // reuse as "action in flight" guard
				id, cur := vm.id, vm.current
				obs.PressureActionsTotal.WithLabelValues("reclaim").Inc()
				go func() {
					defer st.setFreezing(id, false)
					// The write blocks until the kernel reclaimed the amount
					// (or gives up with EAGAIN after partial progress — fine).
					start := time.Now()
					err := m.writeVMMemoryReclaim(id, want)
					after, _ := m.readVMMemory(id)
					m.log.Info("pressure ladder: reclaim (calm host)",
						"id", id, "before_mb", cur>>20, "after_mb", after>>20,
						"asked_mb", want>>20, "took_ms", time.Since(start).Milliseconds(),
						"err", err)
				}()
			}
		}
	case levelElevated, levelCritical:
		sort.Slice(vms, func(i, j int) bool { return coldnessLess(vms[i], vms[j]) })
		// Squeeze rung requires reclaim backing: memory.high without swap
		// stalls the guest instead of shrinking it (T0.2).
		squeezeOK := swapMB > 0 && m.cpuTiers.memDelegated
		if !squeezeOK && !st.warnedNoSwap {
			st.warnedNoSwap = true
			m.log.Warn("pressure ladder: squeeze rung disabled",
				"swap_mb", swapMB, "memory_delegated", m.cpuTiers.memDelegated)
		}
		if squeezeOK {
			squeezed := 0
			for _, vm := range vms {
				if squeezed >= maxSqueezesPerTick {
					break
				}
				if vm.squeezed || vm.class == "db" || vm.current < squeezeSkipBelowBytes {
					continue
				}
				// Never cap a VM that is demonstrably in use, and never mid-deploy.
				if vm.cpuRate > squeezeMaxCPURate ||
					(!vm.lastAct.IsZero() && now.Sub(vm.lastAct) < squeezeMinIdle) {
					continue
				}
				if vm.class == "app" && now.Sub(created[vm.id]) < appSqueezeWarmup {
					continue
				}
				// Anti-ratchet: squeeze against the largest residency ever
				// observed, not the post-squeeze shrunken figure.
				basis := vm.current
				if prev := st.squeezeBasis[vm.id]; prev > basis {
					basis = prev
				}
				st.squeezeBasis[vm.id] = basis
				target := squeezeTarget(basis)
				if err := m.writeVMMemoryHigh(vm.id, strconv.FormatInt(target, 10)); err != nil {
					continue
				}
				squeezed++
				st.squeezedAt[vm.id] = now
				obs.PressureActionsTotal.WithLabelValues("squeeze").Inc()
				m.log.Info("pressure ladder: squeeze",
					"id", vm.id, "level", level.String(), "avail_mb", availMB,
					"psi_some", psiSome, "current_mb", vm.current>>20, "high_mb", target>>20)
			}
		}
		if level == levelCritical && scarce && now.Sub(st.lastFreeze) >= freezeMinInterval {
			for _, vm := range vms {
				if st.isFreezing(vm.id) || now.Before(st.cooldown[vm.id]) ||
					!freezeEligible(vm, created[vm.id], now) {
					continue
				}
				// Don't race a concurrent delete/reap: no live driver, no freeze
				// (storm sim #1: the reaper and the ladder picked the same victim
				// in the same second — pause hit a socket mid-teardown).
				if m.driver(vm.id) == nil {
					continue
				}
				st.lastFreeze = now
				st.setFreezing(vm.id, true)
				id := vm.id
				obs.PressureActionsTotal.WithLabelValues("freeze").Inc()
				m.log.Warn("pressure ladder: Thaw-freeze (critical host pressure)",
					"id", id, "avail_mb", availMB, "psi_full", psiFull,
					"cpu_rate", vm.cpuRate, "idle_s", int(now.Sub(vm.lastAct).Seconds()))
				go func() {
					defer st.setFreezing(id, false)
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					err := m.Hibernate(ctx, id)
					fails, capped := m.noteHibernateResult(id, err)
					if err != nil {
						// Park the victim: a failed hibernate leaves it paused-
						// then-resumed and idle-looking — without a cooldown it
						// is re-picked every interval (pause-wedge storm class).
						st.mu.Lock()
						st.cooldown[id] = time.Now().Add(freezeFailCooldown)
						st.mu.Unlock()
						m.log.Warn("pressure ladder: freeze failed (victim parked)",
							"id", id, "err", err, "fails", fails, "capped", capped)
					}
				}()
				break
			}
		}
	}
}

// pressureInventory lists running tenant-owned VMs with class, CPU rate,
// residency, and squeeze state. Returns created-at per id for age checks.
func (m *Manager) pressureInventory(st *pressureState, now time.Time) ([]pressureVM, map[string]time.Time) {
	rows, err := m.store.ListSandboxesForAgent(context.Background(), m.agentID)
	if err != nil {
		return nil, nil
	}
	dt := now.Sub(st.prevTickAt).Seconds()
	if dt <= 0 || st.prevTickAt.IsZero() {
		dt = pressureTick.Seconds()
	}
	svc := m.cpuTiers.svcDir
	var out []pressureVM
	created := make(map[string]time.Time)
	seen := make(map[string]bool)
	for _, row := range rows {
		rmap, _ := row.(map[string]any)
		if rmap == nil {
			continue
		}
		id, _ := rmap["id"].(string)
		if id == "" || !m.ownsRow(rmap) {
			continue
		}
		if status, _ := rmap["status"].(string); status != string(StatusRunning) {
			continue
		}
		template, _ := rmap["template"].(string)
		md, _ := rmap["metadata"].(map[string]string)
		if md == nil || md["workspace"] == "" {
			continue // non-tenant
		}
		// Rate over OUR OWN window: the cputiers accumulator advances on a 15s
		// ticker, so differentiating it across a 10s tick reads zero on 1 of
		// every 3 windows — which made every VM look cold exactly when the
		// ladder needed the truth. Scrape cpu.stat directly instead.
		usec, err := readCPUStatUsage(filepath.Join(svc, vmCgroupPrefix+id, "cpu.stat"))
		if err != nil {
			continue // cgroup not (or no longer) present — not actionable
		}
		prev, sighted := st.prevUsec[id]
		st.prevUsec[id] = usec
		var rate float64
		if !sighted || usec < prev {
			// First sight or counter reset: treat as HOT — never freeze or
			// squeeze-rank a VM on a window we didn't actually observe.
			rate = squeezeMaxCPURate + 1
		} else {
			rate = float64(usec-prev) / 1e6 / dt
		}
		seen[id] = true
		m.actMu.Lock()
		lastAct := m.lastActivity[id]
		m.actMu.Unlock()
		cur, high := m.readVMMemory(id)
		if ca, okc := rmap["created_at"].(time.Time); okc {
			created[id] = ca
		}
		out = append(out, pressureVM{
			id:       id,
			class:    workloadClass(template, md),
			cpuRate:  rate,
			lastAct:  lastAct,
			current:  cur,
			squeezed: high != "" && high != "max",
		})
	}
	for id := range st.prevUsec {
		if !seen[id] {
			delete(st.prevUsec, id)
			delete(st.squeezeBasis, id)
			delete(st.cooldown, id)
			delete(st.squeezedAt, id)
			delete(st.lastReclaim, id)
		}
	}
	return out, created
}

// readVMMemory returns memory.current bytes and the raw memory.high value.
func (m *Manager) readVMMemory(id string) (current int64, high string) {
	dir := filepath.Join(m.cpuTiers.svcDir, vmCgroupPrefix+id)
	if b, err := os.ReadFile(filepath.Join(dir, "memory.current")); err == nil {
		current, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "memory.high")); err == nil {
		high = strings.TrimSpace(string(b))
	}
	return current, high
}

// writeVMMemoryReclaim asks the kernel to proactively reclaim n bytes from a
// VM's cgroup (memory.reclaim, cgroup v2 / kernel 5.19+). Partial progress
// surfaces as EAGAIN — callers treat that as success-with-less.
func (m *Manager) writeVMMemoryReclaim(id string, n int64) error {
	return os.WriteFile(
		filepath.Join(m.cpuTiers.svcDir, vmCgroupPrefix+id, "memory.reclaim"),
		[]byte(strconv.FormatInt(n, 10)), 0o644)
}

// writeVMMemoryHigh sets memory.high for a VM's cgroup.
func (m *Manager) writeVMMemoryHigh(id, value string) error {
	return os.WriteFile(
		filepath.Join(m.cpuTiers.svcDir, vmCgroupPrefix+id, "memory.high"),
		[]byte(value), 0o644)
}

// UtilSnapshot is the per-sandbox utilization sample served to the control
// plane (GET /sandboxes/{id}/util) — the T3.2 scale-out signal. CPU is the
// cumulative active-CPU-seconds counter (caller differentiates across its own
// sampling interval); memory is current cgroup residency vs committed.
type UtilSnapshot struct {
	ActiveCPUSeconds float64 `json:"active_cpu_seconds"`
	CPUTracked       bool    `json:"cpu_tracked"`
	ResidentBytes    int64   `json:"resident_bytes"`
	CPUs             int     `json:"cpu"`
	MemoryMB         int     `json:"memory_mb"`
}

// Util returns the utilization snapshot for a running sandbox.
func (m *Manager) Util(ctx context.Context, id string) (*UtilSnapshot, error) {
	row, err := m.store.GetSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	rmap, _ := row.(map[string]any)
	if rmap == nil {
		return nil, fmt.Errorf("sandbox not found")
	}
	u := &UtilSnapshot{
		CPUs:     asInt(rmap["cpu"]),
		MemoryMB: asInt(rmap["memory_mb"]),
	}
	u.ActiveCPUSeconds, u.CPUTracked = m.activeCPUTracked(id)
	if m.cpuTiers != nil && m.cpuTiers.ready.Load() {
		cur, _ := m.readVMMemory(id)
		u.ResidentBytes = cur
	}
	return u, nil
}
