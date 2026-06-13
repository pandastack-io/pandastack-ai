// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"encoding/json"
	"fmt"
	"os"
)

// MemRefFile is the conventional filename of the streamed-memory sidecar that
// lives next to a template snapshot's vm.state. Its presence (in lieu of a
// local vm.mem) tells the restore path to stream the guest's memory on demand
// from the recorded GCS object instead of mmap-ing a local file.
const MemRefFile = "vm.mem.gcs"

// MemRef points at the standalone, uncompressed vm.mem object in object
// storage that backs a streaming restore. The seed-sync writes it when an
// agent opts into UFFD streaming (so vm.mem is never downloaded locally); the
// restore path reads it to build a NewGCSRangeSource.
//
// It is deliberately a tiny JSON file rather than extra Firecracker-spec
// plumbing: beginUffdRestore only needs the snapshot directory, and the sidecar
// travels with it.
type MemRef struct {
	// Bucket is the GCS bucket name (no gs:// prefix).
	Bucket string `json:"bucket"`
	// Object is the object key within Bucket, e.g.
	// "seeds/<template>/<generation>/vm.mem". Combined with Bucket it forms
	// https://storage.googleapis.com/<bucket>/<object> for ranged GETs.
	Object string `json:"object"`
	// Size is the object's byte length, used to validate against the chunk
	// header's TotalSize so a drifted header can never stream the wrong bytes.
	Size int64 `json:"size"`
}

// WriteFile marshals m to path as indented JSON.
func (m *MemRef) WriteFile(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// ReadMemRef parses the MemRef sidecar at path. A missing file returns an
// os.IsNotExist error so callers can distinguish "no streaming sidecar" from a
// corrupt one.
func ReadMemRef(path string) (*MemRef, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m MemRef
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("memstream: parse memref %s: %w", path, err)
	}
	if m.Bucket == "" || m.Object == "" {
		return nil, fmt.Errorf("memstream: memref %s missing bucket/object", path)
	}
	return &m, nil
}
