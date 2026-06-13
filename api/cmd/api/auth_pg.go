// SPDX-License-Identifier: Apache-2.0
// Postgres-backed tokenStore: shared across all edge API instances so a token
// minted on edge-A is recognised by edge-B, and survives instance recycling.
//
// Tokens are stored hashed (SHA-256). Plaintext is shown to the user exactly
// once at Create time. A short `prefix` (e.g. "pds_a1b2c3d4") is stored
// alongside the hash so the dashboard / CLI can render and revoke without
// ever round-tripping the secret.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type pgTokenStore struct {
	db  *sql.DB
	log *slog.Logger
}

func newPGTokenStore(ctx context.Context, db *sql.DB, log *slog.Logger) (*pgTokenStore, error) {
	if db == nil {
		return nil, errors.New("pgTokenStore: nil db")
	}
	if _, err := db.ExecContext(ctx, pgTokenSchema); err != nil {
		return nil, fmt.Errorf("create api_tokens table: %w", err)
	}
	return &pgTokenStore{db: db, log: log}, nil
}

const pgTokenSchema = `
CREATE TABLE IF NOT EXISTS api_tokens (
    token_hash    TEXT PRIMARY KEY,
    prefix        TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    label         TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS api_tokens_ws_idx     ON api_tokens (workspace_id);
CREATE INDEX IF NOT EXISTS api_tokens_prefix_idx ON api_tokens (prefix);
`

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (s *pgTokenStore) LookupByToken(tok string) (tokenRec, bool) {
	if strings.TrimSpace(tok) == "" {
		return tokenRec{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT workspace_id, label, created_at FROM api_tokens WHERE token_hash = $1`,
		hashToken(tok))
	var rec tokenRec
	if err := row.Scan(&rec.Workspace, &rec.Label, &rec.CreatedAt); err != nil {
		if !errors.Is(err, sql.ErrNoRows) && s.log != nil {
			s.log.Warn("pgTokenStore lookup failed", "err", err)
		}
		return tokenRec{}, false
	}
	rec.Token = tok
	// Fire-and-forget last-used update (don't block auth on it).
	go func() {
		updCtx, updCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer updCancel()
		_, _ = s.db.ExecContext(updCtx,
			`UPDATE api_tokens SET last_used_at = now() WHERE token_hash = $1`,
			hashToken(tok))
	}()
	return rec, true
}

func (s *pgTokenStore) ListByWorkspace(workspaceID string) []tokenRec {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT prefix, label, created_at FROM api_tokens WHERE workspace_id = $1 ORDER BY created_at DESC`,
		workspaceID)
	if err != nil {
		if s.log != nil {
			s.log.Warn("pgTokenStore list failed", "err", err)
		}
		return nil
	}
	defer rows.Close()
	out := make([]tokenRec, 0)
	for rows.Next() {
		var rec tokenRec
		if err := rows.Scan(&rec.Token, &rec.Label, &rec.CreatedAt); err != nil {
			continue
		}
		rec.Workspace = workspaceID
		out = append(out, rec)
	}
	return out
}

func (s *pgTokenStore) Create(workspaceID, label string) (tokenRec, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return tokenRec{}, errors.New("workspace required")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return tokenRec{}, err
	}
	rec := tokenRec{
		Token:     "pds_" + hex.EncodeToString(b),
		Workspace: workspaceID,
		Label:     strings.TrimSpace(label),
		CreatedAt: time.Now().UTC(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (token_hash, prefix, workspace_id, label, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		hashToken(rec.Token), Prefix(rec.Token), rec.Workspace, rec.Label, rec.CreatedAt)
	if err != nil {
		return tokenRec{}, fmt.Errorf("insert api_token: %w", err)
	}
	return rec, nil
}

func (s *pgTokenStore) RevokeByPrefix(workspaceID, prefix string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_tokens WHERE workspace_id = $1 AND prefix = $2`,
		workspaceID, prefix)
	if err != nil {
		if s.log != nil {
			s.log.Warn("pgTokenStore revoke failed", "err", err)
		}
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// migrateFromFile imports any plaintext tokens from a legacy file store. Runs
// once at boot; safe to re-run (PRIMARY KEY conflict on token_hash is ignored).
func (s *pgTokenStore) migrateFromFile(ctx context.Context, fs *fileTokenStore) (int, error) {
	if fs == nil {
		return 0, nil
	}
	recs := fs.Snapshot()
	if len(recs) == 0 {
		return 0, nil
	}
	imported := 0
	for _, rec := range recs {
		if rec.Token == "" || rec.Workspace == "" {
			continue
		}
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO api_tokens (token_hash, prefix, workspace_id, label, created_at)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (token_hash) DO NOTHING`,
			hashToken(rec.Token), Prefix(rec.Token), rec.Workspace, rec.Label, rec.CreatedAt)
		if err != nil {
			if s.log != nil {
				s.log.Warn("token migration row failed", "prefix", Prefix(rec.Token), "err", err)
			}
			continue
		}
		imported++
	}
	return imported, nil
}
