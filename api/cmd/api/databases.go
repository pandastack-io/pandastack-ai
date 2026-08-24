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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const dbTemplate = "postgres-16"

// dbTemplateBySize maps the user-facing size to its template. This build ships
// a single managed-Postgres template, so there is exactly one entry; the map
// shape is kept because size is a create-time-only choice (Firecracker cannot
// resize a snapshot-restored guest) and callers round-trip it.
var dbTemplateBySize = map[string]string{
	"1g": dbTemplate,
}

// isDBTemplate reports whether t is a managed-database template.
func isDBTemplate(t string) bool {
	for _, tpl := range dbTemplateBySize {
		if t == tpl {
			return true
		}
	}
	return false
}

// dbSizeOfTemplate is the reverse mapping, for surfacing "size" to clients.
func dbSizeOfTemplate(t string) string {
	for size, tpl := range dbTemplateBySize {
		if t == tpl {
			return size
		}
	}
	return ""
}

// dbMetaTrue parses a boolean-ish metadata value.
func dbMetaTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// agentCreatedAtEpoch converts the agent's sandbox `created_at` (serialized from
// a Go time.Time, i.e. an RFC3339 string like "2026-06-19T05:16:33Z") into the
// epoch-seconds int the dashboard expects. Returns 0 on an empty/unparseable
// value so the caller can omit the field. Previously the decode struct typed
// created_at as int64, which silently failed to unmarshal the string and left
// every database's Created column blank ("—") in the dashboard.
func agentCreatedAtEpoch(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}

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
	// SandboxID makes the DB↔sandbox identity explicit. Today the
	// database id doubles as the sandbox id; surfacing it lets clients
	// correlate with agent/sandbox state instead of relying on that implicit
	// mapping — and keeps the field stable if the ids ever diverge.
	SandboxID string `json:"sandbox_id,omitempty"`
	// Size is derived from the template.
	Size string `json:"size,omitempty"`
	// AlwaysOn opts the database out of idle auto-suspend when true.
	AlwaysOn bool `json:"always_on,omitempty"`
}

type databasesAPI struct {
	log *slog.Logger
	v1  http.Handler // internal agent proxy
	db  *sql.DB      // control-plane Postgres (optional, for listing)
	// director is the multi-node router (nil in single-node deployments).
	// Failover needs it to talk to a SPECIFIC agent directly — the normal v1
	// path routes by lease, which points at the dead agent.
	director *MultiNodeDirector
	// failoverMu + failoverInFlight serialize failover per database id: two
	// concurrent POSTs must not spawn two drain+restore pipelines racing over
	// the same volume/archive/lease.
	failoverMu       sync.Mutex
	failoverInFlight map[string]bool
	// ch is the ClickHouse read path for the historical metrics endpoint
	// (databases_metrics.go). Optional — nil in dev or when the CH sink is
	// not wired; the endpoint responds with an empty series in that case.
	ch *chState
	// sweepBreaker parks the archive-janitor sweep after repeated failures
	// so a wedged loop can't hammer GCS/Postgres on every tick.
	sweepBreaker *loopBreaker
}

func newDatabasesAPI(log *slog.Logger, v1 http.Handler, db *sql.DB, director *MultiNodeDirector) *databasesAPI {
	return &databasesAPI{log: log, v1: v1, db: db, director: director,
		failoverInFlight: map[string]bool{},
		sweepBreaker:     newLoopBreaker("archive-janitor", 5, 24*time.Hour)}
}

// WithClickHouse attaches the ClickHouse read path used by /metrics.
// Chainable, nil-safe (a nil ch just leaves the field nil).
func (d *databasesAPI) WithClickHouse(ch *chState) *databasesAPI {
	d.ch = ch
	return d
}

func (d *databasesAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/databases", d.create)
	mux.HandleFunc("GET /v1/databases", d.list)
	mux.HandleFunc("GET /v1/databases/{id}", d.get)
	mux.HandleFunc("DELETE /v1/databases/{id}", d.delete)
	mux.HandleFunc("GET /v1/databases/{id}/connection", d.connection)
	mux.HandleFunc("POST /v1/databases/{id}/failover", d.failover)
	// Historical CPU + memory + net/disk I/O time series (sandbox_metrics
	// from the agent's per-VM sampler, retained 90d, bucketed on-query).
	mux.HandleFunc("GET /v1/databases/{id}/metrics", d.metrics)
	mux.HandleFunc("POST /v1/databases/{id}/wake", d.wake)
	mux.HandleFunc("POST /v1/databases/{id}/reset-credentials", d.resetCredentials)
	mux.HandleFunc("PATCH /v1/databases/{id}", d.patch)
	mux.HandleFunc("GET /v1/databases/{id}/stats", d.stats)
	mux.HandleFunc("GET /v1/databases/{id}/logs", d.logs)
	// Catch-all proxy to the in-VM query broker (port 5544).
	// No method prefix → matches GET/POST/PUT/DELETE/PATCH.
	mux.HandleFunc("/v1/databases/{id}/proxy/", d.proxy)
	mux.HandleFunc("/v1/databases/{id}/proxy", d.proxy)
}

// agentCall proxies a request to the internal v1 handler (agent director),
// bound to the inbound request's context.
func (d *databasesAPI) agentCall(r *http.Request, method, path, workspace string, body io.Reader) (*http.Response, error) {
	return d.agentCallCtx(r.Context(), method, path, workspace, body)
}

// agentCallCtx is agentCall with an explicit context, for background work that
// must outlive the inbound request (e.g. async provisioning after a 202). Using
// r.Context() there would cancel the call the moment the 202 response is sent.
func (d *databasesAPI) agentCallCtx(ctx context.Context, method, path, workspace string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
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
		// Size selects the database template. Create-time only: a
		// snapshot-restored guest cannot be resized.
		Size string `json:"size,omitempty"`
		// AlwaysOn opts the database out of idle auto-suspend.
		AlwaysOn bool `json:"always_on,omitempty"`
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
	if opts.Size == "" {
		opts.Size = "1g"
	}
	template, ok := dbTemplateBySize[strings.ToLower(opts.Size)]
	if !ok {
		writeErrOrg(w, http.StatusBadRequest,
			"invalid size "+opts.Size+" (valid: 1g)")
		return
	}

	// Create sandbox
	meta := map[string]string{}
	if opts.Label != "" {
		meta["db.label"] = opts.Label
	}
	if opts.AlwaysOn {
		meta["db.always_on"] = "true"
	}
	createBody, _ := json.Marshal(map[string]any{
		"template":  template,
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
		ID        string `json:"id"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sbInfo); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "decode sandbox response: "+err.Error())
		return
	}

	// Seed the archive generation at 1. The WAL relay defaults to
	// gen 1 when the row metadata carries no db.archive_gen (the fresh-create
	// case), so the seeded row and the relay's default agree. Every later
	// ownership change (failover / in-place restore) BUMPS this and stamps the
	// new value into the row metadata, which is what fences a stale host's
	// uploads. Best-effort: on error the row is seeded lazily by the first bump.
	if err := d.seedArchiveGeneration(r.Context(), sbInfo.ID); err != nil {
		d.log.Warn("databases: archive generation seed failed (non-fatal)", "id", sbInfo.ID, "err", err)
	}

	// ASYNC CREATE: the agent has accepted the sandbox and returned its id (the
	// VM reaches "running" in ~3s). Postgres init (ready.json) then takes another
	// ~30-60s. We do NOT block the HTTP response on that: a 30-120s synchronous
	// create behind the CDN/edge gets its connection cut on the slow path, so the
	// browser saw a raw "Failed to fetch" / 502 even though the DB was created
	// (orphan/duplicate-on-retry risk). Instead return 202 immediately with the
	// id + status "provisioning"; the client polls GET /v1/databases/{id} until
	// it reports "running" with connection info. fetchPGInfo on that GET is what
	// surfaces the credentials once Postgres is ready.
	result := DatabaseInfo{
		ID:        sbInfo.ID,
		Status:    "provisioning",
		Template:  template,
		Size:      dbSizeOfTemplate(template),
		Label:     opts.Label,
		AlwaysOn:  opts.AlwaysOn,
		CreatedAt: agentCreatedAtEpoch(sbInfo.CreatedAt),
		Host:      sbInfo.ID + dbSNISuffix,
		Port:      5432,
		BrokerURL: dbAPIBase + "/v1/databases/" + sbInfo.ID + "/proxy",
	}

	// Background warm-up: best-effort wait for Postgres to publish credentials,
	// so a fast poll right after the 202 is more likely to already see "running".
	// Detached context (NOT r.Context(), which dies when the 202 is sent). The
	// GET poll re-fetches pg-info regardless, so this goroutine is purely an
	// optimization + a place to log a stuck provision.
	go func(id, ws string) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			if i, err := d.fetchPGInfoCtx(ctx, ws, id); err == nil && i != nil {
				d.log.Info("databases: provisioned and ready", "id", id)
				return
			}
			select {
			case <-ctx.Done():
				d.log.Warn("databases: postgres did not become ready in time (still provisioning or failed)", "id", id)
				return
			case <-t.C:
			}
		}
	}(sbInfo.ID, workspace)

	writeJSON(w, http.StatusAccepted, result)
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
		CreatedAt string            `json:"created_at"`
		Metadata  map[string]string `json:"metadata"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&all)

	out := []DatabaseInfo{}
	for _, sb := range all {
		if !isDBTemplate(sb.Template) {
			continue
		}
		info := DatabaseInfo{
			ID:        sb.ID,
			Status:    dbPublicStatus(sb.Status),
			Template:  sb.Template,
			Size:      dbSizeOfTemplate(sb.Template),
			CreatedAt: agentCreatedAtEpoch(sb.CreatedAt),
		}
		if sb.Metadata != nil {
			info.Label = sb.Metadata["db.label"]
			info.AlwaysOn = dbMetaTrue(sb.Metadata["db.always_on"])
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
		// Restore/clone-in-flight window: RestoreDatabase and runClone both
		// delete-then-recreate the row, and a lease-routed lookup can also
		// miss a row whose lease hasn't been (re)created yet. Fall back to
		// the shared control-plane table before declaring the database gone.
		if info, ok := d.getFromSharedTable(r.Context(), workspace, id); ok {
			writeJSON(w, http.StatusOK, info)
			return
		}
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}
	var sb struct {
		ID        string            `json:"id"`
		Template  string            `json:"template"`
		Status    string            `json:"status"`
		CreatedAt string            `json:"created_at"`
		Metadata  map[string]string `json:"metadata"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sb)
	if !isDBTemplate(sb.Template) {
		writeErrOrg(w, http.StatusNotFound, "database not found (template mismatch)")
		return
	}

	result := DatabaseInfo{
		ID:        sb.ID,
		Status:    dbPublicStatus(sb.Status),
		Template:  sb.Template,
		Size:      dbSizeOfTemplate(sb.Template),
		CreatedAt: agentCreatedAtEpoch(sb.CreatedAt),
		SandboxID: sb.ID,
	}
	if sb.Metadata != nil {
		result.Label = sb.Metadata["db.label"]
		result.AlwaysOn = dbMetaTrue(sb.Metadata["db.always_on"])
	}
	if sb.Status == "running" {
		info, err := d.fetchPGInfo(r, workspace, id)
		if err == nil && info != nil {
			result = mergeInfo(result, info, id)
		} else {
			// VM is up but postgres hasn't published credentials. Report
			// "provisioning" (+error) instead of a credential-less "running".
			result.Status = "provisioning"
			// Kept short + friendly: the dashboard surfaces this as the primary
			// state message on a freshly-created DB. "Launching" reads as
			// progress; the old wording read as failure.
			result.Error = "Launching…"
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

// dbPublicStatus maps internal sandbox statuses onto the database API's
// polling contract: clients poll for "running", and everything that means
// "being built right now" (fresh create, clone staging, failover restore)
// reports "provisioning" — matching what create's 202 promised.
func dbPublicStatus(sandboxStatus string) string {
	if sandboxStatus == "creating" {
		return "provisioning"
	}
	return sandboxStatus
}

// getFromSharedTable serves GET during the windows where the lease-routed
// agent lookup 404s but the database still exists in the shared sandboxes
// table (clone staging, failover restore's delete→recreate). Same tenancy
// rule as failoverAuthorize. ok=false when unavailable (no control-plane DB,
// row truly absent, template mismatch, or foreign workspace).
func (d *databasesAPI) getFromSharedTable(ctx context.Context, workspace, id string) (DatabaseInfo, bool) {
	if d.db == nil {
		return DatabaseInfo{}, false
	}
	var template, status string
	var metaRaw sql.NullString
	var createdAt sql.NullInt64 // BIGINT epoch seconds in the shared schema
	err := d.db.QueryRowContext(ctx,
		`SELECT template, status, metadata, created_at FROM sandboxes WHERE id = $1`, id).
		Scan(&template, &status, &metaRaw, &createdAt)
	if err != nil {
		return DatabaseInfo{}, false
	}
	if !isDBTemplate(template) {
		return DatabaseInfo{}, false
	}
	meta := map[string]string{}
	if metaRaw.Valid && metaRaw.String != "" {
		_ = json.Unmarshal([]byte(metaRaw.String), &meta)
	}
	if workspace != "admin" && workspace != "default" && meta["workspace"] != workspace {
		return DatabaseInfo{}, false
	}
	return DatabaseInfo{
		ID:        id,
		Status:    dbPublicStatus(status),
		Template:  template,
		Size:      dbSizeOfTemplate(template),
		Label:     meta["db.label"],
		AlwaysOn:  dbMetaTrue(meta["db.always_on"]),
		CreatedAt: createdAt.Int64,
		SandboxID: id,
	}, true
}

// recoveringFrom reports whether any database is still replaying out of
// srcID's archive: status "creating" (staging), or "running" but created
// within the stage timeout (WAL replay continues in-guest after the row flips
// running). Conservative by design — a false positive only delays a delete by
// minutes; a false negative destroys the reader's data source mid-replay.
func (d *databasesAPI) recoveringFrom(ctx context.Context, srcID string) (bool, string) {
	if d.db == nil {
		return false, ""
	}
	// The LIKE below embeds $1 between JSON quotes; only a well-formed UUID
	// (no LIKE metacharacters) may participate.
	if _, err := uuid.Parse(srcID); err != nil {
		return false, ""
	}
	// metadata is a TEXT column (agent migration 00001), NOT jsonb — a ->>
	// operator errors on it and, because this guard fails closed, would
	// block EVERY delete. Substring-match the exact JSON pair instead:
	// srcID is a validated UUID (LIKE-safe) and Go's json.Marshal emits
	// `"db.recover_from":"<uuid>"` with no whitespace.
	var readerID string
	err := d.db.QueryRowContext(ctx, `
		SELECT id FROM sandboxes
		WHERE metadata LIKE '%"db.recover_from":"' || $1 || '"%'
		  AND (status = 'creating'
		       OR (status = 'running' AND created_at > EXTRACT(EPOCH FROM now())::bigint - 2700))
		LIMIT 1`, srcID).Scan(&readerID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		// Fail CLOSED: the downstream archive purge is irreversible, and a
		// delete blocked by a transient query error is retryable in seconds.
		d.log.Warn("databases: recovery-in-flight check errored; refusing delete", "id", srcID, "err", err)
		return true, "unknown (lookup failed — retry)"
	}
	return true, readerID
}

func (d *databasesAPI) cleanupArchive(ctx context.Context, id string) error {
	// Item 6: GCS archive cleanup on delete (GDPR/data privacy)
	// Delete all WAL + base backups from gs://$BUCKET/db/{id}/
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if bucket == "" {
		return nil // no bucket = no archive
	}
	prefix := "gs://" + bucket + "/db/" + id
	// P1 item 3: retried, and loudly surfaced on final failure. A silent purge
	// failure leaves customer data in GCS forever — the archive janitor
	// (StartArchiveJanitor) is the durable backstop, but the ERROR log is what
	// makes the failure visible instead of a Warn nobody reads.
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		actx, cancel := context.WithTimeout(ctx, 30*time.Second)
		// Resolve gsutil to an absolute path — the API systemd service PATH
		// excludes /snap/bin where gsutil lives, so a bare "gsutil" silently
		// fails to exec.
		cmd := exec.CommandContext(actx, gsutilPath(), "-m", "rm", "-r", prefix)
		lastErr = cmd.Run()
		cancel()
		if lastErr == nil {
			d.log.Info("database archive cleanup complete", "id", id, "attempt", attempt)
			// The archive is gone — drop its catalog rows so the dashboard never
			return nil
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	d.log.Error("database archive cleanup FAILED after retries — archive orphaned until janitor sweep",
		"id", id, "prefix", prefix, "err", lastErr)
	return nil // never block the delete; the janitor reconciles
}

// StartArchiveJanitor launches the archive sweeper. Per pass it walks
// gs://$BUCKET/db/ and, per archive: purges orphans (row gone — guarded by
// grace + a row re-check), and prunes live archives to the configured
// retention window (PANDASTACK_BACKUP_RETENTION_DAYS).
//
// Coordination: a full sweep is gsutil listings over EVERY archive — seconds of
// CPU each on the small edge VMs. Every edge runs this janitor, so two guards
// keep it off the request path's back: the 10-min startup delay (a deploy
// restarts all edges at once), and claimJanitorSweep — a shared-Postgres claim
// that lets exactly ONE edge sweep per window. Before that claim existed, every
// deploy kicked simultaneous full-fleet sweeps on all edges ~10 min later,
// pinning fleet CPU and flapping the LB health checks (2026-08-20 incident).
func (d *databasesAPI) StartArchiveJanitor(ctx context.Context) {
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if bucket == "" || d.db == nil {
		return
	}
	go func() {
		// Initial delay so a fleet-wide restart doesn't stampede GCS.
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			// Claim window slightly under the tick so one healthy edge's cadence
			// is never skipped by its own previous claim.
			if d.claimJanitorSweep(ctx, 4*time.Hour) {
				d.sweepOrphanArchives(ctx, bucket)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (d *databasesAPI) sweepOrphanArchives(ctx context.Context, bucket string) {
	// A consecutively-failing sweep parks itself instead of
	// hammering GCS/Postgres every interval (the 2026-08-20 stampede class).
	if !d.sweepBreaker.Allow() {
		d.log.Warn("archive janitor: circuit breaker open — sweep parked")
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(lctx, gsutilPath(), "ls", "gs://"+bucket+"/db/").Output()
	if err != nil {
		d.log.Warn("archive janitor: list failed", "err", err)
		if d.sweepBreaker.Fail() {
			d.log.Error("archive janitor: CIRCUIT BREAKER TRIPPED — sweep parked for 24h after repeated list failures")
		}
		return
	}
	d.sweepBreaker.Success()
	// Housekeeping: GC long-expired archive leases.
	d.expireArchiveLeases(ctx)
	first := true
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// Lines look like gs://<bucket>/db/<id>/ — extract the id.
		id := strings.Trim(strings.TrimPrefix(line, "gs://"+bucket+"/db/"), "/")
		if id == "" || strings.Contains(id, "/") {
			continue
		}
		// Pace the sweep: each db costs seconds of gsutil CPU, and this runs on
		// a small edge VM that is also serving traffic. A short gap between dbs
		// keeps the health checks (and real requests) responsive throughout.
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		first = false
		// Renew the sweep claim as we go: the claim is a start timestamp, so a
		// sweep outliving the claim window would let a second edge start
		// sweeping concurrently. Touching last_sweep_at per db keeps the mutual
		// exclusion true for arbitrarily long sweeps (one tiny UPDATE per db,
		// already paced 2s apart).
		if _, err := d.db.ExecContext(ctx,
			`UPDATE db_janitor_state SET last_sweep_at = now() WHERE id = 1`); err != nil {
			d.log.Warn("archive janitor: claim renewal failed", "err", err)
		}
		hasRow, ok := d.archiveHasRow(ctx, id)
		if !ok {
			continue // transient DB error — never purge/prune on a hiccup
		}
		if !hasRow {
			// The database is gone — reclaim its archive so nothing is left
			// behind. Guarded inside purgeOrphanArchive.
			d.purgeOrphanArchive(ctx, bucket, id)
			continue
		}
		// Live database — prune to the configured retention window.
		if retentionEnabled() {
			d.pruneArchiveToRetention(ctx, bucket, id)
		}
	}
}

// isAgentTransient reports whether an agent-proxied status means "the host is
// transiently unreachable" (the director's error handler records 502; LB/edge
// shapes produce 503/504) — cases that must surface as retry-later, never as
// "database not found".
func isAgentTransient(status int) bool {
	return status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

// dbArchiveExists reports whether a restorable GCS archive (base backup)
// exists for the database. Shared by the GET failover-availability fields and
// by the failover handler's preflight — the check that stops failover from
// draining a primary when the restore cannot possibly succeed.
func dbArchiveExists(ctx context.Context, id string) (bool, string) {
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if bucket == "" {
		return false, "GCS archive not configured"
	}
	archCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(archCtx, gsutilPath(), "-m", "ls", "-r",
		"gs://"+bucket+"/db/"+id+"/base/")
	output, err := cmd.CombinedOutput()
	if err == nil && len(output) > 0 {
		return true, ""
	}
	if archCtx.Err() == context.DeadlineExceeded {
		return false, "archive check timed out"
	}
	return false, "no archive available for restore"
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
	if ok, reason := dbArchiveExists(ctx, id); !ok {
		result.FailoverAvailable = false
		result.FailoverReason = reason
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

	// Verify it's a postgres database before deletion. An unreachable
	// agent is NOT "not found" — the database exists, the host is transiently
	// down. NOTE: agentCall serves in-process (httptest recorder), so
	// unreachable agents arrive as a RECORDED 502/503 response from the
	// director's error handler, never as err != nil — gate on the status.
	gr, err := d.agentCall(r, "GET", "/v1/sandboxes/"+id, workspace, nil)
	if err != nil || isAgentTransient(gr.StatusCode) {
		if gr != nil {
			gr.Body.Close()
		}
		w.Header().Set("Retry-After", "15")
		writeErrOrg(w, http.StatusServiceUnavailable,
			"database host is transiently unreachable (agent restarting?) — retry shortly")
		return
	}
	if gr.StatusCode == http.StatusNotFound {
		gr.Body.Close()
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}
	var sb struct {
		Template string `json:"template"`
	}
	_ = json.NewDecoder(gr.Body).Decode(&sb)
	gr.Body.Close()
	if !isDBTemplate(sb.Template) {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}

	// Clone-in-flight guard: a clone of this database replays the source's
	// GCS archive segment-by-segment for its whole recovery — deleting the
	// source now would purge that archive mid-replay and silently truncate the
	// reader. Refuse while anything is still in its provisioning window.
	if inFlight, readerID := d.recoveringFrom(r.Context(), id); inFlight {
		writeErrOrg(w, http.StatusConflict,
			"another database ("+readerID+") is still recovering from this one's backups — retry the delete once it is running")
		return
	}

	dr, err := d.agentCall(r, "DELETE", "/v1/sandboxes/"+id, workspace, nil)
	if err != nil || isAgentTransient(dr.StatusCode) {
		if dr != nil {
			dr.Body.Close()
		}
		w.Header().Set("Retry-After", "15")
		writeErrOrg(w, http.StatusServiceUnavailable,
			"database host is transiently unreachable (agent restarting?) — retry shortly")
		return
	}
	defer dr.Body.Close()
	if dr.StatusCode == http.StatusNotFound {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}
	// The archive purge is IRREVERSIBLE — only run it when the sandbox delete
	// actually succeeded. The old code fell through to purge (and 204) on any
	// non-404 status, including agent 5xx: a failed delete would still have
	// destroyed the only backup.
	if dr.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(dr.Body, 2048))
		writeErrOrg(w, http.StatusBadGateway,
			"agent delete failed (HTTP "+dr.Status+"): "+strings.TrimSpace(string(b)))
		return
	}

	// Item 6: Clean up GCS archives after successful sandbox deletion
	_ = d.cleanupArchive(r.Context(), id)

	w.WriteHeader(http.StatusNoContent)
}

// wake handles POST /v1/databases/{id}/wake: the documented, DB-level
// escape hatch for a hibernated database. Connecting through the proxy wakes
// a DB implicitly; this makes recovery explicit and discoverable instead of
// requiring knowledge of the sandbox-internal wake route.
// patch updates mutable database config. Currently only always_on (the
// idle auto-suspend opt-out). Forwards to the owning agent's
// PATCH /db/{id}/config, which updates the sandbox metadata the idle sweep
// reads. Returns the refreshed database view.
func (d *databasesAPI) patch(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	id := r.PathValue("id")

	// Confirm it's a managed database in this workspace before mutating.
	if _, ok := d.verifyDB(w, r, workspace, id); !ok {
		return
	}

	var req struct {
		AlwaysOn *bool `json:"always_on,omitempty"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErrOrg(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}
	if req.AlwaysOn == nil {
		writeErrOrg(w, http.StatusBadRequest, "no updatable fields provided (supported: always_on)")
		return
	}

	body, _ := json.Marshal(map[string]any{"always_on": *req.AlwaysOn})
	resp, err := d.agentCall(r, "PATCH", "/db/"+id+"/config", workspace, bytes.NewReader(body))
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, "could not update database config: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		writeErrOrg(w, http.StatusBadGateway, "config update failed: "+strings.TrimSpace(string(b)))
		return
	}
	// Return the refreshed view (re-reads metadata → surfaces always_on).
	d.get(w, r)
}

func (d *databasesAPI) wake(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	id := r.PathValue("id")

	gr, err := d.agentCall(r, "GET", "/v1/sandboxes/"+id, workspace, nil)
	if err != nil || isAgentTransient(gr.StatusCode) {
		if gr != nil {
			gr.Body.Close()
		}
		w.Header().Set("Retry-After", "15")
		writeErrOrg(w, http.StatusServiceUnavailable,
			"database host is transiently unreachable (agent restarting?) — retry shortly")
		return
	}
	code := gr.StatusCode
	var sb struct {
		Template string `json:"template"`
	}
	_ = json.NewDecoder(gr.Body).Decode(&sb)
	gr.Body.Close()
	if code == http.StatusNotFound || !isDBTemplate(sb.Template) {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return
	}

	wr, err := d.agentCall(r, "POST", "/v1/sandboxes/"+id+"/wake", workspace, nil)
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, "wake failed: "+err.Error())
		return
	}
	defer wr.Body.Close()
	if wr.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(wr.Body, 2048))
		writeErrOrg(w, http.StatusBadGateway,
			"wake failed (HTTP "+wr.Status+"): "+strings.TrimSpace(string(b)))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"id":     id,
		"status": "waking",
		"detail": "database is waking; poll GET /v1/databases/{id} until running",
	})
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

	// The single most-hit auth trap — callers send their ACCOUNT API key
	// (pds_…), but the broker only accepts the per-database broker_token
	// (pds_pg_…). The broker's own reply is a bare "unauthorized"; catch the
	// unambiguous wrong-credential-type case here with an actionable message.
	// Anything else (missing header, pds_pg_ token, other schemes) passes
	// through — the in-VM broker remains the enforcement layer.
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer pds_") &&
		!strings.HasPrefix(authz, "Bearer pds_pg_") {
		writeErrOrg(w, http.StatusUnauthorized,
			"the query broker requires the per-database broker_token (pds_pg_…) from "+
				"GET /v1/databases/{id} — not your account API key. Note: broker_token is "+
				"only returned while the database is running; wake it first if hibernated")
		return
	}

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

	// Buffer the (small, JSON) request body so the wake-on-connect retry
	// below can replay it. The agent auto-wakes an auto-suspended database
	// when this broker request arrives (activityTracker → EnsureRunning),
	// then dials the in-VM broker; if that first dial lands in the sub-second
	// window before the just-restored broker is re-listening, the agent
	// returns a transient 502/503/504. Retrying once, invisibly, keeps
	// auto-suspend silent — a single query never surfaces the resume. Cap the
	// buffer so a huge body isn't held in memory (falls back to stream+no-retry).
	var bodyBuf []byte
	retryable := true
	if r.Body != nil {
		const maxBufferedBody = 8 << 20 // 8 MiB — broker enforces its own smaller limit
		limited := io.LimitReader(r.Body, maxBufferedBody+1)
		buf, _ := io.ReadAll(limited)
		if len(buf) > maxBufferedBody {
			// Too large to safely buffer/replay — stream it through once.
			retryable = false
			bodyBuf = nil
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
		} else {
			bodyBuf = buf
		}
	}

	newAgentReq := func() (*http.Request, error) {
		var body io.Reader
		if bodyBuf != nil {
			body = bytes.NewReader(bodyBuf)
		} else if r.Body != nil {
			body = r.Body
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://localhost"+target, body)
		if err != nil {
			return nil, err
		}
		// Propagate the body length. http.NewRequestWithContext cannot infer
		// ContentLength from an opaque ReadCloser, so the downstream agent hop
		// would drop a non-nil body with ContentLength==0 (POST /proxy/v1/query
		// → broker "invalid JSON: EOF"). Set it explicitly.
		if bodyBuf != nil {
			req.ContentLength = int64(len(bodyBuf))
		} else {
			req.ContentLength = r.ContentLength
		}
		for k, vs := range r.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("X-Fcs-Workspace", workspace)
		req.Header.Set("X-Pandastack-User-Id", "_db")
		req.Header.Set("X-Pandastack-Auth-Method", "db-api")
		// The agent's generic port proxy strips Authorization before dialing
		// the guest; carry the broker_token across in an explicit header the
		// agent promotes back to Authorization.
		if authz := r.Header.Get("Authorization"); authz != "" {
			req.Header.Set("X-Pandastack-Broker-Auth", authz)
		}
		req.RequestURI = target
		return req, nil
	}

	forward := func() *httptest.ResponseRecorder {
		req, err := newAgentReq()
		rr := httptest.NewRecorder()
		if err != nil {
			rr.Code = http.StatusInternalServerError
			_, _ = rr.WriteString("proxy: " + err.Error())
			return rr
		}
		d.v1.ServeHTTP(rr, req)
		return rr
	}

	rr := forward()

	// Wake-on-connect absorption — narrowly and SAFELY. The agent's
	// activityTracker wakes an auto-suspended database inline before the port
	// proxy dials the in-VM broker, so a warm database answers on the first
	// try. The only residual failure is the sub-second window where the guest
	// is running but the just-restored broker socket isn't re-listening yet:
	// the port proxy's ReverseProxy ErrorHandler returns a bare 502 for that
	// connection-refused, and — because the request was never delivered to the
	// broker — replaying it is provably safe (no double-execution of a
	// mutation). We therefore retry ONLY on 502, and only after an idempotent
	// readiness poll of the broker's /v1/health confirms it is back, then
	// forward exactly once more. A 503 (broker answered — e.g. postgres
	// genuinely down/recovering) or 504 (ambiguous — the request may have
	// executed) is NEVER retried: that avoids both re-running a committed
	// mutation and amplifying a real outage with backoff latency.
	if retryable && rr.Code == http.StatusBadGateway && r.Context().Err() == nil {
		if d.waitBrokerReady(r.Context(), workspace, id) {
			rr = forward()
		}
	}

	for k, vs := range rr.Header() {
		w.Header()[k] = vs
	}
	w.WriteHeader(rr.Code)
	_, _ = w.Write(rr.Body.Bytes())
}

// waitBrokerReady polls the in-VM query broker's idempotent /v1/health via
// the agent port proxy until it answers 200 (broker up) or the bounded
// deadline passes. Used only to absorb the post-wake broker-not-listening-yet
// window before re-forwarding a request. Returns true if the broker became
// ready. The health probe carries no body and mutates nothing.
func (d *databasesAPI) waitBrokerReady(ctx context.Context, workspace, id string) bool {
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	healthTarget := "/v1/sandboxes/" + id + "/proxy/5544/v1/health"
	t := time.NewTicker(750 * time.Millisecond)
	defer t.Stop()
	for {
		req, err := http.NewRequestWithContext(hctx, http.MethodGet, "http://localhost"+healthTarget, nil)
		if err == nil {
			req.Header.Set("X-Fcs-Workspace", workspace)
			req.Header.Set("X-Pandastack-User-Id", "_db")
			req.Header.Set("X-Pandastack-Auth-Method", "db-api")
			req.RequestURI = healthTarget
			rr := httptest.NewRecorder()
			d.v1.ServeHTTP(rr, req)
			if rr.Code == http.StatusOK {
				return true
			}
		}
		select {
		case <-hctx.Done():
			return false
		case <-t.C:
		}
	}
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
	if err != nil || !isDBTemplate(template) {
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
// application_name=pds-stats so the DB idle-suspend sweep (agent db_idle.go)
// excludes this poll from the activity signal — the dashboard refreshing stats
// every 5s must NOT keep an otherwise-idle database awake.
const dbStatsCmd = `sudo -u postgres psql "application_name=pds-stats" -tA -F'|' -c "SELECT current_setting('server_version'), (SELECT sum(pg_database_size(datname)) FROM pg_database)::bigint, (SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend'), current_setting('max_connections')::int, extract(epoch from now()-pg_postmaster_start_time())::bigint, (SELECT CASE WHEN COALESCE(sum(blks_hit+blks_read),0)=0 THEN 1 ELSE round(sum(blks_hit)::numeric/sum(blks_hit+blks_read),4) END FROM pg_stat_database)" 2>/dev/null; echo ---DF---; df -B1 --output=size,used,avail /var/lib/postgresql/data 2>/dev/null | tail -n 1`

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
	if !isDBTemplate(sb.Template) {
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
	return d.fetchPGInfoCtx(r.Context(), workspace, sandboxID)
}

// fetchPGInfoCtx is fetchPGInfo with an explicit context (for background
// provisioning that outlives the inbound request).
func (d *databasesAPI) fetchPGInfoCtx(ctx context.Context, workspace, sandboxID string) (*pgInfoResponse, error) {
	resp, err := d.agentCallCtx(ctx, "GET", "/v1/sandboxes/"+sandboxID+"/postgres-info", workspace, nil)
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
