// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pandastack/agent/internal/config"
)

// mgrWithID builds a bare Manager with a known agentID and DataDir for
// ownership tests. No store/network needed — ownsRow only reads the row map
// plus local-evidence stat checks under DataDir.
func mgrWithID(t *testing.T, agentID string) *Manager {
	t.Helper()
	return &Manager{agentID: agentID, cfg: config.Config{DataDir: t.TempDir()}}
}

func TestOwnsRow(t *testing.T) {
	m := mgrWithID(t, "agent-A")

	// Owned by us → true.
	if !m.ownsRow(map[string]any{"id": "s1", "agent_id": "agent-A"}) {
		t.Error("row stamped agent-A should be owned by agent-A")
	}
	// Owned by a peer → false (THE incident: agent-A must not judge agent-B's row).
	if m.ownsRow(map[string]any{"id": "s2", "agent_id": "agent-B"}) {
		t.Error("row stamped agent-B must NOT be owned by agent-A")
	}
	// Legacy row (empty agent_id) with NO local evidence → false (don't touch).
	if m.ownsRow(map[string]any{"id": "s3", "agent_id": ""}) {
		t.Error("legacy row with no local evidence must NOT be owned")
	}
	// Empty id → false.
	if m.ownsRow(map[string]any{"id": "", "agent_id": "agent-A"}) {
		t.Error("empty id must not be owned")
	}
}

func TestOwnsRow_LegacyWithLocalEvidence(t *testing.T) {
	m := mgrWithID(t, "agent-A")
	// Create a vm dir → local evidence for a legacy (unstamped) row.
	vmDir := filepath.Join(m.cfg.DataDir, "vms", "legacy-1")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !m.ownsRow(map[string]any{"id": "legacy-1", "agent_id": ""}) {
		t.Error("legacy row WITH a local vm dir should be adoptable (owned)")
	}
	// A different legacy id with no dir stays unowned.
	if m.ownsRow(map[string]any{"id": "legacy-2", "agent_id": ""}) {
		t.Error("legacy row without local evidence must stay unowned")
	}
}

// TestOwnsRow_EmptyAgentIDNeverAdoptsPeerRow: even if our own agentID is empty
// (misconfig), we must never claim a row explicitly owned by a named peer.
func TestOwnsRow_PeerRowNeverAdopted(t *testing.T) {
	m := mgrWithID(t, "") // our id somehow unset
	if m.ownsRow(map[string]any{"id": "s", "agent_id": "agent-B"}) {
		t.Error("a row owned by a named peer must never be adopted, even when our id is empty")
	}
}

// TestJanitorGuards encodes the exact 2026-07-12 incident invariants at the
// predicate level the sweep uses to decide whether to delete a failed row:
//
//	(1) a PEER-owned failed row is not ours to reclaim (ownsRow=false), and
//	(2) a managed row (DB or app) is never reclaimed (managedKind != "").
//
// Either guard failing would re-open the cross-agent deletion.
func TestJanitorGuards(t *testing.T) {
	m := mgrWithID(t, "agent-A")

	// (1) The incident row: vkh4's healthy DB, seen by bddt. bddt (=agent-A
	// here) must NOT own it → the sweep's `if !ownsRow { continue }` protects it.
	peerDB := map[string]any{"id": "vkh4-db", "agent_id": "agent-B", "template": "postgres-16", "status": "failed"}
	if m.ownsRow(peerDB) {
		t.Error("a peer's DB row must NOT be owned (this was the incident)")
	}

	// (2) Even a row we DO own, if it's managed, is skipped by the janitor.
	ownedDB := map[string]any{"id": "my-db", "agent_id": "agent-A", "template": "postgres-16", "status": "failed"}
	if managedKind(ownedDB) == "" {
		t.Error("a postgres-16 row must be recognized as managed (janitor-protected)")
	}
	// Future/custom DB template protection relies on managedKind, not a literal.
	// An app (metadata.kind=app) is likewise managed.
	app := map[string]any{"id": "my-app", "agent_id": "agent-A", "status": "failed",
		"metadata": map[string]string{"kind": "app"}}
	if managedKind(app) == "" {
		t.Error("an app row (metadata.kind=app) must be recognized as managed")
	}
	// A plain sandbox we own is NOT managed → janitor may reclaim it (unchanged).
	plain := map[string]any{"id": "s", "agent_id": "agent-A", "template": "base", "status": "failed"}
	if managedKind(plain) != "" {
		t.Error("a plain base sandbox must not be treated as managed")
	}
}
