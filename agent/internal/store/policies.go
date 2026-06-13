// SPDX-License-Identifier: Apache-2.0
package store

import (
	"context"
	"encoding/json"
	"time"
)

// --- boot_events ------------------------------------------------------------
//
// We keep a row per Create() so /stats/boot keeps reporting accurate
// percentiles even after sandboxes are deleted (they normally are, often).

type BootEvent struct {
	ID        int64     `json:"id"`
	SandboxID string    `json:"sandbox_id"`
	Workspace string    `json:"workspace"`
	Template  string    `json:"template"`
	BootMode  string    `json:"boot_mode"`
	BootMS    int64     `json:"boot_ms"`
	TS        time.Time `json:"ts"`
}

func (s *Store) InsertBootEvent(ctx context.Context, e BootEvent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO boot_events (sandbox_id, workspace, template, boot_mode, boot_ms, ts) VALUES (?,?,?,?,?,?)`,
		e.SandboxID, e.Workspace, e.Template, e.BootMode, e.BootMS, e.TS.Unix(),
	)
	return err
}

func (s *Store) ListBootEvents(ctx context.Context, workspace string, limit int) ([]BootEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	q := `SELECT id, sandbox_id, COALESCE(workspace,''), template, boot_mode, boot_ms, ts FROM boot_events`
	var args []any
	if workspace != "" && workspace != "admin" && workspace != "default" {
		q += ` WHERE workspace = ?`
		args = append(args, workspace)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BootEvent
	for rows.Next() {
		var e BootEvent
		var ts int64
		if err := rows.Scan(&e.ID, &e.SandboxID, &e.Workspace, &e.Template, &e.BootMode, &e.BootMS, &ts); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- audit_log --------------------------------------------------------------

type AuditEntry struct {
	ID        int64             `json:"id"`
	TS        time.Time         `json:"ts"`
	Workspace string            `json:"workspace"`
	RequestID string            `json:"request_id"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Status    int               `json:"status"`
	Actor     string            `json:"actor,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
}

func (s *Store) InsertAudit(ctx context.Context, e AuditEntry) error {
	var metaJSON string
	if len(e.Meta) > 0 {
		b, _ := json.Marshal(e.Meta)
		metaJSON = string(b)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, workspace, request_id, method, path, status, actor, meta) VALUES (?,?,?,?,?,?,?,?)`,
		e.TS.Unix(), e.Workspace, e.RequestID, e.Method, e.Path, e.Status, e.Actor, metaJSON,
	)
	return err
}

func (s *Store) ListAudit(ctx context.Context, since time.Time, workspace string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT id, ts, workspace, request_id, method, path, status, actor, meta FROM audit_log WHERE ts >= ?`
	args := []any{since.Unix()}
	if workspace != "" {
		q += ` AND workspace = ?`
		args = append(args, workspace)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		var meta string
		if err := rows.Scan(&e.ID, &ts, &e.Workspace, &e.RequestID, &e.Method, &e.Path, &e.Status, &e.Actor, &meta); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0).UTC()
		if meta != "" {
			_ = json.Unmarshal([]byte(meta), &e.Meta)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAudit deletes audit_log rows older than `retain` and returns the
// number of rows removed. retain<=0 is a no-op.
func (s *Store) PruneAudit(ctx context.Context, retain time.Duration) (int64, error) {
	if retain <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retain).Unix()
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
