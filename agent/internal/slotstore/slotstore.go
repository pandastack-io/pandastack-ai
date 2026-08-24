// SPDX-License-Identifier: Apache-2.0
//
// Package slotstore is the authoritative, single-source-of-truth allocator for
// per-host /30 network slot indices. It replaces the in-memory freeIdx +
// monotonic-high-water scheme (network.Pool) that was patched three times
// (v0.3.11 leak, v0.3.12 NATID-leak, v0.3.13 concurrent double-free) because
// slot ownership lived in two places kept in sync by hand.
//
// Design (see network-slot-allocator-redesign.md):
//
//   - One row per slot index. sandbox_id NULL = free; non-NULL = owned by
//     exactly one sandbox. Ownership lives in ONE place: the row.
//   - Claim = atomic UPDATE of the lowest free row. Release = UPDATE keyed by
//     sandbox_id whose RowsAffected==1 gives the single-owner-of-reclaim
//     guarantee structurally (not bolted on).
//   - LOCAL SQLite on the BOOT disk, separate from the main agent store. The
//     data describes host-local kernel objects (netns/TAP/veth) that are
//     destroyed on host reboot, so its lifetime must match the host's — a
//     local boot-disk file resets to empty on MIG autoheal exactly when those
//     kernel objects are gone. A shared/durable store would resurrect stale
//     ownership and re-create the leak class. The boot path never touches the
//     network: if the local disk is alive the agent can allocate; if it isn't,
//     the host can't run Firecracker anyway.
//   - Single connection (MaxOpenConns(1) + _txlock=immediate) serializes all
//     writers in-process, so concurrent creates can never race for a slot.
package slotstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrPoolExhausted is returned by Claim when no free slot remains.
var ErrPoolExhausted = errors.New("slot pool exhausted")

// Store is a local SQLite-backed slot allocator. Safe for concurrent use.
type Store struct {
	db   *sql.DB
	path string

	// seedMu guards the lazy capacity grow in Seed so two callers can't race
	// the INSERT. Claim/Release rely on the single DB connection + SQL atomicity,
	// not this mutex.
	seedMu sync.Mutex
}

// Open opens (creating if absent) the slot DB at path. path must live on the
// BOOT disk, not the stateful data disk — see package doc. The caller is
// expected to have created the parent directory.
func Open(path string) (*Store, error) {
	if err := guardBootDiskPath(path); err != nil {
		return nil, err
	}
	// Same pragmas as the main store's SQLite path: WAL for durable atomic
	// commit, immediate txlock so a write transaction grabs the lock up front
	// (no upgrade deadlock), single connection so all writes serialize.
	db, err := sql.Open("sqlite",
		path+"?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open slot db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS network_slots (
			idx        INTEGER PRIMARY KEY,
			sandbox_id TEXT,
			kind       TEXT NOT NULL DEFAULT '',
			claimed_at INTEGER
		)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create network_slots: %w", err)
	}
	// Partial index makes "lowest free" an indexed lookup, and release-by-owner fast.
	if _, err := db.ExecContext(context.Background(),
		`CREATE INDEX IF NOT EXISTS network_slots_free ON network_slots(idx) WHERE sandbox_id IS NULL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create free index: %w", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`CREATE INDEX IF NOT EXISTS network_slots_owner ON network_slots(sandbox_id) WHERE sandbox_id IS NOT NULL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create owner index: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// guardBootDiskPath refuses a slot-DB path under the stateful data mount. The
// slot store MUST be on the boot disk so it resets on autoheal/reboot (when the
// netns/TAP it tracks are destroyed). Putting it on the stateful disk would
// resurrect stale slot ownership — the exact bug class this redesign kills.
func guardBootDiskPath(path string) error {
	// The stateful disk is mounted at /var/lib/pandastack/volumes (per the
	// gcp-agent-mig terraform). /var/lib/pandastack-io (the OSS boot-disk db dir)
	// and /var/lib/pandastack/<not volumes> are fine.
	if strings.Contains(path, "/var/lib/pandastack/volumes") {
		return fmt.Errorf("slot db path %q is on the stateful data disk; it must be on the boot disk (slot state is ephemeral and must reset on reboot)", path)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// Seed ensures rows 0..capacity-1 exist (idempotent). New rows are free. It
// never deletes or shrinks: a capacity reduction just leaves higher rows that
// will simply never be claimed. Call once at startup after Open.
func (s *Store) Seed(ctx context.Context, capacity uint32) error {
	if capacity == 0 {
		return nil
	}
	s.seedMu.Lock()
	defer s.seedMu.Unlock()

	var have int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM network_slots`).Scan(&have); err != nil {
		return fmt.Errorf("count slots: %w", err)
	}
	if have >= int64(capacity) {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO network_slots (idx, sandbox_id, kind, claimed_at) VALUES (?, NULL, '', NULL)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := uint32(0); i < capacity; i++ {
		if _, err := stmt.ExecContext(ctx, i); err != nil {
			return fmt.Errorf("seed slot %d: %w", i, err)
		}
	}
	return tx.Commit()
}

// Claim atomically takes the lowest free slot for sandboxID and returns its
// index. kind is a free-form tag for diagnostics ("legacy"|"natid"|"prebuilt").
// Returns ErrPoolExhausted when no slot is free. Idempotent-ish: if sandboxID
// already owns a slot, that slot is returned rather than claiming a second.
func (s *Store) Claim(ctx context.Context, sandboxID, kind string) (uint32, error) {
	if sandboxID == "" {
		return 0, errors.New("claim: empty sandbox id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// If this sandbox already holds a slot, return it (re-entrant safe — e.g. a
	// retried create). Avoids a sandbox accidentally owning two slots.
	var existing int64
	switch err := tx.QueryRowContext(ctx, `SELECT idx FROM network_slots WHERE sandbox_id=?`, sandboxID).Scan(&existing); {
	case err == nil:
		return uint32(existing), tx.Commit()
	case errors.Is(err, sql.ErrNoRows):
		// fall through to claim a fresh slot
	default:
		return 0, err
	}

	var idx int64
	err = tx.QueryRowContext(ctx, `SELECT idx FROM network_slots WHERE sandbox_id IS NULL ORDER BY idx LIMIT 1`).Scan(&idx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrPoolExhausted
	}
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE network_slots SET sandbox_id=?, kind=?, claimed_at=strftime('%s','now') WHERE idx=? AND sandbox_id IS NULL`,
		sandboxID, kind, idx)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		// Lost a race for this exact idx (shouldn't happen with one connection,
		// but be safe): surface as exhausted-this-attempt so the caller retries.
		return 0, fmt.Errorf("claim: slot %d taken concurrently", idx)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uint32(idx), nil
}

// PrebuiltOwner is the sentinel sandbox_id under which a pre-built (but not yet
// claimed) NATID slot is held. Encoding the idx makes each prebuilt owner unique
// and lets adoption target the exact row. Exported so the network package can
// build the same string when adopting.
func PrebuiltOwner(idx uint32) string {
	return fmt.Sprintf("prebuilt:%08x", idx)
}

// ClaimPrebuilt atomically takes the lowest free slot and parks it under the
// PrebuiltOwner(idx) sentinel, returning idx. Unlike Claim (which would make
// concurrent prebuilds collide on a shared owner string), this assigns a unique
// idx-derived owner inside the same transaction, so many prebuilds run safely.
// Returns ErrPoolExhausted when full.
func (s *Store) ClaimPrebuilt(ctx context.Context, kind string) (uint32, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var idx int64
	err = tx.QueryRowContext(ctx, `SELECT idx FROM network_slots WHERE sandbox_id IS NULL ORDER BY idx LIMIT 1`).Scan(&idx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrPoolExhausted
	}
	if err != nil {
		return 0, err
	}
	owner := PrebuiltOwner(uint32(idx))
	res, err := tx.ExecContext(ctx,
		`UPDATE network_slots SET sandbox_id=?, kind=?, claimed_at=strftime('%s','now') WHERE idx=? AND sandbox_id IS NULL`,
		owner, kind, idx)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, fmt.Errorf("claimPrebuilt: slot %d taken concurrently", idx)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uint32(idx), nil
}

// ClaimIdx claims a SPECIFIC index for sandboxID (used when adopting a
// pre-built slot whose netns/veth were already wired at that index). Returns
// false if the index is not free (owned by someone else). If sandboxID already
// owns idx, it's a no-op success.
func (s *Store) ClaimIdx(ctx context.Context, sandboxID, kind string, idx uint32) (bool, error) {
	if sandboxID == "" {
		return false, errors.New("claimIdx: empty sandbox id")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE network_slots SET sandbox_id=?, kind=?, claimed_at=strftime('%s','now')
		   WHERE idx=? AND (sandbox_id IS NULL OR sandbox_id=?)`,
		sandboxID, kind, idx, sandboxID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Reassign atomically transfers ownership of a slot from fromOwner to toOwner.
// Used to adopt a pre-built NATID slot (owned by a "prebuilt:<idx>" sentinel)
// into the real sandbox that claims it, without a free-then-reclaim window in
// which another claimant could steal the index. Returns false if the slot was
// not owned by fromOwner (already adopted / released).
func (s *Store) Reassign(ctx context.Context, fromOwner, toOwner, kind string, idx uint32) (bool, error) {
	if toOwner == "" {
		return false, errors.New("reassign: empty target owner")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE network_slots SET sandbox_id=?, kind=?, claimed_at=strftime('%s','now')
		   WHERE idx=? AND sandbox_id=?`,
		toOwner, kind, idx, fromOwner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Release frees the slot owned by sandboxID. The returned bool is true only if
// THIS call actually freed a row (RowsAffected==1) — the single-owner-of-reclaim
// guarantee: two concurrent releases of the same sandbox can't both "succeed"
// and double-free a slot. Releasing an unknown/already-free sandbox returns
// (false, nil) — not an error (idempotent teardown).
func (s *Store) Release(ctx context.Context, sandboxID string) (bool, error) {
	if sandboxID == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE network_slots SET sandbox_id=NULL, kind='', claimed_at=NULL WHERE sandbox_id=?`,
		sandboxID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// OwnerIdx returns the slot index owned by sandboxID, or ok=false if none.
func (s *Store) OwnerIdx(ctx context.Context, sandboxID string) (uint32, bool, error) {
	var idx int64
	err := s.db.QueryRowContext(ctx, `SELECT idx FROM network_slots WHERE sandbox_id=?`, sandboxID).Scan(&idx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return uint32(idx), true, nil
}

// Reconcile frees every slot whose owning sandbox_id is NOT in liveIDs. This is
// the orphan-by-claim reclaim: it reclaims slots stranded by a crash that
// occurred after the slot was claimed but before/without a recoverable sandbox
// (which the sandbox-row-walking Recover() path misses). Call at startup AFTER
// the manager has determined which sandboxes are actually being recovered.
// Returns the number of slots reclaimed.
func (s *Store) Reconcile(ctx context.Context, liveIDs []string) (int, error) {
	live := make(map[string]struct{}, len(liveIDs))
	for _, id := range liveIDs {
		live[id] = struct{}{}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT idx, sandbox_id FROM network_slots WHERE sandbox_id IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	var orphans []int64
	for rows.Next() {
		var idx int64
		var owner string
		if err := rows.Scan(&idx, &owner); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := live[owner]; !ok {
			orphans = append(orphans, idx)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, idx := range orphans {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE network_slots SET sandbox_id=NULL, kind='', claimed_at=NULL WHERE idx=?`, idx); err != nil {
			return 0, err
		}
	}
	return len(orphans), nil
}

// Stats reports pool occupancy for metrics + soak assertions.
type Stats struct {
	Capacity int
	Free     int
	Used     int
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE sandbox_id IS NULL) FROM network_slots`).
		Scan(&st.Capacity, &st.Free); err != nil {
		return Stats{}, err
	}
	st.Used = st.Capacity - st.Free
	return st, nil
}
