// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"encoding/json"
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

// StartMetricsPoller runs a periodic poll over all running sandboxes, pushing
// one sandbox_metrics row per sandbox per tick. cpu_pct + mem_bytes come from
// the Firecracker driver's resource accounting (best-effort — zero on read
// failure). Safe no-op when ch is unset. Returns immediately; runs until ctx
// cancels.
func (m *Manager) StartMetricsPoller(ctx context.Context, interval time.Duration) {
	if m == nil || m.ch == nil {
		return
	}
	if interval <= 0 {
		interval = 15 * time.Second
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
	// Snapshot the running sandboxes under the manager mutex, then release
	// the lock before doing any per-sandbox work so we don't serialize.
	m.mu.RLock()
	ids := make([]string, 0, len(m.drivers))
	for id := range m.drivers {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		sbAny, err := m.store.GetSandbox(ctx, id)
		if err != nil || sbAny == nil {
			continue
		}
		status, mem := extractStatusMem(sbAny)
		if status != string(StatusRunning) {
			continue
		}
		ws, _ := extractWorkspaceTemplate(sbAny)
		// Phase 1: emit zeros for CPU% (computing requires a delta against
		// last poll; we'll add that in a follow-up). mem_bytes uses the
		// known memory cap until a real cgroup poller lands.
		m.chMetric(ws, id, 0.0, mem)
	}
}
