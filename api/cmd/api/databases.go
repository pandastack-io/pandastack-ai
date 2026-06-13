// SPDX-License-Identifier: Apache-2.0
//
// databases.go — REST API for managed PostgreSQL databases.
//
// A "database" is a sandbox running the postgres-16 Firecracker template.
// The API wraps sandbox lifecycle (create/list/get/delete) with database-
// specific ergonomics: creation returns a ready connection string, and the
// connection endpoint uses the db-proxy Pattern B SNI routing so customers
// get a native postgres:// URL.
//
// Routes (all behind /v1 + JWT auth):
//   POST   /v1/databases                    — provision a new postgres-16 sandbox
//   GET    /v1/databases                    — list all databases in this workspace
//   GET    /v1/databases/{id}               — get database + connection info
//   DELETE /v1/databases/{id}               — destroy database (irreversible)
//   GET    /v1/databases/{id}/connection    — return just the connection string
//   POST   /v1/databases/{id}/failover      — rebuild the DB on a healthy agent (databases_failover.go)
//   GET    /v1/databases/{id}/stats         — live PG stats (size, connections, disk, cache)
//   GET    /v1/databases/{id}/logs          — postgresql.log tail (snapshot)
//   ANY    /v1/databases/{id}/proxy/{path}  — HTTP proxy to the in-VM query broker (port 5544)

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

const dbTemplate = "postgres-16"

// dbSNISuffix is the SNI suffix used by the db-proxy.
// Override with PANDASTACK_DB_SNI_SUFFIX env var.
var dbSNISuffix = func() string {
	if v := strings.TrimSpace(os.Getenv("PANDASTACK_DB_SNI_SUFFIX")); v != "" {
		return v
	}
	return ".db.pandastack.ai"
}()

// dbAPIBase is the public base URL of this API, used to build broker_url.
// Override with PANDASTACK_API_BASE_URL env var.
var dbAPIBase = func() string {
	if v := strings.TrimSpace(os.Getenv("PANDASTACK_API_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.pandastack.ai"
}()

// DatabaseInfo is returned to callers on create/get.
type DatabaseInfo struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Template  string `json:"template"`
	Label     string `json:"label,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
	// Connection fields — populated once sandbox is running.
	Host          string `json:"host,omitempty"`
	Port          int    `json:"port,omitempty"`
	Database      string `json:"database,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	ConnectionURL string `json:"connection_url,omitempty"`
	// REST broker (Pattern A)
	BrokerToken string `json:"broker_token,omitempty"`
	BrokerURL   string `json:"broker_url,omitempty"`
	// Error is set when the sandbox is up but postgres has not published
	// credentials (ready.json) — surfaced instead of silently returning a
	// credential-less "running" database (2026-06-11 stale-seed incident).
	Error string `json:"error,omitempty"`
	// Failover fields (item 15: user visibility for restore)
	FailoverAvailable bool   `json:"failover_available,omitempty"`
	FailoverReason    string `json:"failover_reason,omitempty"`
	FailoverETA       int    `json:"failover_eta_seconds,omitempty"`
}

type databasesAPI struct {
	log *slog.Logger
	v1  http.Handler // internal agent proxy (same as functionsAPI.v1)
	db  *sql.DB      // control-plane Postgres (optional, for listing)
	// director is the multi-node router (nil in single-node deployments).
	// Failover needs it to talk to a SPECIFIC agent directly — the normal v1
	// path routes by lease, which points at the dead agent.
	director *MultiNodeDirector
}

func newDatabasesAPI(log *slog.Logger, v1 http.Handler, db *sql.DB, director *MultiNodeDirector) *databasesAPI {
	return &databasesAPI{log: log, v1: v1, db: db, director: director}
}

func (d *databasesAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/databases", d.create)
	mux.HandleFunc("GET /v1/databases", d.list)
	mux.HandleFunc("GET /v1/databases/{id}", d.get)
	mux.HandleFunc("DELETE /v1/databases/{id}", d.delete)
	mux.HandleFunc("GET /v1/databases/{id}/connection", d.connection)
	mux.HandleFunc("POST /v1/databases/{id}/failover", d.failover)
	mux.HandleFunc("GET /v1/databases/{id}/stats", d.stats)
	mux.HandleFunc("GET /v1/databases/{id}/logs", d.logs)
	// Catch-all proxy to the in-VM query broker (port 5544).
	// No method prefix → matches GET/POST/PUT/DELETE/PATCH.
	mux.HandleFunc("/v1/databases/{id}/proxy/", d.proxy)
	mux.HandleFunc("/v1/databases/{id}/proxy", d.proxy)
}

// agentCall proxies a request to the internal v1 handler (agent director).
func (d *databasesAPI) agentCall(r *http.Request, method, path, workspace string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, "http://localhost"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Fcs-Workspace", workspace)
	req.Header.Set("X-Pandastack-User-Id", "_db")
	req.Header.Set("X-Pandastack-Auth-Method", "db-api")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	d.v1.ServeHTTP(rr, req)
	return rr.Result(), nil
}

// workspace extracts the workspace from the request context (set by auth middleware).
func dbWorkspace(r *http.Request) string {
	return r.Header.Get("X-Fcs-Workspace")
}

// create provisions a new postgres-16 sandbox and waits for it to be ready.
func (d *databasesAPI) create(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	if workspace == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}

	var opts struct {
		CPU      int    `json:"cpu"`
		MemoryMB int    `json:"memory_mb"`
		Label    string `json:"label,omitempty"` // stored in metadata, surfaced in list
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&opts) //nolint:errcheck
	}
	if opts.CPU == 0 {
		opts.CPU = 2
	}
	if opts.MemoryMB == 0 {
		opts.MemoryMB = 1024
	}

	// Create sandbox
	meta := map[string]string{}
	if opts.Label != "" {
		meta["db.label"] = opts.Label
	}
	createBody, _ := json.Marshal(map[string]any{
		"template":  dbTemplate,
		"cpu":       opts.CPU,
		"memory_mb": opts.MemoryMB,
		"metadata":  meta,
		// Managed databases are durable: mark the sandbox persistent so the
		// idle reaper never deletes it (a delete would destroy the per-DB
		// /dev/vdb volume). Only an explicit DELETE /v1/databases/{id} removes
		// the data. Phase 4B adds cross-agent durability (GCP PD + affinity).
		"persistent": true,
	})
	resp, err := d.agentCall(r, "POST", "/v1/sandboxes", workspace, bytes.NewReader(createBody))
	if err != nil {
		d.log.Error("databases: create sandbox failed", "err", err)
		writeErrOrg(w, http.StatusBadGateway, "could not create database sandbox")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		d.log.Error("databases: agent returned error", "status", resp.StatusCode, "body", string(b))
		writeErrOrg(w, http.StatusBadGateway, "agent error: "+string(b))
		return
	}
	var sbInfo struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sbInfo); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "decode sandbox response: "+err.Error())
		return
	}

	// Wait for sandbox to reach running (up to 90s for cold postgres boot)
	ctx := r.Context()
	deadline := time.Now().Add(90 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for sbInfo.Status != "running" {
		select {
		case <-ctx.Done():
			writeErrOrg(w, http.StatusGatewayTimeout, "timed out waiting for database to start")
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				writeErrOrg(w, http.StatusGatewayTimeout, "database did not start within 90s")
				return
			}
			sr, err := d.agentCall(r, "GET", "/v1/sandboxes/"+sbInfo.ID, workspace, nil)
			if err != nil {
				continue
			}
			_ = json.NewDecoder(sr.Body).Decode(&sbInfo)
			sr.Body.Close()
		}
	}

	// Fetch postgres connection info from the sandbox — poll until ready.json
	// appears (PG init takes ~30-60s after the VM reaches "running").
	var info *pgInfoResponse
	pgDeadline := time.Now().Add(120 * time.Second)
	pgTicker := time.NewTicker(3 * time.Second)
	defer pgTicker.Stop()
	for {
		if i, err := d.fetchPGInfo(r, workspace, sbInfo.ID); err == nil && i != nil {
			info = i
			break
		}
		if time.Now().After(pgDeadline) {
			d.log.Warn("databases: postgres did not become ready within 120s", "id", sbInfo.ID)
			break
		}
		select {
		case <-ctx.Done():
			break
		case <-pgTicker.C:
		}
	}
	result := DatabaseInfo{
		ID:       sbInfo.ID,
		Status:   sbInfo.Status,
		Template: dbTemplate,
		Label:    opts.Label,
	}
	if info != nil {
		result = mergeInfo(result, info, sbInfo.ID)
	} else {
		// The VM reached "running" but postgres never published ready.json.
		// Surface that instead of returning a credential-less "running"
		// payload that looks healthy (2026-06-11 stale-seed incident).
		result.Status = "provisioning"
		result.Error = "postgres did not become ready within 120s; poll GET /v1/databases/{id} — if this persists the database failed to start"
	}

	writeJSON(w, http.StatusCreated, result)
}

func (d *databasesAPI) list(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	if workspace == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}
	resp, err := d.agentCall(r, "GET", "/v1/sandboxes", workspace, nil)
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	var all []struct {
		ID        string            `json:"id"`
		Template  string            `json:"template"`
		Status    string            `json:"status"`
		CreatedAt int64             `json:"created_at"`
		Metadata  map[string]string `json:"metadata"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&all)

	out := []DatabaseInfo{}
	for _, sb := range all {
		if sb.Template != dbTemplate {
			continue
		}
		info := DatabaseInfo{
			ID:        sb.ID,
			Status:    sb.Status,
			Template:  sb.Template,
			CreatedAt: sb.CreatedAt,
		}
		if sb.Metadata != nil {
			info.Label = sb.Metadata["db.label"]
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}

func (d *databasesAPI) get(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	id := r.PathValue("id")

	resp, err := d.agentCall(r, "GET", "/v1/sandboxes/"+id, workspace, nil)
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}
	var sb struct {
		ID        string            `json:"id"`
		Template  string            `json:"template"`
		Status    string            `json:"status"`
		CreatedAt int64             `json:"created_at"`
		Metadata  map[string]string `json:"metadata"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sb)
	if sb.Template != dbTemplate {
		writeErrOrg(w, http.StatusNotFound, "database not found (template mismatch)")
		return
	}

	result := DatabaseInfo{
		ID:        sb.ID,
		Status:    sb.Status,
		Template:  sb.Template,
		CreatedAt: sb.CreatedAt,
	}
	if sb.Metadata != nil {
		result.Label = sb.Metadata["db.label"]
	}
	if sb.Status == "running" {
		info, err := d.fetchPGInfo(r, workspace, id)
		if err == nil && info != nil {
			result = mergeInfo(result, info, id)
		} else {
			// VM is up but postgres hasn't published credentials. Report
			// "provisioning" (+error) instead of a credential-less "running".
			result.Status = "provisioning"
			result.Error = "postgres is not ready yet (initializing, or failed to start)"
			if err != nil {
				d.log.Warn("databases: postgres-info unavailable", "id", id, "err", err)
			}
		}
	} else if sb.Status == "failed" {
		// Item 15: User visibility for failover
		// Check if this database can be restored (healthy agents exist + archive exists)
		d.checkFailoverAvailability(r.Context(), &result, id)
	}
	writeJSON(w, http.StatusOK, result)
}

func (d *databasesAPI) cleanupArchive(ctx context.Context, id string) error {
	// Item 6: GCS archive cleanup on delete (GDPR/data privacy)
	// Delete all WAL + base backups from gs://$BUCKET/db/{id}/
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if bucket == "" {
		return nil // no bucket = no archive
	}
	prefix := "gs://" + bucket + "/db/" + id
	// Timeout 30s to avoid hanging the API response
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gsutil", "-m", "rm", "-r", prefix)
	if err := cmd.Run(); err != nil {
		d.log.Warn("database archive cleanup failed (non-fatal)", "id", id, "err", err)
		return nil // don't block delete; user data on host is already gone
	}
	d.log.Info("database archive cleanup complete", "id", id)
	return nil
}

// checkFailoverAvailability populates failover fields on a failed database:
// FailoverAvailable=true if healthy agents exist + GCS archive exists.
// Non-fatal: errors are logged but never returned (don't block GET response).
func (d *databasesAPI) checkFailoverAvailability(ctx context.Context, result *DatabaseInfo, id string) {
	// Prerequisite: must have a multi-node director (single-node has no failover).
	if d.director == nil || d.director.sched == nil {
		result.FailoverAvailable = false
		result.FailoverReason = "single-node deployment (no failover available)"
		return
	}

	// Check if at least one healthy agent exists (status=active + recent heartbeat).
	agents, err := d.director.sched.List(ctx)
	if err != nil {
		d.log.Warn("databases: failover check - agent list failed", "id", id, "err", err)
		result.FailoverAvailable = false
		result.FailoverReason = "unable to check agent availability"
		return
	}

	healthyCount := 0
	now := time.Now()
	for _, a := range agents {
		if a.Status != "active" {
			continue
		}
		// Heartbeat older than 30s = stale, don't count as healthy
		if now.Sub(a.LastHeartbeat) > 30*time.Second {
			continue
		}
		healthyCount++
	}

	if healthyCount == 0 {
		result.FailoverAvailable = false
		result.FailoverReason = "no healthy agents available for failover"
		return
	}

	// Check if GCS archive exists (best-effort via gsutil stat).
	// Non-fatal: if the check fails, we still mark as unavailable rather than erroring.
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if bucket == "" {
		result.FailoverAvailable = false
		result.FailoverReason = "GCS archive not configured"
		return
	}

	archivePrefix := "gs://" + bucket + "/db/" + id
	archiveExists := false

	// Try to stat the base backup directory to confirm archive exists.
	// Non-fatal timeout: 5s max.
	archCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(archCtx, "gsutil", "-m", "ls", "-r", archivePrefix+"/base/")
	output, err := cmd.CombinedOutput()
	if err == nil && len(output) > 0 {
		archiveExists = true
	} else if err != nil {
		// Check if it's a timeout vs. actual error
		if ctx.Err() == context.DeadlineExceeded {
			d.log.Warn("databases: failover archive check timeout", "id", id)
		} else {
			d.log.Debug("databases: failover archive check - no archive found or list error", "id", id, "err", err)
		}
	}

	if !archiveExists {
		result.FailoverAvailable = false
		result.FailoverReason = "no archive available for restore"
		return
	}

	// All checks passed: failover is available.
	result.FailoverAvailable = true
	result.FailoverReason = fmt.Sprintf("database can be restored on one of %d healthy agents", healthyCount)
	result.FailoverETA = 180 // Expected RTO: ~3 minutes (download + restore + replay)
}

func (d *databasesAPI) delete(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	id := r.PathValue("id")

	// Verify it's a postgres database before deletion
	gr, err := d.agentCall(r, "GET", "/v1/sandboxes/"+id, workspace, nil)
	if err != nil || gr.StatusCode == http.StatusNotFound {
		if gr != nil {
			gr.Body.Close()
		}
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}
	var sb struct {
		Template string `json:"template"`
	}
	_ = json.NewDecoder(gr.Body).Decode(&sb)
	gr.Body.Close()
	if sb.Template != dbTemplate {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}

	dr, err := d.agentCall(r, "DELETE", "/v1/sandboxes/"+id, workspace, nil)
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, err.Error())
		return
	}
	defer dr.Body.Close()
	if dr.StatusCode == http.StatusNotFound {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}

	// Item 6: Clean up GCS archives after successful sandbox deletion
	_ = d.cleanupArchive(r.Context(), id)

	w.WriteHeader(http.StatusNoContent)
}

func (d *databasesAPI) connection(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	id := r.PathValue("id")
	info, err := d.fetchPGInfo(r, workspace, id)
	if err != nil {
		writeErrOrg(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if info == nil {
		writeErrOrg(w, http.StatusServiceUnavailable, "postgres not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"connection_url": fmt.Sprintf("postgres://%s:%s@%s:5432/%s", info.Username, info.Password, id+dbSNISuffix, info.Database),
		"broker_url":     dbAPIBase + "/v1/databases/" + id + "/proxy",
		"broker_token":   info.BrokerToken,
	})
}

// proxy forwards any HTTP request to the in-VM query broker on port 5544
// via the agent's existing port-proxy mechanism.
//
// Auth model: this path BYPASSES unified API auth (see dbBrokerProxyPath in
// auth.go). The credential is the per-database broker_token the caller
// received at creation time (Authorization: Bearer pds_pg_…), and it is
// enforced by the in-VM query broker itself — the same trust model as the
// native path, where {id}.db.pandastack.ai:5432 is reachable with only the
// db password. The API's job here is routing: resolve the owning workspace
// from the control-plane sandbox row (404 for ids that are not managed
// databases) so the agent's tenancy scope is satisfied without an API token.
func (d *databasesAPI) proxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	workspace := d.proxyWorkspace(r.Context(), id)
	if workspace == "" {
		// Single-node/dev fallback: no control-plane Postgres. The auth
		// bypass strips X-Fcs-Workspace, so reach the agent with the
		// see-everything dev scope rather than failing closed in a
		// single-tenant deployment. The broker still enforces its token.
		if d.db == nil {
			workspace = "default"
		} else {
			writeErrOrg(w, http.StatusNotFound, "database not found")
			return
		}
	}

	// Strip /v1/databases/{id}/proxy, keep tail (including leading slash).
	prefix := "/v1/databases/" + id + "/proxy"
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	if tail == "" || tail == "/" {
		tail = "/"
	}

	// Rewrite to the agent port-proxy path for broker port 5544.
	target := "/v1/sandboxes/" + id + "/proxy/5544" + tail
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://localhost"+target, r.Body)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "proxy: "+err.Error())
		return
	}
	// Propagate the body length. http.NewRequestWithContext cannot infer
	// ContentLength from an opaque ReadCloser (r.Body is *http.body, not one
	// of the recognized concrete types), so req.ContentLength stays 0. The
	// downstream agent hop (d.v1 → MultiNodeDirector reverse proxy) treats a
	// non-nil body with ContentLength==0 as empty and drops it, so POST
	// /proxy/v1/query reached the in-VM broker with no body ("invalid JSON:
	// EOF"). Carry the incoming length through so the broker sees the payload.
	req.ContentLength = r.ContentLength
	// Copy caller headers. Authorization survives because this path bypasses
	// unified auth (dbBrokerProxyPath) — the broker_token reaches the in-VM
	// broker, which is the layer that enforces it.
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("X-Fcs-Workspace", workspace)
	req.Header.Set("X-Pandastack-User-Id", "_db")
	req.Header.Set("X-Pandastack-Auth-Method", "db-api")
	// The agent's generic port proxy (ports.go) unconditionally strips
	// Authorization before dialing the guest, so the broker_token would
	// never reach the in-VM broker. Carry it across in an explicit header
	// that the agent promotes back to Authorization after the strip.
	if authz := r.Header.Get("Authorization"); authz != "" {
		req.Header.Set("X-Pandastack-Broker-Auth", authz)
	}
	req.RequestURI = target

	d.v1.ServeHTTP(w, req)
}

// proxyWorkspace resolves the workspace that owns database {id} from the
// control-plane sandboxes row (the agent's workspaceScope stamps
// metadata.workspace on every create). Returns "" when the row is missing,
// is not a postgres-16 sandbox, or no control-plane DB is configured —
// callers decide whether that is a 404 (prod) or a dev fallback.
func (d *databasesAPI) proxyWorkspace(ctx context.Context, id string) string {
	if d.db == nil || id == "" {
		return ""
	}
	var template string
	var metaRaw sql.NullString
	err := d.db.QueryRowContext(ctx,
		`SELECT template, metadata FROM sandboxes WHERE id = $1`, id).
		Scan(&template, &metaRaw)
	if err != nil || template != dbTemplate {
		return ""
	}
	meta := map[string]string{}
	if metaRaw.Valid && metaRaw.String != "" {
		_ = json.Unmarshal([]byte(metaRaw.String), &meta)
	}
	return meta["workspace"]
}

// ── Stats & logs ──────────────────────────────────────────────────────────────

// DatabaseStats is the live snapshot returned by GET /v1/databases/{id}/stats.
type DatabaseStats struct {
	PostgresVersion string  `json:"postgres_version,omitempty"`
	DBSizeBytes     int64   `json:"db_size_bytes"`
	Connections     int     `json:"connections"`
	MaxConnections  int     `json:"max_connections"`
	UptimeSeconds   int64   `json:"uptime_seconds"`
	CacheHitRatio   float64 `json:"cache_hit_ratio"`
	DiskSizeBytes   int64   `json:"disk_size_bytes"`
	DiskUsedBytes   int64   `json:"disk_used_bytes"`
	DiskAvailBytes  int64   `json:"disk_avail_bytes"`
	DiskUsedPct     float64 `json:"disk_used_pct"`
}

// dbStatsCmd runs as root inside the guest. psql must run as the postgres OS
// user (peer auth over the unix socket; root has no PG role) — autostart.sh
// uses the same `sudo -u postgres` pattern. The df section reports the durable
// /dev/vdb volume mounted at /var/lib/postgresql/data.
const dbStatsCmd = `sudo -u postgres psql -tA -F'|' -c "SELECT current_setting('server_version'), (SELECT sum(pg_database_size(datname)) FROM pg_database)::bigint, (SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend'), current_setting('max_connections')::int, extract(epoch from now()-pg_postmaster_start_time())::bigint, (SELECT CASE WHEN COALESCE(sum(blks_hit+blks_read),0)=0 THEN 1 ELSE round(sum(blks_hit)::numeric/sum(blks_hit+blks_read),4) END FROM pg_stat_database)" 2>/dev/null; echo ---DF---; df -B1 --output=size,used,avail /var/lib/postgresql/data 2>/dev/null | tail -n 1`

// verifyDB confirms the sandbox exists and is a postgres-16 database.
// Returns (status, true) on success; writes the error response on failure.
func (d *databasesAPI) verifyDB(w http.ResponseWriter, r *http.Request, workspace, id string) (string, bool) {
	gr, err := d.agentCall(r, "GET", "/v1/sandboxes/"+id, workspace, nil)
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, err.Error())
		return "", false
	}
	defer gr.Body.Close()
	if gr.StatusCode == http.StatusNotFound {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return "", false
	}
	var sb struct {
		Template string `json:"template"`
		Status   string `json:"status"`
	}
	_ = json.NewDecoder(gr.Body).Decode(&sb)
	if sb.Template != dbTemplate {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return "", false
	}
	return sb.Status, true
}

// execInDB runs a shell command in the guest via the agent exec endpoint.
func (d *databasesAPI) execInDB(r *http.Request, workspace, id, cmd string) (stdout string, exitCode int, err error) {
	body, _ := json.Marshal(map[string]string{"cmd": cmd})
	resp, err := d.agentCall(r, "POST", "/v1/sandboxes/"+id+"/exec", workspace, bytes.NewReader(body))
	if err != nil {
		return "", -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", -1, fmt.Errorf("exec: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", -1, err
	}
	return out.Stdout, out.ExitCode, nil
}

func (d *databasesAPI) stats(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	if workspace == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}
	id := r.PathValue("id")
	status, ok := d.verifyDB(w, r, workspace, id)
	if !ok {
		return
	}
	if status != "running" {
		writeErrOrg(w, http.StatusServiceUnavailable, "database is not running (status: "+status+")")
		return
	}

	stdout, _, err := d.execInDB(r, workspace, id, dbStatsCmd)
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, "stats exec failed: "+err.Error())
		return
	}

	pgPart, dfPart, _ := strings.Cut(stdout, "---DF---")
	var st DatabaseStats

	// psql row: version|db_size|connections|max_connections|uptime|cache_hit
	if row := strings.TrimSpace(pgPart); row != "" {
		f := strings.Split(row, "|")
		get := func(i int) string {
			if i < len(f) {
				return strings.TrimSpace(f[i])
			}
			return ""
		}
		st.PostgresVersion = get(0)
		st.DBSizeBytes = parseI64(get(1))
		st.Connections = int(parseI64(get(2)))
		st.MaxConnections = int(parseI64(get(3)))
		st.UptimeSeconds = parseI64(get(4))
		st.CacheHitRatio = parseF64(get(5))
	}

	// df row: size used avail (bytes)
	if f := strings.Fields(strings.TrimSpace(dfPart)); len(f) >= 3 {
		st.DiskSizeBytes = parseI64(f[0])
		st.DiskUsedBytes = parseI64(f[1])
		st.DiskAvailBytes = parseI64(f[2])
		if st.DiskSizeBytes > 0 {
			st.DiskUsedPct = float64(st.DiskUsedBytes) / float64(st.DiskSizeBytes) * 100
		}
	}

	if st.PostgresVersion == "" && st.DiskSizeBytes == 0 {
		writeErrOrg(w, http.StatusServiceUnavailable, "postgres is not ready (no stats available)")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// logs returns a tail of the in-guest postgresql.log (written by pg_ctl -l in
// autostart.sh). Snapshot only — the dashboard polls.
func (d *databasesAPI) logs(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	if workspace == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}
	id := r.PathValue("id")
	status, ok := d.verifyDB(w, r, workspace, id)
	if !ok {
		return
	}
	if status != "running" {
		writeErrOrg(w, http.StatusServiceUnavailable, "database is not running (status: "+status+")")
		return
	}

	lines := 300
	if v := r.URL.Query().Get("lines"); v != "" {
		if n := int(parseI64(v)); n >= 10 && n <= 2000 {
			lines = n
		}
	}
	cmd := fmt.Sprintf(`tail -n %d /var/log/postgresql/postgresql.log 2>/dev/null || tail -n %d /var/log/postgresql/initdb.log 2>/dev/null || echo "(no postgres logs yet)"`, lines, lines)
	stdout, _, err := d.execInDB(r, workspace, id, cmd)
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, "logs exec failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"logs": stdout})
}

func parseI64(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}

func parseF64(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f
}

func (d *databasesAPI) fetchPGInfo(r *http.Request, workspace, sandboxID string) (*pgInfoResponse, error) {
	resp, err := d.agentCall(r, "GET", "/v1/sandboxes/"+sandboxID+"/postgres-info", workspace, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("postgres-info: %d %s", resp.StatusCode, string(b))
	}
	var info pgInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

type pgInfoResponse struct {
	Host        string `json:"pg_host"`
	Port        int    `json:"pg_port"`
	Database    string `json:"default_database"`
	Username    string `json:"pg_user"`
	Password    string `json:"pg_password"`
	BrokerToken string `json:"broker_token"`
	BrokerURL   string `json:"broker_url"`
}

// mergeInfo combines sandbox info with PG credentials and computes the
// db-proxy connection URL using SNI routing and the public broker proxy URL.
func mergeInfo(base DatabaseInfo, pg *pgInfoResponse, sandboxID string) DatabaseInfo {
	base.Host = sandboxID + dbSNISuffix
	base.Port = 5432
	base.Database = pg.Database
	base.Username = pg.Username
	base.Password = pg.Password
	base.BrokerToken = pg.BrokerToken
	// Public REST broker URL: routed through this API's proxy handler.
	// Callers authenticate with Authorization: Bearer <broker_token>.
	base.BrokerURL = dbAPIBase + "/v1/databases/" + sandboxID + "/proxy"
	base.ConnectionURL = fmt.Sprintf("postgres://%s:%s@%s:5432/%s",
		pg.Username, pg.Password, base.Host, pg.Database)
	return base
}
