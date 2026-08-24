// SPDX-License-Identifier: Apache-2.0
// Package store is the metadata layer for the agent.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	// agentID stamps every InsertSandbox row with this agent's identity
	// (PANDASTACK_AGENT_ID, hostname fallback — set via SetAgentID from main).
	// On the shared multi-node Postgres this is the ownership boundary: agents
	// only judge/reclaim rows whose agent_id matches their own. Empty (never
	// set, or legacy rows) means "unowned" — see Manager.ownsRow.
	agentID string
}

// SetAgentID records this agent's identity for row-ownership stamping.
func (s *Store) SetAgentID(id string) { s.agentID = id }

// AgentID returns this agent's identity (set via SetAgentID). Empty before
// the manager has resolved it.
func (s *Store) AgentID() string { return s.agentID }

type NetworkState struct {
	NextSubnet   uint32
	NextVsockCID uint32
}

func Open(path string) (*Store, error) {
	driverName := normalizeDriver(os.Getenv("PANDASTACK_DB_DRIVER"))
	dsn := os.Getenv("PANDASTACK_DB_DSN")
	if dsn == "" {
		dsn = path
	}
	db, err := OpenDBForDriver(driverName, dsn)
	if err != nil {
		return nil, err
	}
	if err := runMigrations(driverName, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB so other packages (registry, scheduler)
// can run their own queries using the same shared connection pool.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	// Add workspace column to boot_events if it doesn't exist (added in v2).
	_, _ = s.db.Exec(`ALTER TABLE boot_events ADD COLUMN workspace TEXT NOT NULL DEFAULT ''`)
	return nil
}

// --- sandboxes --------------------------------------------------------------

// (Type only declared in sandbox package; here we use raw maps to avoid an import cycle.)

type sandboxRow struct {
	ID, Template, Status, GuestIP, HostTAP, MAC, FromSnapshot, Metadata string
	CPU, MemoryMB                                                       int
	VsockCID                                                            uint32
	CreatedAt                                                           int64
	BootMS                                                              int64
	BootMode                                                            string
	MeteredAt                                                           int64
	AgentID                                                             string
}

// InsertSandbox stores a sandbox row. The caller passes any struct with json tags
// matching the schema (the sandbox.Sandbox type does).
func (s *Store) InsertSandbox(ctx context.Context, sb any) error {
	r, err := toRow(sb)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sandboxes (id, template, cpu, memory_mb, status, guest_ip, host_tap, mac, vsock_cid, from_snapshot, metadata, created_at, boot_ms, boot_mode, agent_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Template, r.CPU, r.MemoryMB, r.Status, r.GuestIP, r.HostTAP, r.MAC, r.VsockCID, r.FromSnapshot, r.Metadata, r.CreatedAt, r.BootMS, r.BootMode, s.agentID)
	return err
}

// SetSandboxAgentID claims a row for an agent — used to adopt legacy rows
// (created before the agent_id column existed) once the agent has verified it
// holds local evidence for the sandbox (vm dir / FC socket / durable volume).
func (s *Store) SetSandboxAgentID(ctx context.Context, id, agentID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET agent_id=? WHERE id=?`, agentID, id)
	return err
}

// UpdateSandbox rewrites the mutable columns of an existing row from a full
// struct. created_at is deliberately NOT in the SET list: it is immutable, and
// writing it here was a live data bug — callers that rebuild a *Sandbox from
// a row (GetTyped) left CreatedAt at the Go zero value, so a metadata patch or
// a status flip stamped created_at = -62135596800 — an age of ~2000 years,
// which every age-based sweep and every dashboard read then believed. Nothing
// may resurrect this column here.
func (s *Store) UpdateSandbox(ctx context.Context, sb any) error {
	r, err := toRow(sb)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE sandboxes SET template=?, cpu=?, memory_mb=?, status=?, guest_ip=?, host_tap=?, mac=?, vsock_cid=?, from_snapshot=?, metadata=?, boot_ms=?, boot_mode=?
		WHERE id=?`,
		r.Template, r.CPU, r.MemoryMB, r.Status, r.GuestIP, r.HostTAP, r.MAC, r.VsockCID, r.FromSnapshot, r.Metadata, r.BootMS, r.BootMode, r.ID)
	return err
}

func (s *Store) SetStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET status=? WHERE id=?`, status, id)
	return err
}

// GetStatus returns the row's current status ("" when the row is absent).
func (s *Store) GetStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM sandboxes WHERE id=?`, id).Scan(&status)
	if err != nil {
		return "", err
	}
	return status, nil
}

// SetStatusIf updates the status ONLY when the row currently has `from` —
// a compare-and-swap for reconcilers whose judgement can be stale by the
// time they write (e.g. the DB monitor marking `failed`: a concurrent
// hibernate must win, not be overwritten). Returns true when the swap
// happened.
func (s *Store) SetStatusIf(ctx context.Context, id, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sandboxes SET status=? WHERE id=? AND status=?`, to, id, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetSandboxNetwork updates ONLY the network-identity columns for a sandbox.
// Used by the wake netns-recovery path: when a hibernated NATID sandbox's netns
// was torn down across an agent restart and we re-allocate a fresh one, the
// host-side proxy dial target (guest_ip) and netns name (host_tap) change. The
// pg-tunnel/SSH paths read guest_ip from this row, so it must reflect the new
// allocation. Scoped to three columns (vs UpdateSandbox, which rewrites every
// column from a full struct) to avoid clobbering unrelated fields.
func (s *Store) SetSandboxNetwork(ctx context.Context, id, guestIP, hostTAP, mac string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sandboxes SET guest_ip=?, host_tap=?, mac=? WHERE id=?`,
		guestIP, hostTAP, mac, id)
	return err
}

// SetSandboxLifecycle persists the lifecycle config (persistent flag + ttl) for a
// sandbox so an agent restart can rehydrate it instead of falling back to the
// default TTL. `persistent` is stored as 0/1 in a BIGINT/INTEGER column to keep
// scanning uniform across the sqlite and postgres drivers.
func (s *Store) SetSandboxLifecycle(ctx context.Context, id string, persistent bool, ttlSeconds int64) (rows int64, err error) {
	p := int64(0)
	if persistent {
		p = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET persistent=?, ttl_seconds=? WHERE id=?`, p, ttlSeconds, id)
	if err != nil {
		return 0, err
	}
	rows, _ = res.RowsAffected()
	return rows, nil
}

// GetSandboxLifecycle reads the persisted lifecycle config for a sandbox. found is
// false when no row exists for the id.
func (s *Store) GetSandboxLifecycle(ctx context.Context, id string) (persistent bool, ttlSeconds int64, found bool, err error) {
	var p int64
	row := s.db.QueryRowContext(ctx, `SELECT persistent, ttl_seconds FROM sandboxes WHERE id=?`, id)
	if err = row.Scan(&p, &ttlSeconds); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, 0, false, nil
		}
		return false, 0, false, err
	}
	return p != 0, ttlSeconds, true, nil
}

func (s *Store) DeleteSandbox(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sandboxes WHERE id=?`, id)
	return err
}

func (s *Store) GetSandbox(ctx context.Context, id string) (any, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template, cpu, memory_mb, status, guest_ip, host_tap, mac, vsock_cid, from_snapshot, metadata, created_at, boot_ms, boot_mode, metered_at, agent_id FROM sandboxes WHERE id=?`, id)
	return scanSandbox(row.Scan)
}

const sandboxCols = `id, template, cpu, memory_mb, status, guest_ip, host_tap, mac, vsock_cid, from_snapshot, metadata, created_at, boot_ms, boot_mode, metered_at, agent_id`

// ListSandboxes returns every sandbox row, fleet-wide. On the shared multi-node
// Postgres this is EVERY tenant's rows across EVERY agent — only the two
// genuinely fleet-wide callers should use it (the public List() API handler and
// fork-tree promotion, which spans hosts). Every per-agent caller (Recover,
// janitor, meters, capacity pump, slot reconcile) must use
// ListSandboxesForAgent to avoid an O(agents×fleet) scan of the shared table on
// each of their timers.
func (s *Store) ListSandboxes(ctx context.Context) ([]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sandboxCols+` FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	return scanSandboxRows(rows)
}

// ListSandboxesForAgent returns only the rows this agent may act on: rows it
// owns (agent_id == agentID) PLUS legacy unclaimed rows (agent_id == ”, created
// before the ownership column). It is a strict superset of what ownsRow admits
// for this agent — the legacy rows are still adjudicated by ownsRow's
// local-evidence check in Go — so callers see identical results to a fleet-wide
// list + ownsRow filter, but without pulling every peer's rows over the wire.
// The sandboxes_agent_idx index (migration 00017) serves the agent_id predicate.
func (s *Store) ListSandboxesForAgent(ctx context.Context, agentID string) ([]any, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE agent_id=? OR agent_id='' ORDER BY created_at DESC`,
		agentID)
	if err != nil {
		return nil, err
	}
	return scanSandboxRows(rows)
}

func scanSandboxRows(rows *sql.Rows) ([]any, error) {
	defer rows.Close()
	var out []any
	for rows.Next() {
		sb, err := scanSandbox(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func scanSandbox(scan func(...any) error) (any, error) {
	var r sandboxRow
	if err := scan(&r.ID, &r.Template, &r.CPU, &r.MemoryMB, &r.Status, &r.GuestIP, &r.HostTAP, &r.MAC, &r.VsockCID, &r.FromSnapshot, &r.Metadata, &r.CreatedAt, &r.BootMS, &r.BootMode, &r.MeteredAt, &r.AgentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	m := map[string]any{
		"id":            r.ID,
		"template":      r.Template,
		"cpu":           r.CPU,
		"memory_mb":     r.MemoryMB,
		"status":        r.Status,
		"guest_ip":      r.GuestIP,
		"host_tap":      r.HostTAP,
		"mac":           r.MAC,
		"vsock_cid":     r.VsockCID,
		"from_snapshot": r.FromSnapshot,
		"created_at":    time.Unix(r.CreatedAt, 0).UTC(),
		"boot_ms":       r.BootMS,
		"boot_mode":     r.BootMode,
		"agent_id":      r.AgentID,
	}
	if r.MeteredAt > 0 {
		m["metered_at"] = time.Unix(r.MeteredAt, 0).UTC()
	}
	if r.Metadata != "" {
		var md map[string]string
		_ = json.Unmarshal([]byte(r.Metadata), &md)
		m["metadata"] = md
	}
	return m, nil
}

// --- allocations ------------------------------------------------------------

func (s *Store) SaveAllocation(ctx context.Context, alloc any) error {
	b, err := json.Marshal(alloc)
	if err != nil {
		return err
	}
	id, ok := fieldStr(alloc, "sandbox_id")
	if !ok {
		return errors.New("allocation missing sandbox_id")
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO allocations (sandbox_id, payload) VALUES (?,?)`, id, string(b))
	return err
}

func (s *Store) GetAllocation(ctx context.Context, sandboxID string) (allocation, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM allocations WHERE sandbox_id=?`, sandboxID).Scan(&payload)
	if err != nil {
		return allocation{}, err
	}
	var a allocation
	return a, json.Unmarshal([]byte(payload), &a)
}

// GetAllocationJSON returns the raw JSON payload for a sandbox's allocation.
func (s *Store) GetAllocationJSON(ctx context.Context, sandboxID string) (string, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM allocations WHERE sandbox_id=?`, sandboxID).Scan(&payload)
	return payload, err
}

func (s *Store) DeleteAllocation(ctx context.Context, sandboxID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM allocations WHERE sandbox_id=?`, sandboxID)
	return err
}

// allocation is just enough to find the TAP name (or netns/veth pair) for
// teardown. The /30 slot index is owned by the slotstore, not derived from this
// payload, so no Idx/Subnet-reconcile fields are needed here.
type allocation struct {
	TAP    string `json:"tap"`
	Subnet string `json:"subnet"`
}

// --- network state ----------------------------------------------------------

func (s *Store) LoadNetworkState(ctx context.Context) (NetworkState, error) {
	var ns NetworkState
	err := s.db.QueryRowContext(ctx, `SELECT next_subnet, next_vsock_cid FROM network_state WHERE id=1`).Scan(&ns.NextSubnet, &ns.NextVsockCID)
	if errors.Is(err, sql.ErrNoRows) {
		return NetworkState{NextSubnet: 0, NextVsockCID: 3}, nil
	}
	return ns, err
}

func (s *Store) SaveNetworkState(ctx context.Context, ns NetworkState) error {
	_, err := s.db.ExecContext(ctx, `UPDATE network_state SET next_subnet=?, next_vsock_cid=? WHERE id=1`, ns.NextSubnet, ns.NextVsockCID)
	return err
}

// SaveVsockCID advances ONLY the vsock CID high-water mark, leaving next_subnet
// untouched. Slot indices are owned by the slotstore now, so next_subnet is
// vestigial — but we must not zero it via a full SaveNetworkState write.
func (s *Store) SaveVsockCID(ctx context.Context, nextCID uint32) error {
	_, err := s.db.ExecContext(ctx, `UPDATE network_state SET next_vsock_cid=? WHERE id=1`, nextCID)
	return err
}

// --- snapshots --------------------------------------------------------------

func (s *Store) InsertSnapshot(ctx context.Context, snap any) error {
	id, _ := fieldStr(snap, "id")
	sandboxID, _ := fieldStr(snap, "sandbox_id")
	memPath, _ := fieldStr(snap, "mem_path")
	statePath, _ := fieldStr(snap, "state_path")
	_, err := s.db.ExecContext(ctx, `INSERT INTO snapshots (id, sandbox_id, mem_path, state_path, created_at) VALUES (?,?,?,?,?)`,
		id, sandboxID, memPath, statePath, time.Now().Unix())
	return err
}

// SnapshotRow is the minimal snapshot identity used by garbage collection.
type SnapshotRow struct {
	ID        string
	SandboxID string
}

// SnapshotsForSandbox returns all snapshot ids taken from a given sandbox. Used
// by cascade-delete so destroying a sandbox also reaps its snapshots.
func (s *Store) SnapshotsForSandbox(ctx context.Context, sandboxID string) ([]SnapshotRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sandbox_id FROM snapshots WHERE sandbox_id = ?`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSnapshotRows(rows)
}

// AllSnapshots returns every snapshot id with its source sandbox id, so callers
// can attribute snapshot bytes to a workspace. Unlike the GC helpers this is
// unfiltered: every snapshot is reported for as long as it exists.
func (s *Store) AllSnapshots(ctx context.Context) ([]SnapshotRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sandbox_id FROM snapshots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSnapshotRows(rows)
}

// ExpiredOrphanSnapshots returns orphaned snapshots (source sandbox gone) that
// are ALSO older than the grace cutoff — i.e. created_at < olderThanEpoch. This
// gives snapshots a durability window: a snapshot survives its sandbox's
// idle-reap and is only reclaimed once it's been orphaned for the grace period.
// A created_at of 0 (legacy rows with unknown age) is treated as ancient and
// always eligible. olderThanEpoch<=0 disables the age gate (reap any orphan).
func (s *Store) ExpiredOrphanSnapshots(ctx context.Context, olderThanEpoch int64, limit int) ([]SnapshotRow, error) {
	if limit <= 0 {
		limit = 500
	}
	q := `SELECT s.id, s.sandbox_id FROM snapshots s
	      WHERE NOT EXISTS (SELECT 1 FROM sandboxes sb WHERE sb.id = s.sandbox_id)`
	args := []any{}
	if olderThanEpoch > 0 {
		q += ` AND (s.created_at = 0 OR s.created_at < ?)`
		args = append(args, olderThanEpoch)
	}
	q += ` LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSnapshotRows(rows)
}

// DeleteSnapshot removes a single snapshot row. The caller is responsible for
// removing the on-disk dir + GCS blobs (best-effort) before/after.
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id)
	return err
}

func scanSnapshotRows(rows *sql.Rows) ([]SnapshotRow, error) {
	var out []SnapshotRow
	for rows.Next() {
		var r SnapshotRow
		if err := rows.Scan(&r.ID, &r.SandboxID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- helpers ----------------------------------------------------------------

func toRow(v any) (sandboxRow, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return sandboxRow{}, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return sandboxRow{}, err
	}
	md := ""
	if raw, ok := m["metadata"]; ok && raw != nil {
		mb, _ := json.Marshal(raw)
		md = string(mb)
	}
	var created int64
	if t, ok := m["created_at"].(string); ok {
		if pt, err := time.Parse(time.RFC3339Nano, t); err == nil {
			created = pt.Unix()
		}
	}
	return sandboxRow{
		ID:           asString(m["id"]),
		Template:     asString(m["template"]),
		CPU:          asInt(m["cpu"]),
		MemoryMB:     asInt(m["memory_mb"]),
		Status:       asString(m["status"]),
		GuestIP:      asString(m["guest_ip"]),
		HostTAP:      asString(m["host_tap"]),
		MAC:          asString(m["mac"]),
		VsockCID:     uint32(asInt(m["vsock_cid"])),
		FromSnapshot: asString(m["from_snapshot"]),
		Metadata:     md,
		CreatedAt:    created,
		BootMS:       int64(asInt(m["boot_ms"])),
		BootMode:     asString(m["boot_mode"]),
	}, nil
}

func fieldStr(v any, key string) (string, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", false
	}
	s, ok := m[key].(string)
	return s, ok
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
