// SPDX-License-Identifier: Apache-2.0
//
// db_idle.go — transparent auto-suspend for managed databases.
//
// A managed database is a persistent sandbox: it runs 24/7 and holds its full
// RAM and vCPU reservation on the host whether or not anything is querying it.
// On a box with many small databases that is the dominant source of wasted
// capacity. This sweep hibernates a database that has had NO real activity for
// a configurable window; db-proxy + the broker proxy wake it transparently on
// the next connection (wake-on-connect), so a client sees only a slightly
// slower first query after an idle period — never an action to take. A
// suspended database occupies disk, not memory or cores.
//
// Gated OFF by default (PANDASTACK_DB_IDLE_AFTER_SECONDS=0). Enabling it is a
// deliberate fleet decision — wake-on-connect must be proven first, because a
// database that suspends but won't wake is worse than one that never suspends.
//
// Idle signal — postgres-native ground truth, NOT agent-request activity.
// A native postgres client holds a long, quiet connection through the
// pg-tunnel; the agent sees no requests once it's established, so
// request-based idleness (the generic RunIdleSweeper) would suspend a DB with
// a live client. We ask postgres directly (dbActivityProbe) whether any
// NON-platform client is connected, querying, recently-active, or a base
// backup is streaming — see that function for the exact signals.
//
// CRITICAL: PandaStack's OWN postgres queries must be excluded, or the
// platform's health/observability traffic looks like customer activity and
// the database never appears idle. The first cut used a cluster-wide
// transaction-counter delta and was defeated by its own probe: the 30s probe
// SELECT advanced xact_commit every tick, so the delta was never zero. The
// fix is application_name tagging — the probe and the dashboard stats poll
// carry known application_names that the signal filters out.

package sandbox

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// dbIdleSweepInterval: how often the idle sweep re-evaluates each DB.
	dbIdleSweepInterval = 30 * time.Second
	// dbIdleProbeTimeout bounds one activity probe (guest SSH + psql).
	dbIdleProbeTimeout = 15 * time.Second
)

// dbIdleState tracks one database's idle progress across ticks.
type dbIdleState struct {
	idleSince time.Time // zero = not currently idle
	seenBusy  bool      // has this DB ever shown activity? (don't suspend a DB
	// that has never been used since we started watching — it may be
	// mid-first-connection or a bootstrapping clone)
}

// dbIdleProbeAppNames are the application_name values of PandaStack's OWN
// postgres connections — the idle probe itself and the dashboard stats poll.
// They MUST be excluded from the activity signal, or the platform's own
// health/observability queries look like customer traffic and the database
// never appears idle (the self-defeating-probe bug: the 30s probe query
// advanced the transaction counter every tick). Keep in sync with the
// application_name set on those connections (dbStatsAppName in the API, and
// the probe's own conninfo below).
const dbIdleProbeAppName = "pds-idle-probe"

// dbIdleRecentActivityWindow: a client backend whose last state change is
// within this window counts as active, so a query that ran between two 30s
// ticks on a still-open (now-idle) connection isn't missed. Comfortably wider
// than the tick so no in-between query slips through.
const dbIdleRecentActivityWindow = 120 // seconds

// dbIdleAfter returns the configured idle-suspend window, or 0 (disabled).
func dbIdleAfter() time.Duration {
	v := strings.TrimSpace(os.Getenv("PANDASTACK_DB_IDLE_AFTER_SECONDS"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// StartDBIdleSweep launches the managed-database auto-suspend loop, or returns
// immediately when disabled. Call once at agent startup.
func (m *Manager) StartDBIdleSweep(ctx context.Context) {
	idleAfter := dbIdleAfter()
	if idleAfter <= 0 {
		m.log.Info("db idle-suspend: disabled (PANDASTACK_DB_IDLE_AFTER_SECONDS unset/0)")
		return
	}
	go m.runDBIdleSweep(ctx, idleAfter)
}

func (m *Manager) runDBIdleSweep(ctx context.Context, idleAfter time.Duration) {
	states := map[string]*dbIdleState{}
	t := time.NewTicker(dbIdleSweepInterval)
	defer t.Stop()
	m.log.Info("db idle-suspend: started", "idle_after", idleAfter.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		// One bulk status read per tick, same discipline as the health
		// monitor: on a store error, skip the whole tick (don't corrupt
		// idle-since tracking on a transient control-plane hiccup).
		statuses, err := m.sandboxStatuses(ctx)
		if err != nil {
			m.log.Warn("db idle-suspend: status read failed; skipping tick", "err", err)
			continue
		}

		m.mu.RLock()
		ids := make([]string, 0, len(m.drivers))
		for id := range m.drivers {
			ids = append(ids, id)
		}
		m.mu.RUnlock()

		live := map[string]bool{}
		for _, id := range ids {
			if _, statErr := os.Stat(m.dbVolumePath(id)); statErr != nil {
				continue // not a managed database
			}
			if statuses[id] != string(StatusRunning) {
				continue // hibernating/waking/failed — not ours to judge
			}
			live[id] = true

			st := states[id]
			if st == nil {
				st = &dbIdleState{}
				states[id] = st
			}

			// always_on: never auto-suspend. Reset any idle progress so a
			// later un-set behaves cleanly.
			if m.dbAlwaysOn(ctx, id) {
				st.idleSince = time.Time{}
				continue
			}

			active, ok := m.dbActivityProbe(ctx, id)
			if !ok {
				// Couldn't ask postgres (SSH/exec transport, or the VM went
				// away mid-tick). Says nothing about activity — leave
				// idle-since untouched and try next tick. Never suspend on
				// a failed probe.
				continue
			}

			if active {
				st.seenBusy = true
				st.idleSince = time.Time{}
				continue
			}
			if !st.seenBusy {
				// Never observed activity since we started watching. Could be
				// a brand-new DB still finishing its first connection, or a
				// freshly-woken one. Start the idle clock now rather than
				// suspending immediately, so a just-created DB isn't slept
				// before its owner first connects.
				st.seenBusy = true
				st.idleSince = time.Now()
				continue
			}
			if st.idleSince.IsZero() {
				st.idleSince = time.Now()
				continue
			}
			if time.Since(st.idleSince) < idleAfter {
				continue
			}

			// Idle past the window → hibernate. Re-read the row status right
			// before acting (minutes can pass across a tick), and never sleep
			// a DB under a live pg-tunnel — a native client is mid-session
			// even if this instant looked quiet.
			if cur, _ := m.store.GetStatus(ctx, id); cur != string(StatusRunning) {
				st.idleSince = time.Time{}
				continue
			}
			if m.ActiveTunnels(id) > 0 {
				st.idleSince = time.Time{}
				continue
			}
			m.log.Info("db idle-suspend: hibernating idle database",
				"id", id, "idle_for", time.Since(st.idleSince).String())
			if m.bus != nil {
				m.bus.Emit(id, "database.idle_suspended", map[string]any{
					"idle_seconds": int(time.Since(st.idleSince).Seconds()),
				})
			}
			err := m.Hibernate(ctx, id)
			_, capped := m.noteHibernateResult(id, err)
			if err != nil {
				if capped {
					m.log.Error("db idle-suspend: giving up after repeated hibernate failures; leaving running", "id", id)
				} else {
					m.log.Warn("db idle-suspend: hibernate failed", "id", id, "err", err)
				}
				// Re-arm the idle clock so we back off a full idle window
				// instead of re-attempting every 30s. Unlike the generic
				// RunIdleSweeper, this sweep does not key on lastActivity, so
				// noteHibernateResult's give-back would not throttle it — a DB
				// that can't hibernate would pause/snapshot/resume-storm every
				// tick without this reset.
				st.idleSince = time.Now()
				continue
			}
			// Hibernated: drop tracking; the DB is no longer in `live`, and
			// the cleanup below removes its state.
			delete(states, id)
		}

		// Drop state for databases that no longer run here.
		for id := range states {
			if !live[id] {
				delete(states, id)
			}
		}
	}
}

// dbActivityProbe asks postgres whether the database is being used right now,
// robustly against PandaStack's OWN queries. Returns (active, ok): active =
// some non-platform client is connected/querying, or a base backup is
// streaming; ok = the probe actually ran (false on any transport/exec failure
// — callers must NOT treat !ok as idle).
//
// Signals (a match on any ⇒ active), over ALL non-template user databases
// (the role has CREATEDB and the broker serves arbitrary DBs):
//  1. a NON-loopback client backend — a real native psql/app client on the
//     pg-tunnel (pgbouncer + the in-VM broker are 127.0.0.1);
//  2. a client backend in active / idle-in-transaction — work happening now;
//  3. a client backend whose last state change is within the recent window —
//     a query that ran between ticks on a still-open (now-idle) connection,
//     incl. customer traffic through the broker's pooled connection;
//  4. a walsender — an in-flight pg_basebackup / replication (never abort it).
//
// Platform connections are EXCLUDED by application_name (the probe's own,
// tagged in the conninfo below, and the dashboard stats poll, dbStatsAppName)
// and by pg_backend_pid(). This is what fixes the self-defeating probe: the
// old transaction-counter delta counted the probe's own 30s query as activity,
// so the database never looked idle.
func (m *Manager) dbActivityProbe(ctx context.Context, id string) (active bool, ok bool) {
	pctx, cancel := context.WithTimeout(ctx, dbIdleProbeTimeout)
	defer cancel()
	gc, err := m.Guest(id)
	if err != nil {
		return false, false
	}
	q := fmt.Sprintf(`SELECT
  (SELECT count(*) FROM pg_stat_activity
     WHERE backend_type='client backend' AND pid<>pg_backend_pid()
       AND coalesce(datname,'') NOT IN ('template0','template1')
       AND coalesce(application_name,'') NOT IN ('%s','pds-stats')
       AND (
         (client_addr IS NOT NULL AND host(client_addr) NOT IN ('127.0.0.1','::1'))
         OR state IN ('active','idle in transaction','idle in transaction (aborted)','fastpath function call')
         OR state_change > now() - interval '%d seconds'
       )),
  (SELECT count(*) FROM pg_stat_activity WHERE backend_type='walsender')`,
		dbIdleProbeAppName, dbIdleRecentActivityWindow)
	// Connect with application_name=pds-idle-probe (via libpq conninfo) so this
	// very query is filtered out of the signal above.
	cmd := "sudo -u postgres " + dbPGBin + "/psql \"dbname=pandastack application_name=" + dbIdleProbeAppName + "\" -tAF'|' -c \"" + strings.ReplaceAll(q, "\n", " ") + "\""
	res, err := gc.Exec(pctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return false, false
	}
	fields := strings.Split(strings.TrimSpace(res.Stdout), "|")
	if len(fields) != 2 {
		return false, false
	}
	clients, e1 := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
	walsenders, e2 := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
	if e1 != nil || e2 != nil {
		return false, false
	}
	return clients > 0 || walsenders > 0, true
}

// dbAlwaysOn reports whether the database opted out of auto-suspend via
// metadata db.always_on. Best-effort: an unreadable row defaults to NOT
// always-on (auto-suspend eligible) — the sweep's other guards still protect
// a genuinely-active DB, and a transient read error must not pin a DB awake
// forever.
func (m *Manager) dbAlwaysOn(ctx context.Context, id string) bool {
	row, err := m.store.GetSandbox(ctx, id)
	if err != nil || row == nil {
		return false
	}
	rmap, _ := row.(map[string]any)
	if rmap == nil {
		return false
	}
	if md, ok := rmap["metadata"].(map[string]string); ok {
		v := strings.ToLower(strings.TrimSpace(md["db.always_on"]))
		return v == "true" || v == "1" || v == "yes"
	}
	return false
}
