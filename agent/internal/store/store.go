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
}

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
}

// InsertSandbox stores a sandbox row. The caller passes any struct with json tags
// matching the schema (the sandbox.Sandbox type does).
func (s *Store) InsertSandbox(ctx context.Context, sb any) error {
	r, err := toRow(sb)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sandboxes (id, template, cpu, memory_mb, status, guest_ip, host_tap, mac, vsock_cid, from_snapshot, metadata, created_at, boot_ms, boot_mode)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Template, r.CPU, r.MemoryMB, r.Status, r.GuestIP, r.HostTAP, r.MAC, r.VsockCID, r.FromSnapshot, r.Metadata, r.CreatedAt, r.BootMS, r.BootMode)
	return err
}

func (s *Store) UpdateSandbox(ctx context.Context, sb any) error {
	r, err := toRow(sb)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE sandboxes SET template=?, cpu=?, memory_mb=?, status=?, guest_ip=?, host_tap=?, mac=?, vsock_cid=?, from_snapshot=?, metadata=?, boot_ms=?, boot_mode=?, created_at=?
		WHERE id=?`,
		r.Template, r.CPU, r.MemoryMB, r.Status, r.GuestIP, r.HostTAP, r.MAC, r.VsockCID, r.FromSnapshot, r.Metadata, r.BootMS, r.BootMode, r.CreatedAt, r.ID)
	return err
}

func (s *Store) SetStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET status=? WHERE id=?`, status, id)
	return err
}

// SetSandboxLifecycle persists the lifecycle config (persistent flag + ttl) for a
// sandbox so an agent restart can rehydrate it instead of falling back to the
// default TTL. `persistent` is stored as 0/1 in a BIGINT/INTEGER column to keep
// scanning uniform across the sqlite and postgres drivers.
func (s *Store) SetSandboxLifecycle(ctx context.Context, id string, persistent bool, ttlSeconds int64) error {
	p := int64(0)
	if persistent {
		p = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET persistent=?, ttl_seconds=? WHERE id=?`, p, ttlSeconds, id)
	return err
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
	row := s.db.QueryRowContext(ctx, `SELECT id, template, cpu, memory_mb, status, guest_ip, host_tap, mac, vsock_cid, from_snapshot, metadata, created_at, boot_ms, boot_mode FROM sandboxes WHERE id=?`, id)
	return scanSandbox(row.Scan)
}

func (s *Store) ListSandboxes(ctx context.Context) ([]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, template, cpu, memory_mb, status, guest_ip, host_tap, mac, vsock_cid, from_snapshot, metadata, created_at, boot_ms, boot_mode FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
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
	if err := scan(&r.ID, &r.Template, &r.CPU, &r.MemoryMB, &r.Status, &r.GuestIP, &r.HostTAP, &r.MAC, &r.VsockCID, &r.FromSnapshot, &r.Metadata, &r.CreatedAt, &r.BootMS, &r.BootMode); err != nil {
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

// allocation is just enough to find the TAP name (or netns/veth pair) for teardown.
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
