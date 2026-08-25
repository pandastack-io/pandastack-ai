// SPDX-License-Identifier: Apache-2.0
package main

import (
	"testing"
	"time"

	"github.com/pandastack/api/internal/scheduler"
)

// TestAgentHealthy pins the failover guard's health rule: only an active
// agent with an endpoint and a fresh (<30s) heartbeat counts as healthy —
// the condition under which failover must REFUSE to touch a running primary.
func TestAgentHealthy(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		agent *scheduler.Agent
		want  bool
	}{
		{"nil agent", nil, false},
		{"healthy", &scheduler.Agent{Status: "active", Endpoint: "http://a:8081", LastHeartbeat: now}, true},
		{"stale heartbeat", &scheduler.Agent{Status: "active", Endpoint: "http://a:8081", LastHeartbeat: now.Add(-31 * time.Second)}, false},
		{"inactive", &scheduler.Agent{Status: "draining", Endpoint: "http://a:8081", LastHeartbeat: now}, false},
		{"no endpoint", &scheduler.Agent{Status: "active", LastHeartbeat: now}, false},
		{"boundary 29s ok", &scheduler.Agent{Status: "active", Endpoint: "http://a:8081", LastHeartbeat: now.Add(-29 * time.Second)}, true},
	}
	for _, c := range cases {
		if got := agentHealthy(c.agent); got != c.want {
			t.Errorf("%s: agentHealthy = %v, want %v", c.name, got, c.want)
		}
	}
}
