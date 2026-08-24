// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// healthzResponse is the public health payload. It intentionally exposes ONLY a
// coarse status — never the names of internal dependencies or their env vars. Leaking which backends the service runs, and under what
// configuration keys, is information disclosure that helps an attacker map the
// system; the readiness signal a load balancer needs is just ok / unhealthy.
type healthzResponse struct {
	Status string `json:"status"`
}

// healthzStatus computes liveness/readiness. The DB DSN is the only hard gate:
// without it the service can't serve, so it reports 503/unhealthy and an
// orchestrator will pull it out of rotation. The dependency is never named in
// the returned payload.
func healthzStatus() (int, healthzResponse) {
	if strings.TrimSpace(getenv("PANDASTACK_DB_DSN")) == "" {
		return http.StatusServiceUnavailable, healthzResponse{Status: "unhealthy"}
	}
	return http.StatusOK, healthzResponse{Status: "ok"}
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	code, resp := healthzStatus()
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
