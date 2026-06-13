// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

type healthzResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func healthzStatus() (int, healthzResponse) {
	checks := map[string]string{
		"PANDASTACK_DB_DSN": "ok",
	}
	status := "ok"
	code := http.StatusOK

	if strings.TrimSpace(getenv("PANDASTACK_DB_DSN")) == "" {
		checks["PANDASTACK_DB_DSN"] = "missing"
		status = "unhealthy"
		code = http.StatusServiceUnavailable
	}

	return code, healthzResponse{Status: status, Checks: checks}
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	code, resp := healthzStatus()
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
