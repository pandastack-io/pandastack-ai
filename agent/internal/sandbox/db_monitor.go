// SPDX-License-Identifier: Apache-2.0
//
// db_monitor.go — health reconcile loop for managed databases (P1 item 1).
//
// The per-VM health monitor watches the FIRECRACKER process; nothing watched
// POSTGRES. A database whose postgres crashed or wedged kept reporting
// "running" while every connection failed, and nothing ever restarted it or
// marked the row failed (so failover_available never surfaced either).
//
// This loop probes each running managed database with pg_isready. On
// consecutive postgres-level failures it restarts postgres inside the guest
// (the same pg_ctl the template's autostart uses); if restarts don't stick —
// or the restart budget is exhausted — it marks the sandbox row "failed",
// which is exactly the state the failover preflight accepts.
//
// SAFETY RULES (each one closes a reviewed failure mode):
//   - Only judge a database the loop has SEEN HEALTHY at least once. A DB
//     that never answered pg_isready is bootstrapping, WAL-replaying after a
//     restore, or genuinely broken-from-birth — none of which an automated
//     restart improves, and two of which it would corrupt.
//   - Only a REAL pg_isready non-zero exit counts as a failure. Transport
//     trouble (SSH down, host load, VM paused) proves nothing about
//     postgres; corroborate the VM is even alive (pidAlive) before judging.
//   - Re-read the row status immediately before ACTING, and write "failed"
//     only via compare-and-swap from "running" — a concurrent hibernate must
//     win, never be overwritten (the review's headline race: hibernate keeps
//     the row "running" until its final step).
//   - A store read error skips the whole tick, including state cleanup —
//     otherwise one shared-Postgres hiccup would wipe every restart budget.

package sandbox

import (
	"context"
	"fmt"
	"os"
	"time"
)

const (
	// dbMonitorInterval: probe cadence. pg_isready is a no-op socket ping.
	dbMonitorInterval = 30 * time.Second
	// dbMonitorFailThreshold: consecutive REAL pg failures before acting.
	dbMonitorFailThreshold = 2
	// dbMonitorMaxRestarts / dbMonitorRestartWindow: restart budget. A
	// postgres that keeps dying is not going to be fixed by a fourth
	// restart — mark it failed and surface failover instead of flapping.
	dbMonitorMaxRestarts   = 3
	dbMonitorRestartWindow = time.Hour

	// dbPGBin/dbPGData mirror templates/postgres-16/scripts/autostart.sh
	// (PG_BIN and PGDATA). Keep in sync with the template.
	dbPGBin  = "/usr/lib/postgresql/16/bin"
	dbPGData = "/var/lib/postgresql/data/pgdata"
)

// dbProbeResult is the tri-state outcome of a health probe. Only
// dbProbePGDown is evidence against postgres; dbProbeUnreachable is evidence
// about the HOST/VM/SSH path and must never trigger a restart or failure.
type dbProbeResult int

const (
	dbProbeOK dbProbeResult = iota
	dbProbePGDown
	dbProbeUnreachable
)

// StartDBMonitor launches the managed-database health loop. Call once at
// agent startup; returns immediately.
func (m *Manager) StartDBMonitor(ctx context.Context) {
	go m.runDBMonitor(ctx)
}

func (m *Manager) runDBMonitor(ctx context.Context) {
	failures := map[string]int{}         // id -> consecutive REAL pg failures
	seenHealthy := map[string]bool{}     // id -> answered pg_isready at least once
	restarts := map[string][]time.Time{} // id -> restart timestamps (budget)

	t := time.NewTicker(dbMonitorInterval)
	defer t.Stop()
	m.log.Info("db monitor: started", "interval", dbMonitorInterval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		m.mu.RLock()
		ids := make([]string, 0, len(m.drivers))
		for id := range m.drivers {
			ids = append(ids, id)
		}
		m.mu.RUnlock()

		// One bulk status read per tick: only act on rows the store says are
		// RUNNING. On a store error, skip the ENTIRE tick — including the
		// cleanup below — so a transient control-plane hiccup can't reset
		// every database's restart budget and healthy-history.
		statuses, err := m.sandboxStatuses(ctx)
		if err != nil {
			m.log.Warn("db monitor: status read failed; skipping tick", "err", err)
			continue
		}

		live := map[string]bool{}
		for _, id := range ids {
			if _, err := os.Stat(m.dbVolumePath(id)); err != nil {
				continue // not a managed database
			}
			if statuses[id] != string(StatusRunning) {
				continue // hibernating/waking/failed/absent — not ours to judge
			}
			live[id] = true

			switch m.dbProbe(ctx, id) {
			case dbProbeOK:
				seenHealthy[id] = true
				failures[id] = 0
				continue
			case dbProbeUnreachable:
				// SSH/host trouble, or the VM went away mid-tick. Says
				// nothing about postgres — the VM-level monitor owns dead
				// VMs. Don't count it either way.
				m.log.Debug("db monitor: guest unreachable (not counted)", "id", id)
				continue
			case dbProbePGDown:
				if !seenHealthy[id] {
					// Never seen healthy: bootstrapping (initdb), replaying
					// WAL after a restore, or broken from birth. Restarting
					// mid-bootstrap corrupts; marking failed pre-judges.
					// Leave it to converge; log so a stuck one is visible.
					m.log.Info("db monitor: postgres not up yet (never seen healthy; waiting)", "id", id)
					continue
				}
			}

			// Real failure on a database we know was healthy.
			failures[id]++
			m.log.Warn("db monitor: pg_isready failed", "id", id, "consecutive", failures[id])
			if failures[id] < dbMonitorFailThreshold {
				continue
			}

			// Corroborate the VM is alive before touching anything: a dead
			// firecracker is the VM monitor's case, not ours.
			m.mu.RLock()
			drv := m.drivers[id]
			m.mu.RUnlock()
			if drv == nil || !pidAlive(drv.PID()) {
				m.log.Info("db monitor: VM not alive; leaving to VM monitor", "id", id)
				failures[id] = 0
				continue
			}

			// Re-read THIS row's status immediately before acting: minutes
			// can pass inside a tick, and a hibernate may have started since
			// the bulk read (its row stays "running" until its final step,
			// so the CAS below is the real guard — this check just avoids
			// pointless restarts).
			if st, _ := m.store.GetStatus(ctx, id); st != string(StatusRunning) {
				m.log.Info("db monitor: status changed mid-tick; standing down", "id", id, "status", st)
				failures[id] = 0
				continue
			}

			// Prune restart history to the window, then check the budget.
			recent := restarts[id][:0]
			for _, ts := range restarts[id] {
				if time.Since(ts) < dbMonitorRestartWindow {
					recent = append(recent, ts)
				}
			}
			restarts[id] = recent
			if len(recent) >= dbMonitorMaxRestarts {
				m.log.Error("db monitor: restart budget exhausted — marking database failed",
					"id", id, "restarts_in_window", len(recent))
				m.dbMarkFailed(ctx, id, "postgres unresponsive; restart budget exhausted")
				failures[id] = 0
				continue
			}

			restarts[id] = append(restarts[id], time.Now())
			restarted, transport := m.dbRestart(ctx, id)
			if transport {
				// The restart couldn't even be attempted (SSH/exec failure).
				// That's reachability, not postgres — retry next tick, and
				// give the budget slot back.
				restarts[id] = restarts[id][:len(restarts[id])-1]
				m.log.Warn("db monitor: restart not attempted (guest unreachable); retrying next tick", "id", id)
				continue
			}
			if restarted && m.dbProbe(ctx, id) == dbProbeOK {
				m.log.Info("db monitor: postgres restarted in place", "id", id,
					"restarts_in_window", len(restarts[id]))
				if m.bus != nil {
					m.bus.Emit(id, "database.pg_restarted", map[string]any{
						"restarts_in_window": len(restarts[id]),
					})
				}
				failures[id] = 0
				continue
			}
			m.log.Error("db monitor: in-place restart did not stick — marking database failed", "id", id)
			m.dbMarkFailed(ctx, id, "postgres unresponsive; in-place restart failed")
			failures[id] = 0
		}

		// Drop state for databases that no longer run here (deleted,
		// hibernated, failed over away). Only reached on a successful status
		// read, so a store hiccup can't wipe budgets.
		for id := range seenHealthy {
			if !live[id] {
				delete(seenHealthy, id)
				delete(failures, id)
				delete(restarts, id)
			}
		}
	}
}

// sandboxStatuses returns id→status for this agent's rows.
func (m *Manager) sandboxStatuses(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := m.store.ListSandboxesForAgent(sctx, m.agentID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		rmap, _ := row.(map[string]any)
		if rmap == nil {
			continue
		}
		id, _ := rmap["id"].(string)
		status, _ := rmap["status"].(string)
		if id != "" {
			out[id] = status
		}
	}
	return out, nil
}

// dbProbe distinguishes "postgres says no" from "couldn't ask postgres".
func (m *Manager) dbProbe(ctx context.Context, id string) dbProbeResult {
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	gc, err := m.Guest(id)
	if err != nil {
		return dbProbeUnreachable
	}
	res, err := gc.Exec(pctx, "sudo -u postgres "+dbPGBin+"/pg_isready -q")
	if err != nil {
		return dbProbeUnreachable // SSH/exec transport failure
	}
	if res.ExitCode == 0 {
		return dbProbeOK
	}
	return dbProbePGDown
}

// dbRestart restarts (or starts, if it exited entirely) postgres inside the
// guest — the same pg_ctl + PGDATA the template's autostart.sh uses.
// Returns (restarted, transportFailure).
func (m *Manager) dbRestart(ctx context.Context, id string) (bool, bool) {
	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	gc, err := m.Guest(id)
	if err != nil {
		return false, true
	}
	cmd := fmt.Sprintf(
		"bash -c 'sudo -u postgres %s/pg_ctl -D %s restart -m fast -w -t 60"+
			" || sudo -u postgres %s/pg_ctl -D %s start -w -t 60'",
		dbPGBin, dbPGData, dbPGBin, dbPGData)
	res, err := gc.Exec(rctx, cmd)
	if err != nil {
		return false, true // couldn't even run it — reachability, not postgres
	}
	if res.ExitCode != 0 {
		m.log.Warn("db monitor: pg_ctl restart exited non-zero", "id", id, "exit", res.ExitCode)
		return false, false
	}
	return true, false
}

// dbMarkFailed flips the sandbox row running→failed via COMPARE-AND-SWAP —
// if anything else (hibernate, delete, wake) changed the status since we
// judged it, that transition wins and we stand down.
func (m *Manager) dbMarkFailed(ctx context.Context, id, reason string) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	swapped, err := m.store.SetStatusIf(sctx, id, string(StatusRunning), string(StatusFailed))
	if err != nil {
		m.log.Error("db monitor: mark failed errored", "id", id, "err", err)
		return
	}
	if !swapped {
		m.log.Info("db monitor: status changed concurrently; NOT marking failed", "id", id)
		return
	}
	if m.bus != nil {
		m.bus.Emit(id, "database.failed", map[string]any{"reason": reason})
	}
	m.chEvent(m.sandboxWorkspaces(sctx)[id], id, "failed", "db-monitor", reason, nil)
}
