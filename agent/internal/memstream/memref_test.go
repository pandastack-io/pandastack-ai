// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemRefRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, MemRefFile)
	ref := &MemRef{Bucket: "my-bucket", Object: "seeds/code-interpreter/1700000000/vm.mem", Size: 268435456}
	if err := ref.WriteFile(p); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ReadMemRef(p)
	if err != nil {
		t.Fatalf("ReadMemRef: %v", err)
	}
	if got.Bucket != ref.Bucket || got.Object != ref.Object || got.Size != ref.Size {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", got, ref)
	}
}

func TestReadMemRefMissingReturnsNotExist(t *testing.T) {
	_, err := ReadMemRef(filepath.Join(t.TempDir(), "absent.gcs"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("want os.IsNotExist, got %v", err)
	}
}

func TestReadMemRefRejectsMissingFields(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"no bucket": `{"object":"seeds/x/1/vm.mem","size":10}`,
		"no object": `{"bucket":"b","size":10}`,
		"empty":     `{}`,
		"corrupt":   `{not-json`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name+".gcs")
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadMemRef(p); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}
