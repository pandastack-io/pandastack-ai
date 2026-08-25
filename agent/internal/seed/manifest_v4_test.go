// SPDX-License-Identifier: Apache-2.0
package seed

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestManifest_V4CloneFieldsRoundTrip confirms the schema-v4 clone.ext4
// streaming fields serialize/deserialize and that they are omitempty (a pre-v4
// manifest without them decodes cleanly to zero values).
func TestManifest_V4CloneFieldsRoundTrip(t *testing.T) {
	m := Manifest{
		Schema:      SchemaVersion,
		Template:    "base",
		Generation:  "1781671859002155199",
		MemObject:   "seeds/base/1781671859002155199/vm.mem",
		MemBytes:    2147483648,
		CloneObject: "seeds/base/1781671859002155199/clone.ext4",
		CloneBytes:  10737418240,
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"clone_object"`) || !strings.Contains(string(b), `"clone_bytes"`) {
		t.Fatalf("v4 clone fields not serialized: %s", b)
	}
	var got Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.CloneObject != m.CloneObject || got.CloneBytes != m.CloneBytes {
		t.Fatalf("clone fields drift: %+v", got)
	}
	if got.Schema != 4 {
		t.Fatalf("SchemaVersion = %d, want 4", got.Schema)
	}
}

func TestManifest_PreV4DecodesWithZeroCloneFields(t *testing.T) {
	// A v3 manifest (no clone_* keys) must decode without error to zero values.
	v3 := `{"schema":3,"template":"base","generation":"g","mem_object":"o","mem_bytes":1}`
	var got Manifest
	if err := json.Unmarshal([]byte(v3), &got); err != nil {
		t.Fatal(err)
	}
	if got.CloneObject != "" || got.CloneBytes != 0 {
		t.Fatalf("pre-v4 manifest should have zero clone fields, got %+v", got)
	}
}

// TestEssentialFiles_V4ExcludesCloneAndMem documents the v4 tarball contract:
// neither vm.mem (v3) nor clone.ext4 (v4) travels in the gzip tarball — both are
// standalone range-seekable objects.
func TestEssentialFiles_V4ExcludesCloneAndMem(t *testing.T) {
	for _, f := range essentialFiles {
		if f == "clone.ext4" {
			t.Fatal("clone.ext4 must NOT be in the v4 tarball (published standalone)")
		}
		if f == "vm.mem" {
			t.Fatal("vm.mem must NOT be in the tarball (v3 standalone)")
		}
	}
	// The chunk index + prefetch travel as optional tarball files.
	var hasHeader, hasPrefetch bool
	for _, f := range optionalFiles {
		if f == "clone.ext4.header" {
			hasHeader = true
		}
		if f == "clone.ext4.prefetch" {
			hasPrefetch = true
		}
	}
	if !hasHeader || !hasPrefetch {
		t.Fatalf("clone.ext4.header/.prefetch must be optional tarball files (header=%v prefetch=%v)", hasHeader, hasPrefetch)
	}
}
