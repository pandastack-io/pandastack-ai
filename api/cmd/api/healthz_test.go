// SPDX-License-Identifier: Apache-2.0
package main

import (
	"net/http"
	"testing"
)

func TestHealthzStatusOK(t *testing.T) {
	t.Setenv("PANDASTACK_DB_DSN", "postgres://example")

	code, resp := healthzStatus()
	if code != http.StatusOK || resp.Status != "ok" {
		t.Fatalf("expected ok/200, got %s/%d", resp.Status, code)
	}
}

func TestHealthzStatusUnhealthyWhenDBMissing(t *testing.T) {
	t.Setenv("PANDASTACK_DB_DSN", "")

	code, resp := healthzStatus()
	if code != http.StatusServiceUnavailable || resp.Status != "unhealthy" {
		t.Fatalf("expected unhealthy/503, got %s/%d", resp.Status, code)
	}
	if resp.Checks["PANDASTACK_DB_DSN"] != "missing" {
		t.Fatalf("expected missing db check, got %q", resp.Checks["PANDASTACK_DB_DSN"])
	}
}
