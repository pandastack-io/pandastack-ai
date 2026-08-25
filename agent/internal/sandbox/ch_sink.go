// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/clickhouse"
)

// CHSink is a narrow interface the manager calls into to push lifecycle +
// metric rows into ClickHouse. clickhouse.Client satisfies it. Kept as an
// interface so test fakes don't need a real ClickHouse running.
type CHSink interface {
	Insert(row clickhouse.Row)
}

// SetCHSink installs a ClickHouse writer + the agent's identity. nil-safe:
// passing nil simply leaves analytics disabled, every chXxx call becomes a
// no-op. Safe to call once during agent startup.
func (m *Manager) SetCHSink(ch CHSink, agentID string) {
	if m == nil {
		return
	}
	m.ch = ch
	m.agentID = agentID
}

// chBoot emits one row into pandastack.boot_events. Always async-safe via the
// underlying writer's bounded channel; never blocks the hot path.
func (m *Manager) chBoot(workspace, sandboxID, template, mode, fromSnap string, bootMS int64) {
	if m == nil || m.ch == nil {
		return
	}
	m.ch.Insert(clickhouse.Row{
		Table:     "boot_events",
		Workspace: chStringOr(workspace, "_unknown"),
		Cols: map[string]any{
			"sandbox_id":    sandboxID,
			"agent_id":      m.agentID,
			"template":      template,
			"boot_mode":     mode,
			"boot_ms":       uint32(bootMS),
			"from_snapshot": fromSnap,
		},
	})
}

// chEvent emits one row into pandastack.sandbox_events.
func (m *Manager) chEvent(workspace, sandboxID, typ, code, message string, metadata map[string]any) {
	if m == nil || m.ch == nil {
		return
	}
	var meta string
	if len(metadata) > 0 {
		if b, err := json.Marshal(metadata); err == nil {
			meta = string(b)
		}
	}
	m.ch.Insert(clickhouse.Row{
		Table:     "sandbox_events",
		Workspace: chStringOr(workspace, "_unknown"),
		Cols: map[string]any{
			"sandbox_id": sandboxID,
			"agent_id":   m.agentID,
			"type":       typ,
			"code":       code,
			"message":    message,
			"metadata":   meta,
		},
	})
}

// chMetric emits one row into pandastack.sandbox_metrics.
func (m *Manager) chMetric(workspace, sandboxID string, cpuPct float32, memBytes uint64) {
	if m == nil || m.ch == nil {
		return
	}
	m.ch.Insert(clickhouse.Row{
		Table:     "sandbox_metrics",
		Workspace: chStringOr(workspace, "_unknown"),
		Cols: map[string]any{
			"sandbox_id":    sandboxID,
			"agent_id":      m.agentID,
			"cpu_pct":       cpuPct,
			"mem_bytes":     memBytes,
			"net_rx_bytes":  uint64(0),
			"net_tx_bytes":  uint64(0),
			"disk_rd_bytes": uint64(0),
			"disk_wr_bytes": uint64(0),
		},
	})
}

func chStringOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// extractWorkspaceTemplate pulls workspace + template out of a store
// GetSandbox() return value (which is a map[string]any). Empty strings on miss.
func extractWorkspaceTemplate(sbAny any) (workspace, template string) {
	row, ok := sbAny.(map[string]any)
	if !ok {
		return "", ""
	}
	if t, ok := row["template"].(string); ok {
		template = t
	}
	if md, ok := row["metadata"].(map[string]string); ok {
		workspace = md["workspace"]
	}
	return workspace, template
}

// extractStatusMem pulls status + memory_mb out of a GetSandbox() map.
func extractStatusMem(sbAny any) (status string, memBytes uint64) {
	row, ok := sbAny.(map[string]any)
	if !ok {
		return "", 0
	}
	if s, ok := row["status"].(string); ok {
		status = s
	}
	switch v := row["memory_mb"].(type) {
	case int:
		memBytes = uint64(v) * 1024 * 1024
	case int64:
		memBytes = uint64(v) * 1024 * 1024
	case float64:
		memBytes = uint64(v) * 1024 * 1024
	}
	return status, memBytes
}

// metricsPollState holds per-VM state the metrics poller needs across ticks:
// the previous cpu.stat usage_usec + wallclock, so cpu_pct can be a proper
// delta rather than a spot value. Kept SEPARATE from cpuTiers's own lastUsec
// map so the two loops don't fight for the "delta since I last looked" pointer
// (they run on different cadences and one stealing the other's baseline would
// silently zero the other's rate).
type metricsPollState struct {
	mu       sync.Mutex
	lastUsec map[string]uint64    // vm id → cpu.stat usage_usec at last poll
	lastAt   map[string]time.Time // vm id → wallclock at last poll
}

func newMetricsPollState() *metricsPollState {
	return &metricsPollState{lastUsec: map[string]uint64{}, lastAt: map[string]time.Time{}}
}

// deltaCPUPct computes CPU% as (usec-since-last / wall-since-last / vcpus)*100,
// so 100 = fully using all allocated cores. First sight of a VM primes the
// baseline and returns (0, false) — no data point emitted for that tick.
// A counter reset (vm replaced with same id) is treated as first sight.
func (s *metricsPollState) deltaCPUPct(id string, usec uint64, now time.Time, vcpus int) (float64, bool) {
	if vcpus <= 0 {
		vcpus = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prevUsec, seen := s.lastUsec[id]
	prevAt := s.lastAt[id]
	s.lastUsec[id] = usec
	s.lastAt[id] = now
	if !seen || usec < prevUsec || prevAt.IsZero() {
		return 0, false
	}
	wallSec := now.Sub(prevAt).Seconds()
	if wallSec <= 0 {
		return 0, false
	}
	cpuSec := float64(usec-prevUsec) / 1e6
	pct := (cpuSec / wallSec) / float64(vcpus) * 100.0
	// Cap at a sane ceiling — brief scheduling jitter can produce >100 for a
	// single tick when usec_delta briefly outruns wallclock delta.
	if pct < 0 {
		pct = 0
	}
	if pct > 200 {
		pct = 200
	}
	return pct, true
}

// forget drops per-vm state when the VM is gone so the map doesn't grow
// unbounded across an agent's lifetime.
func (s *metricsPollState) forget(live map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.lastUsec {
		if _, ok := live[id]; !ok {
			delete(s.lastUsec, id)
			delete(s.lastAt, id)
		}
	}
}

// StartMetricsPoller runs a periodic poll over all running sandboxes, pushing
// one sandbox_metrics row per sandbox per tick. Reads REAL cgroup values:
//
//   - cpu_pct  = delta(cgroup vm-<id>/cpu.stat usage_usec) / wallclock / vcpus * 100
//   - mem_bytes = cgroup vm-<id>/memory.current
//
// First sight of a VM primes the baseline and skips emitting a row (there's no
// prior sample to delta against). Any read failure emits nothing for that VM
// (better than lying with a zero). net_* and disk_* stay zero for now — the
// dashboard has separate live tiles for those and adding them here means
// reading /sys/class/net/<host_tap>/statistics + cgroup io.stat per VM; worth
// its own iteration.
//
// Safe no-op when ch is unset or cpuTiers is nil (single-node dev without
// cgroups). Returns immediately; runs until ctx cancels.
func (m *Manager) StartMetricsPoller(ctx context.Context, interval time.Duration) {
	if m == nil || m.ch == nil {
		return
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	// Sink-local state, initialized lazily so tests that don't call this
	// still work.
	if m.metricsPoll == nil {
		m.metricsPoll = newMetricsPollState()
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.pollMetricsOnce(ctx)
			}
		}
	}()
}

func (m *Manager) pollMetricsOnce(ctx context.Context) {
	if m.cpuTiers == nil {
		// No cgroup mount (dev/single-node) — nothing sensible to sample.
		return
	}

	// Snapshot the running sandboxes under the manager mutex, then release
	// the lock before doing any per-sandbox work so we don't serialize.
	m.mu.RLock()
	ids := make([]string, 0, len(m.drivers))
	live := make(map[string]struct{}, len(m.drivers))
	for id := range m.drivers {
		ids = append(ids, id)
		live[id] = struct{}{}
	}
	m.mu.RUnlock()

	// Drop delta state for VMs that no longer exist so an id reuse doesn't
	// carry a stale baseline into a fresh VM.
	if m.metricsPoll != nil {
		m.metricsPoll.forget(live)
	}

	now := time.Now()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		sbAny, err := m.store.GetSandbox(ctx, id)
		if err != nil || sbAny == nil {
			continue
		}
		status, _ := extractStatusMem(sbAny)
		if status != string(StatusRunning) {
			continue
		}
		ws, _ := extractWorkspaceTemplate(sbAny)
		vcpus := extractCPUCount(sbAny)

		// Real memory: k8s-style WORKING SET = memory.current − inactive_file.
		// memory.current alone counts the host page cache for the VM's disk
		// I/O against the VM's cgroup — a 1 GiB-tier Postgres spilling 2.7 GB
		// of temp files charted at ~3 GB "memory" (observed live 2026-08-21),
		// which contradicts the dashboard's "guest working-set" caption and
		// reads as a tier violation. Subtracting not-recently-touched file
		// cache (exactly what kubelet's container_memory_working_set_bytes
		// does) reports only the memory that would hurt if reclaimed. Falls
		// back to the baked committed cap when the cgroup is unreadable
		// (just-created VM, or hugepage-backed guests without memory
		// delegation — hugetlbfs is invisible to memory.current either way).
		var memBytes uint64
		if wsBytes, ok := readWorkingSetBytes(filepath.Join(m.cpuTiers.svcDir, vmCgroupPrefix+id)); ok {
			memBytes = wsBytes
		} else if _, baked := extractStatusMem(sbAny); baked > 0 {
			memBytes = baked
		}

		// Real CPU%: delta of cpu.stat usage_usec / wall time / vcpus.
		vmCgroup := filepath.Join(m.cpuTiers.svcDir, vmCgroupPrefix+id, "cpu.stat")
		usec, uerr := readCPUStatUsage(vmCgroup)
		if uerr != nil || m.metricsPoll == nil {
			// Can't compute CPU — still emit the memory row so the chart
			// stays continuous; consumers see cpu_pct=0 for one tick until
			// the next successful read. Better than a dropped sample.
			m.chMetric(ws, id, 0.0, memBytes)
			continue
		}
		pct, primed := m.metricsPoll.deltaCPUPct(id, usec, now, vcpus)
		if !primed {
			// First sight — baseline recorded, don't emit this tick (would
			// be a bogus 0 that pulls the chart line down).
			continue
		}
		m.chMetric(ws, id, float32(pct), memBytes)
	}
}

// readWorkingSetBytes reads a cgroup-v2 dir and returns the k8s-style working
// set: memory.current minus memory.stat's inactive_file, floored at 0.
// ok=false when memory.current is unreadable (cgroup not set up yet, or no
// memory controller delegated). A missing/short memory.stat degrades to raw
// memory.current rather than failing — a slightly-inflated sample beats none.
//
// Semantics note: after a snapshot restore, guest RAM is a MAP_PRIVATE
// file-backed mapping of vm.mem, so *idle* guest pages can sit in
// inactive_file and be subtracted here. That is correct working-set behavior
// (they're re-faultable from vm.mem at page-cache speed, so reclaiming them
// doesn't hurt) — but it means this number can legitimately read BELOW the
// guest's touched-memory footprint on a quiet VM. It answers "how much memory
// does this VM need right now", not "how much has it ever touched".
func readWorkingSetBytes(dir string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, false
	}
	cur, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	if sb, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		for _, ln := range strings.Split(string(sb), "\n") {
			if rest, found := strings.CutPrefix(ln, "inactive_file "); found {
				if inact, perr := strconv.ParseUint(strings.TrimSpace(rest), 10, 64); perr == nil {
					if inact >= cur {
						return 0, true
					}
					cur -= inact
				}
				break
			}
		}
	}
	return cur, true
}

// extractCPUCount pulls the `cpu` count from a GetSandbox() map. Falls back
// to 1 so the CPU% divisor is never zero.
func extractCPUCount(sbAny any) int {
	row, ok := sbAny.(map[string]any)
	if !ok {
		return 1
	}
	switch v := row["cpu"].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return 1
}
