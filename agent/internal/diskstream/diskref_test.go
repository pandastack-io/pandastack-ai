// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskRef_RoundTrip(t *testing.T) {
	in := &DiskRef{
		Bucket:     "pandastack-dev-multi",
		Object:     "seeds/base/123/clone.ext4",
		Size:       10 << 30,
		ChunkSize:  1 << 20,
		Generation: "123",
	}
	p := filepath.Join(t.TempDir(), DiskRefFile)
	if err := in.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDiskRef(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bucket != in.Bucket || got.Object != in.Object || got.Size != in.Size ||
		got.ChunkSize != in.ChunkSize || got.Generation != in.Generation {
		t.Fatalf("round-trip drift: %+v vs %+v", got, in)
	}
}

func TestReadDiskRef_MissingAndCorrupt(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.gcs")
	if _, err := ReadDiskRef(missing); !os.IsNotExist(err) {
		t.Fatalf("missing ref should be IsNotExist, got %v", err)
	}
	bad := filepath.Join(t.TempDir(), DiskRefFile)
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	if _, err := ReadDiskRef(bad); err == nil {
		t.Fatal("corrupt ref should error")
	}
	empty := filepath.Join(t.TempDir(), "empty.gcs")
	_ = os.WriteFile(empty, []byte(`{"size":1}`), 0o644)
	if _, err := ReadDiskRef(empty); err == nil {
		t.Fatal("ref missing bucket/object should error")
	}
}
