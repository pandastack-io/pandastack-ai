// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"strings"
)

// workloadClass buckets a sandbox by what it is running. The pressure ladder
// uses it as a scope guard: a managed database is never frozen to reclaim
// memory (freezing Postgres under load is indistinguishable from an outage),
// and neither is a long-lived served workload.
//
// Classification is deliberately structural — template name plus the metadata
// the control plane stamps at create time — so it works without any lookup.
func workloadClass(template string, md map[string]string) string {
	if md != nil && (md["kind"] == "app" || md["app.id"] != "") {
		return "app"
	}
	if strings.HasPrefix(template, "app-") {
		return "app"
	}
	if strings.HasPrefix(template, "postgres-16") {
		return "db"
	}
	return "sandbox"
}

// sandboxWorkspaces builds sandbox-id → workspace from this agent's live rows.
// Callers use it to attribute an event to a tenant when they only hold an id.
// Scoped to this agent (ListSandboxesForAgent) so a node never scans the whole
// fleet's rows on a housekeeping timer.
func (m *Manager) sandboxWorkspaces(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := m.store.ListSandboxesForAgent(ctx, m.agentID)
	if err != nil {
		return out
	}
	for _, row := range rows {
		rmap, _ := row.(map[string]any)
		if rmap == nil {
			continue
		}
		id, _ := rmap["id"].(string)
		if id == "" {
			continue
		}
		if md, ok := rmap["metadata"].(map[string]string); ok {
			if ws := md["workspace"]; ws != "" {
				out[id] = ws
			}
		}
	}
	return out
}
