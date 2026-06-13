// SPDX-License-Identifier: Apache-2.0
// Package registry provides agent self-registration and heartbeat against
// the shared metadata store. Each pandastack-agent process registers itself on
// startup (writing its endpoint, region, zone, version into the `agents`
// table) and heartbeats every HeartbeatInterval. The api/scheduler reads
// from the same table to pick a least-loaded agent for sandbox placement.
//
// Same SQL dialect for sqlite (local dev) and postgres (production) - the
// store's placeholder rewriter handles the ?-to-$N translation.
package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// HeartbeatInterval is how often each agent UPDATEs its row.
const HeartbeatInterval = 10 * time.Second

// StaleAfter is the TTL used by schedulers to drop offline agents.
const StaleAfter = 30 * time.Second

// Capacity is the JSON payload an agent reports on every heartbeat. It is
// what the scheduler scores against.
type Capacity struct {
	CPUTotal    int     `json:"cpu_total"`
	CPUUsed     int     `json:"cpu_used"`
	MemoryMB    int     `json:"memory_mb_total"`
	MemoryUsed  int     `json:"memory_mb_used"`
	Sandboxes   int     `json:"sandboxes"`
	LoadAverage float64 `json:"load_average"`

	// StreamRestoreEnabled advertises that this agent has UFFD streaming
	// restore turned on (PANDASTACK_STREAM_RESTORE=1). The scheduler
	// prefers streaming-capable hosts because they boot a template without
	// first downloading the entire vm.mem. omitempty keeps it absent from
	// the heartbeat JSON for agents that have not opted in.
	StreamRestoreEnabled bool `json:"stream_restore_enabled,omitempty"`

	// Volume storage telemetry: lets the scheduler place POST /volumes on
	// the agent with the most storage headroom (volumes are host-pinned
	// sparse ext4 images, so placement is sticky and disk-bound, not
	// CPU-bound). Provisioned = sum of apparent sizes of every *.ext4
	// under <data>/volumes; FS size/free describe the filesystem backing
	// that directory (the stateful PD when mounted, else the data fs).
	// omitempty keeps heartbeats from older agents unchanged; the
	// scheduler treats absent values as "unknown, neutral".
	VolumeProvisionedBytes int64 `json:"volume_provisioned_bytes,omitempty"`
	VolumesFSSizeBytes     int64 `json:"volumes_fs_size_bytes,omitempty"`
	VolumesFSFreeBytes     int64 `json:"volumes_fs_free_bytes,omitempty"`
}

// Agent is the runtime view stored in the `agents` table.
type Agent struct {
	ID            string    `json:"id"`
	Endpoint      string    `json:"endpoint"`
	Region        string    `json:"region"`
	Zone          string    `json:"zone"`
	Version       string    `json:"version"`
	Status        string    `json:"status"`
	Capacity      Capacity  `json:"capacity"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// Registry is the shared metadata client used by both agents (writers) and
// the api/scheduler (readers).
type Registry struct {
	db *sql.DB

	// Identity (only set on agent side).
	id       string
	endpoint string
	region   string
	zone     string
	version  string

	// Mutable capacity reported on every heartbeat.
	capacity atomic.Pointer[Capacity]
}

// New wraps a *sql.DB. Callers may also pass identity for the agent side via
// WithIdentity.
func New(db *sql.DB) *Registry { return &Registry{db: db} }

// WithIdentity sets identity for an agent-side registry. id is typically the
// short GCP instance name; endpoint is the URL where the agent's TCP listener
// is reachable from the edge VPC.
func (r *Registry) WithIdentity(id, endpoint, region, zone, version string) *Registry {
	r.id = id
	r.endpoint = endpoint
	r.region = region
	r.zone = zone
	r.version = version
	return r
}

// Register inserts (or upserts) the row for this agent. It does not start the
// heartbeat loop; call StartHeartbeat for that.
func (r *Registry) Register(ctx context.Context, cap Capacity) error {
	if r.id == "" {
		return errors.New("registry: identity not set; call WithIdentity first")
	}
	r.capacity.Store(&cap)
	capJSON, _ := json.Marshal(cap)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agents (id, endpoint, region, zone, version, status, capacity_json, last_heartbeat, created_at)
		VALUES (?, ?, ?, ?, ?, 'active', ?, `+nowExpr(r.db)+`, `+nowExpr(r.db)+`)
		ON CONFLICT (id) DO UPDATE SET
			endpoint      = excluded.endpoint,
			region        = excluded.region,
			zone          = excluded.zone,
			version       = excluded.version,
			status        = 'active',
			capacity_json = excluded.capacity_json,
			last_heartbeat = `+nowExpr(r.db)+`
	`, r.id, r.endpoint, r.region, r.zone, r.version, string(capJSON))
	return err
}

// SetCapacity updates the in-memory snapshot used by the next heartbeat.
func (r *Registry) SetCapacity(cap Capacity) { r.capacity.Store(&cap) }

// StartHeartbeat runs until ctx is cancelled, refreshing the row every
// HeartbeatInterval. It logs but does not fail on transient DB errors.
func (r *Registry) StartHeartbeat(ctx context.Context, log *slog.Logger) {
	if r.id == "" {
		return
	}
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.heartbeat(ctx); err != nil && log != nil {
				log.Warn("registry heartbeat failed", "err", err, "agent_id", r.id)
			}
		}
	}
}

func (r *Registry) heartbeat(ctx context.Context) error {
	cap := r.capacity.Load()
	var capJSON string
	if cap != nil {
		b, _ := json.Marshal(*cap)
		capJSON = string(b)
	} else {
		capJSON = "{}"
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE agents SET capacity_json = ?, last_heartbeat = `+nowExpr(r.db)+`, status = 'active' WHERE id = ?`,
		capJSON, r.id)
	return err
}

// Deregister marks the agent inactive (does not delete the row, so the
// scheduler can still see its history).
func (r *Registry) Deregister(ctx context.Context) error {
	if r.id == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `UPDATE agents SET status='draining' WHERE id=?`, r.id)
	return err
}

// ListActive returns all agents heartbeating within StaleAfter.
func (r *Registry) ListActive(ctx context.Context) ([]Agent, error) {
	return r.listAgents(ctx, true)
}

// ListAll returns every agent, regardless of heartbeat freshness. Useful for
// admin debugging.
func (r *Registry) ListAll(ctx context.Context) ([]Agent, error) {
	return r.listAgents(ctx, false)
}

func (r *Registry) listAgents(ctx context.Context, onlyFresh bool) ([]Agent, error) {
	q := `SELECT id, endpoint, region, zone, version, status, capacity_json, last_heartbeat FROM agents`
	args := []any{}
	if onlyFresh {
		q += ` WHERE status='active' AND last_heartbeat > ` + agoExpr(r.db, StaleAfter)
	}
	q += ` ORDER BY last_heartbeat DESC`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var capJSON string
		var hb any
		if err := rows.Scan(&a.ID, &a.Endpoint, &a.Region, &a.Zone, &a.Version, &a.Status, &capJSON, &hb); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(capJSON), &a.Capacity)
		a.LastHeartbeat = coerceTime(hb)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AcquireLease records that a sandbox is owned by a given agent until ttl
// elapses without renewal. Existing leases are upserted.
func (r *Registry) AcquireLease(ctx context.Context, sandboxID, agentID, workspaceID string, ttl time.Duration) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO leases (sandbox_id, agent_id, workspace_id, expires_at, created_at)
		VALUES (?, ?, ?, `+futureExpr(r.db, ttl)+`, `+nowExpr(r.db)+`)
		ON CONFLICT (sandbox_id) DO UPDATE SET
			agent_id = excluded.agent_id,
			workspace_id = excluded.workspace_id,
			expires_at = excluded.expires_at
	`, sandboxID, agentID, workspaceID)
	return err
}

// LookupLease returns which agent owns a sandbox or "" if no current lease.
func (r *Registry) LookupLease(ctx context.Context, sandboxID string) (string, error) {
	var agentID string
	row := r.db.QueryRowContext(ctx,
		`SELECT agent_id FROM leases WHERE sandbox_id=? AND expires_at > `+nowExpr(r.db),
		sandboxID)
	if err := row.Scan(&agentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return agentID, nil
}

// ReleaseLease deletes the lease row.
func (r *Registry) ReleaseLease(ctx context.Context, sandboxID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM leases WHERE sandbox_id=?`, sandboxID)
	return err
}

// SweepStaleLeases deletes leases whose expires_at has passed and flips the
// matching sandboxes rows to status='failed' so the dashboard stops trying to
// SSH/exec into a guest whose owning agent died without releasing. Returns
// the number of sandbox rows flipped.
//
// Safe to run on every agent concurrently — the WHERE clauses are idempotent.
func (r *Registry) SweepStaleLeases(ctx context.Context) (int, error) {
	// Collect stale lease sandbox_ids first so we can emit a focused UPDATE.
	rows, err := r.db.QueryContext(ctx,
		`SELECT sandbox_id FROM leases WHERE expires_at <= `+nowExpr(r.db))
	if err != nil {
		return 0, fmt.Errorf("scan stale leases: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return r.markFailedAndReleaseLeases(ctx, ids)
}

// SweepAgentZombies removes leases pointing at agentID for sandboxes not in
// liveIDs and marks the corresponding sandboxes rows failed. Use after an
// agent restart to clear out sandboxes the agent has forgotten about.
//
// liveIDs may be empty (a freshly-booted agent with empty local store) — in
// that case ALL leases for this agent are zombies.
func (r *Registry) SweepAgentZombies(ctx context.Context, agentID string, liveIDs []string) (int, error) {
	if agentID == "" {
		return 0, errors.New("agentID required")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT sandbox_id FROM leases WHERE agent_id=?`, agentID)
	if err != nil {
		return 0, fmt.Errorf("list my leases: %w", err)
	}
	live := make(map[string]struct{}, len(liveIDs))
	for _, id := range liveIDs {
		live[id] = struct{}{}
	}
	var zombies []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := live[id]; !ok {
			zombies = append(zombies, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(zombies) == 0 {
		return 0, nil
	}
	return r.markFailedAndReleaseLeases(ctx, zombies)
}

// markFailedAndReleaseLeases is the shared write path for the two sweepers.
// It flips sandboxes rows to status='failed' (only if they're still in a
// live-ish state — avoids resurrecting deleted rows) and removes the leases.
// Done one-row-at-a-time to stay friendly with the placeholder rewriter
// (which doesn't expand IN (?,?,...) for us).
func (r *Registry) markFailedAndReleaseLeases(ctx context.Context, ids []string) (int, error) {
	flipped := 0
	for _, id := range ids {
		res, err := r.db.ExecContext(ctx, `
			UPDATE sandboxes SET status='failed'
			WHERE id=? AND status IN ('running','paused','hibernated')
		`, id)
		if err != nil {
			return flipped, fmt.Errorf("flip %s: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			flipped++
		}
		if _, err := r.db.ExecContext(ctx,
			`DELETE FROM leases WHERE sandbox_id=?`, id); err != nil {
			return flipped, fmt.Errorf("delete lease %s: %w", id, err)
		}
	}
	return flipped, nil
}

// EmitEvent broadcasts a sandbox event over Supabase Realtime (the api/dashboard
// subscribe to `sandbox_events` row INSERTs filtered by workspace_id via RLS).
func (r *Registry) EmitEvent(ctx context.Context, sandboxID, workspaceID, eventType, code, message string, metadata map[string]any) error {
	mdJSON := "{}"
	if metadata != nil {
		b, _ := json.Marshal(metadata)
		mdJSON = string(b)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sandbox_events (sandbox_id, workspace_id, type, code, message, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, `+nowExpr(r.db)+`)
	`, sandboxID, workspaceID, eventType, code, message, mdJSON)
	return err
}

// --- dialect-aware time expressions ----------------------------------------
//
// We try postgres-style first (since Supabase is the production target) and
// fall back to sqlite-style for local dev. The store layer transparently
// rewrites ? placeholders so query bodies share the same template.

func nowExpr(db *sql.DB) string {
	if isPostgres(db) {
		return "now()"
	}
	return "strftime('%s','now')"
}

func futureExpr(db *sql.DB, d time.Duration) string {
	if isPostgres(db) {
		return fmt.Sprintf("now() + interval '%d seconds'", int(d.Seconds()))
	}
	return fmt.Sprintf("strftime('%%s','now') + %d", int(d.Seconds()))
}

func agoExpr(db *sql.DB, d time.Duration) string {
	if isPostgres(db) {
		return fmt.Sprintf("now() - interval '%d seconds'", int(d.Seconds()))
	}
	return fmt.Sprintf("strftime('%%s','now') - %d", int(d.Seconds()))
}

func isPostgres(db *sql.DB) bool {
	if db == nil {
		return false
	}
	d := fmt.Sprintf("%T", db.Driver())
	return d == "*stdlib.Driver"
}

func coerceTime(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case int64:
		return time.Unix(x, 0).UTC()
	case []byte:
		t, err := time.Parse(time.RFC3339Nano, string(x))
		if err == nil {
			return t
		}
	case string:
		t, err := time.Parse(time.RFC3339Nano, x)
		if err == nil {
			return t
		}
	}
	return time.Time{}
}
