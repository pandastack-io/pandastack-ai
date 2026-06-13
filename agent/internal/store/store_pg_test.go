// SPDX-License-Identifier: Apache-2.0
package store

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPostgresSandboxRoundTrip(t *testing.T) {
	loadDotEnvLocal(t)
	dsn := os.Getenv("DATABASE_DIRECT_URL")
	if dsn == "" {
		dsn = os.Getenv("PANDASTACK_DB_DSN")
	}
	if dsn == "" {
		t.Skip("DATABASE_DIRECT_URL is unset")
	}
	t.Setenv("PANDASTACK_DB_DRIVER", "postgres")
	t.Setenv("PANDASTACK_DB_DSN", dsn)

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	id := "pg-test-" + strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339Nano), ":", "-")
	sb := map[string]any{
		"id": id, "template": "postgres-test", "cpu": 1, "memory_mb": 128,
		"status": "creating", "guest_ip": "172.20.0.10", "host_tap": "fc-pg0",
		"mac": "AA:BB:CC:DD:EE:99", "vsock_cid": 900, "from_snapshot": "",
		"boot_ms": 42, "boot_mode": "cold", "created_at": time.Now().Format(time.RFC3339Nano),
		"metadata": map[string]string{"workspace": "pg-test"},
	}
	if err := s.InsertSandbox(ctx, sb); err != nil {
		t.Fatalf("InsertSandbox: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteSandbox(context.Background(), id) })

	got, err := s.GetSandbox(ctx, id)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	row := got.(map[string]any)
	if row["template"] != "postgres-test" || row["boot_mode"] != "cold" || row["boot_ms"].(int64) != 42 {
		t.Fatalf("round-trip mismatch: %#v", row)
	}
	if err := s.DeleteSandbox(ctx, id); err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	got, err = s.GetSandbox(ctx, id)
	if err != nil {
		t.Fatalf("GetSandbox after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("sandbox still present after delete: %#v", got)
	}
}

func loadDotEnvLocal(t *testing.T) {
	t.Helper()
	for _, path := range []string{".env.local", filepath.Join("..", "..", "..", ".env.local")} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			key := strings.TrimSpace(parts[0])
			val := strings.Trim(strings.TrimSpace(parts[1]), "'\"")
			if os.Getenv(key) == "" {
				t.Setenv(key, val)
			}
		}
		return
	}
}
