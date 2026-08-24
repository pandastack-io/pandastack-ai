// SPDX-License-Identifier: Apache-2.0
//
// snapshots.go — control-plane registry for user sandbox snapshots.
//
// Snapshots are CREATED on the agent (POST /v1/sandboxes/{id}/snapshots) and
// the bytes live on the agent + replicate to gs://<bucket>/snapshots/<id>/.
// But the agent's snapshots table is per-host and carries no workspace, and GCS
// objects are flat + untagged — so there was no way for a user to LIST their
// snapshots (the only snapshot endpoint was create).
//
// This adds a control-plane registry: a `snapshots` table in the API Postgres,
// populated when a create succeeds (we wrap the create-proxy and record the
// result), so GET /v1/snapshots is a clean workspace-scoped query — matching how
// apps/databases/functions already work. Only snapshots created after this ships
// are listed (pre-existing GCS objects aren't workspace-attributable).

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"time"
)

type snapshotsAPI struct {
	db  *sql.DB
	log *slog.Logger
	v1  http.Handler // agent proxy (same handler the catch-all /v1/ uses)
}

func newSnapshotsAPI(db *sql.DB, log *slog.Logger, v1 http.Handler) *snapshotsAPI {
	return &snapshotsAPI{db: db, log: log, v1: v1}
}

// snapshotsSchemaStmts is split into individual idempotent statements and run
// one-by-one so a pre-existing legacy `snapshots` table (the AGENT's table —
// id/sandbox_id/mem_path/state_path/created_at, which also lives in this PG and
// lacks our columns) is RECONCILED rather than collided with. A bare
// `CREATE TABLE IF NOT EXISTS` is a no-op against an existing table, so we then
// `ADD COLUMN IF NOT EXISTS` every column before building any index on it —
// otherwise `CREATE INDEX (workspace)` fails 42703 on the legacy table (this is
// exactly what crash-looped the edge fleet when this feature first shipped).
var snapshotsSchemaStmts = []string{
	// created_at is BIGINT epoch seconds — this MUST match the AGENT's snapshots
	// schema (the agent owns this table and writes epoch via its own
	// InsertSnapshot). A TIMESTAMPTZ here would 42804 against the agent's bigint
	// column on the shared PG (and silently lose the workspace stamp).
	`CREATE TABLE IF NOT EXISTS snapshots (
	    id          TEXT PRIMARY KEY,
	    workspace   TEXT NOT NULL DEFAULT '',
	    sandbox_id  TEXT NOT NULL DEFAULT '',
	    template    TEXT,
	    size_bytes  BIGINT,
	    created_at  BIGINT NOT NULL DEFAULT 0
	)`,
	`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS workspace  TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS sandbox_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS template   TEXT`,
	`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS size_bytes BIGINT`,
	`CREATE INDEX IF NOT EXISTS idx_snapshots_workspace ON snapshots (workspace, created_at DESC)`,
}

// SetupSchema creates/reconciles the snapshots table. Idempotent. Tolerant of a
// pre-existing legacy table.
func (a *snapshotsAPI) SetupSchema(ctx context.Context) error {
	if a.db == nil {
		return errors.New("snapshots: nil db")
	}
	for _, stmt := range snapshotsSchemaStmts {
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("snapshots schema (%.40q…): %w", stmt, err)
		}
	}
	return nil
}

func (a *snapshotsAPI) Register(mux *http.ServeMux) {
	// Wrap the create so we record it in the registry; the actual snapshot work
	// still happens on the agent via the proxy. Registered as a SPECIFIC route so
	// it wins over the catch-all /v1/ proxy.
	mux.HandleFunc("POST /v1/sandboxes/{id}/snapshots", a.createSnapshot)
	mux.HandleFunc("GET /v1/snapshots", a.listSnapshots)
	mux.HandleFunc("GET /v1/snapshots/{id}", a.getSnapshot)
	mux.HandleFunc("DELETE /v1/snapshots/{id}", a.deleteSnapshot)
}

// snapshotInfo is the API shape returned to users. created_at is stored as a
// BIGINT epoch (the AGENT owns this table — id/sandbox_id/mem_path/state_path/
// created_at — and writes epoch seconds), so we scan it as int64 and render it
// as an RFC3339 string for clients.
type snapshotInfo struct {
	ID          string `json:"id"`
	Workspace   string `json:"workspace"`
	SandboxID   string `json:"sandbox_id"`
	Template    string `json:"template,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	CreatedAtTS int64  `json:"-"`
	CreatedAt   string `json:"created_at"`
}

// createSnapshot proxies the create to the agent, and on success records the
// snapshot in the control-plane registry so it can be listed later.
func (a *snapshotsAPI) createSnapshot(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := snapshotScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sandboxID := r.PathValue("id")

	// Forward to the agent via the proxy, capturing its response.
	rr := httptest.NewRecorder()
	// Re-point the path to what the agent expects and replay the request.
	proxied := r.Clone(r.Context())
	proxied.URL.Path = "/v1/sandboxes/" + sandboxID + "/snapshots"
	a.v1.ServeHTTP(rr, proxied)
	res := rr.Result()
	bodyBytes, _ := io.ReadAll(res.Body)
	res.Body.Close()

	// Pass the agent's response straight back to the caller regardless.
	for k, vs := range rr.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(bodyBytes)

	// Record only on a successful create. Best-effort — a record failure must not
	// fail the user's snapshot (the bytes already exist on the agent + GCS).
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return
	}
	var created struct {
		ID        string `json:"id"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(bodyBytes, &created); err != nil || created.ID == "" {
		a.log.Warn("snapshots: create succeeded but response unparseable; not recorded",
			"sandbox_id", sandboxID, "err", err)
		return
	}
	// Best-effort: resolve the source sandbox's template for a friendlier list.
	template := a.sandboxTemplate(r.Context(), workspace, sandboxID)
	// The AGENT writes this row first (id/sandbox_id/mem_path/state_path +
	// created_at as a BIGINT epoch) with NO workspace — so we must:
	//  (1) write created_at as epoch seconds, NOT now() (column is BIGINT — a
	//      timestamptz here 42804'd and the row stayed workspace-less, invisible),
	//  (2) ON CONFLICT DO UPDATE to STAMP workspace/template/size onto whatever
	//      the agent already inserted (the agent usually wins the race).
	if _, err := a.db.ExecContext(r.Context(),
		`INSERT INTO snapshots (id, workspace, sandbox_id, template, size_bytes, created_at)
		   VALUES ($1, $2, $3, $4, $5, extract(epoch from now())::bigint)
		 ON CONFLICT (id) DO UPDATE SET
		   workspace  = EXCLUDED.workspace,
		   sandbox_id = COALESCE(NULLIF(snapshots.sandbox_id, ''), EXCLUDED.sandbox_id),
		   template   = COALESCE(NULLIF(EXCLUDED.template, ''), snapshots.template),
		   size_bytes = COALESCE(NULLIF(EXCLUDED.size_bytes, 0), snapshots.size_bytes)`,
		created.ID, workspace, sandboxID, template, created.SizeBytes); err != nil {
		a.log.Warn("snapshots: record failed (snapshot still exists on agent/GCS)",
			"snapshot_id", created.ID, "err", err)
	}
}

// listSnapshots returns the caller's snapshots, newest first. Optional
// ?sandbox_id= filter.
func (a *snapshotsAPI) listSnapshots(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := snapshotScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := `SELECT id, workspace, sandbox_id, COALESCE(template,''), COALESCE(size_bytes,0), COALESCE(created_at,0)
	        FROM snapshots WHERE workspace = $1`
	args := []any{workspace}
	if sb := strings.TrimSpace(r.URL.Query().Get("sandbox_id")); sb != "" {
		q += ` AND sandbox_id = $2`
		args = append(args, sb)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`

	rows, err := a.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		a.log.Error("snapshots: list query", "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()
	out := []snapshotInfo{}
	for rows.Next() {
		var s snapshotInfo
		if err := rows.Scan(&s.ID, &s.Workspace, &s.SandboxID, &s.Template, &s.SizeBytes, &s.CreatedAtTS); err != nil {
			continue
		}
		s.CreatedAt = epochToRFC3339(s.CreatedAtTS)
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

// epochToRFC3339 renders a BIGINT epoch-seconds column as an RFC3339 UTC string
// (the JSON shape clients/SDKs expect). 0/negative → empty string.
func epochToRFC3339(epoch int64) string {
	if epoch <= 0 {
		return ""
	}
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}

func (a *snapshotsAPI) getSnapshot(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := snapshotScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var s snapshotInfo
	err := a.db.QueryRowContext(r.Context(),
		`SELECT id, workspace, sandbox_id, COALESCE(template,''), COALESCE(size_bytes,0), COALESCE(created_at,0)
		   FROM snapshots WHERE workspace = $1 AND id = $2`, workspace, r.PathValue("id")).
		Scan(&s.ID, &s.Workspace, &s.SandboxID, &s.Template, &s.SizeBytes, &s.CreatedAtTS)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	s.CreatedAt = epochToRFC3339(s.CreatedAtTS)
	writeJSON(w, http.StatusOK, s)
}

// deleteSnapshot removes the registry row AND the snapshot bytes from GCS (the
// durable copy). The per-agent local copy is reaped by the agent's own
// retention; the registry + GCS are the user-visible truth.
func (a *snapshotsAPI) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := snapshotScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	snapID := r.PathValue("id")
	// Verify ownership first.
	var owned string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT id FROM snapshots WHERE workspace = $1 AND id = $2`, workspace, snapID).Scan(&owned)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	// Best-effort GCS purge (reuses the apps GCS helper bucket convention).
	a.purgeSnapshotGCS(r.Context(), snapID)
	if _, err := a.db.ExecContext(r.Context(),
		`DELETE FROM snapshots WHERE workspace = $1 AND id = $2`, workspace, snapID); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": snapID})
}

// sandboxTemplate fetches the source sandbox's template via the agent proxy
// (best-effort — returns "" if unavailable; never blocks the record).
func (a *snapshotsAPI) sandboxTemplate(ctx context.Context, workspace, sandboxID string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", "/v1/sandboxes/"+sandboxID, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-Fcs-Workspace", workspace)
	req.Header.Set("X-Pandastack-User-Id", "_snap")
	rr := httptest.NewRecorder()
	a.v1.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		return ""
	}
	var sb struct {
		Template string `json:"template"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	return sb.Template
}

// purgeSnapshotGCS removes gs://<snapshot-bucket>/snapshots/<id>/ best-effort,
// reusing the same gsutil mechanism + benign-error handling as apps_gcs.go.
func (a *snapshotsAPI) purgeSnapshotGCS(ctx context.Context, snapID string) {
	snapID = strings.TrimSpace(snapID)
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if snapID == "" || bucket == "" {
		return
	}
	prefix := "gs://" + bucket + "/snapshots/" + snapID
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gsutilPath(), "-m", "rm", "-r", prefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		if !benignGsutilRm(string(out)) {
			a.log.Warn("snapshots: gcs purge failed (non-fatal)",
				"snapshot_id", snapID, "err", err, "out", strings.TrimSpace(string(out)))
		}
	}
}

// snapshotScope reads the tenant identity a snapshot request acts under.
// ok=false when no workspace header is present, which callers turn into a 400 —
// snapshots are workspace-owned and an unscoped list would cross tenants.
func snapshotScope(r *http.Request) (workspace, userID string, ok bool) {
	workspace = strings.TrimSpace(r.Header.Get("X-Fcs-Workspace"))
	userID = strings.TrimSpace(r.Header.Get("X-Pandastack-User-Id"))
	return workspace, userID, workspace != ""
}
