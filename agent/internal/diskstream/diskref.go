// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"encoding/json"
	"fmt"
	"os"
)

// DiskRefFile is the conventional filename of the streamed-rootfs sidecar that
// lives next to a template snapshot's clone.ext4 header. Its presence (in lieu
// of a local clone.ext4) tells the restore path to stream the rootfs on demand
// from the recorded GCS object instead of using a local file as the CoW origin.
// It is the disk analog of memstream's vm.mem.gcs.
const DiskRefFile = "rootfs.gcs"

// DiskRef points at the standalone clone.ext4 object in object storage that
// backs a streaming restore. seed-sync writes it on a thin schema-v4 seed when
// an agent opts into disk streaming (so clone.ext4 is never downloaded locally);
// the restore path reads it to build a NewGCSRangeSource and SharedCache.
type DiskRef struct {
	// Bucket is the GCS bucket name (no gs:// prefix).
	Bucket string `json:"bucket"`
	// Object is the object key within Bucket, e.g.
	// "seeds/<template>/<generation>/clone.ext4". Combined with Bucket it forms
	// https://storage.googleapis.com/<bucket>/<object> for ranged GETs.
	Object string `json:"object"`
	// Size is the object's exact byte length, validated against the chunk
	// header's TotalSize so a drifted header can never stream the wrong bytes.
	Size int64 `json:"size"`
	// ChunkSize is the streaming chunk granularity (must match the header's
	// ChunkSize). Recorded so a thin seed is self-describing.
	ChunkSize uint32 `json:"chunkSize"`
	// Generation is the seed generation this ref belongs to, used in logs and to
	// cross-check the SharedCache key derivation.
	Generation string `json:"generation,omitempty"`
}

// WriteFile marshals d to path as indented JSON.
func (d *DiskRef) WriteFile(path string) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// ReadDiskRef parses the DiskRef sidecar at path. A missing file returns an
// os.IsNotExist error so callers can distinguish "no streaming sidecar" from a
// corrupt one.
func ReadDiskRef(path string) (*DiskRef, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d DiskRef
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("diskstream: parse diskref %s: %w", path, err)
	}
	if d.Bucket == "" || d.Object == "" {
		return nil, fmt.Errorf("diskstream: diskref %s missing bucket/object", path)
	}
	return &d, nil
}
