// SPDX-License-Identifier: Apache-2.0
//
// pg_info.go — reads /run/pandastack/ready.json from a postgres-16 sandbox
// and returns it as JSON. Used by the API databases layer to surface
// connection credentials without requiring the caller to exec manually.
//
// Route: GET /sandboxes/{id}/postgres-info

package api

import (
	"encoding/json"
	"net/http"

	"github.com/pandastack/agent/internal/sandbox"
)

// PostgresInfo mirrors the structure written by the postgres-16 template's
// autostart.sh into /run/pandastack/ready.json.
type PostgresInfo struct {
	Host          string `json:"pg_host"`
	Port          int    `json:"pg_port"`
	Database      string `json:"default_database"`
	Username      string `json:"pg_user"`
	Password      string `json:"pg_password"`
	BrokerToken   string `json:"broker_token"`
	BrokerURL     string `json:"broker_url"`
	ReadyAt       string `json:"started_at"`
}

func registerPGInfo(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /sandboxes/{id}/postgres-info", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		sb, err := mgr.GetTyped(r.Context(), id)
		if err != nil || sb == nil {
			writeErr(w, http.StatusNotFound, errString("sandbox not found"))
			return
		}
		if sb.Template != pgTunnelTemplate {
			writeErr(w, http.StatusForbidden, errString("sandbox is not a postgres-16 instance"))
			return
		}
		if sb.Status != sandbox.StatusRunning {
			writeErr(w, http.StatusServiceUnavailable, errString("sandbox not running"))
			return
		}

		gc, err := mgr.Guest(id)
		if err != nil {
			writeErr(w, http.StatusBadGateway, errString("guest connection unavailable"))
			return
		}

		ctx := r.Context()
		const cmd = "cat /run/pandastack/ready.json 2>/dev/null"
		res, err := gc.Exec(ctx, cmd)
		if err != nil {
			writeErr(w, http.StatusBadGateway, errString("exec failed: "+err.Error()))
			return
		}
		if res.ExitCode != 0 {
			writeErr(w, http.StatusServiceUnavailable, errString("postgres not ready yet (ready.json missing)"))
			return
		}

		var info PostgresInfo
		if err := json.Unmarshal([]byte(res.Stdout), &info); err != nil {
			writeErr(w, http.StatusInternalServerError, errString("invalid ready.json: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, info)
	})
}
