// SPDX-License-Identifier: Apache-2.0
//
// databases_failover.go — POST /v1/databases/{id}/failover (managed-DB
// roadmap item 4: restore on another agent, the actual availability win).
//
// When the agent hosting a managed database dies, this endpoint rebuilds the
// database on a healthy agent from its GCS archive (latest base backup +
// archived WAL, written continuously by the per-agent WAL relay) and boots it
// under the SAME sandbox id. Because db-proxy resolves <id>.db.pandastack.ai
// from the lease table per connection, routing follows automatically; the
// password rotates on every restore (kickPGPhase2), so no credential state
// needs to move — callers read the fresh connection info from the response.
//
// RPO: bounded by archive_timeout (60s) — at most the last minute of writes.
// RTO: dominated by the base-backup download + postgres WAL replay.
//
// Everything here deliberately bypasses the lease-routed v1 director for
// agent calls: the lease points at the agent we are failing AWAY FROM.

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pandastack/api/internal/scheduler"
)

const (
	// dbFailoverDrainTimeout caps the best-effort DELETE against the old
	// agent. If the host is truly dead this just burns the timeout once.
	dbFailoverDrainTimeout = 20 * time.Second
	// dbFailoverRestoreTimeout covers base-backup download from GCS plus
	// VM boot on the target agent — minutes for large databases.
	dbFailoverRestoreTimeout = 10 * time.Minute
	// dbFailoverReadyTimeout bounds the post-restore poll for postgres to
	// finish WAL replay, promote, and publish fresh credentials.
	dbFailoverReadyTimeout = 180 * time.Second
)

// failover handles POST /v1/databases/{id}/failover.
//
// PREFLIGHT-THEN-ACT (the B1/B2 fix): every check that could make this a
// no-op runs BEFORE anything touches the primary — (1) refuse a database
// that is running on a healthy agent unless force:true (the old code would
// happily drain a healthy primary and strand it when the restore failed),
// (2) require a valid restore target, (3) require a restorable GCS archive.
// Only after all three pass does the drain+restore start — and it runs in
// the BACKGROUND with a 202 (the B3 fix: the old synchronous handler held
// the request through a multi-minute restore and the CDN cut it at ~100s,
// surfacing as a raw 502 while work was still in flight).
func (d *databasesAPI) failover(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	if workspace == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}
	if d.director == nil || d.db == nil {
		writeErrOrg(w, http.StatusNotImplemented, "failover requires a multi-node deployment")
		return
	}
	id := r.PathValue("id")

	// Optional body: {"force": true} turns the healthy-primary refusal into a
	// planned migration. Absent/invalid body = force off.
	var req struct {
		Force bool `json:"force"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	}

	// Ownership + template check straight from the shared sandboxes table.
	// verifyDB/agentCall would route the lookup to the OWNING agent — which
	// is presumed dead; that's the whole reason failover is being invoked.
	meta, template, ok := d.failoverAuthorize(w, r, workspace, id)
	if !ok {
		return
	}

	// GUARD 1 (B1): without force, failover is ONLY for databases whose row
	// says "failed". This gate deliberately depends on nothing that can be
	// stale or error transiently (lease rows, heartbeat caches) — the July
	// E2E proved a healthy running primary could be drained; fail CLOSED on
	// every other status instead:
	//   running    → the primary is fine; forcing is a planned migration.
	//   hibernated → use POST /v1/databases/{id}/wake, not failover.
	//   anything else / unknown → refuse rather than guess.
	if !req.Force {
		switch st := d.sandboxStatus(r.Context(), id); st {
		case "failed":
			// legitimate failover — proceed
		case "hibernated":
			writeErrOrg(w, http.StatusConflict,
				"database is hibernated, not failed — use POST /v1/databases/{id}/wake")
			return
		case "running":
			writeErrOrg(w, http.StatusConflict,
				"database is running; failover is for failed databases. "+
					`POST {"force":true} to run a planned migration anyway`)
			return
		default:
			writeErrOrg(w, http.StatusConflict,
				"database status is '"+st+"' — failover only applies to failed databases "+
					`(POST {"force":true} to override)`)
			return
		}
	}

	// Identify the current owner so we can exclude it from target selection
	// and stop any half-alive postgres (split-brain guard: two instances both
	// archiving WAL under one id). Lookup failure is tolerable HERE — it only
	// degrades drain/exclusion, never the guard above.
	current, err := d.director.sched.LookupLease(r.Context(), id)
	if err != nil {
		d.log.Warn("databases: failover lease lookup failed (continuing)", "id", id, "err", err)
	}

	// GUARD 2 (B2): a valid restore target must exist BEFORE anything touches
	// the primary. No target → clean no-op, primary untouched.
	target := d.pickFailoverTarget(r.Context(), current)
	if target == nil {
		writeErrOrg(w, http.StatusServiceUnavailable,
			"no healthy agent available to fail over to (primary left untouched)")
		return
	}

	// GUARD 3 (B2): a restorable archive must exist. Draining the primary
	// when the restore cannot possibly succeed would destroy the only
	// running copy for nothing.
	if ok, reason := dbArchiveExists(r.Context(), id); !ok {
		writeErrOrg(w, http.StatusPreconditionFailed,
			"no restorable archive ("+reason+"); refusing to touch the primary")
		return
	}

	// Serialize per id: a second POST while a failover is in flight must not
	// spawn a second drain+restore pipeline racing over the same volume,
	// archive, and lease.
	d.failoverMu.Lock()
	if d.failoverInFlight[id] {
		d.failoverMu.Unlock()
		writeErrOrg(w, http.StatusConflict, "a failover for this database is already in progress")
		return
	}
	d.failoverInFlight[id] = true
	d.failoverMu.Unlock()

	// All preflights passed: run drain+restore in the background and answer
	// 202 immediately. Callers poll GET /v1/databases/{id}; note the row can
	// briefly report not-found between the drain and the restore's create.
	go d.runFailover(workspace, id, template, meta, current, target)

	writeJSON(w, http.StatusAccepted, DatabaseInfo{
		ID:       id,
		Status:   "restoring",
		Template: template,
		Size:     dbSizeOfTemplate(template),
		Label:    meta["db.label"],
		Error: "failover started (target agent " + target.ID + "); poll GET /v1/databases/{id} — " +
			"the database may briefly report not-found while the restore provisions",
	})
}

// runFailover is the background half of failover: drain the old owner,
// restore on the target, repoint the lease cache, and log readiness. All
// preflights already passed; errors here are logged (the caller has its 202).
func (d *databasesAPI) runFailover(workspace, id, template string, meta map[string]string, current, target *scheduler.Agent) {
	defer func() {
		d.failoverMu.Lock()
		delete(d.failoverInFlight, id)
		d.failoverMu.Unlock()
	}()
	// TUSK T1.1 + T1.4: this is an ownership change — bump the archive
	// generation (fences the old host's future uploads behind a lower gen) and
	// stamp the new value into the metadata the restored row will carry, so
	// the new owner's WAL relay stamps its base backups with it. Also lease
	// the archive chain: the restore is about to replay from it, and the
	// retention pruner must not GC the anchor mid-replay.
	fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
	gen, gerr := d.bumpArchiveGenerationRetry(fctx, id, "failover:"+target.ID)
	if gerr != nil {
		// ABORT rather than proceed unfenced. An unfenced failover can restore the
		// database onto an abandoned timeline (see bumpArchiveGenerationRetry). The
		// database stays in its current state and the failover is retryable — a
		// delayed-but-correct failover beats a fast-but-silently-wrong one.
		fcancel()
		d.log.Error("databases: failover aborted — could not fence archive generation", "id", id, "err", gerr)
		return
	}
	if meta == nil {
		meta = map[string]string{}
	}
	meta["db.archive_gen"] = strconv.FormatInt(gen, 10)
	leaseHolder := "failover:" + target.ID
	if lerr := d.acquireArchiveLease(fctx, id, leaseHolder, "failover", 45*time.Minute); lerr != nil {
		d.log.Warn("databases: failover archive lease failed (continuing)", "id", id, "err", lerr)
	}
	fcancel()
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		d.releaseArchiveLease(rctx, id, leaseHolder)
		rcancel()
	}()
	if current != nil {
		d.drainOldAgent(current, id)
	}
	if err := d.restoreOnAgent(target, id, template, meta); err != nil {
		d.log.Error("databases: failover restore failed", "id", id, "agent", target.ID, "err", err)
		return
	}

	// Bust this edge's lease cache (persistent sandboxes are cached 1h) so
	// subsequent requests through this edge route to the new owner. The PG
	// lease row itself was retargeted by the upsert inside the agent's Create.
	d.director.sched.RememberLeasePersistent(id, *target)
	d.log.Info("databases: failover restore accepted",
		"id", id, "target_agent", target.ID, "endpoint", target.Endpoint)

	if info := d.waitPGReadyCtx(workspace, id); info != nil {
		d.log.Info("databases: failover complete — postgres ready", "id", id, "agent", target.ID)
	} else {
		d.log.Warn("databases: failover restored but postgres not ready in window (still recovering?)",
			"id", id, "agent", target.ID)
	}
}

// agentHealthy mirrors pickFailoverTarget's health rule: active + fresh
// heartbeat. Package-level and pure so the failover guard is unit-testable.
func agentHealthy(a *scheduler.Agent) bool {
	return a != nil && a.Status == "active" && a.Endpoint != "" &&
		time.Since(a.LastHeartbeat) <= 30*time.Second
}

// sandboxStatus reads the sandbox row status from the shared control-plane
// table (NOT lease-routed — same reasoning as failoverAuthorize). Empty
// string when the row is missing or the query fails.
func (d *databasesAPI) sandboxStatus(ctx context.Context, id string) string {
	var status string
	if err := d.db.QueryRowContext(ctx,
		`SELECT status FROM sandboxes WHERE id = $1`, id).Scan(&status); err != nil {
		return ""
	}
	return status
}

// failoverAuthorize verifies the sandbox row exists, is a managed database,
// and belongs to the caller's workspace. Returns the row's metadata (which
// carries workspace + db.label and is re-applied to the restored sandbox)
// and the row's TEMPLATE (so failover/clone preserve the RAM tier).
// Writes the error response itself when returning ok=false.
func (d *databasesAPI) failoverAuthorize(w http.ResponseWriter, r *http.Request, workspace, id string) (map[string]string, string, bool) {
	var template string
	var metaRaw sql.NullString
	err := d.db.QueryRowContext(r.Context(),
		`SELECT template, metadata FROM sandboxes WHERE id = $1`, id).
		Scan(&template, &metaRaw)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return nil, "", false
	}
	if err != nil {
		d.log.Error("databases: failover sandbox lookup failed", "id", id, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "sandbox lookup failed")
		return nil, "", false
	}
	if !isDBTemplate(template) {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return nil, "", false
	}
	meta := map[string]string{}
	if metaRaw.Valid && metaRaw.String != "" {
		_ = json.Unmarshal([]byte(metaRaw.String), &meta)
	}
	// Same tenancy rule as the agent's workspaceScope: admin/default see
	// everything, everyone else only their own rows (no empty-owner leak).
	if workspace != "admin" && workspace != "default" && meta["workspace"] != workspace {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return nil, "", false
	}
	if meta["workspace"] == "" {
		meta["workspace"] = workspace
	}
	return meta, template, true
}

// pickFailoverTarget returns the healthiest agent that is not the current
// owner: status active, fresh heartbeat, most warm-pool-style recency wins
// (we simply prefer the freshest heartbeat — the restore is IO-bound on GCS,
// not CPU-bound, so fine-grained scoring buys nothing here).
func (d *databasesAPI) pickFailoverTarget(ctx context.Context, current *scheduler.Agent) *scheduler.Agent {
	agents, err := d.director.sched.List(ctx)
	if err != nil {
		d.log.Error("databases: failover agent list failed", "err", err)
		return nil
	}
	var target *scheduler.Agent
	for i := range agents {
		a := agents[i]
		if a.Status != "active" || a.Endpoint == "" {
			continue
		}
		if time.Since(a.LastHeartbeat) > 30*time.Second {
			continue
		}
		if current != nil && a.ID == current.ID {
			continue
		}
		if target == nil || a.LastHeartbeat.After(target.LastHeartbeat) {
			target = &a
		}
	}
	return target
}

// drainOldAgent best-effort STOPS the database VM on the old owner via
// hibernate. Errors are logged and ignored — the old host is normally
// unreachable, that is why we are here. If it IS half-alive, this stops
// postgres so two instances never archive WAL under the same id.
//
// Deliberately hibernate, NOT delete: (a) the agent's managed-sandbox guard
// 409s a plain DELETE anyway, and a force-DELETE would destroy the old
// volume — the last local copy of up to archive_timeout worth of tail WAL;
// (b) hibernate keeps the row + volume, so if the restore then fails the
// database is still recoverable via wake instead of gone.
func (d *databasesAPI) drainOldAgent(current *scheduler.Agent, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), dbFailoverDrainTimeout)
	defer cancel()
	resp, err := d.directAgentCall(ctx, http.MethodPost,
		strings.TrimRight(current.Endpoint, "/")+"/sandboxes/"+id+"/hibernate", nil)
	if err != nil {
		d.log.Info("databases: failover old-agent drain failed (expected if host is dead)",
			"id", id, "agent", current.ID, "err", err)
		return
	}
	defer resp.Body.Close()
	d.log.Info("databases: failover drained old agent (hibernate)",
		"id", id, "agent", current.ID, "status", resp.StatusCode)
}

// restoreOnAgent invokes POST /db/{id}/restore on the target agent.
// template preserves the database's RAM tier across the move.
func (d *databasesAPI) restoreOnAgent(target *scheduler.Agent, id, template string, meta map[string]string) error {
	body, _ := json.Marshal(map[string]any{"metadata": meta, "template": template})
	// Background context: nothing client-derived here — a caller that gave up
	// must not abort the multi-minute volume rebuild on the agent.
	ctx, cancel := context.WithTimeout(context.Background(), dbFailoverRestoreTimeout)
	defer cancel()
	resp, err := d.directAgentCall(ctx, http.MethodPost,
		strings.TrimRight(target.Endpoint, "/")+"/db/"+id+"/restore", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New("restore rejected by agent (HTTP " +
			resp.Status + "): " + strings.TrimSpace(string(b)))
	}
	return nil
}

// waitPGReadyCtx polls postgres-info (now lease-routed to the NEW agent)
// until credentials appear or the deadline passes. nil on timeout. Uses its
// own context — it runs from the background failover goroutine.
func (d *databasesAPI) waitPGReadyCtx(workspace, id string) *pgInfoResponse {
	ctx, cancel := context.WithTimeout(context.Background(), dbFailoverReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if info, err := d.fetchPGInfoCtx(ctx, workspace, id); err == nil && info != nil {
			return info
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// directAgentCall talks to a specific agent endpoint, bypassing lease
// routing. Agent routes carry no /v1 prefix; auth is the shared node token
// (same header the director injects on proxied requests).
func (d *databasesAPI) directAgentCall(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if d.director.nodeToken != "" {
		req.Header.Set("X-Node-Token", d.director.nodeToken)
	}
	client := &http.Client{Transport: d.director.transport}
	return client.Do(req)
}
