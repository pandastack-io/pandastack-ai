// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestHealthzStatusOK(t *testing.T) {
	t.Setenv("PANDASTACK_DB_DSN", "postgres://example")

	code, resp := healthzStatus()
	if code != http.StatusOK || resp.Status != "ok" {
		t.Fatalf("expected ok/200, got %s/%d", resp.Status, code)
	}
}
func TestHealthzResponseHidesInternals(t *testing.T) {
	t.Setenv("PANDASTACK_DB_DSN", "postgres://example")

	_, resp := healthzStatus()
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal healthz response: %v", err)
	}
	got := string(body)
	if got != `{"status":"ok"}` {
		t.Fatalf("healthz payload must be status-only, got %s", got)
	}
	for _, leak := range []string{"DB", "DSN", "checks", "PANDASTACK"} {
		if strings.Contains(got, leak) {
			t.Fatalf("healthz payload leaks internal detail %q: %s", leak, got)
		}
	}
}

func TestHealthzStatusUnhealthyWhenDBMissing(t *testing.T) {

	code, resp := healthzStatus()
	if code != http.StatusServiceUnavailable || resp.Status != "unhealthy" {
		t.Fatalf("expected unhealthy/503, got %s/%d", resp.Status, code)
	}
}
