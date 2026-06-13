// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"path/filepath"
	"time"
)

// LeaseSink is the subset of registry.Registry that Manager needs to record
// sandbox→agent ownership. Defined as an interface so the sandbox package
// stays free of a dependency on internal/registry (which depends on store).
//
// Implementations should be safe to call concurrently and must never panic
// the caller — Manager treats lease errors as non-fatal warnings.
type LeaseSink interface {
	AcquireLease(ctx context.Context, sandboxID, agentID, workspaceID string, ttl time.Duration) error
	ReleaseLease(ctx context.Context, sandboxID string) error
	// SweepStaleLeases removes any lease whose expires_at has passed AND
	// flips the corresponding sandboxes row to status='failed' so the
	// dashboard stops trying to talk to a dead VM.
	SweepStaleLeases(ctx context.Context) (int, error)
	// SweepAgentZombies cleans rows owned by this agent that are not in
	// the provided "live" set. Used at startup after a process restart:
	// any sandbox the PG/leases table claims is on this agent but that
	// our local store has lost is a zombie.
	SweepAgentZombies(ctx context.Context, agentID string, liveIDs []string) (int, error)
}

// SetLeaseSink injects the registry-backed lease writer. Safe to call once
// at startup; later calls overwrite the previous sink.
func (m *Manager) SetLeaseSink(sink LeaseSink, agentID string, ttl time.Duration) {
	m.mu.Lock()
	m.leases = sink
	m.leaseAgentID = agentID
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	m.leaseTTL = ttl
	m.mu.Unlock()
}

// acquireLease is a best-effort wrapper. The sandbox is considered created
// even if the lease write fails — the periodic sweeper or the next agent
// startup will reconcile.
//
// Lease write is fired in a goroutine so the user-facing Create response
// doesn't block on a Supabase round-trip (us-east-2 from us-central1 = ~30ms).
// The edge MultiNodeDirector populates its in-memory lease cache from the
// Create response, so the user's very next request still routes correctly
// even before the PG write lands. Cross-edge requests during the lease-write
// window fall through to scheduler.Pick, which usually re-selects the same
// agent that just created the sandbox.
func (m *Manager) acquireLease(ctx context.Context, sandboxID, workspaceID string) {
	m.mu.RLock()
	sink, agentID, ttl := m.leases, m.leaseAgentID, m.leaseTTL
	m.mu.RUnlock()
	if sink == nil || agentID == "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("lease acquire panic (recovered)", "sandbox_id", sandboxID, "panic", r)
			}
		}()
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sink.AcquireLease(bgCtx, sandboxID, agentID, workspaceID, ttl); err != nil {
			m.log.Warn("lease acquire failed (non-fatal)", "sandbox_id", sandboxID, "err", err)
		}
	}()
}

func (m *Manager) releaseLease(ctx context.Context, sandboxID string) {
	m.mu.RLock()
	sink := m.leases
	m.mu.RUnlock()
	if sink == nil {
		return
	}
	// Fire-and-forget: stale leases are swept by StartLeaseSweeper, so a
	// failed or slow release is non-fatal. Running async prevents a Supabase
	// round-trip (~30-50ms) from blocking the 204 response on the delete path.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sink.ReleaseLease(bgCtx, sandboxID); err != nil {
			m.log.Warn("lease release failed (non-fatal)", "sandbox_id", sandboxID, "err", err)
		}
	}()
}

// liveSandboxIDs returns the set of sandbox IDs that the local store currently
// considers non-terminal. Used to feed SweepAgentZombies on startup.
func (m *Manager) liveSandboxIDs(ctx context.Context) ([]string, error) {
	all, err := m.store.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, raw := range all {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := row["id"].(string)
		status, _ := row["status"].(string)
		if id == "" {
			continue
		}
		// Only "live-ish" statuses keep their lease. Failed/deleted/pooled
		// sandboxes are not owned by this agent for routing purposes.
		switch status {
		case string(StatusRunning), string(StatusPaused), string(StatusHibernated):
			out = append(out, id)
		}
	}
	return out, nil
}

// ReconcileLeasesOnStartup runs once after Recover() to delete leases pointing
// at this agent for sandboxes we no longer have, marking their sandboxes-row
// status=failed so the dashboard stops trying to SSH into dead guests.
func (m *Manager) ReconcileLeasesOnStartup(ctx context.Context) {
	m.mu.RLock()
	sink, agentID := m.leases, m.leaseAgentID
	m.mu.RUnlock()
	if sink == nil || agentID == "" {
		return
	}
	live, err := m.liveSandboxIDs(ctx)
	if err != nil {
		m.log.Warn("lease startup reconcile: list local sandboxes failed", "err", err)
		return
	}
	n, err := sink.SweepAgentZombies(ctx, agentID, live)
	if err != nil {
		m.log.Warn("lease startup reconcile: sweep failed", "err", err)
		return
	}
	if n > 0 {
		m.log.Info("lease startup reconcile: marked zombie sandboxes failed",
			"agent_id", agentID, "count", n, "live_count", len(live))
	}
}

// renewLocalLeases re-stamps the lease for every sandbox this agent
// PHYSICALLY owns, so long-lived sandboxes (managed databases especially,
// which can sit hibernated for weeks under scale-to-zero) never hit the
// default 24h lease TTL and get flipped to failed by SweepStaleLeases.
//
// Ownership signal is deliberately LOCAL, never the store listing: in
// multi-node deployments the store is a shared Postgres and ListSandboxes
// returns every agent's rows — renewing from that would have all agents
// stamping each other's leases. Instead we union:
//   - m.drivers          → VMs currently running on this host
//   - vms/*/hibernation/vm.state on local disk → sandboxes hibernated here
//
// Candidates are then gated on the store row's status (running/paused/
// hibernated) so pooled or failed slots don't reacquire routing.
func (m *Manager) renewLocalLeases(ctx context.Context) {
	m.mu.RLock()
	sink, agentID, ttl := m.leases, m.leaseAgentID, m.leaseTTL
	ids := make(map[string]struct{}, len(m.drivers))
	for id := range m.drivers {
		ids[id] = struct{}{}
	}
	m.mu.RUnlock()
	if sink == nil || agentID == "" {
		return
	}
	pattern := filepath.Join(m.cfg.DataDir, "vms", "*", "hibernation", "vm.state")
	if matches, err := filepath.Glob(pattern); err == nil {
		for _, p := range matches {
			// .../vms/<id>/hibernation/vm.state → <id>
			if id := filepath.Base(filepath.Dir(filepath.Dir(p))); id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	renewed := 0
	for id := range ids {
		sbAny, err := m.store.GetSandbox(ctx, id)
		if err != nil || sbAny == nil {
			continue
		}
		status, _ := extractStatusMem(sbAny)
		switch status {
		case string(StatusRunning), string(StatusPaused), string(StatusHibernated):
		default:
			continue
		}
		ws, _ := extractWorkspaceTemplate(sbAny)
		// AcquireLease is an UPSERT on sandbox_id, so this simply pushes
		// expires_at forward by ttl for leases we already hold.
		if err := sink.AcquireLease(ctx, id, agentID, ws, ttl); err != nil {
			m.log.Warn("lease renew failed (non-fatal)", "sandbox_id", id, "err", err)
			continue
		}
		renewed++
	}
	if renewed > 0 {
		m.log.Debug("lease renew: refreshed locally-owned leases", "count", renewed)
	}
}

// StartLeaseSweeper runs a background goroutine that periodically deletes
// leases whose expires_at has passed and flips the corresponding sandbox
// rows to failed. This handles the case where an owning agent died without
// calling ReleaseLease (kill -9, host crash, MIG destroy).
//
// Each tick first renews the leases of locally-owned sandboxes (see
// renewLocalLeases) so a healthy agent's long-lived sandboxes are never
// collateral damage of the staleness sweep that follows.
func (m *Manager) StartLeaseSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.mu.RLock()
				sink := m.leases
				m.mu.RUnlock()
				if sink == nil {
					continue
				}
				m.renewLocalLeases(ctx)
				n, err := sink.SweepStaleLeases(ctx)
				if err != nil {
					m.log.Warn("lease sweeper: scan failed", "err", err)
					continue
				}
				if n > 0 {
					m.log.Info("lease sweeper: marked expired-lease sandboxes failed", "count", n)
				}
			}
		}
	}()
}
