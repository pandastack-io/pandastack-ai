// SPDX-License-Identifier: Apache-2.0
// NATID claim registry: each agent VM claims a unique integer NATID on boot.
// The NATID defines a non-overlapping IP namespace for Firecracker VMs on that
// agent. NATIDs 1-24 are supported (matching PANDASTACK_NATID_POOL_SIZE=24).
//
// The DB table is the source of truth. Agents claim via psql in their startup
// script (avoids chicken-and-egg: agent needs NATID before it can call the API).
// The API exposes a read-only list endpoint for observability + admin tooling.
//
// Heartbeat: each agent runs a systemd timer (pandastack-natid-heartbeat.timer)
// every 2 minutes to update last_heartbeat. Claims idle > 30 min are evicted on
// the next NATID claim from any booting agent.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const natidSchema = `
CREATE TABLE IF NOT EXISTS agent_natid_claims (
    natid          INT PRIMARY KEY,
    instance_id    TEXT NOT NULL,
    region         TEXT NOT NULL DEFAULT '',
    claimed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS agent_natid_claims_instance_idx
    ON agent_natid_claims (instance_id);
`

func initNatidSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, natidSchema)
	return err
}

type natidClaim struct {
	NATID         int       `json:"natid"`
	InstanceID    string    `json:"instance_id"`
	Region        string    `json:"region"`
	ClaimedAt     time.Time `json:"claimed_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// registerNatidRoutes mounts the NATID observability endpoint.
// Protected by X-Node-Token so only agent/edge nodes can call it.
func registerNatidRoutes(mux *http.ServeMux, db *sql.DB, log *slog.Logger) {
	nodeToken := strings.TrimSpace(os.Getenv("PANDASTACK_NODE_TOKEN"))

	requireNodeToken := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if nodeToken != "" && r.Header.Get("X-Node-Token") != nodeToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}

	// GET /v1/internal/natid/list — returns current NATID claim table.
	// Useful for dashboards, runbooks, and verifying no two agents share a NATID.
	mux.HandleFunc("GET /v1/internal/natid/list", requireNodeToken(func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(),
			`SELECT natid, instance_id, region, claimed_at, last_heartbeat
             FROM agent_natid_claims ORDER BY natid`)
		if err != nil {
			log.Error("natid list query failed", "err", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		claims := make([]natidClaim, 0)
		for rows.Next() {
			var c natidClaim
			if err := rows.Scan(&c.NATID, &c.InstanceID, &c.Region,
				&c.ClaimedAt, &c.LastHeartbeat); err != nil {
				http.Error(w, "scan error", http.StatusInternalServerError)
				return
			}
			claims = append(claims, c)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "rows error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(claims)
	}))
}
