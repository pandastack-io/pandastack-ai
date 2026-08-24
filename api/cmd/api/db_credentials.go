// SPDX-License-Identifier: Apache-2.0
//
// db_credentials.go — password reset for a managed database.
//
// Rotating a database's password is a baseline operation, not a recovery one:
// it re-runs the agent's credential-injection phase against a RUNNING database
// and returns the new connection string. The agent half is
// agent/internal/sandbox/db_rotate.go (POST /db/{id}/rotate-creds).
package main

import (
	"io"
	"net/http"
	"strings"
)

// resetCredentials rotates the postgres password AND broker token of a
// RUNNING database. Synchronous: a 200 carries the new, verified credentials
// (the response is the only time callers should need them — GET serves the
// same values afterwards). Any error means the rotation did not complete;
// retrying is safe.
func (d *databasesAPI) resetCredentials(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	if workspace == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}
	if d.director == nil {
		writeErrOrg(w, http.StatusNotImplemented, "credential reset requires a multi-node deployment")
		return
	}
	id := r.PathValue("id")

	status, ok := d.verifyDB(w, r, workspace, id)
	if !ok {
		return
	}
	if status != "running" {
		writeErrOrg(w, http.StatusConflict,
			"credentials can only be reset while the database is running (status: "+status+")")
		return
	}

	// Serialize per id: two overlapping rotations interleaving their guest
	// steps could return a 200 whose credentials the other rotation already
	// invalidated. Reuses the failover in-flight map (same lifecycle).
	d.failoverMu.Lock()
	if d.failoverInFlight["rotate:"+id] {
		d.failoverMu.Unlock()
		writeErrOrg(w, http.StatusConflict, "a credential rotation for this database is already in progress")
		return
	}
	d.failoverInFlight["rotate:"+id] = true
	d.failoverMu.Unlock()
	defer func() {
		d.failoverMu.Lock()
		delete(d.failoverInFlight, "rotate:"+id)
		d.failoverMu.Unlock()
	}()

	owner, err := d.director.sched.LookupLease(r.Context(), id)
	if err != nil || owner == nil || owner.Endpoint == "" {
		writeErrOrg(w, http.StatusServiceUnavailable, "could not resolve the database's host agent — retry shortly")
		return
	}

	resp, err := d.directAgentCall(r.Context(), http.MethodPost,
		strings.TrimRight(owner.Endpoint, "/")+"/db/"+id+"/rotate-creds", []byte("{}"))
	if err != nil {
		writeErrOrg(w, http.StatusBadGateway, "credential rotation failed to reach the host agent: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeErrOrg(w, http.StatusBadGateway,
			"credential rotation incomplete — safe to retry ("+strings.TrimSpace(string(b))+")")
		return
	}

	// Serve the NEW credentials from the guest's freshly rewritten ready.json.
	info, err := d.fetchPGInfo(r, workspace, id)
	if err != nil || info == nil {
		// Rotation succeeded but the read-back raced; the next GET serves it.
		writeJSON(w, http.StatusOK, map[string]string{
			"id":     id,
			"status": "rotated",
			"detail": "credentials rotated; fetch the new values via GET /v1/databases/{id}",
		})
		return
	}
	result := DatabaseInfo{ID: id, Status: "running", SandboxID: id}
	writeJSON(w, http.StatusOK, mergeInfo(result, info, id))
	d.log.Info("databases: credentials rotated", "id", id)
}
