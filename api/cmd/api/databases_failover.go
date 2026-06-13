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

	// Ownership + template check straight from the shared sandboxes table.
	// verifyDB/agentCall would route the lookup to the OWNING agent — which
	// is presumed dead; that's the whole reason failover is being invoked.
	meta, ok := d.failoverAuthorize(w, r, workspace, id)
	if !ok {
		return
	}

	// Identify the current (presumed-dead) owner so we can exclude it from
	// target selection and best-effort kill any half-alive VM (split-brain
	// guard: two postgres instances both archiving WAL under one id).
	current, err := d.director.sched.LookupLease(r.Context(), id)
	if err != nil {
		d.log.Warn("databases: failover lease lookup failed (continuing)", "id", id, "err", err)
	}

	target := d.pickFailoverTarget(r.Context(), current)
	if target == nil {
		writeErrOrg(w, http.StatusServiceUnavailable, "no healthy agent available to fail over to")
		return
	}

	if current != nil {
		d.drainOldAgent(current, id)
	}

	if !d.restoreOnAgent(w, target, id, meta) {
		return
	}

	// Bust this edge's lease cache (persistent sandboxes are cached 1h) so
	// the readiness poll below — and every subsequent request through this
	// edge — routes to the new owner immediately. The PG lease row itself was
	// already retargeted by the upsert inside the agent's Create.
	d.director.sched.RememberLeasePersistent(id, *target)

	d.log.Info("databases: failover restore accepted",
		"id", id, "target_agent", target.ID, "endpoint", target.Endpoint)

	// Wait for postgres to replay WAL and publish fresh credentials, then
	// return the same shape as create: full connection info on success,
	// status "provisioning" + error hint if it is still recovering.
	result := DatabaseInfo{
		ID:       id,
		Status:   "running",
		Template: dbTemplate,
		Label:    meta["db.label"],
	}
	if info := d.waitPGReady(r, workspace, id); info != nil {
		result = mergeInfo(result, info, id)
	} else {
		result.Status = "provisioning"
		result.Error = "database restored on a new agent but postgres is still recovering; poll GET /v1/databases/{id}"
	}
	writeJSON(w, http.StatusOK, result)
}

// failoverAuthorize verifies the sandbox row exists, is a managed database,
// and belongs to the caller's workspace. Returns the row's metadata (which
// carries workspace + db.label and is re-applied to the restored sandbox).
// Writes the error response itself when returning ok=false.
func (d *databasesAPI) failoverAuthorize(w http.ResponseWriter, r *http.Request, workspace, id string) (map[string]string, bool) {
	var template string
	var metaRaw sql.NullString
	err := d.db.QueryRowContext(r.Context(),
		`SELECT template, metadata FROM sandboxes WHERE id = $1`, id).
		Scan(&template, &metaRaw)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return nil, false
	}
	if err != nil {
		d.log.Error("databases: failover sandbox lookup failed", "id", id, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "sandbox lookup failed")
		return nil, false
	}
	if template != dbTemplate {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return nil, false
	}
	meta := map[string]string{}
	if metaRaw.Valid && metaRaw.String != "" {
		_ = json.Unmarshal([]byte(metaRaw.String), &meta)
	}
	// Same tenancy rule as the agent's workspaceScope: admin/default see
	// everything, everyone else only their own rows (no empty-owner leak).
	if workspace != "admin" && workspace != "default" && meta["workspace"] != workspace {
		writeErrOrg(w, http.StatusNotFound, "database not found")
		return nil, false
	}
	if meta["workspace"] == "" {
		meta["workspace"] = workspace
	}
	return meta, true
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

// drainOldAgent best-effort deletes the sandbox on the old owner. Errors are
// logged and ignored — the old host is normally unreachable, that is why we
// are here. If it IS half-alive, this prevents two postgres instances from
// both archiving WAL under the same id.
func (d *databasesAPI) drainOldAgent(current *scheduler.Agent, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), dbFailoverDrainTimeout)
	defer cancel()
	resp, err := d.directAgentCall(ctx, http.MethodDelete,
		strings.TrimRight(current.Endpoint, "/")+"/sandboxes/"+id, nil)
	if err != nil {
		d.log.Info("databases: failover old-agent drain failed (expected if host is dead)",
			"id", id, "agent", current.ID, "err", err)
		return
	}
	defer resp.Body.Close()
	d.log.Info("databases: failover drained old agent", "id", id, "agent", current.ID, "status", resp.StatusCode)
}

// restoreOnAgent invokes POST /db/{id}/restore on the target agent and
// writes the error response itself on failure (returning false).
func (d *databasesAPI) restoreOnAgent(w http.ResponseWriter, target *scheduler.Agent, id string, meta map[string]string) bool {
	body, _ := json.Marshal(map[string]any{"metadata": meta})
	// Background-derived context: a client that gives up mid-restore must not
	// abort the multi-minute volume rebuild on the agent.
	ctx, cancel := context.WithTimeout(context.Background(), dbFailoverRestoreTimeout)
	defer cancel()
	resp, err := d.directAgentCall(ctx, http.MethodPost,
		strings.TrimRight(target.Endpoint, "/")+"/db/"+id+"/restore", body)
	if err != nil {
		d.log.Error("databases: failover restore call failed", "id", id, "agent", target.ID, "err", err)
		writeErrOrg(w, http.StatusBadGateway, "restore on target agent failed: "+err.Error())
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		d.log.Error("databases: failover restore rejected",
			"id", id, "agent", target.ID, "status", resp.StatusCode, "body", string(b))
		writeErrOrg(w, http.StatusBadGateway, "restore on target agent failed: "+strings.TrimSpace(string(b)))
		return false
	}
	return true
}

// waitPGReady polls postgres-info (now lease-routed to the NEW agent) until
// credentials appear or the deadline passes. nil on timeout.
func (d *databasesAPI) waitPGReady(r *http.Request, workspace, id string) *pgInfoResponse {
	deadline := time.Now().Add(dbFailoverReadyTimeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if info, err := d.fetchPGInfo(r, workspace, id); err == nil && info != nil {
			return info
		}
		if time.Now().After(deadline) {
			d.log.Warn("databases: postgres not ready after failover (still recovering?)", "id", id)
			return nil
		}
		select {
		case <-r.Context().Done():
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
