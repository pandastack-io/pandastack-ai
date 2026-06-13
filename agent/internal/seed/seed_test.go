// SPDX-License-Identifier: Apache-2.0
package seed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestRoundTrip(t *testing.T) {
	in := Manifest{
		Schema:           SchemaVersion,
		Template:         "code-interpreter",
		Generation:       "1717000000000000000",
		TarSHA256:        "deadbeef",
		TarBytes:         12345,
		CPU:              2,
		MemoryMB:         2048,
		DiskGB:           12,
		SSHKeyFP:         "e000feae19111e24",
		Flavor:           "natid",
		RootfsGeneration: "1700000000000001",
		FCVersion:        "Firecracker v1.13.0",
		CPUPlatform:      "Intel Cascade Lake",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Manifest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestTrimNL(t *testing.T) {
	cases := map[string]string{
		"123\n":     "123",
		"123\r\n":   "123",
		"123 \n":    "123",
		"  123\n\n": "  123",
		"123":       "123",
		"":          "",
	}
	for in, want := range cases {
		if got := trimNL(in); got != want {
			t.Errorf("trimNL(%q)=%q want %q", in, got, want)
		}
	}
}

func TestReadTemplateSize(t *testing.T) {
	dir := t.TempDir()
	tplDir := filepath.Join(dir, "templates", "postgres-16")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "meta.json"),
		[]byte(`{"cpu":2,"memory_mb":2048,"disk_gb":12}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := readTemplateSize(dir, "postgres-16")
	if !ok {
		t.Fatal("expected ok")
	}
	if got != (tplSize{2, 2048, 12}) {
		t.Fatalf("got %+v", got)
	}

	// Missing fields fall back to sandbox defaults (1/1024/10) so the gate
	// matches what the bake recorded for custom templates.
	if err := os.WriteFile(filepath.Join(tplDir, "meta.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ = readTemplateSize(dir, "postgres-16")
	if got != (tplSize{1, 1024, 10}) {
		t.Fatalf("defaults: got %+v", got)
	}

	// Absent template => not ok.
	if _, ok := readTemplateSize(dir, "nope"); ok {
		t.Fatal("expected not ok for absent template")
	}
}

func TestLocalTemplates(t *testing.T) {
	dir := t.TempDir()
	// Two templates with rootfs, one dir without rootfs (should be skipped),
	// one stray file (should be skipped).
	for _, n := range []string{"ubuntu-24.04-net", "code-interpreter"} {
		d := filepath.Join(dir, "templates", n)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "rootfs.ext4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "templates", "no-rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := localTemplates(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 templates, got %v", got)
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	if !seen["ubuntu-24.04-net"] || !seen["code-interpreter"] {
		t.Fatalf("missing expected templates: %v", got)
	}
}
