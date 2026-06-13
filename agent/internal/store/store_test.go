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
	if err := s.SetSandboxLifecycle(ctx, "lc1", true, 3600); err != nil {
		t.Fatalf("SetSandboxLifecycle: %v", err)
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
	events, err := s.ListBootEvents(ctx, "", 0)
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
