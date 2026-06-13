// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTemplateMeta(t *testing.T, dataDir, name, raw string) {
	t.Helper()
	dir := filepath.Join(dataDir, "templates", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCreateOwnership(t *testing.T) {
	dataDir := t.TempDir()
	writeTemplateMeta(t, dataDir, "public-tpl", `{"name":"public-tpl"}`)
	writeTemplateMeta(t, dataDir, "owned-tpl", `{"name":"owned-tpl","owner_workspace":"alice"}`)
	writeTemplateMeta(t, dataDir, "corrupt-tpl", `{not valid json`)

	body := func(m map[string]any) []byte {
		b, _ := json.Marshal(m)
		return b
	}

	cases := []struct {
		name     string
		body     []byte
		ws       string
		wantCode int
	}{
		{"public allowed for anyone", body(map[string]any{"template": "public-tpl"}), "bob", 0},
		{"owner may use own template", body(map[string]any{"template": "owned-tpl"}), "alice", 0},
		{"other workspace forbidden", body(map[string]any{"template": "owned-tpl"}), "bob", 403},
		{"corrupt meta fails closed", body(map[string]any{"template": "corrupt-tpl"}), "bob", 403},
		{"missing template defers to manager", body(map[string]any{}), "bob", 0},
		{"from_snapshot bypasses template gate", body(map[string]any{"template": "owned-tpl", "from_snapshot": "snap-123"}), "bob", 0},
		{"invalid template name rejected", body(map[string]any{"template": "../etc"}), "bob", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := checkCreateOwnership(dataDir, tc.body, tc.ws)
			if code != tc.wantCode {
				t.Fatalf("got code %d, want %d", code, tc.wantCode)
			}
		})
	}
}

func TestStampWorkspaceMetaForcesOverwrite(t *testing.T) {
	// A non-admin caller must not be able to spoof another tenant's workspace.
	in := []byte(`{"template":"t","metadata":{"workspace":"attacker","k":"v"}}`)
	out := stampWorkspaceMeta(in, "victim")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	md := m["metadata"].(map[string]any)
	if md["workspace"] != "victim" {
		t.Fatalf("workspace not force-stamped: got %v", md["workspace"])
	}
	if md["k"] != "v" {
		t.Fatalf("unrelated metadata dropped: %v", md)
	}
}
