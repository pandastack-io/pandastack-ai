// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

// Liveness vs readiness.
//
// /healthz is LIVENESS: "this process is up and configured". It must keep
// returning 200 during a control-plane database outage, because reads still
// serve fine from the proxy path and pulling every API instance out of the load
// balancer would turn a partial outage into a total one.
//
// /readyz is READINESS: "the platform can actually do its job right now". It
// pings the control-plane database and confirms at least one compute node is
// advertising a fresh heartbeat. During the 2026-08-19 CloudSQL maintenance the
// API answered /healthz 200 while every POST /v1/sandboxes returned "no
// available compute node" — the dashboard's status pill, and any uptime monitor
// pointed at /healthz, both reported green through a total create outage. This
// endpoint is what those should watch.
//
// The payload names only coarse product-level subsystems ("database",
// "compute") — never hostnames, DSNs, or env vars — keeping the information
// disclosure posture of /healthz.
type readyzResponse struct {
	Status   string   `json:"status"`             // "ok" | "degraded"
	Degraded []string `json:"degraded,omitempty"` // coarse subsystem names
}

type readinessDeps struct {
	db     *sql.DB
	agents func(context.Context) (int, error) // fresh compute nodes, nil when single-node
}

func readyzHandler(deps readinessDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		var degraded []string
		if deps.db != nil {
			if err := deps.db.PingContext(ctx); err != nil {
				degraded = append(degraded, "database")
			}
		}
		if deps.agents != nil {
			// Only report compute degraded when we can positively determine
			// there are zero fresh nodes. A query error is already covered by
			// the database check and must not double-count.
			if n, err := deps.agents(ctx); err == nil && n == 0 {
				degraded = append(degraded, "compute")
			}
		}

		resp := readyzResponse{Status: "ok"}
		code := http.StatusOK
		if len(degraded) > 0 {
			resp = readyzResponse{Status: "degraded", Degraded: degraded}
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("content-type", "application/json")
		w.Header().Set("cache-control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
