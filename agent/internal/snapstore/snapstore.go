// SPDX-License-Identifier: Apache-2.0
// Package snapstore handles cross-agent snapshot replication via GCS.
//
// On Snapshot create the local files (vm.mem + vm.state) are mirrored to
//   gs://<bucket>/snapshots/<id>/{vm.mem,vm.state}
// so that any agent in the fleet can satisfy a `from_snapshot` request,
// not just the agent that took the snapshot.
//
// Implementation note: we shell out to `gsutil` rather than pulling in
// cloud.google.com/go/storage (~5MB of deps + auth churn) because:
//   - gsutil is already installed and authenticated on every agent VM
//     (via the instance's service account)
//   - rsync semantics are exactly what we want for the upload path
//   - parallel chunk download (`-m cp`) is built in
//
// Concurrency: Upload is fire-and-forget (caller doesn't block on GCS).
// Download is synchronous because the caller is about to restore from it.
package snapstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// dlLocks serializes downloads per snapshot id so concurrent forks of the
// same snapshot share a single gsutil cp instead of racing on the same
// .gstmp temp files (gsutil errors out with ENOENT on the temp rename
// when two cp invocations target the same destination).
var (
	dlLocksMu sync.Mutex
	dlLocks   = map[string]*sync.Mutex{}
)

func downloadLock(id string) *sync.Mutex {
	dlLocksMu.Lock()
	defer dlLocksMu.Unlock()
	l, ok := dlLocks[id]
	if !ok {
		l = &sync.Mutex{}
		dlLocks[id] = l
	}
	return l
}

// Meta is JSON-encoded alongside vm.mem/vm.state in the snapshot dir.
// It captures host-side state that FC's snapshot blob references by
// absolute path (notably the rootfs backing file) so cross-agent
// restores can recreate symlinks to satisfy those references.
type Meta struct {
	OriginalSandboxID string `json:"original_sandbox_id"`
	RootfsHostPath    string `json:"rootfs_host_path"`
	Template          string `json:"template"`
	// NATID indicates the original sandbox ran in a NAT-identity netns
	// (vs cold-boot bridge). Restores from a NATID snapshot must run
	// inside a freshly-allocated netns with the SAME baked guest
	// identity (recovered from the template) or the embedded network
	// state in vm.state won't match host topology.
	NATID         bool   `json:"natid,omitempty"`
	VsockUDSPath  string `json:"vsock_uds_path,omitempty"`
	// BakedTapHostIP / BakedGuestIP / BakedMAC capture the NATID
	// identity that was in effect on the origin agent when the
	// snapshot was taken. Cross-agent restores MUST allocate a slot
	// with this exact identity (not the destination agent's local
	// template identity, which may have drifted from an independent
	// bake counter). Empty for pre-Feb-2026 snapshots.
	BakedTapHostIP string `json:"baked_tap_host_ip,omitempty"`
	BakedGuestIP   string `json:"baked_guest_ip,omitempty"`
	BakedMAC       string `json:"baked_mac,omitempty"`
}

// WriteMeta serialises m to <dir>/meta.json.
func WriteMeta(dir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644)
}

// ReadMeta loads <dir>/meta.json. Returns os.ErrNotExist if absent.
func ReadMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// Store is enabled when Bucket is non-empty. A zero Store is a valid no-op.
type Store struct {
	Bucket string // e.g. "your-pandastack-bucket"
}

// NewFromEnv reads PANDASTACK_SNAPSHOT_BUCKET. Returns a no-op store if unset.
func NewFromEnv() *Store {
	return &Store{Bucket: os.Getenv("PANDASTACK_SNAPSHOT_BUCKET")}
}

// Enabled reports whether GCS replication is configured.
func (s *Store) Enabled() bool { return s != nil && s.Bucket != "" }

// objectPrefix is the GCS path prefix for a snapshot id:
//   gs://<bucket>/snapshots/<id>/
func (s *Store) objectPrefix(id string) string {
	return fmt.Sprintf("gs://%s/snapshots/%s/", s.Bucket, id)
}

// Upload mirrors localDir → gs://<bucket>/snapshots/<id>/.
// Uploads only the firecracker artifacts (vm.mem + vm.state).
// Blocks until complete. Caller may invoke in a goroutine.
func (s *Store) Upload(ctx context.Context, snapID, localDir string) error {
	if !s.Enabled() {
		return nil
	}
	dest := s.objectPrefix(snapID)
	// Optional artifacts ride along when present:
	//   - meta.json carries the original rootfs host path which cross-agent
	//     restores need to symlink.
	//   - "hugepages" marks a 2 MiB hugepage-backed snapshot; without it a
	//     remote agent would try mem_file_path and Firecracker would reject
	//     the load (hugepage snapshots restore ONLY via UFFD).
	//   - vm.mem.header is the memstream chunk index; it spares the forced
	//     UFFD restore a full sequential rescan of a multi-GB vm.mem.
	artifacts := []string{"vm.mem", "vm.state"}
	for _, opt := range []string{"meta.json", "hugepages", "vm.mem.header"} {
		if _, err := os.Stat(filepath.Join(localDir, opt)); err == nil {
			artifacts = append(artifacts, opt)
		}
	}
	for _, name := range artifacts {
		src := filepath.Join(localDir, name)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("missing artifact %s: %w", name, err)
		}
		cmd := exec.CommandContext(ctx, "gsutil", "-q",
			"-o", "GSUtil:parallel_composite_upload_threshold=150M",
			"cp", src, dest+name)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("gsutil upload %s: %w: %s", name, err, string(out))
		}
	}
	return nil
}

// Download fetches gs://<bucket>/snapshots/<id>/{vm.mem,vm.state} into
// localDir. Idempotent: if both files are already present locally, returns
// nil immediately without touching GCS.
func (s *Store) Download(ctx context.Context, snapID, localDir string) error {
	if !s.Enabled() {
		return os.ErrNotExist
	}
	memPath := filepath.Join(localDir, "vm.mem")
	statePath := filepath.Join(localDir, "vm.state")
	_, memErr := os.Stat(memPath)
	_, stateErr := os.Stat(statePath)
	if memErr == nil && stateErr == nil {
		return nil
	}
	// Serialize concurrent downloads of the same snapshot. Without this,
	// gsutil's per-invocation .gstmp files collide when N forks of the
	// same snapshot land on this agent before any has finished.
	lk := downloadLock(snapID)
	lk.Lock()
	defer lk.Unlock()
	// Re-check after acquiring the lock — another goroutine may have just
	// finished the download.
	if _, e1 := os.Stat(memPath); e1 == nil {
		if _, e2 := os.Stat(statePath); e2 == nil {
			return nil
		}
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}
	src := s.objectPrefix(snapID)
	// Fetch vm.mem + vm.state required; meta.json is best-effort (snapshots
	// taken before meta was introduced won't have it).
	cmd := exec.CommandContext(ctx, "gsutil", "-q", "-m", "cp",
		src+"vm.mem", src+"vm.state", localDir+"/")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gsutil download: %w: %s", err, string(out))
	}
	// Optional artifacts: ignore not-found. The "hugepages" marker MUST land
	// when it exists upstream (it forces the UFFD restore path); fetching it
	// best-effort alongside meta.json keeps old snapshots working.
	_ = exec.CommandContext(ctx, "gsutil", "-q", "cp",
		src+"meta.json", localDir+"/").Run()
	_ = exec.CommandContext(ctx, "gsutil", "-q", "cp",
		src+"hugepages", localDir+"/").Run()
	_ = exec.CommandContext(ctx, "gsutil", "-q", "cp",
		src+"vm.mem.header", localDir+"/").Run()
	return nil
}

// Delete removes a snapshot from GCS. Best-effort.
func (s *Store) Delete(ctx context.Context, snapID string) error {
	if !s.Enabled() {
		return nil
	}
	cmd := exec.CommandContext(ctx, "gsutil", "-q", "-m", "rm", "-rf",
		s.objectPrefix(snapID))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gsutil rm: %w: %s", err, string(out))
	}
	return nil
}
