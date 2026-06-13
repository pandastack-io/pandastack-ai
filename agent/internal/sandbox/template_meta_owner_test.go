// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func writeMeta(t *testing.T, dataDir, name, raw string) {
	t.Helper()
	dir := filepath.Join(dataDir, "templates", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateOwnerAndIsPublic(t *testing.T) {
	d := t.TempDir()
	writeMeta(t, d, "pub", `{"name":"pub"}`)
	writeMeta(t, d, "owned", `{"name":"owned","owner_workspace":"alice"}`)
	writeMeta(t, d, "corrupt", `{bad`)

	cases := []struct {
		name       string
		tpl        string
		wantOwner  string
		wantRead   bool
		wantPublic bool
	}{
		{"public template", "pub", "", true, true},
		{"owned template", "owned", "alice", true, false},
		{"corrupt fails closed", "corrupt", "", false, false},
		{"missing meta is public", "no-such", "", true, true},
		{"empty name is public", "", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, readable := TemplateOwner(d, tc.tpl)
			if owner != tc.wantOwner || readable != tc.wantRead {
				t.Fatalf("TemplateOwner(%q) = (%q,%v), want (%q,%v)", tc.tpl, owner, readable, tc.wantOwner, tc.wantRead)
			}
			if got := IsPublicTemplate(d, tc.tpl); got != tc.wantPublic {
				t.Fatalf("IsPublicTemplate(%q) = %v, want %v", tc.tpl, got, tc.wantPublic)
			}
		})
	}
}
