// SPDX-License-Identifier: Apache-2.0
package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStore_SandboxRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sb := map[string]any{
		"id":         "s1",
		"template":   "ubuntu-24.04",
		"cpu":        2,
		"memory_mb":  1024,
		"status":     "creating",
		"guest_ip":   "172.20.0.5",
		"host_tap":   "fc-tap0",
		"mac":        "AA:BB:CC:DD:EE:01",
		"vsock_cid":  100,
		"boot_ms":    1234,
		"boot_mode":  "cold",
		"created_at": time.Now().Format(time.RFC3339Nano),
		"metadata":   map[string]string{"workspace": "alpha"},
	}
	if err := s.InsertSandbox(ctx, sb); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.GetSandbox(ctx, "s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	row := got.(map[string]any)
	if row["template"] != "ubuntu-24.04" || row["status"] != "creating" {
		t.Fatalf("round-trip mismatch: %#v", row)
	}
	if int(row["boot_ms"].(int64)) != 1234 {
		t.Fatalf("boot_ms not persisted: %v", row["boot_ms"])
	}
	if row["boot_mode"] != "cold" {
		t.Fatalf("boot_mode not persisted: %v", row["boot_mode"])
	}

	// Update + relist.
	sb["status"] = "running"
	sb["boot_ms"] = 1900
	if err := s.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("Update: %v", err)
	}
	list, err := s.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].(map[string]any)["status"] != "running" {
		t.Fatalf("list/update mismatch: %#v", list)
	}
	if err := s.DeleteSandbox(ctx, "s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if list, _ := s.ListSandboxes(ctx); len(list) != 0 {
		t.Fatalf("post-delete list not empty: %#v", list)
	}
}

func TestStore_SandboxLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sb := map[string]any{
		"id": "lc1", "template": "base", "cpu": 1, "memory_mb": 512,
		"status": "running", "created_at": time.Now().Format(time.RFC3339Nano),
	}
	if err := s.InsertSandbox(ctx, sb); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Defaults before any lifecycle write: not persistent, ttl 0.
	persistent, ttl, found, err := s.GetSandboxLifecycle(ctx, "lc1")
	if err != nil || !found {
		t.Fatalf("GetSandboxLifecycle default: found=%v err=%v", found, err)
	}
	if persistent || ttl != 0 {
		t.Fatalf("expected defaults (false,0); got (%v,%d)", persistent, ttl)
	}

	// Persist persistent=true + ttl, then read back (simulates rehydrate after restart).
	if rows, err := s.SetSandboxLifecycle(ctx, "lc1", true, 3600); err != nil || rows != 1 {
		t.Fatalf("SetSandboxLifecycle: rows=%d err=%v", rows, err)
	}
	// The race the retry wrapper guards against: an UPDATE for a row that
	// doesn't exist yet must report 0 rows, not an error.
	if rows, err := s.SetSandboxLifecycle(ctx, "does-not-exist", true, 60); err != nil || rows != 0 {
		t.Fatalf("missing-row UPDATE must be (0,nil); got (%d,%v)", rows, err)
	}
	persistent, ttl, found, err = s.GetSandboxLifecycle(ctx, "lc1")
	if err != nil || !found {
		t.Fatalf("GetSandboxLifecycle: found=%v err=%v", found, err)
	}
	if !persistent || ttl != 3600 {
		t.Fatalf("expected (true,3600); got (%v,%d)", persistent, ttl)
	}

	// Unknown id reports not found rather than erroring.
	if _, _, found, err := s.GetSandboxLifecycle(ctx, "nope"); err != nil || found {
		t.Fatalf("expected not-found for unknown id; found=%v err=%v", found, err)
	}
}

func TestStore_BootEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.InsertBootEvent(ctx, BootEvent{
			SandboxID: "s",
			Template:  "ubuntu",
			BootMode:  "cold",
			BootMS:    int64(1000 + i*100),
			TS:        time.Now().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.ListBootEvents(ctx, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5", len(events))
	}
	if events[0].BootMS != 1400 {
		t.Fatalf("expected newest-first ordering; got first=%d", events[0].BootMS)
	}
}

func TestStore_Audit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 3; i++ {
		_ = s.InsertAudit(ctx, AuditEntry{
			TS:        now.Add(time.Duration(i) * time.Second),
			Workspace: "team-a",
			Method:    "POST",
			Path:      "/v1/sandboxes",
			Status:    201,
			RequestID: "req-" + string(rune('a'+i)),
		})
	}
	entries, err := s.ListAudit(ctx, now.Add(-time.Minute), "team-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("audit: got %d, want 3", len(entries))
	}

}

// ListSandboxesForAgent is the query-side of the cross-agent ownership fix: on
// the shared multi-node Postgres it must return ONLY this agent's rows plus
// legacy unclaimed rows (agent_id=”), never a peer's rows — so no agent
// full-scans every tenant's rows on its housekeeping timers. It must be a strict
// superset of what ownsRow admits (mine + legacy) so callers see identical
// results to the old fleet-wide-list + ownsRow filter.
func TestStore_ListSandboxesForAgent_Scoping(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// InsertSandbox stamps s.agentID onto each row, so flip identity between
	// inserts to fabricate rows owned by different agents.
	mk := func(id, owner string) {
		s.SetAgentID(owner)
		sb := map[string]any{
			"id": id, "template": "base", "cpu": 1, "memory_mb": 512,
			"status": "running", "created_at": time.Now().Format(time.RFC3339Nano),
		}
		if err := s.InsertSandbox(ctx, sb); err != nil {
			t.Fatalf("Insert %s: %v", id, err)
		}
	}
	mk("mine-1", "agent-A")
	mk("mine-2", "agent-A")
	mk("peer-1", "agent-B")
	mk("peer-2", "agent-B")
	mk("legacy-1", "") // pre-ownership-column row

	ids := func(rows []any) map[string]bool {
		out := map[string]bool{}
		for _, r := range rows {
			out[r.(map[string]any)["id"].(string)] = true
		}
		return out
	}

	// agent-A sees its two rows + the legacy row, and NEVER agent-B's rows.
	got, err := s.ListSandboxesForAgent(ctx, "agent-A")
	if err != nil {
		t.Fatalf("ListSandboxesForAgent(A): %v", err)
	}
	set := ids(got)
	if len(got) != 3 || !set["mine-1"] || !set["mine-2"] || !set["legacy-1"] {
		t.Fatalf("agent-A scope wrong: %v", set)
	}
	if set["peer-1"] || set["peer-2"] {
		t.Fatalf("agent-A leaked a peer's rows: %v", set)
	}

	// agent-B sees its own two + the legacy row, none of agent-A's.
	got, _ = s.ListSandboxesForAgent(ctx, "agent-B")
	set = ids(got)
	if len(got) != 3 || !set["peer-1"] || !set["peer-2"] || !set["legacy-1"] || set["mine-1"] {
		t.Fatalf("agent-B scope wrong: %v", set)
	}

	// A never-seen agent still gets the legacy row (so Recover can claim it).
	got, _ = s.ListSandboxesForAgent(ctx, "agent-C")
	set = ids(got)
	if len(got) != 1 || !set["legacy-1"] {
		t.Fatalf("agent-C should see only the legacy row: %v", set)
	}

	// Fleet-wide List still returns everything (public API + fork-tree path).
	all, _ := s.ListSandboxes(ctx)
	if len(all) != 5 {
		t.Fatalf("fleet-wide ListSandboxes should see all 5, got %d", len(all))
	}
}

// SetSandboxNetwork is used by the wake netns-recovery path: when a hibernated
// NATID sandbox's netns was torn down across an agent restart and we re-allocate
// a fresh one, the proxy dial target (guest_ip) + netns name (host_tap) + mac
// change and must be persisted so the pg-tunnel/SSH paths reach the new netns.
// It must update ONLY those three columns and leave everything else intact.
func TestStore_SetSandboxNetwork_ScopedUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sb := map[string]any{
		"id":         "nrec1",
		"template":   "postgres-16",
		"cpu":        2,
		"memory_mb":  1024,
		"status":     "hibernated",
		"guest_ip":   "10.200.0.2",
		"host_tap":   "ns-p0000008f",
		"mac":        "AA:BB:CC:DD:EE:01",
		"vsock_cid":  0,
		"boot_ms":    42,
		"boot_mode":  "snapshot-natid",
		"created_at": time.Now().Format(time.RFC3339Nano),
		"metadata":   map[string]string{"workspace": "alpha"},
	}
	if err := s.InsertSandbox(ctx, sb); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Simulate a netns re-allocation handing back a NEW proxy IP + netns name.
	if err := s.SetSandboxNetwork(ctx, "nrec1", "10.200.7.42", "ns-p000000a6", "AA:BB:CC:DD:EE:99"); err != nil {
		t.Fatalf("SetSandboxNetwork: %v", err)
	}

	got, err := s.GetSandbox(ctx, "nrec1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	row := got.(map[string]any)
	// The three network columns must be updated...
	if row["guest_ip"] != "10.200.7.42" {
		t.Fatalf("guest_ip not updated: %v", row["guest_ip"])
	}
	if row["host_tap"] != "ns-p000000a6" {
		t.Fatalf("host_tap not updated: %v", row["host_tap"])
	}
	if row["mac"] != "AA:BB:CC:DD:EE:99" {
		t.Fatalf("mac not updated: %v", row["mac"])
	}
	// ...and nothing else may be clobbered.
	if row["template"] != "postgres-16" {
		t.Fatalf("template clobbered: %v", row["template"])
	}
	if row["status"] != "hibernated" {
		t.Fatalf("status clobbered: %v", row["status"])
	}
	if int(row["boot_ms"].(int64)) != 42 {
		t.Fatalf("boot_ms clobbered: %v", row["boot_ms"])
	}
}
