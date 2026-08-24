// SPDX-License-Identifier: Apache-2.0
package store

import (
	"context"
	"testing"
	"time"
)

// TestUpdateSandbox_PreservesCreatedAt pins the fix for a production billing
// bug: UpdateSandbox used to rewrite created_at from the caller's struct, so a
// caller that rebuilt a *Sandbox without populating CreatedAt (GetTyped did
// exactly that) stamped the Go zero time onto a live row. The meter treats a
// zero-time row as having no billing anchor, so the sandbox stopped billing
// permanently. created_at is immutable — no update path may write it.
func TestUpdateSandbox_PreservesCreatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	if err := s.InsertSandbox(ctx, map[string]any{
		"id":         "sb-immutable",
		"template":   "postgres-16-4g",
		"cpu":        8,
		"memory_mb":  4096,
		"status":     "running",
		"created_at": created.Format(time.RFC3339Nano),
		"metadata":   map[string]string{"workspace": "acme"},
	}); err != nil {
		t.Fatalf("InsertSandbox: %v", err)
	}

	// The exact shape that corrupted prod: a struct with no created_at at all,
	// as produced by rebuilding a row and patching one field.
	if err := s.UpdateSandbox(ctx, map[string]any{
		"id":        "sb-immutable",
		"template":  "postgres-16-4g",
		"cpu":       8,
		"memory_mb": 4096,
		"status":    "paused",
		"metadata":  map[string]string{"workspace": "acme", "db.always_on": "true"},
	}); err != nil {
		t.Fatalf("UpdateSandbox: %v", err)
	}

	row, err := s.GetSandbox(ctx, "sb-immutable")
	if err != nil || row == nil {
		t.Fatalf("GetSandbox: %v (row=%v)", err, row)
	}
	m := row.(map[string]any)

	got, _ := m["created_at"].(time.Time)
	if got.IsZero() {
		t.Fatal("created_at was zeroed by UpdateSandbox — the billing anchor is destroyed")
	}
	if !got.Equal(created) {
		t.Fatalf("created_at changed: got %v, want %v", got, created)
	}
	// The mutable columns must still have been written.
	if m["status"] != "paused" {
		t.Fatalf("status not updated: %v", m["status"])
	}
	md, _ := m["metadata"].(map[string]string)
	if md["db.always_on"] != "true" {
		t.Fatalf("metadata not updated: %v", md)
	}
}
